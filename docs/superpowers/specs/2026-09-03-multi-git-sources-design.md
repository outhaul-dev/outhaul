# Multiple connected GitHub accounts (git sources) — design

**Date:** 2026-09-03
**Status:** Implemented

## Overview

Outhaul can connect to exactly one GitHub App. The `github_app` table is a
single row (`CHECK (id = 1)`), `core.GithubApp` is documented as "the single
GitHub App record for a Outhaul instance", and four subsystems read that
singleton directly: `deploy/pipeline.go` (clone auth), `server/handlers.go`
(the create-app repo dropdown), `server/webhooks.go` (signature verification),
and `main.go`'s `previewmgr` token source (PR comments).

That is a hard ceiling on real use. Outhaul creates **private** GitHub Apps
(`"public": false` in the manifest), and GitHub only allows a private App to be
installed on the account that owns it. So one App = one account = one
installation, 1:1. Deploying repos from a personal account *and* a client org —
or from two orgs — genuinely requires two Apps. There is no shortcut.

This design replaces the singleton with a list of **git sources**: a generic
identity record plus per-kind credentials, behind a `Provider` interface.

### Goals

- Connect any number of GitHub accounts/orgs, each its own App + installation.
- Pick a repo across all connected accounts in one step, with no extra
  friction for the (common) single-account install.
- Route each inbound webhook to the source that signed it, so a push for one
  account can never deploy another account's app.
- Leave a real seam for GitLab/Bitbucket/Gitea without building any of it.
- Migrate existing installs silently: the connected App keeps working, its
  apps keep deploying, its already-installed webhook URL is unchanged.

### Non-goals (deferred, and named as seams)

- **Any second provider implementation.** The `Provider` interface ships with
  exactly one implementation: GitHub App. No dead code, no untested path.
- **Installation-token caching.** Tokens are still minted per use, as today.
- **Per-project scoping of sources.** Outhaul is single-admin; scoping would be
  organisation, not permission. Any app may use any source.
- **Renaming/labelling sources.** The account login is the name.

## Approach chosen

A generic `git_sources` table (identity, `kind`) plus a per-kind credential
table (`github_app_sources`), with a `Provider` interface over them.

Rejected alternatives:

- **A single `git_sources` table with a sealed-JSON `credentials` blob.**
  Elegant, and adding a provider needs no migration — but migrations here are
  pure embedded SQL run by `migrate(db)`, and today's secrets are sealed *per
  column* by `secret.Box`. Converting them to one blob would require crypto
  inside the migration path, i.e. a bespoke Go migration step handling the
  secret key. Not worth it.
- **A single wide table with nullable per-provider columns.** Every new
  provider widens a shared table with columns irrelevant to every other one.
- **Keeping `GithubApp` and adding a second singleton.** Doesn't generalise at
  all, and duplicates all four consumers.

The chosen split also makes the migration a straight copy of already-sealed
column values — no key handling in SQL.

## Data model

### Migration `0022_git_sources.sql`

```sql
CREATE TABLE git_sources (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    kind          TEXT NOT NULL,              -- 'github_app'; the only kind today
    account_login TEXT NOT NULL DEFAULT '',   -- 'jsmart' | 'acme-corp'
    account_type  TEXT NOT NULL DEFAULT '',   -- 'User' | 'Organization'
    created_at    TEXT NOT NULL
);

CREATE TABLE github_app_sources (
    source_id       INTEGER PRIMARY KEY REFERENCES git_sources(id) ON DELETE CASCADE,
    app_id          INTEGER NOT NULL,
    slug            TEXT NOT NULL,
    private_key     TEXT NOT NULL,   -- sealed
    webhook_secret  TEXT NOT NULL,   -- sealed
    client_id       TEXT NOT NULL,
    client_secret   TEXT NOT NULL,   -- sealed
    installation_id INTEGER NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX github_app_sources_app_id ON github_app_sources(app_id);

-- Carry the legacy singleton over, sealed values untouched.
INSERT INTO git_sources (kind, account_login, account_type, created_at)
    SELECT 'github_app', '', '', created_at FROM github_app WHERE id = 1;

INSERT INTO github_app_sources
    (source_id, app_id, slug, private_key, webhook_secret, client_id, client_secret, installation_id)
    SELECT (SELECT id FROM git_sources ORDER BY id LIMIT 1),
           app_id, slug, private_key, webhook_secret, client_id, client_secret, installation_id
    FROM github_app WHERE id = 1;

ALTER TABLE apps ADD COLUMN git_source_id INTEGER NOT NULL DEFAULT 0;

UPDATE apps SET git_source_id = (SELECT id FROM git_sources ORDER BY id LIMIT 1)
    WHERE source = 'github' AND EXISTS (SELECT 1 FROM git_sources);
```

