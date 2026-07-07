package server

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/outhaul-dev/outhaul/internal/core"
	"github.com/outhaul-dev/outhaul/internal/store"
)

// projectRow is a project plus the derived stats its card shows.
type projectRow struct {
	core.Project
	AppCount int
	Running  int
}

// projectRows assembles the Projects-page card data: every project with its
// app count and how many of its apps currently have a running deployment.
func (s *Server) projectRows(r *http.Request) ([]projectRow, error) {
	projects, err := s.store.ListProjects(r.Context())
	if err != nil {
		return nil, err
	}
	counts, err := s.store.CountAppsByProject(r.Context())
	if err != nil {
		return nil, err
	}
	apps, err := s.store.ListApps(r.Context())
	if err != nil {
		return nil, err
	}
	running := map[int64]int{}
	for _, a := range apps {
		latest, err := s.store.LatestDeploymentForApp(r.Context(), a.ID)
		if err != nil {
			return nil, err
		}
		if latest != nil && latest.Status == core.StatusRunning {
			running[a.ProjectID]++
		}
	}
	rows := make([]projectRow, 0, len(projects))
	for _, p := range projects {
		rows = append(rows, projectRow{Project: p, AppCount: counts[p.ID], Running: running[p.ID]})
	}
	return rows, nil
}

func (s *Server) handleProjectsList(w http.ResponseWriter, r *http.Request) {
	rows, err := s.projectRows(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.render(w, http.StatusOK, "projects", map[string]any{
		"Title": "Projects", "Active": "projects", "Projects": rows,
	})
}

// renderProjectsWithError re-renders the projects list with the create form
// pre-filled and an error message.
func (s *Server) renderProjectsWithError(w http.ResponseWriter, r *http.Request, msg, name, description string) {
	rows, _ := s.projectRows(r)
	s.render(w, http.StatusBadRequest, "projects", map[string]any{
		"Title": "Projects", "Active": "projects", "Projects": rows,
		"Error": msg,
		"Form":  map[string]string{"Name": name, "Description": description},
	})
}

func (s *Server) handleCreateProject(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.FormValue("name"))
	description := strings.TrimSpace(r.FormValue("description"))
	// Project names share the app-name rule: they appear in URLs and cards,
	// and one rule everywhere beats two subtly different ones.
	if !appNameRe.MatchString(name) {
		s.renderProjectsWithError(w, r, "Name must be lowercase letters, digits and hyphens (2–40 chars).", name, description)
		return
	}
	if _, err := s.store.CreateProject(r.Context(), core.Project{Name: name, Description: description}); err != nil {
		// Most likely a duplicate name (UNIQUE constraint).
		s.renderProjectsWithError(w, r, "Could not create project: "+err.Error(), name, description)
		return
	}
	http.Redirect(w, r, "/projects", http.StatusSeeOther)
}

// renderProject renders the project-detail page (also used to redisplay it
// with an error after a rejected settings change, delete, or database
// creation). dbForm carries the submitted database-create form values so a
// rejected submission re-populates the reopened dialog instead of losing
// what the operator typed; pass nil for non-database-form callers.
func (s *Server) renderProject(w http.ResponseWriter, r *http.Request, status int, p core.Project, errMsg string, dbForm map[string]string) {
	apps, err := s.store.ListAppsByProject(r.Context(), p.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	rows, err := s.appRows(r.Context(), apps, nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	projects, err := s.store.ListProjects(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	envVars, err := s.store.ListProjectEnv(r.Context(), p.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	dbs, err := s.store.ListDatabasesByProject(r.Context(), p.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	data := map[string]any{
		"Title":           p.Name,
		"Active":          "projects",
		"Project":         p,
		"Apps":            rows,
		"AppCount":        len(rows),
		"Databases":       dbs,
		"Env":             maskEnv(envVars),
		"Projects":        projects,
		"SelectedProject": p.ID,
		"InProject":       true,
		"Return":          "/projects/" + strconv.FormatInt(p.ID, 10),
		"DBForm":          dbForm,
	}
	if errMsg != "" {
		data["Error"] = errMsg
	}
	if dbForm != nil {
		data["OpenDialog"] = "db-dialog"
	}
	for k, v := range s.githubRepoData(r) {
		data[k] = v
	}
	s.render(w, status, "project", data)
}

func (s *Server) handleProjectDetail(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	p, err := s.store.GetProject(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	s.renderProject(w, r, http.StatusOK, p, "", nil)
}

// handleProjectSettings renames a project and updates its description.
func (s *Server) handleProjectSettings(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	p, err := s.store.GetProject(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	description := strings.TrimSpace(r.FormValue("description"))
	if !appNameRe.MatchString(name) {
		s.renderProject(w, r, http.StatusBadRequest, p, "Name must be lowercase letters, digits and hyphens (2–40 chars).", nil)
		return
	}
	if err := s.store.UpdateProject(r.Context(), id, name, description); err != nil {
		// Most likely a duplicate name (UNIQUE constraint).
		s.renderProject(w, r, http.StatusBadRequest, p, "Could not update project: "+err.Error(), nil)
		return
	}
	http.Redirect(w, r, "/projects/"+strconv.FormatInt(id, 10), http.StatusSeeOther)
}

// handleSetProjectEnv upserts a shared variable. Apps consume shared variables
// only via ${{project.KEY}} references in their own env values, resolved on
// each app's next deploy — so unlike app env, a project-level PORT is harmless
// and allowed.
func (s *Server) handleSetProjectEnv(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	p, err := s.store.GetProject(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	key := strings.TrimSpace(r.FormValue("key"))
	if !envKeyRe.MatchString(key) {
		s.renderProject(w, r, http.StatusBadRequest, p, "Key must be UPPER_SNAKE_CASE (letters, digits, underscore).", nil)
		return
	}
	if err := s.store.SetProjectEnv(r.Context(), id, key, r.FormValue("value"), r.FormValue("secret") != ""); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/projects/"+strconv.FormatInt(id, 10), http.StatusSeeOther)
}

func (s *Server) handleDeleteProjectEnv(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	if _, err := s.store.GetProject(r.Context(), id); err != nil {
		http.NotFound(w, r)
		return
	}
	key := strings.TrimSpace(r.FormValue("key"))
	if err := s.store.DeleteProjectEnv(r.Context(), id, key); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/projects/"+strconv.FormatInt(id, 10), http.StatusSeeOther)
}

// handleDeleteProject deletes an empty project; a project that still has apps
// is refused and the page redisplayed with an alert (the store enforces the
// guard, so a racing app creation cannot slip through).
func (s *Server) handleDeleteProject(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	p, err := s.store.GetProject(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := s.store.DeleteProject(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrProjectNotEmpty) {
			apps, _ := s.store.ListAppsByProject(r.Context(), id)
			dbs, _ := s.store.ListDatabasesByProject(r.Context(), id)
			s.renderProject(w, r, http.StatusConflict, p,
				fmt.Sprintf("This project still has %d app(s) and %d database(s). Delete them before deleting the project.", len(apps), len(dbs)), nil)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/projects", http.StatusSeeOther)
}

// selectedProject picks the create-app form's preselected project: the
// Default project when it still exists, else the first listed.
func selectedProject(projects []core.Project) int64 {
	for _, p := range projects {
		if p.ID == core.DefaultProjectID {
			return p.ID
		}
	}
	if len(projects) > 0 {
		return projects[0].ID
	}
	return 0
}
