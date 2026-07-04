// Package deploy contains Outhaul's background worker: an in-process dispatcher
// that turns queued deployments into running containers. Deploys for the same
// app serialize; different apps run concurrently. The deployments table is the
// queue (see internal/store); this package owns the pipeline that advances a
// claimed deployment through the state machine.
package deploy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"sync"
	"time"

	"github.com/james-smart/outhaul/internal/builder"
	"github.com/james-smart/outhaul/internal/compose"
	"github.com/james-smart/outhaul/internal/config"
	"github.com/james-smart/outhaul/internal/core"
	"github.com/james-smart/outhaul/internal/docker"
	"github.com/james-smart/outhaul/internal/github"
	"github.com/james-smart/outhaul/internal/logstream"
	"github.com/james-smart/outhaul/internal/store"
)

// pollInterval is a safety-net re-check even if no Notify arrives.
const pollInterval = 3 * time.Second

// maxConcurrent bounds how many app deployments build/deploy at once.
const maxConcurrent = 4

// AppPruner is the optional after-deploy image-retention hook (implemented by
// internal/prune, which stays out of the pipeline's dependency graph).
type AppPruner interface {
	PruneApp(ctx context.Context, app core.App, out io.Writer) error
}

// Worker dispatches and runs deployments.
type Worker struct {
	store   *store.Store
	docker  docker.Client
	builder builder.Builder
	compose compose.Runner
	cloner  Cloner
	broker  *logstream.Broker
	gh      github.Client
	cfg     config.Config

	healthCheck HealthChecker
	pruner      AppPruner // nil disables after-deploy image pruning

	notify chan struct{}
	sem    chan struct{}
	wg     sync.WaitGroup

	mu      sync.Mutex
	cancels map[int64]context.CancelFunc // per-deployment pipeline cancels
}

// NewWorker wires the worker's dependencies.
func NewWorker(st *store.Store, dc docker.Client, b builder.Builder, cp compose.Runner, cl Cloner, br *logstream.Broker, gh github.Client, cfg config.Config) *Worker {
	return &Worker{
		store:   st,
		docker:  dc,
		builder: b,
		compose: cp,
		cloner:  cl,
		broker:  br,
		gh:      gh,
		cfg:     cfg,

		healthCheck: httpPoll,

		notify:  make(chan struct{}, 1),
		sem:     make(chan struct{}, maxConcurrent),
		cancels: map[int64]context.CancelFunc{},
	}
}

// SetPruner installs the after-deploy image-retention hook. Call before Run.
func (w *Worker) SetPruner(p AppPruner) { w.pruner = p }

// Notify wakes the dispatcher to look for claimable work. Non-blocking.
func (w *Worker) Notify() {
	select {
	case w.notify <- struct{}{}:
	default:
	}
}

// Run is the dispatcher loop. It claims and launches deployments until ctx is
// cancelled, then waits for in-flight pipelines to observe the cancellation and
// finish (their guarded status writes persist via a background context).
func (w *Worker) Run(ctx context.Context) {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		w.dispatch(ctx)

		select {
		case <-ctx.Done():
			w.wg.Wait()
			return
		case <-w.notify:
		case <-ticker.C:
		}
	}
}

// dispatch claims as many currently-claimable deployments as concurrency allows
// and launches a pipeline goroutine for each. Claiming happens only here (single
// goroutine), so there is no claim race between iterations.
func (w *Worker) dispatch(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		// Acquire a slot before claiming so a claimed deployment never sits in
		// building with no pipeline running.
		select {
		case w.sem <- struct{}{}:
		case <-ctx.Done():
			return
		}

		dep, err := w.store.NextClaimable(ctx)
		if err != nil {
			log.Printf("deploy: NextClaimable: %v", err)
			<-w.sem
			return
		}
		if dep == nil {
			<-w.sem
			return
		}

		ok, err := w.store.ClaimDeployment(ctx, dep.ID)
		if err != nil {
			log.Printf("deploy: ClaimDeployment(%d): %v", dep.ID, err)
			<-w.sem
			return
		}
		if !ok {
			// Lost the race (shouldn't happen with a single dispatcher, but be
			// safe); release and try the next claimable.
			<-w.sem
			continue
		}
		dep.Status = core.StatusBuilding

		pctx, cancel := context.WithCancel(ctx)
		w.register(dep.ID, cancel)

		w.wg.Add(1)
		go func(d core.Deployment) {
			defer w.wg.Done()
			defer func() { <-w.sem }()
			defer w.unregister(d.ID)
			defer cancel()

			w.runPipeline(pctx, d)
			w.Notify() // the app is now free; re-check for queued work
		}(*dep)
	}
}

// Cancel cancels a deployment if its status allows it (queued or building):
// it flips the row to cancelled and interrupts any running pipeline. Returns
// false if the deployment is not in a cancellable state.
func (w *Worker) Cancel(ctx context.Context, id int64) (bool, error) {
	dep, err := w.store.GetDeployment(ctx, id)
	if err != nil {
		return false, err
	}
	if !dep.Status.CanCancel() {
		return false, nil
	}

	ok, err := w.store.SetStatus(ctx, id, dep.Status, core.StatusCancelled, "cancelled by operator")
	if err != nil || !ok {
		return ok, err
	}

	// Interrupt the pipeline if one is running (building). The pipeline's
	// guarded status writes will no-op against the now-cancelled row.
	w.interrupt(id)
	w.broker.Publish(id, "Deployment cancelled by operator.")
	w.broker.Close(id)
	return true, nil
}

func (w *Worker) register(id int64, cancel context.CancelFunc) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.cancels[id] = cancel
}

func (w *Worker) unregister(id int64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.cancels, id)
}

func (w *Worker) interrupt(id int64) {
	w.mu.Lock()
	cancel := w.cancels[id]
	w.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// --- logging: bridge writer -> broker ---

// logWriter returns an io.Writer that publishes complete lines to the broker
// for the given deployment.
func (w *Worker) logWriter(id int64) io.Writer {
	return &brokerWriter{broker: w.broker, id: id}
}

// brokerWriter splits writes into lines and publishes each complete line. A
// trailing partial line is buffered until the next newline.
type brokerWriter struct {
	broker *logstream.Broker
	id     int64
	mu     sync.Mutex
	buf    bytes.Buffer
}

func (bw *brokerWriter) Write(p []byte) (int, error) {
	bw.mu.Lock()
	defer bw.mu.Unlock()
	bw.buf.Write(p)
	for {
		line, err := bw.buf.ReadString('\n')
		if err != nil {
			// No complete line; keep the remainder for next time.
			bw.buf.WriteString(line)
			break
		}
		bw.broker.Publish(bw.id, trimNewline(line))
	}
	return len(p), nil
}

func trimNewline(s string) string {
	if n := len(s); n > 0 && s[n-1] == '\n' {
		s = s[:n-1]
	}
	if n := len(s); n > 0 && s[n-1] == '\r' {
		s = s[:n-1]
	}
	return s
}

// logf writes a formatted line (with newline) to out.
func logf(out io.Writer, format string, args ...any) {
	fmt.Fprintf(out, format+"\n", args...)
}