Notes:

- `git_source_id = 0` means "no source", which is correct for every
  `public`/`ssh`/`push`/`template` app.
- On a fresh install `github_app` is empty, so both INSERTs and the UPDATE are
  no-ops and no source exists. Correct.
- `account_login` is empty after migration — GitHub never told us. It is
  backfilled lazily (see "Account backfill").

### Migration `0023_drop_github_app.sql`

```sql
DROP TABLE IF EXISTS github_app;
```

`0022` copies the legacy row rather than dropping its table, because the
legacy `github_app` table and its store methods stayed alive for the rest of
the branch — every intermediate commit still compiled and its tests still
passed while callers were migrated across one at a time. `0023` is added once
the last caller (`store.AppsByGithubRepo`) is gone, and retires the table.

### Core types

`internal/core/gitsource.go` (replacing `internal/core/github.go`):

```go
// Git source kinds: which hosting integration a source speaks.
const GitSourceGithubApp = "github_app"

// GitSource is one connected account on a Git host. Kind selects the Provider
// that can list its repos, mint its credentials, and verify its webhooks.
type GitSource struct {
    ID           int64
    Kind         string
    AccountLogin string // "" until the installation is bound
    AccountType  string // "User" | "Organization"
    CreatedAt    time.Time

    // GithubApp carries the credentials when Kind == GitSourceGithubApp.
    // Secret fields hold plaintext in memory; the store seals them at rest.
    GithubApp GithubAppCreds
}

type GithubAppCreds struct {
    AppID          int64
    Slug           string
    PrivateKey     string // PEM
    WebhookSecret  string
    ClientID       string
    ClientSecret   string
    InstallationID int64
}

// Installed reports whether the source has completed installation and can mint
// credentials. An uninstalled source exists on GitHub but grants nothing.
// For GitSourceGithubApp that is GithubApp.InstallationID != 0; an unknown
// kind is never installed.
func (s GitSource) Installed() bool

// Display returns the name to show in the UI: AccountLogin when known,
// otherwise the App slug, otherwise "(pending)".
func (s GitSource) Display() string
```

`core.App` gains `GitSourceID int64` — "git source this app's repo comes from;
0 unless Source == SourceGithub".

### Store

`internal/store/gitsources.go` replaces `internal/store/github.go`:

- `ListGitSources(ctx) ([]core.GitSource, error)` — ordered
  `ORDER BY (account_login = ''), account_login COLLATE NOCASE, id`: named
  sources alphabetically first, still-pending ones last, ties broken by id.
  Deterministic, and it matches the Settings layout below.
- `GetGitSource(ctx, id) (core.GitSource, bool, error)`
- `GitSourceByGithubAppID(ctx, appID int64) (core.GitSource, bool, error)` —
  the webhook routing lookup.
- `CreateGithubAppSource(ctx, creds core.GithubAppCreds) (core.GitSource, error)`
  — inserts both rows in one transaction; called from the manifest callback.
- `BindGithubInstallation(ctx, sourceID, installationID int64, login, accountType string) error`
- `SetGitSourceAccount(ctx, sourceID int64, login, accountType string) error`
  — lazy backfill.
- `DeleteGitSource(ctx, id) error` — `ON DELETE CASCADE` removes the creds row.
- `AppsUsingGitSource(ctx, id) ([]core.App, error)` — powers the delete guard.

`appCols` gains `git_source_id`; `scanApp`, `CreateApp`, and `UpdateAppSource`
carry it. `UpdateAppSource` takes a `gitSourceID` parameter and clears it to 0
for non-GitHub sources.

`AppsByGithubRepo(ctx, sourceID int64, fullName string)` gains the source
parameter and filters on `git_source_id = ?` as well as `source` and
`github_repo`.

## The `Provider` seam

New package `internal/gitsource`. `internal/github` remains the raw GitHub API
client; `gitsource` is the abstraction layered over it. `gitsource` must not
import `internal/deploy` or `internal/server`.

