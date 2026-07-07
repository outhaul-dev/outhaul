package deploy

import (
	"context"
	"testing"

	"github.com/outhaul-dev/outhaul/internal/core"
)

// TestPipelineFiltersProdEnvByScope: a normal (non-ephemeral) app's deploy
// carries shared and prod-scoped vars into the container's runtime env, but
// never a preview-scoped var — that would leak a preview-only setting (e.g. a
// staging API key) into production.
func TestPipelineFiltersProdEnvByScope(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	app := h.app(t, "web")

	if err := h.store.SetEnvScoped(ctx, app.ID, "SHARED_VAR", "shared-val", false, core.ScopeShared); err != nil {
		t.Fatalf("SetEnvScoped SHARED_VAR: %v", err)
	}
	if err := h.store.SetEnvScoped(ctx, app.ID, "PROD_VAR", "prod-val", false, core.ScopeProd); err != nil {
		t.Fatalf("SetEnvScoped PROD_VAR: %v", err)
	}
	if err := h.store.SetEnvScoped(ctx, app.ID, "PREVIEW_VAR", "preview-val", false, core.ScopePreview); err != nil {
		t.Fatalf("SetEnvScoped PREVIEW_VAR: %v", err)
	}

	dep := h.claimedDeployment(t, app.ID)
	h.worker.runPipeline(ctx, dep)

	if got := h.status(t, dep.ID); got.Status != core.StatusRunning {
		t.Fatalf("status = %q (%q), want running", got.Status, got.Reason)
	}
	spec := lastCreatedNamed(t, h, "outhaul-app-web")
	if !contains(spec.Env, "SHARED_VAR=shared-val") {
		t.Errorf("runtime env missing SHARED_VAR: %v", spec.Env)
	}
	if !contains(spec.Env, "PROD_VAR=prod-val") {
		t.Errorf("runtime env missing PROD_VAR: %v", spec.Env)
	}
	if containsPrefix(spec.Env, "PREVIEW_VAR=") {
		t.Errorf("runtime env must not contain preview-scoped PREVIEW_VAR for a normal app: %v", spec.Env)
	}
}

// TestPipelineFiltersPreviewEnvByScope: a preview (ephemeral) app's deploy
// carries shared and preview-scoped vars, but never a prod-scoped var — that
// would leak a production secret (e.g. a live payment key) into a preview.
func TestPipelineFiltersPreviewEnvByScope(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	app, err := h.store.CreateApp(ctx, core.App{
		Name: "web-pr-1", RepoURL: "https://example.com/web.git", Domain: "web-pr-1.test",
		Ephemeral: true,
	})
	if err != nil {
		t.Fatalf("CreateApp: %v", err)
	}

	if err := h.store.SetEnvScoped(ctx, app.ID, "SHARED_VAR", "shared-val", false, core.ScopeShared); err != nil {
		t.Fatalf("SetEnvScoped SHARED_VAR: %v", err)
	}
	if err := h.store.SetEnvScoped(ctx, app.ID, "PROD_VAR", "prod-val", false, core.ScopeProd); err != nil {
		t.Fatalf("SetEnvScoped PROD_VAR: %v", err)
	}
	if err := h.store.SetEnvScoped(ctx, app.ID, "PREVIEW_VAR", "preview-val", false, core.ScopePreview); err != nil {
		t.Fatalf("SetEnvScoped PREVIEW_VAR: %v", err)
	}

	dep := h.claimedDeployment(t, app.ID)
	h.worker.runPipeline(ctx, dep)

	if got := h.status(t, dep.ID); got.Status != core.StatusRunning {
		t.Fatalf("status = %q (%q), want running", got.Status, got.Reason)
	}
	spec := lastCreatedNamed(t, h, "outhaul-app-web-pr-1")
	if !contains(spec.Env, "SHARED_VAR=shared-val") {
		t.Errorf("runtime env missing SHARED_VAR: %v", spec.Env)
	}
	if !contains(spec.Env, "PREVIEW_VAR=preview-val") {
		t.Errorf("runtime env missing PREVIEW_VAR: %v", spec.Env)
	}
	if containsPrefix(spec.Env, "PROD_VAR=") {
		t.Errorf("runtime env must not contain prod-scoped PROD_VAR for a preview app: %v", spec.Env)
	}
}
