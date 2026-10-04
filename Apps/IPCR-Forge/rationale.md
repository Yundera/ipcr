# IPCR Forge — Rationale

IPCR Forge bundles three store apps (Gitea, Radicle, IPCR), a CI runner with its own Docker daemon,
and the bridge (`ipcr-forge-bridge`), which mirrors public Gitea repositories to Radicle and
serves the forge's page. The Gitea, Radicle and IPCR services are copied from their listings,
so each keeps the deviations its standalone listing already argues. This document lists them,
then covers only what the bundle adds.

Gitea is the source of truth. Earlier versions made Radicle the source: first with a read-only
Gitea fed from it (v1), then with CI on the Radicle node alone (v2). Users found both hard to work
with. See docs/forge.md.

## What deviation / exception is being requested

**Inherited, unchanged.** The full argument for each is in that listing's `rationale.md`.

| From | Deviation |
| --- | --- |
| Gitea | An admin account (`gitea_admin`) created with the server's default app password |
| Radicle | No authentication gate on `radicle-` and `radicle-api-<domain>`; host port `8776/tcp` |
| IPCR | `ipcr-gateway` and `ipcr-registry` run as root (no capabilities); `/etc/docker/certs.d` mounted; no authentication; ports `127.0.0.1:4767` and `4768/tcp+udp` |

**Added by the bundle:**

1. **`forge-docker` runs `privileged`** (Docker-in-Docker), as Gitea's runner did.
2. **The bridge (`ipcr-forge`) mounts the Radicle node's home read-write**, including the node's
   key. It publishes repositories signed by that key.
3. **A system webhook and its allow-list.** Gitea may deliver webhooks to `ipcr-forge` on the
   server's network (`GITEA__webhook__ALLOWED_HOST_LIST: external,ipcr-forge`).
4. **An install step that uses the admin password** to mint the bridge's token and to register
   that webhook.
5. **A cross-app network link.** `ipcr-gateway` joins `ci-internal`, where the staging registry
   is.
6. **A public page.** `ipcr-forge-<domain>` is not behind the SSO gate.
7. **Public repositories are published to Radicle automatically**, and that cannot be undone.
8. **It replaces three apps** and cannot be installed beside them: it uses the same container names,
   domains and host ports.
9. **Admin pages with their own login** (`/admin`): Gitea OAuth2, Gitea site admins only. The
   install step registers the OAuth2 app.
10. **The bridge mounts Kubo's keystore read-only**, so it can export the publisher key, encrypted.
11. **An admin API in `ipcr-gateway`** (`:4769`) on a new internal network, `ipcr-admin`. Its Bearer
    token is in `ipcr/state/admin-token`, mode 0644.

## Why it is necessary

**1. Privileged Docker-in-Docker.** Building an image needs a Docker daemon. A CI job can get one
in two ways:
- the host's socket, which gives every workflow root on the PCS;
- a daemon of its own, which needs `privileged`.

The second keeps a build inside its own container and image store. The rootless variant does
not start on this platform (`kernel.apparmor_restrict_unprivileged_userns=1`; see
Apps/Gitea/rationale.md, where it was tried first). The runner itself is a separate, unprivileged
container that reaches the daemon over a unix socket.

