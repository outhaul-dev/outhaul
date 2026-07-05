package store

import (
	"context"
	"testing"

	"github.com/james-smart/outhaul/internal/core"
)

func TestTemplateAppRoundTrip(t *testing.T) {
	st := gitTestStore(t)
	ctx := context.Background()

	compose := "services:\n  web:\n    image: nginx\n"
	app, err := st.CreateApp(ctx, core.App{
		Name: "kuma", Source: core.SourceTemplate, Kind: core.KindCompose,
		ComposePath: "docker-compose.yml",
		TemplateID:  "uptime-kuma", ComposeRaw: compose,
	})
	if err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	got, err := st.GetApp(ctx, app.ID)
	if err != nil {
		t.Fatalf("GetApp: %v", err)
	}
	if got.Source != core.SourceTemplate || got.TemplateID != "uptime-kuma" {
		t.Errorf("round trip = source %q template %q, want template uptime-kuma", got.Source, got.TemplateID)
	}
	if got.ComposeRaw != compose {
		t.Errorf("ComposeRaw = %q, want the snapshot", got.ComposeRaw)
	}
	if got.RepoURL != "" {
		t.Errorf("RepoURL = %q, want empty for a template app", got.RepoURL)
	}
}
