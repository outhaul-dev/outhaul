# Multiple connected GitHub accounts (git sources) — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let Outhaul connect any number of GitHub accounts/orgs, each its own GitHub App, and pick the right one automatically when deploying.

**Architecture:** The single-row `github_app` table becomes a generic `git_sources` identity table plus a per-kind credential table (`github_app_sources`), read through a `gitsource.Provider` interface with exactly one implementation (GitHub App). Apps gain `git_source_id`; webhooks resolve their source from GitHub's `X-GitHub-Hook-Installation-Target-ID` header and fan out scoped to it.

**Tech Stack:** Go 1.x, `net/http` + `html/template`, SQLite (modernc.org/sqlite) with embedded SQL migrations, NaCl secretbox via `internal/secret`, standard-library testing.

**Spec:** `docs/superpowers/specs/2026-09-03-multi-git-sources-design.md`

## Global Constraints

- **Migrations are pure SQL and append-only.** `migrate()` globs `migrations/*.sql`, applies each unseen file in one transaction, and records it in `schema_migrations`. Never edit an existing migration file. No Go code runs inside a migration, so nothing in a migration may encrypt or decrypt.
- **Secrets are sealed per column** by `secret.Box` (`s.box.Seal` / `s.box.Open`). A `Store` opened with `box == nil` must return an error rather than storing or reading a secret. Migrations copy sealed values verbatim; they never re-seal.
- **`store` depends only on `internal/core`.** `internal/gitsource` must not import `internal/deploy` or `internal/server`.
- Timestamps are RFC3339Nano TEXT via `fmtTime` / `parseTime`.
- Every task ends green: `go build ./... && go test ./...` passes before the commit.
- Test style in this repo: standard library only, table-free direct assertions, `t.Fatalf`/`t.Errorf` with the actual value in the message.

### Interim states (expected, resolved by Task 11)

Tasks land on one branch and are released together. Two conditions exist mid-branch and are deliberate:

- The legacy `github_app` table and its store methods survive until Task 11. Migration `0022` copies its row into `git_sources` but does **not** drop it, so every intermediate commit compiles and every existing test keeps passing.
- Between Task 5 (the connect flow starts writing `git_sources`) and Task 7 (the deploy pipeline starts reading it), a *newly* connected account is recorded in the new tables while the pipeline still reads the legacy row. Existing installs are unaffected because `0022` copied their row. Do not "fix" this in an earlier task; Task 7 resolves it.

---

### Task 1: `git_sources` schema, core type, and store CRUD

**Files:**
- Create: `internal/store/migrations/0022_git_sources.sql`
- Create: `internal/core/gitsource.go`
- Create: `internal/store/gitsources.go`
- Create: `internal/store/gitsources_test.go`

**Interfaces:**
- Consumes: nothing (first task).
- Produces:
  - `core.GitSourceGithubApp = "github_app"` (const string)
  - `core.GitSource{ID int64, Kind string, AccountLogin string, AccountType string, CreatedAt time.Time, GithubApp core.GithubAppCreds}`
  - `core.GithubAppCreds{AppID int64, Slug string, PrivateKey string, WebhookSecret string, ClientID string, ClientSecret string, InstallationID int64}`
  - `(core.GitSource) Installed() bool`, `(core.GitSource) Display() string`, `(core.GitSource) AccountKind() string`
  - `(*store.Store) ListGitSources(ctx) ([]core.GitSource, error)`
  - `(*store.Store) GetGitSource(ctx, id int64) (core.GitSource, bool, error)`
  - `(*store.Store) GitSourceByGithubAppID(ctx, appID int64) (core.GitSource, bool, error)`
  - `(*store.Store) CreateGithubAppSource(ctx, creds core.GithubAppCreds) (core.GitSource, error)`
  - `(*store.Store) BindGithubInstallation(ctx, sourceID, installationID int64, login, accountType string) error`
  - `(*store.Store) SetGitSourceAccount(ctx, sourceID int64, login, accountType string) error`
  - `(*store.Store) DeleteGitSource(ctx, id int64) error`

- [ ] **Step 1: Write the migration**

Create `internal/store/migrations/0022_git_sources.sql`:

```sql
-- Multiple connected Git accounts. git_sources is the generic identity record;
-- credentials live in a per-kind table so a future provider adds its own table
-- instead of widening a shared one with nullable columns. The legacy single-row
-- github_app is copied here (sealed values verbatim — migrations do no crypto)
-- and dropped in a later migration, once no code reads it.
CREATE TABLE git_sources (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    kind          TEXT NOT NULL,
    account_login TEXT NOT NULL DEFAULT '',
    account_type  TEXT NOT NULL DEFAULT '',
    created_at    TEXT NOT NULL
);

CREATE TABLE github_app_sources (
    source_id       INTEGER PRIMARY KEY REFERENCES git_sources(id) ON DELETE CASCADE,
    app_id          INTEGER NOT NULL,
    slug            TEXT    NOT NULL,
    private_key     TEXT    NOT NULL,
    webhook_secret  TEXT    NOT NULL,
    client_id       TEXT    NOT NULL,
    client_secret   TEXT    NOT NULL,
    installation_id INTEGER NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX github_app_sources_app_id ON github_app_sources(app_id);

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

- [ ] **Step 2: Write the failing store test**

Create `internal/store/gitsources_test.go`. Note `newTestStore` opens with `box == nil`, so these tests need their own store with a real box:

```go
package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/outhaul-dev/outhaul/internal/core"
	"github.com/outhaul-dev/outhaul/internal/secret"
)

// newSealedStore is newTestStore with a real secret box, which every git-source
// test needs: credentials are sealed at rest and a nil box refuses to store them.
func newSealedStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	box, err := secret.Load(filepath.Join(dir, "key"))
	if err != nil {
		t.Fatalf("secret.Load: %v", err)
	}
	s, err := Open(filepath.Join(dir, "test.db"), box)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func testCreds(appID int64, slug string) core.GithubAppCreds {
	return core.GithubAppCreds{
		AppID: appID, Slug: slug, PrivateKey: "PEM-" + slug,
		WebhookSecret: "whs-" + slug, ClientID: "cid-" + slug, ClientSecret: "csec-" + slug,
	}
}

func TestCreateGithubAppSourceRoundTripsSecrets(t *testing.T) {
	s := newSealedStore(t)
	ctx := context.Background()

	src, err := s.CreateGithubAppSource(ctx, testCreds(55, "outhaul-a"))
	if err != nil {
		t.Fatalf("CreateGithubAppSource: %v", err)
	}
	if src.ID == 0 {
		t.Fatal("created source has no id")
	}
	if src.Installed() {
		t.Error("a source with no installation must not report Installed")
	}

	got, ok, err := s.GetGitSource(ctx, src.ID)
	if err != nil || !ok {
		t.Fatalf("GetGitSource: ok=%v err=%v", ok, err)
	}
	if got.Kind != core.GitSourceGithubApp {
		t.Errorf("kind = %q, want %q", got.Kind, core.GitSourceGithubApp)
	}
	if got.GithubApp.PrivateKey != "PEM-outhaul-a" {
		t.Errorf("private key = %q, want the sealed value back", got.GithubApp.PrivateKey)
	}
	if got.GithubApp.WebhookSecret != "whs-outhaul-a" {
		t.Errorf("webhook secret = %q", got.GithubApp.WebhookSecret)
	}
	if got.GithubApp.ClientSecret != "csec-outhaul-a" {
		t.Errorf("client secret = %q", got.GithubApp.ClientSecret)
	}
}

func TestBindGithubInstallationMakesSourceInstalled(t *testing.T) {
	s := newSealedStore(t)
	ctx := context.Background()
	src, _ := s.CreateGithubAppSource(ctx, testCreds(55, "outhaul-a"))

	if err := s.BindGithubInstallation(ctx, src.ID, 9001, "acme-corp", "Organization"); err != nil {
		t.Fatalf("BindGithubInstallation: %v", err)
	}
	got, _, _ := s.GetGitSource(ctx, src.ID)
	if !got.Installed() {
		t.Error("source must report Installed after binding")
	}
	if got.GithubApp.InstallationID != 9001 {
		t.Errorf("installation id = %d, want 9001", got.GithubApp.InstallationID)
	}
	if got.AccountLogin != "acme-corp" || got.AccountType != "Organization" {
		t.Errorf("account = %q/%q, want acme-corp/Organization", got.AccountLogin, got.AccountType)
	}
	if got.Display() != "acme-corp" {
		t.Errorf("Display() = %q, want the account login", got.Display())
	}
}

func TestGitSourceByGithubAppID(t *testing.T) {
	s := newSealedStore(t)
	ctx := context.Background()
	s.CreateGithubAppSource(ctx, testCreds(55, "outhaul-a"))
	b, _ := s.CreateGithubAppSource(ctx, testCreds(77, "outhaul-b"))

	got, ok, err := s.GitSourceByGithubAppID(ctx, 77)
	if err != nil || !ok {
		t.Fatalf("GitSourceByGithubAppID: ok=%v err=%v", ok, err)
	}
	if got.ID != b.ID {
		t.Errorf("resolved source %d, want %d", got.ID, b.ID)
	}
	if _, ok, _ := s.GitSourceByGithubAppID(ctx, 999); ok {
		t.Error("unknown app id must not resolve to a source")
	}
}

