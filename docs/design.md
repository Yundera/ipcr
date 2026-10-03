# Design notes

Why IPCR looks the way it does. The convention itself is [spec.md](spec.md).

## Choosing the address (decided 2026-10-03)

Docker stores images under their full name, so the address in every `image:` line can never change
once published. Four constraints decided it:

- **A port, not port-less.** The reference must be identical on every host. Port 443 is taken by
  mesh-router-caddy on every Yundera PCS and by whatever web server a generic host runs. Routing
  through that proxy would couple the standard to it and put the registry behind a public listener.
- **4767** ("IPNS" on a phone keypad) is unassigned at IANA (registry checked 2026-10-03). It is below
  Linux's 32768–60999 range for outgoing connections. "IPFS" (4737, `ipdr-sp`) and "IPCR" (4727,
  `fcis`) are assigned. 5000/5001/4001/8080 collide with the Docker registry default, Flask, macOS
  AirPlay and Kubo.
- **`.localhost`** (RFC 6761) can never resolve publicly, so the name cannot be hijacked.
  systemd-resolved already maps `*.localhost` to loopback, so most hosts need no `/etc/hosts` edit.
  Rejected:
  - `ipfs.decentralized`: not a reserved top-level domain, so a hijack risk while ICANN issues new
    ones, and it needs a hosts entry.
  - Bare `ipfs`: without a port, Docker reads it as Docker Hub's real `ipfs` organisation.
  - `ipfs.internal`: safe, but needs a hosts entry.
- **`ipcr`, not `ipfs`, as the host label.** In IPFS tooling, `ipfs.localhost` means an IPFS
  *gateway* (Kubo serves `<cid>.ipfs.localhost:8080`). The host names the service, a container
  registry. The path names the backend (`/ipfs/`, `/ipns/`).

Limitation of any loopback address: only the **host's** Docker daemon can pull. Docker-in-Docker
(e.g. a CI runner with its own daemon) sees its own loopback.

## What is reused and what is ours

| Piece | Source |
| --- | --- |
| Registry API ↔ IPFS translation, image format, `push ipfs://` | [nerdctl](https://github.com/containerd/nerdctl) 2.4.1, unmodified |
| IPFS node, IPNS, DNSLink resolution, MFS | [Kubo](https://github.com/ipfs/kubo) 0.43.1, unmodified |
| `/ipns/<name>:<tag>` routes, pin-walker, auto-pin, TLS + CA export | `gateway/` → `ipcrd` (Go, stdlib only) |
| `ipcr push` | `bin/ipcr` (shell around nerdctl + `ipcrd`) |

```
dockerd ──TLS──▶ ipcr-gateway 127.0.0.1:4767 (ipcrd)
                   │  /v2/ipfs/<cid>/…                → passthrough
                   │  /v2/ipns/<name>/manifests/<tag> → kubo resolve /ipns/<name>/<tag> → image root
                   │  /v2/ipns/<name>/blobs/<digest>  → image roots seen for <name> (or its tag dir)
                   ▼
                 ipcr-registry :5050 — `nerdctl ipfs registry serve`
                   ▼
                 ipcr-kubo — RPC :5001 (app network only), swarm :4768 (public)
```

## Findings that shaped it

1. **nerdctl's registry routes only `/v2/ipfs/<cid>/…`, and the tag must be `latest`.**
   `nerdctl push ipns://` is parsed and then rejected. IPNS support is ours.
2. **Pinning the root CID does not pin the image.** Blobs are linked only through `urls` in JSON
   (spec §4). `ipcrd pin` walks them. The gateway auto-pins every image it serves, so a host keeps
   and re-shares what it runs.
3. **Docker 29 refuses plain HTTP to any name but `localhost`.** With the containerd image store, a
   name that resolves to 127.0.0.1 is *not* treated as insecure (`server gave HTTP response to HTTPS
   client`). Hence TLS from a private CA, dropped into `certs.d`, which Docker re-reads on every pull.
4. **Blob requests carry no tag.** For `/ipns/` repositories the gateway remembers which roots a
   name resolved to and probes them. On a cold start it lists the tag directory.
5. **nerdctl assumes rootless mode under any non-zero uid,** even for `ipfs registry serve`, which
   never touches containerd. Its container runs as root with every capability dropped.
6. **Compose cannot create a bind source containing a colon,** so the parent `certs.d` is mounted
   rather than `certs.d/ipcr.localhost:4767`.
7. **Same image → same CID on any node.** Re-publishing `nginxdemos/hello:0.4` from a fresh node
   produced the identical root CID.
8. **The format is reproducible without containerd.** nerdctl's push is containerd's generic
   converter (Docker→OCI, platform filter, which *removes* other platforms from the index) plus
   stargz-snapshotter's hook (`add?cid-version=1&pin=true`, `urls: ipfs://…`). `ipcrd import`
   mirrors it with the Go standard library, and `nginxdemos/hello:0.4` imported from Docker Hub
   gets nerdctl's root CID byte for byte.
9. **Forge handoff: poll, don't push.** CI jobs run in a nested Docker that cannot reach
   `ipcr.localhost`, so IPCR pulls instead. The watcher polls the registry catalog, the same
   zero-config shape as the Radicle → Gitea bridge.

## Open items

- **Large images.** Only a small image (11 layers) has been tested. A prior project
  ([ipfs-oci-registry](https://github.com/fbongiovanni29/ipfs-oci-registry), archived) concluded
  "IPFS too slow for large blobs". Test a 1 GB+ image pulled by a second host.
- **Narrow the host mount.** Write the CA from a one-shot `x-compose-app.init` step, so the
  long-running gateway needs no mount outside `/DATA`.
- **Storage policy.** Auto-pin keeps every pulled image forever. Unpin images no container uses?
- **DNSLink** is untested end to end. It goes through the same Kubo `resolve` call as IPNS keys, which is.
- **Podman**: document and test `/etc/containers/certs.d`.
- **Publishing UX.** `ipcr push` needs the containerd socket (root-equivalent). `ipcr import`
  from a registry does not, and is the preferred path.
