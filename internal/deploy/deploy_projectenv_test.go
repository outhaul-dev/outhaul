package deploy

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/slipwaydev/slipway/internal/compose"
	"github.com/slipwaydev/slipway/internal/core"
)

// TestPipelineResolvesProjectEnv: an app var referencing ${{project.KEY}} gets
// the project's shared value at deploy time, and a reference to a secret
// shared variable is treated as secret — runtime only, never the build env.
func TestPipelineResolvesProjectEnv(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	app := h.app(t, "web")
	if err := h.store.SetProjectEnv(ctx, app.ProjectID, "DB_URL", "postgres://db/app", false); err != nil {
		t.Fatalf("SetProjectEnv: %v", err)
	}
	if err := h.store.SetProjectEnv(ctx, app.ProjectID, "SHARED_KEY", "s3cr3t", true); err != nil {
		t.Fatalf("SetProjectEnv: %v", err)
	}
	if err := h.store.SetEnv(ctx, app.ID, "DATABASE_URL", "${{project.DB_URL}}?sslmode=disable", false); err != nil {
		t.Fatalf("SetEnv: %v", err)
	}
	if err := h.store.SetEnv(ctx, app.ID, "API_KEY", "${{project.SHARED_KEY}}", false); err != nil {
		t.Fatalf("SetEnv: %v", err)
	}
	dep := h.claimedDeployment(t, app.ID)

	h.worker.runPipeline(ctx, dep)

	if got := h.status(t, dep.ID); got.Status != core.StatusRunning {
		t.Fatalf("status = %q (%q), want running", got.Status, got.Reason)
	}
	spec := lastCreatedNamed(t, h, "slipway-app-web")
	if !contains(spec.Env, "DATABASE_URL=postgres://db/app?sslmode=disable") {
		t.Errorf("runtime env missing resolved DATABASE_URL: %v", spec.Env)
	}
	if !contains(spec.Env, "API_KEY=s3cr3t") {
		t.Errorf("runtime env missing resolved API_KEY: %v", spec.Env)
	}
	be := h.builder.lastReq.Env
	if be["DATABASE_URL"] != "postgres://db/app?sslmode=disable" {
		t.Errorf("build env missing non-secret DATABASE_URL: %v", be)
	}
	if _, ok := be["API_KEY"]; ok {
		t.Error("value resolved from a secret shared variable leaked into the build env")
	}
	// The shared vars themselves are never injected unreferenced.
	if containsPrefix(spec.Env, "DB_URL=") || containsPrefix(spec.Env, "SHARED_KEY=") {
		t.Errorf("unreferenced project vars must not be injected: %v", spec.Env)
	}
}

// TestPipelineFailsOnUndefinedProjectRef: a typo'd reference fails the deploy
// with a reason naming the missing variable, instead of shipping the literal
// placeholder into the container.
func TestPipelineFailsOnUndefinedProjectRef(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	app := h.app(t, "web")
	if err := h.store.SetEnv(ctx, app.ID, "DATABASE_URL", "${{project.DB_URLL}}", false); err != nil {
		t.Fatalf("SetEnv: %v", err)
	}
	dep := h.claimedDeployment(t, app.ID)

	h.worker.runPipeline(ctx, dep)

	got := h.status(t, dep.ID)
	if got.Status != core.StatusFailed {
		t.Fatalf("status = %q, want failed", got.Status)
	}
	if !strings.Contains(got.Reason, "DB_URLL") || !strings.Contains(got.Reason, "DATABASE_URL") {
		t.Errorf("reason = %q, want it to name the missing variable and the referencing key", got.Reason)
	}
	if len(h.docker.Created) != 0 {
		t.Errorf("no container should be created: %v", h.docker.Created)
	}
}

// TestComposePipelineResolvesProjectEnv: the generated .env carries resolved
// values, so ${VAR} interpolation inside the user's compose file sees them.
func TestComposePipelineResolvesProjectEnv(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	app := h.composeApp(t, "shop", "docker-compose.yml", "shop.example.com")
	if err := h.store.SetProjectEnv(ctx, app.ProjectID, "DB_URL", "postgres://db/app", true); err != nil {
		t.Fatalf("SetProjectEnv: %v", err)
	}
	if err := h.store.SetEnv(ctx, app.ID, "DATABASE_URL", "${{project.DB_URL}}", false); err != nil {
		t.Fatalf("SetEnv: %v", err)
	}

	var envContent string
	h.compose.Hook = func(c compose.Call) {
		if c.Verb != "up" {
			return
		}
		if b, err := os.ReadFile(filepath.Join(c.Dir, ".env")); err == nil {
			envContent = string(b)
		}
	}

	dep := h.claimedDeployment(t, app.ID)
	h.worker.runPipeline(ctx, dep)

	if got := h.status(t, dep.ID); got.Status != core.StatusRunning {
		t.Fatalf("status = %q (%q), want running", got.Status, got.Reason)
	}
	if !strings.Contains(envContent, `DATABASE_URL="postgres://db/app"`) {
		t.Errorf(".env missing the resolved value; got:\n%s", envContent)
	}
	if strings.Contains(envContent, "${{project.") {
		t.Errorf(".env still contains an unresolved placeholder:\n%s", envContent)
	}
}
