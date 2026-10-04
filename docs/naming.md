# Naming images — IPNS, publishers, DNSLink and ENS

What an IPCR image reference can name, how each name is resolved, who you trust when you pull
by it, and the options studied for the future (2026-10-04). The normative rules are in
[spec.md](spec.md) §2–3.

Every form ends at the same place: an image root CID under `/ipfs/`. Pulling, pinning and digest
checks are shared; only the name → CID step differs.

## The forms

| Reference | Trust | Mutable | Status |
| --- | --- | --- | --- |
| `ipcr.localhost:4767/ipfs/<cid>` | none: the content is its own proof | no | works |
| `ipcr.localhost:4767/ipns/k51…:<tag>` | the holder of that IPNS key | yes | works |
| `ipcr.localhost:4767/ipns/k51…/<image>:<tag>` | the same, one key for many images | yes | works (ipcr 1.2.0) |
| `ipcr.localhost:4767/ipns/example.com/<image>:<tag>` | the domain owner, via DNS (DNSLink) | yes | works (same resolution path as ENS) |
| `ipcr.localhost:4767/ipns/example.eth/<image>:<tag>` | the ENS name owner, via eth.limo by default | yes | **verified with `metadec.eth`** |
| `ipcr.localhost:4767/rad/<rid>/<image>:<tag>` | the repository's delegates, by Radicle's rules | yes | idea, not built |

## Publisher layout

One IPNS name holds a directory per image, each a tag directory (spec §3):

```
/ipns/k51…                      one IPNS key: the publisher
  ├─ ipcr-hello/
  │    ├─ 1.1.2  → image root CID
  │    └─ latest → image root CID
  └─ team/app/
       └─ 2.3.1  → image root CID
```

`ipcr.localhost:4767/ipns/<name>/team/app:2.3.1` resolves `/ipns/<name>/team/app/2.3.1`. Docker
accepts multi-segment repository paths, and the reference (tag or digest) never contains a `/`,
so the split is unambiguous.

`ipcrd`'s import watcher publishes this way when `IMPORT_PUBLISHER=<key name>` is set
(`IPCR_IMPORT_PUBLISHER` in the apps). It keeps the tree in Kubo's MFS at `/publishers/<key>`
and republishes the key after every import. The key's name (`k51…`) appears in the log and as
`publisher` in `state/published.json`. IPCR Forge uses the key `forge` by default. Without the
setting, each repository gets its own key, as before.

## ENS, step by step

Kubo already resolves `.eth` names: its default `DNS.Resolvers` (`"auto"`) sends them to eth.limo's
DNS-over-HTTPS service, which answers with a DNSLink record built from the name's content hash.
No IPCR code is involved.

1. Register `example.eth`.
2. In the ENS manager: **Records → Edit → Other → Content Hash** =
   `ipns://<publisher k51…>`. One transaction, once.
3. Check: `curl -H 'accept: application/dns-json' "https://dns.eth.limo/dns-query?name=example.eth&type=TXT"`
   should answer `dnslink=/ipns/k51…`.
4. Pull: `ipcr.localhost:4767/ipns/example.eth/<image>:<tag>`.

Verified 2026-10-04 with `metadec.eth` → `ipns://k51qzi5uqu5dk220f3zd460crq58fkmto16wy4v6yo1lgvxgpnej1icesy4sqw`
(the publisher of the IPCR Forge on wisera): `/ipns/metadec.eth/ipcr-hello/1.1.2` resolved to the
image CI built for tag `v1.1.2`, and `docker pull ipcr.localhost:4767/ipns/metadec.eth/ipcr-hello:1.1.2`
succeeded.

Rules that follow:

- **Point the ENS record at `ipns://`, never at `ipfs://`.** Then a release only updates the IPNS
  record (free, seconds). An `ipfs://` content hash would need an on-chain transaction per release.
- **Prefer paths to subdomains.** `example.eth/app` costs nothing to add; `app.example.eth` is a
  new on-chain record per image. Subdomains are worth it to delegate a whole team to its own key.
- **Lowercase ASCII only.** ENS allows Unicode and emoji names; Docker rejects them in a reference.
- **Resolver trust.** By default `.eth` resolves through eth.limo, so you trust its answer. An
  operator can set Kubo's `DNS.Resolvers` to their own Ethereum-backed DoH resolver; no IPCR change.
- **DNSLink domains** work the same way: `_dnslink.example.com` TXT `dnslink=/ipns/k51…`.

## Limits of a single publisher key

- **Whoever holds the key can publish or replace any image under the name.** Keep it on one
  machine.
- **One writer.** Two machines publishing the same key overwrite each other: the record with the
  higher sequence number wins, and the other's tag is silently lost. CI may build anywhere, but
  one node should publish. `ipcrd` serialises its own updates.
- **Liveness.** Kubo signs records for 48 h and re-signs them every few hours while the key holder
  is online. If the holder is offline longer, the name stops resolving, even though the images
  stay fetchable by CID wherever they are pinned.

## Options studied, not built

### The maintainer's Radicle key as the IPNS key

A Radicle identity and an IPNS name are the same kind of key (ed25519), encoded differently:

```
did:key:z6MkfAL5Us4e97yHGkkN2MkxkHaUgqtCqJF5D3jwHryV5LGs                 (Radicle)
→ k51qzi5uqu5dgg124jd2tor15abao6lormx2js08khp7iayskdqgnolt5f4iqs   (same key as an IPNS name)
```

(base58btc → strip the `0xed01` multicodec → libp2p protobuf `08 01 12 20 <key>` → identity
multihash → CIDv1 `libp2p-key` → base36.)

So a repository's image name could be computed from its maintainer's identity, and only that
maintainer could move a tag. The CI node does not hold that key, so a release would become two
steps: CI builds and records the CID; the maintainer runs a small `ipcr release v1.2.3` that signs
the IPNS record with their Radicle key (through the same ssh-agent used to sign commits). Signed
with a long lifetime, the record can then be republished by any seed node without the key: the
maintainer signs once, seeds serve it, which is how Radicle itself works.

### `/rad/<rid>`

Resolve a tag through Radicle: the canonical tag (by the repository's delegate rules) → a CID that
the release recorded in the repository (a signed ref or a release object). It brings multi-delegate
trust and no expiry. Two constraints found:

- **Docker rejects uppercase**, and a RID is base58 (`zc9XLrmUrt2xSgTLcEYfKN8eZuxS`):
  `repository name must be lowercase`. The RID must be re-encoded in lowercase (base32 or base36)
  and decoded by the implementation.
- **A pulling host needs Radicle data.** Options in order of effort: ask a seed's HTTP API (trust
  that seed), verify Radicle's signed refs locally, or run a node (the forge has one).

### Other name systems

The rule: add a namespace only if it brings a source of trust people already have. Each one is a
dependency and attack surface in the implementation. Name systems that map a name to a CID belong
under `/ipns/` through DNSLink or Kubo's resolvers, not as new top-level paths.

| System | Verdict |
| --- | --- |
| ENS (`.eth`) | Under `/ipns/`, already works via Kubo. A separate `/ens/` only to drop eth.limo, better done as a resolver setting. |
| DNSLink | Under `/ipns/`, already works. |
| Solana Name Service | Same shape as ENS; could be added as a Kubo resolver if asked for. |
| Unstoppable Domains | Resolution through one company's API; little use. |
| Handshake, Namecoin | Need their own chain client; little use. |
| DIDs | `did:key` is an IPNS key already; `did:web` is DNS. Nothing new. |
| Radicle | The one that adds new trust (delegates of a repository): `/rad/`, above. |
