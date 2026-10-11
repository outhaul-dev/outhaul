package previewmgr

import (
	"context"
	"errors"
	"log"

	"github.com/outhaul-dev/outhaul/internal/webhook"
)

// queueSize bounds the pull_request events waiting for Run. A full queue refuses
// new events so the webhook can fail the delivery (GitHub keeps it for
// redelivery) instead of dropping it silently.
const queueSize = 64

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
	select {
	case m.events <- prJob{sourceID: sourceID, ev: ev}:
		return nil
	default:
		return errQueueFull
	}
}

func (m *Manager) handleJob(ctx context.Context, j prJob) {
	if err := m.Handle(ctx, j.sourceID, j.ev); err != nil {
		log.Printf("previewmgr: PR %s#%d %s: %v", j.ev.BaseRepoFullName, j.ev.Number, j.ev.Action, err)
	}
}
