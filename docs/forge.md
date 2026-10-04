# IPCR Forge — design notes

How the forge in [Apps/IPCR-Forge/](../Apps/IPCR-Forge/) goes from `git push` to `docker pull`, why it
is shaped this way, and what was learned building it (2026-10-04). The listing's own security
argument is in [rationale.md](../Apps/IPCR-Forge/rationale.md).

## Goal

A decentralised forge that works out of the box: **Radicle is the single source of truth**, builds
use GitHub-style workflows (the developer comfort worth keeping), and images are served from IPFS
through IPCR. Nothing in the chain is a second copy that can drift from Radicle.

## Why Gitea was removed

The first version built with Gitea Actions. A bridge service copied every Radicle repository into
Gitea, Gitea's runner built it, and IPCR imported from Gitea's registry. It worked end to end, but:

- **Two places where state could originate.** Gitea held a full copy of each repository, with its
  own PRs, issues and registry. A copy that is only ever *derived* is a cache; one where things can
  also *start* will desync.
- **A glue service** (the bridge) to keep the two in step, plus its credentials: an admin account,
  two minted tokens, and a capability (`DAC_READ_SEARCH`) IPCR needed only to read one of them.
- What Gitea actually contributed was narrow: running Actions YAML, a web view of runs, and a
  staging registry. None of those needs a forge.

## Architecture

```
developer: git push rad main v1.2.3   (tag signed by a repository delegate)
   │
   ▼
radicle-node ──node events──▶ rad-actions container
                                ├─ cib (radicle-ci-broker 0.32.1, unmodified): filter, queue, reports, job COBs
                                └─ rad-actions adapter (ci/adapter, ours):
                                     · fetch the commit from the node's storage (depth 1)
                                     · apply GitHub's `on:` filters
                                     · run the workflow with act 0.2.89 (unmodified)
                                          │ DOCKER_HOST = unix socket
                                          ▼
                              rad-actions-docker (dind, privileged, no TCP listener)
                                 jobs run on its host network → localhost:5000 =
                              rad-actions-registry (registry:3, shares dind's network namespace)
                                          ▲ polled every minute over ci-internal
                              ipcr-gateway (ipcrd): import → IPFS → publish under IPNS
   │
   ▼
anyone: docker pull ipcr.localhost:4767/ipns/metadec.eth/<repo>:1.2.3
```

- **No mirror.** The build reads the commit where it lives. Results go back into the repository as
  job COBs (`xyz.radworks.job`) signed by the node (see limitations: not for tags yet).
- **CI pages.** `cib` writes static HTML reports; the adapter writes one plain-text log per run. The
  landing page serves both at `ipcr-forge-<domain>/ci/`.
