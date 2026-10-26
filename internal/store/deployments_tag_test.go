package store

import (
	"context"
	"reflect"
	"sort"
	"sync"
	"testing"

	"github.com/outhaul-dev/outhaul/internal/core"
)

func enqueueTag(t *testing.T, s *Store, appID int64, tag string) core.Deployment {
	t.Helper()
	d, ok, err := s.EnqueueTagDeployment(context.Background(), appID,
		func([]string) (string, bool) { return tag, true })
	if err != nil || !ok {
		t.Fatalf("enqueue %s: ok=%v err=%v", tag, ok, err)
	}
	return d
}

func toStatus(t *testing.T, s *Store, id int64, path ...core.DeployStatus) {
	t.Helper()
	from := core.StatusQueued
	for _, next := range path {
		if ok, err := s.SetStatus(context.Background(), id, from, next, ""); err != nil || !ok {
			t.Fatalf("SetStatus %s->%s: ok=%v err=%v", from, next, ok, err)
		}
		from = next
	}
}

func TestEnqueueTagDeploymentSeesLiveAndInFlightTags(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	app := mustApp(t, s, "tagged")

	d1 := enqueueTag(t, s, app.ID, "v1.2.0")
	toStatus(t, s, d1.ID, core.StatusBuilding, core.StatusDeploying, core.StatusRunning)
	d2 := enqueueTag(t, s, app.ID, "v1.3.0")
	toStatus(t, s, d2.ID, core.StatusBuilding, core.StatusDeploying, core.StatusRunning)
	if _, err := s.SupersedeOthers(ctx, app.ID, d2.ID); err != nil {
		t.Fatal(err)
	}
	d3 := enqueueTag(t, s, app.ID, "v1.4.0")
	toStatus(t, s, d3.ID, core.StatusBuilding, core.StatusFailed)
	enqueueTag(t, s, app.ID, "v1.3.5")

	var inPlay []string
	dep, ok, err := s.EnqueueTagDeployment(ctx, app.ID, func(p []string) (string, bool) {
		inPlay = p
		return "v1.3.6", true
	})
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	sort.Strings(inPlay)
	if want := []string{"v1.3.0", "v1.3.5"}; !reflect.DeepEqual(inPlay, want) {
		t.Fatalf("inPlay = %v, want %v", inPlay, want)
	}
	if dep.Tag != "v1.3.6" || !dep.FromTagEvent || dep.Status != core.StatusQueued {
		t.Fatalf("dep = %+v", dep)
	}
	got, err := s.GetDeployment(ctx, dep.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Tag != "v1.3.6" || !got.FromTagEvent {
		t.Fatalf("read back = %+v", got)
	}
}

func TestEnqueueTagDeploymentSkipInsertsNothing(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	app := mustApp(t, s, "skip")
	_, ok, err := s.EnqueueTagDeployment(ctx, app.ID, func([]string) (string, bool) { return "", false })
	if err != nil || ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	ds, err := s.ListDeploymentsForApp(ctx, app.ID)
	if err != nil || len(ds) != 0 {
		t.Fatalf("ds=%v err=%v", ds, err)
	}
}

func TestEnqueueTagDeploymentSerializesConcurrentDeliveries(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	app := mustApp(t, s, "race")

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := s.EnqueueTagDeployment(ctx, app.ID, func(p []string) (string, bool) {
				for _, x := range p {
					if x == "v1.0.0" {
						return "", false
					}
				}
				return "v1.0.0", true
			})
			if err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	ds, err := s.ListDeploymentsForApp(ctx, app.ID)
	if err != nil || len(ds) != 1 {
		t.Fatalf("len=%d err=%v", len(ds), err)
	}
}

func TestLiveDeploymentIsNewestRunning(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	app := mustApp(t, s, "live")
	if d, err := s.LiveDeployment(ctx, app.ID); err != nil || d != nil {
		t.Fatalf("d=%v err=%v", d, err)
	}
	a := enqueueTag(t, s, app.ID, "v1.0.0")
	toStatus(t, s, a.ID, core.StatusBuilding, core.StatusDeploying, core.StatusRunning)
	b := enqueueTag(t, s, app.ID, "v1.1.0")
	toStatus(t, s, b.ID, core.StatusBuilding, core.StatusDeploying, core.StatusRunning)
	d, err := s.LiveDeployment(ctx, app.ID)
	if err != nil || d == nil || d.ID != b.ID {
		t.Fatalf("d=%v err=%v want id %d", d, err, b.ID)
	}
}

func TestCreateRollbackCopiesTag(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	app := mustApp(t, s, "rb")
	src := enqueueTag(t, s, app.ID, "v1.3.0")
	rb, err := s.CreateRollback(ctx, app.ID, "img:1", src.ID)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.GetDeployment(ctx, rb.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Tag != "v1.3.0" || got.FromTagEvent {
		t.Fatalf("got = %+v", got)
	}
}

func TestSetDeploymentTag(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	app := mustApp(t, s, "settag")
	d, err := s.CreateDeployment(ctx, app.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetDeploymentTag(ctx, d.ID, "v2.0.0"); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetDeployment(ctx, d.ID)
	if err != nil || got.Tag != "v2.0.0" {
		t.Fatalf("got=%+v err=%v", got, err)
	}
}
