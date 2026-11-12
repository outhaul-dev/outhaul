package store

import (
	"context"
	"testing"

	"github.com/outhaul-dev/outhaul/internal/core"
)

// An app that existed before the columns does not gain a limit: the migration
// adds zeros, which mean "use the server default".
func TestAppLimitsMigrationAddsZeroColumns(t *testing.T) {
	db := openMigratedBefore(t, "0032")
	if _, err := db.Exec(`INSERT INTO apps (id, name, repo_url, domain, created_at)
	                      VALUES (7, 'web', 'https://example.com/web.git', '', '2026-11-11T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if err := migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	var cpus, mem, pids int64
	if err := db.QueryRow(`SELECT limit_cpus_milli, limit_memory_bytes, limit_pids FROM apps WHERE id = 7`).
		Scan(&cpus, &mem, &pids); err != nil {
		t.Fatalf("read limits after migration: %v", err)
	}
	if cpus != 0 || mem != 0 || pids != 0 {
		t.Errorf("migrated limits = (%d, %d, %d), want zeros", cpus, mem, pids)
	}
}

func TestAppLimitsRoundTrip(t *testing.T) {
	s := openWithBox(t)
	ctx := context.Background()
	app, err := s.CreateApp(ctx, core.App{Name: "shop", RepoURL: "https://example.com/shop.git"})
	if err != nil {
		t.Fatal(err)
	}
	want := core.Limits{MilliCPUs: 500, Memory: 1 << 30, Pids: 512}
	if err := s.SetAppLimits(ctx, app.ID, want); err != nil {
		t.Fatalf("SetAppLimits: %v", err)
	}
	got, err := s.GetApp(ctx, app.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Limits != want {
		t.Errorf("Limits = %+v, want %+v", got.Limits, want)
	}

	if err := s.SetAppLimits(ctx, app.ID, core.Limits{}); err != nil {
		t.Fatalf("SetAppLimits zero: %v", err)
	}
	if got, _ := s.GetApp(ctx, app.ID); got.Limits != (core.Limits{}) {
		t.Errorf("cleared Limits = %+v, want zero", got.Limits)
	}
}

// The preview path copies the parent struct (child := parent). The INSERT does
// not carry the limit columns and CreateApp clears the copy, so neither the
// returned child nor the stored row keeps the parent's limits.
func TestNewAppsStartWithoutLimits(t *testing.T) {
	s := openWithBox(t)
	ctx := context.Background()
	app, err := s.CreateApp(ctx, core.App{
		Name: "web-pr-1", RepoURL: "https://example.com/web.git",
		ParentID: 1, PRNumber: 1, Ephemeral: true,
		Limits: core.Limits{MilliCPUs: 1000, Memory: 1 << 30, Pids: 512},
	})
	if err != nil {
		t.Fatal(err)
	}
	if app.Limits != (core.Limits{}) {
		t.Errorf("returned child limits = %+v, want zero", app.Limits)
	}
	got, err := s.GetApp(ctx, app.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Limits != (core.Limits{}) {
		t.Errorf("stored child limits = %+v, want zero", got.Limits)
	}
}
