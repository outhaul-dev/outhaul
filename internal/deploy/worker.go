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

	"github.com/outhaul-dev/outhaul/internal/builder"
	"github.com/outhaul-dev/outhaul/internal/compose"
	"github.com/outhaul-dev/outhaul/internal/config"
	"github.com/outhaul-dev/outhaul/internal/core"
	"github.com/outhaul-dev/outhaul/internal/docker"
	"github.com/outhaul-dev/outhaul/internal/gitsource"
	"github.com/outhaul-dev/outhaul/internal/logstream"
	"github.com/outhaul-dev/outhaul/internal/store"
	"github.com/outhaul-dev/outhaul/internal/traefik"
)

// pollInterval is a safety-net re-check even if no Notify arrives.
const pollInterval = 3 * time.Second

// AppPruner is the optional after-deploy image-retention hook (implemented by
// internal/prune, which stays out of the pipeline's dependency graph).
type AppPruner interface {
	PruneApp(ctx context.Context, app core.App, out io.Writer) error
}

// DeployHook is called after a deployment reaches a terminal state, with the
// app and whether it succeeded. Optional; nil disables it. Used by the preview
// manager to update a preview's PR comment + status.
type DeployHook func(ctx context.Context, app core.App, success bool)

// Builders holds one build strategy per single-container app kind; the
// pipeline picks by app.Kind (compose stacks build via docker compose, not a
// Builder).
type Builders struct {
	Nixpacks   builder.Builder
	Dockerfile builder.Builder
}

// Worker dispatches and runs deployments.
type Worker struct {
	store    *store.Store
	docker   docker.Client
	builders Builders
	compose  compose.Runner
	cloner   Cloner
	broker   *logstream.Broker
	sources  *gitsource.Registry // providers for the git sources apps clone from
	cfg      config.Config

	healthCheck HealthChecker
	pruner      AppPruner   // nil disables after-deploy image pruning
	deployHook  DeployHook  // nil disables the after-deploy preview hook
	hold        func() bool // when it returns true, dispatch claims nothing

	notify chan struct{}
	sem    chan struct{}
	wg     sync.WaitGroup

	// buildSlots caps how many deployments that must build are in building at
	// once; nil means no cap. buildHeld names the deployments that hold a slot
	// (a claimed image deployment gives it straight back).
	buildSlots chan struct{}
	buildHeld  map[int64]bool

	// claim is store.ClaimDeployment, replaceable in tests.
	claim func(ctx context.Context, id int64) (bool, error)

	mu      sync.Mutex
	cancels map[int64]context.CancelFunc // per-deployment pipeline cancels
}

// NewWorker wires the worker's dependencies.
func NewWorker(st *store.Store, dc docker.Client, b Builders, cp compose.Runner, cl Cloner, br *logstream.Broker, sources *gitsource.Registry, cfg config.Config) *Worker {
	w := &Worker{
		store:    st,
		docker:   dc,
		builders: b,
		compose:  cp,
		cloner:   cl,
		broker:   br,
		sources:  sources,
		cfg:      cfg,

		healthCheck: httpPoll,

		notify:    make(chan struct{}, 1),
		sem:       make(chan struct{}, core.MaxPipelines),
		buildHeld: map[int64]bool{},
		claim:     st.ClaimDeployment,
		cancels:   map[int64]context.CancelFunc{},
	}
	if cfg.MaxConcurrentBuilds > 0 {
		w.buildSlots = make(chan struct{}, cfg.MaxConcurrentBuilds)
	}
	return w
}

// holdBuild records that a claimed build holds a slot.
func (w *Worker) holdBuild(id int64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buildHeld[id] = true
}

// releaseBuild gives back the build slot a deployment holds, if any. It is
// idempotent: a deployment that never built (a rollback) is a no-op, and the
// safety-net calls at the end of a pipeline cannot double-release.
func (w *Worker) releaseBuild(id int64) {
	if w.buildSlots == nil {
		return
	}
	w.mu.Lock()
	held := w.buildHeld[id]
	delete(w.buildHeld, id)
	w.mu.Unlock()
	if !held {
		return
	}
	<-w.buildSlots
	w.Notify()
}

// limits is the app's effective limit: its stored values over the server
// defaults, field by field. A preview stores none, so it reads its parent's at
// each deploy; a parent that has gone is logged and the default applies.
func (w *Worker) limits(ctx context.Context, app core.App) core.Limits {
	stored := app.Limits
	if app.ParentID != 0 {
		if parent, err := w.store.GetApp(ctx, app.ParentID); err == nil {
			stored = parent.Limits
		} else {
			log.Printf("deploy: read preview parent %d: %v", app.ParentID, err)
		}
	}
	return stored.Or(w.cfg.DefaultLimits)
}

// needsModel reports whether any setting needs the normalised Compose model:
// the strict container policy, a required request header, an effective limit,
// or the restart policy.
func (w *Worker) needsModel(ctx context.Context, app core.App) bool {
	return w.cfg.StrictContainerPolicy() || w.cfg.HeaderRequired() ||
		w.limits(ctx, app) != (core.Limits{}) ||
		w.cfg.ComposeRestart != ""
}

