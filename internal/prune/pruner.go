// Package prune reclaims the disk Docker eats over time: per-app retention of
// built images (the deployments table says what is safe to delete — never a
// blanket `image prune -a`, which would destroy registry-less rollback),
// dangling-image and build-cache prunes, and crash-leftover work dirs. An
// after-deploy hook keeps each app's tail short; a daily sweep converges
// everything else.
package prune

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/james-smart/outhaul/internal/core"
	"github.com/james-smart/outhaul/internal/cron"
	"github.com/james-smart/outhaul/internal/docker"
	"github.com/james-smart/outhaul/internal/store"
)

// sweepSchedule is when the daily sweep runs (local time): early morning,
// clear of midnight-scheduled backups.
const sweepSchedule = "30 3 * * *"

// buildCacheAge is how long unused build cache survives: long enough to keep
// day-to-day rebuilds fast, short enough that an idle host converges.
const buildCacheAge = 7 * 24 * time.Hour

// staleTempAge is how old a backup staging temp must be before the sweep
// treats it as a crash leftover (live runs hold theirs for minutes).
const staleTempAge = 24 * time.Hour

// imagePattern is the local image namespace Outhaul owns by convention: every
// nixpacks build is tagged outhaul/<app>:<depID>. Compose service images are
// named outhaul-<app>-<service> (no slash) and never match.
const imagePattern = "outhaul/*"

// Pruner owns disk cleanup. The deploy worker calls PruneApp after each
// successful nixpacks deploy; Run sweeps daily.
type Pruner struct {
	store   *store.Store
	docker  docker.Client
	keep    int    // distinct image tags kept per app; 0 disables image pruning
	workDir string // config.WorkDir(), for stale work-dir cleanup
}

// New wires a Pruner. keep is config.ImageKeep; workDir is config.WorkDir().
func New(st *store.Store, dc docker.Client, keep int, workDir string) *Pruner {
	return &Pruner{store: st, docker: dc, keep: keep, workDir: workDir}
}

// Run ticks once a minute until ctx is cancelled and sweeps when the schedule
// matches, same contract as the backup scheduler.
func (p *Pruner) Run(ctx context.Context) {
	schedule, err := cron.Parse(sweepSchedule)
	if err != nil {
		log.Printf("prune: bad sweep schedule %q: %v", sweepSchedule, err)
		return
	}
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			if schedule.Matches(now) {
				p.Sweep(ctx)
			}
		}
	}
}

// PruneApp enforces the retention window for one app's built images: the
// newest keep distinct tags stay, along with any tag an in-flight deployment
// references and the live (newest running) image. Removed tags are flagged on
// every deployment row that bears them. Progress goes to out — during a
// deploy that is the deploy log.
func (p *Pruner) PruneApp(ctx context.Context, app core.App, out io.Writer) error {
	if p.keep <= 0 || app.Kind == core.KindCompose {
		return nil
	}
	deps, err := p.store.ListDeploymentsForApp(ctx, app.ID)
	if err != nil {
		return fmt.Errorf("list deployments: %w", err)
	}
	for _, tag := range pruneCandidates(deps, p.keep) {
		if err := p.docker.RemoveImage(ctx, tag); err != nil {
			// In use, or the daemon said no: the image is still usable, so
			// the row must stay rollback-able. The next sweep retries.
			fmt.Fprintf(out, "WARNING: could not remove old image %s: %v\n", tag, err)
			continue
		}
		if err := p.store.MarkImagePruned(ctx, tag); err != nil {
			return fmt.Errorf("record pruned image %s: %w", tag, err)
		}
		fmt.Fprintf(out, "Pruned old image %s (keeping the %d most recent)\n", tag, p.keep)
	}
	return nil
}

// pruneCandidates returns the tags to remove from an app's deployments
// (newest first, as ListDeploymentsForApp returns them): everything outside
// the newest keep distinct unpruned tags, minus the always-protected set.
// Distinct because rollback rows repeat older tags — one tag on the host can
// back several rows.
func pruneCandidates(deps []core.Deployment, keep int) []string {
	protected := map[string]bool{}
	var distinct []string // newest first
	seen := map[string]bool{}
	liveSeen := false
	for _, d := range deps {
		if d.Image == "" || d.ImagePruned {
			continue
		}
		// In-flight rows (queued rollbacks, mid-build claims) reference tags
		// that must survive; so must the live image, even when an operator
		// shrank the window below the rollback depth in use.
		if !d.Status.IsTerminal() {
			protected[d.Image] = true
		}
		if !liveSeen && d.Status == core.StatusRunning {
			protected[d.Image] = true
			liveSeen = true
		}
		if !seen[d.Image] {
			seen[d.Image] = true
			distinct = append(distinct, d.Image)
		}
	}
	var remove []string
	for i, tag := range distinct {
		if i < keep || protected[tag] {
			continue
		}
		remove = append(remove, tag)
	}
	return remove
}

