# IPCR Demo — Rationale

## What deviation / exception is being requested

1. **No authentication**: the page is public.
2. **No image tag**: the image is referenced by content address only
   (`ipcr.localhost:4767/ipfs/<cid>@sha256:<digest>`).
3. **A dependency on another app**: the image is pulled through the IPCR app's registry.
4. **No healthcheck.**

## Why it is necessary

1. **Public page.** whoami answers each request with what the server saw of *that* request:
   the visitor's address, user agent and headers. It stores nothing and shows no one else's
   data, so a login would protect nothing. The app exists to be opened and looked at.
2. **No tag.** The checklist asks for a specific version rather than `:latest`, so that an install
   is reproducible. A content address is stricter than a version tag: the CID is the IPFS address
   of these exact bytes, and the `@sha256:` digest makes Docker verify the index itself.
   It names traefik/whoami **v1.12.0**, imported with `ipcrd import --all-platforms`
   (`IMPORT_PLATFORM=all`).
3. **The IPCR dependency** is the point of the demo. The store has no field to declare it, so it
   is stated first in `tips.before_install` and in the description.
4. **No healthcheck.** The image is `FROM scratch`: a single static binary, with no shell or
   HTTP client to run a probe with.

## Security mitigations in place

- Runs as `$PUID:$PGID` on port 8080, with a read-only root filesystem, no capabilities,
  `no-new-privileges` and a 32 MB memory limit. No volumes, no host access.
- The image cannot be swapped: the `@sha256:` digest is checked by Docker on pull, and IPCR
  checks every block against its CID.

## Alternatives considered and rejected

- **`/ipns/<name>:v1.12.0`** (a tag the publisher can move): readable, but it can change under an
  installed app. A store listing should name the exact bytes it was reviewed with.
- **AppShield in front**: it would hide the one thing the page shows, the visitor's own request.

## Data protection

The app has no data and touches no `/DATA` path.
