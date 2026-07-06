package deploy

import (
	"context"
	"testing"

	"github.com/james-smart/outhaul/internal/core"
	"github.com/james-smart/outhaul/internal/docker"
)

// A volume app mounts its volume on the canonical container, creates the
// Docker volume, and uses the stop-first path (no temp health-check container).
func TestPipelineVolumeAppMountsVolumeStopFirst(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	app := h.app(t, "data")
	if _, err := h.store.AddVolume(ctx, app.ID, "/data"); err != nil {
		t.Fatalf("AddVolume: %v", err)
	}
	dep := h.claimedDeployment(t, app.ID)

	h.worker.runPipeline(ctx, dep)

	if got := h.status(t, dep.ID); got.Status != core.StatusRunning {
		t.Fatalf("status = %q (%q), want running", got.Status, got.Reason)
	}
	// Canonical container carries the volume mount.
	spec := lastCreatedNamed(t, h, "outhaul-app-data")
	if !hasMount(spec.Mounts, docker.Mount{Source: "outhaul-data-data", Target: "/data", Volume: true}) {
		t.Errorf("canonical container missing volume mount: %+v", spec.Mounts)
	}
	// Docker volume was created (labelled).
	if vols, _ := h.docker.ListVolumesFull(ctx, core.VolumeLabels("data")); len(vols) != 1 {
		t.Errorf("expected the app's data volume to exist, got %+v", vols)
	}
	// Stop-first: no temp health-check container was ever created.
	for _, c := range h.docker.Created {
		if c.Name == "outhaul-deploy-"+itoa64(dep.ID) {
			t.Errorf("stop-first deploy must not create a temp container, got %q", c.Name)
		}
	}
}

// Stop-first removes the old canonical container before the new one exists.
func TestPipelineVolumeAppRemovesOldBeforeStart(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	app := h.app(t, "data")
	if _, err := h.store.AddVolume(ctx, app.ID, "/data"); err != nil {
		t.Fatalf("AddVolume: %v", err)
	}
	oldID, _ := h.docker.CreateContainer(ctx, docker.ContainerSpec{Name: "outhaul-app-data", Image: "old:1"})
	h.docker.StartContainer(ctx, oldID)

	dep := h.claimedDeployment(t, app.ID)
	h.worker.runPipeline(ctx, dep)

	if _, ok := h.docker.Containers[oldID]; ok {
		t.Error("old container should be removed by a stop-first deploy")
	}
	c, _ := h.docker.FindContainer(ctx, "outhaul-app-data")
	if c == nil || !c.Running() {
		t.Fatalf("new canonical not running: %+v", c)
	}
}

func hasMount(ms []docker.Mount, want docker.Mount) bool {
	for _, m := range ms {
		if m == want {
			return true
		}
	}
	return false
}
