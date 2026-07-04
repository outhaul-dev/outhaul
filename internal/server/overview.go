package server

import (
	"net/http"

	"github.com/slipwaydev/slipway/internal/core"
)

// handleOverview renders the portal dashboard: headline counts plus the most
// recent deployments across all apps. Metric stat blocks are presentational
// placeholders this pass (marked "not live").
func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	apps, err := s.store.ListApps(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	projects, err := s.store.ListProjects(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	deployCount, err := s.store.CountDeployments(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	recent, err := s.store.ListRecentDeployments(r.Context(), 8)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	names := make(map[int64]string, len(apps))
	for _, a := range apps {
		names[a.ID] = a.Name
	}
	running := 0
	for _, a := range apps {
		latest, err := s.store.LatestDeploymentForApp(r.Context(), a.ID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if latest != nil && latest.Status == core.StatusRunning {
			running++
		}
	}

	type recentRow struct {
		core.Deployment
		AppName string
	}
	rows := make([]recentRow, 0, len(recent))
	for _, d := range recent {
		rows = append(rows, recentRow{Deployment: d, AppName: names[d.AppID]})
	}

	s.render(w, http.StatusOK, "overview", map[string]any{
		"Title":        "Overview",
		"Active":       "overview",
		"ProjectCount": len(projects),
		"AppCount":     len(apps),
		"RunningCount": running,
		"DeployCount":  deployCount,
		"Recent":       rows,
	})
}