```go
// Repo is a repository a source can access.
type Repo struct {
    FullName      string // "owner/name"
    DefaultBranch string
}

// Provider is what one Git hosting integration must supply. GitHub App is the
// only implementation today; a second provider is a new file here plus its own
// credential table.
type Provider interface {
    Kind() string
    // Repos lists the repositories the source can access.
    Repos(ctx context.Context, src core.GitSource) ([]Repo, error)
    // Token returns a short-lived credential usable for both HTTPS clone and
    // API calls. It returns a bare string, not a deploy.Auth, so this package
    // stays free of deploy.
    Token(ctx context.Context, src core.GitSource) (string, error)
    // VerifyWebhook reports whether body carries a valid signature for src.
    VerifyWebhook(src core.GitSource, h http.Header, body []byte) bool
}

// Registry resolves a source to the Provider that speaks its kind.
type Registry struct{ ... }
func (r *Registry) For(kind string) (Provider, error)
```

`Token` covers clone auth *and* API calls deliberately: for GitHub that is one
object — the installation access token.

`internal/gitsource/githubapp.go` implements it over `github.Client`:
`Repos` = `AppJWT` → `InstallationToken` → `ListRepos`; `Token` = `AppJWT` →
`InstallationToken`; `VerifyWebhook` = `webhook.VerifyGitHub` against
`src.GithubApp.WebhookSecret`. It errors clearly when `!src.Installed()`.

### Wiring

The registry is built once in `main.go` and passed to the server, the deploy
worker, and the preview manager.

`Server` keeps **both** its `github.Client` and the new registry, and they do
not overlap:

- the raw `github.Client` serves the connect flow — `ExchangeManifest` and
  `Installation` — which is GitHub-App-specific by nature and has no
  cross-provider meaning;
- the registry serves everything a source does once it exists — repo listing,
  token minting, webhook verification.

`server.New` therefore gains a `*gitsource.Registry` parameter rather than
replacing `ghClient`. The deploy worker and preview manager take only the
registry; neither touches `github.Client` any more.

### `internal/github` additions

One new method on `Client` (and on `Fake`):

```go
// Installation describes one App installation and the account it belongs to.
type Installation struct {
    ID           int64
    AccountLogin string
    AccountType  string // "User" | "Organization"
}

Installation(ctx context.Context, appJWT string, installationID int64) (Installation, error)
```

`GET /app/installations/{id}`, decoding `account.login` and `account.type`.

`BuildManifest` is unchanged: the hook URL stays `/webhooks/github` for every
App, which is what keeps already-installed Apps working.

## Connect flow

### `GET /github/connect` — choose where

Renders a small form before bouncing to GitHub:

```
Connect a GitHub account

  (•) My personal account
  ( ) An organization
      [ acme-corp                ]

  Outhaul creates a GitHub App under that
  account and names this source after it.

  [ Continue to GitHub ]
```

The choice selects the target URL —
`https://github.com/settings/apps/new?state=…` or
`https://github.com/organizations/{org}/settings/apps/new?state=…` — and the
existing auto-submitting manifest form posts to it. The current handler already
accepts `?org=`; this gives it a UI instead of leaving it undiscoverable. The
org login is validated against GitHub's account-name rules
(`[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?`) before use, and the page keeps its
existing `NeedsPublicURL` branch.

### `GET /github/callback` — persist immediately

Unchanged in shape: exchange the manifest code, then
`CreateGithubAppSource(...)` and redirect to
`https://github.com/apps/{slug}/installations/new`.

The row is persisted **before** installation, exactly as today. This is
deliberate: a restart between callback and setup must not strand credentials
for an App that now exists on GitHub. Such a source shows in Settings as
"created, not installed" with a Remove button.

### `GET /github/setup` — bind the installation

GitHub sends only `installation_id` and `setup_action`; there is no `state` to
carry a source id, and appending our own query params to `setup_url` relies on
preservation behaviour we would rather not depend on. Resolve through the API
instead — a call we need anyway, since it is also where the account name comes
from:

1. If a source already holds this `installation_id`, refresh its account info
   and finish. (This is the `setup_on_update: true` re-install path.)
2. Otherwise probe each source with `installation_id = 0`, newest first: mint
   its App JWT and call `Installation(ctx, jwt, installationID)`. The App that
   owns the installation answers 200 and returns `account.login` / `type`;
   bind it with `BindGithubInstallation` and finish.
3. No match → 400 "unknown installation".

