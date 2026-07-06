package server

import (
	"context"
	"net/http"
	"strings"

	"github.com/james-smart/outhaul/internal/compose"
	"github.com/james-smart/outhaul/internal/core"
)

const composeProjectLabel = "com.docker.compose.project"

// volumeRow is one line in the global Volumes inventory.
type volumeRow struct {
	Name      string
	AppName   string // owning app; "" when orphaned
	AppID     int64  // link target; 0 when orphaned
	Kind      string // "app" | "compose"
	MountPath string // app volumes only
	Orphan    bool
}

// inventory assembles every Outhaul-known volume: DB-tracked app volumes
// (attached or orphaned) plus compose stack volumes (owned or from a deleted
// stack). User-created stacks not managed by Outhaul are ignored.
func (s *Server) inventory(ctx context.Context) ([]volumeRow, error) {
	dbVols, err := s.store.ListAllVolumes(ctx)
	if err != nil {
		return nil, err
	}
	byName := make(map[string]core.VolumeListing, len(dbVols))
	for _, v := range dbVols {
		byName[v.Name] = v
	}

	var rows []volumeRow

	// App volumes Outhaul created (labelled managed+data). Attached if a DB row
	// still names them; orphaned otherwise (app deleted or volume detached).
	appVols, err := s.runtime.ListVolumesFull(ctx, map[string]string{
		core.VolumeLabelManaged: "true", core.VolumeLabelRole: core.VolumeRoleData,
	})
	if err != nil {
		return nil, err
	}
	for _, v := range appVols {
		if owner, ok := byName[v.Name]; ok {
			rows = append(rows, volumeRow{
				Name: v.Name, AppName: owner.AppName, AppID: owner.AppID,
				Kind: "app", MountPath: owner.MountPath,
			})
		} else {
			rows = append(rows, volumeRow{Name: v.Name, Kind: "app", Orphan: true})
		}
	}

	// Compose stack volumes: enumerate by label presence, keep only Outhaul
	// projects (outhaul-<app>), and mark orphaned when their app is gone.
	apps, err := s.store.ListApps(ctx)
	if err != nil {
		return nil, err
	}
	projectApp := make(map[string]core.App)
	for _, a := range apps {
		if a.Kind == core.KindCompose {
			projectApp[compose.ProjectName(a.Name)] = a
		}
	}
	composeVols, err := s.runtime.ListVolumesFull(ctx, map[string]string{composeProjectLabel: ""})
	if err != nil {
		return nil, err
	}
	for _, v := range composeVols {
		project := v.Labels[composeProjectLabel]
		if !strings.HasPrefix(project, "outhaul-") {
			continue // not an Outhaul-managed stack
		}
		if a, ok := projectApp[project]; ok {
			rows = append(rows, volumeRow{Name: v.Name, AppName: a.Name, AppID: a.ID, Kind: "compose"})
		} else {
			rows = append(rows, volumeRow{Name: v.Name, Kind: "compose", Orphan: true})
		}
	}
	return rows, nil
}

// handleVolumesList renders the global Volumes inventory.
func (s *Server) handleVolumesList(w http.ResponseWriter, r *http.Request) {
	rows, err := s.inventory(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.render(w, http.StatusOK, "volumes", map[string]any{
		"Title":   "Volumes",
		"Active":  "volumes",
		"Volumes": rows,
	})
}

// handleReclaimVolume removes an orphaned Docker volume. It re-checks orphan
// status server-side before deleting — the guard is never UI-only.
func (s *Server) handleReclaimVolume(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		http.Error(w, "missing volume name", http.StatusBadRequest)
		return
	}
	rows, err := s.inventory(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var orphan bool
	found := false
	for _, row := range rows {
		if row.Name == name {
			orphan = row.Orphan
			found = true
			break
		}
	}
	if !found || !orphan {
		http.Error(w, "refusing to remove a volume that is still attached to an app (or not managed by Outhaul)", http.StatusBadRequest)
		return
	}
	if err := s.runtime.RemoveVolume(r.Context(), name, false); err != nil {
		http.Error(w, "could not remove volume: "+err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/volumes", http.StatusSeeOther)
}
