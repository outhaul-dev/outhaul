package prune

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/outhaul-dev/outhaul/internal/core"
	"github.com/outhaul-dev/outhaul/internal/docker"
	"github.com/outhaul-dev/outhaul/internal/store"
)

type harness struct {
	store   *store.Store
	docker  *docker.Fake
	workDir string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "test.db"), nil)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return &harness{store: st, docker: docker.NewFake(), workDir: dir}
}

func (h *harness) pruner(keep int) *Pruner {
	return New(h.store, h.docker, keep, h.workDir)
}

func (h *harness) app(t *testing.T, name string) core.App {
	t.Helper()
	app, err := h.store.CreateApp(context.Background(), core.App{
		Name: name, RepoURL: "https://example.com/r.git", Domain: name + ".test",
	})
	if err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	return app
}

// deployed seeds one finished deployment bearing an image: the row is driven
// through the real state machine and the tag is planted in the fake daemon.
func (h *harness) deployed(t *testing.T, app core.App, final core.DeployStatus) core.Deployment {
	t.Helper()
	ctx := context.Background()
	d, err := h.store.CreateDeployment(ctx, app.ID)
	if err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}
	if _, err := h.store.ClaimDeployment(ctx, d.ID); err != nil {
		t.Fatalf("ClaimDeployment: %v", err)
	}
	image := fmt.Sprintf("outhaul/%s:%d", app.Name, d.ID)
	if err := h.store.SetImage(ctx, d.ID, image); err != nil {
		t.Fatalf("SetImage: %v", err)
	}
	h.docker.Images[image] = true
	switch final {
	case core.StatusRunning:
		mustTransition(t, h.store, d.ID, core.StatusBuilding, core.StatusDeploying)
		mustTransition(t, h.store, d.ID, core.StatusDeploying, core.StatusRunning)
	case core.StatusFailed:
		mustTransition(t, h.store, d.ID, core.StatusBuilding, core.StatusFailed)
	case core.StatusBuilding:
		// leave in-flight
	default:
		t.Fatalf("deployed: unsupported final status %q", final)
	}
	got, err := h.store.GetDeployment(ctx, d.ID)
	if err != nil {
		t.Fatalf("GetDeployment: %v", err)
	}
	return got
}

func mustTransition(t *testing.T, st *store.Store, id int64, from, to core.DeployStatus) {
	t.Helper()
	ok, err := st.SetStatus(context.Background(), id, from, to, "")
	if err != nil || !ok {
		t.Fatalf("SetStatus %s->%s: ok=%v err=%v", from, to, ok, err)
	}
}

func (h *harness) pruneApp(t *testing.T, p *Pruner, app core.App) string {
	t.Helper()
	var out strings.Builder
	if err := p.PruneApp(context.Background(), app, &out); err != nil {
		t.Fatalf("PruneApp: %v", err)
	}
	return out.String()
}

func (h *harness) mustHaveImages(t *testing.T, want ...string) {
	t.Helper()
	for _, tag := range want {
		if !h.docker.Images[tag] {
			t.Errorf("image %s should still exist", tag)
		}
	}
}

func (h *harness) mustNotHaveImages(t *testing.T, gone ...string) {
	t.Helper()
	for _, tag := range gone {
		if h.docker.Images[tag] {
			t.Errorf("image %s should have been removed", tag)
		}
	}
}

func (h *harness) pruned(t *testing.T, id int64) bool {
	t.Helper()
	d, err := h.store.GetDeployment(context.Background(), id)
	if err != nil {
		t.Fatalf("GetDeployment(%d): %v", id, err)
	}
	return d.ImagePruned
}

func TestPruneAppKeepsNewestDistinct(t *testing.T) {
	h := newHarness(t)
	app := h.app(t, "web")
	var deps []core.Deployment
	for i := 0; i < 7; i++ {
		deps = append(deps, h.deployed(t, app, core.StatusRunning))
	}

	out := h.pruneApp(t, h.pruner(5), app)

	// Oldest two tags go; the newest five stay.
	h.mustNotHaveImages(t, deps[0].Image, deps[1].Image)
	h.mustHaveImages(t, deps[2].Image, deps[3].Image, deps[4].Image, deps[5].Image, deps[6].Image)
	if !h.pruned(t, deps[0].ID) || !h.pruned(t, deps[1].ID) {
		t.Error("removed images should mark their rows pruned")
	}
	if h.pruned(t, deps[2].ID) {
		t.Error("a retained image's row must not be marked pruned")
	}
	if !strings.Contains(out, deps[0].Image) {
		t.Errorf("prune output should name the removed tag; got %q", out)
	}
}

