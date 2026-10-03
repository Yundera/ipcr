# IPCR v1 — InterPlanetary Container Registry convention

Status: **draft v1** (2026-10-03). The address in §1 is frozen. Everything an `image:` line or a
published CID depends on is frozen with it.

IPCR is a convention for referencing container images stored on IPFS, so that **unmodified Docker,
Podman and Compose** can pull them. An *IPCR implementation* is anything that serves the address
below with the behaviour below. This repository contains one.

The key words MUST, SHOULD and MAY are used as in RFC 2119.

## 1. Address

```
ipcr.localhost:4767
```

- An implementation MUST serve the OCI Distribution API (pull side) over **HTTPS** at this
  host:port on the host's loopback interface (127.0.0.1, and MAY also listen on ::1).
- It MUST NOT be reachable from other hosts.
- `ipcr.localhost` resolves to loopback by RFC 6761. Hosts whose resolver does not implement that
  SHOULD map `127.0.0.1 ipcr.localhost` in `/etc/hosts`.
- The client MUST trust the implementation's certificate. For Docker, the CA is installed at
  `/etc/docker/certs.d/ipcr.localhost:4767/ca.crt`. For Podman, at
  `/etc/containers/certs.d/ipcr.localhost:4767/ca.crt`.

Image references always carry the full address. `ipcr.localhost:4767/…` and any other spelling
(different port, no port) are different image names to Docker, so an implementation MUST NOT be
offered under another address.

## 2. Repository paths

| Reference | Meaning |
| --- | --- |
| `ipcr.localhost:4767/ipfs/<cid>` | The image whose root (§4) is `<cid>`. Immutable. The only valid tag is `latest`, which Docker applies by default. |
| `ipcr.localhost:4767/ipfs/<cid>@sha256:<digest>` | Same image, pinned to a manifest digest. |
| `ipcr.localhost:4767/ipns/<name>:<tag>` | The image a publisher currently names `<tag>` under `<name>` (§3). Mutable. |

- `<cid>` MUST be a CIDv1 in lowercase base32 (`bafy…`, `bafk…`). Docker rejects uppercase
  repository names, so CIDv0 (`Qm…`) cannot be used.
- `<name>` is either an IPNS key in lowercase base36 (`k51…`) or a DNSLink domain (`example.com`,
  resolved via the `_dnslink.example.com` TXT record).
- `<tag>` follows Docker's tag grammar. With no tag, Docker asks for `latest`.

Future path prefixes (for other content-addressed backends) MAY be added. `/ipfs/` and `/ipns/`
MUST keep their meaning.

## 3. Tag layout (IPNS)

`/ipns/<name>` MUST resolve to a **UnixFS directory**. Each entry of that directory is a tag:

```
/ipns/<name>/
├── latest   → <image root CID>
├── 0.4      → <image root CID>
└── 1.0.0    → <image root CID>
```

- The entry's name is the tag. The entry's target is an image root as defined in §4.
- A publisher SHOULD keep a `latest` entry.
- For backward compatibility an implementation MAY accept `/ipns/<name>` resolving directly to an
  image root, in which case only the tag `latest` is valid.

Moving a tag means publishing a new directory under the same IPNS name (or DNSLink record).

## 4. Image format

An **image root** is the format produced by `nerdctl push ipfs://…` (containerd / stargz-snapshotter
"IPFS-enabled image"):

- The root CID addresses a JSON **OCI descriptor** of the image's index or manifest.
- Every descriptor reachable from it (index → manifests → config and layers) carries the blob's own
  CID in its `urls` field as `ipfs://<cid>`.
- Each blob is stored as its own UnixFS file whose bytes are exactly the blob, so its OCI digest
  matches.

Because blobs are linked only through `urls` inside JSON, **not** through IPLD links, pinning the root
CID alone does not retain the image. A node that wants to keep or re-share an image MUST pin every
CID reachable through `urls`. Implementations SHOULD do this for every image they serve.

A multi-platform index MAY omit the `ipfs://` URL on platforms that were not published. Pulling
those platforms then fails, and pulling the published ones works.

## 5. Behaviour

- The registry is **read-only**: push, delete and catalog endpoints are not part of IPCR.
- For `/ipns/` repositories, blob requests carry no tag. An implementation MUST resolve a blob digest
  against the image roots the name currently points to.
- Errors SHOULD use the OCI error format (`MANIFEST_UNKNOWN`, `BLOB_UNKNOWN`, `NAME_UNKNOWN`).
