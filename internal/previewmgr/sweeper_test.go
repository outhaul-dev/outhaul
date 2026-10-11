package previewmgr

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestSweeperReapsStalePreview(t *testing.T) {
	h := newHarness(t)
	parent := h.seedGithubApp(t, "web", "acme/web", nil) // IdleTTLDays defaults 7
	if err := h.mgr.Handle(context.Background(), h.sourceID, prEvent("opened", 42, "feature-x", "acme/web", false)); err != nil {
		t.Fatal(err)
	}
	child, err := h.st.GetPreviewByPR(context.Background(), parent.ID, 42)
	if err != nil {
		t.Fatalf("child missing before sweep: %v", err)
	}

	// The spawned child got a deployment at ~now. Sweep as if 8 days later.
	future := time.Now().Add(8 * 24 * time.Hour)
	h.mgr.SweepTick(context.Background(), future)

	if _, err := h.st.GetPreviewByPR(context.Background(), parent.ID, 42); err == nil {
		t.Fatal("stale preview should have been reaped")
	}
	if !h.docker.removed[child.Name] {
		t.Errorf("docker.RemoveApp not called for %q during sweep", child.Name)
	}
	// An idle-reaped preview posts the "destroyed" comment via the parent's repo.
	got := h.gh.comments["acme/web#42"]
	if len(got) == 0 || got[len(got)-1] != "Preview environment destroyed." {
		t.Errorf("sweep comment = %v, want a \"Preview environment destroyed.\" notice", got)
	}
}

func TestSweeperKeepsFreshPreview(t *testing.T) {
	h := newHarness(t)
	parent := h.seedGithubApp(t, "web", "acme/web", nil)
	if err := h.mgr.Handle(context.Background(), h.sourceID, prEvent("opened", 42, "feature-x", "acme/web", false)); err != nil {
		t.Fatal(err)
	}
	h.mgr.SweepTick(context.Background(), time.Now()) // deployment is ~now, within TTL
	if _, err := h.st.GetPreviewByPR(context.Background(), parent.ID, 42); err != nil {
		t.Fatal("fresh preview should NOT have been reaped")
	}
}

// Before parent deletes tore their previews down, deleting a parent left its
// children running with copies of its secrets, and the sweeper skipped them
// because the parent lookup failed. An orphan is reaped on the next sweep,
// whatever its age.
func TestSweeperReapsOrphanedPreview(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	parent := h.seedGithubApp(t, "web", "acme/web", nil)
	h.seedAttachment(t, parent, "web-db", "DATABASE_URL")
	if err := h.mgr.Handle(ctx, h.sourceID, prEvent("opened", 42, "feature-x", "acme/web", false)); err != nil {
		t.Fatal(err)
	}
	child, err := h.st.GetPreviewByPR(ctx, parent.ID, 42)
	if err != nil {
		t.Fatalf("child missing: %v", err)
	}
	if err := h.st.DeleteApp(ctx, parent.ID); err != nil { // the old, child-blind delete
		t.Fatalf("DeleteApp parent: %v", err)
	}

	h.mgr.SweepTick(ctx, time.Now()) // fresh deployment: only orphan-ness can reap it

	if _, err := h.st.GetApp(ctx, child.ID); err == nil {
		t.Fatal("orphaned preview was not reaped")
	}
	if !h.docker.removed[child.Name] {
		t.Errorf("docker.RemoveApp not called for %q", child.Name)
	}
	if h.dbprov.destroyed != 1 {
		t.Errorf("preview databases destroyed = %d, want 1", h.dbprov.destroyed)
	}
}

// A closed PR whose close event never landed — lost in a restart, refused by a
// full queue, or overtaken by a late synchronize that respawned the preview —
// kept a public preview, with copies of the parent's secrets, until IdleTTL.
// The sweep checks each preview's PR on GitHub and tears down closed ones.
func TestSweeperReapsPreviewOfClosedPR(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	parent := h.seedGithubApp(t, "web", "acme/web", nil)
	if err := h.mgr.Handle(ctx, h.sourceID, prEvent("opened", 42, "feature-x", "acme/web", false)); err != nil {
		t.Fatal(err)
	}
	h.gh.prStates = map[string]string{"acme/web#42": "closed"}

	h.mgr.SweepTick(ctx, time.Now()) // fresh deployment: only the PR state can reap it

	if _, err := h.st.GetPreviewByPR(ctx, parent.ID, 42); err == nil {
		t.Fatal("preview of a closed PR survived the sweep")
	}
}

func TestSweeperKeepsPreviewWhenPRStateUnknown(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	parent := h.seedGithubApp(t, "web", "acme/web", nil)
	if err := h.mgr.Handle(ctx, h.sourceID, prEvent("opened", 42, "feature-x", "acme/web", false)); err != nil {
		t.Fatal(err)
	}
	h.gh.stateErr = errors.New("github unavailable")

	h.mgr.SweepTick(ctx, time.Now())

	if _, err := h.st.GetPreviewByPR(ctx, parent.ID, 42); err != nil {
		t.Fatal("a failed PR-state lookup must not tear the preview down")
	}
}

// The first sweep used to wait an hour after boot, so previews orphaned before
// an upgrade (or closed while the server was down) lingered that long.
func TestRunSweepsOnStart(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	parent := h.seedGithubApp(t, "web", "acme/web", nil)
	if err := h.mgr.Handle(ctx, h.sourceID, prEvent("opened", 42, "feature-x", "acme/web", false)); err != nil {
		t.Fatal(err)
	}
	h.gh.prStates = map[string]string{"acme/web#42": "closed"}

	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { h.mgr.Run(runCtx); close(done) }()
	defer func() { cancel(); <-done }()

	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := h.st.GetPreviewByPR(ctx, parent.ID, 42); err != nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("Run did not sweep on start")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Backstop for the settings page: a fork preview left running after "Allow
// fork PRs" was turned off (its teardown there failed) goes on the next sweep.
func TestSweeperReapsForkPreviewWhenForksDisabled(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	parent := h.seedGithubApp(t, "web", "acme/web", allowForks)
	if err := h.mgr.Handle(ctx, h.sourceID, prEvent("opened", 7, "patch-1", "acme/web", true)); err != nil {
		t.Fatal(err)
	}
	if err := h.mgr.Handle(ctx, h.sourceID, prEvent("opened", 8, "feature-x", "acme/web", false)); err != nil {
		t.Fatal(err)
	}
	cfg, _ := h.st.GetPreviewConfig(ctx, parent.ID)
	cfg.AllowForkPRs = false
	if err := h.st.SetPreviewConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}

	h.mgr.SweepTick(ctx, time.Now())

	if _, err := h.st.GetPreviewByPR(ctx, parent.ID, 7); err == nil {
		t.Error("fork preview survived the sweep with forks disabled")
	}
	if _, err := h.st.GetPreviewByPR(ctx, parent.ID, 8); err != nil {
		t.Errorf("same-repo preview was reaped: %v", err)
	}
}
