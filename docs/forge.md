# IPCR Forge — design notes

How the forge in [Apps/IPCR-Forge/](../Apps/IPCR-Forge/) goes from `git push` to `docker pull`, why it
is shaped this way, and what was learned building it. The listing's own security argument is in
[rationale.md](../Apps/IPCR-Forge/rationale.md).

## Goal

A forge people actually want to use, whose releases do not depend on it staying online:

- **Gitea is the source of truth.** Repositories, pull requests, issues, users and CI live there.
  Developers use plain git and a familiar web UI.
- **Images go to IPFS** through IPCR, under one name for the whole forge, so any server running IPCR
  can pull them from any peer that holds them.
- **Code goes to Radicle.** Every public repository is mirrored, one way, to the Radicle network,
  so it can be cloned without this server.

## History: three versions

| Version | Source of truth | CI | Feedback |
| --- | --- | --- | --- |
| v1 (`afb1072`) | Radicle | Gitea Actions on a read-only Gitea fed by a Radicle → Gitea bridge | Radicle as the place to work was not user-friendly, and a read-only Gitea was confusing |
| v2 (`734a33f`) | Radicle | Radicle CI broker + act on the node (`ci/`, removed since) | Not user-friendly enough: everything went through `rad` |
| **v3** (this) | **Gitea** | **Gitea Actions** | — |

v3 keeps what worked in both: v1's Gitea + Actions setup, and v2's staging registry inside the
CI's own Docker daemon (no tokens, no registry credentials). The bridge is v1's glue reversed and
rewritten in Go ([bridge/](../bridge/)).

The objection that removed Gitea in v2 was "two places where state can originate". It does not apply
here: state originates in Gitea only. Radicle receives a copy that nobody edits; patches or issues
opened on the Radicle side are not read.

## Architecture

```
developer: git push origin main v1.2.3   (or: create a tag in Gitea's UI)
   │
   ▼
gitea ─────────── system webhook (push/create/delete/repository) ──▶ ipcr-forge (bridge)
   │                                                                    · git fetch from Gitea
   │ Actions                                                            · rad init (first time)
   ▼                                                                    · git push rad (signed by the node)
forge-runner (act_runner 0.6.1) ──unix socket──▶ forge-docker (dind)       ▼
   jobs on the daemon's host network → localhost:5000 =              radicle-node ──▶ Radicle network
forge-registry (registry:3, shares dind's network namespace)
   ▲ polled every minute over ci-internal
ipcr-gateway (ipcrd): import → IPFS → publish under the forge's IPNS key
   │
   ▼
anyone: docker pull ipcr.localhost:4767/ipns/<forge k51… | example.eth>/<owner>/<repo>:1.2.3
```

The two legs do not depend on each other: a repository can be built without being mirrored
(private repositories), and mirrored without being built (no workflow).

## The bridge

[bridge/](../bridge/): one Go binary, `ipcr-forge-bridge`, image `ghcr.io/yundera/ipcr-forge-bridge`.

- **`setup`**, an install step (`post_up`, on every start, idempotent). It signs in once as
  `gitea_admin` with the default app password and does four things:
  - mints the daemon's token, `public-only` + `read:repository`;
  - generates the webhook secret;
  - creates or repairs one **system webhook** (`POST /api/v1/admin/hooks`, `is_system_webhook`),
    matched by URL so it is never duplicated;
  - creates the example `gitea_admin/ipcr-hello`.
- **`serve`**:
  - `:8081` takes the webhook. It is internal, with no Caddy route. It checks the HMAC, queues the
    repository ID and answers 202 at once, because Gitea gives a delivery 5 seconds.
  - One worker, with a coalescing queue: a burst of deliveries is one sync.
  - A **reconcile** every 10 minutes catches what no event announces. In Gitea 1.27 that is renames
    and visibility changes, plus any lost delivery.
  - `:8080` serves the forge's page (`/`, `/images.json`, `/repos.json`). It is the place for future
    admin pages, such as Radicle settings.

### Mirror semantics

