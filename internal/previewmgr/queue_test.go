package previewmgr

import (
	"context"
	"testing"
	"time"
)

// Spawning provisions databases synchronously, which can outlast GitHub's
// 10-second delivery timeout; handled on the request, the dropped connection
// cancelled the spawn halfway. Webhooks enqueue instead and Run handles the
// events in arrival order on the manager's own context.
func TestEnqueuedEventIsHandledByRun(t *testing.T) {
	h := newHarness(t)
	parent := h.seedGithubApp(t, "web", "acme/web", nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { h.mgr.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	if err := h.mgr.Enqueue(h.sourceID, prEvent("opened", 42, "feature-x", "acme/web", false)); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := h.st.GetPreviewByPR(context.Background(), parent.ID, 42); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("enqueued pull_request event was never handled")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestEnqueueRefusesWhenQueueIsFull(t *testing.T) {
	h := newHarness(t) // Run not started: nothing drains the queue
	ev := prEvent("opened", 1, "b", "acme/web", false)
	for i := 0; i < queueSize; i++ {
		if err := h.mgr.Enqueue(h.sourceID, ev); err != nil {
			t.Fatalf("Enqueue %d: %v", i, err)
		}
	}
	if err := h.mgr.Enqueue(h.sourceID, ev); err == nil {
		t.Fatal("Enqueue on a full queue returned nil; the event would be silently lost")
	}
}
