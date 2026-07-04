package server

import "net/http"

// placeholderPage describes a not-yet-built portal section.
type placeholderPage struct {
	active string // sidebar nav key
	title  string
	desc   string
}

// handlePlaceholder returns a handler rendering a styled "coming soon" page for
// a section that is present in the nav but not yet implemented.
func (s *Server) handlePlaceholder(p placeholderPage) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.render(w, http.StatusOK, "placeholder", map[string]any{
			"Title":   p.title,
			"Active":  p.active,
			"Heading": p.title,
			"Desc":    p.desc,
		})
	}
}

// placeholderPages is the fixed set of stubbed sections and their copy.
var placeholderPages = []placeholderPage{
	{"databases", "Databases", "Provision managed Postgres, MySQL, Redis and Mongo instances and attach them to your apps."},
	{"volumes", "Volumes", "Create and mount persistent volumes so your apps keep data across deploys."},
	{"registry", "Registry", "Connect a private container registry to pull and push images."},
	{"domains", "Domains", "Manage custom domains, DNS, and TLS certificates in one place."},
	{"infrastructure", "Infrastructure", "See the host, Docker, and Traefik that power your deployments."},
	{"metrics", "Metrics", "Live CPU, memory, and network metrics for every running app."},
}