| Gitea | Radicle |
| --- | --- |
| public repository, first sight | `rad init --public` from a bare mirror; this node is the only delegate; then a `xyz.radicle.crefs` rule `refs/tags/*` (threshold 1) so tags are canonical |
| push, force push, branch delete | `git push --prune rad +refs/heads/*:… +refs/tags/*:…`, then `rad sync <rid> --announce` |
| tag created (git or web UI) | same; becomes canonical `refs/tags/<tag>` |
| tag deleted | removed from the node's namespace; **the canonical `refs/tags/<tag>` stays** (see findings) |
| default branch changed | `rad id update … defaultBranch` |
| rename / transfer | same RID (state is keyed by Gitea's repository ID) |
| made private, deleted | **frozen**: no further sync. Nothing is retracted; it cannot be |
| private, fork, pull mirror | never mirrored (forks and mirrors did not originate here) |
| empty | waits for its first push |

The token cannot see a private repository at all (`public-only`), so a bug cannot publish one. The
code checks visibility again anyway.

## Trust

- **Radicle signatures mean "this forge published it"**, not "this developer wrote it". Radicle is a
  distribution channel here, not the trust root. The `/rad/<rid>/…` image-naming idea in
  [naming.md](naming.md) is weaker for it.
- **Anyone who can push to a repository can publish images.** A job can push any name to the
  unauthenticated `localhost:5000`, and IPCR publishes whatever it finds there under the forge's IPNS
  key, including images built from private repositories. Registration is off, so every user was
  created by the admin.
- **Fork pull requests** from users without write access wait for "Approve and run" in Gitea 1.27.
- The page, the tips and `rationale.md` say all of this plainly.
- **Follow-up:** per-repository push credentials, with the bridge acting as a registry token
  server, would tie an image name to its repository.

## Workflow contract

A repository opts in by committing a workflow under `.gitea/workflows/`. The template is
[bridge/example/.gitea/workflows/build.yml](../bridge/example/.gitea/workflows/build.yml):

- `images: localhost:5000/${{ github.repository }}`: the staging registry. No login, no secret.
  `docker/metadata-action` lowercases the name.
- `context: .` and `driver-opts: network=host`: see findings 2 and 3.
- The image is published as `/ipns/<forge>/<owner>/<repo>:<tag>`.

## Findings

Carried over from v1 and v2; each one silently produced the wrong result rather than an error.

1. **Jobs must share the daemon's network namespace.** `localhost:5000` is the registry only on
   forge-docker's own network, and Docker/BuildKit accept plain HTTP for `localhost` only. The runner
   config sets `container.network: host`. As a consequence, compose service names do not resolve
   inside jobs, so `GITEA_INSTANCE_URL` is Gitea's public address.
2. **`docker/build-push-action` defaults to a Git context** (`<server>/<repo>.git#<ref>`), which needs
   credentials here. Hence `context: .`.
3. **buildx's builder container** does not share the job's network. `driver-opts: network=host` puts
   it on the daemon's, where `localhost:5000` is the registry.
4. **act_runner's `run.sh` reads the registration token once** and gives up after 10 tries. The token
   is minted by an install step that runs after the stack is up, so the compose entrypoint waits for
   it first.
5. **Radicle has no shared `refs/tags/`** unless the identity carries a canonical-refs rule. Without
   one, a tag lives under its pusher's namespace, where `rad clone` and the explorer do not show it.
   The bridge adds the rule right after `rad init`.

New in v3 (tested locally with Gitea 1.27.3 and rad 1.10.1, 2026-10-04):

6. **`rad init` works on a bare repository** (it uses `workdir().unwrap_or(path)`), so the bridge
   keeps bare mirrors only. It also seeds the repository on the node and adds the `rad` remote.
7. **A deleted tag stays canonical.** `git push --prune` removes it from the node's namespace, but
   `refs/tags/<tag>` keeps the last target. Tags are meant to be immutable, so this was left alone.
8. **Gitea's default webhook allow-list (`external`) blocks the bridge.** It is on the server's
   private network. `ALLOWED_HOST_LIST: external,ipcr-forge` allows exactly that one container.
   `private` would let any user's webhook reach every container on the shared network.
9. **`rad sync --announce` takes its RID positionally** (`rad sync <rid> --announce`); there is no
   `--rid` flag.
10. **`rad sync` blocks for its timeout when the node has no peers.** The bridge pushes with
    `-o no-sync` and announces separately with a 30-second bound, so a push is never held up by the
    network.
11. **`act_runner` needs about 1 GB.** It holds a job's checkout and actions in memory while it
    copies them into the job container. At 256 MB it was OOM-killed mid-job. The job container
    then ran on, orphaned, and Gitea showed the run as "running" until it timed out.
12. **The first `rad init` can fail if the node is still starting.** The bridge's retry adopts the
    RID that `rad init` already wrote into the mirror's `rad` remote, so no second repository is
    created (seen on holyhorse). `rad` prints its errors on stdout, which the bridge now logs.
13. **Gitea's API returns `html_url` built from the internal address** (`http://gitea:3000/…`) when
    called through it. The page builds Gitea links from its own host name instead.
14. **Right after `up`, new domains answer with the gateway's SSO redirect** until Caddy picks up the
    labels (about a minute). The runner's registration retries through that window.

## Verified

**Locally** (Gitea 1.27.3-rootless, `radicle-seed-node` 1.10.1, the bridge image, one Docker network):

- `setup` run twice leaves exactly one system hook.
- `ipcr-hello` was created and mirrored with the crefs rule in its identity.
- A tag created through Gitea's API was canonical on Radicle within seconds, through the webhook.
- A private repository never appeared.
- A public repository made private was frozen.

**On holyhorse** (a test PCS, hand install that mimics Maison, 2026-10-04):

| Step | Result |
| --- | --- |
| install steps: Gitea admin, runner token, `forge-setup` | token, system hook, `ipcr-hello` created; runner registered |
| `ipcr-hello` mirrored to Radicle | `rad:z4Nc3jTG2q2rfQbio2gfuA7tWouee`, crefs rule in place |
| tag `v1.0.1` created through Gitea's API (as the web UI does) | Actions run started on `push`; tag canonical on Radicle in seconds |
| build (job image pulled cold) | success after about 4 min; `localhost:5000/gitea_admin/ipcr-hello:1.0.1` and `latest` |
| IPCR import | about a minute later: `/ipns/k51qzi5uqu5dg7urd3olssfzho23ze73n0jd9rwlqkivjlz3gspzyajk8cqfmd/gitea_admin/ipcr-hello:1.0.1` |
| `docker pull` of that name, then `docker run` | page served (`Hello from IPFS`) |
| forge page `ipcr-forge-holyhorse.nsl.sh` | 200; `/images.json` and `/repos.json` filled |

## Open items

- **Pull from a second server.** Verified on holyhorse itself only; v2's images pulled across
  servers the same way.
- **Publish the images**: `ghcr.io/yundera/ipcr:1.2.0` and `ghcr.io/yundera/ipcr-forge-bridge:0.1.0`
  (workflows `image.yml` and `bridge.yml`, tag `bridge-v0.1.0`).
- **Per-repository image names:** the bridge as a registry token server (see Trust).
- **act_runner** is pinned at 0.6.1, proven with Gitea 1.27.3. Upstream has moved on.
- **Radicle → Gitea:** patches opened on Radicle could become pull requests. Not built.
- **Store install:** test through the store, not only as a hand install.
