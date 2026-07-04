package deploy

import (
	"context"
	"testing"
	"time"

	"github.com/slipwaydev/slipway/internal/core"
	"github.com/slipwaydev/slipway/internal/docker"
)

// claimedRollback enqueues a rollback (image pre-set) and moves it to
// building, as the dispatcher would before invoking the pipeline.
func (h *harness) claimedRollback(t *testing.T, appID int64, image string, rollbackOf int64) core.Deployment {
	t.Helper()
	d, err := h.store.CreateRollback(context.Background(), appID, image, rollbackOf)
	if err != nil {
		t.Fatalf("CreateRollback: %v", err)
	}
	ok, err := h.store.ClaimDeployment(context.Background(), d.ID)
	if err != nil || !ok {
		t.Fatalf("ClaimDeployment: ok=%v err=%v", ok, err)
	}
	d.Status = core.StatusBuilding
	return d
}

func TestPipelineRollbackSkipsCloneAndBuild(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	app := h.app(t, "web")

	// The app is live on the image from a newer (bad) deploy.
	oldID, _ := h.docker.CreateContainer(ctx, docker.ContainerSpec{Name: "slipway-app-web", Image: "slipway/web:9"})
	h.docker.StartContainer(ctx, oldID)

	dep := h.claimedRollback(t, app.ID, "slipway/web:7", 7)
	h.worker.runPipeline(ctx, dep)

	got := h.status(t, dep.ID)
	if got.Status != core.StatusRunning {
		t.Fatalf("status = %q (reason %q), want running", got.Status, got.Reason)
	}
	if len(h.cloner.cloned) != 0 {
		t.Errorf("rollback must not clone, cloned %v", h.cloner.cloned)
	}
	if h.builder.lastReq.ImageTag != "" {
		t.Errorf("rollback must not build, built %q", h.builder.lastReq.ImageTag)
	}
	c, _ := h.docker.FindContainer(ctx, "slipway-app-web")
	if c == nil || !c.Running() || c.Image != "slipway/web:7" {
		t.Fatalf("canonical container should run the rolled-back image, got %+v", c)
	}
	if got.Image != "slipway/web:7" {
		t.Errorf("deployment image = %q, want the reused tag", got.Image)
	}
}

// A rollback goes through the same health gate as a build: if the old image
// no longer starts cleanly, the live container is untouched.
func TestPipelineRollbackUnhealthyKeepsCurrentContainer(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	app := h.app(t, "web")

	oldID, _ := h.docker.CreateContainer(ctx, docker.ContainerSpec{Name: "slipway-app-web", Image: "slipway/web:9"})
	h.docker.StartContainer(ctx, oldID)
	h.worker.healthCheck = func(context.Context, string, time.Duration) bool { return false }

	dep := h.claimedRollback(t, app.ID, "slipway/web:7", 7)
	h.worker.runPipeline(ctx, dep)

	if got := h.status(t, dep.ID); got.Status != core.StatusFailed {
		t.Fatalf("status = %q, want failed", got.Status)
	}
	if c, _ := h.docker.FindContainer(ctx, "slipway-app-web"); c == nil || c.Image != "slipway/web:9" {
		t.Errorf("current container should survive an unhealthy rollback, got %+v", c)
	}
}
