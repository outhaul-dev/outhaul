package deploy

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/slipwaydev/slipway/internal/compose"
	"github.com/slipwaydev/slipway/internal/core"
)

// composeApp creates a compose-kind app whose fake clone contains a compose
// file at the given path.
func (h *harness) composeApp(t *testing.T, name, composePath, domain string) core.App {
	t.Helper()
	h.cloner.files = map[string]string{composePath: "services:\n  web:\n    image: nginx\n"}
	app, err := h.store.CreateApp(context.Background(), core.App{
		Name: name, RepoURL: "https://example.com/" + name + ".git", Domain: domain,
		Kind: core.KindCompose, ComposePath: composePath,
		ComposeService: "web", ComposePort: 3000,
	})
	if err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	return app
}

func TestComposePipelineHappyPath(t *testing.T) {
	h := newHarness(t)
	app := h.composeApp(t, "shop", "docker-compose.yml", "shop.example.com")

	// Capture generated files while the work dir still exists.
	var envContent, overrideContent string
	h.compose.Hook = func(c compose.Call) {
		if c.Verb != "up" {
			return
		}
		if b, err := os.ReadFile(filepath.Join(c.Dir, ".env")); err == nil {
			envContent = string(b)
		}
		if b, err := os.ReadFile(filepath.Join(c.Dir, compose.OverrideFile)); err == nil {
			overrideContent = string(b)
		}
	}
	if err := h.store.SetEnv(context.Background(), app.ID, "API_KEY", "s3cr3t", true); err != nil {
		t.Fatal(err)
	}

	dep := h.claimedDeployment(t, app.ID)
	h.worker.runPipeline(context.Background(), dep)

	got := h.status(t, dep.ID)
	if got.Status != core.StatusRunning {
		t.Fatalf("status = %q (reason %q), want running", got.Status, got.Reason)
	}

	builds, ups := h.compose.CallsFor("build"), h.compose.CallsFor("up")
	if len(builds) != 1 || len(ups) != 1 {
		t.Fatalf("compose calls: build=%d up=%d, want 1 each", len(builds), len(ups))
	}
	if ups[0].Project != "slipway-shop" {
		t.Errorf("project = %q, want slipway-shop", ups[0].Project)
	}
	wantFiles := []string{"docker-compose.yml", compose.OverrideFile}
	if len(ups[0].Files) != 2 || ups[0].Files[0] != wantFiles[0] || ups[0].Files[1] != wantFiles[1] {
		t.Errorf("up files = %v, want %v", ups[0].Files, wantFiles)
	}
	if !strings.Contains(envContent, `API_KEY="s3cr3t"`) {
		t.Errorf(".env missing the secret var; got:\n%s", envContent)
	}
	if !strings.Contains(overrideContent, "Host(`shop.example.com`)") {
		t.Errorf("override missing the domain rule; got:\n%s", overrideContent)
	}
	// The compose path never touches single-container machinery.
	if len(h.docker.Created) != 0 {
		t.Errorf("no containers should be created directly: %v", h.docker.Created)
	}
	if h.builder.lastReq.ImageTag != "" {
		t.Error("nixpacks builder must not run for a compose app")
	}
}

func TestComposePipelineNestedPathAndNoDomain(t *testing.T) {
	h := newHarness(t)
	app := h.composeApp(t, "shop", "deploy/compose.yml", "")

	var hadOverride bool
	var envPath string
	h.compose.Hook = func(c compose.Call) {
		if c.Verb != "up" {
			return
		}
		_, err := os.Stat(filepath.Join(c.Dir, "deploy", compose.OverrideFile))
		hadOverride = err == nil
		if _, err := os.Stat(filepath.Join(c.Dir, "deploy", ".env")); err == nil {
			envPath = "deploy/.env"
		}
	}

	dep := h.claimedDeployment(t, app.ID)
	h.worker.runPipeline(context.Background(), dep)

	if got := h.status(t, dep.ID); got.Status != core.StatusRunning {
		t.Fatalf("status = %q (%q), want running", got.Status, got.Reason)
	}
	ups := h.compose.CallsFor("up")
	if len(ups) != 1 || len(ups[0].Files) != 1 || ups[0].Files[0] != "deploy/compose.yml" {
		t.Fatalf("up files = %+v, want just deploy/compose.yml (no domain, no override)", ups)
	}
	if hadOverride {
		t.Error("override must not be written when the app has no domain")
	}
	if envPath != "deploy/.env" {
		t.Error(".env must be written next to the compose file, not the repo root")
	}
}

