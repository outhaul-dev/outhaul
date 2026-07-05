package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net/http"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/james-smart/outhaul/internal/compose"
	"github.com/james-smart/outhaul/internal/core"
	"github.com/james-smart/outhaul/internal/github"
	"github.com/james-smart/outhaul/internal/sshkey"
)

// appContainerPrefix is prepended to an app's name to get its container name.
const appContainerPrefix = "outhaul-app-"

// appNameRe restricts app names to values safe as container names, Traefik
// router identifiers, and URL segments.
var appNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,38}[a-z0-9]$`)

// envKeyRe restricts env var names to conventional shell identifiers.
var envKeyRe = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)

// composeServiceRe matches compose service names (the compose spec's own rule).
var composeServiceRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)

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

	rows, err := s.appRows(r.Context(), apps, projectNames)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
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

// appRow is one row of an app-list table: the app plus its display context.
type appRow struct {
	core.App
	ProjectName string
	Domains     []core.ComposeDomain // a compose app's published domains
	Latest      *core.Deployment
}

// appRows decorates apps with their project name, latest deployment status,
// and (for compose apps) published domains. projectNames may be nil when the
// listing is already scoped to one project.
func (s *Server) appRows(ctx context.Context, apps []core.App, projectNames map[int64]string) ([]appRow, error) {
	rows := make([]appRow, 0, len(apps))
	for _, a := range apps {
		latest, err := s.store.LatestDeploymentForApp(ctx, a.ID)
		if err != nil {
			return nil, err
		}
		row := appRow{App: a, ProjectName: projectNames[a.ProjectID], Latest: latest}
		if a.Kind == core.KindCompose {
			if row.Domains, err = s.store.ListComposeDomains(ctx, a.ID); err != nil {
				return nil, err
			}
		}
		rows = append(rows, row)
	}
	return rows, nil
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
	kind := strings.TrimSpace(r.FormValue("kind"))
	autoDeploy := r.FormValue("auto_deploy") != ""
	if source == "" {
		source = core.SourcePublic
	}
	if branch == "" {
		branch = "main"
	}
	if kind == "" {
		kind = core.KindNixpacks
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

	app := core.App{
		Name: name, RepoURL: repo, Domain: domain, Source: source, Kind: kind,
		Branch: branch, AutoDeploy: autoDeploy, GithubRepo: githubRepo,
		WebhookSecret: newSecret(), ProjectID: projectID,
	}
	// Compose domains live in their own table (a stack can have many); the
	// form's optional domain/service/port trio seeds the first row after the
	// app exists.
	var firstDomain *core.ComposeDomain
	if kind == core.KindDockerfile {
		verr := ""
		if app.DockerfilePath, verr = parseDockerfilePath(r); verr != "" {
			s.renderAppsWithError(w, r, verr, name, repo, domain)
			return
		}
	}
	if kind == core.KindCompose {
		app.Domain = ""
		verr := ""
		if app.ComposePath, verr = parseComposePath(r); verr != "" {
			s.renderAppsWithError(w, r, verr, name, repo, domain)
			return
		}
		if domain != "" {
			d, verr := parseDomainFields(domain, r.FormValue("compose_service"), r.FormValue("compose_port"))
			if verr != "" {
				s.renderAppsWithError(w, r, verr, name, repo, domain)
				return
			}
			firstDomain = &d
		}
	}

	if verr := validateApp(app); verr != "" {
		s.renderAppsWithError(w, r, verr, name, repo, domain)
		return
	}

	if source == core.SourceSSH {
		priv, pub, err := sshkey.Generate()
		if err != nil {
			s.renderAppsWithError(w, r, "Could not generate SSH key: "+err.Error(), name, repo, domain)
			return
		}
		app.SSHPrivateKey, app.SSHPublicKey = priv, pub
	}

	created, err := s.store.CreateApp(r.Context(), app)
	if err != nil {
		// Most likely a duplicate name (UNIQUE constraint).
		s.renderAppsWithError(w, r, "Could not create app: "+err.Error(), name, repo, domain)
		return
	}
	if firstDomain != nil {
		firstDomain.AppID = created.ID
		if _, err := s.store.AddComposeDomain(r.Context(), *firstDomain); err != nil {
			// The app row exists; surface the domain failure instead of hiding it.
			s.renderAppsWithError(w, r,
				"App created, but the domain could not be added: "+err.Error(), name, repo, domain)
			return
		}
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

// handleAppSettings updates the mutable per-app deploy settings: branch,
// auto-deploy-on-push, and watch paths for every app; plus the compose file
// path for compose apps and the Dockerfile path for dockerfile apps. Compose
// domains have their own endpoints.
func (s *Server) handleAppSettings(w http.ResponseWriter, r *http.Request) {
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
	branch := strings.TrimSpace(r.FormValue("branch"))
	if branch == "" {
		branch = "main"
	}
	autoDeploy := r.FormValue("auto_deploy") != ""
	watchPaths := parseWatchPaths(r.FormValue("watch_paths"))
	if err := s.store.UpdateAppSettings(r.Context(), id, branch, autoDeploy, watchPaths); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if app.Kind == core.KindCompose {
		composePath, verr := parseComposePath(r)
		if verr != "" {
			http.Error(w, verr, http.StatusBadRequest)
			return
		}
		if err := s.store.UpdateAppComposePath(r.Context(), id, composePath); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	if app.Kind == core.KindDockerfile {
		dockerfilePath, verr := parseDockerfilePath(r)
		if verr != "" {
			http.Error(w, verr, http.StatusBadRequest)
			return
		}
		if err := s.store.UpdateAppDockerfilePath(r.Context(), id, dockerfilePath); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
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
	// Errors are tolerated here: this page is already reporting a form error,
	// so render it with whatever rows could be built.
	rows, _ := s.appRows(r.Context(), apps, projectNames)
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
	// Runtime state: nixpacks apps have one canonical container; compose apps
	// have a stack of them, enumerated via the compose project label.
	runtimeState := "absent"
	type serviceRow struct {
		Service string
		State   string
	}
	var stack []serviceRow
	var domains []core.ComposeDomain
	if app.Kind == core.KindCompose {
		if domains, err = s.store.ListComposeDomains(r.Context(), id); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		cs, err := s.runtime.ListContainers(r.Context(),
			map[string]string{"com.docker.compose.project": compose.ProjectName(app.Name)})
		if err == nil && len(cs) > 0 {
			running := 0
			for _, c := range cs {
				svc := c.Labels["com.docker.compose.service"]
				if svc == "" {
					svc = c.Name
				}
				stack = append(stack, serviceRow{Service: svc, State: c.State})
				if c.Running() {
					running++
				}
			}
			sort.Slice(stack, func(i, j int) bool { return stack[i].Service < stack[j].Service })
			runtimeState = fmt.Sprintf("%d/%d running", running, len(cs))
		}
	} else if c, err := s.runtime.FindContainer(r.Context(), appContainerPrefix+app.Name); err == nil && c != nil {
		runtimeState = c.State
	}
	data := map[string]any{
		"Title":       app.Name,
		"Active":      "apps",
		"App":         app,
		"Deployments": deployments,
		"Env":         maskEnv(envVars),
		"Runtime":     runtimeState,
		"Stack":       stack,
		"Domains":     domains,
	}
	// Breadcrumb context; tolerate a missing project rather than 500 the page.
	if p, err := s.store.GetProject(r.Context(), app.ProjectID); err == nil {
		data["Project"] = p
	}
	if s.publicURL != "" {
		data["WebhookURL"] = strings.TrimRight(s.publicURL, "/") + "/webhooks/app/" + app.WebhookSecret
	}
	// Volume backups only make sense for compose stacks; nixpacks apps are
	// stateless, so their page omits the panel.
	if app.Kind == core.KindCompose {
		panel, err := s.backupPanelData(r.Context(), core.BackupTargetApp, app.ID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		for k, v := range panel {
			data[k] = v
		}
	}
	s.render(w, http.StatusOK, "app", data)
}

// handleAddComposeDomain publishes a stack service on another domain. The row
// is stored immediately but only reaches Traefik on the next deploy, when the
// override file is regenerated — the same contract as Dokploy.
func (s *Server) handleAddComposeDomain(w http.ResponseWriter, r *http.Request) {
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
	if app.Kind != core.KindCompose {
		http.Error(w, "Domains can only be added to compose apps; edit a nixpacks app's single domain instead.", http.StatusBadRequest)
		return
	}
	d, verr := parseDomainFields(r.FormValue("domain"), r.FormValue("service"), r.FormValue("port"))
	if verr != "" {
		http.Error(w, verr, http.StatusBadRequest)
		return
	}
	d.AppID = id
	if _, err := s.store.AddComposeDomain(r.Context(), d); err != nil {
		// Most likely the UNIQUE(app_id, domain) constraint.
		http.Error(w, "Could not add domain (is it already configured?): "+err.Error(), http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, "/apps/"+strconv.FormatInt(id, 10), http.StatusSeeOther)
}

func (s *Server) handleDeleteComposeDomain(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(r.PathValue("id"))
	domainID, ok2 := parseID(r.PathValue("domainID"))
	if !ok || !ok2 {
		http.NotFound(w, r)
		return
	}
	if _, err := s.store.GetApp(r.Context(), id); err != nil {
		http.NotFound(w, r)
		return
	}
	if err := s.store.DeleteComposeDomain(r.Context(), id, domainID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/apps/"+strconv.FormatInt(id, 10), http.StatusSeeOther)
}

// envRow is an env var prepared for display: secret values are masked.
type envRow struct {
	Key      string
	Value    string
	IsSecret bool
}

func maskEnv(vars []core.EnvVar) []envRow {
	rows := make([]envRow, 0, len(vars))
	for _, v := range vars {
		row := envRow{Key: v.Key, IsSecret: v.IsSecret}
		if !v.IsSecret {
			row.Value = v.Value
		}
		rows = append(rows, row)
	}
	return rows
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
		http.Error(w, "PORT is managed by Outhaul and cannot be set.", http.StatusBadRequest)
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
	s.appLifecycle(w, r,
		func(ctx context.Context, cid string) error {
			return s.runtime.StopContainer(ctx, cid, 10*time.Second)
		},
		func(ctx context.Context, project string) error {
			return s.compose.Stop(ctx, project, io.Discard)
		})
}

func (s *Server) handleRestartApp(w http.ResponseWriter, r *http.Request) {
	s.appLifecycle(w, r,
		func(ctx context.Context, cid string) error {
			_ = s.runtime.StopContainer(ctx, cid, 10*time.Second) // ignore: already-stopped is fine
			return s.runtime.StartContainer(ctx, cid)
		},
		func(ctx context.Context, project string) error {
			return s.compose.Restart(ctx, project, io.Discard)
		})
}

// appLifecycle runs a stop/restart-style action against an app's runtime:
// nixpacks apps act on their single container (404-equivalent 409 when it
// doesn't exist yet), compose apps act on their stack by project name —
// compose rediscovers the containers from labels, so no files are needed.
func (s *Server) appLifecycle(w http.ResponseWriter, r *http.Request,
	containerAction func(ctx context.Context, containerID string) error,
	stackAction func(ctx context.Context, project string) error) {
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
	if app.Kind == core.KindCompose {
		if err := stackAction(r.Context(), compose.ProjectName(app.Name)); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		http.Redirect(w, r, "/apps/"+strconv.FormatInt(id, 10), http.StatusSeeOther)
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
	if err := containerAction(r.Context(), c.ID); err != nil {
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
	// Best-effort teardown; proceed with the row delete regardless, but log
	// failures so orphaned containers aren't silently lost. Compose stacks go
	// down whole (containers + networks; named volumes deliberately survive —
	// they are data).
	if app.Kind == core.KindCompose {
		if derr := s.compose.Down(r.Context(), compose.ProjectName(app.Name), io.Discard); derr != nil {
			log.Printf("delete app %d: could not tear down compose stack (its containers may be orphaned): %v", id, derr)
		}
	} else if c, ferr := s.runtime.FindContainer(r.Context(), appContainerPrefix+app.Name); ferr != nil {
		log.Printf("delete app %d: could not inspect container (any container is now orphaned): %v", id, ferr)
	} else if c != nil {
		_ = s.runtime.StopContainer(r.Context(), c.ID, 10*time.Second)
		if rerr := s.runtime.RemoveContainer(r.Context(), c.ID, true); rerr != nil {
			log.Printf("delete app %d: could not remove container %s: %v", id, c.ID, rerr)
		}
	}
	// The app's built images go with it (the deployment rows about to be
	// deleted are the only record of the tags). Best-effort like the container
	// teardown: a leftover is reclaimed by the pruner's daily reconciliation.
	if app.Kind != core.KindCompose {
		if deps, derr := s.store.ListDeploymentsForApp(r.Context(), id); derr != nil {
			log.Printf("delete app %d: could not list deployments (its images are now orphaned until the next sweep): %v", id, derr)
		} else {
			for _, tag := range distinctImages(deps) {
				if rerr := s.runtime.RemoveImage(r.Context(), tag); rerr != nil {
					log.Printf("delete app %d: could not remove image %s: %v", id, tag, rerr)
				}
			}
		}
	}
	if err := s.store.DeleteApp(r.Context(), id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/apps", http.StatusSeeOther)
}

// distinctImages collects the unique unpruned image tags across deployments.
func distinctImages(deps []core.Deployment) []string {
	seen := map[string]bool{}
	var tags []string
	for _, d := range deps {
		if d.Image == "" || d.ImagePruned || seen[d.Image] {
			continue
		}
		seen[d.Image] = true
		tags = append(tags, d.Image)
	}
	return tags
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

// handleRollback enqueues a deployment that reuses the source deployment's
// built image instead of cloning and building. Env vars, domain, and routing
// are the app's current settings — only the image is rolled back.
func (s *Server) handleRollback(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	src, err := s.store.GetDeployment(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	app, err := s.store.GetApp(r.Context(), src.AppID)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if app.Kind == core.KindCompose {
		http.Error(w, "compose stacks cannot be rolled back: they have no per-deployment image", http.StatusBadRequest)
		return
	}
	if src.Image == "" {
		http.Error(w, "this deployment never finished a build, so there is no image to roll back to", http.StatusBadRequest)
		return
	}
	if src.ImagePruned {
		http.Error(w, "this deployment's image was pruned by image retention; deploy to rebuild from the branch instead", http.StatusBadRequest)
		return
	}
	dep, err := s.store.CreateRollback(r.Context(), app.ID, src.Image, src.ID)
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
	// Project is for the breadcrumb only; a lookup failure just shortens it.
	project, _ := s.store.GetProject(r.Context(), app.ProjectID)
	s.render(w, http.StatusOK, "deployment", map[string]any{
		"Title":      "Deployment #" + r.PathValue("id"),
		"Active":     "deployments",
		"App":        app,
		"Project":    project,
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
// needs an http(s) URL. A domain is required for single-container apps
// (nixpacks/dockerfile — they exist to be served) but optional for compose
// stacks (which may be internal-only).
func validateApp(app core.App) string {
	if !appNameRe.MatchString(app.Name) {
		return "Name must be lowercase letters, digits and hyphens (2–40 chars)."
	}
	switch app.Kind {
	case core.KindNixpacks, core.KindDockerfile, core.KindCompose:
	default:
		return "Choose a deploy method."
	}
	if app.Kind != core.KindCompose && app.Domain == "" {
		return "Domain must be a bare hostname (e.g. app.example.com)."
	}
	if app.Domain != "" && strings.ContainsAny(app.Domain, " /") {
		return "Domain must be a bare hostname (e.g. app.example.com)."
	}
	switch app.Source {
	case core.SourceGithub:
		if !strings.Contains(app.GithubRepo, "/") {
			return "Select a GitHub repository (owner/name)."
		}
	case core.SourceSSH:
		if app.RepoURL == "" || strings.HasPrefix(app.RepoURL, "-") || strings.ContainsAny(app.RepoURL, " \t") {
			return "Repository must be an SSH clone URL (e.g. git@github.com:owner/repo.git)."
		}
		if !strings.HasPrefix(app.RepoURL, "ssh://") && !(strings.Contains(app.RepoURL, "@") && strings.Contains(app.RepoURL, ":")) {
			return "Repository must be an SSH clone URL (e.g. git@github.com:owner/repo.git or ssh://…)."
		}
	default: // public
		if app.RepoURL == "" || !(strings.HasPrefix(app.RepoURL, "http://") || strings.HasPrefix(app.RepoURL, "https://")) {
			return "Repository must be a public http(s) Git URL."
		}
	}
	return ""
}

// parseComposePath reads and normalizes the compose_path form field.
func parseComposePath(r *http.Request) (string, string) {
	p, ok := cleanRepoPath(r.FormValue("compose_path"), "docker-compose.yml")
	if !ok {
		return "", "Compose file must be a relative path inside the repository (e.g. docker-compose.yml)."
	}
	return p, ""
}

// parseDockerfilePath reads and normalizes the dockerfile_path form field.
func parseDockerfilePath(r *http.Request) (string, string) {
	p, ok := cleanRepoPath(r.FormValue("dockerfile_path"), "Dockerfile")
	if !ok {
		return "", "Dockerfile must be a relative path inside the repository (e.g. Dockerfile)."
	}
	return p, ""
}

// parseDomainFields validates one domain→service:port publication. Publishing
// needs all three: the host Traefik matches, the compose service it routes
// to, and the container port that service listens on.
func parseDomainFields(domain, service, portStr string) (core.ComposeDomain, string) {
	domain = strings.TrimSpace(domain)
	if domain == "" || strings.ContainsAny(domain, " /") {
		return core.ComposeDomain{}, "Domain must be a bare hostname (e.g. app.example.com)."
	}
	if !composeServiceRe.MatchString(strings.TrimSpace(service)) {
		return core.ComposeDomain{}, "Name the compose service to expose on the domain (e.g. web)."
	}
	port, err := strconv.Atoi(strings.TrimSpace(portStr))
	if err != nil || port < 1 || port > 65535 {
		return core.ComposeDomain{}, "Service port must be a number between 1 and 65535."
	}
	return core.ComposeDomain{Domain: domain, Service: strings.TrimSpace(service), Port: port}, ""
}

// cleanRepoPath normalizes a repo-relative file path (empty = fallback),
// rejecting anything that could escape the clone (absolute paths, ".."
// elements).
func cleanRepoPath(p, fallback string) (string, bool) {
	p = strings.TrimSpace(strings.ReplaceAll(p, "\\", "/"))
	if p == "" {
		p = fallback
	}
	p = path.Clean(p)
	if path.IsAbs(p) || p == "." || p == ".." || strings.HasPrefix(p, "../") {
		return "", false
	}
	return p, true
}

// parseWatchPaths splits a textarea (one glob per line) into patterns.
func parseWatchPaths(v string) []string {
	var out []string
	for _, line := range strings.Split(v, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	return out
}
