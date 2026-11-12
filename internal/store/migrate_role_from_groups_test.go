package store

import "testing"

// Accounts that existed before the column reads as "not from a group".
func TestRoleFromGroupsDefaultsOffForExistingAccounts(t *testing.T) {
	db := openMigratedBefore(t, "0031")
	if _, err := db.Exec(`INSERT INTO users (username, password_hash, created_at, role)
	                      VALUES ('old', '', '2026-11-01T00:00:00Z', 'system_admin')`); err != nil {
		t.Fatal(err)
	}
	if err := migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	var fromGroups int
	if err := db.QueryRow(`SELECT role_from_groups FROM users WHERE username = 'old'`).Scan(&fromGroups); err != nil {
		t.Fatal(err)
	}
	if fromGroups != 0 {
		t.Errorf("role_from_groups = %d, want 0", fromGroups)
	}
}