// Named sources sort alphabetically ahead of still-pending ones, so the
// Settings page reads sensibly without extra sorting in the handler.
func TestListGitSourcesOrdersNamedFirst(t *testing.T) {
	s := newSealedStore(t)
	ctx := context.Background()
	pending, _ := s.CreateGithubAppSource(ctx, testCreds(1, "outhaul-pending"))
	zed, _ := s.CreateGithubAppSource(ctx, testCreds(2, "outhaul-z"))
	acme, _ := s.CreateGithubAppSource(ctx, testCreds(3, "outhaul-a"))
	s.BindGithubInstallation(ctx, zed.ID, 10, "zed", "User")
	s.BindGithubInstallation(ctx, acme.ID, 11, "acme-corp", "Organization")

	list, err := s.ListGitSources(ctx)
	if err != nil {
		t.Fatalf("ListGitSources: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("got %d sources, want 3", len(list))
	}
	if list[0].ID != acme.ID || list[1].ID != zed.ID || list[2].ID != pending.ID {
		t.Errorf("order = %d,%d,%d; want acme(%d), zed(%d), pending(%d)",
			list[0].ID, list[1].ID, list[2].ID, acme.ID, zed.ID, pending.ID)
	}
}

func TestDeleteGitSourceCascadesCredentials(t *testing.T) {
	s := newSealedStore(t)
	ctx := context.Background()
	src, _ := s.CreateGithubAppSource(ctx, testCreds(55, "outhaul-a"))

	if err := s.DeleteGitSource(ctx, src.ID); err != nil {
		t.Fatalf("DeleteGitSource: %v", err)
	}
	if _, ok, _ := s.GetGitSource(ctx, src.ID); ok {
		t.Error("source still present after delete")
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM github_app_sources WHERE source_id = ?`, src.ID).Scan(&n); err != nil {
		t.Fatalf("count credentials: %v", err)
	}
	if n != 0 {
		t.Errorf("credential rows left behind: %d", n)
	}
}

func TestFreshDatabaseHasNoGitSources(t *testing.T) {
	s := newSealedStore(t)
	list, err := s.ListGitSources(context.Background())
	if err != nil {
		t.Fatalf("ListGitSources: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("fresh database has %d sources, want 0", len(list))
	}
}
```

- [ ] **Step 3: Run the tests to verify they fail**

Run: `go test ./internal/store/ -run 'GitSource|GithubAppSource' -v`
Expected: FAIL — compile error, `s.CreateGithubAppSource undefined`, `core.GitSource undefined`.

- [ ] **Step 4: Write `internal/core/gitsource.go`**

```go
package core

import "time"

// GitSourceGithubApp is the only git source kind today: a GitHub App plus the
// single installation it was created for. Kind selects the gitsource.Provider
// that can list a source's repos, mint its credentials, and verify its webhooks.
const GitSourceGithubApp = "github_app"

// GitSource is one connected account on a Git host. Outhaul creates private
// GitHub Apps, and GitHub only installs a private App on the account that owns
// it — so one source is one App is one account.
type GitSource struct {
	ID           int64
	Kind         string
	AccountLogin string // "" until the installation is bound
	AccountType  string // "User" | "Organization"
	CreatedAt    time.Time

	// GithubApp carries the credentials when Kind == GitSourceGithubApp.
	GithubApp GithubAppCreds
}

// GithubAppCreds are a GitHub App's credentials. Secret fields hold plaintext
// in memory (decrypted); the store seals them at rest.
type GithubAppCreds struct {
	AppID          int64
	Slug           string
	PrivateKey     string // PEM
	WebhookSecret  string
	ClientID       string
	ClientSecret   string
	InstallationID int64
}

// Installed reports whether the source finished installation and can mint
// credentials. An uninstalled source exists on GitHub but grants nothing.
func (s GitSource) Installed() bool {
	switch s.Kind {
	case GitSourceGithubApp:
		return s.GithubApp.InstallationID != 0
	default:
		return false
	}
}

// Display is the name to show in the UI: the account login once GitHub has
// told us, the App slug while it has not, and a marker if neither is known.
func (s GitSource) Display() string {
	if s.AccountLogin != "" {
		return s.AccountLogin
	}
	if s.GithubApp.Slug != "" {
		return s.GithubApp.Slug
	}
	return "(pending)"
}

// AccountKind renders AccountType for the UI; "" when the account is unknown.
func (s GitSource) AccountKind() string {
	switch s.AccountType {
	case "Organization":
		return "org"
	case "User":
		return "personal"
	}
	return ""
}
```

- [ ] **Step 5: Write `internal/store/gitsources.go`**

```go
package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/outhaul-dev/outhaul/internal/core"
)

// gitSourceSelect reads a source with its per-kind credentials. The LEFT JOIN
// keeps a source row readable even when this build knows nothing about its
// kind, so an unknown kind degrades to "not installed" rather than an error.
const gitSourceSelect = `SELECT
	s.id, s.kind, s.account_login, s.account_type, s.created_at,
	COALESCE(g.app_id, 0), COALESCE(g.slug, ''), COALESCE(g.private_key, ''),
	COALESCE(g.webhook_secret, ''), COALESCE(g.client_id, ''),
	COALESCE(g.client_secret, ''), COALESCE(g.installation_id, 0)
	FROM git_sources s LEFT JOIN github_app_sources g ON g.source_id = s.id`

// ListGitSources returns every connected source: named ones alphabetically
// first, still-pending ones last.
func (s *Store) ListGitSources(ctx context.Context) ([]core.GitSource, error) {
	rows, err := s.db.QueryContext(ctx, gitSourceSelect+
		` ORDER BY (s.account_login = ''), s.account_login COLLATE NOCASE, s.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []core.GitSource
	for rows.Next() {
		src, err := s.scanGitSource(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, src)
	}
	return out, rows.Err()
}

// GetGitSource returns one source by id, or ok=false if there is none.
func (s *Store) GetGitSource(ctx context.Context, id int64) (core.GitSource, bool, error) {
	return s.oneGitSource(ctx, gitSourceSelect+` WHERE s.id = ?`, id)
}

// GitSourceByGithubAppID resolves the source that owns a GitHub App id. This is
// the webhook routing lookup: GitHub names the App in every delivery header.
func (s *Store) GitSourceByGithubAppID(ctx context.Context, appID int64) (core.GitSource, bool, error) {
	return s.oneGitSource(ctx, gitSourceSelect+` WHERE g.app_id = ?`, appID)
}

func (s *Store) oneGitSource(ctx context.Context, query string, arg any) (core.GitSource, bool, error) {
	src, err := s.scanGitSource(s.db.QueryRowContext(ctx, query, arg))
	if err == sql.ErrNoRows {
		return core.GitSource{}, false, nil
	}
	if err != nil {
		return core.GitSource{}, false, err
	}
	return src, true, nil
}

func (s *Store) scanGitSource(row scanner) (core.GitSource, error) {
	var (
		src                 core.GitSource
		createdAt           string
		encPK, encWH, encCS string
	)
	if err := row.Scan(&src.ID, &src.Kind, &src.AccountLogin, &src.AccountType, &createdAt,
		&src.GithubApp.AppID, &src.GithubApp.Slug, &encPK, &encWH,
		&src.GithubApp.ClientID, &encCS, &src.GithubApp.InstallationID); err != nil {
		return core.GitSource{}, err
	}
	if t, err := parseTime(createdAt); err == nil {
		src.CreatedAt = t
	}
	if src.Kind != core.GitSourceGithubApp {
		return src, nil
	}
	if s.box == nil {
		return core.GitSource{}, fmt.Errorf("store: no secret box configured; cannot read git source")
	}
	pk, err := s.box.Open(encPK)
	if err != nil {
		return core.GitSource{}, fmt.Errorf("decrypt private_key: %w", err)
	}
	wh, err := s.box.Open(encWH)
	if err != nil {
		return core.GitSource{}, fmt.Errorf("decrypt webhook_secret: %w", err)
	}
	cs, err := s.box.Open(encCS)
	if err != nil {
		return core.GitSource{}, fmt.Errorf("decrypt client_secret: %w", err)
	}
	src.GithubApp.PrivateKey = string(pk)
	src.GithubApp.WebhookSecret = string(wh)
	src.GithubApp.ClientSecret = string(cs)
	return src, nil
}

// CreateGithubAppSource records a freshly-created GitHub App, before it has been
// installed. Both rows land in one transaction so a source never exists without
// its credentials.
func (s *Store) CreateGithubAppSource(ctx context.Context, creds core.GithubAppCreds) (core.GitSource, error) {
	if s.box == nil {
		return core.GitSource{}, fmt.Errorf("store: no secret box configured; cannot store git source")
	}
	encPK, err := s.box.Seal([]byte(creds.PrivateKey))
	if err != nil {
		return core.GitSource{}, err
	}
	encWH, err := s.box.Seal([]byte(creds.WebhookSecret))
	if err != nil {
		return core.GitSource{}, err
	}
	encCS, err := s.box.Seal([]byte(creds.ClientSecret))
	if err != nil {
		return core.GitSource{}, err
	}
	now := time.Now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return core.GitSource{}, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx,
		`INSERT INTO git_sources (kind, account_login, account_type, created_at) VALUES (?, '', '', ?)`,
		core.GitSourceGithubApp, fmtTime(now))
	if err != nil {
		return core.GitSource{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return core.GitSource{}, err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO github_app_sources
		   (source_id, app_id, slug, private_key, webhook_secret, client_id, client_secret, installation_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		id, creds.AppID, creds.Slug, encPK, encWH, creds.ClientID, encCS, creds.InstallationID); err != nil {
		return core.GitSource{}, err
	}
	if err := tx.Commit(); err != nil {
		return core.GitSource{}, err
	}
	return core.GitSource{
		ID: id, Kind: core.GitSourceGithubApp, CreatedAt: now, GithubApp: creds,
	}, nil
}

// BindGithubInstallation completes a source: it records the installation the
// operator chose on GitHub, and the account that installation belongs to.
func (s *Store) BindGithubInstallation(ctx context.Context, sourceID, installationID int64, login, accountType string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`UPDATE github_app_sources SET installation_id = ? WHERE source_id = ?`,
		installationID, sourceID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE git_sources SET account_login = ?, account_type = ? WHERE id = ?`,
		login, accountType, sourceID); err != nil {
		return err
	}
	return tx.Commit()
}

// SetGitSourceAccount backfills the account a source belongs to. Sources
// migrated from the pre-0022 single-App record have no account recorded,
// because GitHub was never asked.
func (s *Store) SetGitSourceAccount(ctx context.Context, sourceID int64, login, accountType string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE git_sources SET account_login = ?, account_type = ? WHERE id = ?`,
		login, accountType, sourceID)
	return err
}

// DeleteGitSource removes a source; its credential row cascades. Callers must
// refuse to call this while apps still reference the source.
func (s *Store) DeleteGitSource(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM git_sources WHERE id = ?`, id)
	return err
}
```

- [ ] **Step 6: Run the tests to verify they pass**

Run: `go test ./internal/store/ -run 'GitSource|GithubAppSource' -v`
Expected: PASS (6 tests).

- [ ] **Step 7: Write the migration test**

Append to `internal/store/gitsources_test.go`:

```go
// A pre-0022 database must come out the other side with its App as source #1
// and its GitHub-sourced apps pointing at it — the sealed credentials copied
// verbatim, since migrations cannot decrypt.
func TestMigrationCarriesLegacyGithubApp(t *testing.T) {
	dir := t.TempDir()
	box, err := secret.Load(filepath.Join(dir, "key"))
	if err != nil {
		t.Fatalf("secret.Load: %v", err)
	}
	path := filepath.Join(dir, "test.db")

	// Open once to get a fully migrated schema, then fake a pre-0022 state by
	// re-populating github_app and clearing what 0022 derived from it.
	s, err := Open(path, box)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ctx := context.Background()
	app, err := s.CreateApp(ctx, core.App{
		Name: "web", Domain: "web.test", Source: core.SourceGithub,
		GithubRepo: "acme-corp/api", WebhookSecret: "w",
	})
	if err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	encPK, _ := box.Seal([]byte("LEGACY-PEM"))
	encWH, _ := box.Seal([]byte("LEGACY-WHS"))
	encCS, _ := box.Seal([]byte("LEGACY-CSEC"))
	if _, err := s.db.Exec(
		`INSERT INTO github_app (id, app_id, slug, private_key, webhook_secret, client_id, client_secret, installation_id, created_at)
		 VALUES (1, 42, 'outhaul-legacy', ?, ?, 'cid', ?, 777, ?)`,
		encPK, encWH, encCS, fmtTime(time.Now().UTC())); err != nil {
		t.Fatalf("seed github_app: %v", err)
	}
	if _, err := s.db.Exec(`DELETE FROM git_sources`); err != nil {
		t.Fatalf("clear git_sources: %v", err)
	}
	if _, err := s.db.Exec(`UPDATE apps SET git_source_id = 0`); err != nil {
		t.Fatalf("clear git_source_id: %v", err)
	}
	if _, err := s.db.Exec(`DELETE FROM schema_migrations WHERE name = ?`, "migrations/0022_git_sources.sql"); err != nil {
		t.Fatalf("unrecord 0022: %v", err)
	}
	// 0022 is written to run against a schema without its own tables *or* the
	// column it adds — replaying it over an existing git_source_id column fails
	// with "duplicate column name".
	for _, stmt := range []string{
		`DROP TABLE github_app_sources`,
		`DROP TABLE git_sources`,
		`ALTER TABLE apps DROP COLUMN git_source_id`,
	} {
		if _, err := s.db.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	s.Close()

	// Re-open: 0022 replays against the seeded legacy row.
	s2, err := Open(path, box)
	if err != nil {
		t.Fatalf("re-Open: %v", err)
	}
	defer s2.Close()

	list, err := s2.ListGitSources(ctx)
	if err != nil {
		t.Fatalf("ListGitSources: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("got %d sources after migration, want 1", len(list))
	}
	got := list[0]
	if got.GithubApp.AppID != 42 || got.GithubApp.Slug != "outhaul-legacy" {
		t.Errorf("app = %d/%q, want 42/outhaul-legacy", got.GithubApp.AppID, got.GithubApp.Slug)
	}
	if !got.Installed() || got.GithubApp.InstallationID != 777 {
		t.Errorf("installation = %d, want 777 and Installed", got.GithubApp.InstallationID)
	}
	if got.GithubApp.PrivateKey != "LEGACY-PEM" || got.GithubApp.WebhookSecret != "LEGACY-WHS" {
		t.Errorf("secrets did not survive the copy: %q / %q",
			got.GithubApp.PrivateKey, got.GithubApp.WebhookSecret)
	}

	var sourceID int64
	if err := s2.db.QueryRow(`SELECT git_source_id FROM apps WHERE id = ?`, app.ID).Scan(&sourceID); err != nil {
		t.Fatalf("read git_source_id: %v", err)
	}
	if sourceID != got.ID {
		t.Errorf("app backfilled to source %d, want %d", sourceID, got.ID)
	}
}
```

Add `"time"` to the test file's imports.

- [ ] **Step 8: Run the migration test**

Run: `go test ./internal/store/ -run TestMigrationCarriesLegacyGithubApp -v`
Expected: PASS.

- [ ] **Step 9: Run the full suite and commit**

Run: `go build ./... && go test ./...`
Expected: PASS — nothing else references the new tables yet.

```bash
git add internal/store/migrations/0022_git_sources.sql internal/core/gitsource.go \
        internal/store/gitsources.go internal/store/gitsources_test.go
git commit -m "feat(store): git_sources table, core.GitSource, and source CRUD

Adds the generic git_sources identity table plus a per-kind
github_app_sources credential table, and copies the legacy single-row
github_app into it. The old table stays until nothing reads it.

Splitting credentials into a per-kind table keeps the migration a pure
SQL copy of already-sealed values: migrations run no Go, so they cannot
re-encrypt into a combined blob."
```

---

### Task 2: Link apps to their source

**Files:**
- Modify: `internal/store/apps.go` (`appCols` line 15, `CreateApp` insert, `UpdateAppSource` line 211, `AppsByGithubRepo` line 145, `scanApp` line 281)
- Modify: `internal/server/handlers.go:451` (the one `UpdateAppSource` call site)
- Create: `internal/store/apps_gitsource_test.go`

**Interfaces:**
- Consumes: `core.GitSource`, `store.CreateGithubAppSource` (Task 1).
- Produces:
  - `core.App.GitSourceID int64`
  - `(*store.Store) AppsByGithubRepoSource(ctx, sourceID int64, fullName string) ([]core.App, error)`
  - `(*store.Store) AppsUsingGitSource(ctx, sourceID int64) ([]core.App, error)`
  - `(*store.Store) UpdateAppSource(ctx, id int64, source, repoURL, githubRepo string, gitSourceID int64, sshPublicKey, sshPrivateKey string) error` — **signature changed**, `gitSourceID` inserted after `githubRepo`

The existing unscoped `AppsByGithubRepo` stays for now; Task 11 removes it once both callers have moved.

- [ ] **Step 1: Write the failing test**

Create `internal/store/apps_gitsource_test.go`:

```go
package store

import (
	"context"
	"testing"

	"github.com/outhaul-dev/outhaul/internal/core"
)

func TestAppRoundTripsGitSourceID(t *testing.T) {
	s := newSealedStore(t)
	ctx := context.Background()
	src, _ := s.CreateGithubAppSource(ctx, testCreds(55, "outhaul-a"))

	app, err := s.CreateApp(ctx, core.App{
		Name: "web", Domain: "web.test", Source: core.SourceGithub,
		GithubRepo: "acme-corp/api", GitSourceID: src.ID, WebhookSecret: "w",
	})
	if err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	got, err := s.GetApp(ctx, app.ID)
	if err != nil {
		t.Fatalf("GetApp: %v", err)
	}
	if got.GitSourceID != src.ID {
		t.Errorf("GitSourceID = %d, want %d", got.GitSourceID, src.ID)
	}
}

// Two accounts can each be connected, and nothing stops the same repo full name
// existing under both. A push for one must never reach the other's app.
func TestAppsByGithubRepoSourceScopesToOneSource(t *testing.T) {
	s := newSealedStore(t)
	ctx := context.Background()
	a, _ := s.CreateGithubAppSource(ctx, testCreds(55, "outhaul-a"))
	b, _ := s.CreateGithubAppSource(ctx, testCreds(77, "outhaul-b"))

	appA, _ := s.CreateApp(ctx, core.App{
		Name: "a-web", Domain: "a.test", Source: core.SourceGithub,
		GithubRepo: "acme-corp/api", GitSourceID: a.ID, WebhookSecret: "w1",
	})
	s.CreateApp(ctx, core.App{
		Name: "b-web", Domain: "b.test", Source: core.SourceGithub,
		GithubRepo: "acme-corp/api", GitSourceID: b.ID, WebhookSecret: "w2",
	})

	got, err := s.AppsByGithubRepoSource(ctx, a.ID, "acme-corp/api")
	if err != nil {
		t.Fatalf("AppsByGithubRepoSource: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d apps, want only source A's", len(got))
	}
	if got[0].ID != appA.ID {
		t.Errorf("matched app %d, want %d", got[0].ID, appA.ID)
	}
}

func TestAppsUsingGitSource(t *testing.T) {
	s := newSealedStore(t)
	ctx := context.Background()
	src, _ := s.CreateGithubAppSource(ctx, testCreds(55, "outhaul-a"))
	s.CreateApp(ctx, core.App{
		Name: "web", Domain: "web.test", Source: core.SourceGithub,
		GithubRepo: "acme-corp/api", GitSourceID: src.ID, WebhookSecret: "w",
	})
	s.CreateApp(ctx, core.App{
		Name: "plain", Domain: "plain.test", Source: core.SourcePublic,
		RepoURL: "https://example.com/r.git", WebhookSecret: "w2",
	})

	users, err := s.AppsUsingGitSource(ctx, src.ID)
	if err != nil {
		t.Fatalf("AppsUsingGitSource: %v", err)
	}
	if len(users) != 1 || users[0].Name != "web" {
		t.Fatalf("got %d apps (%v), want just web", len(users), users)
	}
	none, _ := s.AppsUsingGitSource(ctx, 4242)
	if len(none) != 0 {
		t.Errorf("unreferenced source reported %d apps", len(none))
	}
}

func TestUpdateAppSourceClearsGitSourceForNonGithub(t *testing.T) {
	s := newSealedStore(t)
	ctx := context.Background()
	src, _ := s.CreateGithubAppSource(ctx, testCreds(55, "outhaul-a"))
	app, _ := s.CreateApp(ctx, core.App{
		Name: "web", Domain: "web.test", Source: core.SourceGithub,
		GithubRepo: "acme-corp/api", GitSourceID: src.ID, WebhookSecret: "w",
	})

	if err := s.UpdateAppSource(ctx, app.ID, core.SourcePublic,
		"https://example.com/r.git", "", 0, "", ""); err != nil {
		t.Fatalf("UpdateAppSource: %v", err)
	}
	got, _ := s.GetApp(ctx, app.ID)
	if got.GitSourceID != 0 {
		t.Errorf("GitSourceID = %d after moving off GitHub, want 0", got.GitSourceID)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/store/ -run 'GitSourceID|AppsByGithubRepoSource|AppsUsingGitSource' -v`
Expected: FAIL — `app.GitSourceID undefined`, `s.AppsByGithubRepoSource undefined`.

- [ ] **Step 3: Add the field to `core.App`**

In `internal/core/app.go`, immediately after the `GithubRepo` field:

```go
	GithubRepo    string // "owner/name" when Source == SourceGithub
	GitSourceID   int64  // git source supplying the repo; 0 unless Source == SourceGithub
```

- [ ] **Step 4: Plumb the column through `internal/store/apps.go`**

`appCols` (line 15) — insert `git_source_id` after `github_repo`:

```go
const appCols = `id, project_id, name, repo_url, domain, created_at, branch, auto_deploy, source, webhook_secret, ssh_public_key, github_repo, git_source_id, kind, compose_path, dockerfile_path, watch_paths, template_id, compose_raw, parent_id, pr_number, ephemeral, preview_status`
```

`scanApp` — insert `&app.GitSourceID` in the matching position:

```go
	if err := row.Scan(&app.ID, &app.ProjectID, &app.Name, &app.RepoURL, &app.Domain, &createdAt,
		&app.Branch, &autoDeploy, &app.Source, &app.WebhookSecret, &app.SSHPublicKey, &app.GithubRepo,
		&app.GitSourceID,
		&app.Kind, &app.ComposePath, &app.DockerfilePath, &watchPaths, &app.TemplateID, &app.ComposeRaw,
		&app.ParentID, &app.PRNumber, &ephemeral, &app.PreviewStatus); err != nil {
```

`CreateApp` — add the column, one more `?`, and the value:

```go
	res, err := tx.ExecContext(ctx,
		`INSERT INTO apps
		   (project_id, name, repo_url, domain, created_at, branch, auto_deploy, source, webhook_secret, ssh_private_key, ssh_public_key, github_repo, git_source_id,
		    kind, compose_path, dockerfile_path, watch_paths, template_id, compose_raw, parent_id, pr_number, ephemeral, preview_status)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		app.ProjectID, app.Name, app.RepoURL, app.Domain, fmtTime(app.CreatedAt),
		app.Branch, boolToInt(app.AutoDeploy), app.Source, app.WebhookSecret, encKey, app.SSHPublicKey, app.GithubRepo, app.GitSourceID,
		app.Kind, app.ComposePath, app.DockerfilePath, joinWatchPaths(app.WatchPaths), app.TemplateID, app.ComposeRaw,
		app.ParentID, app.PRNumber, boolToInt(app.Ephemeral), app.PreviewStatus)
```

Count the placeholders after editing: 23 columns, 23 `?`.

`UpdateAppSource` — new parameter and column:

```go
func (s *Store) UpdateAppSource(ctx context.Context, id int64, source, repoURL, githubRepo string, gitSourceID int64, sshPublicKey, sshPrivateKey string) error {
	enc, err := s.sealMaybe(sshPrivateKey)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx,
		`UPDATE apps SET source = ?, repo_url = ?, github_repo = ?, git_source_id = ?, ssh_public_key = ?, ssh_private_key = ? WHERE id = ?`,
		source, repoURL, githubRepo, gitSourceID, sshPublicKey, enc, id)
	return err
}
```

- [ ] **Step 5: Add the two new lookups**

Immediately after `AppsByGithubRepo` in `internal/store/apps.go`:

```go
// AppsByGithubRepoSource returns the apps sourced from "owner/name" *through a
// particular git source*. Scoping by source is what stops a push signed by one
// connected account from deploying another account's identically-named repo.
func (s *Store) AppsByGithubRepoSource(ctx context.Context, sourceID int64, fullName string) ([]core.App, error) {
	return s.appsQuery(ctx,
		`SELECT `+appCols+` FROM apps WHERE source = ? AND github_repo = ? AND git_source_id = ?`,
		core.SourceGithub, fullName, sourceID)
}

// AppsUsingGitSource returns every app that depends on a source, ordered by
// name. Removing a source is refused while this is non-empty.
func (s *Store) AppsUsingGitSource(ctx context.Context, sourceID int64) ([]core.App, error) {
	return s.appsQuery(ctx,
		`SELECT `+appCols+` FROM apps WHERE git_source_id = ? ORDER BY name`, sourceID)
}

// appsQuery runs a query returning appCols and scans every row.
func (s *Store) appsQuery(ctx context.Context, query string, args ...any) ([]core.App, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var apps []core.App
	for rows.Next() {
		app, err := scanApp(rows)
		if err != nil {
			return nil, err
		}
		apps = append(apps, app)
	}
	return apps, rows.Err()
}
```

- [ ] **Step 6: Fix the one `UpdateAppSource` call site**

`internal/server/handlers.go:451` — pass `0` for now; Task 9 supplies the real source id:

```go
	if err := s.store.UpdateAppSource(r.Context(), id, source, repo, githubRepo, 0, pub, priv); err != nil {
```

- [ ] **Step 7: Run tests to verify they pass**

Run: `go build ./... && go test ./internal/store/ ./internal/server/ -v`
Expected: PASS. If an existing store test fails on column count, re-check that `appCols`, the `INSERT` column list, the `?` count, and `scanApp`'s argument order all place `git_source_id` in the same position.

- [ ] **Step 8: Commit**

```bash
git add internal/core/app.go internal/store/apps.go internal/store/apps_gitsource_test.go internal/server/handlers.go
git commit -m "feat(store): link apps to the git source their repo comes from

Adds App.GitSourceID plus source-scoped lookups. Scoping repo lookups by
source is the security-relevant half: two connected accounts can expose
the same owner/repo, and a push for one must not deploy the other's app."
```

---

### Task 3: `github.Client.Installation`

**Files:**
- Modify: `internal/github/client.go` (add type + interface method)
- Modify: `internal/github/real.go` (HTTP implementation)
- Modify: `internal/github/fake.go` (fake + `iss` decoding)
- Create: `internal/github/installation_test.go`

**Interfaces:**
- Consumes: `github.AppJWT` (existing).
- Produces:
  - `github.Installation{ID int64, AccountLogin string, AccountType string}`
  - `github.Client.Installation(ctx, appJWT string, installationID int64) (Installation, error)`
  - `github.Fake.InstallationsByApp map[int64][]Installation` — keyed by App ID
  - `github.Fake.InstallationErr error`

- [ ] **Step 1: Write the failing test**

Create `internal/github/installation_test.go`:

```go
package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHTTPClientInstallationReadsAccount(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/app/installations/9001" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer JWT" {
			t.Errorf("auth = %q, want the App JWT", got)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"id":      9001,
			"account": map[string]any{"login": "acme-corp", "type": "Organization"},
		})
	}))
	defer srv.Close()

	c := &HTTPClient{BaseURL: srv.URL, HTTP: srv.Client()}
	got, err := c.Installation(context.Background(), "JWT", 9001)
	if err != nil {
		t.Fatalf("Installation: %v", err)
	}
	if got.ID != 9001 || got.AccountLogin != "acme-corp" || got.AccountType != "Organization" {
		t.Errorf("got %+v", got)
	}
}

