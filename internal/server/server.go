// Package server is Slipway's admin web UI and HTTP API: server-rendered
// html/template pages (with htmx and a little vanilla JS, no build step),
// argon2id auth, and an SSE endpoint that streams build logs from the logstream
// broker. It depends on store, logstream, and a Deployer (the deploy worker).
package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"html/template"
	"io/fs"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/slipwaydev/slipway/internal/core"
	"github.com/slipwaydev/slipway/internal/docker"
	"github.com/slipwaydev/slipway/internal/github"
	"github.com/slipwaydev/slipway/internal/logstream"
	"github.com/slipwaydev/slipway/internal/store"
)

// Deployer is the slice of the deploy worker the server needs.
type Deployer interface {
	Notify()
	Cancel(ctx context.Context, id int64) (bool, error)
}

// Runtime is the slice of the Docker client the server needs for app lifecycle.
type Runtime interface {
	FindContainer(ctx context.Context, name string) (*docker.Container, error)
	StartContainer(ctx context.Context, id string) error
	StopContainer(ctx context.Context, id string, timeout time.Duration) error
	RemoveContainer(ctx context.Context, id string, force bool) error
}

// Server holds the HTTP layer's dependencies.
type Server struct {
	store    *store.Store
	deployer Deployer
	runtime  Runtime
	broker   *logstream.Broker
	gh       github.Client

	pages      map[string]*template.Template
	setupToken string
	publicURL  string
	secure     bool // Secure cookie flag; the admin UI is served directly over HTTP (not behind Traefik), so this stays false

	stateMu     sync.Mutex
	stateTokens map[string]time.Time // CSRF states for the GitHub App manifest flow
}

// New constructs a Server, parsing the embedded templates. setupToken guards the
// first-boot admin-creation flow (printed by the caller as a one-time URL).
// publicURL is Slipway's externally reachable base URL, used to build the
// GitHub App manifest's callback and webhook URLs.
func New(st *store.Store, d Deployer, rt Runtime, br *logstream.Broker, gh github.Client, publicURL, setupToken string) (*Server, error) {
	s := &Server{
		store:       st,
		deployer:    d,
		runtime:     rt,
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
	pages := []string{"login", "setup", "overview", "apps", "app", "deployment", "deployments", "github_connect", "placeholder"}
	s.pages = make(map[string]*template.Template, len(pages))
	for _, p := range pages {
		t := template.New("base").Funcs(templateFuncs())
		t, err := t.ParseFS(templatesFS, "templates/base.tmpl", "templates/"+p+".tmpl")
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
	mux.HandleFunc("GET /apps", s.requireAuth(s.handleAppsList))
	mux.HandleFunc("POST /apps", s.requireAuth(s.handleCreateApp))
	mux.HandleFunc("GET /apps/{id}", s.requireAuth(s.handleAppDetail))
	mux.HandleFunc("POST /apps/{id}/deploy", s.requireAuth(s.handleDeploy))
	mux.HandleFunc("POST /apps/{id}/settings", s.requireAuth(s.handleAppSettings))
	mux.HandleFunc("POST /apps/{id}/env", s.requireAuth(s.handleSetEnv))
	mux.HandleFunc("POST /apps/{id}/env/delete", s.requireAuth(s.handleDeleteEnv))
	mux.HandleFunc("POST /apps/{id}/stop", s.requireAuth(s.handleStopApp))
	mux.HandleFunc("POST /apps/{id}/restart", s.requireAuth(s.handleRestartApp))
	mux.HandleFunc("POST /apps/{id}/delete", s.requireAuth(s.handleDeleteApp))
	mux.HandleFunc("GET /deployments", s.requireAuth(s.handleDeployments))
	mux.HandleFunc("GET /deployments/{id}", s.requireAuth(s.handleDeploymentDetail))
	mux.HandleFunc("GET /deployments/{id}/logs", s.requireAuth(s.handleLogsSSE))
	mux.HandleFunc("POST /deployments/{id}/cancel", s.requireAuth(s.handleCancel))

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
