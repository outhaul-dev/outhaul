package deploy

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/outhaul-dev/outhaul/internal/core"
	"github.com/outhaul-dev/outhaul/internal/docker"
)

// The canonical container is created fresh at cutover (the healthy temp is
// discarded), so it must pass its own health check before the deploy is
// recorded as running. When it doesn't, it stays up — the old container is
// already gone, so tearing it down would only turn degraded into down — and
// the deployment says what happened.
func TestBlueGreenFailsWhenCanonicalUnhealthy(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	app := h.app(t, "web")
	calls := 0
	h.worker.healthCheck = func(context.Context, string, time.Duration) bool {
		calls++
		return calls == 1 // temp healthy, canonical not
	}
	dep := h.claimedDeployment(t, app.ID)
	h.worker.runPipeline(ctx, dep)

	got := h.status(t, dep.ID)
	if got.Status != core.StatusFailed {
		t.Fatalf("status = %q, want failed (canonical never became healthy)", got.Status)
	}
	if calls != 2 {
		t.Errorf("health checks = %d, want 2 (temp, then canonical)", calls)
	}
	if c, _ := h.docker.FindContainer(ctx, "outhaul-app-web"); c == nil {
		t.Error("unhealthy canonical was torn down; it should stay degraded-but-live")
	}
	if tmp, _ := h.docker.FindContainer(ctx, "outhaul-deploy-"+itoa64(dep.ID)); tmp != nil {
		t.Error("temp container leaked")
	}
}

// Shutdown arriving before the cutover begins must abort it and leave the
// old container serving — and say so, rather than claiming the app was
// deleted.
func TestBlueGreenShutdownBeforeCutoverKeepsOldContainer(t *testing.T) {
	h := newHarness(t)
	app := h.app(t, "web")
	oldID, _ := h.docker.CreateContainer(context.Background(), docker.ContainerSpec{Name: "outhaul-app-web", Image: "old:1"})
	_ = h.docker.StartContainer(context.Background(), oldID)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.worker.healthCheck = func(context.Context, string, time.Duration) bool {
		cancel() // SIGTERM lands while the temp is being probed
		return true
	}
	dep := h.claimedDeployment(t, app.ID)
	h.worker.runPipeline(ctx, dep)

	if c, _ := h.docker.FindContainer(context.Background(), "outhaul-app-web"); c == nil || c.Image != "old:1" {
		t.Fatalf("old container was replaced or removed during shutdown: %+v", c)
	}
	got := h.status(t, dep.ID)
	if got.Status != core.StatusFailed || !strings.Contains(got.Reason, "shutting down") {
		t.Errorf("status/reason = %q/%q, want failed / shutting down", got.Status, got.Reason)
	}
}

// Once the old container is removed the cutover must finish even if the
// pipeline context is cancelled, or the app is left with no container.
func TestBlueGreenCutoverCompletesDespiteCancellation(t *testing.T) {
	h := newHarness(t)
	app := h.app(t, "web")
	oldID, _ := h.docker.CreateContainer(context.Background(), docker.ContainerSpec{Name: "outhaul-app-web", Image: "old:1"})
	_ = h.docker.StartContainer(context.Background(), oldID)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.docker.FailRemove = func(id string) error {
		if id == oldID {
			cancel() // SIGTERM lands mid-cutover
		}
		return nil
	}
	h.worker.healthCheck = func(c context.Context, _ string, _ time.Duration) bool { return c.Err() == nil }
	dep := h.claimedDeployment(t, app.ID)
	h.worker.runPipeline(ctx, dep)

	c, _ := h.docker.FindContainer(context.Background(), "outhaul-app-web")
	if c == nil || c.ID == oldID {
		t.Fatalf("cutover did not complete: canonical = %+v", c)
	}
	if got := h.status(t, dep.ID); got.Status != core.StatusRunning {
		t.Errorf("status = %q (%s), want running", got.Status, got.Reason)
	}
}
