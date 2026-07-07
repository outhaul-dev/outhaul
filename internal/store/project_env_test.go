package store

import (
	"context"
	"testing"

	"github.com/outhaul-dev/outhaul/internal/core"
)

func TestProjectEnvCRUD(t *testing.T) {
	st := openWithBox(t)
	ctx := context.Background()
	p, err := st.CreateProject(ctx, core.Project{Name: "acme"})
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	if err := st.SetProjectEnv(ctx, p.ID, "DB_URL", "postgres://u:p@h/db", true); err != nil {
		t.Fatalf("SetProjectEnv: %v", err)
	}
	if err := st.SetProjectEnv(ctx, p.ID, "REGION", "eu-west-1", false); err != nil {
		t.Fatalf("SetProjectEnv: %v", err)
	}
	// Upsert: same key updates value and secret flag in place.
	if err := st.SetProjectEnv(ctx, p.ID, "REGION", "us-east-1", true); err != nil {
		t.Fatalf("SetProjectEnv upsert: %v", err)
	}

	vars, err := st.ListProjectEnv(ctx, p.ID)
	if err != nil {
		t.Fatalf("ListProjectEnv: %v", err)
	}
	if len(vars) != 2 || vars[0].Key != "DB_URL" || vars[1].Key != "REGION" {
		t.Fatalf("vars = %+v, want DB_URL then REGION", vars)
	}
	if vars[0].Value != "postgres://u:p@h/db" || !vars[0].IsSecret {
		t.Errorf("DB_URL round trip wrong: %+v", vars[0])
	}
	if vars[1].Value != "us-east-1" || !vars[1].IsSecret {
		t.Errorf("REGION upsert wrong: %+v", vars[1])
	}

	if err := st.DeleteProjectEnv(ctx, p.ID, "DB_URL"); err != nil {
		t.Fatalf("DeleteProjectEnv: %v", err)
	}
	vars, _ = st.ListProjectEnv(ctx, p.ID)
	if len(vars) != 1 || vars[0].Key != "REGION" {
		t.Errorf("after delete vars = %+v, want just REGION", vars)
	}
}

func TestProjectEnvScopedToProject(t *testing.T) {
	st := openWithBox(t)
	ctx := context.Background()
	a, _ := st.CreateProject(ctx, core.Project{Name: "a"})
	b, _ := st.CreateProject(ctx, core.Project{Name: "b"})
	if err := st.SetProjectEnv(ctx, a.ID, "K", "va", false); err != nil {
		t.Fatal(err)
	}
	if err := st.SetProjectEnv(ctx, b.ID, "K", "vb", false); err != nil {
		t.Fatal(err)
	}

	vars, err := st.ListProjectEnv(ctx, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(vars) != 1 || vars[0].Value != "vb" {
		t.Errorf("project b vars = %+v, want only its own K=vb", vars)
	}
}

func TestProjectEnvStoredAsCiphertext(t *testing.T) {
	st := openWithBox(t)
	ctx := context.Background()
	p, _ := st.CreateProject(ctx, core.Project{Name: "acme"})
	if err := st.SetProjectEnv(ctx, p.ID, "SECRET", "plaintextvalue", true); err != nil {
		t.Fatalf("SetProjectEnv: %v", err)
	}

	var raw string
	if err := st.db.QueryRowContext(ctx, `SELECT value FROM project_env WHERE key = 'SECRET'`).Scan(&raw); err != nil {
		t.Fatalf("query: %v", err)
	}
	if raw == "plaintextvalue" {
		t.Error("value stored as plaintext on disk")
	}
}

func TestDeleteProjectRemovesEnv(t *testing.T) {
	st := openWithBox(t)
	ctx := context.Background()
	p, _ := st.CreateProject(ctx, core.Project{Name: "acme"})
	if err := st.SetProjectEnv(ctx, p.ID, "K", "v", false); err != nil {
		t.Fatal(err)
	}

	if err := st.DeleteProject(ctx, p.ID); err != nil {
		t.Fatalf("DeleteProject: %v", err)
	}
	var n int
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM project_env WHERE project_id = ?`, p.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("project_env rows = %d after project delete, want 0", n)
	}
}
