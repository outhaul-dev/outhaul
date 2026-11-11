package server

import (
	"log"
	"net/http"

	"github.com/outhaul-dev/outhaul/internal/core"
	"github.com/outhaul-dev/outhaul/internal/docker"
)

// domainRow is a row of the Domains page with its Access cell.
type domainRow struct {
	core.DomainListing
	Access accessState
}

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
	byRouter := rowsByRouter(domains) // every app's, before the scope filters them
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
	// One call for every managed container, grouped by app, rather than one
	// per row. When Docker can't be asked every row is Unknown.
	byApp := map[string][]docker.Container{}
	all, listErr := s.runtime.ListContainers(r.Context(), map[string]string{"outhaul.managed": "true"})
	if listErr != nil {
		log.Printf("domains page: list the managed containers: %v", listErr)
	}
	all = s.served(all)
	var running []docker.Container
	for _, c := range all {
		if answers(c) {
			byApp[c.Labels["outhaul.app"]] = append(byApp[c.Labels["outhaul.app"]], c)
			running = append(running, c)
		}
	}
	latest := map[int64]*core.Deployment{} // fetched only for rows that need it
	rows := make([]domainRow, 0, len(domains))
	for _, d := range domains {
		row := domainRow{DomainListing: d}
		switch {
		case listErr != nil:
			row.Access = unknownAccess()
		default:
			cs := byApp[d.AppName]
			row.Access = accessStateFor(d.Domain, d.AppName, cs, nil)
			if row.Access.Label == accessNotYet {
				dep, ok := latest[d.AppID]
				if !ok {
					var err error
					if dep, err = s.store.LatestDeploymentForApp(r.Context(), d.AppID); err != nil {
						log.Printf("domains page: latest deployment of %q: %v", d.AppName, err)
					}
					latest[d.AppID] = dep
				}
				row.Access = accessStateFor(d.Domain, d.AppName, cs, dep)
			}
			// Another route answering part of a protected row's address
			// without the gate.
			if d.Protected && row.Access.Label == accessSignIn {
				if owner, found := strayRoute(d.Domain, d.AppName, running, byRouter); found {
					row.Access = s.strayState(r.Context(), owner, d.AppName, scope)
				}
			}
		}
		rows = append(rows, row)
	}
	s.render(w, r, http.StatusOK, "domains", map[string]any{
		"Title":          "Domains",
		"Active":         "domains",
		"Domains":        rows,
		"ProtectedHTTPS": s.protectedOverHTTPS(r.Context()),
		"HTTPSOnly":      s.routing.HTTPOff,
	})
}
