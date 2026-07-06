package deploy

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/james-smart/outhaul/internal/compose"
	"github.com/james-smart/outhaul/internal/core"
)

// Template apps carry their compose file with them (ComposeRaw); the pipeline
// must deploy the snapshot without ever touching the cloner.
func TestTemplateAppDeploysWithoutCloning(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	raw := "services:\n  kuma:\n    image: louislam/uptime-kuma:2.1.0\n"
	app, err := h.store.CreateApp(ctx, core.App{
		Name: "kuma", Source: core.SourceTemplate, Kind: core.KindCompose,
		ComposePath: "docker-compose.yml",
		TemplateID:  "uptime-kuma", ComposeRaw: raw,
	})
	if err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	if _, err := h.store.AddDomain(ctx, core.Domain{
		AppID: app.ID, Host: "kuma-abc123.sslip.io", Service: "kuma", Port: 3001,
	}); err != nil {
		t.Fatal(err)
	}

	// Capture the work dir's compose file while it still exists.
	var written, override string
	h.compose.Hook = func(c compose.Call) {
		if c.Verb != "up" {
			return
		}
		if b, err := os.ReadFile(filepath.Join(c.Dir, "docker-compose.yml")); err == nil {
			written = string(b)
		}
		if b, err := os.ReadFile(filepath.Join(c.Dir, compose.OverrideFile)); err == nil {
			override = string(b)
		}
	}

	dep := h.claimedDeployment(t, app.ID)
	h.worker.runPipeline(ctx, dep)

	got := h.status(t, dep.ID)
	if got.Status != core.StatusRunning {
		t.Fatalf("status = %q (reason %q), want running", got.Status, got.Reason)
	}
	if len(h.cloner.cloned) != 0 {
		t.Errorf("template app must not clone; cloned %v", h.cloner.cloned)
	}
	if written != raw {
		t.Errorf("work dir compose file = %q, want the app's snapshot", written)
	}
	if !strings.Contains(override, "Host(`kuma-abc123.sslip.io`)") {
		t.Errorf("override missing the generated domain; got:\n%s", override)
	}
	if ups := h.compose.CallsFor("up"); len(ups) != 1 || ups[0].Project != "outhaul-kuma" {
		t.Errorf("compose up calls = %v, want one for outhaul-kuma", ups)
	}
}

// A repo-backed compose app must still clone — the snapshot branch is gated
// on ComposeRaw, not on the compose kind.
func TestRepoComposeAppStillClones(t *testing.T) {
	h := newHarness(t)
	app := h.composeApp(t, "shop", "docker-compose.yml", "")

	dep := h.claimedDeployment(t, app.ID)
	h.worker.runPipeline(context.Background(), dep)

	if got := h.status(t, dep.ID); got.Status != core.StatusRunning {
		t.Fatalf("status = %q (reason %q), want running", got.Status, got.Reason)
	}
	if len(h.cloner.cloned) != 1 {
		t.Errorf("repo compose app should clone once; cloned %v", h.cloner.cloned)
	}
}
