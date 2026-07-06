package core

import "testing"

func TestValidEnvVar(t *testing.T) {
	valid := []string{"DATABASE_URL", "REDIS_URL", "A", "PG_1", "_FOO"}
	for _, v := range valid {
		if !ValidEnvVar(v) {
			t.Errorf("ValidEnvVar(%q) = false, want true", v)
		}
	}
	invalid := []string{"", "1DB", "database_url", "DB-URL", "DB URL", "DB.URL", "DB!"}
	for _, v := range invalid {
		if ValidEnvVar(v) {
			t.Errorf("ValidEnvVar(%q) = true, want false", v)
		}
	}
}
