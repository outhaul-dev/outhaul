package deploy

import (
	"context"
	"testing"

	"github.com/outhaul-dev/outhaul/internal/core"
	"github.com/outhaul-dev/outhaul/internal/docker"
)

var defaultTestLimits = core.Limits{MilliCPUs: 1000, Memory: 1 << 30, Pids: 512}

func wantLimits(t *testing.T, spec docker.ContainerSpec, want core.Limits) {
	t.Helper()
	if spec.NanoCPUs != want.MilliCPUs*1_000_000 || spec.Memory != want.Memory || spec.PidsLimit != want.Pids {
		t.Errorf("%s limits = (%d, %d, %d), want (%d, %d, %d)",
			spec.Name, spec.NanoCPUs, spec.Memory, spec.PidsLimit,
			want.MilliCPUs*1_000_000, want.Memory, want.Pids)
	}
}

func TestSingleContainerCarriesDefaultLimits(t *testing.T) {
	h := newHarness(t)
	h.worker.cfg.DefaultLimits = defaultTestLimits
	ctx := context.Background()
	app := h.app(t, "web")
	dep := h.claimedDeployment(t, app.ID)

	h.worker.runPipeline(ctx, dep)

	wantLimits(t, lastCreatedNamed(t, h, "outhaul-deploy-"+itoa64(dep.ID)), defaultTestLimits)
	wantLimits(t, lastCreatedNamed(t, h, "outhaul-app-web"), defaultTestLimits)
}

func TestSingleContainerWithoutLimitsIsUnchanged(t *testing.T) {
	h := newHarness(t)
	app := h.app(t, "web")
	dep := h.claimedDeployment(t, app.ID)

	h.worker.runPipeline(context.Background(), dep)

	if len(h.docker.Created) == 0 {
		t.Fatal("no containers created")
	}
	for _, spec := range h.docker.Created {
		wantLimits(t, spec, core.Limits{})
	}
}

func TestStopFirstDeployCarriesDefaultLimits(t *testing.T) {
	h := newHarness(t)
	h.worker.cfg.DefaultLimits = defaultTestLimits
	ctx := context.Background()
	app := h.app(t, "data")
	if _, err := h.store.AddVolume(ctx, app.ID, "/data"); err != nil {
		t.Fatalf("AddVolume: %v", err)
	}
	dep := h.claimedDeployment(t, app.ID)

	h.worker.runPipeline(ctx, dep)

	wantLimits(t, lastCreatedNamed(t, h, "outhaul-app-data"), defaultTestLimits)
}