// The fake scopes installations to the calling App the way GitHub does, by
// reading the App id out of the JWT's iss claim.
func TestFakeInstallationScopedToCallingApp(t *testing.T) {
	pem := testRSAKeyPEM(t)
	f := &Fake{InstallationsByApp: map[int64][]Installation{
		77: {{ID: 9001, AccountLogin: "acme-corp", AccountType: "Organization"}},
	}}

	jwtOwner, err := AppJWT(pem, 77, timeNowForTest())
	if err != nil {
		t.Fatalf("AppJWT: %v", err)
	}
	got, err := f.Installation(context.Background(), jwtOwner, 9001)
	if err != nil {
		t.Fatalf("owner App got an error: %v", err)
	}
	if got.AccountLogin != "acme-corp" {
		t.Errorf("account = %q", got.AccountLogin)
	}

	jwtOther, _ := AppJWT(pem, 55, timeNowForTest())
	if _, err := f.Installation(context.Background(), jwtOther, 9001); err == nil {
		t.Error("an App that does not own the installation must get an error")
	}
}
```

Add these helpers to the same file (`testRSAKeyPEM` may already exist in `jwt_test.go` — check first and reuse it rather than duplicating):

```go
func timeNowForTest() time.Time { return time.Unix(1_700_000_000, 0) }

// testRSAKeyPEM generates a PKCS#1 RSA key PEM. AppJWT needs a real key.
func testRSAKeyPEM(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{
		Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key),
	}))
}
```

Imports for those helpers: `crypto/rand`, `crypto/rsa`, `crypto/x509`, `encoding/pem`, `time`.

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/github/ -run Installation -v`
Expected: FAIL — `c.Installation undefined`.

- [ ] **Step 3: Extend the interface**

In `internal/github/client.go`, add the type and the method:

```go
// Installation is one GitHub App installation and the account it belongs to.
// A private App has exactly one, on the account that owns the App.
type Installation struct {
	ID           int64
	AccountLogin string
	AccountType  string // "User" | "Organization"
}
```

and inside `type Client interface`:

```go
	// Installation describes one installation. GitHub scopes this to the App
	// the JWT authenticates as, so a 404 means "this App does not own it" —
	// which is how an installation is matched back to its source.
	Installation(ctx context.Context, appJWT string, installationID int64) (Installation, error)
```

- [ ] **Step 4: Implement it on `HTTPClient`**

Append to `internal/github/real.go`:

```go
func (c *HTTPClient) Installation(ctx context.Context, appJWT string, installationID int64) (Installation, error) {
	var r struct {
		ID      int64 `json:"id"`
		Account struct {
			Login string `json:"login"`
			Type  string `json:"type"`
		} `json:"account"`
	}
	url := fmt.Sprintf("%s/app/installations/%d", c.BaseURL, installationID)
	if err := c.do(ctx, http.MethodGet, url, "Bearer "+appJWT, &r); err != nil {
		return Installation{}, err
	}
	return Installation{ID: r.ID, AccountLogin: r.Account.Login, AccountType: r.Account.Type}, nil
}
```

- [ ] **Step 5: Implement it on `Fake`**

Add the fields to the `Fake` struct:

```go
	// InstallationsByApp maps App id -> the installations that App owns.
	InstallationsByApp map[int64][]Installation
	InstallationErr    error
```

and append:

```go
func (f *Fake) Installation(ctx context.Context, appJWT string, installationID int64) (Installation, error) {
	f.LastJWT = appJWT
	if f.InstallationErr != nil {
		return Installation{}, f.InstallationErr
	}
	for _, inst := range f.InstallationsByApp[issFromJWT(appJWT)] {
		if inst.ID == installationID {
			return inst, nil
		}
	}
	return Installation{}, fmt.Errorf("github: installation %d not found for this app", installationID)
}

// issFromJWT reads the App id from an unverified JWT payload. The fake needs it
// to scope installations to the calling App the way GitHub's API does; nothing
// in production trusts a JWT this way.
func issFromJWT(token string) int64 {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return 0
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return 0
	}
	var claims struct {
		Iss int64 `json:"iss"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return 0
	}
	return claims.Iss
}
```

Add `encoding/base64`, `encoding/json`, and `strings` to `fake.go`'s imports.

- [ ] **Step 6: Run to verify it passes**

Run: `go test ./internal/github/ -v`
Expected: PASS. `internal/server`'s `github.Fake` use still compiles because the new fields are optional.

- [ ] **Step 7: Commit**

```bash
git add internal/github/client.go internal/github/real.go internal/github/fake.go internal/github/installation_test.go
git commit -m "feat(github): read an installation and the account it belongs to

GET /app/installations/{id} is scoped by GitHub to the calling App, which
makes it both the source of a connected account's name and the way an
installation is matched back to the App that owns it."
```

---

### Task 4: `internal/gitsource` — the Provider seam

**Files:**
- Create: `internal/gitsource/provider.go`
- Create: `internal/gitsource/githubapp.go`
- Create: `internal/gitsource/githubapp_test.go`

**Interfaces:**
- Consumes: `core.GitSource`, `core.GitSourceGithubApp` (Task 1); `github.Client`, `github.AppJWT` (Task 3); `webhook.VerifyGitHub` (existing).
- Produces:
  - `gitsource.Repo{FullName string, DefaultBranch string}`
  - `gitsource.Provider` interface: `Kind() string`, `Repos(ctx, core.GitSource) ([]Repo, error)`, `Token(ctx, core.GitSource) (string, error)`, `VerifyWebhook(core.GitSource, http.Header, []byte) bool`
  - `gitsource.NewRegistry(providers ...Provider) *Registry`
  - `(*Registry) For(kind string) (Provider, error)`
  - `(*Registry) TokenFor(ctx, core.GitSource) (string, error)` — convenience used by deploy and previews
  - `gitsource.NewGithubApp(c github.Client) *GithubApp`

- [ ] **Step 1: Write the failing test**

Create `internal/gitsource/githubapp_test.go`:

```go
package gitsource

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/hmac"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"net/http"
	"testing"

	"github.com/outhaul-dev/outhaul/internal/core"
	"github.com/outhaul-dev/outhaul/internal/github"
)

func testKeyPEM(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{
		Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key),
	}))
}

func installedSource(t *testing.T) core.GitSource {
	t.Helper()
	return core.GitSource{
		ID: 1, Kind: core.GitSourceGithubApp, AccountLogin: "acme-corp", AccountType: "Organization",
		GithubApp: core.GithubAppCreds{
			AppID: 77, Slug: "outhaul-a", PrivateKey: testKeyPEM(t),
			WebhookSecret: "whs", InstallationID: 9001,
		},
	}
}

func TestGithubAppTokenMintsForTheSourcesInstallation(t *testing.T) {
	f := &github.Fake{Token: "ghs_abc"}
	p := NewGithubApp(f)

	tok, err := p.Token(context.Background(), installedSource(t))
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if tok != "ghs_abc" {
		t.Errorf("token = %q", tok)
	}
	if f.LastInstallationID != 9001 {
		t.Errorf("minted for installation %d, want 9001", f.LastInstallationID)
	}
}

func TestGithubAppRefusesUninstalledSource(t *testing.T) {
	src := installedSource(t)
	src.GithubApp.InstallationID = 0
	p := NewGithubApp(&github.Fake{Token: "ghs_abc"})

	if _, err := p.Token(context.Background(), src); err == nil {
		t.Error("Token on an uninstalled source must fail")
	}
	if _, err := p.Repos(context.Background(), src); err == nil {
		t.Error("Repos on an uninstalled source must fail")
	}
}

func TestGithubAppReposListsInstallationRepos(t *testing.T) {
	f := &github.Fake{Token: "ghs_abc", Repos: []github.Repo{
		{FullName: "acme-corp/api", DefaultBranch: "main"},
	}}
	p := NewGithubApp(f)

	repos, err := p.Repos(context.Background(), installedSource(t))
	if err != nil {
		t.Fatalf("Repos: %v", err)
	}
	if len(repos) != 1 || repos[0].FullName != "acme-corp/api" || repos[0].DefaultBranch != "main" {
		t.Errorf("repos = %+v", repos)
	}
}

