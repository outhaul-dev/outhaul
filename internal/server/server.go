// Package server is Outhaul's admin web UI and HTTP API: server-rendered
// html/template pages (with htmx and a little vanilla JS, no build step),
// argon2id auth, and an SSE endpoint that streams build logs from the logstream
// broker. It depends on store, logstream, and a Deployer (the deploy worker).
package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"html/template"
	"io"
	"io/fs"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/james-smart/outhaul/internal/blobstore"
	"github.com/james-smart/outhaul/internal/compose"
	"github.com/james-smart/outhaul/internal/core"
	"github.com/james-smart/outhaul/internal/docker"
	"github.com/james-smart/outhaul/internal/github"
	"github.com/james-smart/outhaul/internal/logstream"
	"github.com/james-smart/outhaul/internal/store"
)

// Deployer is the slice of the deploy worker the server needs.
type Deployer interface {
	Notify()
	Cancel(ctx context.Context, id int64) (bool, error)
}

// Databases is the slice of the dbaas manager the server needs. Provision is
// asynchronous (it can pull an image); the rest are immediate.
type Databases interface {
	Provision(d core.Database)
	Start(ctx context.Context, d core.Database) error
	Stop(ctx context.Context, d core.Database) error
	Remove(ctx context.Context, d core.Database) error
}

// Backups is the slice of the backup manager the server needs. RunNow and
// RestoreNow are asynchronous; the rest are synchronous.
type Backups interface {
	RunNow(b core.Backup)
	RestoreNow(b core.Backup, objectKey string)
	ListRestoreObjects(ctx context.Context, b core.Backup) ([]blobstore.Object, error)
	RestoreDir(ctx context.Context, b core.Backup) (string, error)
	TestDestination(ctx context.Context, d core.Destination) error
}

// Runtime is the slice of the Docker client the server needs for app lifecycle.
type Runtime interface {
	FindContainer(ctx context.Context, name string) (*docker.Container, error)
	ListContainers(ctx context.Context, match map[string]string) ([]docker.Container, error)
	StartContainer(ctx context.Context, id string) error
	StopContainer(ctx context.Context, id string, timeout time.Duration) error
	RemoveContainer(ctx context.Context, id string, force bool) error
	ContainerLogs(ctx context.Context, id string, tail int) (io.ReadCloser, error)
	ContainerStats(ctx context.Context, id string) (docker.Stats, error)
	RemoveImage(ctx context.Context, ref string) error
}

// Server holds the HTTP layer's dependencies.
type Server struct {
	store     *store.Store
	deployer  Deployer
	runtime   Runtime
	compose   compose.Runner
	databases Databases
	backups   Backups
	broker    *logstream.Broker
	gh        github.Client

	pages      map[string]*template.Template
	setupToken string
	publicURL  string
	secure     bool // Secure cookie flag; the admin UI is served directly over HTTP (not behind Traefik), so this stays false

	stateMu     sync.Mutex
	stateTokens map[string]time.Time // CSRF states for the GitHub App manifest flow
}

// New constructs a Server, parsing the embedded templates. setupToken guards the
// first-boot admin-creation flow (printed by the caller as a one-time URL).
// publicURL is Outhaul's externally reachable base URL, used to build the
// GitHub App manifest's callback and webhook URLs.
func New(st *store.Store, d Deployer, rt Runtime, cp compose.Runner, dbm Databases, bk Backups, br *logstream.Broker, gh github.Client, publicURL, setupToken string) (*Server, error) {
	s := &Server{
		store:       st,
		deployer:    d,
		runtime:     rt,
		compose:     cp,
		databases:   dbm,
		backups:     bk,
		broker:      br,
		gh:          gh,
		publicURL:   publicURL,
		setupToken:  setupToken,
		stateTokens: map[string]time.Time{},
	}
	if err := s.parseTemplates(); err != nil {
		return nil, err
	}
	return s, nil
}

// parseTemplates builds one template set per page, each combining base.tmpl with
// the page template (so every page can define its own "content" block).
func (s *Server) parseTemplates() error {
	pages := []string{"login", "setup", "overview", "projects", "project", "apps", "app", "database", "deployment", "deployments", "github_connect", "settings", "restore", "placeholder"}
	s.pages = make(map[string]*template.Template, len(pages))
	for _, p := range pages {
		t := template.New("base").Funcs(templateFuncs())
		// appform.tmpl is a shared partial (the create-app form, used by the
		// Apps and project-detail pages); parsing it into every set is harmless.
		t, err := t.ParseFS(templatesFS, "templates/base.tmpl", "templates/appform.tmpl", "templates/backups.tmpl", "templates/"+p+".tmpl")
		if err != nil {
			return err
		}
		s.pages[p] = t
	}
	return nil
}