func TestPruneAppRollbackRowsShareOneTag(t *testing.T) {
	h := newHarness(t)
	app := h.app(t, "web")
	var deps []core.Deployment
	for i := 0; i < 6; i++ {
		deps = append(deps, h.deployed(t, app, core.StatusRunning))
	}
	// Roll back to the oldest build and finish that deploy: its tag is now
	// the newest distinct entry even though the source row is ancient.
	rb, err := h.store.CreateRollback(context.Background(), app.ID, deps[0].Image, deps[0].ID)
	if err != nil {
		t.Fatalf("CreateRollback: %v", err)
	}
	ctx := context.Background()
	if _, err := h.store.ClaimDeployment(ctx, rb.ID); err != nil {
		t.Fatal(err)
	}
	mustTransition(t, h.store, rb.ID, core.StatusBuilding, core.StatusDeploying)
	mustTransition(t, h.store, rb.ID, core.StatusDeploying, core.StatusRunning)

	h.pruneApp(t, h.pruner(5), app)

	// Distinct newest five: dep0's tag (via the rollback row), then 5,4,3,2.
	// Only dep1's tag falls out.
	h.mustNotHaveImages(t, deps[1].Image)
	h.mustHaveImages(t, deps[0].Image, deps[2].Image, deps[3].Image, deps[4].Image, deps[5].Image)
	if h.pruned(t, deps[0].ID) {
		t.Error("the rollback's source row shares the live tag and must stay unpruned")
	}
}

func TestPruneAppProtectsInFlightRollback(t *testing.T) {
	h := newHarness(t)
	app := h.app(t, "web")
	old := h.deployed(t, app, core.StatusRunning)
	var deps []core.Deployment
	for i := 0; i < 5; i++ {
		deps = append(deps, h.deployed(t, app, core.StatusRunning))
	}
	// A rollback to the ancient image is queued but not yet claimed: its tag
	// is outside the window but must survive or the pipeline breaks.
	if _, err := h.store.CreateRollback(context.Background(), app.ID, old.Image, old.ID); err != nil {
		t.Fatal(err)
	}

	h.pruneApp(t, h.pruner(3), app)

	h.mustHaveImages(t, old.Image)
	if h.pruned(t, old.ID) {
		t.Error("in-flight rollback's tag must not be marked pruned")
	}
}

func TestPruneAppProtectsLiveImage(t *testing.T) {
	h := newHarness(t)
	app := h.app(t, "web")
	first := h.deployed(t, app, core.StatusRunning)
	live := h.deployed(t, app, core.StatusRunning)
	// A newer build succeeded but its deploy failed: with keep=1 the window
	// holds only the failed build's tag — the live image must still survive.
	failed := h.deployed(t, app, core.StatusFailed)

	h.pruneApp(t, h.pruner(1), app)

	h.mustHaveImages(t, live.Image, failed.Image)
	h.mustNotHaveImages(t, first.Image)
}

func TestPruneAppDisabledAndCompose(t *testing.T) {
	h := newHarness(t)
	app := h.app(t, "web")
	for i := 0; i < 4; i++ {
		h.deployed(t, app, core.StatusRunning)
	}

	h.pruneApp(t, h.pruner(0), app) // keep=0 disables
	if len(h.docker.RemovedImages) != 0 {
		t.Errorf("keep=0 must remove nothing; removed %v", h.docker.RemovedImages)
	}

	composeApp := app
	composeApp.Kind = core.KindCompose
	h.pruneApp(t, h.pruner(1), composeApp) // compose stacks have no per-deploy images
	if len(h.docker.RemovedImages) != 0 {
		t.Errorf("compose apps must be skipped; removed %v", h.docker.RemovedImages)
	}
}

