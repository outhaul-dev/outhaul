package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/outhaul-dev/outhaul/internal/catalog"
	"github.com/outhaul-dev/outhaul/internal/core"
	"github.com/outhaul-dev/outhaul/internal/store"
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
	projects, err := s.projectsFor(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.render(w, r, status, "templates", map[string]any{
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

	rendered, err := catalog.Render(tmpl, name, s.naming)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// The form has no sign-in switch: a deploy that doesn't say is protected.
	// An explicit protected_offered without protected is the only way to open a
	// template's rows, and no page sends it.
	protect := s.protectNew && (r.FormValue("protected_offered") == "" || r.FormValue("protected") != "")

	// Every host is checked before the app exists, as the new-app form does.
	hosts := map[string]bool{}
	for _, d := range rendered.Domains {
		host := strings.ToLower(d.Host)
		hosts[host] = true
		if s.isOwnHost(host) {
			s.renderTemplates(w, r, http.StatusBadRequest, ownHostRefusal)
			return
		}
		if protect {
			if ok, reason := s.protectionAvailability(r.Context(), core.Domain{Host: host}); !ok {
				s.renderTemplates(w, r, http.StatusBadRequest, reason)
				return
			}
		}
		refused, err := s.sharedHostRefused(r, projectID, host, true, protect)
		if err != nil {
			s.renderTemplates(w, r, http.StatusInternalServerError, "Could not create app: "+err.Error())
			return
		}
		if refused {
			s.renderTemplates(w, r, http.StatusBadRequest, sharedHostRefusal)
			return
		}
	}
	if protect {
		scope, err := s.scopeFor(r)
		if err != nil {
			log.Printf("deploy template: read the person's projects: %v", err)
			scope = projectScope{}
		}
		// The app doesn't exist yet, so nothing is exempt: an orphan container a
		// same-named app's failed teardown left behind still serves the host openly.
		others, err := s.leftoverRoutes(r.Context(), core.App{}, hosts, scope)
		if err != nil {
			s.renderTemplates(w, r, http.StatusServiceUnavailable,
				fmt.Sprintf("The app wasn't created: Outhaul couldn't tell which other apps still serve old routes on %s (%v).", rendered.Domains[0].Host, err))
			return
		}
		if len(others) > 0 {
			s.renderTemplates(w, r, http.StatusBadRequest, fmt.Sprintf(
				"The app wasn't created: %s still serves an old route on %s with no sign-in. Deploy it from its page, then try again.",
				others[0].label(), others[0].Host))
			return
		}
	}

	rows := make([]core.Domain, 0, len(rendered.Domains))
	for _, d := range rendered.Domains {
		rows = append(rows, core.Domain{Host: d.Host, Service: d.Service, Port: d.Port, TLS: true, Protected: protect})
	}
	app, err := s.store.CreateAppWithDomains(r.Context(), core.App{
		Name: name, ProjectID: projectID,
		Source: core.SourceTemplate, Kind: core.KindCompose,
		ComposePath: "docker-compose.yml",
		TemplateID:  tmpl.ID, ComposeRaw: tmpl.Compose,
		WebhookToken:  newSecret(),
		WebhookSecret: newSecret(),
	}, rows)
	if err != nil {
		if errors.Is(err, store.ErrHostReserved) {
			s.renderTemplates(w, r, http.StatusBadRequest, ownHostRefusal)
			return
		}
		s.renderTemplates(w, r, http.StatusBadRequest, "Could not create app: "+s.takenText(r, err))
		return
	}
	// Before the env and the deployment, so a gate that can't be mapped leaves
	// neither an app nor a queued deploy behind.
	if protect {
		ctx := context.WithoutCancel(r.Context())
		if err := s.ensureGateHost(ctx); err != nil {
			if undo := s.store.DeleteApp(ctx, app.ID); undo != nil {
				log.Printf("deploy template: remove %q after the proxy failed: %v", app.Name, undo)
				s.renderTemplates(w, r, http.StatusServiceUnavailable, fmt.Sprintf(
					"Sign-in couldn't be turned on: Outhaul couldn't map the gate (%v), and couldn't remove the app it had just created (%v). Delete it from the list before trying again.", err, undo))
				return
			}
			s.renderTemplates(w, r, http.StatusServiceUnavailable, fmt.Sprintf(
				"The app wasn't created: Outhaul couldn't map the sign-in gate before it deploys (%v).", err))
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