func TestGithubAppVerifyWebhookUsesTheSourcesSecret(t *testing.T) {
	src := installedSource(t)
	p := NewGithubApp(&github.Fake{})
	body := []byte(`{"ref":"refs/heads/main"}`)

	mac := hmac.New(sha256.New, []byte("whs"))
	mac.Write(body)
	h := http.Header{}
	h.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	if !p.VerifyWebhook(src, h, body) {
		t.Error("a body signed with the source's secret must verify")
	}

	other := http.Header{}
	other.Set("X-Hub-Signature-256", "sha256=deadbeef")
	if p.VerifyWebhook(src, other, body) {
		t.Error("a bad signature must not verify")
	}
}

func TestRegistryResolvesByKind(t *testing.T) {
	reg := NewRegistry(NewGithubApp(&github.Fake{Token: "t"}))
	if _, err := reg.For(core.GitSourceGithubApp); err != nil {
		t.Fatalf("For(github_app): %v", err)
	}
	if _, err := reg.For("gitlab"); err == nil {
		t.Error("an unknown kind must error, not return a nil provider")
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/gitsource/ -v`
Expected: FAIL — package does not exist.

- [ ] **Step 3: Write `internal/gitsource/provider.go`**

```go
// Package gitsource is the seam between Outhaul and the Git hosts it deploys
// from. A core.GitSource records *which* account is connected; a Provider knows
// how to talk to it. GitHub App is the only implementation today — a second
// host is a new Provider here plus its own credential table in the store.
package gitsource

import (
	"context"
	"fmt"
	"net/http"

	"github.com/outhaul-dev/outhaul/internal/core"
)

// Repo is a repository a source can access.
type Repo struct {
	FullName      string // "owner/name"
	DefaultBranch string
}

// Provider is what one Git hosting integration must supply.
type Provider interface {
	// Kind is the core.GitSource kind this provider serves.
	Kind() string
	// Repos lists the repositories the source can access.
	Repos(ctx context.Context, src core.GitSource) ([]Repo, error)
	// Token returns a short-lived credential valid for both HTTPS clone and
	// API calls. For GitHub these are one object — an installation token.
	Token(ctx context.Context, src core.GitSource) (string, error)
	// VerifyWebhook reports whether body carries a valid signature for src.
	VerifyWebhook(src core.GitSource, h http.Header, body []byte) bool
}

// Registry resolves a source to the Provider that speaks its kind.
type Registry struct{ byKind map[string]Provider }

// NewRegistry indexes providers by kind. Later duplicates of a kind win.
func NewRegistry(providers ...Provider) *Registry {
	r := &Registry{byKind: make(map[string]Provider, len(providers))}
	for _, p := range providers {
		r.byKind[p.Kind()] = p
	}
	return r
}

// For returns the provider for a kind, or an error naming the unknown kind.
func (r *Registry) For(kind string) (Provider, error) {
	p, ok := r.byKind[kind]
	if !ok {
		return nil, fmt.Errorf("gitsource: no provider for kind %q", kind)
	}
	return p, nil
}

// TokenFor mints a credential for a source in one step. Callers that only need
// to clone or call an API use this instead of resolving the provider first.
func (r *Registry) TokenFor(ctx context.Context, src core.GitSource) (string, error) {
	p, err := r.For(src.Kind)
	if err != nil {
		return "", err
	}
	return p.Token(ctx, src)
}
```

- [ ] **Step 4: Write `internal/gitsource/githubapp.go`**

```go
package gitsource

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/outhaul-dev/outhaul/internal/core"
	"github.com/outhaul-dev/outhaul/internal/github"
	"github.com/outhaul-dev/outhaul/internal/webhook"
)

// GithubApp serves sources of kind core.GitSourceGithubApp: a GitHub App and
// the one installation it was created for.
type GithubApp struct{ gh github.Client }

// NewGithubApp wraps a GitHub API client as a Provider.
func NewGithubApp(c github.Client) *GithubApp { return &GithubApp{gh: c} }

func (p *GithubApp) Kind() string { return core.GitSourceGithubApp }

// Token mints a fresh installation access token. Tokens are short-lived and
// minted per use; caching them is a deliberate seam, not an oversight.
func (p *GithubApp) Token(ctx context.Context, src core.GitSource) (string, error) {
	jwt, err := p.appJWT(src)
	if err != nil {
		return "", err
	}
	tok, err := p.gh.InstallationToken(ctx, jwt, src.GithubApp.InstallationID)
	if err != nil {
		return "", fmt.Errorf("mint installation token for %s: %w", src.Display(), err)
	}
	return tok, nil
}

func (p *GithubApp) Repos(ctx context.Context, src core.GitSource) ([]Repo, error) {
	tok, err := p.Token(ctx, src)
	if err != nil {
		return nil, err
	}
	ghRepos, err := p.gh.ListRepos(ctx, tok)
	if err != nil {
		return nil, fmt.Errorf("list repos for %s: %w", src.Display(), err)
	}
	repos := make([]Repo, 0, len(ghRepos))
	for _, r := range ghRepos {
		repos = append(repos, Repo{FullName: r.FullName, DefaultBranch: r.DefaultBranch})
	}
	return repos, nil
}

func (p *GithubApp) VerifyWebhook(src core.GitSource, h http.Header, body []byte) bool {
	return webhook.VerifyGitHub(src.GithubApp.WebhookSecret, h.Get("X-Hub-Signature-256"), body)
}

// appJWT builds the App-authenticating JWT, refusing a source that cannot mint
// anything yet. The messages surface in deploy logs, so they name the source.
func (p *GithubApp) appJWT(src core.GitSource) (string, error) {
	if src.Kind != core.GitSourceGithubApp {
		return "", fmt.Errorf("gitsource: %q is not a GitHub App source", src.Kind)
	}
	if !src.Installed() {
		return "", fmt.Errorf("git source %s is not installed on GitHub", src.Display())
	}
	jwt, err := github.AppJWT(src.GithubApp.PrivateKey, src.GithubApp.AppID, time.Now())
	if err != nil {
		return "", fmt.Errorf("build app jwt for %s: %w", src.Display(), err)
	}
	return jwt, nil
}
```

- [ ] **Step 5: Run to verify it passes**

Run: `go test ./internal/gitsource/ -v`
Expected: PASS (5 tests).

- [ ] **Step 6: Commit**

```bash
git add internal/gitsource/
git commit -m "feat(gitsource): Provider interface with the GitHub App impl

One interface, one implementation. Token covers both clone auth and API
calls because for GitHub they are the same object, and it returns a bare
string so this package never imports deploy."
```

---

### Task 5: Connect flow writes git sources

**Files:**
- Modify: `internal/server/github.go` (all three handlers)
- Modify: `internal/server/server.go` (`Server` struct + `New` signature)
- Modify: `internal/server/templates/github_connect.tmpl`
- Modify: `main.go` (the `server.New` call)
- Modify: `internal/server/server_test.go:230` (`newTestEnv`)
- Modify: `internal/server/github_test.go` (existing assertions move to the new store API)

**Interfaces:**
- Consumes: `store.CreateGithubAppSource`, `store.ListGitSources`, `store.BindGithubInstallation`, `store.GetGitSource` (Task 1); `github.Client.Installation`, `github.AppJWT` (Task 3); `gitsource.Registry` (Task 4).
- Produces:
  - `server.New(..., gh github.Client, reg *gitsource.Registry, previews PreviewHandler, ...)` — **signature changed**, `reg` inserted directly after `gh`
  - `(*Server).sources *gitsource.Registry` field
  - `(*Server).bindInstallation(ctx, installationID int64) (core.GitSource, error)`

- [ ] **Step 1: Write the failing test**

Create `internal/server/gitsources_test.go`:

```go
package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/outhaul-dev/outhaul/internal/core"
	"github.com/outhaul-dev/outhaul/internal/github"
)

// connectApp drives the manifest callback for one App and returns its source id.
func connectApp(t *testing.T, env *testEnv, appID int64, slug string) int64 {
	t.Helper()
	env.gh.ManifestResult = github.ManifestResult{
		AppID: appID, Slug: slug, PEM: testAppKeyPEM(t),
		WebhookSecret: "whs-" + slug, ClientID: "cid", ClientSecret: "csec",
	}
	state := env.srv.newGithubState()
	rec := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(rec,
		httptest.NewRequest("GET", "/github/callback?code=c&state="+state, nil))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("callback status = %d, want 303; body=%s", rec.Code, rec.Body)
	}
	src, ok, err := env.store.GitSourceByGithubAppID(context.Background(), appID)
	if err != nil || !ok {
		t.Fatalf("source for app %d not stored: ok=%v err=%v", appID, ok, err)
	}
	return src.ID
}

func TestGithubCallbackCreatesAGitSource(t *testing.T) {
	env := newTestEnv(t)
	id := connectApp(t, env, 55, "outhaul-a")

	src, _, _ := env.store.GetGitSource(context.Background(), id)
	if src.Kind != core.GitSourceGithubApp {
		t.Errorf("kind = %q", src.Kind)
	}
	if src.Installed() {
		t.Error("a source must not be Installed before setup runs")
	}
}

func TestConnectingASecondAccountKeepsTheFirst(t *testing.T) {
	env := newTestEnv(t)
	connectApp(t, env, 55, "outhaul-a")
	connectApp(t, env, 77, "outhaul-b")

	list, err := env.store.ListGitSources(context.Background())
	if err != nil {
		t.Fatalf("ListGitSources: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("got %d sources, want 2 — connecting an account must not replace the previous one", len(list))
	}
}

// Setup carries no state, so the installation is matched back to its App by
// asking GitHub which App owns it.
func TestGithubSetupBindsInstallationToTheOwningSource(t *testing.T) {
	env := newTestEnv(t)
	connectApp(t, env, 55, "outhaul-a")
	wantID := connectApp(t, env, 77, "outhaul-b")

	env.gh.InstallationsByApp = map[int64][]github.Installation{
		77: {{ID: 9001, AccountLogin: "acme-corp", AccountType: "Organization"}},
	}
	rec := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(rec,
		httptest.NewRequest("GET", "/github/setup?installation_id=9001", nil))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("setup status = %d, want 303; body=%s", rec.Code, rec.Body)
	}

	ctx := context.Background()
	bound, _, _ := env.store.GetGitSource(ctx, wantID)
	if !bound.Installed() || bound.GithubApp.InstallationID != 9001 {
		t.Errorf("owning source not bound: installation=%d", bound.GithubApp.InstallationID)
	}
	if bound.AccountLogin != "acme-corp" || bound.AccountType != "Organization" {
		t.Errorf("account = %q/%q", bound.AccountLogin, bound.AccountType)
	}
	// The other pending source must be untouched.
	other, _, _ := env.store.GitSourceByGithubAppID(ctx, 55)
	if other.Installed() {
		t.Error("bound the installation to the wrong source")
	}
}

func TestGithubSetupRejectsAnUnownedInstallation(t *testing.T) {
	env := newTestEnv(t)
	connectApp(t, env, 55, "outhaul-a")
	env.gh.InstallationsByApp = map[int64][]github.Installation{}

	rec := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(rec,
		httptest.NewRequest("GET", "/github/setup?installation_id=4242", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestGithubConnectOffersPersonalAndOrg(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)
	page := body(t, env.get(t, "/github/connect"))
	if !strings.Contains(page, `name="owner"`) {
		t.Error("connect page must ask where to create the App")
	}
	if !strings.Contains(page, `value="org"`) {
		t.Error("connect page must offer an organization")
	}
}

func TestGithubConnectOrgTargetsTheOrgAppForm(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)
	page := body(t, env.get(t, "/github/connect?owner=org&org=acme-corp"))
	if !strings.Contains(page, "github.com/organizations/acme-corp/settings/apps/new") {
		t.Errorf("org flow must post to the org App form; page did not contain it")
	}
}

func TestGithubConnectRejectsAMalformedOrg(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)
	res := env.get(t, "/github/connect?owner=org&org=not/an/org")
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", res.StatusCode)
	}
}
```

- [ ] **Step 2: Add the test key helper**

Append to `internal/server/server_test.go`:

```go
// testAppKeyPEM generates an RSA key PEM. Handlers that mint an App JWT need a
// real key, so seeded App credentials cannot use a placeholder string.
func testAppKeyPEM(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{
		Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key),
	}))
}
```

Imports to add: `crypto/rand`, `crypto/rsa`, `crypto/x509`, `encoding/pem`.

- [ ] **Step 3: Run to verify it fails**

Run: `go test ./internal/server/ -run 'GitSource|GithubSetup|GithubConnect|GithubCallback' -v`
Expected: FAIL — `env.store.GitSourceByGithubAppID` compiles (Task 1) but the handlers still write `github_app`, so `TestGithubCallbackCreatesAGitSource` fails on "source for app 55 not stored".

- [ ] **Step 4: Wire the registry into `Server`**

In `internal/server/server.go`, add the import `"github.com/outhaul-dev/outhaul/internal/gitsource"`, add the field next to `gh`:

```go
	gh        github.Client       // raw GitHub API: the manifest/installation connect flow only
	sources   *gitsource.Registry // providers for connected git sources
