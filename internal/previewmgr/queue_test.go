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

// Only opened/reopened/synchronize/closed do anything. labeled, edited,
// review_requested and the rest must not take queue slots a closed event needs.
func TestEnqueueSkipsActionsPreviewsIgnore(t *testing.T) {
	h := newHarness(t) // Run not started
	for i := 0; i < queueSize+5; i++ {
		if err := h.mgr.Enqueue(h.sourceID, prEvent("labeled", 1, "b", "acme/web", false)); err != nil {
			t.Fatalf("Enqueue labeled %d: %v", i, err)
		}
	}
	if n := len(h.mgr.events); n != 0 {
		t.Fatalf("queued %d ignorable events, want 0", n)
	}
}

// One stalled spawn (an image pull that never finishes) used to hold the single
// consumer forever, freezing every other PR event and the sweeper. Each job now
// has a deadline, and the half-built preview is still rolled back after it.
func TestHungSpawnDoesNotStallTheQueue(t *testing.T) {
	h := newHarness(t)
	h.mgr.jobTimeout = 100 * time.Millisecond
	stuck := h.seedGithubApp(t, "web", "acme/web", nil)
	h.seedAttachment(t, stuck, "web-db", "DATABASE_URL")
	h.dbprov.hang = true
	next := h.seedGithubApp(t, "api", "acme/api", nil) // no databases to provision

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { h.mgr.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	if err := h.mgr.Enqueue(h.sourceID, prEvent("opened", 1, "f", "acme/web", false)); err != nil {
		t.Fatal(err)
	}
	if err := h.mgr.Enqueue(h.sourceID, prEvent("opened", 2, "g", "acme/api", false)); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := h.st.GetPreviewByPR(context.Background(), next.ID, 2); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the event behind a hung spawn was never handled")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := h.st.GetPreviewByPR(context.Background(), stuck.ID, 1); err == nil {
		t.Error("the timed-out spawn left its half-built preview behind")
	}
}
