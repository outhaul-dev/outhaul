package deploy

import (
	"context"
	"testing"

	"github.com/outhaul-dev/outhaul/internal/core"
)

// Dockerfile apps run the exact same single-container pipeline as nixpacks
// apps; only the build strategy differs. These tests pin the builder
// selection — everything downstream (cutover, health gating, rollback,
// pruning) is covered by the shared pipeline tests.

func TestDockerfileAppBuildsWithDockerfileBuilder(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	app, err := h.store.CreateApp(ctx, core.App{
		Name: "api", RepoURL: "https://example.com/api.git", Domain: "api.test",
		Kind: core.KindDockerfile, DockerfilePath: "build/Dockerfile.prod",
	})
	if err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	dep := h.claimedDeployment(t, app.ID)

	h.worker.runPipeline(ctx, dep)

	got := h.status(t, dep.ID)
	if got.Status != core.StatusRunning {
		t.Fatalf("status = %q (reason %q), want running", got.Status, got.Reason)
	}
	if h.dockerfile.lastReq.Dockerfile != "build/Dockerfile.prod" {
		t.Errorf("dockerfile builder got Dockerfile = %q, want the app's configured path",
			h.dockerfile.lastReq.Dockerfile)
	}
	if h.dockerfile.lastReq.ImageTag == "" {
		t.Error("dockerfile builder was not invoked")
	}
	if h.builder.lastReq.ImageTag != "" {
		t.Errorf("nixpacks builder must not run for a dockerfile app, built %q", h.builder.lastReq.ImageTag)
	}
	if c, _ := h.docker.FindContainer(ctx, "outhaul-app-api"); c == nil || !c.Running() {
		t.Fatalf("app container not running: %+v", c)
	}
}

func TestNixpacksAppLeavesDockerfileBuilderIdle(t *testing.T) {
	h := newHarness(t)
	app := h.app(t, "web")
	dep := h.claimedDeployment(t, app.ID)

	h.worker.runPipeline(context.Background(), dep)

	if h.builder.lastReq.ImageTag == "" {
		t.Error("nixpacks builder was not invoked")
	}
	if h.dockerfile.lastReq.ImageTag != "" {
		t.Errorf("dockerfile builder must not run for a nixpacks app, built %q", h.dockerfile.lastReq.ImageTag)
	}
}
