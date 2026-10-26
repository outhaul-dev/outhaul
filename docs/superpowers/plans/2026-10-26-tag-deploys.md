# Deploy on New Version Tags Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let an app deploy when a new tag matching a pattern (default `v*`) is pushed, building the tagged commit, instead of on every push to its branch.

**Architecture:** A new pure package `internal/tagdeploy` owns version parsing and the single "which tag, if any, deploys" decision. The three push sources (GitHub App webhook, per-app webhook, `git push` bridge) call it through one store method, `EnqueueTagDeployment`, which reads the tags in play and inserts the deployment in one transaction. The worker fetches `refs/tags/<name>`, re-checks tag-event deployments against the live tag when it claims them, and resolves the newest matching tag for a Deploy click.

**Tech Stack:** Go 1.26 (see `go.mod`), SQLite via `modernc.org/sqlite`, `net/http`, `html/template`, the `git` CLI.

**Spec:** `docs/superpowers/specs/2026-10-26-tag-deploys-design.md` (read it first; this plan argues from it).

## Global Constraints

- No new dependencies. The version parser is in-house, like `internal/cron` and the glob matcher.
- One new migration, `internal/store/migrations/0026_tag_deploys.sql`. Never edit a shipped migration.
- Defaults: `deploy_trigger` `'branch'`, `tag_pattern` `'v*'`, `tag_prereleases` `0`. Every existing app stays in branch mode.
- Trigger values are exactly `branch` and `tag` (`core.TriggerBranch`, `core.TriggerTag`).
- Skips never fail a webhook: the status stays 200, and the reason goes in the response body and the process log.
- A `git push` that deploys nothing exits 0 with a `→ … so no deploy` line.
- No new UI or table for skipped tags (maintainer's decision).
- Branch-mode behaviour is unchanged, including watch paths and the branch-deletion open issue in `AGENTS.md`.
- Test first (`AGENTS.md`). Before calling it done: `go vet ./...` and `go test -race ./...`.
- Commits look like `feat(scope): summary`; the body says what the user saw and what changed, and ends with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.

## Review Focus

1. **A branch and a tag with the same name** (`v1.0.0` as both). The tag's commit must be built, not the branch's. Test: Task 8 `TestCloneTagNotSameNamedBranch`.
2. **Two webhook deliveries for the same tag at once** (GitHub redelivery, or GitHub App and per-app webhook both configured). Exactly one deployment. Test: Task 5 `TestEnqueueTagDeploymentSerializesConcurrentDeliveries`.
3. **The pattern changed after a deploy** (live `v3.0.0`, pattern now `api/v*`). New `api/v1.x` tags must still deploy. Test: Task 2, `TestDecide` row "pattern change".
4. **First tag after switching an app from branch mode** (the live deployment has no tag). The first matching tag must deploy. Test: Task 6 `TestTagAppFirstTagAfterBranchDeploy`.
5. **Deploy clicked on a repo with no tags yet.** The deployment fails, with a reason naming the pattern, instead of building the branch. Test: Task 9 `TestDeployClickWithNoMatchingTagFails`.

---

### Task 1: Version parsing and ordering

**Files:**
- Create: `internal/tagdeploy/version.go`
- Test: `internal/tagdeploy/version_test.go`

**Interfaces:**
- Produces:
  - `type Version struct { Major, Minor, Patch uint64; Pre []string }`
  - `func ParseVersion(s string) (Version, bool)`
  - `func (v Version) Compare(o Version) int`, returning -1, 0 or 1
  - `func (v Version) IsPrerelease() bool`

- [ ] **Step 1: Write the failing tests**

`TestParseVersion`, table-driven over `in → (want Version, ok)`:
- **Parses:**
  - `v1.4.0` → `{1,4,0,nil}`
  - `V2` → `{2,0,0,nil}`
  - `1.2` → `{1,2,0,nil}`
  - `v1.5.0-rc.1` → `{1,5,0,["rc","1"]}`
  - `v1.4.0+build.7` → `{1,4,0,nil}`
  - `v1.5.0-rc.1+b` → `{1,5,0,["rc","1"]}`
- **Rejected (`ok == false`):** `""`, `v`, `release-oct`, `v1.2.3.4`, `v1.x`, `1.0.0-`, `1.0.0-a..b`, `v-1`, `1..2`, `1.0.0+`.

`TestCompareVersions`:
- Each element of this chain is `< 0` against the next (SemVer 2.0's own list plus ours):

```go
chain := []string{"1.0.0-alpha", "1.0.0-alpha.1", "1.0.0-alpha.beta", "1.0.0-beta",
	"1.0.0-beta.2", "1.0.0-beta.11", "1.0.0-rc.1", "1.0.0", "v1.9.2", "v1.10.0"}
```

- These pairs compare as `0`: `v1.4.0+1` / `v1.4.0`, and `v2` / `v2.0.0`.

`TestIsPrerelease`:
- `v1.5.0-rc.1` → true
- `v1.5.0` → false
- `v1.5.0+meta` → false

- [ ] **Step 2: Run the tests to see them fail**

Run: `go test ./internal/tagdeploy/ -run 'Version|Prerelease' -v`
Expected: FAIL (the package does not exist yet).

- [ ] **Step 3: Implement it in `internal/tagdeploy/version.go`**

- **Package doc:** "Package tagdeploy decides which pushed tag, if any, a tag-mode app deploys. Pure: no I/O."
- **Parsing:**
  - Strip one leading `v` or `V`.
  - Split off `+build` (it must be non-empty when the `+` is present) and drop it.
  - Split off `-pre`: its identifiers are dot-separated and must be non-empty, from `[0-9A-Za-z-]`.
  - The core is 1–3 dot-separated decimal integers.
- **`Compare` follows SemVer 2.0 precedence:**
  - Compare the numeric parts first.
  - A version with no pre-release sorts above the same version with one.
  - Pre-release identifiers are compared pairwise: both numeric compares numerically, numeric sorts below alphanumeric, and otherwise ASCII order. A shorter list sorts first when all its identifiers are equal.

- [ ] **Step 4: Run the tests to see them pass**

Run: `go test ./internal/tagdeploy/ -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/tagdeploy
git commit -m "feat(tagdeploy): parse and order version tags"
```

---

### Task 2: Tag policy — matching, eligibility, and the deploy decision

**Files:**
- Create: `internal/tagdeploy/policy.go`
- Modify: `internal/webhook/match.go` (export the matcher), `internal/core/app.go` (the trigger constants and App fields)
- Test: `internal/tagdeploy/policy_test.go`, `internal/webhook/match_test.go`

**Interfaces:**
- Consumes: Task 1's `ParseVersion`, `Version.Compare`, `Version.IsPrerelease`.
- Produces:
  - In `core`: `const TriggerBranch = "branch"`, `const TriggerTag = "tag"` and `const DefaultTagPattern = "v*"`, plus the `App` fields `DeployTrigger string` (`TriggerBranch` or `TriggerTag`), `TagPattern string` and `TagPrereleases bool`. These are pure fields: the store persists them in Task 4.
  - `func webhook.MatchGlob(pattern, path string) bool`: the renamed `matchGlob`; `MatchAny` now calls it.
  - `func webhook.ValidGlob(pattern string) bool`: false if any `[` is unclosed.
  - `type tagdeploy.Policy struct { Pattern string; Prereleases bool }`
  - `func tagdeploy.PolicyFor(app core.App) Policy`
  - `func (p Policy) Version(tag string) (Version, bool)`: strips the pattern's literal prefix (everything before its first `*`, `?` or `[`) from the tag, then calls `ParseVersion`.
  - `func (p Policy) Eligible(tag string) bool`: the tag matches the pattern, and it isn't a pre-release unless `p.Prereleases` is on. A tag that isn't version-like is never a pre-release.
  - `type Decision struct { Tag string; Reason string }`: `Tag == ""` means skip, and `Reason` says why.
  - `func (p Policy) Decide(pushed, inPlay []string) Decision`
  - `func (p Policy) Newest(tags []string) (string, bool)`: the highest version-like eligible tag.

- [ ] **Step 1: Write the failing tests**

`TestValidGlob` (in `internal/webhook/match_test.go`):
- `v*` → true
- `release-[0-9]*` → true
- `v[0-9` → false

`TestPolicyVersionStripsLiteralPrefix`:
- `{Pattern:"api/v*"}.Version("api/v1.2.0")` → `1.2.0`
- `{Pattern:"v*"}.Version("v1.4.0")` → `1.4.0`
- `{Pattern:"pkg@*"}.Version("pkg@1.2.3")` → `1.2.3`
- `{Pattern:"*"}.Version("v1.0.0")` → `1.0.0`
- `{Pattern:"v*"}.Version("vnext")` → not ok

`TestPolicyEligible`, with pattern `v*` and pre-releases off:
- `v1.4.0` → true
- `v1.5.0-rc.1` → false
- `release-oct` → false (no match)
- `vnext` → true (it matches, and it isn't version-like)

With pre-releases on, `v1.5.0-rc.1` → true.

`TestDecide`, table-driven (pattern `v*`, pre-releases off unless the row says otherwise). For a skip, the row gives substrings the `Reason` must contain:

| row | pushed | inPlay | want Tag | Reason contains |
|---|---|---|---|---|
| new tag | `v1.4.0` | — | `v1.4.0` | |
| older than live | `v1.3.1` | `v1.4.0` | `""` | `v1.3.1`, `v1.4.0` |
| newer than rollback | `v1.3.1` | `v1.3.0` | `v1.3.1` | |
| re-pushed | `v1.3.1` | `v1.3.1` | `""` | `not newer` |
| build metadata only | `v1.4.0+2` | `v1.4.0` | `""` | `not newer` |
| highest of several | `v1.3.0`,`v1.4.0`,`v1.2.0` | — | `v1.4.0` | |
| non-version fails open (pattern `*`) | `release-oct` | `v1.4.0` | `release-oct` | |
| last non-version (pattern `nightly-*`) | `nightly-a`,`nightly-b` | — | `nightly-b` | |
| pre-release off | `v1.5.0-rc.1` | `v1.4.0` | `""` | `pre-release` |
| pre-release on | `v1.5.0-rc.1` | `v1.4.0` | `v1.5.0-rc.1` | |
| release beats its rc (on) | `v1.5.0` | `v1.5.0-rc.2` | `v1.5.0` | |
| no match | `x1.0` | — | `""` | `"v*"` |
| pattern change (`api/v*`) | `api/v1.1.0` | `v3.0.0` | `api/v1.1.0` | |
| non-version in play ignored | `v1.0.0` | `release-oct` | `v1.0.0` | |
| empty in play ignored | `v0.1.0` | `""` | `v0.1.0` | |

`TestNewest`, over the tags `v1.2.0`, `v1.10.0`, `v1.9.9`, `v2.0.0-rc.1`, `notes`:
- pre-releases off → `v1.10.0`
- pre-releases on → `v2.0.0-rc.1`
- tags `vnext` and `notes` → `"", false`

- [ ] **Step 2: Run the tests to see them fail**

Run: `go test ./internal/tagdeploy/ ./internal/webhook/ -v`
Expected: FAIL (the symbols are undefined).

- [ ] **Step 3: Implement it**

`Decide` works like this:
1. Filter `pushed` to the eligible tags. If none remain, explain the first pushed tag that was rejected, using the reasons below.
2. The high-water mark is the highest `p.Version` among the `inPlay` tags that match `p.Pattern`.
3. If any eligible tag is version-like, take the highest. It deploys if there is no high-water mark or it compares above it; otherwise it is skipped.
4. If no eligible tag is version-like, deploy the last eligible tag.

Reason copy (exact):
- no match: `no pushed tag matches "<pattern>"`
- pre-release: `<tag> is a pre-release, and this app doesn't deploy pre-releases`
- older: `<tag> is not newer than <high-water tag>, which is live or already on its way`

`PolicyFor` copies `app.TagPattern` and `app.TagPrereleases`. Add the `core` constants and `App` fields listed under Interfaces in this step.

- [ ] **Step 4: Run the tests to see them pass**

Run: `go test ./internal/tagdeploy/ ./internal/webhook/ -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/tagdeploy internal/webhook/match.go internal/webhook/match_test.go internal/core/app.go
git commit -m "feat(tagdeploy): decide which pushed tag a tag-mode app deploys"
```

---

### Task 3: Push payloads carry tags

**Files:**
- Modify: `internal/webhook/parse.go`
- Test: `internal/webhook/parse_test.go`

**Interfaces:**
- Produces: `webhook.PushEvent` gains two fields:
  - `Tag string`: `"v1.4.0"` for `refs/tags/v1.4.0`, and empty otherwise.
  - `Deleted bool`: the push removes the ref.

- [ ] **Step 1: Write the failing tests**

`TestParsePushTags`, table-driven. Payloads use each host's real shape, with SHAs 40 characters long:

| name | body (abridged) | Tag | Deleted | Branch |
|---|---|---|---|---|
| github tag create | `{"ref":"refs/tags/v1.4.0","created":true,"deleted":false,"after":"<sha>","commits":[],"repository":{"full_name":"o/r"}}` | `v1.4.0` | false | `""` |
| github tag delete | `{"ref":"refs/tags/v1.4.0","deleted":true,"after":"000…0",…}` | `v1.4.0` | true | `""` |
| gitlab tag_push create | `{"object_kind":"tag_push","ref":"refs/tags/v1.0.0","before":"000…0","after":"<sha>","project":{"path_with_namespace":"g/p"}}` | `v1.0.0` | false | `""` |
| gitlab tag_push delete | same, `"after":"000…0"` | `v1.0.0` | true | `""` |
| gitea tag push | `{"ref":"refs/tags/v2.0.0","after":"<sha>","repository":{"full_name":"o/r"}}` | `v2.0.0` | false | `""` |
| branch push | `{"ref":"refs/heads/main","after":"<sha>",…}` | `""` | false | `main` |

The existing `TestParsePush` rows must still pass.

- [ ] **Step 2: Run the tests to see them fail**

Run: `go test ./internal/webhook/ -run ParsePush -v`
Expected: FAIL (the fields are undefined).

- [ ] **Step 3: Implement it**

- Read `after` and `deleted` from the payload.
- `Deleted` is `deleted == true` or `after` being all zeros (any length, so SHA-256 repositories work too).
- `Tag` is the ref with its `refs/tags/` prefix removed.
- Update the `ParsePush` doc comment.

- [ ] **Step 4: Run the tests to see them pass**

Run: `go test ./internal/webhook/ -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/webhook/parse.go internal/webhook/parse_test.go
git commit -m "feat(webhook): read tag pushes and deletions from push payloads"
```

---

### Task 4: App deploy settings and the migration

**Files:**
- Create: `internal/store/migrations/0026_tag_deploys.sql`
- Modify: `internal/core/app.go`, `internal/core/deployment.go`, `internal/store/apps.go`, and `internal/server/handlers.go:375-397` (the one caller)
- Test: `internal/store/migrate_tag_deploys_test.go`, `internal/store/apps_git_test.go`

**Interfaces:**
- Consumes: Task 2's `core` constants and `App` fields.
- Produces, in `core`: the `Deployment` fields `Tag string` (the tag this attempt builds; empty for the branch, or not yet resolved) and `FromTagEvent bool` (queued by a tag push, so the worker re-checks it at claim).
- Produces, in `store`:
  - `type DeploySettings struct { Branch string; AutoDeploy bool; WatchPaths []string; Trigger string; TagPattern string; TagPrereleases bool }`
  - `func (s *Store) UpdateAppSettings(ctx context.Context, id int64, set DeploySettings) error`. This replaces the positional signature.

- [ ] **Step 1: Write the failing tests**

`TestTagDeployMigrationDefaults`:
1. Run `openMigratedBefore(t, "0026")`.
2. Insert an app row (as in `migrate_webhook_token_test.go`) and a deployment row (`app_id`, `status`, `created_at`).
3. Run `migrate(db)`.
4. Assert the app reads `deploy_trigger = 'branch'`, `tag_pattern = 'v*'` and `tag_prereleases = 0`, and the deployment reads `tag = ''` and `from_tag_event = 0`.

`TestCreateAppDefaultsToBranchTrigger`: `CreateApp` with neither field set reads back as `DeployTrigger == "branch"` and `TagPattern == "v*"`.

Extend `TestUpdateAppSettings`: save `DeploySettings{Branch:"develop", AutoDeploy:true, WatchPaths:[]string{"src/**"}, Trigger:"tag", TagPattern:"release-*", TagPrereleases:true}`, and assert that every field round-trips through `GetApp`.

- [ ] **Step 2: Run the tests to see them fail**

Run: `go test ./internal/store/ -run 'TagDeploy|BranchTrigger|UpdateAppSettings' -v`
Expected: FAIL (compile errors on the new fields and signature).

- [ ] **Step 3: Implement it**

The migration:

```sql
-- Tag deploys: an app can deploy on new version tags instead of every push to
-- its branch. Existing apps stay in branch mode. A deployment records the tag
-- it builds, and whether a tag push queued it (the worker re-checks those).
ALTER TABLE apps ADD COLUMN deploy_trigger  TEXT    NOT NULL DEFAULT 'branch';
ALTER TABLE apps ADD COLUMN tag_pattern     TEXT    NOT NULL DEFAULT 'v*';
ALTER TABLE apps ADD COLUMN tag_prereleases INTEGER NOT NULL DEFAULT 0;
ALTER TABLE deployments ADD COLUMN tag            TEXT    NOT NULL DEFAULT '';
ALTER TABLE deployments ADD COLUMN from_tag_event INTEGER NOT NULL DEFAULT 0;
```

Then:
- Add the three app columns to `appCols`, the `INSERT` in `CreateAppWithDomains` and `scanApp`.
- In `CreateAppWithDomains`, default an empty `DeployTrigger` to `core.TriggerBranch` and an empty `TagPattern` to `core.DefaultTagPattern`.
- Make `handleAppSettings` pass `store.DeploySettings`, carrying the app's current `DeployTrigger`, `TagPattern` and `TagPrereleases` for now. Task 11 adds the form fields.
- Add the two `Deployment` fields to `core/deployment.go`. They are scanned in Task 5.

- [ ] **Step 4: Run the tests to see them pass**

Run: `go test ./internal/store/ ./internal/server/ -v 2>&1 | tail -5`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/core internal/store internal/server/handlers.go
git commit -m "feat(store): per-app deploy trigger, tag pattern and pre-release setting"
```

---

### Task 5: Tag deployments in the store

**Files:**
- Modify: `internal/store/deployments.go`
- Test: `internal/store/deployments_tag_test.go`

**Interfaces:**
- Consumes: Task 4's `core.Deployment.Tag` and `FromTagEvent`.
- Produces:
  - `func (s *Store) EnqueueTagDeployment(ctx context.Context, appID int64, choose func(inPlay []string) (tag string, ok bool)) (core.Deployment, bool, error)`:
    - In one transaction, it reads the in-play tags and calls `choose`.
    - When `ok`, it inserts a queued row with `tag` and `from_tag_event = 1`.
    - It returns `(dep, true, nil)` when it inserts, and `(core.Deployment{}, false, nil)` when it skips.
    - `choose` must not call the store: the store has a single connection, so a call from inside it would deadlock.
  - `func (s *Store) LiveDeployment(ctx context.Context, appID int64) (*core.Deployment, error)`: the newest `running` row, or nil.
  - `func (s *Store) SetDeploymentTag(ctx context.Context, id int64, tag string) error`
  - `CreateRollback` keeps its signature, and copies the source row's tag into the new row.

**"In play"** means the non-empty tags of the newest `running` row, plus every `queued`, `building` or `deploying` row of the app.

- [ ] **Step 1: Write the failing tests**

- **`TestEnqueueTagDeploymentSeesLiveAndInFlightTags`:**
  1. Enqueue tags for one app, and walk each to a status with `SetStatus`:
     - `v1.2.0` → running → superseded (via `SupersedeOthers`, once the next row is running)
     - `v1.3.0` → running
     - `v1.4.0` → building → failed
     - `v1.3.5` → stays queued
  2. Enqueue again with a `choose` that records `inPlay`, and returns `("v1.3.6", true)`.
  3. Assert:
     - `inPlay`, sorted, equals `[v1.3.0 v1.3.5]`;
     - the returned deployment has `Tag == "v1.3.6"`, `FromTagEvent == true` and `Status == queued`;
     - `GetDeployment` reads both fields back.
- **`TestEnqueueTagDeploymentSkipInsertsNothing`:** a `choose` returning `("", false)` gives `ok == false`, and `ListDeploymentsForApp` is empty.
- **`TestEnqueueTagDeploymentSerializesConcurrentDeliveries`:**
  - 8 goroutines each call `EnqueueTagDeployment` with a `choose` that returns `("v1.0.0", true)` unless `inPlay` contains `v1.0.0`.
  - Assert that exactly 1 deployment exists afterwards.
- **`TestLiveDeploymentIsNewestRunning`:** it returns nil with no deployments, and the newer of two running rows when there are two.
- **`TestCreateRollbackCopiesTag`:** a rollback of a `v1.3.0` row has `Tag == "v1.3.0"` and `FromTagEvent == false`.
- **`TestSetDeploymentTag`:** the tag round-trips.

- [ ] **Step 2: Run the tests to see them fail**

Run: `go test ./internal/store/ -run 'TagDeployment|LiveDeployment|RollbackCopiesTag|SetDeploymentTag' -v`
Expected: FAIL (the methods are undefined).

- [ ] **Step 3: Implement it**

- Add `tag, from_tag_event` to `deploymentCols` and `scanDeployment`.
- The in-play query:

```sql
SELECT tag FROM deployments
 WHERE app_id = ? AND tag != '' AND (
       status IN (?, ?, ?)
    OR id = (SELECT id FROM deployments WHERE app_id = ? AND status = ?
             ORDER BY created_at DESC, id DESC LIMIT 1))
```

- `CreateRollback` copies the tag with `(SELECT tag FROM deployments WHERE id = ?)` inside its `INSERT`.

- [ ] **Step 4: Run the tests to see them pass**

Run: `go test -race ./internal/store/ -v 2>&1 | tail -5`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/store
git commit -m "feat(store): enqueue tag deployments against the tags already in play"
```

---

### Task 6: Webhooks deploy tags

**Files:**
- Modify: `internal/server/webhooks.go`
- Test: `internal/server/webhooks_tags_test.go`

**Interfaces:**
- Consumes:
  - Task 2's `tagdeploy.PolicyFor` and `Policy.Decide`
  - Task 3's `PushEvent.Tag` and `Deleted`
  - Task 5's `EnqueueTagDeployment`
- Produces: `func (s *Server) maybeDeploy(ctx context.Context, app core.App, ev webhook.PushEvent) string`. The return value is a one-line outcome for the response body; it is `""` when there is nothing to say.

- [ ] **Step 1: Write the failing tests**

Helpers:
- `tagApp(t, env, pattern string, pre bool) core.App`: a public-source app like `watchedApp`, with `AutoDeploy: true`, `WebhookToken: "tok"`, `WebhookSecret: "sek"`, then `UpdateAppSettings` into tag mode.
- `pushTagBody(tag string, deleted bool) string`
- `seedLiveTag(t, env, appID, tag)`: `EnqueueTagDeployment`, then `SetStatus` queued → building → deploying → running.

Each test posts through `pushWebhook` unless it says otherwise, and asserts 200.

- **`TestTagAppDeploysNewTag`:** pushing `v1.4.0` creates one deployment, with `Tag == "v1.4.0"` and `FromTagEvent`.
- **`TestTagAppIgnoresBranchPush`:** a `refs/heads/main` push creates 0 deployments.
- **`TestTagAppSkipsOlderTagAndSaysWhy`:** with live `v1.4.0`, pushing `v1.3.1` adds no deployment. The response body contains `v1.3.1 is not newer than v1.4.0`.
  - Extend `pushWebhook`, or add a sibling that returns the recorder, to read the body.
- **`TestTagAppIgnoresDeletedTag`:** a deleted tag creates 0 deployments.
- **`TestTagAppPreReleases`:** `v1.5.0-rc.1` creates 0 deployments with pre-releases off and 1 with them on.
- **`TestTagAppAutoDeployOffDoesNothing`:** with `AutoDeploy` false, there are 0 deployments.
- **`TestTagAppWatchPathsDontApply`:** with watch path `src/**`, a tag push whose commits list only `docs/x.md` still deploys.
- **`TestTagAppFirstTagAfterBranchDeploy`** (Review Focus 4):
  - Seed a running deployment with no tag: `CreateDeployment`, then `SetStatus` to running.
  - Push `v0.1.0`; one new deployment has `Tag == "v0.1.0"`.
- **`TestGithubAppWebhookStagingAndProduction`:**
  - Two GitHub-source apps on `o/r` from the same source (as in `TestGithubAppWebhookFansOut`): `stage` in branch mode on `main`, and `prod` in tag mode.
  - A tag push deploys only `prod`; a `main` push deploys only `stage`.
  - Add a helper, `postGithubPayload(t, env, appID, secret, payload)`, to the `postPush` code.
- **`TestGitlabTagPushHookDeploys`:** a GitLab `tag_push` body with the `X-Gitlab-Token: sek` header deploys the tag.

- [ ] **Step 2: Run the tests to see them fail**

Run: `go test ./internal/server/ -run 'TagApp|StagingAndProduction|GitlabTagPush' -v`
Expected: FAIL. Tag pushes create no deployments today.

- [ ] **Step 3: Implement it**

`maybeDeploy` returns `""` when auto-deploy is off. Otherwise it branches on `app.DeployTrigger`:
- **Branch mode:** today's code path, unchanged, returning `""`.
- **Tag mode:**
  - Ignore events with `ev.Tag == ""` or `ev.Deleted`, returning `""`.
  - Otherwise call `EnqueueTagDeployment` with `choose` wrapping `PolicyFor(app).Decide([]string{ev.Tag}, inPlay)`, and capture the `Decision`.
  - On a deploy, call `s.deployer.Notify()`.
  - Log the outcome as `webhook: app <id> <outcome>`.
  - Return `<app name>: deploying <tag>` or `<app name>: not deployed: <reason>`.

Both handlers collect the non-empty outcome lines and write them, newline-joined, as a `text/plain` body with status 200.

- [ ] **Step 4: Run the tests to see them pass**

Run: `go test ./internal/server/ -v 2>&1 | tail -5`
Expected: PASS, with the existing webhook tests unchanged.

- [ ] **Step 5: Commit**

```bash
git add internal/server/webhooks.go internal/server/webhooks_tags_test.go
git commit -m "feat(webhook): tag-mode apps deploy new tags and say why they skip one"
```

---

### Task 7: `git push` deploys tags

**Files:**
- Modify: `internal/githook/bridge.go`
- Test: `internal/githook/bridge_test.go`

**Interfaces:**
- Consumes: Task 2's `Policy.Decide` and Task 5's `EnqueueTagDeployment`.
- Produces:
  - The bridge's `Store` interface gains the `EnqueueTagDeployment` signature from Task 5.
  - `runDeploy` becomes `runDeploy(ctx context.Context, conn io.ReadWriter, app core.App, dep core.Deployment) int`; the caller enqueues.
  - `func tagRefs(refs []RefUpdate) []string`: the names of the `refs/tags/*` updates, in push order, skipping deletions (an all-zero `New`).

- [ ] **Step 1: Write the failing tests**

Helpers:
- `tagPushApp(t, st)`: the `api` push app, then `UpdateAppSettings` into tag mode.
- `seedLiveTag`: as in Task 6, in this package.

Use `runHandle` as the existing tests do, and the `markTerminal` goroutine pattern for streamed deploys.

- **`TestBridgeTagAppBranchPushDoesNotDeploy`:**
  - Push `refs/heads/main`.
  - The output contains `no deploy` and `exit 0`, and there are 0 deployments.
- **`TestBridgeTagAppDeploysPushedTag`:**
  - Push `refs/tags/v1.4.0`.
  - The deployment has `Tag == "v1.4.0"`, and the output contains `deploying tag v1.4.0`, `✓ deployed` and `exit 0`.
- **`TestBridgeTagAppPicksHighestOfSeveralTags`:**
  - Push the refs `refs/heads/main`, `refs/tags/v1.2.0`, `refs/tags/v1.4.0` and `refs/tags/v1.3.0`.
  - Exactly one deployment is created, with `Tag == "v1.4.0"`.
- **`TestBridgeTagAppSkipsOlderTag`:**
  - With live `v1.4.0`, push `v1.3.1`.
  - The output contains `v1.3.1 is not newer than v1.4.0`, `no deploy` and `exit 0`.
- **`TestBridgeTagAppIgnoresDeletedTag`:**
  - Push `RefUpdate{Old:"abc", New:"0000000000000000000000000000000000000000", Ref:"refs/tags/v1.4.0"}`.
  - There are 0 deployments and the output contains `exit 0`.

- [ ] **Step 2: Run the tests to see them fail**

Run: `go test ./internal/githook/ -run TagApp -v`
Expected: FAIL

- [ ] **Step 3: Implement it**

In `serve`, after `resolveOrCreate`, branch on tag mode:
- If `tagRefs` is empty, print `→ stored %d ref(s); this app deploys tags matching %q, and no tag was pushed, so no deploy` and return 0.
- Otherwise call `EnqueueTagDeployment` with `Decide(tagRefs(req.Refs), inPlay)`.
  - On a skip, print `→ stored %d ref(s); <reason>, so no deploy` and return 0.
  - On a deploy, print `→ deploying tag <tag>`, then call `runDeploy`.

Branch mode keeps today's path, with `CreateDeployment` moved out to the caller. Push apps ignore `AutoDeploy`, as they do today.

- [ ] **Step 4: Run the tests to see them pass**

Run: `go test -race ./internal/githook/ -v 2>&1 | tail -5`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/githook
git commit -m "feat(githook): git push deploys a new tag on tag-mode apps"
```

---

### Task 8: Fetch tags and list them

**Files:**
- Modify: `internal/deploy/git.go`, and `internal/deploy/deploy_test.go` (the fake cloner)
- Test: `internal/deploy/git_tag_test.go`

**Interfaces:**
- Produces:
  - `Cloner` gains `ListTags(ctx context.Context, spec CloneSpec) ([]string, error)`, which returns tag names without `refs/tags/`.
  - `(*Git).ListTags` runs `git ls-remote --tags --refs -- <url>`, with the same auth environment as `Clone`. Factor that environment out into `func (g *Git) gitEnv(spec CloneSpec) (bin string, env []string, cleanup func(), err error)`.
  - The fake cloner gains the fields `tags []string`, `tagsErr error`, `listed int` (a call count) and `branches []string` (it records `spec.Branch` on every `Clone`).

- [ ] **Step 1: Write the failing tests**

These use real `git`, and are skipped when it isn't installed, as in `git_pullref_test.go`. Reuse `gitRun`.

- **`TestCloneChecksOutAnnotatedTag`:**
  - Commit A writes `v.txt=one` and is tagged with `git tag -a v1.0.0 -m r`. Commit B then writes `v.txt=two` on `main`.
  - `Clone` with `Branch: "refs/tags/v1.0.0"` gives `v.txt == "one"`.
- **`TestCloneTagNotSameNamedBranch`** (Review Focus 1):
  - The same repository, plus a branch named `v1.0.0` at commit B.
  - Cloning `refs/tags/v1.0.0` gives `one`.
- **`TestCloneTagFromBareRepo`:**
  - Run `git clone -q --bare <src> <bare>`, then `Clone` with `URL: <bare>` as a plain path (as push apps use), `Branch: "refs/tags/v1.0.0"`.
  - The result is `one`.
- **`TestListTags`:**
  - A lightweight `v1.1.0` and an annotated `v1.0.0`.
  - `ListTags` returns exactly `{v1.0.0, v1.1.0}` (compare sorted), with no `^{}` entries.

- [ ] **Step 2: Run the tests to see them fail**

Run: `go test ./internal/deploy/ -run 'Tag' -v`
Expected: FAIL. `ListTags` is undefined. The clone tests may already pass, because they exercise the existing full-ref fetch path; keep them as regression tests.

- [ ] **Step 3: Implement `ListTags` and the fake's new fields**

- [ ] **Step 4: Run the tests to see them pass**

Run: `go test ./internal/deploy/ -v 2>&1 | tail -5`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/deploy/git.go internal/deploy/git_tag_test.go internal/deploy/deploy_test.go
git commit -m "feat(deploy): list a repository's tags and fetch one by its full ref"
```

---

### Task 9: The worker builds tags

**Files:**
- Modify: `internal/deploy/pipeline.go`
- Test: `internal/deploy/deploy_tag_test.go`

**Interfaces:**
- Consumes:
  - Task 2's `PolicyFor`, `Decide`, `Newest` and `Eligible`
  - Task 5's `LiveDeployment` and `SetDeploymentTag`
  - Task 8's `Cloner.ListTags`
- Produces: `func (w *Worker) prepareTag(ctx context.Context, dep *core.Deployment, app core.App, out io.Writer) bool`. It returns false when the deployment has already been settled (cancelled or failed).

- [ ] **Step 1: Write the failing tests**

Harness helpers:
- `h.tagApp(t, name)`: `h.app`, then `UpdateAppSettings` into tag mode with pattern `v*`.
- `h.claimedTagDeployment(t, appID, tag)`: `EnqueueTagDeployment`, then `ClaimDeployment`.

- **`TestPipelineBuildsTheDeploymentsTag`:**
  - Run the pipeline on a `v1.4.0` tag deployment.
  - It ends `running`, `h.cloner.branches[0] == "refs/tags/v1.4.0"`, and the log contains `Building tag v1.4.0`.
- **`TestPipelineSkipsTagOlderThanLiveWhenClaimed`** (the builder-check scenario):
  - Run `v1.4.0` to `running`.
  - Then claim a tag-event deployment for `v1.3.1`, made with `EnqueueTagDeployment` and a `choose` that always returns `("v1.3.1", true)` to simulate it slipping in early. Run the pipeline.
  - It ends `cancelled`, with a reason starting `skipped: v1.3.1 is not newer than v1.4.0`. `len(h.cloner.branches)` is still 1.
- **`TestDeployClickBuildsNewestMatchingTag`:**
  - Set `h.cloner.tags = {"v1.2.0","v1.10.0","v2.0.0-rc.1","docs"}`, then run the pipeline on a click (`claimedDeployment`).
  - It ends `running`, `GetDeployment(...).Tag == "v1.10.0"` and `branches[0] == "refs/tags/v1.10.0"`.
- **`TestDeployClickWithNoMatchingTagFails`** (Review Focus 5):
  - Set `tags = {}`, with nothing live.
  - It ends `failed`, with the reason `no tag matches "v*"`, and nothing is cloned.
- **`TestDeployClickFallsBackToLiveTag`:**
  - With `v1.0.0` live, set `tags = {"vnext"}`, then click.
  - It builds `refs/tags/v1.0.0`.
- **`TestDeployClickNonVersionTagsNothingLiveFails`:**
  - Set `tags = {"vnext"}`, with nothing live.
  - It ends `failed`, with a reason containing `looks like a version`.
- **`TestRollbackOfTagDeployDoesNotListTags`:**
  - Use `CreateRollback` of a built `v1.0.0` row, and set `h.cloner.tagsErr` to an error.
  - It ends `running`, with `h.cloner.listed == 0`.
- **`TestComposeTagAppClonesTag`:** a compose-kind tag app (built as in `deploy_compose_test.go`) with a `v1.4.0` tag deployment gets `branches[0] == "refs/tags/v1.4.0"`.

- [ ] **Step 2: Run the tests to see them fail**

Run: `go test ./internal/deploy/ -run 'Tag|DeployClick' -v`
Expected: FAIL

- [ ] **Step 3: Implement it**

In `runPipeline`, call `prepareTag` right after `GetApp` and before the compose branch, so both pipelines get it. `prepareTag` does the following:
- **A rollback** (`dep.Image != ""`): return true at once.
- **`dep.FromTagEvent`:**
  - Decide `[dep.Tag]` against the live deployment's tag.
  - On a skip, `SetStatus(context.WithoutCancel(ctx), dep.ID, StatusBuilding, StatusCancelled, "skipped: "+reason)`, log the reason, and return false.
  - Cancelling from `building` is as legal as from `queued`, and it puts the reason in this deployment's own log.
- **`app.DeployTrigger == TriggerTag` with `dep.Tag == ""`** (a click):
  1. Call `ListTags`, with `cloneSpec(ctx, app)`.
  2. Use `Newest`. If there is none, fall back to the live deployment's tag.
  3. If there is still none, fail. The reason is `no tag matches "<pattern>"` when no tag is `Eligible`, and `no tag matching "<pattern>" looks like a version` otherwise.
  4. On success, `SetDeploymentTag`, set `dep.Tag`, and log `Deploying the newest tag matching "<pattern>": <tag>`, or for the fallback `No tag matching "<pattern>" looks like a version; redeploying the live tag <tag>`.
  5. If `ListTags` fails, fail with `list tags: <err>`.

In `cloneWorkDir`, when `dep.Tag != ""`, set `spec.Branch = "refs/tags/" + dep.Tag` and log `Building tag <tag>`.

- [ ] **Step 4: Run the tests to see them pass**

Run: `go test -race ./internal/deploy/ -v 2>&1 | tail -5`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/deploy
git commit -m "feat(deploy): build the deployment's tag, re-check tag pushes when claimed"
```

---

### Task 10: Previews always follow the PR

**Files:**
- Modify: `internal/previewmgr/manager.go:162-170`
- Test: `internal/previewmgr/fork_test.go`

**Interfaces:**
- Consumes: Task 4's `core.TriggerBranch` and `store.DeploySettings`.

- [ ] **Step 1: Write the failing test**

`TestPreviewOfTagModeParentFollowsBranch`:
- `seedGithubApp` the parent `web`, then `UpdateAppSettings` it into tag mode, keeping its branch and auto-deploy.
- Handle the event `prEvent("opened", 9, "feature-x", "acme/web", false)`.
- The child has `DeployTrigger == core.TriggerBranch` and `Branch == "feature-x"`.

- [ ] **Step 2: Run the test to see it fail**

Run: `go test ./internal/previewmgr/ -run TagModeParent -v`
Expected: FAIL. The child copies `tag`.

- [ ] **Step 3: Set `child.DeployTrigger = core.TriggerBranch` next to `child.Branch = previewRef(ev)`**

- [ ] **Step 4: Run the tests to see them pass**

Run: `go test ./internal/previewmgr/ -v 2>&1 | tail -3`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/previewmgr
git commit -m "fix(previews): a tag-mode app's previews still follow the PR head"
```

---

### Task 11: Settings form, history, hints and docs

**Files:**
- Modify:
  - `internal/server/handlers.go` (`handleAppSettings`)
  - `internal/server/templates/app.tmpl`: Deploy settings at about line 409, history at 179, Connect this repo at about 538, and the push remote at about 164
  - `internal/server/templates/deployment.tmpl:5`
  - `ARCHITECTURE.md`
- Test: `internal/server/handlers_git_test.go`

**Interfaces:**
- Consumes:
  - Task 2's `webhook.ValidGlob`
  - Task 4's `store.DeploySettings` and the `core` constants

- [ ] **Step 1: Write the failing tests**

- **`TestUpdateAppSettingsTagMode`:**
  - Post `deploy_trigger=tag`, `tag_pattern=release-*`, `tag_prereleases=on`, `branch=main` and `auto_deploy=on`.
  - It answers 303, and all of them are stored.
- **`TestUpdateAppSettingsRejectsBadTagPattern`:** the table below. Each row answers 400 with the given body substring, and the app is unchanged in the store, because validation runs before anything is saved.

| `deploy_trigger` | `tag_pattern` | body contains |
|---|---|---|
| `tag` | `""` | `A tag pattern is required` |
| `tag` | `v 1*` | `can't contain spaces` |
| `tag` | `v[0-9` | `unclosed [` |
| `nightly` | `v*` | `unknown deploy trigger` |

- **`TestUpdateAppSettingsBranchModeKeepsDefaultPattern`:** posting `deploy_trigger=branch` with an empty `tag_pattern` stores `v*`.
- **The existing `TestUpdateAppSettingsHandler`** still passes. A form with no `deploy_trigger` means branch mode.
- **`TestAppPageShowsDeploymentTag`:** a deployment with `Tag "v1.4.0"`, both in the app page's history and on `/deployments/{id}`, renders `v1.4.0`.
- **`TestAppPageTagModeHints`:**
  - A tag-mode public app with a webhook renders `Tag push events`.
  - A tag-mode GitHub-source app renders `more than three tags`.

Expected copy:
- GitLab note: "GitLab: also tick Tag push events."
- GitHub App note: "New tags matching `<pattern>` deploy automatically. GitHub sends no push events when more than three tags are pushed at once."

- [ ] **Step 2: Run the tests to see them fail**

Run: `go test ./internal/server/ -run 'AppSettings|DeploymentTag|TagModeHints' -v`
Expected: FAIL

- [ ] **Step 3: Implement the handler**

1. Parse `deploy_trigger`: empty means `branch`, and anything other than `branch` or `tag` is a 400 `unknown deploy trigger`.
2. Trim `tag_pattern`.
3. In tag mode, reject an empty pattern ("A tag pattern is required to deploy on tags."), any whitespace ("A tag pattern can't contain spaces.") and `!webhook.ValidGlob` ("The tag pattern has an unclosed [.").
4. In branch mode, an empty pattern becomes `core.DefaultTagPattern`.
5. Validate all of this **before** `UpdateAppSettings`.

- [ ] **Step 4: Implement the templates**

**Deploy settings:**
- A `Deploy when` select named `deploy_trigger`, with the options `A push to the branch` (`branch`) and `A new tag` (`tag`).
- The branch field and watch paths carry `data-trigger="branch"`.
- A new `Tag pattern` field (`tag_pattern`; hint: "Tags matching this glob deploy, if they're newer than what's live. v* matches v1.4.0.") and an `Include pre-releases` checkbox (`tag_prereleases`; hint: "Also deploy tags like v1.5.0-rc.1.") carry `data-trigger="tag"`.
- A small inline script hides the `[data-trigger]` elements that don't match the select, on load and on change, in the style of the existing inline scripts. Hidden inputs still submit, so the branch is kept.
- Fix the auto-deploy hint's "(GitHub App source only)", which is wrong for webhooks: "Deploy automatically when the source notifies Outhaul."

**History:** after the ID and the rollback marker, `{{if .Tag}} <span class="mono">{{.Tag}}</span>{{end}}`.

**Deployment page `h1`:** `{{if .Deployment.Tag}} <span class="muted h1-note">{{.Deployment.Tag}}</span>{{end}}`.

**Connect this repo:** in tag mode, add the GitLab note under the webhook secret, and swap the GitHub App sentence for the tag-mode one above.

**Push remote:** in tag mode, add `git push outhaul <tag>` under the branch push line, e.g. `git tag v1.0.0 && git push outhaul v1.0.0`.

- [ ] **Step 5: Update `ARCHITECTURE.md`**

- In the M3 paragraph, change "tag pushes and pushes to other branches no-op" to say that branch-mode apps ignore tag pushes.
- Add a `### Tag deploys (done)` section after "Push-to-deploy". In about 15 lines, cover:
  - the trigger, pattern and pre-release setting;
  - the high-water rule (live plus in-flight, current pattern only), which lives in `internal/tagdeploy` and is shared by all three sources through `store.EnqueueTagDeployment`;
  - the claim-time re-check ending a deployment as `cancelled`;
  - the Deploy click resolving the newest tag with `ls-remote`;
  - that skips show in logs and webhook response bodies only;
  - the spec's path.
- Add `tagdeploy/` to the package layout.

- [ ] **Step 6: Run everything**

Run: `go vet ./... && go test -race ./...`
Expected: no vet output, and every package `ok`.

- [ ] **Step 7: Commit**

```bash
git add internal/server ARCHITECTURE.md
git commit -m "feat(ui): choose to deploy on tags, and show each deployment's tag"
```

---

### Task 12: Check against the real thing

The spec requires this before calling the work done. Record what was checked and what couldn't be reached in the final report. Don't claim a check that wasn't run.

- [ ] **Step 1: `git push` to a running Outhaul** (`sh deploy/dev/run.sh`):
  1. Create a push app and switch it to tag mode.
  2. `git push outhaul main`: the terminal says `no deploy`.
  3. `git tag v0.1.0 && git push outhaul v0.1.0`: the deploy streams, and the history shows `v0.1.0`.
  4. Push an annotated `v0.0.9`: it is skipped, with the reason.
  5. Click Deploy: it builds `v0.1.0`.
- [ ] **Step 2: GitHub App tag push**, on a server with a public URL (e.g. the maintainer's test server):
  - A lightweight and an annotated tag each deploy.
  - Deleting a tag does nothing.
  - The skip reason shows in the App's "Recent deliveries" response body.
- [ ] **Step 3: GitLab Tag Push Hook**, on gitlab.com with the per-app webhook and "Tag push events" ticked: a tag deploys. If the server isn't reachable from gitlab.com, say so in the report.