// Handler returns the fully-routed HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	sub, _ := fs.Sub(staticFS, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(sub))))

	mux.HandleFunc("GET /setup", s.handleSetupForm)
	mux.HandleFunc("POST /setup", s.handleSetupSubmit)
	mux.HandleFunc("GET /login", s.handleLoginForm)
	mux.HandleFunc("POST /login", s.handleLoginSubmit)
	mux.HandleFunc("POST /logout", s.handleLogout)

	mux.HandleFunc("GET /{$}", s.requireAuth(s.handleOverview))
	mux.HandleFunc("GET /projects", s.requireAuth(s.handleProjectsList))
	mux.HandleFunc("POST /projects", s.requireAuth(s.handleCreateProject))
	mux.HandleFunc("GET /projects/{id}", s.requireAuth(s.handleProjectDetail))
	mux.HandleFunc("POST /projects/{id}/settings", s.requireAuth(s.handleProjectSettings))
	mux.HandleFunc("POST /projects/{id}/env", s.requireAuth(s.handleSetProjectEnv))
	mux.HandleFunc("POST /projects/{id}/env/delete", s.requireAuth(s.handleDeleteProjectEnv))
	mux.HandleFunc("POST /projects/{id}/delete", s.requireAuth(s.handleDeleteProject))
	mux.HandleFunc("POST /projects/{id}/databases", s.requireAuth(s.handleCreateDatabase))
	mux.HandleFunc("GET /databases/{id}", s.requireAuth(s.handleDatabaseDetail))
	mux.HandleFunc("GET /databases/{id}/logs", s.requireAuth(s.handleDatabaseLogsSSE))
	mux.HandleFunc("POST /databases/{id}/start", s.requireAuth(s.handleStartDatabase))
	mux.HandleFunc("POST /databases/{id}/stop", s.requireAuth(s.handleStopDatabase))
	mux.HandleFunc("POST /databases/{id}/settings", s.requireAuth(s.handleDatabaseSettings))
	mux.HandleFunc("POST /databases/{id}/delete", s.requireAuth(s.handleDeleteDatabase))
	mux.HandleFunc("GET /apps", s.requireAuth(s.handleAppsList))
	mux.HandleFunc("POST /apps", s.requireAuth(s.handleCreateApp))
	mux.HandleFunc("GET /apps/{id}", s.requireAuth(s.handleAppDetail))
	mux.HandleFunc("GET /apps/{id}/logs", s.requireAuth(s.handleRuntimeLogsSSE))
	mux.HandleFunc("GET /apps/{id}/stats", s.requireAuth(s.handleAppStats))
	mux.HandleFunc("POST /apps/{id}/deploy", s.requireAuth(s.handleDeploy))
	mux.HandleFunc("POST /apps/{id}/settings", s.requireAuth(s.handleAppSettings))
	mux.HandleFunc("POST /apps/{id}/domains", s.requireAuth(s.handleAddComposeDomain))
	mux.HandleFunc("POST /apps/{id}/domains/{domainID}/delete", s.requireAuth(s.handleDeleteComposeDomain))
	mux.HandleFunc("POST /apps/{id}/env", s.requireAuth(s.handleSetEnv))
	mux.HandleFunc("POST /apps/{id}/env/delete", s.requireAuth(s.handleDeleteEnv))
	mux.HandleFunc("POST /apps/{id}/stop", s.requireAuth(s.handleStopApp))
	mux.HandleFunc("POST /apps/{id}/restart", s.requireAuth(s.handleRestartApp))
	mux.HandleFunc("POST /apps/{id}/delete", s.requireAuth(s.handleDeleteApp))
	mux.HandleFunc("GET /deployments", s.requireAuth(s.handleDeployments))
	mux.HandleFunc("GET /deployments/{id}", s.requireAuth(s.handleDeploymentDetail))
	mux.HandleFunc("GET /deployments/{id}/logs", s.requireAuth(s.handleLogsSSE))
	mux.HandleFunc("POST /deployments/{id}/cancel", s.requireAuth(s.handleCancel))
	mux.HandleFunc("POST /deployments/{id}/rollback", s.requireAuth(s.handleRollback))

	mux.HandleFunc("POST /backups", s.requireAuth(s.handleCreateBackup))
	mux.HandleFunc("POST /backups/{id}/run", s.requireAuth(s.handleRunBackup))
	mux.HandleFunc("POST /backups/{id}/toggle", s.requireAuth(s.handleToggleBackup))
	mux.HandleFunc("POST /backups/{id}/delete", s.requireAuth(s.handleDeleteBackup))
	mux.HandleFunc("GET /backups/{id}/restore", s.requireAuth(s.handleRestorePage))
	mux.HandleFunc("POST /backups/{id}/restore", s.requireAuth(s.handleRestoreBackup))

	mux.HandleFunc("GET /settings", s.requireAuth(s.handleSettings))
	mux.HandleFunc("POST /settings/password", s.requireAuth(s.handleChangePassword))
	mux.HandleFunc("POST /settings/destinations", s.requireAuth(s.handleCreateDestination))
	mux.HandleFunc("POST /settings/destinations/{id}/test", s.requireAuth(s.handleTestDestination))
	mux.HandleFunc("POST /settings/destinations/{id}/delete", s.requireAuth(s.handleDeleteDestination))

	mux.HandleFunc("GET /github/connect", s.requireAuth(s.handleGithubConnect))
	// callback and setup are called by GitHub directly (no session cookie); the
	// callback is guarded by the one-time CSRF state instead.
	mux.HandleFunc("GET /github/callback", s.handleGithubCallback)
	mux.HandleFunc("GET /github/setup", s.handleGithubSetup)

	// Webhook endpoints are called by GitHub/Gitea/GitLab directly (no session
	// cookie); each verifies its own HMAC signature or token instead.
	mux.HandleFunc("POST /webhooks/github", s.handleGithubWebhook)
	mux.HandleFunc("POST /webhooks/app/{token}", s.handleAppWebhook)

	for _, p := range placeholderPages {
		mux.HandleFunc("GET /"+p.active, s.requireAuth(s.handlePlaceholder(p)))
	}

	return mux
}