**2. The node's home.** Publishing to Radicle means three things: creating a repository in the
node's storage, signing its refs with the node's key, and announcing them through the node's
control socket. Those live in that folder. This is the mount `radicle-api` already has. The bridge
uses the stock `rad` CLI (1.10.1, the node's version) rather than reimplementing any of it.

**3. The webhook.** Pushes reach Radicle within seconds instead of at the next 10-minute
reconcile. Gitea's default allow-list (`external`) refuses private addresses, and the bridge sits
on one. The list adds exactly one host name. `private` would let any user's webhook reach every
container on the shared network.

**4. The admin password at install.** Gitea's token API and its system-webhook API accept only an
administrator signing in. The step uses the password once per start, never stores it, and
leaves the running service holding nothing but a `public-only`, `read:repository` token. If the
password has been changed, the step keeps the existing token and webhook instead of failing.

**5. The network link.** IPCR publishes images by *pulling* them: it polls a registry and
imports every new tag into IPFS. Jobs run in the CI's own daemon, which cannot reach
`ipcr.localhost`, because that name is the host's loopback. So a workflow pushes to a staging
registry instead. That registry shares the daemon's network namespace, so every job sees it as
`localhost:5000`. That is the only address Docker and BuildKit accept plain HTTP for, so no TLS
or credentials are involved. `ipcr-gateway` reaches it as `forge-docker:5000` over `ci-internal`.

**6. The public page.** It holds links, documentation, image names that are public on IPFS anyway,
and the list of repositories already public on Radicle. A login in front of it would stand between
a stranger and the instructions for pulling an image.

**7. Publishing to Radicle.** That is the point of the mirror: the code of a released image stays
available without this server. Only public repositories go. A public Gitea repository is meant to
be read by anyone, and Radicle is one more place to read it.

**8. Replacing three apps.** The bundle runs the same services, not a different product. Renaming
every container, domain and port so they could coexist would break the defaults the services rely
on (`radicle-api:8080`, `gitea:3000`) and the addresses users type.

**9. Admin pages.** The operator needs to manage the IPCR side: the ENS or DNS name; whether the
name and the images resolve from outside; unpublishing; which repositories may publish; and, most
of all, backing up the publisher key, which is the only copy of every image name. Gitea is the
forge's identity system, so the admin pages sign in with it rather than adding accounts or relying
on the PCS gate, which is not on every host.

**10. The keystore mount.** Kubo exports keys from its CLI only (`key/export` is not available
over its HTTP API). ipcrd runs as root without capabilities, so it cannot read a `$PUID` 0700
folder; giving it `DAC_READ_SEARCH` was rejected once already. The bridge runs as `$PUID` and can.

**11. The admin API and its token.** Unpublishing, moving tags, key restore and the allowlist
change ipcrd's own state (the published tree, `published.json`), so they live in ipcrd. The token is
needed because CI jobs on `ci-internal` can reach `ipcr-gateway`. The file is world-readable
because ipcrd, root without `CAP_CHOWN`, cannot hand a file to `$PUID` any other way. It sits in
the app's own state folder.

## Security mitigations in place

**The bridge**
- **It sees public repositories only.** Its Gitea token is scoped `public-only` and
  `read:repository`. Gitea hides every private or limited repository from it, and it can write
  nothing in Gitea. The code checks visibility a second time. Forks and pull mirrors are skipped.
- **Webhooks are authenticated and carry no content.** Each delivery must pass the HMAC-SHA256
  check against a random secret written at install (`bridge/secrets`, 0700). A delivery only says
  *which* repository changed. The bridge re-reads that repository from Gitea, so a forged payload
  can at most cause a pointless sync. Port 8081 has no Caddy route.
- **It runs unprivileged:** `$PUID`, every capability dropped, a read-only root filesystem,
  `no-new-privileges`, 256 MB. One worker does one sync at a time.
- **The node's key stays in the bridge and the node.** No CI job mounts the Radicle home.

**The CI daemon**
- **No network listener.** `dockerd` gets an explicit `--host=unix:///run/dind/docker.sock` and an
  empty `DOCKER_TLS_CERTDIR`. The unix socket sits in a folder shared only with `forge-runner`,
  group `$PGID`.
- **`forge-runner` runs unprivileged:** `$PUID`, no capabilities, `no-new-privileges`. Jobs run in
  the nested daemon, never on the host's.
- **The staging registry is unreachable from outside.** It has no port and no Caddy route and sits
  on `ci-internal` only. It runs as `$PUID` with no capabilities.
- **Bounded.** One job at a time, at most 1 hour per job, 2 GB for the daemon.
- **Severable.** Stopping `forge-runner` and `forge-docker` removes the privileged container and
  leaves a working Gitea, Radicle node and IPCR registry.

**Gitea**
- **Fork pull requests wait for approval.** In Gitea 1.27, a run from a user without write access
  waits for "Approve and run".
- **Registration is off.** Every account is created by the admin.
- **Who can publish is stated** in the tips and on the page. Write access to any repository means
  being able to publish images under the forge's name. Images from private repositories become
  public on IPFS.

**The page**
- **The public page cannot change anything.** The bridge serves it read-only: the embedded page,
  IPCR's `published.json` (mounted read-only) and the bridge's repository list.

**The admin pages**
- **Only Gitea site admins get in.** The check is `is_admin`, read from Gitea's own API through the
  internal address. The Gitea token is dropped right after.
- **The session cookie:**
  - HMAC-signed, `HttpOnly`, `Secure`, valid 8 h;
  - the login uses PKCE and `state`, and is only accepted on the hosts in `PUBLIC_HOSTS`.
- **Every write must be same-origin.** The `Origin` header (or Fetch Metadata) must match, and the
  custom `X-IPCR-Admin` header is required. A cross-site form cannot send that header, and a
  cross-site script cannot without a CORS preflight, which is never answered.
- **The admin API is only on `ipcr-admin`.** That network is `internal: true` and shared only by
  the bridge and `ipcr-gateway`; every call needs the Bearer token, compared in constant time. Kubo's
  own API stays on `ipcr-internal`.
- **The cleartext key never crosses the network.** The bridge encrypts it in memory (AES-256-GCM,
  PBKDF2 at 600k iterations, at least 12 characters of passphrase) and ipcrd decrypts it. A
  restore never deletes a key: the previous one is kept, renamed.
- **The allowlist is closed by default.** Until the bridge sends its list, nothing is published
  (`IMPORT_ALLOW_REQUIRED`). Private repositories are never on it.

## Alternatives considered and rejected

| Alternative | Why not |
| --- | --- |
| Radicle as the source, Gitea read-only (v1) | Users had to work in Radicle and found a read-only Gitea confusing |
| Radicle only, CI on the node (v2) | Not user-friendly enough: every step went through `rad` |
| Gitea push mirror to Radicle | Gitea cannot push to `rad://` (no `git-remote-rad`), and mirror pushes run on a timer |
| Publishing to Radicle from a workflow job | It would put the node's key inside CI jobs |
| A per-repository webhook | Every repository would need setup; one system webhook covers all of them, including new ones |
| Gitea's package registry as the staging registry (v1) | Needs tokens for the workflow and for IPCR, plus the `DAC_READ_SEARCH` capability for IPCR to read its token. It would also be a second place images get published from |
| Host Docker socket for jobs | Any workflow would get root on the host |
| A long-lived admin token for the bridge | The webhook is repaired by the install step on every start instead, so the running service never holds admin rights |

## Known limitations

- **Radicle cannot unpublish.** A repository made private or deleted in Gitea is *frozen* (no
  further sync). What already reached the network stays there.
- **A deleted tag stays in Radicle's canonical `refs/tags/`.** It is removed from the node's own
  namespace only.
- **Radicle signatures are the forge's, not the developer's.** The node is each repository's only
  delegate.
- **Image names are limited, but their source is not proven.** Only allowed names (public
  repositories that are switched on) are published. A build in any repository can still push an
  allowed name to the staging registry. Per-repository credentials, issued by the bridge, are a
  planned follow-up.
- **The bridge can read the publisher key.** A compromise of the public-facing bridge leaks it. That
  is the trust it already has with the Radicle node key.
- **The admin token is world-readable** inside the app's state folder on the host.
- **Old keys accumulate.** `<key>-replaced-*` keys are kept and republished by Kubo until removed by
  hand (`docker exec ipcr-kubo ipfs key rm <name>`).
- **Re-importing a tag needs it in staging,** and staging is cleaned once a tag is published. A new
  CI run brings it back.

## Data protection

Everything lives under `/DATA/AppData/ipcr-forge/`, one folder per part:

| Folder | Holds |
| --- | --- |
| `gitea/` | repositories, the database (SQLite), configuration, the runner's registration and config |
| `radicle/` | node identity (key pair), seeded repositories, Caddy config |
| `bridge/` | `state/`: the Gitea → Radicle mapping, one bare mirror per repository (disposable), the admin session key and the repository toggles (`admin.json`); `secrets/` (0700): its Gitea token, the webhook secret, the OAuth2 client credentials |
| `ci/` | the daemon's socket, its image store (`docker/`), the staging registry (`registry/`, disposable) |
| `ipcr/` | Kubo repo (node identity, IPNS keys, pinned images), TLS CA, watcher state, `config.json` (name, allowlist), the admin token |

No user directory (`/DATA/Documents`, `Media`, …) is mounted. The one file outside `/DATA` is
IPCR's `/etc/docker/certs.d/ipcr.localhost:4767/ca.crt`, as in the standalone app. Identities that
matter on the network survive uninstall and reinstall, so published repositories and image names
stay valid:
- Radicle's node key;
- Kubo's node key and IPNS keys.
