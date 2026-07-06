package server

import (
	"net/http"
	"sort"
	"strings"
)

// containerKind classifies an Outhaul-managed container by its name and returns
// a human label ("app", "database", "service"), or "" for containers the
// Containers page must hide: infra (the Traefik proxy), transient helpers
// (deploy scratch containers, database-removal helpers), and anything not
// created by Outhaul. Order matters: the more specific transient prefixes are
// tested before the generic "outhaul-db-"/"outhaul-" ones.
func containerKind(name string) string {
	switch {
	case strings.HasPrefix(name, appContainerPrefix):
		return "app"
	case strings.HasPrefix(name, "outhaul-deploy-"):
		return "" // transient build/deploy scratch container
	case strings.HasPrefix(name, "outhaul-db-rm-"):
		return "" // transient database-removal helper
	case strings.HasPrefix(name, "outhaul-db-"):
		return "database"
	case name == "outhaul-traefik":
		return "" // infrastructure proxy
	case strings.HasPrefix(name, "outhaul-"):
		return "service" // compose service, e.g. outhaul-shop-worker-1
	default:
		return "" // not Outhaul-managed
	}
}

// containerRow is one Outhaul-managed container as rendered on the Containers
// page.
type containerRow struct{ Name, Kind, Image, State string }

// handleContainers renders every Outhaul-managed container across apps and
// databases, hiding infrastructure and transient containers.
func (s *Server) handleContainers(w http.ResponseWriter, r *http.Request) {
	all, err := s.runtime.ListContainers(r.Context(), nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	rows := make([]containerRow, 0, len(all))
	for _, c := range all {
		kind := containerKind(c.Name)
		if kind == "" {
			continue
		}
		rows = append(rows, containerRow{Name: c.Name, Kind: kind, Image: c.Image, State: c.State})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
	s.render(w, http.StatusOK, "containers", map[string]any{
		"Title":      "Containers",
		"Active":     "containers",
		"Containers": rows,
	})
}

// handleDeploymentsRedirect preserves old /deployments list links by sending
// them to the new Containers page.
func (s *Server) handleDeploymentsRedirect(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/containers", http.StatusFound)
}