```

and change `New`:

```go
func New(st *store.Store, d Deployer, rt Runtime, cp compose.Runner, dbm Databases, bk Backups, br *logstream.Broker, gh github.Client, reg *gitsource.Registry, previews PreviewHandler, publicURL, serverIP string, tlsEnabled bool, setupToken string) (*Server, error) {
	s := &Server{
		...
		gh:          gh,
		sources:     reg,
		previews:    previews,
		...
	}
```

- [ ] **Step 5: Update both `server.New` call sites**

`main.go` — build the registry immediately after `ghClient := github.New()` (line 178), **before** `deploy.NewWorker` at line 182. Task 7 and Task 8 both need it there; creating it next to `server.New` (line 255) would be too late:

```go
	ghClient := github.New()
	// One registry, shared by the deploy worker, the preview manager, and the
	// HTTP layer: every consumer resolves a source's provider the same way.
	sources := gitsource.NewRegistry(gitsource.NewGithubApp(ghClient))
```

then pass it to `server.New`:

```go
	srv, err := server.New(st, worker, dc, compose.NewDocker(), dbm, backups, broker, ghClient,
		sources, previews,
		cfg.PublicURL, serverIP, cfg.TLSEnabled(), setupToken)
```

Add the `gitsource` import to `main.go`.

`internal/server/server_test.go:230`:

```go
	gh := &github.Fake{}
	srv, err := New(st, dep, rt, cp, dbm, bk, br, gh, gitsource.NewRegistry(gitsource.NewGithubApp(gh)), nil, "https://slip.example.com", "203.0.113.7", false, "SETUPTOKEN")
```

Add the `gitsource` import to `server_test.go`.

- [ ] **Step 6: Rewrite the three handlers**

Replace the bodies in `internal/server/github.go`:

```go
// orgLogin matches a GitHub account name: alphanumerics and single hyphens,
// no leading or trailing hyphen.
var orgLogin = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?$`)

// handleGithubConnect asks where the App should be created, then renders the
// auto-submitting manifest form pointed at that account's App form. A private
// App can only be installed on the account that owns it, so this choice is what
// decides which account the resulting source covers.
func (s *Server) handleGithubConnect(w http.ResponseWriter, r *http.Request) {
	if !s.publicURLSet() {
		s.render(w, http.StatusOK, "github_connect", map[string]any{
			"Title": "Connect GitHub", "Active": "settings", "NeedsPublicURL": true,
		})
		return
	}
	owner := r.URL.Query().Get("owner")
	org := strings.TrimSpace(r.URL.Query().Get("org"))

	// No choice made yet: show the picker.
	if owner == "" {
		s.render(w, http.StatusOK, "github_connect", map[string]any{
			"Title": "Connect GitHub", "Active": "settings", "Choose": true,
		})
		return
	}
	action := "https://github.com/settings/apps/new"
	if owner == "org" {
		if !orgLogin.MatchString(org) {
			http.Error(w, "Enter a valid GitHub organization name.", http.StatusBadRequest)
			return
		}
		action = "https://github.com/organizations/" + org + "/settings/apps/new"
	}
	manifest, err := github.BuildManifest(github.ManifestParams{
		Name:      "outhaul-" + s.newNameSuffix(),
		PublicURL: s.publicURL,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.render(w, http.StatusOK, "github_connect", map[string]any{
		"Title":    "Connect GitHub",
		"Active":   "settings",
		"Action":   action + "?state=" + s.newGithubState(),
		"Manifest": manifest,
	})
}

// handleGithubCallback exchanges the manifest code and records a new source.
// The row is persisted before installation on purpose: a restart between here
// and setup must not strand credentials for an App that now exists on GitHub.
func (s *Server) handleGithubCallback(w http.ResponseWriter, r *http.Request) {
	if !s.consumeGithubState(r.URL.Query().Get("state")) {
		http.Error(w, "invalid or expired state", http.StatusBadRequest)
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		http.Error(w, "missing code", http.StatusBadRequest)
		return
	}
	res, err := s.gh.ExchangeManifest(r.Context(), code)
	if err != nil {
		http.Error(w, "manifest exchange failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	if _, err := s.store.CreateGithubAppSource(r.Context(), core.GithubAppCreds{
		AppID: res.AppID, Slug: res.Slug, PrivateKey: res.PEM,
		WebhookSecret: res.WebhookSecret, ClientID: res.ClientID, ClientSecret: res.ClientSecret,
	}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "https://github.com/apps/"+res.Slug+"/installations/new", http.StatusSeeOther)
}

// handleGithubSetup records the installation the operator just created.
func (s *Server) handleGithubSetup(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.URL.Query().Get("installation_id"), 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "missing installation_id", http.StatusBadRequest)
		return
	}
	if _, err := s.bindInstallation(r.Context(), id); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// bindInstallation matches an installation id back to the source whose App owns
// it. GitHub sends no state to the setup URL, so instead of guessing we ask:
// GET /app/installations/{id} is scoped to the calling App, so only the owner
// gets an answer — and that answer carries the account name we want anyway.
//
// A source that already holds this installation is simply refreshed, which is
// the setup_on_update re-install path.
func (s *Server) bindInstallation(ctx context.Context, installationID int64) (core.GitSource, error) {
	sources, err := s.store.ListGitSources(ctx)
	if err != nil {
		return core.GitSource{}, err
	}
	// Already-bound source first, then pending ones newest-first: a retry of a
	// re-install must refresh rather than claim an unrelated pending App.
	var candidates []core.GitSource
	for _, src := range sources {
		if src.Kind == core.GitSourceGithubApp && src.GithubApp.InstallationID == installationID {
			candidates = append(candidates, src)
		}
	}
	for i := len(sources) - 1; i >= 0; i-- {
		if sources[i].Kind == core.GitSourceGithubApp && sources[i].GithubApp.InstallationID == 0 {
			candidates = append(candidates, sources[i])
		}
	}
	for _, src := range candidates {
		jwt, err := github.AppJWT(src.GithubApp.PrivateKey, src.GithubApp.AppID, time.Now())
		if err != nil {
			log.Printf("github setup: app jwt for %s: %v", src.Display(), err)
			continue
		}
		inst, err := s.gh.Installation(ctx, jwt, installationID)
		if err != nil {
			continue // this App does not own the installation
		}
		if err := s.store.BindGithubInstallation(ctx, src.ID, installationID, inst.AccountLogin, inst.AccountType); err != nil {
			return core.GitSource{}, err
		}
		src.GithubApp.InstallationID = installationID
		src.AccountLogin, src.AccountType = inst.AccountLogin, inst.AccountType
		return src, nil
	}
	return core.GitSource{}, fmt.Errorf("no connected GitHub App owns installation %d", installationID)
}
```

Imports for `internal/server/github.go`: `context`, `crypto/rand`, `encoding/hex`, `fmt`, `log`, `net/http`, `regexp`, `strconv`, `strings`, `time`, plus `internal/core` and `internal/github`.

- [ ] **Step 7: Update `github_connect.tmpl`**

```html
{{define "content"}}
<div class="pagehead"><h1>Connect GitHub</h1></div>
{{if .NeedsPublicURL}}
<section class="panel">
  <p class="alert alert-error">Set <code>OUTHAUL_PUBLIC_URL</code> to your admin UI's
  public HTTPS URL, then reload this page. GitHub needs a reachable callback and
  webhook URL to create the App.</p>
</section>
{{else if .Choose}}
<section class="panel">
  <p>Outhaul creates a GitHub App under one account and names this source after
  it. A private App can only be installed on the account that owns it, so
  connecting an organization means creating the App there.</p>
  <form method="get" action="/github/connect" class="grid-form">
    <label class="field"><span>
      <input type="radio" name="owner" value="user" checked> My personal account
    </span></label>
    <label class="field"><span>
      <input type="radio" name="owner" value="org"> An organization
    </span>
      <input type="text" name="org" placeholder="acme-corp">
    </label>
    <button type="submit" class="btn btn-primary">Continue to GitHub</button>
  </form>
</section>
{{else}}
<section class="panel">
  <p>Click below to create a GitHub App for Outhaul. GitHub will ask you to
  approve permissions (read-only code + metadata) and choose which repositories
  to grant access to.</p>
  <form id="manifest-form" method="post" action="{{.Action}}">
    <input type="hidden" name="manifest" value="{{.Manifest}}">
    <button type="submit" class="btn btn-primary">Create GitHub App</button>
  </form>
</section>
<script>document.getElementById('manifest-form').submit();</script>
{{end}}
{{end}}
```

- [ ] **Step 8: Update the existing GitHub handler tests**

In `internal/server/github_test.go`, `TestGithubCallbackStoresApp` asserts through the removed `env.store.GithubApp`. Replace that assertion:

```go
	if _, ok, _ := env.store.GitSourceByGithubAppID(req.Context(), 55); !ok {
		t.Error("git source not stored")
	}
```

and give its `ManifestResult` a real key: `PEM: testAppKeyPEM(t)`. Do the same for `TestGithubSetupStoresInstallation` — rewrite it to seed `env.gh.InstallationsByApp` and assert through `GetGitSource`, or delete it as superseded by `TestGithubSetupBindsInstallationToTheOwningSource` in the new file. Prefer deleting it; the new test covers strictly more.

- [ ] **Step 9: Run to verify it passes**

Run: `go build ./... && go test ./internal/server/ -v`
Expected: PASS.

- [ ] **Step 10: Commit**

```bash
git add internal/server/github.go internal/server/server.go internal/server/gitsources_test.go \
        internal/server/github_test.go internal/server/server_test.go \
        internal/server/templates/github_connect.tmpl main.go
git commit -m "feat(server): connect any number of GitHub accounts

The connect flow asks where the App should live (personal or org), then
records each App as its own git source instead of overwriting a
singleton.

Setup carries no state back from GitHub, so an installation is matched to
its App by asking: GET /app/installations/{id} is App-scoped, so only the
owner answers — and the answer carries the account name for the UI."
```

---

### Task 6: Route webhooks to the source that signed them

**Files:**
- Modify: `internal/server/webhooks.go:20-70` (`handleGithubWebhook`)
- Modify: `internal/server/server.go:37` (`PreviewHandler` interface)
- Create: `internal/server/webhooks_source_test.go`

**Interfaces:**
- Consumes: `store.GitSourceByGithubAppID`, `store.AppsByGithubRepoSource` (Tasks 1-2); `gitsource.Registry` (Task 4); `Server.sources` (Task 5).
- Produces: `PreviewHandler.Handle(ctx context.Context, sourceID int64, ev webhook.PullRequestEvent) error` — **signature changed**.

- [ ] **Step 1: Write the failing test**

Create `internal/server/webhooks_source_test.go`:

```go
package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/outhaul-dev/outhaul/internal/core"
)

// postPush delivers a signed push webhook as GitHub would, naming the App.
func postPush(t *testing.T, env *testEnv, appID int64, secret, repo, branch string) *httptest.ResponseRecorder {
	t.Helper()
	payload := `{"ref":"refs/heads/` + branch + `","repository":{"full_name":"` + repo + `"}}`
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(payload))

	req := httptest.NewRequest("POST", "/webhooks/github", strings.NewReader(payload))
	req.Header.Set("X-GitHub-Event", "push")
	req.Header.Set("X-GitHub-Hook-Installation-Target-Type", "integration")
	req.Header.Set("X-GitHub-Hook-Installation-Target-ID", strconv.FormatInt(appID, 10))
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	rec := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(rec, req)
	return rec
}

func deployCount(t *testing.T, env *testEnv, appID int64) int {
	t.Helper()
	deps, err := env.store.ListDeploymentsForApp(context.Background(), appID)
	if err != nil {
		t.Fatalf("ListDeploymentsForApp: %v", err)
	}
	return len(deps)
}

// The security-relevant case: two connected accounts, the same repo full name
// under each. A push signed by one must not deploy the other's app.
func TestWebhookDeploysOnlyTheSigningSourcesApp(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	srcA := connectApp(t, env, 55, "outhaul-a")
	srcB := connectApp(t, env, 77, "outhaul-b")

	appA, _ := env.store.CreateApp(ctx, core.App{
		Name: "a-web", Domain: "a.test", Source: core.SourceGithub, Kind: core.KindNixpacks,
		GithubRepo: "acme-corp/api", GitSourceID: srcA, Branch: "main", AutoDeploy: true, WebhookSecret: "w1",
	})
	appB, _ := env.store.CreateApp(ctx, core.App{
		Name: "b-web", Domain: "b.test", Source: core.SourceGithub, Kind: core.KindNixpacks,
		GithubRepo: "acme-corp/api", GitSourceID: srcB, Branch: "main", AutoDeploy: true, WebhookSecret: "w2",
	})

	rec := postPush(t, env, 55, "whs-outhaul-a", "acme-corp/api", "main")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body)
	}
	if got := deployCount(t, env, appA.ID); got != 1 {
		t.Errorf("source A's app got %d deployments, want 1", got)
	}
	if got := deployCount(t, env, appB.ID); got != 0 {
		t.Errorf("source B's app got %d deployments, want 0 — a push for one account must not deploy another's", got)
	}
}

func TestWebhookRejectsAnUnknownApp(t *testing.T) {
	env := newTestEnv(t)
	connectApp(t, env, 55, "outhaul-a")
	rec := postPush(t, env, 4242, "whs-outhaul-a", "acme-corp/api", "main")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 for an unknown App id", rec.Code)
	}
}

func TestWebhookRejectsASignatureFromAnotherSource(t *testing.T) {
	env := newTestEnv(t)
	connectApp(t, env, 55, "outhaul-a")
	connectApp(t, env, 77, "outhaul-b")
	// Claims App 55, signed with App 77's secret.
	rec := postPush(t, env, 55, "whs-outhaul-b", "acme-corp/api", "main")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 — a signature from another source must not verify", rec.Code)
	}
}
```

`connectApp` comes from Task 5's `gitsources_test.go`, and `postForm`/`body`/`itoa` are the existing helpers in `server_test.go`.

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/server/ -run TestWebhook -v`
Expected: FAIL — the handler still verifies against the legacy `store.GithubApp`, so `TestWebhookDeploysOnlyTheSigningSourcesApp` returns 401.

- [ ] **Step 3: Rewrite `handleGithubWebhook`**

Replace the body up to the `switch` in `internal/server/webhooks.go`:

```go
// handleGithubWebhook verifies a GitHub App delivery and deploys the matching
// apps. Every connected App posts here, so the delivery is first matched to the
// source that signed it: GitHub names the App in
// X-GitHub-Hook-Installation-Target-ID, and only that source's secret is
// checked. Fan-out is then scoped to the same source, so a push for one
// connected account can never deploy another account's app.
func (s *Server) handleGithubWebhook(w http.ResponseWriter, r *http.Request) {
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	if r.Header.Get("X-GitHub-Hook-Installation-Target-Type") != "integration" {
		http.Error(w, "unexpected hook target", http.StatusUnauthorized)
		return
	}
	appID, err := strconv.ParseInt(r.Header.Get("X-GitHub-Hook-Installation-Target-ID"), 10, 64)
	if err != nil {
		http.Error(w, "unidentified hook", http.StatusUnauthorized)
		return
	}
	src, found, err := s.store.GitSourceByGithubAppID(r.Context(), appID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !found {
		http.Error(w, "unknown app", http.StatusUnauthorized)
		return
	}
	provider, err := s.sources.For(src.Kind)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !provider.VerifyWebhook(src, r.Header, body) {
		http.Error(w, "bad signature", http.StatusUnauthorized)
		return
	}
	switch r.Header.Get("X-GitHub-Event") {
	case "push":
		ev, err := webhook.ParsePush(body)
		if err != nil {
			http.Error(w, "bad payload", http.StatusBadRequest)
			return
		}
		apps, err := s.store.AppsByGithubRepoSource(r.Context(), src.ID, ev.RepoFullName)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		for _, app := range apps {
			s.maybeDeploy(r.Context(), app, ev)
		}
		w.WriteHeader(http.StatusOK)
		return
	case "pull_request":
		if s.previews == nil {
			w.WriteHeader(http.StatusOK)
			return
		}
		ev, err := webhook.ParsePullRequest(body)
		if err != nil {
			http.Error(w, "bad payload", http.StatusBadRequest)
			return
		}
		if err := s.previews.Handle(r.Context(), src.ID, ev); err != nil {
			log.Printf("webhook: preview handling: %v", err)
		}
		w.WriteHeader(http.StatusOK)
		return
	default:
		w.WriteHeader(http.StatusOK) // ignore other events
		return
	}
}
```

Add `strconv` to the imports; drop `core` if it is now unused in this file.

- [ ] **Step 4: Update the `PreviewHandler` interface**

`internal/server/server.go:37`:

```go
	Handle(ctx context.Context, sourceID int64, ev webhook.PullRequestEvent) error
```

- [ ] **Step 5: Fix compilation in existing webhook tests**

`internal/server/webhooks_test.go` and `webhooks_watchpaths_test.go` build requests against the old single-App scheme, and any fake preview handler now needs the new signature. Update each to use the `postPush` helper's header set: seed a source via `connectApp`, give the app that source's id, and sign with `"whs-<slug>"`. Update any local `Handle` implementation to take `sourceID int64`.

- [ ] **Step 6: Run to verify it passes**

Run: `go build ./... && go test ./internal/server/ -v`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/server/webhooks.go internal/server/server.go internal/server/webhooks_source_test.go \
        internal/server/webhooks_test.go internal/server/webhooks_watchpaths_test.go
git commit -m "feat(server): resolve each webhook to the source that signed it

GitHub names the App in every delivery header, so the source is looked up
rather than guessed, and only its secret is checked — no scan across
every connected secret.

Fan-out is scoped to that source: two accounts can each expose the same
owner/repo, and a push for one must not deploy the other's app."
```

---

### Task 7: Deploy pipeline mints from the app's own source

**Files:**
- Modify: `internal/deploy/pipeline.go:464-487` (`cloneSpec` GitHub branch, `githubToken`)
- Modify: `internal/deploy/worker.go` (Worker field + constructor)
- Modify: `main.go` (worker construction)
- Create: `internal/deploy/gitsource_test.go`

**Interfaces:**
- Consumes: `store.GetGitSource` (Task 1), `core.App.GitSourceID` (Task 2), `gitsource.Registry` (Task 4).
- Produces: `(*Worker).sourceToken(ctx context.Context, app core.App) (string, error)`; `deploy.NewWorker`'s `gh github.Client` parameter becomes `sources *gitsource.Registry`.

The package's `newHarness(t) *harness` (in `deploy_test.go:80`) already opens a **real** `*store.Store` with a real secret box and exposes `h.store` and `h.worker`, so these tests need no new fixture.

- [ ] **Step 1: Write the failing test**

Create `internal/deploy/gitsource_test.go`:

```go
package deploy

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"strings"
	"testing"

	"github.com/outhaul-dev/outhaul/internal/core"
)

// testKeyPEM generates a PKCS#1 RSA key PEM; AppJWT needs a real key.
func testKeyPEM(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{
		Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key),
	}))
}

func TestCloneSpecUsesTheAppsOwnGitSource(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	src, err := h.store.CreateGithubAppSource(ctx, core.GithubAppCreds{
		AppID: 77, Slug: "outhaul-b", PrivateKey: testKeyPEM(t),
		WebhookSecret: "whs", ClientID: "cid", ClientSecret: "csec",
	})
	if err != nil {
		t.Fatalf("CreateGithubAppSource: %v", err)
	}
	if err := h.store.BindGithubInstallation(ctx, src.ID, 9001, "acme-corp", "Organization"); err != nil {
		t.Fatalf("BindGithubInstallation: %v", err)
	}
	app := core.App{
		Name: "web", Source: core.SourceGithub, GithubRepo: "acme-corp/api",
		GitSourceID: src.ID, Branch: "main",
	}

	spec, err := h.worker.cloneSpec(ctx, app)
	if err != nil {
		t.Fatalf("cloneSpec: %v", err)
	}
	if spec.URL != "https://github.com/acme-corp/api.git" {
		t.Errorf("URL = %q", spec.URL)
	}
	if spec.Auth.Kind != AuthToken || spec.Auth.Token == "" {
		t.Errorf("auth = %+v, want a token", spec.Auth)
	}
}

func TestCloneSpecFailsClearlyWithoutASource(t *testing.T) {
	h := newHarness(t)
	app := core.App{Name: "web", Source: core.SourceGithub, GithubRepo: "acme-corp/api", GitSourceID: 0}

	_, err := h.worker.cloneSpec(context.Background(), app)
	if err == nil {
		t.Fatal("an app with no git source must not produce a clone spec")
	}
	if !strings.Contains(err.Error(), "git source") {
		t.Errorf("error = %q, want it to name the missing git source", err)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/deploy/ -run CloneSpec -v`
Expected: FAIL — `w.cloneSpec` still calls `githubToken(ctx)` and reads the legacy singleton.

- [ ] **Step 3: Replace `githubToken` with `sourceToken`**

In `internal/deploy/pipeline.go`, the `core.SourceGithub` branch of `cloneSpec`:

```go
	case core.SourceGithub:
		token, err := w.sourceToken(ctx, app)
		if err != nil {
			return CloneSpec{}, err
		}
		spec.URL = "https://github.com/" + app.GithubRepo + ".git"
		spec.Auth = Auth{Kind: AuthToken, Token: token}
```

and replace `githubToken` with:

```go
// sourceToken mints a fresh clone credential from the git source this app's
// repo comes from. Messages surface in deploy logs, so they say which link is
// missing rather than "github app not configured".
func (w *Worker) sourceToken(ctx context.Context, app core.App) (string, error) {
	if app.GitSourceID == 0 {
		return "", fmt.Errorf("app %s has no git source; reconnect its account in Settings", app.Name)
	}
	src, ok, err := w.store.GetGitSource(ctx, app.GitSourceID)
	if err != nil {
		return "", fmt.Errorf("load git source: %w", err)
	}
	if !ok {
		return "", fmt.Errorf("app %s references git source %d, which no longer exists", app.Name, app.GitSourceID)
	}
	return w.sources.TokenFor(ctx, src)
}
```

`Worker.store` is the concrete `*store.Store` (`worker.go:54`), so `GetGitSource` is already reachable — no interface to widen. Drop the now-unused `github` import from `pipeline.go` if `AppJWT` was its only use there.

- [ ] **Step 4: Swap the Worker's GitHub client for the registry**

`NewWorker(st, docker, Builders{...}, compose, cloner, broker, gh github.Client, cfg)` passes a raw `github.Client` whose only use was minting App JWTs and installation tokens. Replace that parameter:

```go
	sources *gitsource.Registry // providers for the git sources apps clone from
```

Update the field, the constructor parameter, `main.go`'s `NewWorker` call (build the registry *before* the worker so both it and `server.New` receive it), and `newHarness` in `deploy_test.go:101`:

```go
	h.worker = NewWorker(st, h.docker, Builders{Nixpacks: h.builder, Dockerfile: h.dockerfile},
		h.compose, h.cloner, h.broker,
		gitsource.NewRegistry(gitsource.NewGithubApp(&github.Fake{Token: "ghs_test"})), cfg)
```

Keep `github.Fake`'s `Token` non-empty so `cloneSpec` produces a usable token.

- [ ] **Step 5: Run to verify it passes**

Run: `go build ./... && go test ./internal/deploy/ -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/deploy/ main.go
git commit -m "feat(deploy): clone with the app's own git source credentials

Each app now mints its token from the source its repo came from, so two
connected accounts deploy side by side. Errors name the broken link,
since they land in a deploy log."
```

---

### Task 8: Preview environments follow the source

**Files:**
- Modify: `internal/previewmgr/manager.go:20-31` (`Store` interface), `:55-60` (`TokenSource`), `Handle`, `comment`, `redeploy`, `teardown`
- Modify: `internal/previewmgr/sweeper.go:46`
- Modify: `main.go` (the `tokenSource` closure)
- Create: `internal/previewmgr/gitsource_test.go`

**Interfaces:**
- Consumes: `store.AppsByGithubRepoSource` (Task 2), `core.App.GitSourceID` (Task 2).
- Produces:
  - `previewmgr.TokenSource = func(ctx context.Context, sourceID int64) (token string, ok bool, err error)`
  - `(*Manager).Handle(ctx context.Context, sourceID int64, ev webhook.PullRequestEvent) error`
  - `previewmgr.Store.AppsByGithubRepoSource(ctx, sourceID int64, repo string) ([]core.App, error)`

- [ ] **Step 1: Give the harness a real git source**

`newHarness` (in `manager_test.go:107`) opens a **real** `*store.Store`, and `seedGithubApp(t, name, repo, tweak)` creates the GitHub-sourced parent every preview test uses. Both need a source now.

In `newHarness`, add a real source and record its id on the harness:

```go
type harness struct {
	st       *store.Store
	mgr      *Manager
	notifier *fakeNotifier
	docker   *fakeDocker
	dbprov   *fakeDBProvisioner
	gh       *fakeCommenter
	sourceID int64 // git source every seeded parent app belongs to
}
```

and at the end of `newHarness`, before building the manager:

```go
	src, err := st.CreateGithubAppSource(context.Background(), core.GithubAppCreds{
		AppID: 77, Slug: "outhaul-test", PrivateKey: "PEM",
		WebhookSecret: "whs", ClientID: "cid", ClientSecret: "csec",
	})
	if err != nil {
		t.Fatalf("CreateGithubAppSource: %v", err)
	}
	if err := st.BindGithubInstallation(context.Background(), src.ID, 9001, "acme-corp", "Organization"); err != nil {
		t.Fatalf("BindGithubInstallation: %v", err)
	}
	h.sourceID = src.ID
	ts := func(context.Context, int64) (string, bool, error) { return "tok", true, nil }
	h.mgr = New(st, h.notifier, h.dbprov, h.docker, h.gh, ts, serverIP)
```

In `seedGithubApp`, add `GitSourceID: h.sourceID` to the `CreateApp` literal. Every existing preview test then passes `h.sourceID` to `Handle`; update those call sites.

- [ ] **Step 2: Write the failing test**

Create `internal/previewmgr/gitsource_test.go`:

```go
package previewmgr

import (
	"context"
	"testing"

	"github.com/outhaul-dev/outhaul/internal/core"
	"github.com/outhaul-dev/outhaul/internal/webhook"
)

// A preview clones the same repo as its parent, so it must carry the parent's
// credentials. This is inherited by the `child := parent` struct copy in spawn
// — the guard matters because losing it fails only at clone time, inside a
// deploy log, long after the mistake.
func TestPreviewChildInheritsParentGitSource(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	parent := h.seedGithubApp(t, "web", "acme-corp/api", nil)

	if err := h.mgr.Handle(ctx, h.sourceID, webhook.PullRequestEvent{
		Action: "opened", Number: 7, BaseRepoFullName: "acme-corp/api", HeadRef: "feat",
	}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	child, err := h.st.GetPreviewByPR(ctx, parent.ID, 7)
	if err != nil {
		t.Fatalf("GetPreviewByPR: %v", err)
	}
	if child.GitSourceID != parent.GitSourceID {
		t.Errorf("preview GitSourceID = %d, want the parent's %d", child.GitSourceID, parent.GitSourceID)
	}
}

// Handle looks up apps scoped to the event's source, so a PR delivered by one
// connected account never spawns previews for another's identically-named repo.
func TestHandleIgnoresAppsFromAnotherSource(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	parent := h.seedGithubApp(t, "web", "acme-corp/api", nil)

	const otherSource = 4242
	if err := h.mgr.Handle(ctx, otherSource, webhook.PullRequestEvent{
		Action: "opened", Number: 7, BaseRepoFullName: "acme-corp/api", HeadRef: "feat",
	}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if _, err := h.st.GetPreviewByPR(ctx, parent.ID, 7); err == nil {
		t.Error("a PR delivered by a different source must not spawn a preview")
	}
}
```

- [ ] **Step 3: Run to verify it fails**

Run: `go test ./internal/previewmgr/ -run 'GitSource|EventSource' -v`
Expected: FAIL — `m.Handle` takes two arguments.

- [ ] **Step 4: Thread the source id through the manager**

`Store` interface: replace `AppsByGithubRepo(ctx, repo)` with

```go
	AppsByGithubRepoSource(ctx context.Context, sourceID int64, repo string) ([]core.App, error)
```

`TokenSource`:

```go
// TokenSource yields an API credential for one git source (nil disables comments).
type TokenSource func(ctx context.Context, sourceID int64) (token string, ok bool, err error)
```

`Handle`:

```go
// Handle routes one pull_request event to the right lifecycle action for every
// enabled app targeting the PR's base repo *through the source that delivered
// the event*.
func (m *Manager) Handle(ctx context.Context, sourceID int64, ev webhook.PullRequestEvent) error {
	apps, err := m.store.AppsByGithubRepoSource(ctx, sourceID, ev.BaseRepoFullName)
	...
```

The child app already inherits `GitSourceID` for free — `child := parent` copies the whole struct, and Task 2 added the field to `core.App`. The test above is a regression guard on that, not a change.

`comment` takes the source id and passes it to `m.token`:

```go
func (m *Manager) comment(ctx context.Context, cfg core.PreviewConfig, sourceID int64, repo string, pr int, body string) {
	if !cfg.PostPRComment || m.token == nil || repo == "" {
		return
	}
	tok, ok, err := m.token(ctx, sourceID)
	...
```

Update every `m.comment(...)` call site to pass the relevant app's `GitSourceID` (`parent.GitSourceID` in `redeploy`/`teardown`/`OnDeployFinished`, and the `sourceID` argument in `Handle`). `sweeper.go:46` has the parent app in hand, so pass `parent.GitSourceID`.

- [ ] **Step 5: Update `main.go`'s token source**

```go
	// Preview environments post PR comments as the app's own connected account.
	tokenSource := func(ctx context.Context, sourceID int64) (string, bool, error) {
		src, ok, err := st.GetGitSource(ctx, sourceID)
		if err != nil || !ok {
			return "", false, err
		}
		tok, err := sources.TokenFor(ctx, src)
		return tok, err == nil, err
	}
```

- [ ] **Step 6: Run to verify it passes**

Run: `go build ./... && go test ./internal/previewmgr/ ./internal/server/ -v`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/previewmgr/ main.go
git commit -m "feat(previews): scope preview lifecycle to the event's git source

Previews now look up apps and mint comment tokens per source. The child
inherits GitSourceID from its parent via the existing struct copy; the
new test guards that, because losing it fails only at clone time."
```

---

### Task 9: One grouped repo picker across all accounts

**Files:**
- Modify: `internal/server/handlers.go:110-190` (`ghRepoCache`, `cachedRepos`, `storeRepos`, `githubRepoData`), `:191-260` (`handleCreateApp`), `:411-460` (`handleUpdateAppSource`)
- Modify: `internal/server/server.go:112-113` (cache fields)
- Modify: `internal/server/templates/appform.tmpl:33-59` and the script block at `:141-158`
- Modify: `internal/server/templates/app.tmpl:431-452`
- Create: `internal/server/repopicker_test.go`

**Interfaces:**
- Consumes: `store.ListGitSources`, `store.GetGitSource` (Task 1); `gitsource.Registry`, `gitsource.Repo` (Task 4); `Server.sources` (Task 5).
- Produces:
  - `server.repoGroup{SourceID int64, AccountLogin string, AccountKind string, Repos []gitsource.Repo}`
  - `(*Server).gitSourceData(r *http.Request) map[string]any` — sets `GitSourceConnected` (bool) and `RepoGroups` (`[]repoGroup`)
  - `(*Server).resolveRepoSource(ctx, sourceID int64, fullName string) (int64, string)` — returns the validated source id, or `0` and a user-facing error string

- [ ] **Step 1: Write the failing test**

Create `internal/server/repopicker_test.go`:

```go
package server

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/outhaul-dev/outhaul/internal/core"
	"github.com/outhaul-dev/outhaul/internal/github"
)

func installSource(t *testing.T, env *testEnv, appID int64, slug string, installID int64, login, kind string) int64 {
	t.Helper()
	id := connectApp(t, env, appID, slug)
	if err := env.store.BindGithubInstallation(context.Background(), id, installID, login, kind); err != nil {
		t.Fatalf("BindGithubInstallation: %v", err)
	}
	return id
}

func TestAppFormGroupsReposByAccount(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)
	installSource(t, env, 55, "outhaul-a", 9001, "jsmart", "User")
	installSource(t, env, 77, "outhaul-b", 9002, "acme-corp", "Organization")
	env.gh.Repos = []github.Repo{{FullName: "acme-corp/api", DefaultBranch: "main"}}

	page := body(t, env.get(t, "/apps"))
	if !strings.Contains(page, `<optgroup label="jsmart (personal)"`) {
		t.Error("personal account group missing")
	}
	if !strings.Contains(page, `<optgroup label="acme-corp (org)"`) {
		t.Error("organization group missing")
	}
	if !strings.Contains(page, `name="git_source_id"`) {
		t.Error("hidden git_source_id field missing")
	}
}