Probing is bounded by the number of *pending* sources, which is normally one.

### Account backfill

A source migrated from `github_app` has `account_login = ""`. `renderSettings`
calls a `backfillAccounts` helper: for each installed source with an empty
login, call `Installation` and `SetGitSourceAccount`. Failures are logged and
ignored — `GitSource.Display()` falls back to the App slug, so the page always
renders.

## Webhooks

`POST /webhooks/github` keeps its path — every App, old and new, posts there,
and GitHub does not let us rewrite an existing App's hook URL via API.

Routing becomes explicit rather than assumed:

```
1. Read body (existing 1 MiB cap).
2. Require X-GitHub-Hook-Installation-Target-Type == "integration".
3. appID := X-GitHub-Hook-Installation-Target-ID  → GitSourceByGithubAppID.
   Unknown or unparseable → 401. No fallback scan across secrets.
4. provider.VerifyWebhook(src, header, body) → 401 on failure.
5. push          → AppsByGithubRepo(ctx, src.ID, ev.RepoFullName), maybeDeploy each.
   pull_request  → previews.Handle(ctx, src.ID, ev).
   other         → 200 no-op.
```

Scoping the lookup by `src.ID` is the security-relevant part: a push signed by
source A can never deploy an app wired to source B, even when both cover the
same `owner/repo` (possible when one account is connected twice).

`handleAppWebhook` (the per-app generic webhook) is untouched — it identifies
the app by its own token and has nothing to do with sources.

## Deploy pipeline

`Worker.githubToken(ctx)` becomes `Worker.sourceToken(ctx, app core.App)`:
load `app.GitSourceID`, resolve the provider from the registry, call
`Token(ctx, src)`. Error messages stay specific — "app has no git source",
"git source not installed", "mint token" — since they surface in deploy logs.

`cloneSpec`'s `core.SourceGithub` branch is otherwise unchanged: the URL is
still `https://github.com/{GithubRepo}.git` with `Auth{Kind: AuthToken}`.

## Preview environments

- `previewmgr.Store.AppsByGithubRepo` gains the source-id parameter.
- `Manager.Handle(ctx, sourceID int64, ev webhook.PullRequestEvent)`.
- `TokenSource` becomes `func(ctx context.Context, sourceID int64) (string, bool, error)`;
  `main.go` implements it over the registry.
- `comment(...)` and `teardown(...)` pass `parent.GitSourceID`.
- **Preview child apps inherit `GitSourceID` from their parent** when created —
  without this a preview clones with no credentials.
- The sweeper already holds the parent app, so it has the source id.

## Create-app form

`githubRepoData` becomes `gitSourceData`, returning:

- `GitSourceConnected` (bool) — at least one installed source exists.
- `RepoGroups` — `[]RepoGroup{SourceID, AccountLogin, AccountType, Repos}`,
  ordered by account login (case-insensitive).

Rendered as a native `<select>` with one `<optgroup>` per account:

```
Source      [ GitHub App                   v ]

Repository  [ filter repos…                  ]
            ┌────────────────────────────────┐
            │ ── jsmart (personal) ──        │
            │   jsmart/outhaul               │
            │   jsmart/dotfiles              │
            │ ── acme-corp (org) ──          │
            │   acme-corp/api            ←   │
            │   acme-corp/web               │
            └────────────────────────────────┘

Branch      [ main                           ]
```

- Native `<optgroup>` keeps keyboard type-ahead working with JS disabled.
- The filter input hides non-matching options; it lives in the existing script
  block alongside the default-branch sync, and is purely additive.
- Each `<option>` carries `data-source-id`; the same change handler that
  already fills the branch also writes a hidden `git_source_id` input.
- With exactly one connected source the rendered form is identical to today's
  apart from the filter input — no account step, no added clicks.
- The **group header is the only place an account appears**, so the flat
  `owner/repo` list stays scannable. This is deliberately one step fewer than
  Dokploy/Coolify, which make you choose a provider before you can see a repo.

The app detail page's change-source form (`app.tmpl`,
`POST /apps/{id}/source`) renders the **same** grouped picker from the same
template block, so switching an existing app between accounts works exactly
like creating one. Extracting that block is the only reason `app.tmpl` and
`appform.tmpl` both change.

Server-side, `handleCreateApp` and `handleUpdateAppSource` read `github_repo`
and `git_source_id` and **re-validate**: the source must exist and be
installed, and — when a fresh repo list is cached for it — must actually
contain that repo. A stale/absent cache degrades to accepting the pair
(consistent with the existing graceful-degradation policy), since the deploy
would fail loudly anyway.

