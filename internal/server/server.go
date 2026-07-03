// Package server is Slipway's admin web UI and HTTP API: server-rendered
// html/template pages (with htmx and a little vanilla JS, no build step),
// argon2id auth, and an SSE endpoint that streams build logs from the logstream
// broker. It depends on store, logstream, and a Deployer (the deploy worker).
package server

import (
	"context"
	"html/template"
	"io/fs"
	"net/http"
	"strconv"
	"time"

	"github.com/slipwaydev/slipway/internal/core"
	"github.com/slipwaydev/slipway/internal/logstream"
	"github.com/slipwaydev/slipway/internal/store"
)

// Deployer is the slice of the deploy worker the server needs.
type Deployer interface {
	Notify()
	Cancel(ctx context.Context, id int64) (bool, error)
}

// Server holds the HTTP layer's dependencies.
type Server struct {
	store    *store.Store
	deployer Deployer
	broker   *logstream.Broker

	pages      map[string]*template.Template
	setupToken string
	secure     bool // set Secure flag on cookies (behind TLS); false in M1
}

// New constructs a Server, parsing the embedded templates. setupToken guards the
// first-boot admin-creation flow (printed by the caller as a one-time URL).
func New(st *store.Store, d Deployer, br *logstream.Broker, setupToken string) (*Server, error) {
	s := &Server{
		store:      st,
		deployer:   d,
		broker:     br,
		setupToken: setupToken,
	}
	if err := s.parseTemplates(); err != nil {
		return nil, err
	}
	return s, nil
}

// parseTemplates builds one template set per page, each combining base.tmpl with
// the page template (so every page can define its own "content" block).
func (s *Server) parseTemplates() error {
	pages := []string{"login", "setup", "apps", "app", "deployment"}
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

	mux.HandleFunc("GET /{$}", s.requireAuth(s.handleAppsList))
	mux.HandleFunc("POST /apps", s.requireAuth(s.handleCreateApp))
	mux.HandleFunc("GET /apps/{id}", s.requireAuth(s.handleAppDetail))
	mux.HandleFunc("POST /apps/{id}/deploy", s.requireAuth(s.handleDeploy))
	mux.HandleFunc("POST /apps/{id}/env", s.requireAuth(s.handleSetEnv))
	mux.HandleFunc("POST /apps/{id}/env/delete", s.requireAuth(s.handleDeleteEnv))
	mux.HandleFunc("GET /deployments/{id}", s.requireAuth(s.handleDeploymentDetail))
	mux.HandleFunc("GET /deployments/{id}/logs", s.requireAuth(s.handleLogsSSE))
	mux.HandleFunc("POST /deployments/{id}/cancel", s.requireAuth(s.handleCancel))

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