// One account failing to list must not blank the whole dropdown — which is
// exactly what the old single-slot cache did. The failure is injected with a
// private key AppJWT cannot parse, so only that source's group dies.
func TestRepoGroupsSurviveOneFailingSource(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)
	ctx := context.Background()
	good := installSource(t, env, 55, "outhaul-a", 9001, "jsmart", "User")

	broken, err := env.store.CreateGithubAppSource(ctx, core.GithubAppCreds{
		AppID: 77, Slug: "outhaul-b", PrivateKey: "not-a-pem",
		WebhookSecret: "whs", ClientID: "cid", ClientSecret: "csec",
	})
	if err != nil {
		t.Fatalf("CreateGithubAppSource: %v", err)
	}
	if err := env.store.BindGithubInstallation(ctx, broken.ID, 9002, "acme-corp", "Organization"); err != nil {
		t.Fatalf("BindGithubInstallation: %v", err)
	}
	env.gh.Repos = []github.Repo{{FullName: "jsmart/outhaul", DefaultBranch: "main"}}

	data := env.srv.gitSourceData(httptest.NewRequest("GET", "/apps", nil))
	groups, _ := data["RepoGroups"].([]repoGroup)
	var haveGood bool
	for _, g := range groups {
		if g.SourceID == good {
			haveGood = true
		}
		if g.SourceID == broken.ID {
			t.Error("a source whose key cannot mint a JWT must not offer repos")
		}
	}
	if !haveGood {
		t.Error("a failing source must not remove the working source's repos")
	}
	if data["GitSourceConnected"] != true {
		t.Error("GitSourceConnected must stay true while any source is installed")
	}
}

