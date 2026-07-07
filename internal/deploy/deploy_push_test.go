package deploy

import (
	"context"
	"testing"

	"github.com/outhaul-dev/outhaul/internal/config"
	"github.com/outhaul-dev/outhaul/internal/core"
)

func TestCloneSpecForPushApp(t *testing.T) {
	w := &Worker{cfg: config.Config{DataDir: "/data"}}
	app := core.App{Name: "api", Source: core.SourcePush, Branch: "main"}

	spec, err := w.cloneSpec(context.Background(), app)
	if err != nil {
		t.Fatalf("cloneSpec: %v", err)
	}
	if want := "/data/git/api.git"; spec.URL != want {
		t.Fatalf("spec.URL = %q; want %q", spec.URL, want)
	}
	if spec.Branch != "main" {
		t.Fatalf("spec.Branch = %q; want main", spec.Branch)
	}
	if spec.Auth.Kind != AuthNone {
		t.Fatalf("spec.Auth.Kind = %v; want AuthNone", spec.Auth.Kind)
	}
}
