package previewmgr

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"time"

	"github.com/outhaul-dev/outhaul/internal/core"
)

// Run handles enqueued pull_request events and sweeps once an hour until ctx is
// cancelled, tearing down previews whose newest deployment is older than their
// parent's IdleTTLDays. Events and sweeps share this one goroutine, so they
// never touch the same preview concurrently.
func (m *Manager) Run(ctx context.Context) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	// Sweep once up front: previews orphaned before an upgrade, or whose PR
	// closed while the server was down, shouldn't wait an hour.
	m.SweepTick(ctx, time.Now())
	for {
		select {
		case <-ctx.Done():
			return
		case j := <-m.events:
			m.handleJob(ctx, j)
		case now := <-t.C:
			m.SweepTick(ctx, now)
		}
	}
}

// SweepTick tears down previews whose newest deployment is older than the
// parent's IdleTTLDays, previews whose PR GitHub reports closed (a backstop for
// close events that never landed: lost in a restart, refused by a full queue,
// or overtaken by a late synchronize), and previews whose parent no longer
// exists (left behind by parent deletes from before those tore their previews
// down). now is passed in so tests are deterministic.
func (m *Manager) SweepTick(ctx context.Context, now time.Time) {
	previews, err := m.store.ListPreviews(ctx)
	if err != nil {
		log.Printf("previewmgr: sweep list: %v", err)
		return
	}
	for _, p := range previews {
		parent, err := m.store.GetApp(ctx, p.ParentID)
		if errors.Is(err, sql.ErrNoRows) {
			if err := m.destroyChild(ctx, p); err != nil {
				log.Printf("previewmgr: sweep orphan %s: %v", p.Name, err)
			}
			continue
		}
		if err != nil {
			continue
		}
		cfg, err := m.store.GetPreviewConfig(ctx, p.ParentID)
		if err != nil {
			continue
		}
		if m.prClosed(ctx, parent, p.PRNumber) {
			if err := m.teardown(ctx, parent, p.PRNumber, parent.GithubRepo, cfg); err != nil {
				log.Printf("previewmgr: sweep teardown %s (PR closed): %v", p.Name, err)
			}
			continue
		}
		if cfg.IdleTTLDays <= 0 {
			continue
		}
		last, ok, err := m.store.LastDeploymentAt(ctx, p.ID)
		if err != nil || !ok {
			continue
		}
		if now.Sub(last) > time.Duration(cfg.IdleTTLDays)*24*time.Hour {
			if err := m.teardown(ctx, parent, p.PRNumber, parent.GithubRepo, cfg); err != nil {
				log.Printf("previewmgr: sweep teardown %s: %v", p.Name, err)
			}
		}
	}
}

// prClosed reports whether GitHub says parent's PR pr is closed. Anything short
// of a definite "closed" — no token source, a lookup error — keeps the preview.
func (m *Manager) prClosed(ctx context.Context, parent core.App, pr int) bool {
	if m.token == nil || parent.GithubRepo == "" {
		return false
	}
	tok, ok, err := m.token(ctx, parent.GitSourceID)
	if err != nil || !ok {
		return false
	}
	state, err := m.gh.PullRequestState(ctx, tok, parent.GithubRepo, pr)
	if err != nil {
		log.Printf("previewmgr: sweep PR state %s#%d: %v", parent.GithubRepo, pr, err)
		return false
	}
	return state == "closed"
}
