package previewmgr

import (
	"context"
	"log"
	"time"
)

// Run sweeps once an hour until ctx is cancelled, tearing down previews whose
// newest deployment is older than their parent's IdleTTLDays.
func (m *Manager) Run(ctx context.Context) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			m.SweepTick(ctx, now)
		}
	}
}

// SweepTick tears down previews whose newest deployment is older than the
// parent's IdleTTLDays. now is passed in so tests are deterministic.
func (m *Manager) SweepTick(ctx context.Context, now time.Time) {
	previews, err := m.store.ListPreviews(ctx)
	if err != nil {
		log.Printf("previewmgr: sweep list: %v", err)
		return
	}
	for _, p := range previews {
		cfg, err := m.store.GetPreviewConfig(ctx, p.ParentID)
		if err != nil || cfg.IdleTTLDays <= 0 {
			continue
		}
		last, ok, err := m.store.LastDeploymentAt(ctx, p.ID)
		if err != nil || !ok {
			continue
		}
		if now.Sub(last) > time.Duration(cfg.IdleTTLDays)*24*time.Hour {
			parent, err := m.store.GetApp(ctx, p.ParentID)
			if err != nil {
				continue
			}
			if err := m.teardown(ctx, parent, p.PRNumber, parent.GithubRepo, cfg); err != nil {
				log.Printf("previewmgr: sweep teardown %s: %v", p.Name, err)
			}
		}
	}
}
