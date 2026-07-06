package server

import "net/http"

// handleDomainsList renders every route across all apps: the global Domains tab.
func (s *Server) handleDomainsList(w http.ResponseWriter, r *http.Request) {
	domains, err := s.store.ListAllDomains(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.render(w, http.StatusOK, "domains", map[string]any{
		"Title":   "Domains",
		"Active":  "domains",
		"Domains": domains,
	})
}
