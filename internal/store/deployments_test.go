package store

import (
	"context"
	"testing"

	"github.com/outhaul-dev/outhaul/internal/core"
)

func TestCountActiveDeployments(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// One deployment per app, each moved to a different status.
	mk := func(name string, to core.DeployStatus) {
		t.Helper()
		app := mustApp(t, s, name)
		d, err := s.CreateDeployment(ctx, app.ID)
		if err != nil {
			t.Fatal(err)
		}
		if to == core.StatusQueued {
			return
		}
		if ok, err := s.ClaimDeployment(ctx, d.ID); err != nil || !ok {
			t.Fatalf("claim: ok=%v err=%v", ok, err)
		}
		from := core.StatusBuilding
		for _, next := range []core.DeployStatus{core.StatusDeploying, core.StatusRunning} {
			if from == to {
				break
			}
			if ok, err := s.SetStatus(ctx, d.ID, from, next, ""); err != nil || !ok {
				t.Fatalf("SetStatus %s->%s: ok=%v err=%v", from, next, ok, err)
			}
			from = next
		}
	}
	mk("a", core.StatusQueued)
	mk("b", core.StatusBuilding)
	mk("c", core.StatusDeploying)
	mk("d", core.StatusRunning)

	n, err := s.CountActiveDeployments(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("CountActiveDeployments = %d, want 2 (building + deploying)", n)
	}
}