// render executes the named page template with the base layout.
func (s *Server) render(w http.ResponseWriter, status int, page string, data map[string]any) {
	t, ok := s.pages[page]
	if !ok {
		http.Error(w, "unknown page: "+page, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := t.ExecuteTemplate(w, "base", data); err != nil {
		// Response may be partially written; log via the standard logger.
		http.Error(w, "render error: "+err.Error(), http.StatusInternalServerError)
	}
}

func templateFuncs() template.FuncMap {
	return template.FuncMap{
		// stateClass maps a deployment status to its CSS class.
		"stateClass": func(s core.DeployStatus) string { return "state-" + string(s) },
		// dbStateClass maps a database status onto the deployment state colors.
		"dbStateClass": func(s string) string {
			if s == core.DBCreating {
				return "state-building" // in-progress color
			}
			return "state-" + s
		},
		// runStateClass maps a backup-run status onto the same colors.
		"runStateClass": func(s string) string {
			switch s {
			case core.RunOK:
				return "state-running"
			case core.RunRunning:
				return "state-building"
			default:
				return "state-failed"
			}
		},
		// fmtSize renders a byte count human-readably.
		"fmtSize": func(n int64) string {
			if n <= 0 {
				return "—"
			}
			return fmtBytes(uint64(n))
		},
		// joinLines renders a list one-per-line (the watch-paths textarea).
		"joinLines": func(ss []string) string { return strings.Join(ss, "\n") },
		"fmtTime": func(t time.Time) string {
			if t.IsZero() {
				return "—"
			}
			return t.Local().Format("2006-01-02 15:04:05")
		},
		"fmtTimePtr": func(t *time.Time) string {
			if t == nil || t.IsZero() {
				return "—"
			}
			return t.Local().Format("2006-01-02 15:04:05")
		},
		"buildDur": func(d core.Deployment) string {
			if d.StartedAt == nil || d.FinishedAt == nil || d.FinishedAt.Before(*d.StartedAt) {
				return "—"
			}
			return d.FinishedAt.Sub(*d.StartedAt).Round(time.Second).String()
		},
	}
}

func parseID(s string) (int64, bool) {
	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

const githubStateTTL = 10 * time.Minute

// newGithubState mints and stores a CSRF token for the manifest round-trip.
func (s *Server) newGithubState() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	tok := hex.EncodeToString(b)
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	// Opportunistic GC of expired states.
	for k, exp := range s.stateTokens {
		if time.Now().After(exp) {
			delete(s.stateTokens, k)
		}
	}
	s.stateTokens[tok] = time.Now().Add(githubStateTTL)
	return tok
}

// consumeGithubState validates and removes a state token (one-time use).
func (s *Server) consumeGithubState(tok string) bool {
	if tok == "" {
		return false
	}
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	exp, ok := s.stateTokens[tok]
	if !ok || time.Now().After(exp) {
		delete(s.stateTokens, tok)
		return false
	}
	delete(s.stateTokens, tok)
	return true
}
