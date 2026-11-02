package server

import "net/http"

// handleDomainsList renders every route across the apps the person may see: the
// global Domains tab.
func (s *Server) handleDomainsList(w http.ResponseWriter, r *http.Request) {
	scope, err := s.scopeFor(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	domains, err := s.store.ListAllDomains(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !scope.all {
		apps, err := s.store.ListApps(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		projectOf := make(map[int64]int64, len(apps))
		for _, a := range apps {
			projectOf[a.ID] = a.ProjectID
		}
		kept := domains[:0]
		for _, d := range domains {
			if pid, ok := projectOf[d.AppID]; ok && scope.allows(pid) {
				kept = append(kept, d)
			}
		}
		domains = kept
	}
	s.render(w, r, http.StatusOK, "domains", map[string]any{
		"Title":   "Domains",
		"Active":  "domains",
		"Domains": domains,
	})
}
