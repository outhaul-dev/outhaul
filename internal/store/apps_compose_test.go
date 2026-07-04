package store

import (
	"context"
	"testing"

	"github.com/slipwaydev/slipway/internal/core"
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
		Name: "stack", RepoURL: "https://example.com/r.git", Domain: "stack.example.com",
		Kind: core.KindCompose, ComposePath: "deploy/docker-compose.yml",
		ComposeService: "web", ComposePort: 3000,
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
	if got.Kind != core.KindCompose || got.ComposePath != "deploy/docker-compose.yml" ||
		got.ComposeService != "web" || got.ComposePort != 3000 {
		t.Errorf("compose fields not round-tripped: %+v", got)
	}
	if len(got.WatchPaths) != 2 || got.WatchPaths[0] != "deploy/**" {
		t.Errorf("watch paths not round-tripped: %v", got.WatchPaths)
	}
}

func TestUpdateAppCompose(t *testing.T) {
	st := gitTestStore(t)
	ctx := context.Background()

	app, err := st.CreateApp(ctx, core.App{
		Name: "stack", RepoURL: "https://example.com/r.git",
		Kind: core.KindCompose, ComposePath: "docker-compose.yml",
	})
	if err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	if err := st.UpdateAppCompose(ctx, app.ID, "app.example.com", "compose/prod.yml", "frontend", 8081); err != nil {
		t.Fatalf("UpdateAppCompose: %v", err)
	}
	got, _ := st.GetApp(ctx, app.ID)
	if got.Domain != "app.example.com" || got.ComposePath != "compose/prod.yml" ||
		got.ComposeService != "frontend" || got.ComposePort != 8081 {
		t.Errorf("compose settings not updated: %+v", got)
	}
}
