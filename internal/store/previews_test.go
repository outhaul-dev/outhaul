package store

import (
	"context"
	"testing"

	"github.com/james-smart/outhaul/internal/core"
)

func TestPreviewConfigRoundTrip(t *testing.T) {
	s := openWithBox(t)
	ctx := context.Background()
	app, _ := s.CreateApp(ctx, core.App{Name: "web", Source: core.SourceGithub, Kind: core.KindNixpacks, Branch: "main"})

	cfg, err := s.GetPreviewConfig(ctx, app.ID)
	if err != nil || cfg.Enabled {
		t.Fatalf("default cfg = %+v, err %v", cfg, err)
	}
	cfg = core.DefaultPreviewConfig(app.ID)
	cfg.Enabled = true
	cfg.BaseDomain = "preview.example.com"
	cfg.MaxConcurrent = 3
	if err := s.SetPreviewConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetPreviewConfig(ctx, app.ID)
	if !got.Enabled || got.BaseDomain != "preview.example.com" || got.MaxConcurrent != 3 {
		t.Fatalf("round trip = %+v", got)
	}
}

func TestEnvScopePersists(t *testing.T) {
	s := openWithBox(t)
	ctx := context.Background()
	app, _ := s.CreateApp(ctx, core.App{Name: "web", Source: core.SourcePublic, Kind: core.KindNixpacks, Branch: "main"})
	if err := s.SetEnvScoped(ctx, app.ID, "SECRET_KEY", "x", true, core.ScopeProd); err != nil {
		t.Fatal(err)
	}
	vars, _ := s.ListEnv(ctx, app.ID)
	if len(vars) != 1 || vars[0].Scope != core.ScopeProd {
		t.Fatalf("scope not persisted: %+v", vars)
	}
}

func TestCreatePreviewChildApp(t *testing.T) {
	s := openWithBox(t)
	ctx := context.Background()
	parent, _ := s.CreateApp(ctx, core.App{Name: "web", Source: core.SourceGithub, Kind: core.KindNixpacks, Branch: "main"})
	child, err := s.CreateApp(ctx, core.App{
		Name: core.PreviewAppName("web", 42), Source: core.SourceGithub, Kind: core.KindNixpacks,
		Branch: "feature-x", ParentID: parent.ID, PRNumber: 42, Ephemeral: true, PreviewStatus: core.PreviewBuilding,
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.GetPreviewByPR(ctx, parent.ID, 42)
	if err != nil || got.ID != child.ID || !got.Ephemeral || got.PRNumber != 42 || got.ParentID != parent.ID {
		t.Fatalf("GetPreviewByPR = %+v, err %v", got, err)
	}
	list, _ := s.ListPreviews(ctx)
	if len(list) != 1 {
		t.Fatalf("ListPreviews len = %d", len(list))
	}
}
