# IPCR Forge — Rationale

IPCR Forge bundles two store apps (Radicle, IPCR), the node's own CI (`rad-actions`) and a static
landing page. The Radicle and IPCR services are copied verbatim, so each keeps the deviations its
standalone listing already argues. This document lists them, then covers only what the bundle adds.

Earlier versions built with Gitea Actions, fed by a bridge that copied every Radicle repository
into Gitea. That made two places where state could start (Gitea held a second copy of each
repository, with its own PRs, issues and registry) and needed a glue service to keep them in step.
Gitea, its runner and the bridge are gone; the CI now runs on the Radicle node itself.

## What deviation / exception is being requested

**Inherited, unchanged** (the full argument is in each listing's `rationale.md`):

| From | Deviation |
| --- | --- |
| Radicle | No authentication gate on `radicle-` and `radicle-api-<domain>`; host port `8776/tcp` |
| IPCR | `ipcr-gateway` and `ipcr-registry` run as root (no capabilities); `/etc/docker/certs.d` mounted; no authentication; ports `127.0.0.1:4767` and `4768/tcp+udp` |

**Added by the bundle:**

1. **`rad-actions-docker` runs `privileged`** (Docker-in-Docker), as Gitea's runner did.
2. **`rad-actions` mounts the Radicle node's home read-write**, including the node's key.
3. **A cross-app network link.** `ipcr-gateway` joins `ci-internal`, where the staging registry is.
4. **A public landing page, including the CI's pages.** `ipcr-forge-<domain>` and its `/ci/`
   (run reports and logs) are not behind the SSO gate.
5. **It replaces two apps** and cannot be installed beside them: same container names, domains
   and host ports.

## Why it is necessary

**1. Privileged Docker-in-Docker.** Building an image needs a Docker daemon. The two ways to give
a CI job one are the host's socket (root on the PCS for every workflow) or a daemon of its own,
which needs `privileged`. The second confines a build to its own container and image store. The
rootless variant does not start on this platform (`kernel.apparmor_restrict_unprivileged_userns=1`;
see Apps/Gitea/rationale.md, where it was tried first).

**2. The node's home.** The CI broker (`cib`, upstream `radicle-ci-broker`) is a client of the
node: it subscribes to the node's events over the control socket in that folder, reads repository
storage to check out a commit, and writes each run's result back into the repository as a job COB,
which the node's key signs. This is the mount `radicle-api` already has. It is what removes the
mirror: the build reads the commit where it lives, so there is no copy to drift.

**3. The network link.** IPCR publishes images by *pulling* them: it polls a registry and imports
every new tag into IPFS. Jobs run in the CI's own daemon, which cannot reach `ipcr.localhost` (that
name is the host's loopback), so a workflow pushes to a staging registry instead. That registry
shares the daemon's network namespace, so it is `localhost:5000` to every job — the only address
Docker and BuildKit accept plain HTTP for, so no TLS or credentials are involved. `ipcr-gateway`
reaches it as `rad-actions-docker:5000` over `ci-internal`.

**4. The public pages.** The landing page holds links, documentation and image names that are
public on IPFS anyway. `/ci/` shows `cib`'s report pages and the run logs. Radicle repositories are
public by design, and the adapter refuses to run a private repository, so a log never shows code
that is not already public. A login in front would stand between a developer and their build log.

**5. Replacing two apps.** The bundle is the same services, not a different product. Renaming every
container, domain and port to coexist would break the defaults the services rely on
(`radicle-api:8080`) and the documented addresses users type.

## Security mitigations in place

- **Only delegates can start a build.** `cib`'s filter (ci/ci-broker.yaml) runs CI only for changes
  that come from a delegate of the repository (`AnyDelegate`): release tags, the default branch and
  their patches. A stranger's patch is never run. This matters because a build can publish: what a
  job pushes to the staging registry ends up on IPFS under the repository's IPNS name. A build
  therefore gets exactly the trust of a release.
- **The privileged daemon has no network listener.** `dockerd` gets an explicit
  `--host=unix:///run/dind/docker.sock` and `DOCKER_TLS_CERTDIR` is empty, so the entrypoint adds no
  TCP socket. The unix socket sits in a folder shared only with `rad-actions`, group `$PGID`.
- **`rad-actions` itself runs unprivileged:** `$PUID`, every capability dropped,
  `no-new-privileges`, 512 MB. Whoever controls it controls the daemon (that is the point of the
  socket), so it accepts no input but the node's events and a workflow from a delegate.
- **The node's key never enters a job.** Jobs run in the nested daemon from a self-contained,
  depth-1 checkout that the adapter fetched; the Radicle home is not mounted into them.
- **The staging registry is unreachable from outside.** No port, no Caddy route, on `ci-internal`
  only (`rad-actions`, the daemon and `ipcr-gateway`). It runs as `$PUID` with no capabilities.
- **Bounded.** One run at a time (`concurrent_adapters: 1`), 60 minutes per run, 2 GB for the daemon.
- **The landing page cannot change anything.** `nginx-unprivileged` runs as `$PUID` with a
  read-only root filesystem, no capabilities, `no-new-privileges` and 32 MB. It mounts the page,
  its config, IPCR's `state/` and the CI's `html/`, all read-only, and serves static files only.
  From `state/` it maps exactly one file (`/images.json` → `published.json`).
- **The CI is severable.** Stopping `rad-actions` and `rad-actions-docker` removes the privileged
  container and leaves a working Radicle node and IPCR registry.
- **Smaller than what it replaces.** Gone: an admin account with the default password, two minted
  tokens, the `DAC_READ_SEARCH` capability IPCR needed to read one of them, and a second git server.

## Alternatives considered and rejected

| Alternative | Why not |
| --- | --- |
| Keep Gitea, fed by a bridge (previous version) | Two sources of state (a second copy of each repository, with its own PRs and registry) and a glue service to keep them in step. |
| Gitea as a strict read-only mirror | Still a second git server and a sync to babysit, for one thing it adds: a web view of builds, which `cib`'s pages now provide. |
| Gitea's `act_runner` in `exec` mode | Same engine as `act` (a fork), but built for Gitea; nektos `act` is what people run on their laptops. |
| `cib`'s native adapter, or Ambient | Neither runs GitHub-style workflows; Ambient needs KVM. |
| `ipcrd` accepting pushes itself | A push endpoint would be a second registry implementation. The staging registry plus IPCR's existing import is byte-identical to `nerdctl push` and needed no change. |
| Host Docker socket for jobs | Any workflow would get root on the host. |
| Run patches from anyone | Their build could publish under the repository's IPNS name. Needs a separate daemon with no registry first. |

## Known limitations

- **No job COB for tag builds.** cib 0.32.1 keys a job by the event's tip, which for an annotated
  tag is the tag object rather than a commit, and creating that job fails (silently; the log only
  shows `NoJob` when the run finishes). Branch and patch builds get their job COB. Tag builds still
  appear in `/ci/` with their log. Upstream fix: peel the tag before creating the job.
- **Tags arrive as pushes.** cib sends a tag event as `event_type: push` with the tag's name in
  `branch`. The adapter tells them apart by looking for that name under `refs/tags/` in storage.
- **`HasFile` matches files only**, not folders, so cib cannot pre-filter on a workflow folder; the
  adapter answers "no workflow" itself.

## Data protection

Everything lives under `/DATA/AppData/ipcr-forge/`, one folder per part:

| Folder | Holds |
| --- | --- |
| `radicle/` | node identity (key pair), seeded repositories (including job COBs), Caddy config |
| `ci/` | `cib`'s database (`state/`), report pages and run logs (`html/`), act's action cache, the daemon's image store (`docker/`), the staging registry (`registry/`, disposable) |
| `ipcr/` | Kubo repo (node identity, IPNS keys, pinned images), TLS CA, watcher state |
| `site/`, `web/` | the landing page and its nginx config, re-rendered on every start |

No user directory (`/DATA/Documents`, `Media`, …) is mounted. The one file outside `/DATA` is
IPCR's `/etc/docker/certs.d/ipcr.localhost:4767/ca.crt`, as in the standalone app. Identities that
matter on the network (Radicle's node key, Kubo's node key and IPNS keys) survive uninstall and
reinstall, so published repositories and image names stay valid.
