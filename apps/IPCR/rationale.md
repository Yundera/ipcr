# IPCR — Rationale

## What deviation / exception is being requested

1. **Two services run as root** (`user: "0:0"`): `ipcr-gateway` and `ipcr-registry`.
2. **A volume outside `/DATA`**: `ipcr-gateway` mounts the host's `/etc/docker/certs.d` read-write.
3. **No authentication**: the app has no login gate.
4. **Host ports**: `127.0.0.1:4767/tcp` (loopback only) and `4768/tcp+udp` (public). The app is tagged `needs-public-ip`.
5. **No web UI** (headless): no Caddy route, and the tile has no open action.

## Why it is necessary

IPCR is a container registry for the **host's Docker daemon**, not a service for people. Its client
is `dockerd` resolving `ipcr.localhost:4767` in an `image:` line, and that shapes every point above.

- **The certs.d mount (2) and root on the gateway (1).** With the containerd image store (Docker 29),
  Docker refuses plain HTTP to any registry except the exact name `localhost`. The alternative is
  `insecure-registries` in `daemon.json`, which needs a daemon reload that restarts every app on the
  box. So the gateway serves TLS from a private CA it generates once (`/DATA/AppData/ipcr/tls`) and
  drops that CA into `/etc/docker/certs.d/ipcr.localhost:4767/ca.crt`. Docker re-reads this on every
  pull, so no restart and no config edit is needed. The directory is root-owned on the host, so the
  writer is root. The parent `certs.d` is mounted, not the single subdirectory, because Compose cannot
  create a bind source whose name contains a colon (`ipcr.localhost:4767`).
- **Root on the registry (1).** `nerdctl ipfs registry serve` (upstream, unmodified) assumes
  "rootless mode" when run as any non-zero uid. It then refuses to start without a rootless containerd
  that this command never uses. The service has **no volumes at all**.
- **No authentication (3).** There is nothing to log into. The registry is read-only (it implements
  only the pull half of the OCI distribution API). It serves public, content-addressed data: anyone can
  already fetch the same bytes from the IPFS network by CID. And it is reachable only on `127.0.0.1`.
- **Ports (4).** `127.0.0.1:4767` is how the host daemon reaches the registry. It is bound to loopback,
  so it is not reachable from the network and bypasses no gateway. `4768` is the libp2p swarm port, a
  peer protocol Caddy cannot carry. Without it the node still pulls, but other peers cannot fetch what
  it pins. It is 4768 and not Kubo's usual 4001 so that the Kubo store app can be installed alongside.
- **Headless (5).** The user interface *is* the `image:` line in a compose file.

## Security mitigations in place

- Both root services run with `cap_drop: [ALL]` and `no-new-privileges`. Neither needs a capability:
  the gateway only writes files it owns in root-owned directories, and the registry only opens a socket.
- The gateway writes exactly one host file outside `/DATA`:
  `/etc/docker/certs.d/ipcr.localhost:4767/ca.crt`. Its CA key never leaves `/DATA/AppData/ipcr/tls`
  (mode 0600). A CA trusted by Docker only for `ipcr.localhost:4767` can impersonate nothing else:
  Docker scopes a `certs.d` entry to its own `host:port`.
- The name `ipcr.localhost` cannot be hijacked: `.localhost` is reserved by RFC 6761 and never
  resolves publicly. Every blob Docker receives is digest-checked against its manifest, and the
  gateway resolves CIDs only through the local Kubo node, which verifies content hashes.
- Kubo's RPC API (unauthenticated, total control of the node) is `expose`d on the app-private
  `ipcr-internal` network only. No service joins the shared `pcs` network.
- Kubo runs as `$PUID:$PGID` and keeps all state under `/DATA/AppData/ipcr/kubo`.
- Memory limits on all three services, and a 20 GB soft cap on Kubo's block store with GC enabled.

## Alternatives considered and rejected

- **`insecure-registries` in `daemon.json`**: needs a host config edit and a Docker restart, which
  stops every app. The CA drop needs neither.
- **Mounting only `/etc/docker/certs.d/ipcr.localhost:4767`**: Compose rejects the colon in a bind
  source it has to create. A one-shot `x-compose-app.init` step could write the CA instead, keeping
  the long-running gateway free of the host mount. This is planned once that path is verified on Maison.
- **Plain `localhost:<port>`**: the only name Docker accepts over HTTP, but generic and collision-prone.
  IPCR's address is meant to be a fixed, recognisable standard (`ipcr.localhost:4767`).
- **A public DNS name with a real certificate**: every box would need the certificate's private key,
  which would get the certificate revoked.
- **Running the registry as `$PUID`**: blocked by nerdctl's rootless detection (see above).

## Data protection

IPCR touches no user directory (`/DATA/Documents`, `Downloads`, `Media`, `Gallery`). Its state is
`/DATA/AppData/ipcr/kubo` (node identity, IPNS publishing keys, pinned images) and
`/DATA/AppData/ipcr/tls` (the registry CA). Both survive uninstall and reinstall. Keeping the CA means
Docker's trust stays valid across a reinstall, and keeping the node identity keeps published IPNS names.
On uninstall, the one host file, `/etc/docker/certs.d/ipcr.localhost:4767/ca.crt`, is left in place.
It is inert without the registry, and reused on reinstall.
