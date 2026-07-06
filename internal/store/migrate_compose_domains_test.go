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

	// The compose app's exposure survives the 0006→0015 move into `domains`.
	var host, service string
	var port int
	err = db.QueryRow(`SELECT d.host, d.service, d.port FROM domains d
	                   JOIN apps a ON a.id = d.app_id WHERE a.name = 'shop'`).Scan(&host, &service, &port)
	if err != nil {
		t.Fatalf("shop domain row: %v", err)
	}
	if host != "shop.example.com" || service != "web" || port != 3000 {
		t.Errorf("shop = %s → %s:%d, want shop.example.com → web:3000", host, service, port)
	}

	// The nixpacks app is backfilled as a service-less row on 8080.
	err = db.QueryRow(`SELECT d.host, d.service, d.port FROM domains d
	                   JOIN apps a ON a.id = d.app_id WHERE a.name = 'web'`).Scan(&host, &service, &port)
	if err != nil {
		t.Fatalf("web domain row: %v", err)
	}
	if host != "web.example.com" || service != "" || port != 8080 {
		t.Errorf("web = %s → %s:%d, want web.example.com → :8080", host, service, port)
	}

	// The internal compose app (blank domain) gets no row; total is exactly 2.
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM domains`).Scan(&n); err != nil || n != 2 {
		t.Errorf("domains rows = %d (%v), want 2", n, err)
	}

	// 0015 syncs apps.domain to the folded row's host for every migrated app.
	var shopMirror string
	if err := db.QueryRow(`SELECT domain FROM apps WHERE name = 'shop'`).Scan(&shopMirror); err != nil || shopMirror != "shop.example.com" {
		t.Errorf("compose app primary mirror = %q (%v), want shop.example.com after 0015 sync", shopMirror, err)
	}
	if _, err := db.Query(`SELECT 1 FROM compose_domains`); err == nil {
		t.Error("compose_domains table should be dropped by 0015")
	}
}
