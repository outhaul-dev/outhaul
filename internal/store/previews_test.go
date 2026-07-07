package store

import (
	"context"
	"testing"

	"github.com/outhaul-dev/outhaul/internal/core"
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

func TestSetEnvPreservesScope(t *testing.T) {
	s := openWithBox(t)
	ctx := context.Background()
	app, _ := s.CreateApp(ctx, core.App{Name: "web", Source: core.SourcePublic, Kind: core.KindNixpacks, Branch: "main"})
	if err := s.SetEnvScoped(ctx, app.ID, "K", "v", false, core.ScopeProd); err != nil {
		t.Fatal(err)
	}
	if err := s.SetEnv(ctx, app.ID, "K", "v2", false); err != nil {
		t.Fatal(err)
	}
	vars, _ := s.ListEnv(ctx, app.ID)
	if len(vars) != 1 || vars[0].Value != "v2" || vars[0].Scope != core.ScopeProd {
		t.Fatalf("SetEnv clobbered scope or value: %+v", vars)
	}
}

func TestListPreviewsForParent(t *testing.T) {
	s := openWithBox(t)
	ctx := context.Background()
	parent, _ := s.CreateApp(ctx, core.App{Name: "web", Source: core.SourceGithub, Kind: core.KindNixpacks, Branch: "main"})
	if _, err := s.CreateApp(ctx, core.App{
		Name: core.PreviewAppName("web", 7), Source: core.SourceGithub, Kind: core.KindNixpacks,
		Branch: "b7", ParentID: parent.ID, PRNumber: 7, Ephemeral: true, PreviewStatus: core.PreviewBuilding,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateApp(ctx, core.App{
		Name: core.PreviewAppName("web", 3), Source: core.SourceGithub, Kind: core.KindNixpacks,
		Branch: "b3", ParentID: parent.ID, PRNumber: 3, Ephemeral: true, PreviewStatus: core.PreviewBuilding,
	}); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListPreviewsForParent(ctx, parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].PRNumber != 3 || list[1].PRNumber != 7 {
		t.Fatalf("ListPreviewsForParent = %+v", list)
	}
}

func TestSetPreviewStatus(t *testing.T) {
	s := openWithBox(t)
	ctx := context.Background()
	parent, _ := s.CreateApp(ctx, core.App{Name: "web", Source: core.SourceGithub, Kind: core.KindNixpacks, Branch: "main"})
	child, err := s.CreateApp(ctx, core.App{
		Name: core.PreviewAppName("web", 9), Source: core.SourceGithub, Kind: core.KindNixpacks,
		Branch: "b9", ParentID: parent.ID, PRNumber: 9, Ephemeral: true, PreviewStatus: core.PreviewBuilding,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetPreviewStatus(ctx, child.ID, core.PreviewReady); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetPreviewByPR(ctx, parent.ID, 9)
	if err != nil || got.PreviewStatus != core.PreviewReady {
		t.Fatalf("SetPreviewStatus: got %+v, err %v", got, err)
	}
}