### Repo cache

`ghRepoCache` (a single slot keyed by installation id) becomes
`map[int64]*repoCache` keyed by source id, under the existing mutex, with
**per-source stale fallback**. Today one failed fetch blanks the dropdown
entirely; with N sources, one bad source must never hide the others' repos.

## Settings

The "GitHub App" panel becomes **Git sources**:

```
Git sources

  jsmart          User   outhaul-3f2a    12 repos   Installed      [Remove]
  acme-corp       Org    outhaul-91bd     4 repos   Installed      [Remove]
  (pending)              outhaul-c4e0     —         Not installed  [Finish] [Remove]

  [ Connect another account ]
```

`POST /settings/git-sources/{id}/delete` guards removal: if
`AppsUsingGitSource` returns any app, respond 400 and re-render Settings with
an error naming and linking each app. Nothing is deleted, and no running app is
ever left un-deployable by a Settings action.

```
Cannot remove acme-corp — 3 apps use this source:
  • acme-api      acme-corp/api
  • acme-web      acme-corp/web
  • acme-worker   acme-corp/jobs
Change their source or delete them first.
```

The `NeedsPublicURL` branch is preserved: without `OUTHAUL_PUBLIC_URL` no
source can be connected, and the panel says so.

## Error handling

| Situation | Behaviour |
|---|---|
| Webhook with unknown/absent App ID header | 401, logged |
| Webhook signature fails for the resolved source | 401, no fallback to other sources |
| Setup with an installation no pending source owns | 400 "unknown installation" |
| One source's repo fetch fails | That group falls back to its stale cache, or is omitted; other groups still render |
| Deploy for an app whose source was deleted | Deploy fails with "app has no git source" in the log |
| Deploy for an uninstalled source | "git source not installed" |
| Account backfill fails | Logged; UI falls back to the App slug |

## Testing

**`internal/store`**
- Source CRUD; `GitSourceByGithubAppID`; sealed fields round-trip.
- `DeleteGitSource` cascades the creds row; `AppsUsingGitSource` finds
  referencing apps.
- **Migration test:** seed a pre-0022 DB with a `github_app` row and a
  `source='github'` app, migrate, assert one `git_sources` row carrying the
  same decryptable credentials and the app backfilled to it. Assert a fresh DB
  produces zero sources.
- `AppsByGithubRepo` filters by source: same `owner/repo` under two sources
  returns only the matching one.

**`internal/gitsource`**
- GitHub App provider `Repos`/`Token`/`VerifyWebhook` against `github.Fake`.
- `Token` and `Repos` error when the source is not installed.
- `Registry.For` on an unknown kind errors.

**`internal/server`**
- Webhook routes to the source named by the App ID header; unknown id → 401;
  a body signed by source A's secret but claiming source B's App id → 401.
- A push signed by source A does not deploy an app wired to source B for the
  same repo full name.
- `gitSourceData` groups two sources; one source failing still lists the other.
- Setup binds the installation to the correct pending source when two are
  pending, via the `Installation` probe.
- Deleting a referenced source is refused and names the apps; deleting an
  unreferenced one succeeds.
- Create-app rejects a `git_source_id` that does not exist or is not installed.

**`internal/deploy`**
- `cloneSpec` mints the token from the app's own source.

**`internal/previewmgr`**
- Preview children inherit the parent's `GitSourceID`.
- `Handle` only touches apps belonging to the event's source.

## Files touched

New: `internal/store/migrations/0022_git_sources.sql`,
`internal/core/gitsource.go`, `internal/store/gitsources.go`,
`internal/gitsource/{provider.go,githubapp.go}` (+ tests).

Removed: `internal/core/github.go`, `internal/store/github.go`.

Modified: `internal/github/{client.go,real.go,fake.go}`,
`internal/core/app.go`, `internal/store/apps.go`,
`internal/server/{server.go,github.go,webhooks.go,handlers.go,settings.go}`,
`internal/deploy/pipeline.go`, `internal/previewmgr/manager.go`,
`internal/previewmgr/sweeper.go`, `main.go`,
templates `github_connect.tmpl`, `appform.tmpl`, `app.tmpl`, `settings.tmpl`,
and `ARCHITECTURE.md` + `README.md`.
