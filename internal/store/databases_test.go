package store

import (
	"context"
	"errors"
	"testing"

	"github.com/james-smart/outhaul/internal/core"
)

func mustDatabase(t *testing.T, s *Store, name string) core.Database {
	t.Helper()
	d, err := s.CreateDatabase(context.Background(), core.Database{
		Name:     name,
		Engine:   core.EnginePostgres,
		Image:    "postgres:17",
		Username: name,
		Password: "s3cret-" + name,
		DBName:   name,
	})
	if err != nil {
		t.Fatalf("CreateDatabase: %v", err)
	}
	return d
}

func TestCreateDatabaseRoundTrip(t *testing.T) {
	s := openWithBox(t)
	ctx := context.Background()

	d, err := s.CreateDatabase(ctx, core.Database{
		Name: "shop", Engine: core.EnginePostgres, Image: "postgres:17",
		Username: "shop", Password: "pw-plain", DBName: "shop", ExtPort: 5433,
	})
	if err != nil {
		t.Fatalf("CreateDatabase: %v", err)
	}
	if d.Status != core.DBCreating {
		t.Errorf("status = %q, want creating", d.Status)
	}
	if d.ProjectID != core.DefaultProjectID {
		t.Errorf("project = %d, want default (%d)", d.ProjectID, core.DefaultProjectID)
	}

	got, err := s.GetDatabase(ctx, d.ID)
	if err != nil {
		t.Fatalf("GetDatabase: %v", err)
	}
	if got.Password != "pw-plain" {
		t.Errorf("password did not survive the encrypt/decrypt round-trip: %q", got.Password)
	}
	if got.Engine != core.EnginePostgres || got.Image != "postgres:17" ||
		got.Username != "shop" || got.DBName != "shop" || got.ExtPort != 5433 {
		t.Errorf("round-trip mismatch: %+v", got)
	}
	if got.CreatedAt.IsZero() {
		t.Error("CreatedAt not set")
	}
}

func TestDatabasePasswordEncryptedAtRest(t *testing.T) {
	s := openWithBox(t)
	d := mustDatabase(t, s, "shop")

	var raw string
	if err := s.db.QueryRow(`SELECT password FROM databases WHERE id = ?`, d.ID).Scan(&raw); err != nil {
		t.Fatalf("read raw password: %v", err)
	}
	if raw == d.Password {
		t.Fatal("password stored in plaintext")
	}
}

func TestDatabaseNameUnique(t *testing.T) {
	s := openWithBox(t)
	mustDatabase(t, s, "shop")
	if _, err := s.CreateDatabase(context.Background(), core.Database{
		Name: "shop", Engine: core.EngineRedis, Image: "redis:7", Password: "x",
	}); err == nil {
		t.Fatal("duplicate database name accepted")
	}
}

func TestListDatabasesByProject(t *testing.T) {
	s := openWithBox(t)
	ctx := context.Background()
	p, err := s.CreateProject(ctx, core.Project{Name: "client"})
	if err != nil {
		t.Fatal(err)
	}
	mustDatabase(t, s, "in-default")
	if _, err := s.CreateDatabase(ctx, core.Database{
		ProjectID: p.ID, Name: "in-client", Engine: core.EngineRedis, Image: "redis:7", Password: "x",
	}); err != nil {
		t.Fatal(err)
	}

	dbs, err := s.ListDatabasesByProject(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(dbs) != 1 || dbs[0].Name != "in-client" {
		t.Errorf("ListDatabasesByProject = %+v, want just in-client", dbs)
	}
}

func TestSetDatabaseStatusAndExtPort(t *testing.T) {
	s := openWithBox(t)
	ctx := context.Background()
	d := mustDatabase(t, s, "shop")

	if err := s.SetDatabaseStatus(ctx, d.ID, core.DBFailed, "pull timed out"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetDatabaseExtPort(ctx, d.ID, 5433); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetDatabase(ctx, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != core.DBFailed || got.Reason != "pull timed out" || got.ExtPort != 5433 {
		t.Errorf("got status %q reason %q port %d", got.Status, got.Reason, got.ExtPort)
	}
}

func TestRecoverCreatingDatabases(t *testing.T) {
	s := openWithBox(t)
	ctx := context.Background()
	stuck := mustDatabase(t, s, "stuck")
	fine := mustDatabase(t, s, "fine")
	if err := s.SetDatabaseStatus(ctx, fine.ID, core.DBRunning, ""); err != nil {
		t.Fatal(err)
	}

	n, err := s.RecoverCreatingDatabases(ctx, "interrupted by restart")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("recovered %d rows, want 1", n)
	}
	if got, _ := s.GetDatabase(ctx, stuck.ID); got.Status != core.DBFailed || got.Reason != "interrupted by restart" {
		t.Errorf("stuck row = %q/%q, want failed/interrupted", got.Status, got.Reason)
	}
	if got, _ := s.GetDatabase(ctx, fine.ID); got.Status != core.DBRunning {
		t.Errorf("running row was touched: %q", got.Status)
	}
}

func TestDeleteProjectBlockedByDatabases(t *testing.T) {
	s := openWithBox(t)
	ctx := context.Background()
	p, err := s.CreateProject(ctx, core.Project{Name: "client"})
	if err != nil {
		t.Fatal(err)
	}
	d, err := s.CreateDatabase(ctx, core.Database{
		ProjectID: p.ID, Name: "shop", Engine: core.EnginePostgres, Image: "postgres:17", Password: "x",
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := s.DeleteProject(ctx, p.ID); !errors.Is(err, ErrProjectNotEmpty) {
		t.Fatalf("DeleteProject with a database = %v, want ErrProjectNotEmpty", err)
	}
	if err := s.DeleteDatabase(ctx, d.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteProject(ctx, p.ID); err != nil {
		t.Fatalf("DeleteProject after removing the database: %v", err)
	}
}
