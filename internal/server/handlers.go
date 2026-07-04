package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/slipwaydev/slipway/internal/core"
	"github.com/slipwaydev/slipway/internal/github"
	"github.com/slipwaydev/slipway/internal/sshkey"
)

// appContainerPrefix is prepended to an app's name to get its container name.
const appContainerPrefix = "slipway-app-"

// appNameRe restricts app names to values safe as container names, Traefik
// router identifiers, and URL segments.
var appNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,38}[a-z0-9]$`)

// envKeyRe restricts env var names to conventional shell identifiers.
var envKeyRe = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)

func (s *Server) handleAppsList(w http.ResponseWriter, r *http.Request) {
	apps, err := s.store.ListApps(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	projects, err := s.store.ListProjects(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	projectNames := make(map[int64]string, len(projects))
	for _, p := range projects {
		projectNames[p.ID] = p.Name
	}

	// Attach each app's latest deployment status for the list view.
	type appRow struct {
		core.App
		ProjectName string
		Latest      *core.Deployment
	}
	rows := make([]appRow, 0, len(apps))
	for _, a := range apps {
		latest, err := s.store.LatestDeploymentForApp(r.Context(), a.ID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		rows = append(rows, appRow{App: a, ProjectName: projectNames[a.ProjectID], Latest: latest})
	}

	data := map[string]any{
		"Title": "Apps", "Active": "apps", "Apps": rows,
		"Projects": projects, "SelectedProject": selectedProject(projects),
	}
	for k, v := range s.githubRepoData(r) {
		data[k] = v
	}
	s.render(w, http.StatusOK, "apps", data)
}

// githubRepoData returns template data describing GitHub App connectivity for
// the create-app form: "GithubConnected" (bool) when an App is connected and
// installed, and "GithubRepos" ([]github.Repo) when the repo list could be
// fetched. Any failure along the way (missing key, token exchange, API error)
// degrades gracefully to no repo dropdown rather than failing the page.
func (s *Server) githubRepoData(r *http.Request) map[string]any {
	data := map[string]any{}
	ga, ok, err := s.store.GithubApp(r.Context())
	if err != nil || !ok || ga.InstallationID == 0 {
		return data
	}
	data["GithubConnected"] = true
	jwt, err := github.AppJWT(ga.PrivateKey, ga.AppID, time.Now())
	if err != nil {
		return data
	}
	tok, err := s.gh.InstallationToken(r.Context(), jwt, ga.InstallationID)
	if err != nil {
		return data
	}
	repos, err := s.gh.ListRepos(r.Context(), tok)
	if err != nil {
		return data
	}
	data["GithubRepos"] = repos
	return data
}

func (s *Server) handleCreateApp(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.FormValue("name"))
	domain := strings.TrimSpace(r.FormValue("domain"))
	source := strings.TrimSpace(r.FormValue("source"))
	branch := strings.TrimSpace(r.FormValue("branch"))
	autoDeploy := r.FormValue("auto_deploy") != ""
	if source == "" {
		source = core.SourcePublic
	}
	if branch == "" {
		branch = "main"
	}

	repo := strings.TrimSpace(r.FormValue("repo_url"))
	githubRepo := strings.TrimSpace(r.FormValue("github_repo"))
	if source == core.SourceGithub {
		repo = "https://github.com/" + githubRepo + ".git"
	}

	projectID := core.DefaultProjectID
	if v := strings.TrimSpace(r.FormValue("project_id")); v != "" {
		id, ok := parseID(v)
		if !ok {
			s.renderAppsWithError(w, r, "Choose a project for the app.", name, repo, domain)
			return
		}
		projectID = id
	}
	// No DB-level FK on apps.project_id, so the reference is validated here.
	if _, err := s.store.GetProject(r.Context(), projectID); err != nil {
		s.renderAppsWithError(w, r, "Choose a project for the app.", name, repo, domain)
		return
	}

	if verr := validateApp(name, repo, domain, source, githubRepo); verr != "" {
		s.renderAppsWithError(w, r, verr, name, repo, domain)
		return
	}

	app := core.App{
		Name: name, RepoURL: repo, Domain: domain, Source: source,
		Branch: branch, AutoDeploy: autoDeploy, GithubRepo: githubRepo,
		WebhookSecret: newSecret(), ProjectID: projectID,
	}
	if source == core.SourceSSH {
		priv, pub, err := sshkey.Generate()
		if err != nil {
			s.renderAppsWithError(w, r, "Could not generate SSH key: "+err.Error(), name, repo, domain)
			return
		}
		app.SSHPrivateKey, app.SSHPublicKey = priv, pub
	}

	if _, err := s.store.CreateApp(r.Context(), app); err != nil {
		// Most likely a duplicate name (UNIQUE constraint).
		s.renderAppsWithError(w, r, "Could not create app: "+err.Error(), name, repo, domain)
		return
	}
	// The project-detail form asks to land back on its project; anything else
	// (or a tampered value) falls back to the apps list.
	ret := r.FormValue("return")
	if !strings.HasPrefix(ret, "/projects/") {
		ret = "/apps"
	}
	http.Redirect(w, r, ret, http.StatusSeeOther)
}

// newSecret returns a random hex token for a per-app webhook.
func newSecret() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// handleAppSettings updates the mutable per-app deploy settings (branch and
// auto-deploy-on-push).
func (s *Server) handleAppSettings(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	if _, err := s.store.GetApp(r.Context(), id); err != nil {
		http.NotFound(w, r)
		return
	}
	branch := strings.TrimSpace(r.FormValue("branch"))
	if branch == "" {
		branch = "main"
	}
	autoDeploy := r.FormValue("auto_deploy") != ""
	if err := s.store.UpdateAppSettings(r.Context(), id, branch, autoDeploy); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/apps/"+strconv.FormatInt(id, 10), http.StatusSeeOther)
}

// renderAppsWithError re-renders the apps list with the create form pre-filled
// and an error message.
func (s *Server) renderAppsWithError(w http.ResponseWriter, r *http.Request, msg, name, repo, domain string) {
	apps, _ := s.store.ListApps(r.Context())
	projects, _ := s.store.ListProjects(r.Context())
	projectNames := make(map[int64]string, len(projects))
	for _, p := range projects {
		projectNames[p.ID] = p.Name
	}
	type appRow struct {
		core.App
		ProjectName string
		Latest      *core.Deployment
	}
	rows := make([]appRow, 0, len(apps))
	for _, a := range apps {
		latest, _ := s.store.LatestDeploymentForApp(r.Context(), a.ID)
		rows = append(rows, appRow{App: a, ProjectName: projectNames[a.ProjectID], Latest: latest})
	}
	data := map[string]any{
		"Title": "Apps", "Active": "apps", "Apps": rows,
		"Projects": projects, "SelectedProject": selectedProject(projects),
		"Error": msg,
		"Form":  map[string]string{"Name": name, "RepoURL": repo, "Domain": domain},
	}
	for k, v := range s.githubRepoData(r) {
		data[k] = v
	}
	s.render(w, http.StatusBadRequest, "apps", data)
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
	envVars, err := s.store.ListEnv(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	type envRow struct {
		Key      string
		Value    string
		IsSecret bool
	}
	envRows := make([]envRow, 0, len(envVars))
	for _, v := range envVars {
		row := envRow{Key: v.Key, IsSecret: v.IsSecret}
		if !v.IsSecret {
			row.Value = v.Value
		}
		envRows = append(envRows, row)
	}
	runtimeState := "absent"
	if c, err := s.runtime.FindContainer(r.Context(), appContainerPrefix+app.Name); err == nil && c != nil {
		runtimeState = c.State
	}
	data := map[string]any{
		"Title":       app.Name,
		"Active":      "apps",
		"App":         app,
		"Deployments": deployments,
		"Env":         envRows,
		"Runtime":     runtimeState,
	}
	// Breadcrumb context; tolerate a missing project rather than 500 the page.
	if p, err := s.store.GetProject(r.Context(), app.ProjectID); err == nil {
		data["Project"] = p
	}
	if s.publicURL != "" {
		data["WebhookURL"] = strings.TrimRight(s.publicURL, "/") + "/webhooks/app/" + app.WebhookSecret
	}
	s.render(w, http.StatusOK, "app", data)
}

func (s *Server) handleSetEnv(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	if _, err := s.store.GetApp(r.Context(), id); err != nil {
		http.NotFound(w, r)
		return
	}
	key := strings.TrimSpace(r.FormValue("key"))
	value := r.FormValue("value")
	isSecret := r.FormValue("secret") != ""

	if !envKeyRe.MatchString(key) {
		http.Error(w, "Key must be UPPER_SNAKE_CASE (letters, digits, underscore).", http.StatusBadRequest)
		return
	}
	if key == "PORT" {
		http.Error(w, "PORT is managed by Slipway and cannot be set.", http.StatusBadRequest)
		return
	}
	if err := s.store.SetEnv(r.Context(), id, key, value, isSecret); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/apps/"+strconv.FormatInt(id, 10), http.StatusSeeOther)
}

func (s *Server) handleDeleteEnv(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	if _, err := s.store.GetApp(r.Context(), id); err != nil {
		http.NotFound(w, r)
		return
	}
	key := strings.TrimSpace(r.FormValue("key"))
	if err := s.store.DeleteEnv(r.Context(), id, key); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/apps/"+strconv.FormatInt(id, 10), http.StatusSeeOther)
}

func (s *Server) handleStopApp(w http.ResponseWriter, r *http.Request) {
	s.appLifecycle(w, r, func(ctx context.Context, cid string) error {
		return s.runtime.StopContainer(ctx, cid, 10*time.Second)
	})
}

func (s *Server) handleRestartApp(w http.ResponseWriter, r *http.Request) {
	s.appLifecycle(w, r, func(ctx context.Context, cid string) error {
		_ = s.runtime.StopContainer(ctx, cid, 10*time.Second) // ignore: already-stopped is fine
		return s.runtime.StartContainer(ctx, cid)
	})
}

func (s *Server) appLifecycle(w http.ResponseWriter, r *http.Request, action func(ctx context.Context, containerID string) error) {
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
	c, err := s.runtime.FindContainer(r.Context(), appContainerPrefix+app.Name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if c == nil {
		http.Error(w, "No container for this app. Deploy it first.", http.StatusConflict)
		return
	}
	if err := action(r.Context(), c.ID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/apps/"+strconv.FormatInt(id, 10), http.StatusSeeOther)
}

func (s *Server) handleDeleteApp(w http.ResponseWriter, r *http.Request) {
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
	// Best-effort container removal; proceed with the row delete regardless,
	// but log failures so an orphaned container isn't silently lost.
	c, ferr := s.runtime.FindContainer(r.Context(), appContainerPrefix+app.Name)
	if ferr != nil {
		log.Printf("delete app %d: could not inspect container (any container is now orphaned): %v", id, ferr)
	} else if c != nil {
		_ = s.runtime.StopContainer(r.Context(), c.ID, 10*time.Second)
		if rerr := s.runtime.RemoveContainer(r.Context(), c.ID, true); rerr != nil {
			log.Printf("delete app %d: could not remove container %s: %v", id, c.ID, rerr)
		}
	}
	if err := s.store.DeleteApp(r.Context(), id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/apps", http.StatusSeeOther)
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
		"Active":     "deployments",
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

// validateApp returns an empty string when valid, else an error message. The
// repo-URL rule is relaxed per source: github needs an "owner/name" repo
// selection, ssh needs a non-empty clone URL with no whitespace, and public
// needs an http(s) URL.
func validateApp(name, repo, domain, source, githubRepo string) string {
	if !appNameRe.MatchString(name) {
		return "Name must be lowercase letters, digits and hyphens (2–40 chars)."
	}
	if domain == "" || strings.ContainsAny(domain, " /") {
		return "Domain must be a bare hostname (e.g. app.example.com)."
	}
	switch source {
	case core.SourceGithub:
		if !strings.Contains(githubRepo, "/") {
			return "Select a GitHub repository (owner/name)."
		}
	case core.SourceSSH:
		if repo == "" || strings.HasPrefix(repo, "-") || strings.ContainsAny(repo, " \t") {
			return "Repository must be an SSH clone URL (e.g. git@github.com:owner/repo.git)."
		}
		if !strings.HasPrefix(repo, "ssh://") && !(strings.Contains(repo, "@") && strings.Contains(repo, ":")) {
			return "Repository must be an SSH clone URL (e.g. git@github.com:owner/repo.git or ssh://…)."
		}
	default: // public
		if repo == "" || !(strings.HasPrefix(repo, "http://") || strings.HasPrefix(repo, "https://")) {
			return "Repository must be a public http(s) Git URL."
		}
	}
	return ""
}
