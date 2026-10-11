package previewmgr

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/outhaul-dev/outhaul/internal/webhook"
)

// queueSize bounds the pull_request events waiting for Run. A full queue refuses
// new events so the webhook fails the delivery — it then shows as failed in the
// App's Recent Deliveries on GitHub, where it can be redelivered by hand —
// instead of acknowledging and dropping it.
const queueSize = 64

// defaultJobTimeout bounds one event's handling. A spawn provisions databases,
// which can pull an image; without a deadline a stalled pull would hold the
// single consumer, and with it every other PR event and the sweeper, forever.
const defaultJobTimeout = 15 * time.Minute

// handledActions are the pull_request actions handleApp acts on. Everything
// else (labeled, edited, review_requested, …) is dropped at Enqueue so it never
// takes a queue slot a closed event needs.
var handledActions = map[string]bool{"opened": true, "reopened": true, "synchronize": true, "closed": true}

// errQueueFull is returned by Enqueue when Run has fallen queueSize events behind.
var errQueueFull = errors.New("preview event queue is full")

type prJob struct {
	sourceID int64
	ev       webhook.PullRequestEvent
}

// Enqueue hands a pull_request event to Run without waiting for it. Spawning a
// preview provisions its databases synchronously, which can outlast GitHub's
// 10-second delivery timeout; handled on the webhook request, the dropped
// connection cancelled the spawn halfway. Run handles events one at a time, in
// arrival order, so a PR's opened/synchronize/closed never race each other.
func (m *Manager) Enqueue(sourceID int64, ev webhook.PullRequestEvent) error {
	if !handledActions[ev.Action] {
		return nil
	}
	select {
	case m.events <- prJob{sourceID: sourceID, ev: ev}:
		return nil
	default:
		return errQueueFull
	}
}

func (m *Manager) handleJob(ctx context.Context, j prJob) {
	ctx, cancel := context.WithTimeout(ctx, m.jobTimeout)
	defer cancel()
	if err := m.Handle(ctx, j.sourceID, j.ev); err != nil {
		log.Printf("previewmgr: PR %s#%d %s: %v", j.ev.BaseRepoFullName, j.ev.Number, j.ev.Action, err)
	}
}
