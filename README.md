# IPCR — InterPlanetary Container Registry

Pull container images from IPFS with **unmodified Docker and Compose**:

```yaml
services:
  app:
    image: ipcr.localhost:4767/ipfs/bafkrei…               # immutable, by content
  app2:
    image: ipcr.localhost:4767/ipns/k51qzi5…:0.4            # a tag, via an IPNS key
  app3:
    image: ipcr.localhost:4767/ipns/myapp.example.com:v2    # a tag, via a DNSLink domain
```

`ipcr.localhost:4767` is a fixed local address that every IPCR host serves the same way. A compose
file that uses it works on any host running IPCR, and the image comes from whichever IPFS peers hold it.

- **[docs/spec.md](docs/spec.md)**: the IPCR convention (address, paths, tag layout, image format).
- **[docs/design.md](docs/design.md)**: why it is built this way, findings, open items.
- **[apps/IPCR/](apps/IPCR/)**: the Yundera AppStore listing.

## Install

**On a Yundera PCS:** install **IPCR** from the AppStore (listing in [apps/IPCR/](apps/IPCR/)).

**Anywhere else (Docker 29+, containerd image store):** run the same compose by hand:

```sh
mkdir -p /DATA/AppData/ipcr && cd /DATA/AppData/ipcr
curl -fsSLO https://raw.githubusercontent.com/Yundera/ipcr/main/apps/IPCR/docker-compose.yml
install -d -m 0700 -o 1000 -g 1000 kubo && install -d -o 1000 -g 1000 init.d
curl -fsSL -o init.d/01-ipcr-node.sh https://raw.githubusercontent.com/Yundera/ipcr/main/apps/IPCR/seed/init.d/01-ipcr-node.sh
chmod 0755 init.d/01-ipcr-node.sh
printf 'PUID=1000\nPGID=1000\nTZ=UTC\nAppID=ipcr\n' > .env
docker compose up -d
```

If the host's resolver does not map `*.localhost` to loopback (no systemd-resolved), add
`127.0.0.1 ipcr.localhost` to `/etc/hosts`.

Check: `docker pull ipcr.localhost:4767/ipfs/bafkreia2ljjsoi7leeyp65slqqsg7fcckp6vqgnk3i4l2iqaxufkehbwdu`
(the `nginxdemos/hello:0.4` test image).

What it changes on the host: one file, `/etc/docker/certs.d/ipcr.localhost:4767/ca.crt`, so Docker
trusts the registry's private CA. Docker is not reconfigured or restarted. Ports: `127.0.0.1:4767`
(loopback only) and `4768/tcp+udp` (IPFS swarm, public).

## Publish an image

Publishing reads the image from the host's containerd store, so it needs the containerd socket. It
is a one-off command, not something the installed app exposes:

```sh
docker pull nginxdemos/hello:0.4        # or any image you built
docker run --rm --network ipcr_ipcr-internal \
  -v /run/containerd/containerd.sock:/run/containerd/containerd.sock \
  ghcr.io/yundera/ipcr:0.1.0 ipcr push nginxdemos/hello:0.4 hello:0.4
```

```
root CID: bafkreia2ljjsoi7leeyp65slqqsg7fcckp6vqgnk3i4l2iqaxufkehbwdu
pull:     ipcr.localhost:4767/ipfs/bafkreia2ljjsoi7leeyp65slqqsg7fcckp6vqgnk3i4l2iqaxufkehbwdu
pull:     ipcr.localhost:4767/ipns/k51qzi5…:0.4
dnslink:  _dnslink.<your-domain> TXT "dnslink=/ipns/k51qzi5…" → ipcr.localhost:4767/ipns/<your-domain>:0.4
```

- The image is pinned with every layer (see spec §4), and stays available from this node.
- `<app>:<tag>` (optional) keeps a tag directory published under an IPNS key named `<app>` in this
  node's keystore. A non-`latest` tag also moves `latest`. Each IPNS publish takes about 40 s.
- To give it a readable name, add the printed DNSLink TXT record to a domain you own.

Other commands, same `docker run` prefix: `ipcr pin <cid>`, `ipcr tag <app> <tag> <cid>`,
`ipcr resolve <name> [tag]`.

Requires Docker's containerd image store (default since Docker 29), where images live in containerd
namespace `moby`.

## Develop

```sh
docker compose up -d --build                                   # dev stack, state in ./data
docker compose run --rm cli ipcr push <image> [<app>[:<tag>]]
```

```
gateway/         ipcrd — the IPCR gateway (Go, stdlib only)
bin/             entrypoint (roles: gateway | registry | ipcr) and the ipcr CLI
Dockerfile       one image for every role: ghcr.io/yundera/ipcr
apps/IPCR/       AppStore listing: compose, rationale, seed, assets
docs/            spec and design notes
```

## Release

Images are built by GitHub Actions ([.github/workflows/image.yml](.github/workflows/image.yml)) for
`linux/amd64` and `linux/arm64`:

| Event | Tags pushed to `ghcr.io/yundera/ipcr` |
| --- | --- |
| push of tag `v0.1.0` | `0.1.0`, `0.1` |
| push to `main` | `main`, `sha-<short>` |
| pull request | built, not pushed |

To release: bump the image tag in `apps/IPCR/docker-compose.yml` and in this README, commit, then
`git tag v0.1.0 && git push origin v0.1.0`.
