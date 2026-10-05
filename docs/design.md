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
10. **Announce new content immediately.** In Kubo's lowpower profile new content reaches the DHT
    only at the next reprovide, so another node's first pull of a fresh image spent 2 minutes
    finding the root (whoami: 134 s). `ipcrd` now runs `routing/provide` on the root after every
    import and pin. The same cold pull then took 8 s. The root is enough: once a peer has found
    this node, bitswap fetches every other block over the same connection.
9. **Forge handoff: poll, don't push.** CI jobs run in a nested Docker that cannot reach
   `ipcr.localhost`, so IPCR pulls instead. The watcher polls the registry catalog. In IPCR Forge
   that registry is the CI's internal staging registry; see [IPCR Forge](https://github.com/Yundera/ipcr-forge/blob/main/docs/forge.md).
11. **One name for many images.** `/ipns/<name>/<path>:<tag>` and the watcher's publisher mode put
    every image under one IPNS key, which an ENS or DNSLink name can point at once and for all.
    Verified with `metadec.eth`; see [naming.md](naming.md).
12. **An admin API, not admin features in the registry.** Unpublishing, moving tags, the
    allowlist, name checks and key restore are served on a separate listener (`ADMIN_LISTEN`), with
    a Bearer token, for one client: a UI that puts its own login in front (IPCR Forge's service). The
    registry port stays read-only. See [IPCR Forge](https://github.com/Yundera/ipcr-forge/blob/main/docs/forge.md#admin).


## Contract with add-ons

IPCR is the engine; add-ons such as [IPCR Forge](https://github.com/Yundera/ipcr-forge) (a git
submodule here, at `ipcr-forge/`) build on it without the engine knowing about them. What an add-on
may rely on, and what a release of `ghcr.io/yundera/ipcr` must therefore keep or announce as a
breaking change (a major version):

| Surface | What | Where |
| --- | --- | --- |
| Registry API | `/v2/ipfs/<cid>`, `/v2/ipns/<name>[/<path>]:<tag>`, read-only | `gateway/main.go` |
| Import watcher | `IMPORT_WATCH`, `IMPORT_PUBLISHER`, `IMPORT_AUTH[_FILE\|_GENERATE]`, `IMPORT_ALLOW_REQUIRED`, `IMPORT_CLEANUP`, `IPNS_LIFETIME`, `IPNS_TTL` | `gateway/import.go` |
| State files | `STATE_DIR/published.json` (read by pages), `config.json` (`name`, `allow.repos`, `allow.aliases`, `public`), `admin-token`, `staging-auth` | `gateway/import.go`, `gateway/admin.go` |
| Admin API | `ADMIN_LISTEN`, Bearer token, the `/admin/*` endpoints | `gateway/admin.go` |
| Key backups | the `*.ipcrkey.json` format (an add-on keeps a copy; both test the same vector) | `gateway/keybackup.go` |
| Public front door | `PUBLIC_POLICY=<state dir>` | `gateway/public.go` |

## Open items

- **Large images.** Only a small image (11 layers) has been tested. A prior project
  ([ipfs-oci-registry](https://github.com/fbongiovanni29/ipfs-oci-registry), archived) concluded
  "IPFS too slow for large blobs". Test a 1 GB+ image pulled by a second host.
- **Narrow the host mount.** Write the CA from a one-shot `x-compose-app.init` step, so the
  long-running gateway needs no mount outside `/DATA`.
- **Storage policy.** Auto-pin keeps every pulled image forever. Unpin images no container uses?
- **DNSLink** with a plain DNS domain is untested end to end. ENS (`metadec.eth`) is verified, and
  reaches Kubo as a DNSLink record, so the path is the same.
- **Cross-host pull of a publisher path.** Verified on the publishing host only; other hosts need
  ipcr 1.2.0 or later.
- **Podman**: document and test `/etc/containers/certs.d`.
- **Publishing UX.** `ipcr push` needs the containerd socket (root-equivalent). `ipcr import`
  from a registry does not, and is the preferred path.
