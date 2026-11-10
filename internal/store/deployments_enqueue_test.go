package store

import (
	"context"
	"sync"
	"testing"

	"github.com/outhaul-dev/outhaul/internal/core"
)

// EnqueueDeployment hands choose the app's deployments, newest first, and
// queues what it picks: a reuse of an image (like CreateRollback, tag
// included) or a normal deploy (like CreateDeployment).
func TestEnqueueDeploymentQueuesWhatChooseReturns(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	app := mustApp(t, s, "web")

	built := enqueueTag(t, s, app.ID, "v1.2.0")
	if err := s.SetImage(ctx, built.ID, "outhaul/web:1"); err != nil {
		t.Fatal(err)
	}
	toStatus(t, s, built.ID, core.StatusBuilding, core.StatusDeploying, core.StatusRunning)
	other := mustApp(t, s, "other")
	if _, err := s.CreateDeployment(ctx, other.ID); err != nil {
		t.Fatal(err)
	}
	queued, err := s.CreateDeployment(ctx, app.ID)
	if err != nil {
		t.Fatal(err)
	}

	var seen []int64
	dep, queuedOne, err := s.EnqueueDeployment(ctx, app.ID, func(deps []core.Deployment) DeployChoice {
		for _, d := range deps {
			seen = append(seen, d.ID)
		}
		return DeployChoice{Image: "outhaul/web:1", RollbackOf: built.ID}
	})
	if err != nil || !queuedOne {
		t.Fatalf("EnqueueDeployment = %v, %v; want one queued", queuedOne, err)
	}
	if len(seen) != 2 || seen[0] != queued.ID || seen[1] != built.ID {
		t.Errorf("choose saw %v, want this app's deployments newest first: [%d %d]", seen, queued.ID, built.ID)
	}
	got, err := s.GetDeployment(ctx, dep.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != core.StatusQueued || got.Image != "outhaul/web:1" || got.RollbackOf != built.ID || got.Tag != "v1.2.0" || got.AppID != app.ID {
		t.Errorf("reuse = %+v, want a queued reuse of outhaul/web:1 from #%d with its tag", got, built.ID)
	}
	if dep != got {
		t.Errorf("returned %+v, stored %+v", dep, got)
	}

	dep, queuedOne, err = s.EnqueueDeployment(ctx, app.ID, func([]core.Deployment) DeployChoice { return DeployChoice{} })
	if err != nil || !queuedOne {
		t.Fatalf("EnqueueDeployment = %v, %v; want one queued", queuedOne, err)
	}
	if dep.Status != core.StatusQueued || dep.Image != "" || dep.RollbackOf != 0 || dep.Tag != "" {
		t.Errorf("normal deploy = %+v, want a plain queued deployment", dep)
	}

	// choose can decide, from the same read, to queue nothing.
	before, err := s.ListDeploymentsForApp(ctx, app.ID)
	if err != nil {
		t.Fatal(err)
	}
	dep, queuedOne, err = s.EnqueueDeployment(ctx, app.ID, func([]core.Deployment) DeployChoice { return DeployChoice{Skip: true} })
	if err != nil || queuedOne || dep.ID != 0 {
		t.Errorf("skip: EnqueueDeployment = %+v, %v, %v; want nothing queued", dep, queuedOne, err)
	}
	if after, err := s.ListDeploymentsForApp(ctx, app.ID); err != nil || len(after) != len(before) {
		t.Errorf("skip: deployments %d -> %d (%v), want none added", len(before), len(after), err)
	}
}

// Nothing else can queue a deployment between choose's read and the insert:
// eight callers that each queue a reuse only when nothing is queued yet
// queue exactly one.
func TestEnqueueDeploymentSerializesCallers(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	app := mustApp(t, s, "race")

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := s.EnqueueDeployment(ctx, app.ID, func(deps []core.Deployment) DeployChoice {
				for _, d := range deps {
					if d.Status == core.StatusQueued {
						return DeployChoice{}
					}
				}
				return DeployChoice{Image: "img:1", RollbackOf: 1}
			})
			if err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	ds, err := s.ListDeploymentsForApp(ctx, app.ID)
	if err != nil {
		t.Fatal(err)
	}
	reuses := 0
	for _, d := range ds {
		if d.Image != "" {
			reuses++
		}
	}
	if len(ds) != 8 || reuses != 1 {
		t.Errorf("%d deployments, %d reuses; want 8 and 1", len(ds), reuses)
	}
}
