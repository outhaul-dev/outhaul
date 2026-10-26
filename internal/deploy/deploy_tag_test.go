package deploy

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/outhaul-dev/outhaul/internal/core"
	"github.com/outhaul-dev/outhaul/internal/store"
)

// tagApp creates an app in tag mode with the pattern v*.
func (h *harness) tagApp(t *testing.T, name string) core.App {
	t.Helper()
	app := h.app(t, name)
	h.tagMode(t, app.ID)
	return app
}

func (h *harness) tagMode(t *testing.T, appID int64) {
	t.Helper()
	err := h.store.UpdateAppSettings(context.Background(), appID, store.DeploySettings{
		Branch: "main", Trigger: core.TriggerTag, TagPattern: "v*",
	})
	if err != nil {
		t.Fatalf("UpdateAppSettings: %v", err)
	}
}

// claimedTagDeployment enqueues a tag-event deployment and claims it.
func (h *harness) claimedTagDeployment(t *testing.T, appID int64, tag string) core.Deployment {
	t.Helper()
	d, ok, err := h.store.EnqueueTagDeployment(context.Background(), appID,
		func([]string) (string, bool) { return tag, true })
	if err != nil || !ok {
		t.Fatalf("EnqueueTagDeployment: ok=%v err=%v", ok, err)
	}
	claimed, err := h.store.ClaimDeployment(context.Background(), d.ID)
	if err != nil || !claimed {
		t.Fatalf("ClaimDeployment: ok=%v err=%v", claimed, err)
	}
	d.Status = core.StatusBuilding
	return d
}

func (h *harness) logOf(dep core.Deployment) string {
	history, _, cancel := h.broker.Subscribe(dep.ID)
	cancel()
	return strings.Join(history, "\n")
}

func TestPipelineBuildsTheDeploymentsTag(t *testing.T) {
	h := newHarness(t)
	app := h.tagApp(t, "web")
	dep := h.claimedTagDeployment(t, app.ID, "v1.4.0")

	h.worker.runPipeline(context.Background(), dep)

	if got := h.status(t, dep.ID); got.Status != core.StatusRunning {
		t.Fatalf("status = %q (%q), want running", got.Status, got.Reason)
	}
	if len(h.cloner.branches) != 1 || h.cloner.branches[0] != "refs/tags/v1.4.0" {
		t.Errorf("branches = %v, want [refs/tags/v1.4.0]", h.cloner.branches)
	}
	if log := h.logOf(dep); !strings.Contains(log, "Building tag v1.4.0") {
		t.Errorf("log lacks 'Building tag v1.4.0':\n%s", log)
	}
}

func TestPipelineSkipsTagOlderThanLiveWhenClaimed(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	app := h.tagApp(t, "web")
	first := h.claimedTagDeployment(t, app.ID, "v1.4.0")
	h.worker.runPipeline(ctx, first)
	if got := h.status(t, first.ID); got.Status != core.StatusRunning {
		t.Fatalf("first status = %q (%q)", got.Status, got.Reason)
	}

	older := h.claimedTagDeployment(t, app.ID, "v1.3.1")
	h.worker.runPipeline(ctx, older)

	got := h.status(t, older.ID)
	if got.Status != core.StatusCancelled {
		t.Fatalf("status = %q (%q), want cancelled", got.Status, got.Reason)
	}
	if !strings.HasPrefix(got.Reason, "skipped: v1.3.1 is not newer than v1.4.0") {
		t.Errorf("reason = %q", got.Reason)
	}
	if len(h.cloner.branches) != 1 {
		t.Errorf("branches = %v, want only the first build", h.cloner.branches)
	}
	if log := h.logOf(older); !strings.Contains(log, "v1.3.1 is not newer than v1.4.0") {
		t.Errorf("reason missing from the log:\n%s", log)
	}
}

