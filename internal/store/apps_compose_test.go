package store

import (
	"context"
	"testing"

	"github.com/james-smart/outhaul/internal/core"
)

// TestCreateAppDefaultsToNixpacksKind: apps that never mention a kind (all
// pre-compose callers) are nixpacks, matching the migration's backfill default.
func TestCreateAppDefaultsToNixpacksKind(t *testing.T) {
	st := gitTestStore(t)
	ctx := context.Background()

	app, err := st.CreateApp(ctx, core.App{
		Name: "web", RepoURL: "https://example.com/r.git", Domain: "web.example.com",
	})
	if err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	got, err := st.GetApp(ctx, app.ID)
	if err != nil {
		t.Fatalf("GetApp: %v", err)
	}
	if got.Kind != core.KindNixpacks {
		t.Errorf("Kind = %q, want %q", got.Kind, core.KindNixpacks)
	}
}

func TestCreateAppPersistsComposeFields(t *testing.T) {
	st := gitTestStore(t)
	ctx := context.Background()

	in := core.App{
		Name: "stack", RepoURL: "https://example.com/r.git",
		Kind: core.KindCompose, ComposePath: "deploy/docker-compose.yml",
		WatchPaths: []string{"deploy/**", "src/**"},
	}
	created, err := st.CreateApp(ctx, in)
	if err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	got, err := st.GetApp(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetApp: %v", err)
	}
	if got.Kind != core.KindCompose || got.ComposePath != "deploy/docker-compose.yml" {
		t.Errorf("compose fields not round-tripped: %+v", got)
	}
	if len(got.WatchPaths) != 2 || got.WatchPaths[0] != "deploy/**" {
		t.Errorf("watch paths not round-tripped: %v", got.WatchPaths)
	}
}

func TestUpdateAppComposePath(t *testing.T) {
	st := gitTestStore(t)
	ctx := context.Background()

	app, err := st.CreateApp(ctx, core.App{
		Name: "stack", RepoURL: "https://example.com/r.git",
		Kind: core.KindCompose, ComposePath: "docker-compose.yml",
	})
	if err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	if err := st.UpdateAppComposePath(ctx, app.ID, "compose/prod.yml"); err != nil {
		t.Fatalf("UpdateAppComposePath: %v", err)
	}
	got, _ := st.GetApp(ctx, app.ID)
	if got.ComposePath != "compose/prod.yml" {
		t.Errorf("ComposePath = %q, want compose/prod.yml", got.ComposePath)
	}
}

func TestComposeDomainsCRUD(t *testing.T) {
	st := gitTestStore(t)
	ctx := context.Background()

	app, err := st.CreateApp(ctx, core.App{
		Name: "stack", RepoURL: "https://example.com/r.git", Kind: core.KindCompose,
	})
	if err != nil {
		t.Fatalf("CreateApp: %v", err)
	}

	web, err := st.AddDomain(ctx, core.Domain{
		AppID: app.ID, Host: "shop.example.com", Service: "web", Port: 3000,
	})
	if err != nil {
		t.Fatalf("AddDomain: %v", err)
	}
	if web.ID == 0 {
		t.Error("AddDomain must return the row ID")
	}
	if _, err := st.AddDomain(ctx, core.Domain{
		AppID: app.ID, Host: "api.example.com", Service: "api", Port: 8080,
	}); err != nil {
		t.Fatalf("AddDomain second: %v", err)
	}

	// Duplicate domain on the same app violates UNIQUE(app_id, host, path).
	if _, err := st.AddDomain(ctx, core.Domain{
		AppID: app.ID, Host: "shop.example.com", Service: "other", Port: 1,
	}); err == nil {
		t.Error("duplicate domain on one app must be rejected")
	}

	domains, err := st.ListDomains(ctx, app.ID)
	if err != nil {
		t.Fatalf("ListDomains: %v", err)
	}
	if len(domains) != 2 || domains[0].Host != "api.example.com" || domains[1].Host != "shop.example.com" {
		t.Fatalf("domains = %+v, want two ordered by domain", domains)
	}
	if domains[1].Service != "web" || domains[1].Port != 3000 || domains[1].AppID != app.ID {
		t.Errorf("domain fields not round-tripped: %+v", domains[1])
	}

	// Deleting with the wrong app scope must not remove the row.
	if err := st.DeleteDomain(ctx, app.ID+999, web.ID); err != nil {
		t.Fatalf("DeleteDomain (wrong app): %v", err)
	}
	if ds, _ := st.ListDomains(ctx, app.ID); len(ds) != 2 {
		t.Fatal("wrong-app delete must be a no-op")
	}
	if err := st.DeleteDomain(ctx, app.ID, web.ID); err != nil {
		t.Fatalf("DeleteDomain: %v", err)
	}
	if ds, _ := st.ListDomains(ctx, app.ID); len(ds) != 1 || ds[0].Host != "api.example.com" {
		t.Errorf("after delete domains = %+v, want just api.example.com", ds)
	}
}

func TestDeleteAppRemovesComposeDomains(t *testing.T) {
	st := gitTestStore(t)
	ctx := context.Background()

	app, err := st.CreateApp(ctx, core.App{
		Name: "stack", RepoURL: "https://example.com/r.git", Kind: core.KindCompose,
	})
	if err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	if _, err := st.AddDomain(ctx, core.Domain{
		AppID: app.ID, Host: "shop.example.com", Service: "web", Port: 3000,
	}); err != nil {
		t.Fatalf("AddDomain: %v", err)
	}
	if err := st.DeleteApp(ctx, app.ID); err != nil {
		t.Fatalf("DeleteApp: %v", err)
	}
	if ds, _ := st.ListDomains(ctx, app.ID); len(ds) != 0 {
		t.Errorf("domains must not survive their app: %+v", ds)
	}
}
