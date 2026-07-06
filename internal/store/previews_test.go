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
