package store

import (
	"database/sql"
	"io/fs"
	"path/filepath"
	"sort"
	"testing"
)

// openMigratedBefore returns a raw database with every migration that sorts
// before prefix applied and recorded, so a test can seed old-schema rows and
// then run migrate to replay the rest.
func openMigratedBefore(t *testing.T, prefix string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
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
		if name >= "migrations/"+prefix {
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
	return db
}

// An existing app's single webhook value becomes its URL token and, so senders
// already signing with it keep working, its secret as well.
func TestWebhookTokenMigrationKeepsExistingValue(t *testing.T) {
	db := openMigratedBefore(t, "0025")
	if _, err := db.Exec(`INSERT INTO apps (id, name, repo_url, domain, created_at, webhook_secret)
	                      VALUES (7, 'web', 'r', 'web.example.com', '2026-07-04T00:00:00Z', 'abc123')`); err != nil {
		t.Fatal(err)
	}

	if err := migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	var token, secret string
	if err := db.QueryRow(`SELECT webhook_token, webhook_secret FROM apps WHERE id = 7`).Scan(&token, &secret); err != nil {
		t.Fatalf("read app after migration: %v", err)
	}
	if token != "abc123" || secret != "abc123" {
		t.Fatalf("token=%q secret=%q, want both abc123", token, secret)
	}
}

// Preview children used to copy their parent's webhook value, so after the
// split both rows would answer the parent's /webhooks/app/<token> URL and the
// lookup could pick the preview. The migration strips children's credentials.
func TestWebhookTokenMigrationClearsPreviewChildren(t *testing.T) {
	db := openMigratedBefore(t, "0025")
	if _, err := db.Exec(`INSERT INTO apps (id, name, repo_url, domain, created_at, webhook_secret)
	                      VALUES (7, 'web', 'r', '', '2026-07-04T00:00:00Z', 'abc123')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO apps (id, name, repo_url, domain, created_at, webhook_secret, parent_id, pr_number, ephemeral)
	                      VALUES (8, 'web-pr-1', 'r', '', '2026-07-04T00:00:00Z', 'abc123', 7, 1, 1)`); err != nil {
		t.Fatal(err)
	}

	if err := migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	var token, secret string
	if err := db.QueryRow(`SELECT webhook_token, webhook_secret FROM apps WHERE id = 8`).Scan(&token, &secret); err != nil {
		t.Fatal(err)
	}
	if token != "" || secret != "" {
		t.Errorf("preview child token=%q secret=%q, want both empty", token, secret)
	}
	if err := db.QueryRow(`SELECT webhook_token FROM apps WHERE id = 7`).Scan(&token); err != nil || token != "abc123" {
		t.Errorf("parent token = %q (err %v), want abc123", token, err)
	}
}