// Sweep is the daily pass: per-app retention, orphan reconciliation of the
// outhaul/* image namespace, dangling-image and build-cache prunes, and
// stale work-dir cleanup. Failures are logged and the sweep moves on — every
// step retries tomorrow.
func (p *Pruner) Sweep(ctx context.Context) {
	log.Print("prune: sweep starting")
	apps, err := p.store.ListApps(ctx)
	if err != nil {
		log.Printf("prune: list apps: %v", err)
		apps = nil
	}
	logw := logWriter{}
	for _, app := range apps {
		if err := p.PruneApp(ctx, app, logw); err != nil {
			log.Printf("prune: app %s: %v", app.Name, err)
		}
	}
	if p.keep > 0 {
		p.reconcile(ctx)
	}
	if reclaimed, err := p.docker.PruneImages(ctx); err != nil {
		log.Printf("prune: dangling images: %v", err)
	} else if reclaimed > 0 {
		log.Printf("prune: dangling images reclaimed %s", formatBytes(reclaimed))
	}
	if reclaimed, err := p.docker.PruneBuildCache(ctx, buildCacheAge); err != nil {
		log.Printf("prune: build cache: %v", err)
	} else if reclaimed > 0 {
		log.Printf("prune: build cache reclaimed %s", formatBytes(reclaimed))
	}
	p.cleanWorkDir(ctx)
	log.Print("prune: sweep done")
}

// reconcile removes outhaul/* tags no unpruned deployment row references:
// leftovers from failed delete-time removal, rows that vanished with their
// app, and pre-retention-era images. A tag whose deployment is still
// in-flight is skipped — a fresh build's SetImage may not have committed when
// the sweep listed the host.
func (p *Pruner) reconcile(ctx context.Context) {
	tags, err := p.docker.ListImages(ctx, imagePattern)
	if err != nil {
		log.Printf("prune: list images: %v", err)
		return
	}
	retainedList, err := p.store.RetainedImages(ctx)
	if err != nil {
		log.Printf("prune: retained images: %v", err)
		return
	}
	retained := map[string]bool{}
	for _, tag := range retainedList {
		retained[tag] = true
	}
	for _, tag := range tags {
		if retained[tag] || p.tagInFlight(ctx, tag) {
			continue
		}
		if err := p.docker.RemoveImage(ctx, tag); err != nil {
			log.Printf("prune: orphan image %s: %v", tag, err)
			continue
		}
		log.Printf("prune: removed orphan image %s", tag)
	}
}

// tagInFlight reports whether tag names a deployment that is still active.
// Tags are outhaul/<app>:<depID>; anything else in our namespace was not
// written by the pipeline and cannot be in flight.
func (p *Pruner) tagInFlight(ctx context.Context, tag string) bool {
	_, idStr, ok := strings.Cut(tag, ":")
	if !ok {
		return false
	}
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		return false
	}
	dep, err := p.store.GetDeployment(ctx, id)
	return err == nil && !dep.Status.IsTerminal()
}

// cleanWorkDir removes crash leftovers under the work dir: dep-<id> checkouts
// whose deployment is finished (or gone — the pipeline normally removes these
// itself) and day-old backup staging temps.
func (p *Pruner) cleanWorkDir(ctx context.Context) {
	entries, err := os.ReadDir(p.workDir)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("prune: read work dir: %v", err)
		}
		return
	}
	for _, e := range entries {
		full := filepath.Join(p.workDir, e.Name())
		switch {
		case e.IsDir() && strings.HasPrefix(e.Name(), "dep-"):
			id, err := strconv.ParseInt(strings.TrimPrefix(e.Name(), "dep-"), 10, 64)
			if err != nil {
				continue
			}
			dep, err := p.store.GetDeployment(ctx, id)
			if err == nil && !dep.Status.IsTerminal() {
				continue // a pipeline is (or will be) using it
			}
			if err := os.RemoveAll(full); err != nil {
				log.Printf("prune: stale work dir %s: %v", e.Name(), err)
			} else {
				log.Printf("prune: removed stale work dir %s", e.Name())
			}
		case !e.IsDir() && strings.HasPrefix(e.Name(), "backup-"):
			info, err := e.Info()
			if err != nil || time.Since(info.ModTime()) < staleTempAge {
				continue
			}
			if err := os.Remove(full); err != nil {
				log.Printf("prune: stale temp %s: %v", e.Name(), err)
			}
		}
	}
}

// logWriter adapts the sweep's PruneApp calls onto the process log, matching
// how the rest of the sweep reports.
type logWriter struct{}

func (logWriter) Write(p []byte) (int, error) {
	if s := strings.TrimRight(string(p), "\n"); s != "" {
		log.Printf("prune: %s", s)
	}
	return len(p), nil
}

// formatBytes renders a byte count the way `docker system prune` does.
func formatBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGT"[exp])
}
