package previewmgr

import (
	"context"
	"testing"
	"time"
)

func TestSweeperReapsStalePreview(t *testing.T) {
	h := newHarness(t)
	parent := h.seedGithubApp(t, "web", "acme/web", nil) // IdleTTLDays defaults 7
	if err := h.mgr.Handle(context.Background(), prEvent("opened", 42, "feature-x", "acme/web", false)); err != nil {
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
}

func TestSweeperKeepsFreshPreview(t *testing.T) {
	h := newHarness(t)
	parent := h.seedGithubApp(t, "web", "acme/web", nil)
	if err := h.mgr.Handle(context.Background(), prEvent("opened", 42, "feature-x", "acme/web", false)); err != nil {
		t.Fatal(err)
	}
	h.mgr.SweepTick(context.Background(), time.Now()) // deployment is ~now, within TTL
	if _, err := h.st.GetPreviewByPR(context.Background(), parent.ID, 42); err != nil {
		t.Fatal("fresh preview should NOT have been reaped")
	}
}