- **Handoff to IPFS is a pull.** Jobs cannot reach `ipcr.localhost` (the host's loopback), so they
  push to the staging registry and `ipcrd` imports from it. `ipcrd` needed no change for this.

## Trust model

- `cib` runs CI only for changes from a **delegate** of the repository (`!AnyDelegate`): release
  tags, the default branch and patches. A build can publish an image under the forge's IPNS name,
  so a build gets the trust of a release. Patches from non-delegates are never run.
- The node's key (which signs job COBs) never enters a job: jobs get a self-contained checkout.
- The adapter refuses private repositories, because run logs are public.
- To run outsiders' patches safely, they would need a separate daemon with no route to the staging
  registry. Not built.

## Workflow contract

A repository opts in by committing a workflow under `.github/workflows/` (`.gitea/` and
`.forgejo/` are also read). The template on the landing page:

```yaml
on:
  push:
    tags: ['v*']
jobs:
  image:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: docker/metadata-action@v5
        id: meta
        with:
          images: ${{ vars.IMAGE }}
          tags: |
            type=semver,pattern={{version}}
            type=raw,value=latest
      - uses: docker/setup-buildx-action@v3
        with:
          driver-opts: network=host
      - uses: docker/build-push-action@v6
        with:
          context: .
          push: true
          tags: ${{ steps.meta.outputs.tags }}
          labels: ${{ steps.meta.outputs.labels }}
```

- `vars.IMAGE` = `localhost:5000/<repository name>`; `vars.REGISTRY` = `localhost:5000`. No secret.
- Two lines differ from a GitHub-hosted workflow: `context: .` and `driver-opts: network=host`
  (both explained below).
- `github.repository` is `rad/<name>`; `RADICLE_RID` is set as an env var and a var.

## Findings

Each of these silently produced the wrong result rather than an error.

### act

1. **act does not apply `on:` branch/tag filters.** A workflow with `on: push: tags: ['v*']` ran for
   a push to `main`. GitHub and Gitea filter server-side; the adapter does it here
   ([filter.go](../ci/adapter/filter.go), GitHub's pattern syntax, tested).
2. **Without a git remote, `github.repository` is `nektos/act`;** with a non-GitHub remote it is the
   whole URL. The adapter passes `--env GITHUB_REPOSITORY=rad/<name>`.
3. **`actions/checkout` is skipped** by act, which copies the working directory into the job. So the
   checkout must be a self-contained repository. A `git clone --shared` of storage left a `.git`
   whose alternates pointed outside the job (`unable to normalize alternate object path`). The
   adapter now fetches the one commit, depth 1, with `uploadpack.allowAnySHA1InWant` on the serving
   side — what `actions/checkout` does by default.
4. **Jobs run on the daemon's host network** (`--network host`, act's default). That is what makes
   `localhost:5000` reach the staging registry. It is also why compose service names do not
   resolve inside jobs (`getaddrinfo ENOTFOUND rad-actions`): the adapter advertises its own IP
   address instead.

### GitHub-isms in common actions

5. **`docker/metadata-action` calls the GitHub REST API** (`GET /repos/{owner}/{repo}`) and fails
   with `Bad credentials` without it. The adapter serves that one call for the length of a run, from
   what the node knows, and sets `GITHUB_API_URL` to it ([api.go](../ci/adapter/api.go)). `html_url`
   is the Radicle explorer page, so the image's `org.opencontainers.image.source` label points there.
6. **`docker/build-push-action` defaults to a Git context** (`https://github.com/<repo>.git#<ref>`),
   which does not exist here. Hence `context: .`.
7. **buildx's builder container** does not share the job's network; `driver-opts: network=host`
   puts it on the daemon's, where `localhost:5000` is the registry. Docker and BuildKit accept
   plain HTTP for `localhost` only, which is why the registry shares the daemon's namespace.

### radicle-ci-broker (cib) 0.32.1

8. **Tags arrive as pushes.** A tag event reaches the adapter as `event_type: "push"` with the tag's
   name in `branch` (src/msg.rs), so the first tag build ran as if `v1.1.1` were a branch. The
   adapter looks the name up in storage (`refs/namespaces/*/refs/tags/<name>`).
9. **`!HasFile` matches files only,** never directories (it checks `into_blob()`), despite its doc
   comment. A filter on `.github/workflows` therefore never matched and nothing ran. It was dropped;
   the adapter answers "no workflow" in about a second.
10. **No job COB for tag builds.** The job is keyed by the event's tip, which for an annotated tag is
    the tag object; creating it fails silently and the run ends with `NoJob`. Branch builds get their
    COB. Upstream fix: peel the tag before creating the job.
11. **Default log level is trace:** every gossip message the node sees. The image runs
    `--log-level info`.
12. **cib must not start before the node's control socket exists.** It restart-loops a few times on
    first install, then settles (`restart: unless-stopped`).

### Radicle

13. **The node only reacts to what it receives** from other nodes: pushes come from the developer's
    own node. The forge node still needs `rad seed <rid> --scope all` per repository (seeding policy
    `block`).
14. **Commit and tag signing need an ssh-agent** holding the Radicle key.

## Verified (wisera, 2026-10-04)

`ipcr-hello` (`rad:zc9XLrmUrt2xSgTLcEYfKN8eZuxS`), tag `v1.1.2` pushed from a delegate's node:

| Step | Time |
| --- | --- |
| tag pushed | 12:54:21 |
| build finished (first run on the box: act's image pulled cold) | 13:01:06 |
| imported into IPFS and published under IPNS | ~13:02:30 |
| pulled on wisera, and on holyhorse (26 s) | after |

Later, with `metadec.eth` pointing at the forge's publisher name (see [naming.md](naming.md)),
`ipcr.localhost:4767/ipns/metadec.eth/ipcr-hello:1.1.2` resolved and pulled on wisera.

## Open items

- **Publish the images.** `ghcr.io/yundera/rad-actions` (workflow `rad-actions.yml`, tag
  `rad-actions-v*`) and `ghcr.io/yundera/ipcr:1.2.0` are so far built locally only.
- **Job COBs for tags** (finding 10): an upstream patch to cib.
- **Patches from non-delegates:** a second, registry-less daemon.
- **Warm cache.** The first build pulls `catthehacker/ubuntu:act-latest` (~1.5 GB) and clones the
  actions it uses; later builds reuse both (`ci/docker`, `ci/cache`).
- **Path filters** (`on.push.paths`) are not applied: a workflow with only `paths:` runs on every
  matching event.
- **Store install.** Tested as a hand install that mimics Maison (compose, `.env`, `.seed/`), not yet
  through the store.
