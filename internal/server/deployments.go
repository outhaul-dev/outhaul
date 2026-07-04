package server

import (
	"net/http"

	"github.com/james-smart/outhaul/internal/core"
)

// handleDeployments renders a global, cross-app deployment history.
func (s *Server) handleDeployments(w http.ResponseWriter, r *http.Request) {
	apps, err := s.store.ListApps(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	names := make(map[int64]string, len(apps))
	for _, a := range apps {
		names[a.ID] = a.Name
	}
	deps, err := s.store.ListRecentDeployments(r.Context(), 200)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	type row struct {
		core.Deployment
		AppName string
	}
	rows := make([]row, 0, len(deps))
	for _, d := range deps {
		rows = append(rows, row{Deployment: d, AppName: names[d.AppID]})
	}
	s.render(w, http.StatusOK, "deployments", map[string]any{
		"Title":       "Deployments",
		"Active":      "deployments",
		"Deployments": rows,
	})
}
