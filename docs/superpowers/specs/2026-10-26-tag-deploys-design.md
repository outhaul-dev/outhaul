# Deploy on new version tags — design

**Date:** 2026-10-26
**Status:** Draft, for maintainer review

## Why

Auto-deploy today means "every push to the app's branch goes live". That suits
continuous deployment, but many teams push to `main` all day and only want
production to change when they cut a release by tagging it (`v1.4.0`). The
maintainer wants an app to be able to follow version tags instead of a branch.

This is the second of the two common release patterns (the first is what
Outhaul does now). In CI it is the familiar `on: push: tags: ['v*']`. The
hosted PaaSes mostly don't offer it themselves. Render and Railway tell users
to turn auto-deploy off and call their API from a tag-triggered CI job, and
Dokploy's auto-deploy matches branches only. Building it in is a small
differentiator, not a catch-up.

A common setup this enables: two apps on the same repository, a staging app
following `main` and a production app following tags. That already works
today for two branch apps, so nothing new is needed for it beyond this
feature.

## Goals

- Each app chooses **when it deploys**: on every push to its branch (today's
  behaviour, and the default) or on a **new tag matching a pattern** (default
  `v*`). *(Maintainer's decision.)*
- A tag deploy builds **exactly the commit the tag points at**. The
  deployment records the tag, and the UI shows it (e.g. "v1.4.0").
- **A tag only deploys if it is newer than what is live.** "Newer" is version
  order, so `v1.10.0` beats `v1.9.2`. With `v1.4.0` live, a hotfix tag
  `v1.3.1` is skipped, and the skip is logged with its reason.
  *(Maintainer's decision.)*
- An **"Include pre-releases"** setting, **off by default**. When off, tags
  such as `v1.5.0-rc.1` and `v2.0.0-beta` are ignored, both by auto-deploy
  and when picking the newest tag for the Deploy button. When on, they count,
  in version order: `v1.4.0` < `v1.5.0-rc.1` < `v1.5.0-rc.2` < `v1.5.0`.
  *(Maintainer's decision.)*
- **Deleted tags are ignored.** *(Maintainer agreed.)*
- The **Deploy button** on a tag-mode app builds the newest matching tag, not
  the tip of the branch. *(Maintainer agreed.)*
- Works from **every push source**: the GitHub App, the per-app generic
  webhook (GitHub/Gitea/GitLab repository webhooks), and `git push` to
  Outhaul's own repos. *(Maintainer agreed.)*

## Non-goals

- Deploying on GitHub **Releases** (the `release` webhook event). Tags work
  the same on every host and every source; a release always has a tag, so a
  tag-mode app already deploys when a release is published with a new tag.
- Choosing an arbitrary tag or commit to deploy from the UI. The Deploy button
  builds the newest matching tag; going back is Rollback (below).
- Tag mode for **template apps**. They have no repository; their settings
  panel is hidden today and stays hidden.
- Tag mode for **preview children**. Previews follow PR heads, whatever the
  parent's mode.
- Exclusion patterns, several patterns per app, or regular expressions. One
  glob plus the pre-release switch.
- Watch paths in tag mode (see "Watch paths" below).

## Anti-goals (failures even if it "works")

- A tag-mode app that **still deploys on a branch push**. Production
  changing between releases is exactly what this feature exists to stop.
- An older tag replacing a newer live version because webhooks arrived out of
  order, or because two tags were queued at once.
- A matching tag **silently** not deploying. Every skip says why (see
  "Where a skip shows" below).
- Existing apps changing behaviour after the upgrade. Every existing app stays
  in branch mode with its current settings.

## Behaviour

### The setting

Per app, in Settings → Deploy settings:

- **Deploy when:** "a push to `<branch>`" (branch mode) or "a new tag matching
  `<pattern>`" (tag mode).
- **Tag pattern:** a glob, default `v*`. It uses the same matcher as watch
  paths (`*` and `?` within a `/`-separated segment, `**` across segments,
  `[seq]`), applied to the tag name without its `refs/tags/` prefix. *(Design's
  call: reuse the existing matcher rather than add a second syntax.)*
- **Include pre-releases:** checkbox, off by default.

The existing **Auto-deploy** toggle keeps its meaning: when off, nothing
deploys automatically in either mode. *(Design's call.)* `git push` apps keep
today's behaviour of deploying regardless of that toggle (the bridge never
checked it); they now follow the deploy mode, though. *(Design's call: don't
change push-app behaviour beyond what this feature needs.)*

In tag mode, the branch field and watch paths don't apply to deploys. The form
should make that clear (hide or disable them while tag mode is selected). The
branch is still stored: switching back to branch mode restores it, and a
`git push` app's cold-create still needs it. *(Design's call.)*

Switching mode never deploys by itself. The next matching event (or a Deploy
click) does.

### What counts as a version

First, the **pattern's literal prefix** is removed from the tag: everything in
the pattern before its first wildcard (`*`, `?` or `[`). So with the pattern
`v*` the tag `v1.4.0` is read as `1.4.0`; with `api/v*`, `api/v1.2.0` is
read as `1.2.0`; with `pkg@*`, `pkg@1.2.3` is read as `1.2.3`. This keeps
monorepo tags (Go's `api/v1.2.0` submodule convention, `api-v1.2.0`) ordered
and Deploy-able. *(Design's call.)*

What's left is **version-like** if, after an optional leading `v` or `V`, it is
`MAJOR[.MINOR[.PATCH]]` (non-negative integers), optionally followed by
`-<pre-release>` and/or `+<build>`, per SemVer 2.0. Missing minor/patch count
as 0 (`v2` = `v2.0.0`). Ordering is SemVer 2.0 precedence: numeric parts
compared numerically; a pre-release sorts below its release; pre-release
identifiers compared dot by dot (numeric numerically, others as strings,
numeric below alphanumeric); build metadata ignored. *(Design's call: a small
in-house parser, like the cron parser and glob matcher. No dependency.)*

A tag is a **pre-release** if it is version-like and has a `-<pre-release>`
part. A tag that isn't version-like is never a pre-release.

### When a tag event deploys

A tag event for a tag-mode app with auto-deploy on deploys when **all** of
these hold:

1. The ref is `refs/tags/<name>` and the event creates or moves the tag (not a
   deletion).
2. `<name>` matches the app's tag pattern.
3. Pre-releases are included, or `<name>` is not a pre-release.
4. `<name>` is **newer than the app's current high-water mark** (below), or it
   can't be compared.

**The high-water mark** is the highest version among the tags of:

- the app's live deployment (status `running`), and
- any of its deployments that are `queued`, `building` or `deploying`,

counting only tags that match the app's **current** pattern. A live `v3.0.0`
from before a switch to `api/v*` doesn't block `api/v1.x` forever.
*(Design's call.)*

Including in-flight deployments is what stops two quick tags, or a late
webhook, from leaving the older one live. *(Design's call.)* Failed,
cancelled and superseded deployments don't count. The check and the insert
of the new deployment run under one in-process lock, so two concurrent
deliveries can't both pass it. *(Design's call.)*

**The worker checks again when it claims** a queued tag deployment that came
from a tag event (not a Deploy click), this time against the live deployment.
This catches a tag queued behind a Deploy click whose tag wasn't known yet:
with `v1.3.0` live, a click queued to retry a failed `v1.4.0`, and a hotfix
`v1.3.1` queued after it, the click goes live as `v1.4.0`. When `v1.3.1` is
then claimed, it is older than live. A deployment dropped this way ends
`cancelled` (a legal `queued → cancelled` transition, so the locked state
machine is unchanged), with a reason that says why, e.g. `skipped: v1.3.1 is
older than the live v1.4.0`. *(Design's call.)* The deployment needs to record
whether it came from a tag event or a click, so the worker knows which rows to
re-check.

The consequences:

| Situation | `v1.3.1` arrives | Why |
|---|---|---|
| `v1.4.0` live | skipped | older than live *(maintainer's decision)* |
| `v1.4.0` queued, `v1.3.0` live | skipped | older than in-flight |
| rolled back to `v1.3.0` (rollback row carries `v1.3.0`) | deploys | newer than what actually runs *(design's call)* |
| `v1.4.0` failed to build, `v1.3.0` live | deploys | failures don't count *(design's call)* |
| `v1.3.1` live, `v1.3.1` re-pushed at another commit | skipped | not newer; the Deploy button still works *(design's call)* |
| live deployment has no tag (built from a branch before the switch) | deploys | nothing to compare against |

**Tags that aren't version-like** (e.g. `release-oct`) can't be ordered. If
one matches the pattern, it deploys. This follows the rule watch paths already
use: not knowing must never silently drop a release. *(Design's call.)* When
computing the high-water mark, such tags are ignored.

**Several tags in one event.** A `git push --tags` can carry many refs. Only
the highest eligible version-like tag deploys; if no eligible tag is
version-like, the last eligible non-version tag in the push order deploys.
*(Design's call.)* GitHub and the other hosts send one push webhook per tag.
Note that GitHub sends **no** push events at all when more than three tags
are pushed at once. That limit is GitHub's; it belongs in the UI hint.

### Where a skip shows

No new UI or table for skipped tags. *(Maintainer's decision: "likely
enough".)* A skip, with its reason, shows:

- **webhook sources:** in Outhaul's process log (as the watch-path skip does
  today) and in the webhook's HTTP response body, which GitHub's and
  GitLab's delivery logs ("Recent deliveries") display. The status stays 200,
  so hosts don't retry or flag the hook as failing;
- **`git push`:** in the pusher's terminal, exit code 0;
- **the worker's claim-time re-check:** as a `cancelled` deployment with the
  reason, in the app's history.

### What a tag deploy builds

The deployment row records the tag name. The pipeline fetches
`refs/tags/<name>` instead of the branch, through the existing full-ref path
in `internal/deploy/git.go` (the one fork previews use), so a branch with the
same name as the tag can't be built by mistake. Annotated and lightweight
tags both work. For `git push` apps, it fetches the tag from Outhaul's bare
repo, as it clones the branch today. *(Design's call.)*

The tag is resolved at clone time, so a tag moved between enqueue and clone
builds the moved tag. That's acceptable: the event that moved it would
have queued its own deploy anyway. *(Design's call.)*

### The Deploy button in tag mode

The Deploy button (and anything else that enqueues a plain deploy for a
tag-mode app) builds the **newest matching tag**: the highest version-like tag
that matches the pattern and passes the pre-release setting. There is no
newer-than-live check, since a click is an explicit request (it may redeploy
what is live).

- The newest tag is looked up in the worker at clone time, using the same
  source and credentials as the clone (e.g. `git ls-remote --tags`, which
  works against public, SSH, GitHub-token and local bare repos alike).
  The deployment row then records the tag it chose. *(Design's call:
  credentials already live in the worker's `cloneSpec`.)*
- If no matching tag is version-like, fall back to redeploying the live
  deployment's tag; if there is none, fail the deployment with a reason that
  names the pattern, e.g. `no tag matches "v*"`. *(Design's call.)*

### Rollback

A rollback row copies its source deployment's tag, so the history reads
"rollback to #12 (v1.3.0)" and the high-water mark reflects what actually
runs. Rollback itself is unchanged.

### Deployment history

Wherever a deployment is listed or shown (app page history, deployment detail
page), a deployment with a tag shows it. The deploy log's first lines say
what is being built ("Building tag v1.4.0"), and say so when the Deploy
button picked the tag.

### Watch paths

In tag mode, watch paths don't gate deploys. A tag push carries no reliable
"files changed since the last release" list, and the release decision has
already been made by whoever tagged. The form shows watch paths only in
branch mode. *(Design's call.)*

### Each push source

- **GitHub App** (`/webhooks/github`, `push` event). The App already
  subscribes to `push`, which includes tag pushes, so no manifest change is
  needed. A tag push has `ref: refs/tags/<name>`; a deletion has
  `deleted: true` (and `after` all zeros).
- **Per-app generic webhook** (`/webhooks/app/{token}`):
  - GitHub and Gitea repository webhooks send tag pushes as `push` events
    with `ref: refs/tags/<name>`, so the existing "push events" setting is
    enough. *(Gitea's behaviour should be checked against a real Gitea.)*
  - GitLab sends tag pushes as a separate **Tag Push Hook** (`object_kind:
    "tag_push"`), which the user must tick when creating the webhook. The
    app's "Connect this repo" panel should say so in tag mode. Deletion:
    `after` is all zeros.
- **`git push` to Outhaul** (`internal/githook` bridge). The post-receive hook
  already relays every ref, tags included. In tag mode, the bridge applies the
  rules above to `refs/tags/*` updates (a deletion has `New` all zeros) and
  ignores branch updates. A push with no deployable tag exits 0 with a line
  saying why, in the style of today's "none is the deploy branch" message,
  e.g. `→ stored 2 ref(s); this app deploys tags matching "v*", and v1.3.1 is
  older than the live v1.4.0, so no deploy`. A cold push still needs a
  branch, as now.

### Previews

A preview child is created as a copy of its parent (`child := parent` in
`internal/previewmgr`). The child must be forced into branch mode, because it
follows the PR head. Previews keep working unchanged for a tag-mode parent.

## Data model

One new migration (`0026_…`; never edit a shipped one):

- `apps.deploy_trigger TEXT NOT NULL DEFAULT 'branch'`, with values `branch`
  and `tag`.
- `apps.tag_pattern TEXT NOT NULL DEFAULT 'v*'`.
- `apps.tag_prereleases INTEGER NOT NULL DEFAULT 0`.
- `deployments.tag TEXT NOT NULL DEFAULT ''`, the tag a deployment builds or
  built. Empty for branch deploys and for a tag-mode Deploy click until the
  worker resolves it.
- Something on `deployments` that tells a tag-event deployment from a Deploy
  click, for the worker's claim-time check (a column, or a flag). Builder's
  choice of shape.

The defaults leave every existing app in branch mode. Nothing here is a
secret, so `sealedQueries` is untouched.

`webhook.PushEvent` needs to carry the tag name (when the ref is a tag) and
whether the event is a deletion, alongside `Branch`. `ParsePush` must accept
GitLab's `tag_push` payload, which has the same `ref`/`after` fields.

## Validation

- The tag pattern can't be empty in tag mode, and can't contain whitespace.
  An unclosed `[` is rejected at save time (the matcher would otherwise match
  nothing, silently). *(Design's call.)*
- The deploy trigger is `branch` or `tag`; anything else is a 400.

## Testing

Test first, with the existing fakes, per `AGENTS.md`:

- The version parser and ordering, table-driven: `v` prefix, missing parts,
  pre-release precedence (including SemVer's own example list), build
  metadata, and non-version tags.
- `ParsePush` for tag create, tag delete and branch pushes, using real
  GitHub, Gitea and GitLab payload shapes (GitLab `tag_push`).
- The gate (`maybeDeploy` or its replacement), every row of the table above,
  plus: branch push to a tag-mode app, pre-release on and off, pattern
  mismatch, deletion, auto-deploy off.
- The bridge: a tag-mode push with branch only, tag only, several tags, an
  older tag, a deleted tag.
- The worker: a tag deployment clones the tag; a tag-mode Deploy click
  resolves the newest matching tag and records it; no matching tag fails with
  the reason. Use a real local git repo with tags for the clone/ls-remote
  path, as the push-source tests do with bare repos.
- Previews: a tag-mode parent's preview child is in branch mode.
- The store: migration defaults on old-schema rows (`openMigratedBefore`).

Check against the real thing before calling it done: a GitHub App tag push
(lightweight and annotated), a GitLab Tag Push Hook, and `git push --tags` to
a running Outhaul.

## Left to the builder

- Form layout, and whether hiding branch/watch-paths in tag mode is inline JS
  or a server-rendered toggle (house style has small inline scripts).
- Where the version parser lives (a small pure package, or `core`).
- How the gate is factored so the three sources share one implementation of
  "which tag, if any, deploys".
- The exact wording of hints, skip messages and log lines, within the
  "always say why" rule.
- Updating `ARCHITECTURE.md`'s M3 paragraph that says tag pushes no-op.