func TestComposePipelineMissingFileFails(t *testing.T) {
	h := newHarness(t)
	app := h.composeApp(t, "shop", "docker-compose.yml", "")
	h.cloner.files = map[string]string{"README.md": "no compose here"}

	dep := h.claimedDeployment(t, app.ID)
	h.worker.runPipeline(context.Background(), dep)

	got := h.status(t, dep.ID)
	if got.Status != core.StatusFailed {
		t.Fatalf("status = %q, want failed", got.Status)
	}
	if !strings.Contains(got.Reason, "docker-compose.yml") || !strings.Contains(got.Reason, "not found") {
		t.Errorf("reason = %q, want it to name the missing compose file", got.Reason)
	}
	if len(h.compose.Calls) != 0 {
		t.Errorf("no compose command should run: %v", h.compose.Calls)
	}
}

func TestComposePipelineBuildFailureMarksFailed(t *testing.T) {
	h := newHarness(t)
	app := h.composeApp(t, "shop", "docker-compose.yml", "")
	h.compose.FailBuild = errors.New("service web has neither an image nor a build context")

	dep := h.claimedDeployment(t, app.ID)
	h.worker.runPipeline(context.Background(), dep)

	got := h.status(t, dep.ID)
	if got.Status != core.StatusFailed || !strings.Contains(got.Reason, "build failed") {
		t.Fatalf("status = %q (%q), want failed with build reason", got.Status, got.Reason)
	}
	if len(h.compose.CallsFor("up")) != 0 {
		t.Error("up must not run after a failed build")
	}
}

func TestComposePipelineUpFailureMarksFailed(t *testing.T) {
	h := newHarness(t)
	app := h.composeApp(t, "shop", "docker-compose.yml", "")
	h.compose.FailUp = errors.New("container slipway-shop-web-1 is unhealthy")

	dep := h.claimedDeployment(t, app.ID)
	h.worker.runPipeline(context.Background(), dep)

	got := h.status(t, dep.ID)
	if got.Status != core.StatusFailed {
		t.Fatalf("status = %q, want failed", got.Status)
	}
	if !strings.Contains(got.Reason, "stack failed to become ready") {
		t.Errorf("reason = %q", got.Reason)
	}
}

func TestComposePipelineCancelDuringBuildAborts(t *testing.T) {
	h := newHarness(t)
	app := h.composeApp(t, "shop", "docker-compose.yml", "")
	dep := h.claimedDeployment(t, app.ID)
	// Operator cancels while `compose build` runs: the row flips to cancelled,
	// so the guarded building->deploying transition must abort the pipeline.
	h.compose.Hook = func(c compose.Call) {
		if c.Verb == "build" {
			if _, err := h.store.SetStatus(context.Background(), dep.ID, core.StatusBuilding, core.StatusCancelled, "cancelled by operator"); err != nil {
				t.Errorf("cancel: %v", err)
			}
		}
	}

	h.worker.runPipeline(context.Background(), dep)

	if got := h.status(t, dep.ID); got.Status != core.StatusCancelled {
		t.Fatalf("status = %q, want cancelled to survive", got.Status)
	}
	if len(h.compose.CallsFor("up")) != 0 {
		t.Error("up must not run after a cancel")
	}
}

func TestComposePipelineAbortsIfAppDeletedDuringBuild(t *testing.T) {
	h := newHarness(t)
	app := h.composeApp(t, "shop", "docker-compose.yml", "")
	dep := h.claimedDeployment(t, app.ID)
	h.compose.Hook = func(c compose.Call) {
		if c.Verb == "build" {
			_ = h.store.DeleteApp(context.Background(), app.ID)
		}
	}

	h.worker.runPipeline(context.Background(), dep)

	if len(h.compose.CallsFor("up")) != 0 {
		t.Error("up must not run for a deleted app")
	}
}
