# IPCR Forge — Rationale

IPCR Forge bundles four store apps (Radicle, Gitea, Radicle Gitea Bridge, IPCR) and a static
landing page. Their services are copied verbatim, so each part keeps the deviations its
standalone listing already argues. This document lists them, then covers only what the
bundle adds.

## What deviation / exception is being requested

**Inherited, unchanged** (the full argument is in each listing's `rationale.md`):

| From | Deviation |
| --- | --- |
| Radicle | No authentication gate on `radicle-` and `radicle-api-<domain>`; host port `8776/tcp` |
| Gitea | `gitea-runner` is `privileged` (Docker-in-Docker); `ALLOW_LOCALNETWORKS`; admin account `gitea_admin` |
| Radicle Gitea Bridge | An install step signs in once with the default app password to mint scoped tokens |
| IPCR | `ipcr-gateway` and `ipcr-registry` run as root (no capabilities); `/etc/docker/certs.d` mounted; no authentication; ports `127.0.0.1:4767` and `4768/tcp+udp` |

**Added by the bundle:**

1. **A cross-app network link.** `ipcr-gateway` joins `gitea-internal`, the network the
   standalone Gitea app reserves for Gitea and its runner.
2. **A cross-app credential.** The bridge's install step writes a token into IPCR's folder
   (`ipcr/secrets/registry-auth`), which `ipcr-gateway` reads.
3. **A public landing page.** `ipcr-forge-<domain>` is not behind the SSO gate.
4. **It replaces four apps** and cannot be installed beside them: same container names,
   domains and host ports.
5. **One added capability.** `ipcr-gateway` gets `cap_add: [DAC_READ_SEARCH]` on top of the
   standalone listing's `cap_drop: [ALL]`.

## Why it is necessary

**1. The network link.** IPCR publishes images by *pulling* them from Gitea's registry: it
polls the registry and imports every new tag into IPFS. The pull has to come from IPCR's
side, because Gitea's build jobs run in the runner's nested Docker, which cannot reach
`ipcr.localhost` (that name is the host's loopback). In the standalone apps the gateway pulls
over Gitea's public HTTPS address. In the bundle both are on the same server, so it uses
`http://gitea:3000` over `gitea-internal` instead: no round trip through Caddy, and it keeps
working on a box with no public address. Gitea's registry hands out its bearer tokens at the
address the request came in on (`http://gitea:3000/v2/token`), so authentication stays
internal too.

**2. The credential.** Gitea's registry requires sign-in (`REQUIRE_SIGNIN_VIEW`), so IPCR
needs a token. Gitea mints tokens only for a caller holding the account password, and the
bridge's install step is the one place that already does that. It mints one more token,
`ipcr-import`, with `read:package` only, and writes it as a file because an install step can
produce files but not environment variables. `ipcrd` reads `IMPORT_AUTH_FILE` at the start of
every poll, so the file may appear after the gateway has started.

**3. The landing page.** It holds no data that isn't public already: links to Radicle (public
by design) and Gitea (behind its own login), documentation, and the list of images IPCR has
published, which are public on IPFS by definition. A login in front of it would protect
nothing and would stand between a developer and the `image:` line they came for.

**4. Replacing the four apps.** The bundle is the same services, not a different product.
Renaming every container, domain and port to coexist would break the defaults the bridge
and IPCR rely on (`radicle-api:8080`, `gitea:3000`) and the documented addresses users type,
for no use case: running two copies of the same forge on one server.

**5. `DAC_READ_SEARCH`.** The gateway runs as root (for the CA it drops into the host's Docker
trust store) with every capability dropped, and a root with no capabilities is bound by file
modes like any other user. The import token is written by the bridge's install step as `$PUID`,
mode `0600`, in a `0700` folder, so without the capability the gateway cannot open it (verified
on a fresh install: `open /data/secrets/registry-auth: permission denied`, then `401` from
Gitea). `DAC_READ_SEARCH` restores exactly the missing half: reading files and traversing
directories regardless of their modes.

## Security mitigations in place

- **The added capability is read-only and narrow.** `DAC_READ_SEARCH` bypasses read and
  directory-search checks only. It grants no write, ownership or privilege change, and reaches
  only what the gateway mounts: its own TLS folder and state, the token (mounted read-only) and
  the Docker trust store it already writes to.

- **The network link is narrow.** `gitea-internal` holds only `gitea`, `gitea-runner` and now
  `ipcr-gateway`. The gateway's only listener is the read-only registry API. Kubo's
  unauthenticated RPC API stays on `ipcr-internal`, which nothing from Gitea joins. Jobs run
  in the runner's nested daemon, on a network where none of these names resolve.
- **The credential is read-only and contained.** `read:package` cannot push, delete or change
  anything. The file is written as `$PUID` into a `0700` folder and mounted read-only into the
  gateway. It is written then renamed, so a half-written token is never read. Re-running the
  install step replaces the token in Gitea, which revokes the old one.
- **The admin password is used once and never stored.** It is passed to `bridge-init` on its
  command line, as the Gitea app's own `create-admin` step does, and only the scoped tokens
  are written to disk.
- **The landing page cannot change anything.** `nginx-unprivileged` runs as `$PUID` with a
  read-only root filesystem (a tmpfs `/tmp` only), no capabilities, `no-new-privileges` and a
  32 MB limit. It mounts the page, its config and IPCR's `state/` read-only, and serves static
  files only.
- **It publishes one file of IPCR's state, not the folder.** `state/` is mounted outside the
  web root, and `default.conf` maps exactly one path to it:
  `location = /images.json { alias /srv/ipcr-state/published.json; }`. The watcher's own
  bookkeeping (`imported.json`, tag → digest) and anything else in `state/` answers 404.
- **Every inherited mitigation still applies,** because the services are unchanged. For
  example, the privileged runner can still be stopped without affecting the rest of the forge.

## Alternatives considered and rejected

| Alternative | Why not |
| --- | --- |
| Gitea pushes to IPCR (webhook, or a workflow step) | A workflow step cannot reach `ipcr.localhost`. A webhook would give IPCR a write endpoint on a shared network; a pull needs no inbound access at all. |
| Gateway pulls over the public `gitea-<domain>` address, as standalone | Works, but routes a same-box fetch out through Caddy and back, and fails on a box with no public address. |
| An admin-password credential for IPCR | Simpler, but gives the gateway full control of Gitea for a job that only reads packages. |
| `IMPORT_AUTH` as an environment variable | An install step cannot set one, so it would be a manual step after every install. |
| Renaming everything so the bundle can sit beside the four apps | Breaks the defaults and the documented addresses (see 4). |
| No landing page, Radicle explorer as the tile | Leaves newcomers without the pipeline explained or the IPNS name to pull, which IPCR mints and nobody can guess. |

## Data protection

Everything lives under `/DATA/AppData/ipcr-forge/`, one folder per part:

| Folder | Holds |
| --- | --- |
| `radicle/` | node identity (key pair), seeded repositories, Caddy config |
| `gitea/` | repositories, SQLite database, `app.ini`, runner registration, the runner's image store |
| `bridge/` | `cache/` (disposable bare repositories), `secrets/gitea-token` (0600) |
| `ipcr/` | Kubo repo (node identity, IPNS keys, pinned images), TLS CA, watcher state, `secrets/registry-auth` |
| `site/`, `web/` | the landing page and its nginx config, re-rendered on every start |

No user directory (`/DATA/Documents`, `Media`, …) is mounted. The one file outside `/DATA` is
IPCR's `/etc/docker/certs.d/ipcr.localhost:4767/ca.crt`, as in the standalone app. Identities
that matter on the network (Radicle's node key, Kubo's node key and IPNS keys) survive
uninstall and reinstall, so published repositories and image names stay valid.
