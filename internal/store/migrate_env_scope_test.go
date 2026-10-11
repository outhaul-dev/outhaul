package store

import (
	"database/sql"
	"io/fs"
	"path/filepath"
	"sort"
	"testing"
)

// TestEnvScopeUniqueMigration replays a pre-0024 database holding a scoped env
// row and checks the rebuild keeps every row (id, value, scope) and then lets
// the same key exist once per scope.
func TestEnvScopeUniqueMigration(t *testing.T) {
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	if _, err := db.Exec(`CREATE TABLE schema_migrations (name TEXT PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	names, err := fs.Glob(migrationsFS, "migrations/*.sql")
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(names)
	for _, name := range names {
		if name >= "migrations/0024" {
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
	mustExec(`INSERT INTO apps (id, name, repo_url, domain, created_at) VALUES (7, 'web', 'r', 'web.example.com', '2026-07-04T00:00:00Z')`)
	mustExec(`INSERT INTO app_env (id, app_id, key, value, is_secret, scope, created_at)
	          VALUES (42, 7, 'DATABASE_URL', 'sealed-prod', 1, 'prod', '2026-07-04T00:00:00Z')`)

	if err := migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	var id int64
	var value, scope string
	var secret int
	if err := db.QueryRow(`SELECT id, value, is_secret, scope FROM app_env WHERE app_id = 7 AND key = 'DATABASE_URL'`).Scan(&id, &value, &secret, &scope); err != nil {
		t.Fatalf("env row after migration: %v", err)
	}
	if id != 42 || value != "sealed-prod" || secret != 1 || scope != "prod" {
		t.Fatalf("row changed: id=%d value=%q secret=%d scope=%q", id, value, secret, scope)
	}

	mustExec(`INSERT INTO app_env (app_id, key, value, is_secret, scope, created_at)
	          VALUES (7, 'DATABASE_URL', 'sealed-preview', 1, 'preview', '2026-07-04T00:00:00Z')`)
	if _, err := db.Exec(`INSERT INTO app_env (app_id, key, value, is_secret, scope, created_at)
	          VALUES (7, 'DATABASE_URL', 'dup', 0, 'preview', '2026-07-04T00:00:00Z')`); err == nil {
		t.Fatal("a second row for the same (app, key, scope) was accepted")
	}

	// The app-delete cascade must survive the rebuild.
	if _, err := db.Exec(`PRAGMA foreign_keys = ON`); err != nil {
		t.Fatal(err)
	}
	mustExec(`DELETE FROM apps WHERE id = 7`)
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM app_env WHERE app_id = 7`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("env rows survived app delete: %d", n)
	}
}
