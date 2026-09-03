package previewmgr

import (
	"context"
	"testing"

	"github.com/outhaul-dev/outhaul/internal/webhook"
)

// A preview clones the same repo as its parent, so it must carry the parent's
// credentials. This is inherited by the `child := parent` struct copy in spawn
// — the guard matters because losing it fails only at clone time, inside a
// deploy log, long after the mistake.
func TestPreviewChildInheritsParentGitSource(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	parent := h.seedGithubApp(t, "web", "acme-corp/api", nil)

	if err := h.mgr.Handle(ctx, h.sourceID, webhook.PullRequestEvent{
		Action: "opened", Number: 7, BaseRepoFullName: "acme-corp/api", HeadRef: "feat",
	}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	child, err := h.st.GetPreviewByPR(ctx, parent.ID, 7)
	if err != nil {
		t.Fatalf("GetPreviewByPR: %v", err)
	}
	if child.GitSourceID != parent.GitSourceID {
		t.Errorf("preview GitSourceID = %d, want the parent's %d", child.GitSourceID, parent.GitSourceID)
	}
}

// Handle looks up apps scoped to the event's source, so a PR delivered by one
// connected account never spawns previews for another's identically-named repo.
func TestHandleIgnoresAppsFromAnotherSource(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	parent := h.seedGithubApp(t, "web", "acme-corp/api", nil)

	const otherSource = 4242
	if err := h.mgr.Handle(ctx, otherSource, webhook.PullRequestEvent{
		Action: "opened", Number: 7, BaseRepoFullName: "acme-corp/api", HeadRef: "feat",
	}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if _, err := h.st.GetPreviewByPR(ctx, parent.ID, 7); err == nil {
		t.Error("a PR delivered by a different source must not spawn a preview")
	}
}