func TestCreateAppRejectsAnUnknownGitSource(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)
	installSource(t, env, 55, "outhaul-a", 9001, "jsmart", "User")

	form := url.Values{
		"name": {"web"}, "domain": {"web.test"}, "source": {"github"},
		"github_repo": {"jsmart/outhaul"}, "git_source_id": {"4242"},
		"branch": {"main"}, "kind": {"nixpacks"},
	}
	res := env.postForm(t, "/apps", form)
	if res.StatusCode == http.StatusSeeOther {
		t.Fatal("app created against a git source that does not exist")
	}
	if _, err := env.store.GetAppByName(context.Background(), "web"); err == nil {
		t.Error("app row created despite the bad source")
	}
}

func TestCreateAppStoresTheChosenGitSource(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)
	id := installSource(t, env, 55, "outhaul-a", 9001, "jsmart", "User")
	env.gh.Repos = []github.Repo{{FullName: "jsmart/outhaul", DefaultBranch: "main"}}

	form := url.Values{
		"name": {"web"}, "domain": {"web.test"}, "source": {"github"},
		"github_repo": {"jsmart/outhaul"}, "git_source_id": {strconv.FormatInt(id, 10)},
		"branch": {"main"}, "kind": {"nixpacks"},
	}
	if res := env.postForm(t, "/apps", form); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", res.StatusCode)
	}
	app, err := env.store.GetAppByName(context.Background(), "web")
	if err != nil {
		t.Fatalf("GetAppByName: %v", err)
	}
	if app.GitSourceID != id {
		t.Errorf("GitSourceID = %d, want %d", app.GitSourceID, id)
	}
	if app.Source != core.SourceGithub {
		t.Errorf("source = %q", app.Source)
	}
}
```

`connectApp` is Task 5's helper; `env.get`, `env.postForm`, `body` and `itoa` are the existing ones in `server_test.go`. Imports: add `net/http/httptest` and `context`.

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/server/ -run 'AppForm|RepoGroups|CreateApp' -v`
Expected: FAIL — `repoGroup` and `gitSourceData` undefined.

- [ ] **Step 3: Replace the repo cache with a per-source one**

`internal/server/server.go` — replace the two cache fields:

```go
	ghReposMu sync.Mutex
	ghRepos   map[int64]*repoCache // per git source; see gitSourceData
```

`internal/server/handlers.go` — replace `ghRepoCache`, `cachedRepos`, `storeRepos`:

```go
// repoCache memoizes one source's repo list. Listing costs two sequential
// api.github.com round-trips (token exchange + list), and every app/create/
// project render fills a repo dropdown — without this, each page open paid that
// latency per connected account, and blocked on GitHub being reachable.
type repoCache struct {
	repos     []gitsource.Repo
	fetchedAt time.Time
}

func (s *Server) cachedRepos(sourceID int64) (repos []gitsource.Repo, fresh bool) {
	s.ghReposMu.Lock()
	defer s.ghReposMu.Unlock()
	c := s.ghRepos[sourceID]
	if c == nil {
		return nil, false
	}
	return c.repos, time.Since(c.fetchedAt) < ghRepoTTL
}

func (s *Server) storeRepos(sourceID int64, repos []gitsource.Repo) {
	s.ghReposMu.Lock()
	defer s.ghReposMu.Unlock()
	if s.ghRepos == nil {
		s.ghRepos = map[int64]*repoCache{}
	}
	s.ghRepos[sourceID] = &repoCache{repos: repos, fetchedAt: time.Now()}
}
```

Keep the existing `ghRepoTTL` constant (`60 * time.Second`) and its comment exactly as they are — only the cache's keying changes, not its freshness policy. Amend `ghRepoTTL`'s comment only if it mentions the installation.

- [ ] **Step 4: Write `gitSourceData`**

Replace `githubRepoData` in `internal/server/handlers.go`:

```go
// repoGroup is one connected account's repositories, rendered as an <optgroup>.
type repoGroup struct {
	SourceID     int64
	AccountLogin string
	AccountKind  string // "personal" | "org" | ""
	Repos        []gitsource.Repo
}

// gitSourceData describes connected git sources for the create-app and
// change-source forms: "GitSourceConnected" when at least one source is
// installed, and "RepoGroups" — every account's repos, in one list the operator
// picks from directly.
//
// Each source degrades on its own: a source whose fetch fails falls back to its
// stale cache, or drops out. One unreachable account must never blank the whole
// dropdown, which is what a single shared cache used to do.
func (s *Server) gitSourceData(r *http.Request) map[string]any {
	data := map[string]any{}
	sources, err := s.store.ListGitSources(r.Context())
	if err != nil {
		log.Printf("git sources: %v", err)
		return data
	}
	var groups []repoGroup
	for _, src := range sources {
		if !src.Installed() {
			continue
		}
		data["GitSourceConnected"] = true
		repos, ok := s.reposFor(r.Context(), src)
		if !ok || len(repos) == 0 {
			continue
		}
		groups = append(groups, repoGroup{
			SourceID: src.ID, AccountLogin: src.Display(),
			AccountKind: src.AccountKind(), Repos: repos,
		})
	}
	if len(groups) > 0 {
		data["RepoGroups"] = groups
	}
	return data
}

// reposFor returns a source's repositories, preferring a fresh cache and
// falling back to a stale one when the refetch fails.
func (s *Server) reposFor(ctx context.Context, src core.GitSource) ([]gitsource.Repo, bool) {
	cached, fresh := s.cachedRepos(src.ID)
	if fresh {
		return cached, true
	}
	provider, err := s.sources.For(src.Kind)
	if err != nil {
		return cached, cached != nil
	}
	repos, err := provider.Repos(ctx, src)
	if err != nil {
		log.Printf("git source %s: list repos: %v", src.Display(), err)
		return cached, cached != nil
	}
	s.storeRepos(src.ID, repos)
	return repos, true
}
```

Update the two or three call sites of `githubRepoData` (the Apps page and project-detail render paths — `grep -rn githubRepoData internal/server`) to call `gitSourceData`.

- [ ] **Step 5: Validate the submitted pair**

Add to `internal/server/handlers.go`:

```go
// resolveRepoSource checks that a submitted (git source, repo) pair is real:
// the source must exist and be installed, and — when we hold a fresh repo list
// for it — must actually contain the repo. A stale or missing cache accepts the
// pair rather than blocking on GitHub; a wrong pair then fails loudly at clone
// time instead of silently deploying someone else's repo.
func (s *Server) resolveRepoSource(ctx context.Context, sourceID int64, fullName string) (int64, string) {
	if sourceID == 0 {
		return 0, "Choose a repository from a connected GitHub account."
	}
	src, ok, err := s.store.GetGitSource(ctx, sourceID)
	if err != nil {
		return 0, "Could not read the connected GitHub account."
	}
	if !ok || !src.Installed() {
		return 0, "That GitHub account is not connected. Reconnect it in Settings."
	}
	if repos, fresh := s.cachedRepos(sourceID); fresh {
		for _, repo := range repos {
			if repo.FullName == fullName {
				return sourceID, ""
			}
		}
		return 0, "That repository is not available from the selected GitHub account."
	}
	return sourceID, ""
}
```

In `handleCreateApp`, after `githubRepo` is read:

```go
	githubRepo := strings.TrimSpace(r.FormValue("github_repo"))
	var gitSourceID int64
	if source == core.SourceGithub {
		id, _ := parseID(strings.TrimSpace(r.FormValue("git_source_id")))
		resolved, verr := s.resolveRepoSource(r.Context(), id, githubRepo)
		if verr != "" {
			s.renderAppsWithError(w, r, verr, name, repo, domain)
			return
		}
		gitSourceID = resolved
		repo = "https://github.com/" + githubRepo + ".git"
	}
```

and add `GitSourceID: gitSourceID` to the `core.App` literal.

In `handleUpdateAppSource`, mirror it and pass the id through (replacing the `0` left by Task 2):

```go
	var gitSourceID int64
	if source == core.SourceGithub {
		id, _ := parseID(strings.TrimSpace(r.FormValue("git_source_id")))
		resolved, verr := s.resolveRepoSource(r.Context(), id, githubRepo)
		if verr != "" {
			http.Error(w, verr, http.StatusBadRequest)
			return
		}
		gitSourceID = resolved
		repo = "https://github.com/" + githubRepo + ".git"
	}
	...
	if err := s.store.UpdateAppSource(r.Context(), id, source, repo, githubRepo, gitSourceID, pub, priv); err != nil {
```

Note `id` is already the app id in that handler — name the parsed source id `srcID` to avoid shadowing.

- [ ] **Step 6: Update `appform.tmpl`**

Replace the source/repo fields (lines 33-59):

```html
        <label class="field">
          <span>Source {{template "hint" "Where your code lives. Public Git URL needs no credentials; SSH uses a deploy key; GitHub App lets you pick a repo from a connected account and auto-deploy on push."}}</span>
          <select name="source" id="source-select">
            <option value="public">Public Git URL</option>
            <option value="ssh">Private (SSH deploy key)</option>
            <option value="push">Push to deploy (git push)</option>
            {{if .GitSourceConnected}}<option value="github">GitHub App</option>{{end}}
          </select>
        </label>
        {{if .RepoGroups}}
        <label class="field" data-source="github">
          <span>Repository {{template "hint" "Every repository Outhaul can reach, grouped by the account it comes from."}}</span>
          <input type="text" id="repo-filter" placeholder="filter repos…" autocomplete="off">
          <select name="github_repo" id="github-repo-select" size="8">
            {{range .RepoGroups}}{{$group := .}}
            <optgroup label="{{.AccountLogin}}{{if .AccountKind}} ({{.AccountKind}}){{end}}">
              {{range .Repos}}<option value="{{.FullName}}" data-source-id="{{$group.SourceID}}" data-default-branch="{{.DefaultBranch}}">{{.FullName}}</option>{{end}}
            </optgroup>
            {{end}}
          </select>
          <input type="hidden" name="git_source_id" id="git-source-id">
        </label>
        {{end}}
        <label class="field" data-source="public ssh">
          <span>Git URL {{template "hint" "The clone URL of your repository, e.g. https://github.com/owner/repo.git"}}</span>
          <input type="text" name="repo_url" placeholder="https://github.com/owner/repo.git" value="{{with .Form}}{{.RepoURL}}{{end}}">
        </label>
        <label class="field">
          <span>Branch {{template "hint" "The Git branch to deploy. For a GitHub repo this is filled with the repo's default branch automatically."}}</span>
          <input type="text" name="branch" id="branch-input" placeholder="main" value="main">
        </label>
        {{if not .GitSourceConnected}}<p class="env-note"><a href="/github/connect">Connect a GitHub account</a> to deploy private repos and auto-deploy on push.</p>{{end}}
```

- [ ] **Step 7: Extend the form script**

In `appform.tmpl`'s script block, replace `syncBranch` and add the filter:

```js
    var source = document.getElementById('source-select');
    var repo = document.getElementById('github-repo-select');
    var branch = document.getElementById('branch-input');
    var sourceID = document.getElementById('git-source-id');
    var filter = document.getElementById('repo-filter');

    // Picking a repo also picks the account it came from: the option carries
    // its source, so the operator never chooses an account separately.
    function syncRepo() {
      if (!repo || !source || source.value !== 'github') return;
      var opt = repo.options[repo.selectedIndex];
      if (!opt) return;
      var def = opt.getAttribute('data-default-branch');
      if (def && branch) branch.value = def;
      if (sourceID) sourceID.value = opt.getAttribute('data-source-id') || '';
    }
    if (repo) repo.addEventListener('change', syncRepo);

    // Filtering hides options in place; with JS off the full grouped list and
    // the browser's own type-ahead still work.
    if (filter && repo) {
      filter.addEventListener('input', function () {
        var q = filter.value.toLowerCase();
        for (var i = 0; i < repo.options.length; i++) {
          var o = repo.options[i];
          o.hidden = q !== '' && o.value.toLowerCase().indexOf(q) === -1;
        }
      });
    }
```

Replace the two existing `syncBranch()` calls inside `applySource` with `syncRepo()`, and call `syncRepo()` once at the end of the IIFE so the hidden field is populated before any change event.

- [ ] **Step 8: Update `app.tmpl`'s change-source form**

Replace lines 440-446 with the same grouped select, keeping the current selection:

```html
    {{if .RepoGroups}}
    <label class="field" data-src="github"><span>Repository</span>
      <input type="text" id="src-repo-filter" placeholder="filter repos…" autocomplete="off">
      <select name="github_repo" id="src-github-repo" size="8">
        {{range .RepoGroups}}{{$group := .}}
        <optgroup label="{{.AccountLogin}}{{if .AccountKind}} ({{.AccountKind}}){{end}}">
          {{range .Repos}}<option value="{{.FullName}}" data-source-id="{{$group.SourceID}}"{{if and (eq .FullName $.App.GithubRepo) (eq $group.SourceID $.App.GitSourceID)}} selected{{end}}>{{.FullName}}</option>{{end}}
        </optgroup>
        {{end}}
      </select>
      <input type="hidden" name="git_source_id" id="src-git-source-id" value="{{.App.GitSourceID}}">
    </label>
    {{end}}
```

