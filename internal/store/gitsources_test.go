package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

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
	// 0022 is written to run against a schema without its own tables, and
	// before it added apps.git_source_id.
	for _, stmt := range []string{
		`DROP TABLE github_app_sources`, `DROP TABLE git_sources`,
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
