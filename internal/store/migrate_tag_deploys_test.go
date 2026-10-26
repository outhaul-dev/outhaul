package store

import "testing"

// Existing apps stay in branch mode, and existing deployments read as branch
// builds that no tag push queued.
func TestTagDeployMigrationDefaults(t *testing.T) {
	db := openMigratedBefore(t, "0026")
	if _, err := db.Exec(`INSERT INTO apps (id, name, repo_url, domain, created_at, webhook_secret)
	                      VALUES (7, 'web', 'r', 'web.example.com', '2026-07-04T00:00:00Z', 'abc123')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO deployments (app_id, status, created_at)
	                      VALUES (7, 'queued', '2026-07-04T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if err := migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	var trigger, pattern string
	var pre int
	if err := db.QueryRow(`SELECT deploy_trigger, tag_pattern, tag_prereleases FROM apps WHERE id = 7`).
		Scan(&trigger, &pattern, &pre); err != nil {
		t.Fatal(err)
	}
	if trigger != "branch" || pattern != "v*" || pre != 0 {
		t.Errorf("app = %q %q %d, want branch v* 0", trigger, pattern, pre)
	}
	var tag string
	var fromTag int
	if err := db.QueryRow(`SELECT tag, from_tag_event FROM deployments WHERE app_id = 7`).Scan(&tag, &fromTag); err != nil {
		t.Fatal(err)
	}
	if tag != "" || fromTag != 0 {
		t.Errorf("deployment = %q %d, want empty 0", tag, fromTag)
	}
}
