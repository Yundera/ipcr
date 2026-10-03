# Radicle Gitea Bridge — rationale

## Why a separate app

The bridge is an integration between two apps, and belongs to neither. Put in the Gitea
app, every Gitea install would carry a Radicle component; put in the Radicle app, a seed
node would depend on one particular forge. Gitea's own runner and GitHub's integrations
follow the same split.

## Why a push, not a Gitea pull mirror

Gitea's pull mirror is the built-in way to follow an outside git URL, and the Gitea app's
description suggests it. It does not work for this:

1. **Mirror syncs don't reliably start filtered workflows.** The sync hands Actions a bare
   branch name where the push matcher expects `refs/heads/…`, so a workflow with a branch or
   tag filter silently never runs (gitea#24824, #24926). A real push always works.
2. **Radicle has no shared `refs/tags/`.** A tag lives under the identity that pushed it:
   `refs/namespaces/<did>/refs/tags/v1.0.0`. A mirror copies it there, where Gitea does not
   see a tag. The bridge maps each delegate's tags to `refs/tags/*`.
3. **Mirrors poll at most every 10 minutes** on the Gitea app. The bridge polls every 30 s.

## Why polling

Radicle's node sends no webhooks. Its CI broker (`radicle-ci-broker`) does react to node
events, but ships no container image and must run next to the node's control socket. When it
does, this bridge becomes its adapter; until then, a 30 s poll of radicle-httpd is enough for
releases.

## Opt-in by file, not by list

Which repositories to copy is decided by the repository itself: its head contains
`.gitea/workflows`. The check uses radicle-httpd's tree endpoint, so repositories without CI
are never fetched. Nothing to configure per project, and nothing in the app to keep in sync
with the node's seeding list.

## Safety rules

- **Tags are never forced.** A tag moved after it was built fails the push and is logged, so
  an image version always maps to one commit. Branches are forced: Radicle's canonical head is
  the truth.
- **Delegates must agree.** A tag two delegates point at different commits is skipped.
- **Only its own copies.** An existing Gitea repository is written to only if its description
  names the Radicle ID — the bridge never overwrites a repository someone made by hand.

## Credentials

Token management in Gitea accepts only a password, and creating a repository needs the
`write:user` scope. So the init step signs in once with the default app password (the Gitea
app's own create-admin step sets it) and leaves two tokens:

| Token | Scopes | Where |
| --- | --- | --- |
| `radicle-gitea-bridge` | `write:repository`, `write:user` | `secrets/gitea-token`, mode 0600 |
| `actions-registry-push` | `write:package` | user-level Actions secret `REGISTRY_TOKEN` |

A user-level secret applies to every repository the account owns, so new copies can push
images with no per-repository setup. A separate token is needed because the
`secrets.GITHUB_TOKEN` that Actions injects is refused by Gitea's registry.

## Image

`alpine/git` plus `curl` and `jq` and two shell scripts (github.com/worph/radicle-gitea-bridge). Runs as `$PUID`
with every capability dropped.
