package server

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/outhaul-dev/outhaul/internal/catalog"
	"github.com/outhaul-dev/outhaul/internal/core"
)

// handleTemplatesList renders the one-click template gallery.
func (s *Server) handleTemplatesList(w http.ResponseWriter, r *http.Request) {
	s.renderTemplates(w, r, http.StatusOK, "")
}

func (s *Server) renderTemplates(w http.ResponseWriter, r *http.Request, status int, errMsg string) {
	tmpls, err := catalog.All()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	projects, err := s.store.ListProjects(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.render(w, status, "templates", map[string]any{
		"Title": "Templates", "Active": "templates", "Templates": tmpls,
		"Projects": projects, "SelectedProject": selectedProject(projects),
		"Error": errMsg,
	})
}

// handleDeployTemplate is the one click: create a compose app from a catalog
// template (compose snapshot, generated domains, generated env) and enqueue
// its first deployment, landing on the live build log.
func (s *Server) handleDeployTemplate(w http.ResponseWriter, r *http.Request) {
	tmpl, err := catalog.Get(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}

	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		name = tmpl.ID
	}
	if !appNameRe.MatchString(name) {
		s.renderTemplates(w, r, http.StatusBadRequest, "Name must be lowercase letters, digits and hyphens (2–40 chars).")
		return
	}
	projectID := core.DefaultProjectID
	if v := strings.TrimSpace(r.FormValue("project_id")); v != "" {
		id, ok := parseID(v)
		if !ok {
			s.renderTemplates(w, r, http.StatusBadRequest, "Choose a project for the app.")
			return
		}
		projectID = id
	}
	if _, err := s.store.GetProject(r.Context(), projectID); err != nil {
		s.renderTemplates(w, r, http.StatusBadRequest, "Choose a project for the app.")
		return
	}

	rendered, err := catalog.Render(tmpl, name, s.serverIP)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	app, err := s.store.CreateApp(r.Context(), core.App{
		Name: name, ProjectID: projectID,
		Source: core.SourceTemplate, Kind: core.KindCompose,
		ComposePath: "docker-compose.yml",
		TemplateID:  tmpl.ID, ComposeRaw: tmpl.Compose,
		WebhookSecret: newSecret(),
	})
	if err != nil {
		// Most likely a duplicate name (UNIQUE constraint).
		s.renderTemplates(w, r, http.StatusBadRequest, "Could not create app: "+err.Error())
		return
	}
	for _, d := range rendered.Domains {
		if _, err := s.store.AddDomain(r.Context(),
			core.Domain{AppID: app.ID, Host: d.Host, Service: d.Service, Port: d.Port, TLS: true}); err != nil {
			s.renderTemplates(w, r, http.StatusInternalServerError,
				"App created, but a domain could not be added: "+err.Error())
			return
		}
	}
	for _, e := range rendered.Env {
		if err := s.store.SetEnv(r.Context(), app.ID, e.Key, e.Value, e.Secret); err != nil {
			s.renderTemplates(w, r, http.StatusInternalServerError,
				"App created, but an env var could not be set: "+err.Error())
			return
		}
	}

	dep, err := s.store.CreateDeployment(r.Context(), app.ID)
	if err != nil {
		// The app exists and is configured; land there rather than erroring.
		http.Redirect(w, r, "/apps/"+strconv.FormatInt(app.ID, 10), http.StatusSeeOther)
		return
	}
	s.deployer.Notify()
	http.Redirect(w, r, deploymentPath(dep.ID), http.StatusSeeOther)
}
