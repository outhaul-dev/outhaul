package deploy

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/outhaul-dev/outhaul/internal/compose"
	"github.com/outhaul-dev/outhaul/internal/config"
	"github.com/outhaul-dev/outhaul/internal/core"
)

func newBuildCapHarness(t *testing.T, n int) *harness {
	return newHarnessWith(t, func(c *config.Config) { c.MaxConcurrentBuilds = n })
}

// blockBuild holds every nixpacks build until release is closed, and closes
// started when the first one begins.
func blockBuild(h *harness) (started, release chan struct{}) {
	started, release = make(chan struct{}), make(chan struct{})
	h.builder.started, h.builder.release = started, release
	return started, release
}

func TestBuildCapRunsBuildsOneAtATime(t *testing.T) {
	h := newBuildCapHarness(t, 1)
	started, release := blockBuild(h)
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	go h.worker.Run(ctx)

	one, two := h.app(t, "one"), h.app(t, "two")
	d1, err := h.store.CreateDeployment(ctx, one.ID)
	if err != nil {
		t.Fatal(err)
	}
	d2, err := h.store.CreateDeployment(ctx, two.ID)
	if err != nil {
		t.Fatal(err)
	}
	h.worker.Notify()

	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("no build started")
	}
	if got := h.status(t, d1.ID).Status; got != core.StatusBuilding {
		t.Errorf("first deployment = %q, want building", got)
	}
	if got := h.status(t, d2.ID).Status; got != core.StatusQueued {
		t.Errorf("second deployment = %q, want queued while the only build slot is held", got)
	}

	close(release)
	waitForStatus(t, h, d1.ID, core.StatusRunning, 5*time.Second)
	waitForStatus(t, h, d2.ID, core.StatusRunning, 5*time.Second)
}

func TestBuildCapLetsARollbackRunBesideABuild(t *testing.T) {
	h := newBuildCapHarness(t, 1)
	started, release := blockBuild(h)
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	go h.worker.Run(ctx)

	buildApp, rollbackApp := h.app(t, "builder"), h.app(t, "rollback")
	build, err := h.store.CreateDeployment(ctx, buildApp.ID)
	if err != nil {
		t.Fatal(err)
	}
	rollback, err := h.store.CreateRollback(ctx, rollbackApp.ID, "img:old", 0)
	if err != nil {
		t.Fatal(err)
	}
	h.worker.Notify()

	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("no build started")
	}
	// The rollback has an image, so it never takes a build slot.
	waitForStatus(t, h, rollback.ID, core.StatusRunning, 5*time.Second)
	if got := h.status(t, build.ID).Status; got != core.StatusBuilding {
		t.Errorf("build = %q, want still building while the rollback ran", got)
	}
	close(release)
	waitForStatus(t, h, build.ID, core.StatusRunning, 5*time.Second)
}

func TestBuildSlotIsReleasedWhenBuildingEnds(t *testing.T) {
	h := newBuildCapHarness(t, 1)
	upStarted, upRelease := make(chan struct{}), make(chan struct{})
	var once sync.Once
	h.compose.Hook = func(c compose.Call) {
		if c.Verb != "up" {
			return
		}
		once.Do(func() { close(upStarted) })
		<-upRelease
	}
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	go h.worker.Run(ctx)

	one := h.composeApp(t, "one", "docker-compose.yml", "")
	two := h.composeApp(t, "two", "docker-compose.yml", "")
	d1, err := h.store.CreateDeployment(ctx, one.ID)
	if err != nil {
		t.Fatal(err)
	}
	d2, err := h.store.CreateDeployment(ctx, two.ID)
	if err != nil {
		t.Fatal(err)
	}
	h.worker.Notify()

	select {
	case <-upStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("the first stack never reached up")
	}
	// The first deployment has left building, so its build slot is free even
	// though its `up` is still blocked. The second build starting proves it;
	// its status moves on from building quickly, so assert on the call.
	if got := h.status(t, d1.ID).Status; got != core.StatusDeploying {
		t.Errorf("first deployment = %q, want deploying", got)
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(h.compose.CallsFor("build")) < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("the second stack never started building while the first waited in up")
		}
		time.Sleep(10 * time.Millisecond)
	}

	close(upRelease)
	waitForStatus(t, h, d1.ID, core.StatusRunning, 5*time.Second)
	waitForStatus(t, h, d2.ID, core.StatusRunning, 5*time.Second)
}