func TestDeployClickBuildsNewestMatchingTag(t *testing.T) {
	h := newHarness(t)
	app := h.tagApp(t, "web")
	h.cloner.tags = []string{"v1.2.0", "v1.10.0", "v2.0.0-rc.1", "docs"}
	dep := h.claimedDeployment(t, app.ID)

	h.worker.runPipeline(context.Background(), dep)

	got := h.status(t, dep.ID)
	if got.Status != core.StatusRunning {
		t.Fatalf("status = %q (%q), want running", got.Status, got.Reason)
	}
	if got.Tag != "v1.10.0" {
		t.Errorf("Tag = %q, want v1.10.0", got.Tag)
	}
	if len(h.cloner.branches) != 1 || h.cloner.branches[0] != "refs/tags/v1.10.0" {
		t.Errorf("branches = %v", h.cloner.branches)
	}
	if log := h.logOf(dep); !strings.Contains(log, `Deploying the newest tag matching "v*": v1.10.0`) {
		t.Errorf("log:\n%s", log)
	}
}

func TestDeployClickWithNoMatchingTagFails(t *testing.T) {
	h := newHarness(t)
	app := h.tagApp(t, "web")
	h.cloner.tags = []string{}
	dep := h.claimedDeployment(t, app.ID)

	h.worker.runPipeline(context.Background(), dep)

	got := h.status(t, dep.ID)
	if got.Status != core.StatusFailed || got.Reason != `no tag matches "v*"` {
		t.Fatalf("status = %q reason = %q", got.Status, got.Reason)
	}
	if len(h.cloner.branches) != 0 {
		t.Errorf("nothing should be cloned, got %v", h.cloner.branches)
	}
}

func TestDeployClickFallsBackToLiveTag(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	app := h.tagApp(t, "web")
	first := h.claimedTagDeployment(t, app.ID, "vnext")
	h.worker.runPipeline(ctx, first)

	h.cloner.tags = []string{"vnext"}
	dep := h.claimedDeployment(t, app.ID)
	h.worker.runPipeline(ctx, dep)

	got := h.status(t, dep.ID)
	if got.Status != core.StatusRunning {
		t.Fatalf("status = %q (%q), want running", got.Status, got.Reason)
	}
	if n := len(h.cloner.branches); n != 2 || h.cloner.branches[1] != "refs/tags/vnext" {
		t.Errorf("branches = %v", h.cloner.branches)
	}
	if got.Tag != "vnext" {
		t.Errorf("Tag = %q", got.Tag)
	}
	if log := h.logOf(dep); !strings.Contains(log, `No tag matching "v*" looks like a version; redeploying the live tag vnext`) {
		t.Errorf("log:\n%s", log)
	}
}

func TestDeployClickNonVersionTagsNothingLiveFails(t *testing.T) {
	h := newHarness(t)
	app := h.tagApp(t, "web")
	h.cloner.tags = []string{"vnext"}
	dep := h.claimedDeployment(t, app.ID)

	h.worker.runPipeline(context.Background(), dep)

	got := h.status(t, dep.ID)
	if got.Status != core.StatusFailed || !strings.Contains(got.Reason, "looks like a version") {
		t.Fatalf("status = %q reason = %q", got.Status, got.Reason)
	}
	if len(h.cloner.branches) != 0 {
		t.Errorf("nothing should be cloned, got %v", h.cloner.branches)
	}
}

func TestDeployClickListTagsFailureFails(t *testing.T) {
	h := newHarness(t)
	app := h.tagApp(t, "web")
	h.cloner.tagsErr = errors.New("boom")
	dep := h.claimedDeployment(t, app.ID)

	h.worker.runPipeline(context.Background(), dep)

	got := h.status(t, dep.ID)
	if got.Status != core.StatusFailed || got.Reason != "list tags: boom" {
		t.Fatalf("status = %q reason = %q", got.Status, got.Reason)
	}
}

