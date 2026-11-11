package server

import (
	"context"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/outhaul-dev/outhaul/internal/compose"
	"github.com/outhaul-dev/outhaul/internal/core"
	"github.com/outhaul-dev/outhaul/internal/dbaas"
)

// RunRestorePass runs the first-start pass while the marker is pending, once a
// minute, until it has run to its end or ctx ends. A step that fails globally
// is logged and retried; a per-app or per-database failure is recorded in the
// notice and the pass continues.
func (s *Server) RunRestorePass(ctx context.Context) {
	every := s.restoreEvery
	if every <= 0 {
		every = time.Minute
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		m, ok, err := s.store.RestoreMarker(ctx)
		if err != nil {
			log.Printf("restore pass: read marker: %v", err)
			return
		}
		if !ok || !m.Pending {
			return
		}
		if err := s.runRestorePassOnce(ctx, m); err != nil {
			log.Printf("restore pass: %v; retrying", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// runRestorePassOnce re-creates what is missing, in order: images, managed
// databases, then apps (ordinary apps before previews). It saves the marker
// after every step, so a retry keeps what already happened.
func (s *Server) runRestorePassOnce(ctx context.Context, m core.RestoreMarker) error {
	save := func() error {
		m.Databases = dedupeStrings(m.Databases)
		m.Queued = dedupeStrings(m.Queued)
		m.Failed = dedupeStrings(m.Failed)
		m.WaitingPush = dedupeStrings(m.WaitingPush)
		return s.store.SetRestoreMarker(ctx, m)
	}

	// 1. Images: retain nothing this host no longer has.
	retained, err := s.store.RetainedImages(ctx)
	if err != nil {
		return err
	}
	hostImages, err := s.runtime.ListImages(ctx, "outhaul/*")
	if err != nil {
		return err
	}
	onHost := make(map[string]bool, len(hostImages))
	for _, img := range hostImages {
		onHost[img] = true
	}
	for _, img := range retained {
		if !strings.HasPrefix(img, "outhaul/") || onHost[img] {
			continue
		}
		if err := s.store.MarkImagePruned(ctx, img); err != nil {
			return err
		}
		m.PrunedImages++
	}
	if err := save(); err != nil {
		return err
	}

	// 2. Managed databases: running rows with no container come back empty.
	databases, err := s.store.ListDatabases(ctx)
	if err != nil {
		return err
	}
	for _, db := range databases {
		if db.Status != core.DBRunning {
			continue
		}
		c, err := s.runtime.FindContainer(ctx, dbaas.ContainerName(db.Name))
		if err != nil {
			m.Failed = append(m.Failed, "database "+db.Name+": "+err.Error())
			continue
		}
		if c != nil {
			continue
		}
		if err := s.databases.ProvisionSync(ctx, db); err != nil {
			if serr := s.store.SetDatabaseStatus(ctx, db.ID, core.DBFailed, err.Error()); serr != nil {
				return serr
			}
			m.Failed = append(m.Failed, "database "+db.Name+": "+err.Error())
			continue
		}
		m.Databases = append(m.Databases, db.Name)
	}
	if err := save(); err != nil {
		return err
	}

	// 3. Apps: a live app with no container gets the deploy a protection
	// change would queue. Ordinary apps first, then previews.
	apps, err := s.store.ListApps(ctx)
	if err != nil {
		return err
	}
	sort.SliceStable(apps, func(i, j int) bool { return !apps[i].Ephemeral && apps[j].Ephemeral })
	for _, app := range apps {
		live, err := s.store.LiveDeployment(ctx, app.ID)
		if err != nil {
			return err
		}
		if live == nil {
			continue
		}
		present, err := s.appHasContainers(ctx, app)
		if err != nil {
			m.Failed = append(m.Failed, "app "+app.Name+": "+err.Error())
			continue
		}
		if present {
			continue
		}
		if app.Source == core.SourcePush && (s.repos == nil || !s.repos.Exists(app.Name)) {
			m.WaitingPush = append(m.WaitingPush, app.Name)
			continue
		}
		serving := ""
		if live.Image != "" && !live.ImagePruned && onHost[live.Image] {
			serving = live.Image
		}
		dep, queued, err := s.enqueueProtectionDeploy(ctx, app, true, serving)
		if err != nil {
			m.Failed = append(m.Failed, "app "+app.Name+": "+err.Error())
			continue
		}
		if queued || dep.ID != 0 {
			m.Queued = append(m.Queued, app.Name)
		}
	}

	// 4. Done. The notice keeps the lists until a system admin dismisses it.
	m.Pending = false
	if err := save(); err != nil {
		return err
	}
	for _, name := range m.Databases {
		log.Printf("restore pass: provisioned database %s", name)
	}
	for _, name := range m.Queued {
		log.Printf("restore pass: queued a deploy of %s", name)
	}
	for _, name := range m.WaitingPush {
		log.Printf("restore pass: %s waits for a git push", name)
	}
	for _, f := range m.Failed {
		log.Printf("restore pass: %s", f)
	}
	if m.PrunedImages > 0 {
		log.Printf("restore pass: marked %d image(s) pruned", m.PrunedImages)
	}
	log.Printf("restore pass: done: %d database(s), %d app(s) queued, %d waiting for a push, %d failed",
		len(m.Databases), len(m.Queued), len(m.WaitingPush), len(m.Failed))
	return nil
}

// appHasContainers reports whether the app has any container of its own,
// stopped or running. A compose app counts its project's containers.
func (s *Server) appHasContainers(ctx context.Context, app core.App) (bool, error) {
	cs, err := s.appContainers(ctx, app)
	if err != nil {
		return false, err
	}
	if app.Kind == core.KindCompose {
		stack, err := s.runtime.ListContainers(ctx, map[string]string{"com.docker.compose.project": compose.ProjectName(app.Name)})
		if err != nil {
			return false, err
		}
		cs = append(cs, stack...)
	}
	return len(cs) > 0, nil
}

// handleDismissRestoreNotice removes the notice from Settings.
func (s *Server) handleDismissRestoreNotice(w http.ResponseWriter, r *http.Request) {
	if err := s.store.ClearRestoreMarker(r.Context()); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}

func dedupeStrings(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := in[:0]
	for _, v := range in {
		if seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}