func TestBuildCapDoesNotJumpAnOlderQueuedBuild(t *testing.T) {
	h := newBuildCapHarness(t, 1)
	started, release := blockBuild(h)
	ctx := context.Background()
	busy := h.app(t, "busy")
	first, err := h.store.CreateDeployment(ctx, busy.ID)
	if err != nil {
		t.Fatal(err)
	}
	h.worker.dispatch(ctx)
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("no build started")
	}

	a := h.app(t, "a")
	older, err := h.store.CreateDeployment(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	newer, err := h.store.CreateRollback(ctx, a.ID, "img:old", 0)
	if err != nil {
		t.Fatal(err)
	}

	// The only build slot is held, and the newer rollback must not run ahead of
	// the older queued build of its own app.
	h.worker.dispatch(ctx)
	if got := h.status(t, older.ID).Status; got != core.StatusQueued {
		t.Errorf("older build = %q, want queued", got)
	}
	if got := h.status(t, newer.ID).Status; got != core.StatusQueued {
		t.Errorf("newer rollback = %q, want queued behind its app's older build", got)
	}

	close(release)
	waitForStatus(t, h, first.ID, core.StatusRunning, 5*time.Second)
	h.worker.wg.Wait()
}

func TestBuildSlotIsReleasedWhenTheClaimFails(t *testing.T) {
	h := newBuildCapHarness(t, 1)
	ctx := context.Background()
	app := h.app(t, "web")
	if _, err := h.store.CreateDeployment(ctx, app.ID); err != nil {
		t.Fatal(err)
	}
	h.worker.claim = func(context.Context, int64) (bool, error) { return false, errors.New("boom") }

	h.worker.dispatch(ctx)

	if n := len(h.worker.buildSlots); n != 0 {
		t.Errorf("build slots held = %d, want 0 after a failed claim", n)
	}
	if n := len(h.worker.sem); n != 0 {
		t.Errorf("pipeline slots held = %d, want 0 after a failed claim", n)
	}
}

func TestBuildSlotIsReleasedWhenTheClaimLosesTheRace(t *testing.T) {
	h := newBuildCapHarness(t, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	app := h.app(t, "web")
	if _, err := h.store.CreateDeployment(ctx, app.ID); err != nil {
		t.Fatal(err)
	}
	h.worker.claim = func(context.Context, int64) (bool, error) {
		cancel() // end the dispatch loop after the lost claim
		return false, nil
	}

	h.worker.dispatch(ctx)

	if n := len(h.worker.buildSlots); n != 0 {
		t.Errorf("build slots held = %d, want 0 after a lost claim", n)
	}
	if n := len(h.worker.sem); n != 0 {
		t.Errorf("pipeline slots held = %d, want 0 after a lost claim", n)
	}
}

// A build that fails gives its slot back as soon as fail records it; the
// pipeline goroutine's defer is only the safety net.
func TestFailReleasesTheBuildSlot(t *testing.T) {
	h := newBuildCapHarness(t, 1)
	d := core.Deployment{ID: 7}
	h.worker.buildSlots <- struct{}{}
	h.worker.holdBuild(d.ID)

	h.worker.fail(d, core.StatusBuilding, "build failed", io.Discard)

	if n := len(h.worker.buildSlots); n != 0 {
		t.Fatalf("build slots held = %d, want 0 after fail", n)
	}
}
