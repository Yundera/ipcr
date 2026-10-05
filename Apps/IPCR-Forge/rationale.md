# IPCR Forge — Rationale

IPCR Forge is the forge without the apps it is built on: it uses the store's **Gitea** app
(required) and **Radicle** app (optional), and brings the rest: the forge service (`ipcr-forge`),
a CI runner with its own Docker daemon, the staging registry and its gate, and IPCR. It is
[IPCR Forge (all in one)](../IPCR-Forge-AIO/rationale.md) minus the bundled Gitea and Radicle, with
the same image and the same services otherwise. That document's arguments apply here unchanged for
everything this listing still ships: the privileged CI daemon, the staging gate and push
credentials, the forge service's access to IPCR's admin API and to the publisher key, the public front
door, and IPCR's own deviations. This document covers only what the split changes.

## What deviation / exception is being requested

Everything the all-in-one listing requests for the parts kept here (see its rationale), and two
things of its own:

1. **A bind mount into another app's folder.** The forge service mounts the Radicle app's home,
   `/DATA/AppData/radicle/home`, read-write: its key, its storage and its control socket. The
   listing also declares that folder (`x-compose-app.folders`), so it exists, owned by `$PUID`,
   before either app starts.
2. **A webhook allowance in the Gitea app.** The Gitea listing allows webhooks to the host name
   `ipcr-forge` (`webhook.ALLOWED_HOST_LIST: external,ipcr-forge`).

## Why it is necessary

1. Radicle has no write API: its HTTP daemon is read-only. Creating a repository, pushing to it
   and announcing it take the node's own key, storage and control socket, which is what the
   all-in-one listing mounts too (there, inside one app). Declaring the folder matters: left to
   Docker, a missing bind source is created as root, and the Radicle app's install step
   (`rad auth`, run as `$PUID`) could then not write the node's key when it is installed after the
   forge.
2. Gitea refuses webhooks to private addresses by default, and the forge service is reached on
   this server's shared network. The name resolves only while the forge is installed; without it,
   the allowance is inert.

## Security mitigations in place

- **The mount is the trust the all-in-one forge service already has** with the same node: it signs
  what the forge publishes. Nothing is mounted from the Gitea app: the forge reaches it over its
  API only, with a `public-only` token for reading, so a private repository is invisible to it.
- **Uninstalling the forge leaves Radicle alone.** The store moves only the app's own folder
  (`$AppID`) on uninstall; `/DATA/AppData/radicle/home` belongs to the Radicle app.
- **No admin token is stored for setup.** As in the all-in-one listing, the setup step signs in
  with the admin password (`FORGE_GITEA_PASSWORD`, default the server's app password) once per
  start, and keeps only scoped tokens. Gitea mints tokens only for a password, not for another token,
  which is why the override is a password and not a pasted token.
- **The runner registration token** comes from Gitea's admin API in that same step, rather than
  from `gitea actions generate-runner-token` on the Gitea app's volume, which this app does not
  mount.
- **Only `ipcr` jobs reach the staging registry.** The forge's runner takes the `ipcr` label only,
  so ordinary CI stays on the Gitea app's runner, and a publishing job never lands on a runner
  that cannot reach `localhost:5000`.
- **`allowed_host_list` stays narrow**: `external` plus one name, not `private`.

## Alternatives considered and rejected

| Alternative | Why not |
| --- | --- |
| Pasting an admin token instead of the password | Gitea mints tokens for a password only: the forge could not create its `public-only` token, and would have to read with an admin token that sees private repositories |
| Mounting the Gitea app's volume to run `gitea actions generate-runner-token` | Write access to all of Gitea's data for one token; the admin API gives the same token |
| Same runner labels as the Gitea app's runner | Gitea hands a job to whichever matching runner is free: publishing would fail at random |
| The forge running a Radicle node of its own beside the Radicle app | Two node identities, two ports to open, and the Radicle app would not show the mirrors |
| Requiring Radicle | Many users want Gitea → IPFS without the mirror; the forge waits for a node instead |

## Known limitations

- **No dependencies between apps in the store.** Removing the Gitea app leaves the forge reporting
  Gitea as unreachable on its page; removing Radicle stops the mirror (repositories show
  *waiting*).
- **The Radicle app is found at its default app ID** (`radicle`). Installed under another ID, the
  forge does not see it.
- **`rad` versions:** the forge image carries the `rad` CLI at the version the Radicle listing pins.
  An update of one without the other may break mirroring until both match.
- **Everything else** is as in the all-in-one listing's "Known limitations".

## Data protection

Everything this app owns lives under `/DATA/AppData/ipcr-forge/`:

| Folder | Holds |
| --- | --- |
| `forge/` | `state/`: the Gitea → Radicle mapping, one bare mirror per repository (disposable), the admin session key and the repository toggles (`admin.json`); `gate/credentials.json` (token hashes); `secrets/` (0700): its two Gitea tokens, the webhook secret, the OAuth2 client credentials, `setup-status` |
| `runner/`, `runner-config/` | the runner's registration and config |
| `ci/` | the daemon's socket, its image store (`docker/`), the staging registry (`registry/`, disposable) and its socket (`registry-socket/`) |
| `ipcr/` | Kubo repo (node identity, IPNS keys, pinned images), TLS CA, watcher state, `config.json` (name, allowlist), the admin token |

Gitea's data stays in the Gitea app, and Radicle's (the node key, the mirrored repositories) in the
Radicle app. No user directory is mounted. The one file outside `/DATA` is IPCR's
`/etc/docker/certs.d/ipcr.localhost:4767/ca.crt`, as in the standalone app.
