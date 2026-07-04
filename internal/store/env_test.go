package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/james-smart/outhaul/internal/core"
	"github.com/james-smart/outhaul/internal/secret"
)

func openWithBox(t *testing.T) *Store {
	t.Helper()
	box, err := secret.Load(filepath.Join(t.TempDir(), "key"))
	if err != nil {
		t.Fatalf("secret.Load: %v", err)
	}
	st, err := Open(filepath.Join(t.TempDir(), "test.db"), box)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestSetAndListEnv(t *testing.T) {
	st := openWithBox(t)
	ctx := context.Background()
	app, _ := st.CreateApp(ctx, core.App{Name: "web", RepoURL: "https://x/y.git", Domain: "web.test"})

	if err := st.SetEnv(ctx, app.ID, "DATABASE_URL", "postgres://u:p@h/db", true); err != nil {
		t.Fatalf("SetEnv: %v", err)
	}
	if err := st.SetEnv(ctx, app.ID, "LOG_LEVEL", "info", false); err != nil {
		t.Fatalf("SetEnv: %v", err)
	}

	vars, err := st.ListEnv(ctx, app.ID)
	if err != nil {
		t.Fatalf("ListEnv: %v", err)
	}
	if len(vars) != 2 {
		t.Fatalf("got %d vars, want 2", len(vars))
	}
	byKey := map[string]core.EnvVar{}
	for _, v := range vars {
		byKey[v.Key] = v
	}
	if byKey["DATABASE_URL"].Value != "postgres://u:p@h/db" || !byKey["DATABASE_URL"].IsSecret {
		t.Errorf("DATABASE_URL round trip wrong: %+v", byKey["DATABASE_URL"])
	}
	if byKey["LOG_LEVEL"].Value != "info" || byKey["LOG_LEVEL"].IsSecret {
		t.Errorf("LOG_LEVEL round trip wrong: %+v", byKey["LOG_LEVEL"])
	}
}

func TestSetEnvUpsert(t *testing.T) {
	st := openWithBox(t)
	ctx := context.Background()
	app, err := st.CreateApp(ctx, core.App{Name: "web", RepoURL: "https://x/y.git", Domain: "web.test"})
	if err != nil {
		t.Fatalf("CreateApp: %v", err)
	}

	if err := st.SetEnv(ctx, app.ID, "K", "v1", false); err != nil {
		t.Fatalf("SetEnv: %v", err)
	}
	if err := st.SetEnv(ctx, app.ID, "K", "v2", true); err != nil { // same key updates in place
		t.Fatalf("SetEnv: %v", err)
	}

	vars, _ := st.ListEnv(ctx, app.ID)
	if len(vars) != 1 || vars[0].Value != "v2" || !vars[0].IsSecret {
		t.Fatalf("upsert failed: %+v", vars)
	}
}

func TestEnvStoredAsCiphertext(t *testing.T) {
	st := openWithBox(t)
	ctx := context.Background()
	app, err := st.CreateApp(ctx, core.App{Name: "web", RepoURL: "https://x/y.git", Domain: "web.test"})
	if err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	if err := st.SetEnv(ctx, app.ID, "SECRET", "plaintextvalue", true); err != nil {
		t.Fatalf("SetEnv: %v", err)
	}

	var raw string
	if err := st.db.QueryRowContext(ctx, `SELECT value FROM app_env WHERE key = 'SECRET'`).Scan(&raw); err != nil {
		t.Fatalf("query: %v", err)
	}
	if raw == "plaintextvalue" {
		t.Error("value stored as plaintext on disk")
	}
}

func TestDeleteEnv(t *testing.T) {
	st := openWithBox(t)
	ctx := context.Background()
	app, err := st.CreateApp(ctx, core.App{Name: "web", RepoURL: "https://x/y.git", Domain: "web.test"})
	if err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	if err := st.SetEnv(ctx, app.ID, "K", "v", false); err != nil {
		t.Fatalf("SetEnv: %v", err)
	}

	if err := st.DeleteEnv(ctx, app.ID, "K"); err != nil {
		t.Fatalf("DeleteEnv: %v", err)
	}
	vars, _ := st.ListEnv(ctx, app.ID)
	if len(vars) != 0 {
		t.Errorf("expected 0 vars after delete, got %d", len(vars))
	}
}

func TestDeleteAppCascades(t *testing.T) {
	st := openWithBox(t)
	ctx := context.Background()
	app, _ := st.CreateApp(ctx, core.App{Name: "web", RepoURL: "https://x/y.git", Domain: "web.test"})
	st.SetEnv(ctx, app.ID, "K", "v", false)
	dep, _ := st.CreateDeployment(ctx, app.ID)

	if err := st.DeleteApp(ctx, app.ID); err != nil {
		t.Fatalf("DeleteApp: %v", err)
	}
	if _, err := st.GetApp(ctx, app.ID); err == nil {
		t.Error("app still present after delete")
	}
	vars, _ := st.ListEnv(ctx, app.ID)
	if len(vars) != 0 {
		t.Errorf("env not cascaded: %v", vars)
	}
	if _, err := st.GetDeployment(ctx, dep.ID); err == nil {
		t.Error("deployment not cascaded")
	}
}

func TestEnvWithoutBoxErrors(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "test.db"), nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	app, err := st.CreateApp(ctx, core.App{Name: "web", RepoURL: "https://x/y.git", Domain: "web.test"})
	if err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	if err := st.SetEnv(ctx, app.ID, "K", "v", false); err == nil {
		t.Error("SetEnv should error with a nil box")
	}
	if _, err := st.ListEnv(ctx, app.ID); err == nil {
		t.Error("ListEnv should error with a nil box")
	}
}
