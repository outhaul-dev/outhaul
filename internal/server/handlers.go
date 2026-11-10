package server

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/outhaul-dev/outhaul/internal/compose"
	"github.com/outhaul-dev/outhaul/internal/core"
	"github.com/outhaul-dev/outhaul/internal/docker"
	"github.com/outhaul-dev/outhaul/internal/gitsource"
	"github.com/outhaul-dev/outhaul/internal/sshkey"
	"github.com/outhaul-dev/outhaul/internal/store"
	"github.com/outhaul-dev/outhaul/internal/webhook"
)

// appContainerPrefix is prepended to an app's name to get its container name.
const appContainerPrefix = core.AppContainerPrefix

// appNameRe restricts app names to values safe as container names, Traefik
// router identifiers, and URL segments.
var appNameRe = core.AppNameRe

// envKeyRe restricts env var names to conventional shell identifiers.
var envKeyRe = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)

// composeServiceRe matches compose service names (the compose spec's own rule).
var composeServiceRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)

// domainHostRe matches a bare, lowercase hostname (dot-separated DNS labels).
var domainHostRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)*$`)

// urlPathRe matches a safe absolute URL path prefix (no backticks, spaces, or
// other characters that could corrupt a Traefik rule).
var urlPathRe = regexp.MustCompile(`^/[A-Za-z0-9._~%/-]*$`)

// mountPathRe matches a safe absolute container mount path (no backticks,
// spaces, or shell metacharacters); "" and ".." are rejected by the caller.
var mountPathRe = regexp.MustCompile(`^/[A-Za-z0-9._/-]+$`)

// deployAppPort mirrors deploy.AppPort — the port single-container apps listen
// on — for domain rows created without a compose service.
const deployAppPort = 8080

func (s *Server) handleAppsList(w http.ResponseWriter, r *http.Request) {
	scope, err := s.scopeFor(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	apps, err := s.scopedApps(r.Context(), scope)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	projects, err := s.scopedProjects(r.Context(), scope)
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
	for k, v := range s.gitSourceData(r) {
		data[k] = v
	}
	s.render(w, r, http.StatusOK, "apps", data)
}

// appRow is one row of an app-list table: the app plus its display context.
type appRow struct {
	core.App
	ProjectName string
	Domains     []core.Domain // the app's published domains
	Latest      *core.Deployment
}

// appRows decorates apps with their project name, latest deployment status,
// and published domains. projectNames may be nil when the listing is already
// scoped to one project.
func (s *Server) appRows(ctx context.Context, apps []core.App, projectNames map[int64]string) ([]appRow, error) {
	rows := make([]appRow, 0, len(apps))
	for _, a := range apps {
		latest, err := s.store.LatestDeploymentForApp(ctx, a.ID)
		if err != nil {
			return nil, err
		}
		row := appRow{App: a, ProjectName: projectNames[a.ProjectID], Latest: latest}
		if row.Domains, err = s.store.ListDomains(ctx, a.ID); err != nil {
			return nil, err
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// repoCache memoizes one source's repo list. Listing costs two sequential
// api.github.com round-trips (token exchange + list), and every app/create/
// project render fills a repo dropdown — without this, each page open paid that
// latency per connected account, and blocked on GitHub being reachable.
type repoCache struct {
	repos     []gitsource.Repo
	fetchedAt time.Time
}

// ghRepoTTL is how long a fetched repo list is served before refetching. Short
// enough that a newly-created repo shows up promptly; long enough that rapid
// navigation doesn't hammer the API.
const ghRepoTTL = 60 * time.Second

// cachedRepos returns the memoized repo list for a source and whether it is
// still fresh. Stale-but-present entries are returned too (fresh=false) so
// callers can fall back to them when a refetch fails.
func (s *Server) cachedRepos(sourceID int64) (repos []gitsource.Repo, fresh bool) {
	s.ghReposMu.Lock()
	defer s.ghReposMu.Unlock()
	c := s.ghRepos[sourceID]
	if c == nil {
		return nil, false
	}
	return c.repos, time.Since(c.fetchedAt) < ghRepoTTL
}

func (s *Server) storeRepos(sourceID int64, repos []gitsource.Repo) {
	s.ghReposMu.Lock()
	defer s.ghReposMu.Unlock()
	if s.ghRepos == nil {
		s.ghRepos = map[int64]*repoCache{}
	}
	s.ghRepos[sourceID] = &repoCache{repos: repos, fetchedAt: time.Now()}
}

// repoGroup is one connected account's repositories, rendered as an <optgroup>.
type repoGroup struct {
	SourceID     int64
	AccountLogin string
	AccountKind  string // "personal" | "org" | ""
	Repos        []gitsource.Repo
}

// gitSourceData describes connected git sources for the create-app and
// change-source forms: "GitSourceConnected" when at least one source is
// installed, and "RepoGroups" — every account's repos, in one list the operator
// picks from directly.
//
// Each source degrades on its own: a source whose fetch fails falls back to its
// stale cache, or drops out. One unreachable account must never blank the whole
// dropdown, which is what a single shared cache used to do.
func (s *Server) gitSourceData(r *http.Request) map[string]any {
	data := map[string]any{}
	sources, err := s.store.ListGitSources(r.Context())
	if err != nil {
		log.Printf("git sources: %v", err)
		return data
	}
	var groups []repoGroup
	for _, src := range sources {
		if !src.Installed() {
			continue
		}
		data["GitSourceConnected"] = true
		repos, ok := s.reposFor(r.Context(), src)
		if !ok || len(repos) == 0 {
			continue
		}
		groups = append(groups, repoGroup{
			SourceID: src.ID, AccountLogin: src.Display(),
			AccountKind: src.AccountKind(), Repos: repos,
		})
	}
	if len(groups) > 0 {
		data["RepoGroups"] = groups
	}
	return data
}

// reposFor returns a source's repositories, preferring a fresh cache and
// falling back to a stale one when the refetch fails.
func (s *Server) reposFor(ctx context.Context, src core.GitSource) ([]gitsource.Repo, bool) {
	cached, fresh := s.cachedRepos(src.ID)
	if fresh {
		return cached, true
	}
	provider, err := s.sources.For(src.Kind)
	if err != nil {
		log.Printf("git source %s: provider unavailable: %v", src.Display(), err)
		return cached, cached != nil
	}
	repos, err := provider.Repos(ctx, src)
	if err != nil {
		log.Printf("git source %s: list repos: %v", src.Display(), err)
		// Re-stamp the cache with what we had (possibly nil) so ghRepoTTL
		// rate-limits the retry instead of re-fetching on every render — a
		// permanently-unreachable source would otherwise pay the ~60s worst
		// case (two sequential api.github.com calls) on every page load.
		s.storeRepos(src.ID, cached)
		return cached, cached != nil
	}
	s.storeRepos(src.ID, repos)
	return repos, true
}

// resolveRepoSource checks that a submitted (git source, repo) pair is real:
// the source must exist and be installed, and — when we hold a fresh repo list
// for it — must actually contain the repo. A stale or missing cache accepts the
// pair rather than blocking on GitHub; a wrong pair then fails loudly at clone
// time instead of silently deploying someone else's repo.
func (s *Server) resolveRepoSource(ctx context.Context, sourceID int64, fullName string) (int64, string) {
	if sourceID == 0 {
		return 0, "Choose a repository from a connected GitHub account."
	}
	src, ok, err := s.store.GetGitSource(ctx, sourceID)
	if err != nil {
		return 0, "Could not read the connected GitHub account."
	}
	if !ok || !src.Installed() {
		return 0, "That GitHub account is not connected. Reconnect it in Settings."
	}
	if repos, fresh := s.cachedRepos(sourceID); fresh {
		for _, repo := range repos {
			if repo.FullName == fullName {
				return sourceID, ""
			}
		}
		return 0, "That repository is not available from the selected GitHub account."
	}
	return sourceID, ""
}

func (s *Server) handleCreateApp(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.FormValue("name"))
	domain := strings.ToLower(strings.TrimSpace(r.FormValue("domain")))
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
	var gitSourceID int64
	if source == core.SourceGithub {
		id, _ := parseID(strings.TrimSpace(r.FormValue("git_source_id")))
		resolved, verr := s.resolveRepoSource(r.Context(), id, githubRepo)
		if verr != "" {
			s.renderAppsWithError(w, r, verr)
			return
		}
		gitSourceID = resolved
		repo = "https://github.com/" + githubRepo + ".git"
	}
	if source == core.SourcePush {
		// The Git URL box is only hidden for a push app, so it may still
		// carry a URL typed before the source changed.
		repo = ""
	}

	projectID := core.DefaultProjectID
	if v := strings.TrimSpace(r.FormValue("project_id")); v != "" {
		id, ok := parseID(v)
		if !ok {
			s.renderAppsWithError(w, r, "Choose a project for the app.")
			return
		}
		projectID = id
	}
	// No DB-level FK on apps.project_id, so the reference is validated here.
	if _, err := s.store.GetProject(r.Context(), projectID); err != nil {
		s.renderAppsWithError(w, r, "Choose a project for the app.")
		return
	}

	app := core.App{
		Name: name, RepoURL: repo, Domain: domain, Source: source, Kind: kind,
		Branch: branch, AutoDeploy: autoDeploy, GithubRepo: githubRepo, GitSourceID: gitSourceID,
		WebhookToken: newSecret(), WebhookSecret: newSecret(), ProjectID: projectID,
	}
	// Compose domains live in their own table (a stack can have many); the
	// form's optional domain/service/port trio seeds the first row after the
	// app exists.
	var firstDomain *core.Domain
	if kind == core.KindDockerfile {
		verr := ""
		if app.DockerfilePath, verr = parseDockerfilePath(r); verr != "" {
			s.renderAppsWithError(w, r, verr)
			return
		}
	}
	if kind == core.KindCompose {
		app.Domain = ""
		verr := ""
		if app.ComposePath, verr = parseComposePath(r); verr != "" {
			s.renderAppsWithError(w, r, verr)
			return
		}
		if domain != "" {
			d, verr := parseDomainFields(domain, r.FormValue("compose_service"), r.FormValue("compose_port"))
			if verr != "" {
				s.renderAppsWithError(w, r, verr)
				return
			}
			firstDomain = &d
		}
	}

	if verr := validateApp(app); verr != "" {
		s.renderAppsWithError(w, r, verr)
		return
	}
	if s.isOwnHost(app.Domain) || firstDomain != nil && s.isOwnHost(firstDomain.Host) {
		s.renderAppsWithError(w, r, ownHostRefusal)
		return
	}

	if source == core.SourceSSH {
		priv, pub, err := sshkey.Generate()
		if err != nil {
			s.renderAppsWithError(w, r, "Could not generate SSH key: "+err.Error())
			return
		}
		app.SSHPrivateKey, app.SSHPublicKey = priv, pub
	}

	// A compose app's first route is created with it, so a refused route (say,
	// a host another app serves) leaves no half-created app behind.
	var extra []core.Domain
	if firstDomain != nil {
		extra = []core.Domain{*firstDomain}
	}
	if _, err := s.store.CreateAppWithDomains(r.Context(), app, extra); err != nil {
		if errors.Is(err, store.ErrHostReserved) {
			s.renderAppsWithError(w, r, ownHostRefusal)
			return
		}
		// Most likely a duplicate name (UNIQUE constraint) or a taken host.
		s.renderAppsWithError(w, r, "Could not create app: "+err.Error())
		return
	}
	// CreateApp itself seeds a primary domain row for non-compose apps (see
	// store.CreateApp); compose apps seed one just above via firstDomain. Either
	// way a new domain may now need a cert, so sync once here regardless of kind.
	s.syncCerts(r.Context())
	// The project-detail form asks to land back on a project: the one the app
	// joined, which a form refused and reopened on the apps page may have
	// changed. Anything else falls back to the apps list.
	ret := "/apps"
	if strings.HasPrefix(r.FormValue("return"), "/projects/") {
		ret = "/projects/" + strconv.FormatInt(projectID, 10)
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
	trigger := strings.TrimSpace(r.FormValue("deploy_trigger"))
	if trigger == "" {
		trigger = core.TriggerBranch
	}
	tagPattern := strings.TrimSpace(r.FormValue("tag_pattern"))
	switch trigger {
	case core.TriggerBranch:
		if tagPattern == "" {
			tagPattern = core.DefaultTagPattern
		}
	case core.TriggerTag:
		switch {
		case app.Source == core.SourceTemplate || app.TemplateID != "" || app.ParentID != 0:
			http.Error(w, "This app can't deploy on tags.", http.StatusBadRequest)
			return
		case tagPattern == "":
			http.Error(w, "A tag pattern is required to deploy on tags.", http.StatusBadRequest)
			return
		case strings.ContainsAny(tagPattern, " \t\r\n"):
			http.Error(w, "A tag pattern can't contain spaces.", http.StatusBadRequest)
			return
		case !webhook.ValidGlob(tagPattern):
			http.Error(w, "The tag pattern has an unclosed [.", http.StatusBadRequest)
			return
		}
	default:
		http.Error(w, "unknown deploy trigger", http.StatusBadRequest)
		return
	}
	if err := s.store.UpdateAppSettings(r.Context(), id, store.DeploySettings{
		Branch: branch, AutoDeploy: autoDeploy, WatchPaths: watchPaths,
		Trigger: trigger, TagPattern: tagPattern, TagPrereleases: r.FormValue("tag_prereleases") != "",
	}); err != nil {
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

// handleUpdateAppKind changes an app's build strategy (nixpacks/dockerfile/
// compose). The single-container <-> compose boundary is guarded in the UI by a
// typed confirm; the server validates the target kind and persists the path the
// new kind needs. Takes effect on the next deploy.
func (s *Server) handleUpdateAppKind(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	if _, err := s.store.GetApp(r.Context(), id); err != nil {
		http.NotFound(w, r)
		return
	}
	kind := strings.TrimSpace(r.FormValue("kind"))
	switch kind {
	case core.KindNixpacks, core.KindDockerfile, core.KindCompose:
	default:
		http.Error(w, "Choose a build method.", http.StatusBadRequest)
		return
	}
	composePath, dockerfilePath := "", ""
	if kind == core.KindCompose {
		p, verr := parseComposePath(r)
		if verr != "" {
			http.Error(w, verr, http.StatusBadRequest)
			return
		}
		composePath = p
	}
	if kind == core.KindDockerfile {
		p, verr := parseDockerfilePath(r)
		if verr != "" {
			http.Error(w, verr, http.StatusBadRequest)
			return
		}
		dockerfilePath = p
	}
	if err := s.store.UpdateAppKind(r.Context(), id, kind, composePath, dockerfilePath); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/apps/"+strconv.FormatInt(id, 10), http.StatusSeeOther)
}

// handleUpdateAppSource re-points an app at a different source (public/ssh/
// github/push), re-running the same wiring the create form uses: github resolves
// owner/name to an https clone URL, ssh regenerates a deploy keypair, push clears
// the repo URL. Template apps have no repo and are rejected. Takes effect on the
// next deploy.
func (s *Server) handleUpdateAppSource(w http.ResponseWriter, r *http.Request) {
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
	if app.TemplateID != "" {
		http.Error(w, "Template apps have no source to change.", http.StatusBadRequest)
		return
	}
	source := strings.TrimSpace(r.FormValue("source"))
	repo := strings.TrimSpace(r.FormValue("repo_url"))
	githubRepo := strings.TrimSpace(r.FormValue("github_repo"))
	var gitSourceID int64
	if source == core.SourceGithub {
		srcID, _ := parseID(strings.TrimSpace(r.FormValue("git_source_id")))
		resolved, verr := s.resolveRepoSource(r.Context(), srcID, githubRepo)
		if verr != "" {
			http.Error(w, verr, http.StatusBadRequest)
			return
		}
		gitSourceID = resolved
		repo = "https://github.com/" + githubRepo + ".git"
	}
	if source == core.SourcePush {
		repo = ""
	}
	if source != core.SourceGithub {
		// gitSourceID is already 0 here (only the github branch above sets it).
		// Clear githubRepo alongside it rather than writing whatever the form
		// submitted — otherwise a stale repo name sits next to git_source_id = 0
		// with nothing left to validate it against.
		githubRepo = ""
	}
	// Validate with the same rules as create: keep the app's existing name/
	// domain/kind (already valid) and swap in the new source fields.
	check := app
	check.Source, check.RepoURL, check.GithubRepo = source, repo, githubRepo
	// Domains stored before strict validation may be mixed-case; Traefik
	// matches Host case-insensitively, so judge the host, not its spelling.
	check.Domain = strings.ToLower(check.Domain)
	if verr := validateApp(check); verr != "" {
		http.Error(w, verr, http.StatusBadRequest)
		return
	}
	var pub, priv string
	if source == core.SourceSSH {
		priv, pub, err = sshkey.Generate()
		if err != nil {
			http.Error(w, "Could not generate SSH key: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}
	if err := s.store.UpdateAppSource(r.Context(), id, source, repo, githubRepo, gitSourceID, pub, priv); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/apps/"+strconv.FormatInt(id, 10), http.StatusSeeOther)
}

// createAppForm is the New app form as submitted. A refused create sends it back,
// so the reopened form keeps every choice: with the source or the project
// back at its default, a resubmit would save an app other than the one asked
// for.
type createAppForm struct {
	Name, Source, RepoURL, GithubRepo, Branch, Kind string
	DockerfilePath, ComposePath                     string
	Domain, ComposeService, ComposePort             string
	GitSourceID                                     int64
	AutoDeploy                                      bool
}

func submittedAppForm(r *http.Request) createAppForm {
	field := func(key string) string { return strings.TrimSpace(r.FormValue(key)) }
	gitSourceID, _ := parseID(field("git_source_id"))
	return createAppForm{
		Name: field("name"), Source: field("source"), RepoURL: field("repo_url"),
		GithubRepo: field("github_repo"), GitSourceID: gitSourceID,
		Branch: field("branch"), Kind: field("kind"),
		DockerfilePath: field("dockerfile_path"), ComposePath: field("compose_path"),
		Domain: field("domain"), ComposeService: field("compose_service"),
		ComposePort: field("compose_port"), AutoDeploy: r.FormValue("auto_deploy") != "",
	}
}

// renderAppsWithError re-renders the apps list with the create form pre-filled
// and an error message.
func (s *Server) renderAppsWithError(w http.ResponseWriter, r *http.Request, msg string) {
	// Errors are tolerated here: see below. A failed scope shows nothing.
	scope, _ := s.scopeFor(r)
	apps, _ := s.scopedApps(r.Context(), scope)
	projects, _ := s.scopedProjects(r.Context(), scope)
	projectNames := make(map[int64]string, len(projects))
	selected := selectedProject(projects)
	submitted, _ := parseID(strings.TrimSpace(r.FormValue("project_id")))
	for _, p := range projects {
		projectNames[p.ID] = p.Name
		if p.ID == submitted {
			selected = p.ID
		}
	}
	// Errors are tolerated here: this page is already reporting a form error,
	// so render it with whatever rows could be built.
	rows, _ := s.appRows(r.Context(), apps, projectNames)
	data := map[string]any{
		"Title": "Apps", "Active": "apps", "Apps": rows,
		"Projects": projects, "SelectedProject": selected,
		"Error":      msg,
		"Form":       submittedAppForm(r),
		"OpenDialog": "app-dialog",
	}
	if ret := r.FormValue("return"); strings.HasPrefix(ret, "/projects/") {
		data["Return"] = ret
	}
	for k, v := range s.gitSourceData(r) {
		data[k] = v
	}
	s.render(w, r, http.StatusBadRequest, "apps", data)
}

func (s *Server) handleAppDetail(w http.ResponseWriter, r *http.Request) {
	s.renderAppPage(w, r, http.StatusOK, "", "")
}

// changedElsewhereNotice answers an edit from a dialog filled before the
// row's protection changed.
const changedElsewhereNotice = "This domain was changed by someone else. Check it and try again."

// refuseStaleDomainSave answers a domain save from a page that no longer
// matches the rows: nothing was saved, and the app page is rendered again,
// as it is now, on its Networking tab, with notice at the top.
func (s *Server) refuseStaleDomainSave(w http.ResponseWriter, r *http.Request, notice string) {
	s.renderAppPage(w, r, http.StatusConflict, notice, "")
}

// renderAppPage renders the app page of the app in the request's {id}, with
// status. A non-empty notice is shown at the top, and opens the Networking
// tab: only domain saves send one. The domain dialog names the other apps a
// save requiring sign-in would redeploy on the app's own hosts, and on
// alsoHost when it isn't "": the host of a refused save, so its page can
// warn of them and send them back.
func (s *Server) renderAppPage(w http.ResponseWriter, r *http.Request, status int, notice, alsoHost string) {
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
	domains, err := s.store.ListDomains(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var volumes []core.Volume
	if app.Kind != core.KindCompose {
		volumes, err = s.store.ListVolumes(r.Context(), id)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	if app.Kind == core.KindCompose {
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
	attachments, err := s.store.ListAttachments(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	projDatabases, err := s.store.ListDatabasesByProject(r.Context(), app.ProjectID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	dbName := map[int64]string{}
	for _, d := range projDatabases {
		dbName[d.ID] = d.Name
	}
	type attachRow struct {
		ID     int64
		EnvVar string
		DBName string
	}
	attachRows := make([]attachRow, 0, len(attachments))
	for _, a := range attachments {
		attachRows = append(attachRows, attachRow{ID: a.ID, EnvVar: a.EnvVar, DBName: dbName[a.DatabaseID]})
	}
	// Preview environments: config + active previews. Only GitHub-connected apps
	// can host previews; other kinds get a zero config, no rows, and the section
	// is hidden. The keys are always set so the template never errors on any kind.
	previewCfg, err := s.store.GetPreviewConfig(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	type previewRow struct {
		ID     int64 // child app id (for destroy)
		PR     int
		Status string
		URL    string
	}
	var previewRows []previewRow
	if app.Source == core.SourceGithub {
		kids, err := s.store.ListPreviewsForParent(r.Context(), id)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		protectedHTTPS := s.protectedOverHTTPS(r.Context())
		for _, k := range kids {
			url := ""
			if ds, _ := s.store.ListDomains(r.Context(), k.ID); len(ds) > 0 {
				scheme := "http://"
				if ds[0].TLS || ds[0].Protected && protectedHTTPS {
					scheme = "https://"
				}
				url = scheme + ds[0].Host
			}
			previewRows = append(previewRows, previewRow{ID: k.ID, PR: k.PRNumber, Status: k.PreviewStatus, URL: url})
		}
	}
	// A viewer gets the page without anything that could be used to act on the
	// app: the template gets a copy with the webhook credentials removed, and
	// masked variables.
	canChange := s.canChange(r)
	shownApp, shownEnv := app, maskEnv(envVars)
	if !canChange {
		shownApp.WebhookSecret, shownApp.WebhookToken = "", ""
		shownEnv = viewerEnv(shownEnv)
	}
	data := map[string]any{
		"Title":         app.Name,
		"Active":        "apps",
		"App":           shownApp,
		"Deployments":   deployments,
		"Env":           shownEnv,
		"Runtime":       runtimeState,
		"Stack":         stack,
		"Domains":       domains,
		"Volumes":       volumes,
		"Attachments":   attachRows,
		"ProjDatabases": projDatabases,
		"ServerIP":      s.serverIP,
		"TLSAvailable":  s.tlsEnabled,
		"LocalCA":       s.caFile != "",

		"PreviewsAvailable": app.Source == core.SourceGithub,
		"PreviewConfig":     previewCfg,
		"Previews":          previewRows,
	}
	// Breadcrumb context; tolerate a missing project rather than 500 the page.
	if p, err := s.store.GetProject(r.Context(), app.ProjectID); err == nil {
		data["Project"] = p
	}
	if canChange && s.publicURL != "" && app.WebhookToken != "" {
		data["WebhookURL"] = strings.TrimRight(s.publicURL, "/") + "/webhooks/app/" + app.WebhookToken
	}
	// Repo-backed apps without a token (created by a cold git push, then moved
	// to a public or SSH repo) can enable a webhook from the page.
	data["WebhookEnable"] = canChange && s.publicURL != "" && app.WebhookToken == "" &&
		(app.Source == core.SourcePublic || app.Source == core.SourceSSH)
	if canChange && app.Source == core.SourcePush {
		data["PushRemote"] = s.pushRemote(app.Name)
		data["PushBranch"] = app.Branch
	}
	// Volume backups apply to compose stacks and to single-container apps that
	// have attached a persistent volume; stateless apps omit the panel. A
	// schedule created while a volume was attached outlives its volume, so keep
	// the panel whenever one still exists — otherwise a lingering schedule keeps
	// ticking with no UI left to pause or remove it.
	backups, err := s.store.ListBackupsForTarget(r.Context(), core.BackupTargetApp, app.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if app.Kind == core.KindCompose || len(volumes) > 0 || len(backups) > 0 {
		panel, err := s.backupPanelData(r.Context(), core.BackupTargetApp, app.ID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		for k, v := range panel {
			data[k] = v
		}
	}
	// The newest deployment in RUNNING state (terminal success) is the one that
	// is actually live; older running rows are superseded. deployments is
	// newest-first, so the first running row wins.
	var liveID int64
	for _, d := range deployments {
		if d.Status == core.StatusRunning {
			liveID = d.ID
			break
		}
	}
	data["LiveDeploymentID"] = liveID
	// The domain dialog's "Require sign-in" switch: whether it can be turned on,
	// and what a save that redeploys would ship. A protected row whose sign-in
	// stopped being available keeps its gate until it is turned off, and the
	// page says so. An open row on a host that requires sign-in redeploys when
	// it is removed or moved (OpenOnProtectedHost). When the containers'
	// labels disagree with the rows, Restart deploys instead.
	protectOK, protectReason := s.protectionAvailability(r.Context(), core.Domain{})
	data["ProtectionAvailable"] = protectOK
	data["ProtectionReason"] = protectReason
	for _, d := range domains {
		if d.Protected && !protectOK {
			data["ProtectionUnreachable"] = true
		}
	}
	openOnProtected := map[int64]bool{}
	data["OpenOnProtectedHost"] = openOnProtected
	data["ProtectedHTTPS"] = s.protectedOverHTTPS(r.Context())
	// The Access column reads the running containers' labels, for viewers too.
	cs, csErr := s.appContainers(r.Context(), app)
	if csErr != nil {
		log.Printf("app page: list the containers of %q: %v", app.Name, csErr)
	}
	if canChange {
		data["ProtectionWarning"] = protectionWarningFor(app, deployments, canonicalImage(app, cs))
		// Other apps a save requiring sign-in on one of the app's hosts would
		// redeploy, named in the dialog before the save as far as the person
		// can see their projects; the dialog sends back which it named.
		hosts := map[string]bool{}
		for _, d := range domains {
			hosts[strings.ToLower(d.Host)] = true
		}
		if alsoHost != "" {
			hosts[strings.ToLower(alsoHost)] = true
		}
		scope, err := s.scopeFor(r)
		if err != nil {
			log.Printf("app page: read the person's projects: %v", err)
			scope = projectScope{} // names no other app
		}
		others, err := s.leftoverRoutes(r.Context(), app, hosts, scope)
		if err != nil {
			log.Printf("app page: other apps' old routes: %v", err)
		}
		// Its previews get the same protection, and are named the same way.
		previews, err := s.previewRedeploys(r.Context(), app)
		if err != nil {
			log.Printf("app page: the previews of %q: %v", app.Name, err)
		}
		keys := make([]string, 0, len(others)+len(previews))
		for _, list := range [][]otherRedeploy{others, previews} {
			for _, o := range list {
				keys = append(keys, o.key())
			}
		}
		data["AlsoRedeploys"] = alsoLines(others)
		data["PreviewRedeploys"] = previews
		data["HasPreviews"] = len(previews) > 0
		data["OthersWas"] = strings.Join(keys, ",")
		protectedHosts, err := s.store.ProtectedHosts(r.Context())
		if err != nil {
			// Warn of a redeploy on every open row rather than on none.
			log.Printf("app page: read the protected hosts: %v", err)
			protectedHosts = map[string]bool{}
			for _, d := range domains {
				protectedHosts[strings.ToLower(d.Host)] = true
			}
		}
		for _, d := range domains {
			if openOnProtectedHost(d, protectedHosts) {
				openOnProtected[d.ID] = true
			}
		}
		if csErr == nil && len(cs) > 0 && protectionMismatch(domains, app.Name, cs, protectedHosts) {
			data["RestartDeploys"] = true
		}
	}
	states := accessStates(domains, app.Name, cs, csErr, deployments)
	if csErr == nil {
		s.strayAccess(r, domains, app.Name, states)
	}
	data["AccessStates"] = states
	// GitHub connectivity for the Settings "Source & build" editor (repo dropdown).
	// Degrades gracefully to no dropdown when no App is connected.
	for k, v := range s.gitSourceData(r) {
		data[k] = v
	}
	// If this app's own (source, repo) pair isn't in any rendered group — its
	// source failed to fetch, was uninstalled, or the repo list is stale — no
	// <option> matches it and the browser would silently select the first repo
	// of a *different* account. Render the app's real pair as its own optgroup
	// so "Change source" round-trips instead of moving the app to another
	// account's repo. This also covers the case where RepoGroups is empty
	// entirely: without it, "GitHub App" could be selected with no repo field.
	if app.Source == core.SourceGithub && app.GithubRepo != "" {
		found := false
		if groups, ok := data["RepoGroups"].([]repoGroup); ok {
			for _, g := range groups {
				if g.SourceID != app.GitSourceID {
					continue
				}
				for _, repo := range g.Repos {
					if repo.FullName == app.GithubRepo {
						found = true
					}
				}
			}
		}
		if !found {
			label := "current account unavailable"
			if src, ok, err := s.store.GetGitSource(r.Context(), app.GitSourceID); err == nil && ok {
				label = src.Display() + " (unavailable)"
			}
			data["CurrentRepoUnlisted"] = true
			data["CurrentRepoLabel"] = label
		}
	}
	if notice != "" {
		data["Notice"] = notice
		data["OpenTab"] = "networking"
	}
	s.render(w, r, status, "app", data)
}

// handleAddDomain publishes a route on an app. Compose apps name a service and
// port; single-container apps route to their container on AppPort with no
// service. An open row reaches Traefik on the app's next deploy; a protected
// one redeploys a running app at once (see protection.go).
func (s *Server) handleAddDomain(w http.ResponseWriter, r *http.Request) {
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
	d, verr := s.parseDomainForm(r, app)
	if verr != "" {
		http.Error(w, verr, http.StatusBadRequest)
		return
	}
	d.AppID = id
	// From here the save and the deploy that applies it finish together, even
	// if the browser goes: the reconcile can recreate the Traefik it came through.
	ctx := context.WithoutCancel(r.Context())
	var others []otherRedeploy
	if r.FormValue("protected") != "" {
		if ok, reason := s.protectionAvailability(ctx, d); !ok {
			http.Error(w, reason, http.StatusBadRequest)
			return
		}
		d.Protected = true
		if others, ok = s.othersToRedeploy(ctx, w, r, app, d, true); !ok {
			return
		}
	}
	added, err := s.store.AddDomain(ctx, d)
	if err != nil {
		if errors.Is(err, store.ErrHostReserved) {
			http.Error(w, ownHostRefusal, http.StatusBadRequest)
			return
		}
		http.Error(w, "Could not add domain (is it already configured?): "+err.Error(), http.StatusBadRequest)
		return
	}
	if d.Protected {
		if err := s.ensureGateHost(ctx); err != nil {
			undo := s.store.DeleteDomain(ctx, id, added.ID)
			s.refuseProtection(w, err, undo)
			return
		}
	}
	s.finishDomainSave(ctx, w, r, app, d.Protected, others, d.Protected)
}

// handleUpdateDomain rewrites an existing route in place.
func (s *Server) handleUpdateDomain(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(r.PathValue("id"))
	domainID, ok2 := parseID(r.PathValue("domainID"))
	if !ok || !ok2 {
		http.NotFound(w, r)
		return
	}
	app, err := s.store.GetApp(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	existing, err := s.store.GetDomain(r.Context(), id, domainID)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	// The dialog says what it was filled from. When the row's protection has
	// changed since, its switch shows the old state: saving it could open a
	// protected row, or deploy, with no warning shown.
	offered := r.FormValue("protected_offered") != ""
	if offered {
		was := r.FormValue("protected_was")
		if (was != "0" && was != "1") || (was == "1") != existing.Protected {
			s.refuseStaleDomainSave(w, r, changedElsewhereNotice)
			return
		}
	}
	d, verr := s.parseDomainForm(r, app)
	if verr == ownHostRefusal && s.isOwnHost(existing.Host) {
		verr = ownHostRowRefusal
	}
	if verr != "" {
		http.Error(w, verr, http.StatusBadRequest)
		return
	}
	d.ID = domainID
	d.AppID = id
	// When HTTPS automation is unavailable the wizard disables the toggle, which
	// would submit no value and silently clear a previously-enabled certificate.
	// Preserve the stored intent so re-enabling ACME later restores HTTPS.
	if !s.tlsEnabled {
		d.TLS = existing.TLS
	}
	// See handleAddDomain.
	ctx := context.WithoutCancel(r.Context())
	// UpdateDomain writes Protected as given, so it is always set here: the
	// switch's value when the form offered it, otherwise the row's own. Only
	// turning it on needs sign-in to be available; turning it off is always
	// allowed, so a row that nobody can get through can be opened. The dialog
	// disables the switch only on an open row, where it then submits nothing.
	switch {
	case r.FormValue("protected") != "":
		if !existing.Protected {
			if ok, reason := s.protectionAvailability(ctx, d); !ok {
				http.Error(w, reason, http.StatusBadRequest)
				return
			}
		}
		d.Protected = true
	case r.FormValue("protected_offered") != "":
		d.Protected = false
	default:
		d.Protected = existing.Protected
	}
	// A deploy applies a change to what sign-in covers: turning it on or off,
	// moving a protected row, or moving an open row off a route on a host that
	// requires sign-in, which its old container would keep serving openly.
	moved := !strings.EqualFold(existing.Host, d.Host) || existing.Path != d.Path
	redeploy := d.Protected != existing.Protected || existing.Protected && moved
	if !redeploy && moved {
		leaves, err := s.leavesOpenRoute(ctx, existing)
		if err != nil {
			http.Error(w, "Could not update domain: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if leaves && offered && r.FormValue("exception_was") != "1" {
			// The dialog didn't know, so it didn't warn that this redeploys.
			s.refuseStaleDomainSave(w, r, existing.Host+" now requires sign-in, so changing this domain's host or path redeploys this app if it is running. Check it and try again.")
			return
		}
		redeploy = leaves
	}
	// Requiring sign-in on a host (turning it on, or moving a protected row)
	// also redeploys other apps still serving an old open route there. Any
	// save leaving the row protected gives the app's previews its protection.
	var others []otherRedeploy
	if d.Protected {
		var ok bool
		if others, ok = s.othersToRedeploy(ctx, w, r, app, d, !existing.Protected || moved); !ok {
			return
		}
	}
	if err := s.store.UpdateDomain(ctx, d); err != nil {
		if errors.Is(err, store.ErrHostReserved) {
			msg := ownHostRefusal
			if strings.EqualFold(strings.TrimSuffix(existing.Host, "."), strings.TrimSuffix(d.Host, ".")) {
				msg = ownHostRowRefusal
			}
			http.Error(w, msg, http.StatusBadRequest)
			return
		}
		http.Error(w, "Could not update domain: "+err.Error(), http.StatusBadRequest)
		return
	}
	if d.Protected && !existing.Protected {
		if err := s.ensureGateHost(ctx); err != nil {
			undo := s.store.UpdateDomain(ctx, existing)
			s.refuseProtection(w, err, undo)
			return
		}
	}
	s.finishDomainSave(ctx, w, r, app, redeploy, others, d.Protected)
}

// othersToRedeploy works out, before a save that leaves d protected, which
// other apps it redeploys. When the save requires sign-in on d's host
// (onHost), those are the apps still serving an old open route there
// (leftoverRoutes), which it returns for finishDomainSave to redeploy.
// Either way they include the previews of app that d's save gives its
// protection to (previewsToProtect), which finishDomainSave protects and
// redeploys. Only the apps with an old route on d's host are read, and those
// in projects the person can't see aren't named (otherRedeploy.Hidden). It
// answers, and reports false, when that can't be worked out (the save is
// refused) or when the form's others_was doesn't name them all: its page
// didn't warn of them, so the page is rendered again with the warning, and
// with d's host among those its dialog names other apps on.
func (s *Server) othersToRedeploy(ctx context.Context, w http.ResponseWriter, r *http.Request, app core.App, d core.Domain, onHost bool) ([]otherRedeploy, bool) {
	var others []otherRedeploy
	if onHost {
		scope, err := s.scopeFor(r)
		if err != nil {
			log.Printf("protected routes: read the person's projects: %v", err)
			scope = projectScope{} // names no other app
		}
		others, err = s.leftoverRoutes(ctx, app, map[string]bool{strings.ToLower(d.Host): true}, scope)
		if err != nil {
			http.Error(w, fmt.Sprintf("Sign-in wasn't turned on: Outhaul couldn't tell which other apps still serve old routes on %s, which it would redeploy (%v). The domain wasn't changed.", d.Host, err), http.StatusServiceUnavailable)
			return nil, false
		}
	}
	previews, err := s.previewsAfterSave(ctx, app, d)
	if err != nil {
		http.Error(w, fmt.Sprintf("The domain wasn't changed: Outhaul couldn't tell which of this app's previews it would also require sign-in on (%v).", err), http.StatusServiceUnavailable)
		return nil, false
	}
	shown := map[string]bool{}
	for _, k := range strings.Split(r.FormValue("others_was"), ",") {
		shown[k] = true
	}
	var unshown []otherRedeploy
	for _, list := range [][]otherRedeploy{others, previews} {
		for _, o := range list {
			if !shown[o.key()] {
				unshown = append(unshown, o)
			}
		}
	}
	if len(unshown) > 0 {
		var say []string
		for _, l := range alsoLines(unshown) {
			say = append(say, l.Sentence)
		}
		s.renderAppPage(w, r, http.StatusConflict, strings.Join(say, " ")+" Check it and try again.", d.Host)
		return nil, false
	}
	return others, true
}

// previewsAfterSave is previewsToProtect for app's rows as they will be once
// d is saved: added, or in place of the row it edits.
func (s *Server) previewsAfterSave(ctx context.Context, app core.App, d core.Domain) ([]otherRedeploy, error) {
	rows, err := s.store.ListDomains(ctx, app.ID)
	if err != nil {
		return nil, err
	}
	after := make([]core.Domain, 0, len(rows)+1)
	for _, row := range rows {
		if row.ID != d.ID { // an added d has no ID yet
			after = append(after, row)
		}
	}
	return s.previewsToProtect(ctx, app, append(after, d))
}

// ensureGateHost has the proxy reconciled now, with the rows as saved, so
// Traefik maps the gate's host before a gated container is deployed. The
// rows alone don't promise it: a tunnel enable can fail half-way, Docker can
// be down at boot, a public URL can lack a host.
func (s *Server) ensureGateHost(ctx context.Context) error {
	if s.proxy == nil {
		return errors.New("update its proxy: this server has none it can update")
	}
	return s.proxy.ReconcileProxy(ctx)
}

// refuseProtection answers a save that would have turned sign-in on when the
// proxy couldn't be reconciled. err names what failed (ProxyControl); undo is
// the result of putting the row back.
func (s *Server) refuseProtection(w http.ResponseWriter, err, undo error) {
	if undo != nil {
		log.Printf("protected routes: could not put a row back after the proxy failed: %v", undo)
		http.Error(w, fmt.Sprintf("Sign-in wasn't turned on: Outhaul couldn't %v, and couldn't undo the change to the domain (%v). Check the domain before you deploy.", err, undo), http.StatusInternalServerError)
		return
	}
	http.Error(w, fmt.Sprintf("Sign-in wasn't turned on: Outhaul couldn't %v. The domain wasn't changed.", err), http.StatusServiceUnavailable)
}

// finishDomainSave ends a saved add, edit or removal. When the save changed
// what sign-in covers it queues the deploy that applies it (applyProtection),
// and one for each of others, by the same rules. When it left a row protected
// (previews), the app's previews get its rows' protection, and redeploy
// (protectPreviews). Either way it syncs the local CA, which mints a
// protected row's certificate whatever its TLS flag.
func (s *Server) finishDomainSave(ctx context.Context, w http.ResponseWriter, r *http.Request, app core.App, redeploy bool, others []otherRedeploy, previews bool) {
	var failed []string
	if redeploy {
		if _, err := s.applyProtection(ctx, app); err != nil {
			log.Printf("protected routes: queue the deploy of %q: %v", app.Name, err)
			failed = append(failed, app.Name+" ("+err.Error()+")")
		}
	}
	queued := map[int64]bool{}
	for _, o := range others {
		if queued[o.App.ID] {
			continue // an app with old routes on the host under two names
		}
		queued[o.App.ID] = true
		if _, err := s.queueProtectionDeploy(ctx, o.App); err != nil {
			log.Printf("protected routes: queue the deploy of %q: %v", o.App.Name, err)
			failed = append(failed, o.label()+" ("+err.Error()+")")
		}
	}
	var previewErr error
	if previews {
		if previewErr = s.protectPreviews(ctx, app); previewErr != nil {
			log.Printf("protected routes: require sign-in on the previews of %q: %v", app.Name, previewErr)
		}
	}
	s.syncCerts(ctx)
	if len(failed) > 0 || previewErr != nil {
		var parts []string
		if len(failed) > 0 {
			parts = append(parts, "the deploy that applies it to sign-in couldn't be queued for "+strings.Join(failed, ", ")+". Deploy them to apply it.")
		}
		if previewErr != nil {
			parts = append(parts, "it couldn't be applied to every preview of "+app.Name+": "+strings.ReplaceAll(previewErr.Error(), "\n", ", ")+". Check them on the Domains page.")
		}
		http.Error(w, "The domain change was saved, but "+strings.Join(parts, " Also, "), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/apps/"+strconv.FormatInt(app.ID, 10), http.StatusSeeOther)
}

// handleDeleteDomain removes a route. Removing an open row on a host that
// requires sign-in redeploys a running app, whose container would otherwise
// keep serving that route with no gate; a protected row's old route keeps its
// gate, so removing one needs no deploy. Removing a row the app doesn't have
// does nothing.
func (s *Server) handleDeleteDomain(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(r.PathValue("id"))
	domainID, ok2 := parseID(r.PathValue("domainID"))
	if !ok || !ok2 {
		http.NotFound(w, r)
		return
	}
	app, err := s.store.GetApp(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	// See handleAddDomain.
	ctx := context.WithoutCancel(r.Context())
	var redeploy bool
	existing, err := s.store.GetDomain(ctx, id, domainID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	default:
		if redeploy, err = s.leavesOpenRoute(ctx, existing); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	// The remove button says whether its page showed that removing the row
	// redeploys (warned); a page that didn't gets the warning now instead.
	if redeploy && r.FormValue("warned") != "1" {
		notice := "Removing " + existing.Host + existing.Path + " redeploys this app if it is running, because " + existing.Host + " requires sign-in."
		if warning := s.protectionWarning(ctx, app); warning != "" {
			notice += " " + warning
		}
		s.refuseStaleDomainSave(w, r, notice+" Remove it again to go ahead.")
		return
	}
	if err := s.store.DeleteDomain(ctx, id, domainID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.finishDomainSave(ctx, w, r, app, redeploy, nil, false)
}

// handleAddVolume attaches a persistent volume to a single-container app. The
// volume is created and mounted on the app's next deploy.
func (s *Server) handleAddVolume(w http.ResponseWriter, r *http.Request) {
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
	mountPath, verr := parseVolumeForm(r, app)
	if verr != "" {
		http.Error(w, verr, http.StatusBadRequest)
		return
	}
	if _, err := s.store.AddVolume(r.Context(), id, mountPath); err != nil {
		http.Error(w, "Could not add volume (is that path already used?): "+err.Error(), http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, "/apps/"+strconv.FormatInt(id, 10), http.StatusSeeOther)
}

// handleUpdateVolume changes a volume's mount path (its Docker name is fixed).
func (s *Server) handleUpdateVolume(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(r.PathValue("id"))
	volumeID, ok2 := parseID(r.PathValue("volumeID"))
	if !ok || !ok2 {
		http.NotFound(w, r)
		return
	}
	app, err := s.store.GetApp(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if _, err := s.store.GetVolume(r.Context(), id, volumeID); err != nil {
		http.NotFound(w, r)
		return
	}
	mountPath, verr := parseVolumeForm(r, app)
	if verr != "" {
		http.Error(w, verr, http.StatusBadRequest)
		return
	}
	if err := s.store.UpdateVolume(r.Context(), id, volumeID, mountPath); err != nil {
		http.Error(w, "Could not update volume: "+err.Error(), http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, "/apps/"+strconv.FormatInt(id, 10), http.StatusSeeOther)
}

// handleDeleteVolume detaches a volume (removes the row); the Docker volume is
// left in place and becomes a reclaimable orphan in the inventory.
func (s *Server) handleDeleteVolume(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(r.PathValue("id"))
	volumeID, ok2 := parseID(r.PathValue("volumeID"))
	if !ok || !ok2 {
		http.NotFound(w, r)
		return
	}
	if _, err := s.store.GetApp(r.Context(), id); err != nil {
		http.NotFound(w, r)
		return
	}
	if err := s.store.DeleteVolume(r.Context(), id, volumeID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/apps/"+strconv.FormatInt(id, 10), http.StatusSeeOther)
}

// parseVolumeForm validates the mount path. Volumes are only for
// single-container apps (compose stacks declare their own in the compose file).
func parseVolumeForm(r *http.Request, app core.App) (string, string) {
	if app.Kind == core.KindCompose {
		return "", "Compose stacks declare volumes in their compose file."
	}
	mountPath := strings.TrimSpace(r.FormValue("mount_path"))
	if !mountPathRe.MatchString(mountPath) || strings.Contains(mountPath, "..") {
		return "", "Mount path must be an absolute path like /data (letters, digits, . _ - / only, no '..')."
	}
	if mountPath != "/" {
		mountPath = strings.TrimRight(mountPath, "/")
	}
	return mountPath, ""
}

// parseDomainForm reads and validates the wizard's fields for one route. For a
// compose app it requires a service + port; for a single-container app it forces
// service="" and port=AppPort. host_kind=="sslip" mints a host under the
// server's sslip.io wildcard instead of the custom field.
func (s *Server) parseDomainForm(r *http.Request, app core.App) (core.Domain, string) {
	host := strings.TrimSpace(r.FormValue("host"))
	if r.FormValue("host_kind") == "sslip" {
		if s.serverIP == "" {
			return core.Domain{}, "This server has no public IP configured, so a sslip.io domain can't be generated. Enter a custom domain."
		}
		host = app.Name + "." + s.serverIP + ".sslip.io"
	}
	host = strings.ToLower(host)
	if !domainHostRe.MatchString(host) {
		return core.Domain{}, "Domain must be a bare hostname (e.g. app.example.com)."
	}
	if s.isOwnHost(host) {
		return core.Domain{}, ownHostRefusal
	}

	d := core.Domain{Host: host, TLS: r.FormValue("tls") != ""}

	urlPath, verr := cleanURLPath(r.FormValue("path"))
	if verr != "" {
		return core.Domain{}, verr
	}
	internalPath, verr := cleanURLPath(r.FormValue("internal_path"))
	if verr != "" {
		return core.Domain{}, verr
	}
	// An internal path only rewrites a matched external prefix, so it is
	// meaningless without one.
	if internalPath != "" && urlPath == "" {
		return core.Domain{}, "An internal path only applies when an external path is set."
	}
	d.Path, d.InternalPath = urlPath, internalPath

	if app.Kind == core.KindCompose {
		if !composeServiceRe.MatchString(strings.TrimSpace(r.FormValue("service"))) {
			return core.Domain{}, "Name the compose service to expose on the domain (e.g. web)."
		}
		port, err := strconv.Atoi(strings.TrimSpace(r.FormValue("port")))
		if err != nil || port < 1 || port > 65535 {
			return core.Domain{}, "Container port must be a number between 1 and 65535."
		}
		d.Service, d.Port = strings.TrimSpace(r.FormValue("service")), port
	} else {
		d.Service, d.Port = "", deployAppPort
	}
	return d, ""
}

// ownHostRefusal is why a domain row can't use Outhaul's own host. The store
// refuses it too (store.ErrHostReserved), whoever writes the row; the forms
// check first so they can say why.
const ownHostRefusal = "That is Outhaul's own address, so an app can't use it."

// ownHostRowRefusal answers an edit to a row put on Outhaul's own host before
// that was refused. It can still be deleted.
const ownHostRowRefusal = "This domain is on Outhaul's own address, which an app can't use, so it can't be edited. Delete it instead."

// isOwnHost reports whether host is the host of Outhaul's public URL, ignoring
// case and any port. An app routed there, even on a path of its own, would
// share Outhaul's origin and so its cookies.
func (s *Server) isOwnHost(host string) bool {
	if !s.publicURLSet() {
		return false
	}
	u, err := url.Parse(s.publicURL)
	if err != nil {
		return false
	}
	own := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	return own != "" && own == strings.TrimSuffix(strings.ToLower(host), ".")
}

// cleanURLPath validates an optional external/internal path: empty is allowed
// (meaning "no path" / "unchanged"); otherwise it must be a clean absolute path.
func cleanURLPath(p string) (string, string) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", ""
	}
	if !urlPathRe.MatchString(p) || strings.Contains(p, "..") {
		return "", "Paths must be an absolute path like /api (letters, digits, . _ ~ % - / only, no '..')."
	}
	if p != "/" {
		p = strings.TrimRight(p, "/")
	}
	return p, ""
}

// envRow is an env var prepared for display: secret values are masked.
type envRow struct {
	Key      string
	Value    string
	IsSecret bool
	Scope    string
	Reach    string // shared rows only: where the value still applies, when a scoped row overrides it
}

// viewerEnv hides every value, so a viewer sees which variables exist and
// nothing they hold. Plain values can carry credentials too, and a viewer may
// read but not use them.
func viewerEnv(rows []envRow) []envRow {
	out := make([]envRow, len(rows))
	for i, r := range rows {
		r.Value, r.IsSecret = "", true
		out[i] = r
	}
	return out
}

func maskEnv(vars []core.EnvVar) []envRow {
	reach := sharedEnvReach(vars)
	rows := make([]envRow, 0, len(vars))
	for _, v := range vars {
		row := envRow{Key: v.Key, IsSecret: v.IsSecret, Scope: v.Scope}
		if !v.IsSecret {
			row.Value = v.Value
		}
		if v.Scope == core.ScopeShared || v.Scope == "" {
			row.Reach = reach[v.Key]
		}
		rows = append(rows, row)
	}
	return rows
}

// sharedEnvReach describes, per key, where a shared value still applies once
// the same key has a production- and/or preview-only value (core.EnvForScope
// lets the scoped value win). Keys with no scoped sibling are absent. It is
// shown next to the shared row so that re-adding a secret as "Production
// only" visibly leaves the shared copy flowing to previews.
func sharedEnvReach(vars []core.EnvVar) map[string]string {
	prod, preview := map[string]bool{}, map[string]bool{}
	for _, v := range vars {
		switch v.Scope {
		case core.ScopeProd:
			prod[v.Key] = true
		case core.ScopePreview:
			preview[v.Key] = true
		}
	}
	out := map[string]string{}
	for _, v := range vars {
		if v.Scope != core.ScopeShared && v.Scope != "" {
			continue
		}
		switch {
		case prod[v.Key] && preview[v.Key]:
			out[v.Key] = "unused — production and previews both have their own value"
		case prod[v.Key]:
			out[v.Key] = "previews only — production has its own value"
		case preview[v.Key]:
			out[v.Key] = "production only — previews have their own value"
		}
	}
	return out
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
	scope := r.FormValue("scope")
	switch scope {
	case "":
		scope = core.ScopeShared
	case core.ScopeShared, core.ScopeProd, core.ScopePreview:
		// valid
	default:
		http.Error(w, "Invalid scope.", http.StatusBadRequest)
		return
	}
	if err := s.store.SetEnvScoped(r.Context(), id, key, value, isSecret, scope); err != nil {
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
	if err := s.store.DeleteEnv(r.Context(), id, key, r.FormValue("scope")); err != nil {
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

// handleRestartApp restarts the app's container or stack, unless their labels
// disagree with its rows' protection: restarting would bring back the old
// labels, so a protected route could come back public. Then it deploys
// instead, by the rules in queueProtectionDeploy, and shows that deployment.
func (s *Server) handleRestartApp(w http.ResponseWriter, r *http.Request) {
	if s.restartDeploysInstead(w, r) {
		return
	}
	s.appLifecycle(w, r,
		func(ctx context.Context, cid string) error {
			_ = s.runtime.StopContainer(ctx, cid, 10*time.Second) // ignore: already-stopped is fine
			return s.runtime.StartContainer(ctx, cid)
		},
		func(ctx context.Context, project string) error {
			return s.compose.Restart(ctx, project, io.Discard)
		})
}

// restartDeploysInstead queues a deploy, and answers, when the app's containers
// disagree with its rows' protection (protectionMismatch). It reports whether
// it answered. An app with no containers has nothing to restart, and is left
// to appLifecycle. If the labels, or the hosts that have protected rows,
// can't be read, nothing is restarted.
func (s *Server) restartDeploysInstead(w http.ResponseWriter, r *http.Request) bool {
	id, ok := parseID(r.PathValue("id"))
	if !ok {
		return false
	}
	app, err := s.store.GetApp(r.Context(), id)
	if err != nil {
		return false
	}
	domains, err := s.store.ListDomains(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return true
	}
	cs, err := s.appContainers(r.Context(), app)
	if err != nil {
		http.Error(w, "Could not read the app's containers, so it wasn't restarted: "+err.Error(), http.StatusInternalServerError)
		return true
	}
	if len(cs) == 0 {
		return false
	}
	protectedHosts, err := s.store.ProtectedHosts(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return true
	}
	if !protectionMismatch(domains, app.Name, cs, protectedHosts) {
		return false
	}
	dep, err := s.queueProtectionDeploy(r.Context(), app)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return true
	}
	http.Redirect(w, r, deploymentPath(dep.ID), http.StatusSeeOther)
	return true
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
	// Deletion is several destructive steps; once started it must finish even
	// if the browser gives up, or it stops half-done (a preview's database
	// destroyed only after its app row, say, is left behind).
	ctx := context.WithoutCancel(r.Context())
	if err := s.destroyPreviews(ctx, app); err != nil {
		http.Error(w, "Could not delete app: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// Best-effort teardown; proceed with the row delete regardless, but log
	// failures so orphaned containers aren't silently lost. Compose stacks go
	// down whole (containers + networks; named volumes deliberately survive —
	// they are data).
	if app.Kind == core.KindCompose {
		if derr := s.compose.Down(ctx, compose.ProjectName(app.Name), io.Discard); derr != nil {
			log.Printf("delete app %d: could not tear down compose stack (its containers may be orphaned): %v", id, derr)
		}
	} else if c, ferr := s.runtime.FindContainer(ctx, appContainerPrefix+app.Name); ferr != nil {
		log.Printf("delete app %d: could not inspect container (any container is now orphaned): %v", id, ferr)
	} else if c != nil {
		_ = s.runtime.StopContainer(ctx, c.ID, 10*time.Second)
		if rerr := s.runtime.RemoveContainer(ctx, c.ID, true); rerr != nil {
			log.Printf("delete app %d: could not remove container %s: %v", id, c.ID, rerr)
		}
	}
	// The app's built images go with it (the deployment rows about to be
	// deleted are the only record of the tags). Best-effort like the container
	// teardown: a leftover is reclaimed by the pruner's daily reconciliation.
	if app.Kind != core.KindCompose {
		if deps, derr := s.store.ListDeploymentsForApp(ctx, id); derr != nil {
			log.Printf("delete app %d: could not list deployments (its images are now orphaned until the next sweep): %v", id, derr)
		} else {
			for _, tag := range distinctImages(deps) {
				if rerr := s.runtime.RemoveImage(ctx, tag); rerr != nil {
					log.Printf("delete app %d: could not remove image %s: %v", id, tag, rerr)
				}
			}
		}
	}
	if err := s.store.DeleteApp(ctx, id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Whatever the source is now, a push may once have made a repository.
	if s.repos != nil {
		if rerr := s.repos.Remove(app.Name); rerr != nil {
			log.Printf("delete app %d: remove bare repo: %v", id, rerr)
		}
	}
	http.Redirect(w, r, "/apps", http.StatusSeeOther)
}

// destroyPreviews tears down every preview of parent before the parent itself
// goes. A preview carries copies of the parent's secrets and keeps serving until
// torn down, and once the parent row is gone nothing would ever reap it — so any
// failure (including having no preview manager to do it) keeps the parent, and a
// retried delete picks up where this one stopped.
func (s *Server) destroyPreviews(ctx context.Context, parent core.App) error {
	kids, err := s.store.ListPreviewsForParent(ctx, parent.ID)
	if err != nil {
		return err
	}
	if len(kids) > 0 && s.previews == nil {
		return fmt.Errorf("it has %d preview environment(s) and previews are not enabled on this server to tear them down", len(kids))
	}
	for _, k := range kids {
		if err := s.previews.DestroyByID(ctx, parent.ID, k.ID); err != nil {
			return fmt.Errorf("could not tear down the preview for PR #%d: %w", k.PRNumber, err)
		}
	}
	return nil
}

// handleRegenerateWebhookSecret gives an app a fresh generic-webhook secret,
// keeping its URL token, so the sender needs only the new secret. An app with
// no token yet (created by a cold git push) gets both, enabling its webhook.
func (s *Server) handleRegenerateWebhookSecret(w http.ResponseWriter, r *http.Request) {
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
	if app.WebhookToken == "" {
		err = s.store.SetWebhookCredentials(r.Context(), id, newSecret(), newSecret())
	} else {
		err = s.store.SetWebhookSecret(r.Context(), id, newSecret())
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/apps/"+strconv.FormatInt(id, 10), http.StatusSeeOther)
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
		http.Error(w, "this deployment's image was pruned by image retention; deploy to rebuild instead", http.StatusBadRequest)
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
	s.render(w, r, http.StatusOK, "deployment", map[string]any{
		"Title":      "Deployment #" + r.PathValue("id"),
		"Active":     "apps",
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
	if app.Domain != "" && !domainHostRe.MatchString(app.Domain) {
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
	case core.SourcePush:
		if app.RepoURL != "" {
			return "A push-to-deploy app has no repository URL."
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
func parseDomainFields(domain, service, portStr string) (core.Domain, string) {
	domain = strings.ToLower(strings.TrimSpace(domain))
	if !domainHostRe.MatchString(domain) {
		return core.Domain{}, "Domain must be a bare hostname (e.g. app.example.com)."
	}
	if !composeServiceRe.MatchString(strings.TrimSpace(service)) {
		return core.Domain{}, "Name the compose service to expose on the domain (e.g. web)."
	}
	port, err := strconv.Atoi(strings.TrimSpace(portStr))
	if err != nil || port < 1 || port > 65535 {
		return core.Domain{}, "Service port must be a number between 1 and 65535."
	}
	return core.Domain{Host: domain, Service: strings.TrimSpace(service), Port: port, TLS: true}, ""
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

// accessStates is the Access cell of each of an app's rows, by row ID, from
// its containers (listErr is why they couldn't be listed, or nil) and its
// deployments, newest first. With Docker unreachable every row is Unknown.
func accessStates(domains []core.Domain, appName string, containers []docker.Container, listErr error, newestFirst []core.Deployment) map[int64]accessState {
	var latest *core.Deployment
	if len(newestFirst) > 0 {
		latest = &newestFirst[0]
	}
	out := make(map[int64]accessState, len(domains))
	for _, d := range domains {
		if listErr != nil {
			out[d.ID] = unknownAccess()
			continue
		}
		out[d.ID] = accessStateFor(d, appName, containers, latest)
	}
	return out
}
