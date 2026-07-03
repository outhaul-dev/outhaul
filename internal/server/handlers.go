package server

import (
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/slipwaydev/slipway/internal/core"
)

// appNameRe restricts app names to values safe as container names, Traefik
// router identifiers, and URL segments.
var appNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,38}[a-z0-9]$`)

func (s *Server) handleAppsList(w http.ResponseWriter, r *http.Request) {
	apps, err := s.store.ListApps(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Attach each app's latest deployment status for the list view.
	type appRow struct {
		core.App
		Latest *core.Deployment
	}
	rows := make([]appRow, 0, len(apps))
	for _, a := range apps {
		latest, err := s.store.LatestDeploymentForApp(r.Context(), a.ID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		rows = append(rows, appRow{App: a, Latest: latest})
	}

	s.render(w, http.StatusOK, "apps", map[string]any{
		"Title": "Apps",
		"Apps":  rows,
	})
}

func (s *Server) handleCreateApp(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.FormValue("name"))
	repo := strings.TrimSpace(r.FormValue("repo_url"))
	domain := strings.TrimSpace(r.FormValue("domain"))

	if verr := validateApp(name, repo, domain); verr != "" {
		s.renderAppsWithError(w, r, verr, name, repo, domain)
		return
	}

	_, err := s.store.CreateApp(r.Context(), core.App{Name: name, RepoURL: repo, Domain: domain})
	if err != nil {
		// Most likely a duplicate name (UNIQUE constraint).
		s.renderAppsWithError(w, r, "Could not create app: "+err.Error(), name, repo, domain)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// renderAppsWithError re-renders the apps list with the create form pre-filled
// and an error message.
func (s *Server) renderAppsWithError(w http.ResponseWriter, r *http.Request, msg, name, repo, domain string) {
	apps, _ := s.store.ListApps(r.Context())
	type appRow struct {
		core.App
		Latest *core.Deployment
	}
	rows := make([]appRow, 0, len(apps))
	for _, a := range apps {
		latest, _ := s.store.LatestDeploymentForApp(r.Context(), a.ID)
		rows = append(rows, appRow{App: a, Latest: latest})
	}
	s.render(w, http.StatusBadRequest, "apps", map[string]any{
		"Title": "Apps", "Apps": rows,
		"Error": msg,
		"Form":  map[string]string{"Name": name, "RepoURL": repo, "Domain": domain},
	})
}

func (s *Server) handleAppDetail(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	app, err := s.store.GetApp(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	deployments, err := s.store.ListDeploymentsForApp(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.render(w, http.StatusOK, "app", map[string]any{
		"Title":       app.Name,
		"App":         app,
		"Deployments": deployments,
	})
}

func (s *Server) handleDeploy(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	app, err := s.store.GetApp(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	dep, err := s.store.CreateDeployment(r.Context(), app.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.deployer.Notify()
	http.Redirect(w, r, deploymentPath(dep.ID), http.StatusSeeOther)
}

func (s *Server) handleDeploymentDetail(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	dep, err := s.store.GetDeployment(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	app, err := s.store.GetApp(r.Context(), dep.AppID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.render(w, http.StatusOK, "deployment", map[string]any{
		"Title":      "Deployment #" + r.PathValue("id"),
		"App":        app,
		"Deployment": dep,
		"Live":       !dep.Status.IsTerminal(),
	})
}

func (s *Server) handleCancel(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	if _, err := s.deployer.Cancel(r.Context(), id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, deploymentPath(id), http.StatusSeeOther)
}

func deploymentPath(id int64) string {
	return "/deployments/" + strconv.FormatInt(id, 10)
}

// validateApp returns an empty string when valid, else an error message.
func validateApp(name, repo, domain string) string {
	if !appNameRe.MatchString(name) {
		return "Name must be lowercase letters, digits and hyphens (2–40 chars)."
	}
	if repo == "" || !(strings.HasPrefix(repo, "http://") || strings.HasPrefix(repo, "https://")) {
		return "Repository must be a public http(s) Git URL."
	}
	if domain == "" || strings.ContainsAny(domain, " /") {
		return "Domain must be a bare hostname (e.g. app.example.com)."
	}
	return ""
}