Add the matching `change` listener for `src-github-repo` → `src-git-source-id` in `app.tmpl`'s existing script block, mirroring `syncRepo` above. Change the `{{if .GithubConnected}}` guard on the source `<option>` (line 437) to `{{if or .GitSourceConnected (eq .App.Source "github")}}`.

- [ ] **Step 9: Run to verify it passes**

Run: `go build ./... && go test ./internal/server/ -v`
Expected: PASS. Template parse errors surface as a failure in `New`, so a bad `{{$group}}` binding fails every server test at once.

- [ ] **Step 10: Commit**

```bash
git add internal/server/handlers.go internal/server/server.go internal/server/repopicker_test.go \
        internal/server/templates/appform.tmpl internal/server/templates/app.tmpl
git commit -m "feat(server): one grouped repo picker across every connected account

Repos from all connected accounts appear in one list, grouped by account,
so picking a repo also picks its credentials — one step fewer than
Dokploy's provider-then-repo cascade, and with a single account connected
the form is unchanged.

The repo cache is now per source with per-source stale fallback: one
unreachable account used to blank the entire dropdown."
```

---

### Task 10: Settings — list and remove git sources

**Files:**
- Modify: `internal/server/settings.go:20-30` (`renderSettings`)
- Modify: `internal/server/server.go` (route registration, near line 270)
- Modify: `internal/server/templates/settings.tmpl:7-18`
- Create: `internal/server/gitsources_settings_test.go`

**Interfaces:**
- Consumes: `store.ListGitSources`, `store.GetGitSource`, `store.DeleteGitSource` (Task 1); `store.AppsUsingGitSource` (Task 2); `github.Client.Installation`, `github.AppJWT` (Task 3).
- Produces:
  - Route `POST /settings/git-sources/{id}/delete` → `(*Server).handleDeleteGitSource`
  - `renderSettings` template key `GitSources` (`[]core.GitSource`)
  - `(*Server).backfillAccounts(ctx)`

- [ ] **Step 1: Write the failing test**

Create `internal/server/gitsources_settings_test.go`:

```go
package server

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/outhaul-dev/outhaul/internal/core"
	"github.com/outhaul-dev/outhaul/internal/github"
)

func TestSettingsListsEveryConnectedAccount(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)
	installSource(t, env, 55, "outhaul-a", 9001, "jsmart", "User")
	installSource(t, env, 77, "outhaul-b", 9002, "acme-corp", "Organization")

	page := body(t, env.get(t, "/settings"))
	for _, want := range []string{"jsmart", "acme-corp", "Connect another account"} {
		if !strings.Contains(page, want) {
			t.Errorf("settings page missing %q", want)
		}
	}
}

func TestRemovingAReferencedSourceIsRefused(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)
	ctx := context.Background()
	id := installSource(t, env, 55, "outhaul-a", 9001, "jsmart", "User")
	env.store.CreateApp(ctx, core.App{
		Name: "web", Domain: "web.test", Source: core.SourceGithub, Kind: core.KindNixpacks,
		GithubRepo: "jsmart/outhaul", GitSourceID: id, WebhookSecret: "w",
	})

	res := env.postForm(t, "/settings/git-sources/"+strconv.FormatInt(id, 10)+"/delete", url.Values{})
	if res.StatusCode == http.StatusSeeOther {
		t.Fatal("removed a source that apps still use")
	}
	if _, ok, _ := env.store.GetGitSource(ctx, id); !ok {
		t.Fatal("source was deleted despite being referenced")
	}
	if page := body(t, res); !strings.Contains(page, "web") {
		t.Error("the refusal must name the apps that depend on the source")
	}
}

func TestRemovingAnUnreferencedSourceSucceeds(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)
	id := installSource(t, env, 55, "outhaul-a", 9001, "jsmart", "User")

	env.postForm(t, "/settings/git-sources/"+strconv.FormatInt(id, 10)+"/delete", url.Values{})
	if _, ok, _ := env.store.GetGitSource(context.Background(), id); ok {
		t.Error("source still present after removing it")
	}
}

// A source migrated from the pre-0022 record has no account name; the settings
// page is where we finally ask GitHub for one.
func TestSettingsBackfillsAMissingAccountName(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)
	ctx := context.Background()
	id := connectApp(t, env, 55, "outhaul-a")
	if err := env.store.BindGithubInstallation(ctx, id, 9001, "", ""); err != nil {
		t.Fatalf("BindGithubInstallation: %v", err)
	}
	env.gh.InstallationsByApp = map[int64][]github.Installation{
		55: {{ID: 9001, AccountLogin: "jsmart", AccountType: "User"}},
	}

	body(t, env.get(t, "/settings"))

	src, _, _ := env.store.GetGitSource(ctx, id)
	if src.AccountLogin != "jsmart" {
		t.Errorf("account login = %q, want it backfilled to jsmart", src.AccountLogin)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/server/ -run 'Settings|Removing' -v`
Expected: FAIL — the route does not exist (404) and the page still shows the old single-App panel.

- [ ] **Step 3: Update `renderSettings`**

In `internal/server/settings.go`, replace the `GithubSlug`/`GithubInstalled` block:

```go
	s.backfillAccounts(r.Context())
	if sources, err := s.store.ListGitSources(r.Context()); err == nil {
		data["GitSources"] = sources
	}
```

and add to the same file:

```go
// backfillAccounts names sources that have none. A source carried over from the
// pre-0022 single-App record was never told which account it belongs to,
// because the old flow never asked. Failures are logged and ignored — Display()
// falls back to the App slug, so the page always renders.
func (s *Server) backfillAccounts(ctx context.Context) {
	sources, err := s.store.ListGitSources(ctx)
	if err != nil {
		return
	}
	for _, src := range sources {
		if src.AccountLogin != "" || !src.Installed() || src.Kind != core.GitSourceGithubApp {
			continue
		}
		jwt, err := github.AppJWT(src.GithubApp.PrivateKey, src.GithubApp.AppID, time.Now())
		if err != nil {
			log.Printf("git source %s: app jwt: %v", src.Display(), err)
			continue
		}
		inst, err := s.gh.Installation(ctx, jwt, src.GithubApp.InstallationID)
		if err != nil {
			log.Printf("git source %s: read installation: %v", src.Display(), err)
			continue
		}
		if err := s.store.SetGitSourceAccount(ctx, src.ID, inst.AccountLogin, inst.AccountType); err != nil {
			log.Printf("git source %s: record account: %v", src.Display(), err)
		}
	}
}
```

- [ ] **Step 4: Add the delete handler**

Append to `internal/server/settings.go`:

```go
// handleDeleteGitSource removes a connected account, refusing while apps still
// depend on it. Deleting anyway would leave running apps un-deployable, and a
// Settings action must not have that blast radius — so the operator is shown
// exactly which apps to move first.
func (s *Server) handleDeleteGitSource(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	src, found, err := s.store.GetGitSource(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !found {
		http.NotFound(w, r)
		return
	}
	users, err := s.store.AppsUsingGitSource(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if len(users) > 0 {
		names := make([]string, 0, len(users))
		for _, app := range users {
			names = append(names, app.Name)
		}
		s.renderSettings(w, r, http.StatusBadRequest, fmt.Sprintf(
			"Cannot remove %s — %d app(s) still use it: %s. Change their source or delete them first.",
			src.Display(), len(users), strings.Join(names, ", ")))
		return
	}
	if err := s.store.DeleteGitSource(r.Context(), id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}
```

Add `context`, `fmt`, `log`, `strings`, `time`, `internal/core`, `internal/github` to the file's imports as needed.

- [ ] **Step 5: Register the route**

In `internal/server/server.go`, beside the other `/settings/...` routes:

```go
	mux.HandleFunc("POST /settings/git-sources/{id}/delete", s.requireAuth(s.handleDeleteGitSource))
```

- [ ] **Step 6: Replace the Settings panel**

`internal/server/templates/settings.tmpl` lines 7-18:

```html
<section class="panel">
  <h2 class="panel-title">Git sources {{template "hint" "The GitHub accounts and organizations Outhaul can deploy from. Each is its own GitHub App: a private App can only be installed on the account that owns it, so a second account means a second App."}}</h2>
  {{if .NeedsPublicURL}}
  <p class="env-note">Set <code>OUTHAUL_PUBLIC_URL</code> to your admin UI's public HTTPS URL to connect a GitHub account.</p>
  {{else}}
  {{if .GitSources}}
  <table class="table">
    <thead><tr><th>Account</th><th>Type</th><th>App</th><th>Status</th><th></th></tr></thead>
    <tbody>
      {{range .GitSources}}
      <tr>
        <td>{{.Display}}</td>
        <td>{{if .AccountKind}}{{.AccountKind}}{{else}}—{{end}}</td>
        <td><span class="mono">{{.GithubApp.Slug}}</span></td>
        <td>{{if .Installed}}Installed{{else}}<a href="/github/connect">Not installed</a>{{end}}</td>
        <td><form method="post" action="/settings/git-sources/{{.ID}}/delete"><button type="submit" class="btn btn-danger">Remove</button></form></td>
      </tr>
      {{end}}
    </tbody>
  </table>
  {{else}}
  <p>No GitHub account connected yet. Connect one to deploy private repos and auto-deploy on push.</p>
  {{end}}
  <p><a href="/github/connect" class="btn">Connect another account</a></p>
  {{end}}
</section>
```

- [ ] **Step 7: Run to verify it passes**

Run: `go build ./... && go test ./internal/server/ -v`
Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add internal/server/settings.go internal/server/server.go \
        internal/server/templates/settings.tmpl internal/server/gitsources_settings_test.go
git commit -m "feat(server): manage connected git sources from Settings

Lists every connected account and adds another. Removal is refused while
apps still depend on the source and names them, so a Settings click can
never leave a running app un-deployable.

Accounts carried over from the single-App record have no name recorded;
the settings render backfills it."
```

---

### Task 11: Retire the legacy App record and document the feature

**Files:**
- Create: `internal/store/migrations/0023_drop_github_app.sql`
- Modify: `internal/store/github.go` (delete), `internal/core/github.go` (delete)
- Modify: `internal/store/apps.go` (delete the unscoped `AppsByGithubRepo`)
- Modify: `ARCHITECTURE.md`, `README.md`
- Modify: `docs/superpowers/specs/2026-09-03-multi-git-sources-design.md` (status line)

**Interfaces:**
- Consumes: everything above.
- Produces: no new API. Removes `store.GithubApp`, `store.SetGithubApp`, `store.SetInstallationID`, `core.GithubApp`, `store.AppsByGithubRepo`.

- [ ] **Step 1: Confirm nothing still references the legacy API**

Run: `grep -rn "GithubApp\b\|SetInstallationID\|AppsByGithubRepo(" --include='*.go' . | grep -v GithubAppCreds | grep -v GithubAppSource | grep -v AppsByGithubRepoSource`
Expected: only `internal/store/github.go` and `internal/core/github.go` themselves. Anything else must be migrated before continuing.

- [ ] **Step 2: Write the drop migration**

Create `internal/store/migrations/0023_drop_github_app.sql`:

```sql
-- The single-App record is now carried by git_sources (0022) and nothing reads
-- it. Dropping it here rather than in 0022 kept every commit on the way to
-- multi-account support compiling and green.
DROP TABLE IF EXISTS github_app;
```

- [ ] **Step 3: Delete the legacy code**

```bash
git rm internal/store/github.go internal/core/github.go
```

Delete `AppsByGithubRepo` from `internal/store/apps.go` (keep `AppsByGithubRepoSource`). Delete any test that exercised the removed methods — chiefly parts of `internal/store/store_test.go` and `internal/server/github_test.go`.

- [ ] **Step 4: Run the full suite**

Run: `go build ./... && go test ./...`
Expected: PASS. A failure here means a caller was missed in Step 1.

- [ ] **Step 5: Verify the migration test still covers the copy**

`TestMigrationCarriesLegacyGithubApp` (Task 1) seeds `github_app` directly, which 0023 now drops on every fresh `Open`. Update it: after the initial `Open`, also `DELETE FROM schema_migrations WHERE name = 'migrations/0023_drop_github_app.sql'` and `CREATE TABLE github_app (...)` before seeding, so both migrations replay in order.

Run: `go test ./internal/store/ -run TestMigrationCarriesLegacyGithubApp -v`
Expected: PASS.

- [ ] **Step 6: Update the docs**

In `ARCHITECTURE.md`, amend the M3 design-decisions paragraph (around line 410) so it describes sources rather than a single App:

```markdown
Design decisions from M3: private-repo access goes through **GitHub Apps**, set
up via GitHub's manifest flow (the operator submits a pre-filled manifest,
GitHub redirects back with a temporary code that is exchanged for the App's
credentials — no manual "create an app" form-filling). Outhaul creates
*private* Apps, and GitHub only installs a private App on the account that owns
it, so one App is one account: connecting a second account or an organization
means a second App. Each is stored as a **git source** — a generic
`git_sources` identity row plus a per-kind credential table — and read through
the `internal/gitsource` `Provider` interface, which has exactly one
implementation today. Apps carry `git_source_id`, so the create-app form offers
every account's repositories in one grouped list and the chosen repo also
chooses its credentials.

Every App posts to the same `/webhooks/github` path (GitHub does not allow
rewriting an installed App's hook URL), so a delivery is matched to its source
by the `X-GitHub-Hook-Installation-Target-ID` header and verified against only
that source's secret. Fan-out is scoped to the same source: two connected
accounts can each expose the same `owner/repo`, and a push for one must never
deploy the other's app.

Clones authenticate with a short-lived **installation access token** minted from
the source's private key, so no long-lived user PAT is ever stored.
```

In `README.md`, update the private-repos bullet:

```markdown
- **Private repos** — connect one or more GitHub accounts or organizations (each its own GitHub App), plus per-app SSH deploy keys.
```

Set the spec's status line to `**Status:** Implemented`.

- [ ] **Step 7: Final verification and commit**

Run: `go build ./... && go test ./... && go vet ./...`
Expected: PASS.

```bash
git add -A
git commit -m "refactor: drop the single-App record, document git sources

Nothing reads github_app any more, so 0023 drops it. Deleting it here
rather than in 0022 kept every commit on the way green."
```

---

## Post-implementation manual verification

Automated tests do not exercise GitHub itself. Before merging, on a test server with `OUTHAUL_PUBLIC_URL` set:

1. Connect a personal account; confirm Settings names it correctly (not the App slug).
2. Connect an organization via the org radio; confirm the App form opens under the org and Settings shows `org`.
3. Create an app from a repo in each account; confirm both deploy.
4. Push to each repo; confirm each deploys only its own app (check the other's deployment list stays put).
5. Try removing a source in use; confirm the refusal names the app and nothing is deleted.
6. Upgrade an existing install with a connected App: confirm its apps still deploy without reconnecting, and that its account name appears after one Settings visit.