// routing builds the server-wide routing settings for a deploy. A Cloudflare
// Tunnel terminates TLS at Cloudflare's edge and Traefik serves plain HTTP
// behind it, so an enabled tunnel forces HTTPS routers off even when ACME is
// configured. On a read error we assume no tunnel (the common case) and log.
func (w *Worker) routing(ctx context.Context) traefik.Routing {
	on, err := w.store.TunnelEnabled(ctx)
	if err != nil {
		log.Printf("deploy: could not read tunnel setting, assuming HTTPS on: %v", err)
		return traefik.RoutingFrom(w.cfg, false)
	}
	return traefik.RoutingFrom(w.cfg, on)
}

// SetPruner installs the after-deploy image-retention hook. Call before Run.
func (w *Worker) SetPruner(p AppPruner) { w.pruner = p }

// SetDeployHook installs the after-deploy hook. Call before Run.
func (w *Worker) SetDeployHook(h DeployHook) { w.deployHook = h }

// SetHold installs a predicate that pauses dispatch: while it returns true,
// no queued deployment is claimed (rows stay queued, running pipelines are
// untouched). Used to hold work during an update. Call before Run.
func (w *Worker) SetHold(hold func() bool) { w.hold = hold }

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
		if w.hold != nil && w.hold() {
			return
		}
		// Acquire a slot before claiming so a claimed deployment never sits in
		// building with no pipeline running.
		select {
		case w.sem <- struct{}{}:
		case <-ctx.Done():
			return
		}

		// Take a build slot when one is free; otherwise offer the queue only
		// deployments that already have an image. The slot is held by the
		// deployment until it leaves building, not just until this claims.
		canBuild := w.buildSlots == nil
		buildTaken := false
		if !canBuild {
			select {
			case w.buildSlots <- struct{}{}:
				canBuild, buildTaken = true, true
			default:
			}
		}
		releaseSlots := func() {
			<-w.sem
			if buildTaken {
				<-w.buildSlots
			}
		}

		dep, err := w.store.NextClaimable(ctx, canBuild)
		if err != nil {
			log.Printf("deploy: NextClaimable: %v", err)
			releaseSlots()
			return
		}
		if dep == nil {
			releaseSlots()
			return
		}
		if !canBuild && dep.Image == "" {
			// The query never offers a build without a slot; claiming it would
			// leave the row building with no pipeline.
			log.Printf("deploy: NextClaimable offered build %d with no build slot; skipping", dep.ID)
			releaseSlots()
			return
		}

		ok, err := w.claim(ctx, dep.ID)
		if err != nil {
			log.Printf("deploy: ClaimDeployment(%d): %v", dep.ID, err)
			releaseSlots()
			return
		}
		if !ok {
			// Lost the race (shouldn't happen with a single dispatcher, but be
			// safe); release and try the next claimable.
			releaseSlots()
			continue
		}
		dep.Status = core.StatusBuilding
		if dep.Image != "" {
			// Nothing to build: give the slot back now.
			if buildTaken {
				<-w.buildSlots
			}
		} else {
			w.holdBuild(dep.ID)
		}
		buildTaken = false // owned by the deployment (or already given back)

		pctx, cancel := context.WithCancel(ctx)
		w.register(dep.ID, cancel)

		w.wg.Add(1)
		go func(d core.Deployment) {
			defer w.wg.Done()
			defer func() { <-w.sem }()
			defer w.unregister(d.ID)
			defer cancel()
			defer w.releaseBuild(d.ID) // safety net; the pipelines release on leaving building

			w.runPipeline(pctx, d)
			w.notifyDeployHook(d.ID) // update a preview's PR comment + status
			w.Notify()               // the app is now free; re-check for queued work
		}(*dep)
	}
}

// notifyDeployHook fires the after-deploy hook once a pipeline run has settled,
// keyed off the deployment's PERSISTED terminal status (the source of truth):
// running => success, failed => failure. Cancelled/superseded (or any
// non-terminal) status is skipped so a cancelled preview deploy never clobbers a
// good "ready" comment, and a deploy whose app was deleted mid-flight fires
// nothing. Uses context.Background() because the pipeline ctx may already be
// cancelled (graceful shutdown) — a failed/cancelled-by-shutdown deploy must
// still be able to post its comment.
func (w *Worker) notifyDeployHook(depID int64) {
	if w.deployHook == nil {
		return
	}
	ctx := context.Background()
	dep, err := w.store.GetDeployment(ctx, depID)
	if err != nil {
		log.Printf("deploy: deploy hook load deployment %d: %v", depID, err)
		return
	}
	var success bool
	switch dep.Status {
	case core.StatusRunning:
		success = true
	case core.StatusFailed:
		success = false
	default:
		return // cancelled / superseded / non-terminal: leave any existing state
	}
	app, err := w.store.GetApp(ctx, dep.AppID)
	if err != nil {
		return // app deleted mid-deploy: nothing to update
	}
	w.deployHook(ctx, app, success)
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
