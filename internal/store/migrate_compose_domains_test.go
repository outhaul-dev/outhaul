package store

import (
	"database/sql"
	"io/fs"
	"path/filepath"
	"sort"
	"testing"
)

// TestComposeDomainsMigrationBackfill replays the pre-0006 world — a compose
// app exposing one service via the old inline domain/compose_service/
// compose_port columns — and checks migration 0006 moves that exposure into
// compose_domains, blanks the compose app's domain, and leaves nixpacks
// domains alone. Fresh databases never hit this path, so it is pinned here.
func TestComposeDomainsMigrationBackfill(t *testing.T) {
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	// Apply everything before 0006 by hand, recording each as migrate would.
	if _, err := db.Exec(`CREATE TABLE schema_migrations (name TEXT PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	names, err := fs.Glob(migrationsFS, "migrations/*.sql")
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(names)
	for _, name := range names {
		if name >= "migrations/0006" {
			break
		}
		body, err := migrationsFS.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(string(body)); err != nil {
			t.Fatalf("apply %s: %v", name, err)
		}
		if _, err := db.Exec(`INSERT INTO schema_migrations (name, applied_at) VALUES (?, datetime('now'))`, name); err != nil {
			t.Fatal(err)
		}
	}

	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	mustExec(`INSERT INTO apps (name, repo_url, domain, created_at, kind, compose_service, compose_port)
	          VALUES ('shop', 'https://example.com/shop.git', 'shop.example.com', '2026-07-04T00:00:00Z', 'compose', 'web', 3000)`)
	mustExec(`INSERT INTO apps (name, repo_url, domain, created_at, kind)
	          VALUES ('internal', 'https://example.com/internal.git', '', '2026-07-04T00:00:00Z', 'compose')`)
	mustExec(`INSERT INTO apps (name, repo_url, domain, created_at)
	          VALUES ('web', 'https://example.com/web.git', 'web.example.com', '2026-07-04T00:00:00Z')`)

	if err := migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	var domain, service string
	var port int
	err = db.QueryRow(`SELECT d.domain, d.service, d.port FROM compose_domains d
	                   JOIN apps a ON a.id = d.app_id WHERE a.name = 'shop'`).Scan(&domain, &service, &port)
	if err != nil {
		t.Fatalf("backfilled domain row: %v", err)
	}
	if domain != "shop.example.com" || service != "web" || port != 3000 {
		t.Errorf("backfill = %s → %s:%d, want shop.example.com → web:3000", domain, service, port)
	}

	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM compose_domains`).Scan(&n); err != nil || n != 1 {
		t.Errorf("compose_domains rows = %d (%v), want only the exposed stack's", n, err)
	}
	var shopDomain, webDomain string
	if err := db.QueryRow(`SELECT domain FROM apps WHERE name = 'shop'`).Scan(&shopDomain); err != nil || shopDomain != "" {
		t.Errorf("compose app domain = %q (%v), want blanked", shopDomain, err)
	}
	if err := db.QueryRow(`SELECT domain FROM apps WHERE name = 'web'`).Scan(&webDomain); err != nil || webDomain != "web.example.com" {
		t.Errorf("nixpacks domain = %q (%v), want untouched", webDomain, err)
	}
	if _, err := db.Query(`SELECT compose_service FROM apps`); err == nil {
		t.Error("compose_service column should be dropped by 0006")
	}
}