func TestRollbackOfTagDeployDoesNotListTags(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	app := h.tagApp(t, "web")
	built := h.claimedTagDeployment(t, app.ID, "v1.0.0")
	h.worker.runPipeline(ctx, built)
	row := h.status(t, built.ID)
	if row.Status != core.StatusRunning || row.Image == "" {
		t.Fatalf("setup: %+v", row)
	}

	h.cloner.tagsErr = errors.New("remote unreachable")
	dep := h.claimedRollback(t, app.ID, row.Image, built.ID)
	h.worker.runPipeline(ctx, dep)

	if got := h.status(t, dep.ID); got.Status != core.StatusRunning {
		t.Fatalf("status = %q (%q), want running", got.Status, got.Reason)
	}
	if h.cloner.listed != 0 {
		t.Errorf("ListTags called %d times for a rollback", h.cloner.listed)
	}
}

func TestComposeTagAppClonesTag(t *testing.T) {
	h := newHarness(t)
	app := h.composeApp(t, "shop", "docker-compose.yml", "")
	h.tagMode(t, app.ID)
	dep := h.claimedTagDeployment(t, app.ID, "v1.4.0")

	h.worker.runPipeline(context.Background(), dep)

	if got := h.status(t, dep.ID); got.Status != core.StatusRunning {
		t.Fatalf("status = %q (%q), want running", got.Status, got.Reason)
	}
	if len(h.cloner.branches) != 1 || h.cloner.branches[0] != "refs/tags/v1.4.0" {
		t.Errorf("branches = %v", h.cloner.branches)
	}
}

func TestDeployClickLiveTagGoneFromListFails(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	app := h.tagApp(t, "web")
	h.worker.runPipeline(ctx, h.claimedTagDeployment(t, app.ID, "v1.0.0"))

	h.cloner.tags = []string{"nightly"}
	dep := h.claimedDeployment(t, app.ID)
	h.worker.runPipeline(ctx, dep)

	got := h.status(t, dep.ID)
	if got.Status != core.StatusFailed || got.Reason != `no tag matches "v*"` {
		t.Fatalf("status = %q reason = %q", got.Status, got.Reason)
	}
	if log := h.logOf(dep); strings.Contains(log, "redeploying the live tag") {
		t.Errorf("fallback logged although not taken:\n%s", log)
	}
}

func TestDeployClickPrereleaseOnlyFails(t *testing.T) {
	h := newHarness(t)
	app := h.tagApp(t, "web")
	h.cloner.tags = []string{"v2.0.0-rc.1"}
	dep := h.claimedDeployment(t, app.ID)

	h.worker.runPipeline(context.Background(), dep)

	got := h.status(t, dep.ID)
	want := `only pre-release tags match "v*", and this app doesn't deploy pre-releases`
	if got.Status != core.StatusFailed || got.Reason != want {
		t.Fatalf("status = %q reason = %q", got.Status, got.Reason)
	}
}

func TestDeployClickPublicCredentialsHint(t *testing.T) {
	h := newHarness(t)
	app := h.tagApp(t, "web")
	h.cloner.tagsErr = ErrCredentialsRequired
	dep := h.claimedDeployment(t, app.ID)

	h.worker.runPipeline(context.Background(), dep)

	got := h.status(t, dep.ID)
	if got.Status != core.StatusFailed || !strings.Contains(got.Reason, "private or does not exist") {
		t.Fatalf("status = %q reason = %q", got.Status, got.Reason)
	}
}

func TestComposeTagAppMissingFileNamesTag(t *testing.T) {
	h := newHarness(t)
	app := h.composeApp(t, "shop", "missing.yml", "")
	h.cloner.files = nil
	h.tagMode(t, app.ID)
	dep := h.claimedTagDeployment(t, app.ID, "v1.4.0")

	h.worker.runPipeline(context.Background(), dep)

	got := h.status(t, dep.ID)
	if got.Status != core.StatusFailed || !strings.Contains(got.Reason, `tag "v1.4.0"`) || strings.Contains(got.Reason, "branch") {
		t.Fatalf("status = %q reason = %q", got.Status, got.Reason)
	}
}