func TestPruneAppRemoveFailureLeavesRowRollbackable(t *testing.T) {
	h := newHarness(t)
	app := h.app(t, "web")
	stuck := h.deployed(t, app, core.StatusRunning)
	gone := h.deployed(t, app, core.StatusRunning)
	for i := 0; i < 2; i++ {
		h.deployed(t, app, core.StatusRunning)
	}
	h.docker.FailRemoveImage = func(ref string) error {
		if ref == stuck.Image {
			return errors.New("image is being used by running container abc")
		}
		return nil
	}

	out := h.pruneApp(t, h.pruner(2), app)

	if h.pruned(t, stuck.ID) {
		t.Error("a row whose image Docker refused to delete must stay rollback-able")
	}
	if !h.pruned(t, gone.ID) {
		t.Error("other candidates should still be pruned after one failure")
	}
	if !strings.Contains(out, "WARNING") {
		t.Errorf("failure should be logged to the deploy log; got %q", out)
	}
}

func TestSweepReconcilesOrphans(t *testing.T) {
	h := newHarness(t)
	app := h.app(t, "web")
	kept := h.deployed(t, app, core.StatusRunning)
	inflight := h.deployed(t, app, core.StatusBuilding)
	// Orphans: a deleted app's leftover and a pre-retention-era tag.
	h.docker.Images["outhaul/gone:9999"] = true
	// Foreign images must never be candidates.
	h.docker.Images["postgres:17"] = true

	h.pruner(5).Sweep(context.Background())

	h.mustHaveImages(t, kept.Image, inflight.Image, "postgres:17")
	h.mustNotHaveImages(t, "outhaul/gone:9999")
	if h.docker.ImagePrunes != 1 {
		t.Errorf("dangling-image prune calls = %d, want 1", h.docker.ImagePrunes)
	}
	if len(h.docker.BuildCachePrunes) != 1 || h.docker.BuildCachePrunes[0] != buildCacheAge {
		t.Errorf("build-cache prunes = %v, want one at %v", h.docker.BuildCachePrunes, buildCacheAge)
	}
}

func TestSweepDisabledRetentionStillPrunesDanglingAndCache(t *testing.T) {
	h := newHarness(t)
	h.docker.Images["outhaul/gone:9999"] = true

	h.pruner(0).Sweep(context.Background())

	h.mustHaveImages(t, "outhaul/gone:9999") // reconciliation off with keep=0
	if h.docker.ImagePrunes != 1 || len(h.docker.BuildCachePrunes) != 1 {
		t.Error("dangling and build-cache prunes should run even with retention disabled")
	}
}

func TestSweepCleansStaleWorkDirs(t *testing.T) {
	h := newHarness(t)
	app := h.app(t, "web")
	done := h.deployed(t, app, core.StatusRunning)
	active := h.deployed(t, app, core.StatusBuilding)

	mkdir := func(name string) string {
		full := filepath.Join(h.workDir, name)
		if err := os.MkdirAll(full, 0o755); err != nil {
			t.Fatal(err)
		}
		return full
	}
	doneDir := mkdir(fmt.Sprintf("dep-%d", done.ID))
	activeDir := mkdir(fmt.Sprintf("dep-%d", active.ID))
	goneDir := mkdir("dep-424242") // deployment row no longer exists

	oldTemp := filepath.Join(h.workDir, "backup-old")
	if err := os.WriteFile(oldTemp, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	stale := time.Now().Add(-2 * staleTempAge)
	if err := os.Chtimes(oldTemp, stale, stale); err != nil {
		t.Fatal(err)
	}
	freshTemp := filepath.Join(h.workDir, "backup-fresh")
	if err := os.WriteFile(freshTemp, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	h.pruner(5).Sweep(context.Background())

	for _, gone := range []string{doneDir, goneDir, oldTemp} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Errorf("%s should have been removed", filepath.Base(gone))
		}
	}
	for _, keep := range []string{activeDir, freshTemp} {
		if _, err := os.Stat(keep); err != nil {
			t.Errorf("%s should have been kept: %v", filepath.Base(keep), err)
		}
	}
}
