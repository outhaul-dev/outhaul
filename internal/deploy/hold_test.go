package deploy

import (
	"context"
	"testing"
	"time"

	"github.com/outhaul-dev/outhaul/internal/core"
)

func TestHeldWorkerClaimsNothing(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	app := h.app(t, "web")
	dep, err := h.store.CreateDeployment(ctx, app.ID)
	if err != nil {
		t.Fatal(err)
	}
	held := true
	h.worker.SetHold(func() bool { return held })

	h.worker.dispatch(ctx)

	if got := h.status(t, dep.ID).Status; got != core.StatusQueued {
		t.Fatalf("held: status = %q, want queued", got)
	}
	if len(h.cloner.cloned) != 0 {
		t.Errorf("held: cloned %v, want nothing", h.cloner.cloned)
	}

	held = false
	h.worker.dispatch(ctx)
	waitForStatus(t, h, dep.ID, core.StatusRunning, 10*time.Second)
	h.worker.wg.Wait()
}
