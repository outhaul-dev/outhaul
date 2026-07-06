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

// placeholderPages is the fixed set of stubbed sections and their copy. Where
// a slice of the feature already shipped elsewhere, the copy says so instead
// of pretending nothing exists.
var placeholderPages = []placeholderPage{
	{"volumes", "Volumes", "Compose stacks' named volumes can already be backed up and restored from each app's Backups panel. A standalone volume browser is coming soon."},
	{"registry", "Registry", "Connect a private container registry to pull and push images."},
	{"infrastructure", "Infrastructure", "See the host, Docker, and Traefik that power your deployments."},
	{"metrics", "Metrics", "Live CPU, memory, and network metrics are already on every app's page. A host-wide metrics dashboard is coming soon."},
}
