// Package server is Outhaul's admin web UI and HTTP API: server-rendered
// html/template pages (with htmx and a little vanilla JS, no build step),
// argon2id auth, and an SSE endpoint that streams build logs from the logstream
// broker. It depends on store, logstream, and a Deployer (the deploy worker).
package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"html/template"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/outhaul-dev/outhaul/internal/blobstore"
	"github.com/outhaul-dev/outhaul/internal/compose"
	"github.com/outhaul-dev/outhaul/internal/core"
	"github.com/outhaul-dev/outhaul/internal/docker"
	"github.com/outhaul-dev/outhaul/internal/github"
	"github.com/outhaul-dev/outhaul/internal/gitrepo"
	"github.com/outhaul-dev/outhaul/internal/gitsource"
	"github.com/outhaul-dev/outhaul/internal/hostmetrics"
	"github.com/outhaul-dev/outhaul/internal/logstream"
	"github.com/outhaul-dev/outhaul/internal/sso"
	"github.com/outhaul-dev/outhaul/internal/store"
	"github.com/outhaul-dev/outhaul/internal/traefik"
	"github.com/outhaul-dev/outhaul/internal/update"
	"github.com/outhaul-dev/outhaul/internal/webhook"
)

// PreviewHandler routes a parsed pull_request event to the preview manager.
// *previewmgr.Manager satisfies it. Nil when previews aren't wired. Enqueue
// returns without handling the event (see previewmgr.Manager.Enqueue).
type PreviewHandler interface {
	Enqueue(sourceID int64, ev webhook.PullRequestEvent) error
	DestroyByID(ctx context.Context, parentID, childID int64) error
}

// Deployer is the slice of the deploy worker the server needs.
type Deployer interface {
	Notify()
	Cancel(ctx context.Context, id int64) (bool, error)
}

// Databases is the slice of the dbaas manager the server needs. Provision is
// asynchronous (it can pull an image); the rest are immediate.
type Databases interface {
	Provision(d core.Database)
	ProvisionSync(ctx context.Context, d core.Database) error
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
	RunStateNow()
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
	ListImages(ctx context.Context, refPattern string) ([]string, error)
	ListVolumesFull(ctx context.Context, match map[string]string) ([]docker.VolumeInfo, error)
	RemoveVolume(ctx context.Context, name string, force bool) error
}

// CertSyncer re-mints local-CA certificates; satisfied by *localca.Manager.
type CertSyncer interface{ Sync(context.Context) error }

// Server holds the HTTP layer's dependencies.
type Server struct {
	store     *store.Store
	deployer  Deployer
	runtime   Runtime
	compose   compose.Runner
	databases Databases
	backups   Backups
	broker    *logstream.Broker
	gh        github.Client       // raw GitHub API: the manifest/installation connect flow only
	sources   *gitsource.Registry // providers for connected git sources
	previews  PreviewHandler      // routes pull_request webhooks to the preview manager; nil disables previews
	metrics   metricsSampler      // host/self resource sampler for the Metrics page

	restoreEvery  time.Duration // how often the first-start pass retries; tests shorten it
	restoreMarker func(ctx context.Context) (core.RestoreMarker, bool, error)

	pages      map[string]*template.Template
	setupToken string
	publicURL  string
	serverIP   string          // for generated sslip.io template domains; may be empty
	tlsEnabled bool            // ACME or local CA configured: HTTPS automation is available
	routing    traefik.Routing // the server-wide routing settings the pages show

	azureDestinationSet bool              // OUTHAUL_BACKUP_AZURE_* set: the Azure row belongs to the environment
	stateBackup         *StateBackupPanel // nil or no schedule: no State backup panel

	defaultLimits core.Limits // per-container server default; zero fields mean no limit
	buildCap      int         // OUTHAUL_MAX_CONCURRENT_BUILDS; 0 = unset (no note)

	repos      *gitrepo.Manager // push-app bare repos; nil disables push-repo cleanup
	sshControl SSHControl       // git-push SSH server; nil disables the port setting
	tunnel     TunnelControl    // Cloudflare Tunnel management; nil disables the tunnel card
	proxy      ProxyControl     // reconciles Traefik before sign-in is turned on; nil refuses to turn it on
	build      update.Build     // the running binary's stamp; /healthz reports its commit
	updates    *update.Dir      // shared with the root updater; nil hides the Updates panel
	checker    *update.Checker  // the running build against main; set with updates
	updatesOff bool             // OUTHAUL_UPDATES=off: the panel explains, the routes are absent
	certSync   CertSyncer       // local-CA cert re-mint hook; nil disables cert re-sync
	caFile     string           // path to the local CA root cert; "" disables /ca.pem and the settings card

	stateMu     sync.Mutex
	stateTokens map[string]time.Time // CSRF states for the GitHub App manifest flow

	ghReposMu sync.Mutex
	ghRepos   map[int64]*repoCache // per git source; see gitSourceData

	backfillMu        sync.Mutex
	backfillAttempted map[int64]bool // git source ids already probed for a missing account name, this process lifetime

	streams     context.Context // cancelled by CloseStreams; ends every open log stream
	stopStreams context.CancelFunc

	discover  func(ctx context.Context, cfg sso.Config) (sso.Provider, error) // sso.Discover; tests replace it
	ssoEnv    SSOEnvironment                                                  // set once before Handler; the zero value means "not from the environment"
	ssoMu     sync.Mutex
	ssoCached *ssoConn // nil until discovery succeeds; dropped when the settings change
	ssoGen    int      // bumped by dropSSOProvider, so a stale discovery isn't cached

	ssoFailed       error         // the last discovery failure, returned until ssoFailedUntil
	ssoFailedUntil  time.Time     // see ssoRetryAfter
	ssoRetryAfter   time.Duration // how long a failed discovery is remembered (ssoDiscoveryRetry; tests shorten it)
	ssoDiscovering  chan struct{} // holds one token while a discovery runs, so only one does
	ssoExchanges    chan struct{} // bounds calls to the provider's token endpoint; see withExchangeSlot
	ssoExchangeWait time.Duration // how long a callback waits for an exchange slot

	hashSlots chan struct{} // bounds concurrent argon2 work; see withHashSlot
	hashWait  time.Duration // how long a password check waits for a slot

	signIn    *signInLimiter // nil unless OUTHAUL_SIGNIN_BACKOFF=on
	signInLog onceAMinute    // the "locked" line; see checkPassword

	httpsPort string // the host port Traefik's HTTPS entrypoint is published on; see appPort

	strictPolicy bool // OUTHAUL_CONTAINER_POLICY=strict: databases publish no host port

	formKey []byte // keys the opaque values a form names other apps by; see hiddenAppKey

	gateNoPublicURLLog onceAMinute // the gate's "no public URL" line; see gateSignIn
	gateBadHostLog     onceAMinute // the gate's "isn't a host name" line
	gateNoPendingLog   onceAMinute // the gate's "no sign-in in progress" line
	gateBadCodeLog     onceAMinute // the gate's "refused a sign-in code" line
	gateMovedLog       onceAMinute // /gate/start's "moved, but arrived elsewhere" line
	ssoMovedLog        onceAMinute // SSO start's "moved, but arrived elsewhere" line
}

// New constructs a Server, parsing the embedded templates. setupToken guards the
// first-boot admin-creation flow (printed by the caller as a one-time URL).
// publicURL is Outhaul's externally reachable base URL, used to build the
// GitHub App manifest's callback and webhook URLs.
func New(st *store.Store, d Deployer, rt Runtime, cp compose.Runner, dbm Databases, bk Backups, br *logstream.Broker, gh github.Client, reg *gitsource.Registry, previews PreviewHandler, publicURL, serverIP string, tlsEnabled bool, setupToken string) (*Server, error) {
	s := &Server{
		store:             st,
		deployer:          d,
		runtime:           rt,
		compose:           cp,
		databases:         dbm,
		backups:           bk,
		broker:            br,
		gh:                gh,
		sources:           reg,
		previews:          previews,
		metrics:           hostmetrics.NewSampler("/"),
		publicURL:         publicURL,
		serverIP:          serverIP,
		tlsEnabled:        tlsEnabled,
		setupToken:        setupToken,
		restoreEvery:      time.Minute,
		restoreMarker:     st.RestoreMarker,
		stateTokens:       map[string]time.Time{},
		backfillAttempted: map[int64]bool{},
		discover:          sso.Discover,
	}
	s.streams, s.stopStreams = context.WithCancel(context.Background())
	s.formKey = []byte(rand.Text())
	s.hashSlots = make(chan struct{}, maxConcurrentHashes)
	s.hashWait = defaultHashWait
	s.ssoRetryAfter = ssoDiscoveryRetry
	s.ssoDiscovering = make(chan struct{}, 1)
	s.ssoExchanges = make(chan struct{}, maxConcurrentSSOExchanges)
	s.ssoExchangeWait = defaultSSOExchangeWait
	if err := s.parseTemplates(); err != nil {
		return nil, err
	}
	return s, nil
}

// SetDefaultLimits records the server's per-container default limits. The
// Limits panel and its route exist only when at least one field is set.
func (s *Server) SetDefaultLimits(l core.Limits) { s.defaultLimits = l }

// SetBuildCap records how many builds run at once, for the queued
// deployment's "waiting its turn" note. 0 means the setting is unset.
func (s *Server) SetBuildCap(n int) { s.buildCap = n }

// hasDefaultLimits reports whether a server default is set at all.
func (s *Server) hasDefaultLimits() bool { return s.defaultLimits != (core.Limits{}) }

// CloseStreams ends every open log stream. A stream otherwise lasts until its
// deployment finishes, its container stops or the browser goes away, so one
// open log page kept http.Server.Shutdown waiting out its whole timeout.
// Register it with http.Server.RegisterOnShutdown.
func (s *Server) CloseStreams() { s.stopStreams() }

// streamContext is r's context, also cancelled by CloseStreams. Long-lived
// handlers (SSE) wait on it instead of r.Context().
func (s *Server) streamContext(r *http.Request) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(r.Context())
	stop := context.AfterFunc(s.streams, cancel)
	return ctx, func() { stop(); cancel() }
}

// SetCertSync installs the hook that refreshes local-CA certs after domain
// changes. Call before Handler.
// SetAzureDestinationSet records whether the environment owns the Azure row.
func (s *Server) SetAzureDestinationSet(on bool) { s.azureDestinationSet = on }

// StateBackupPanel is what Settings shows for the state-backup schedule.
type StateBackupPanel struct {
	Schedule    string
	Destination string // the configured name, or "Azure Blob"
	Keep        int
	Prefix      string
}

// SetStateBackup installs the panel data; an empty schedule hides it.
func (s *Server) SetStateBackup(p StateBackupPanel) { s.stateBackup = &p }

func (s *Server) SetCertSync(cs CertSyncer) { s.certSync = cs }

// SetLocalCAFile publishes the CA root certificate at GET /ca.pem.
func (s *Server) SetLocalCAFile(path string) { s.caFile = path }

// SetRouting installs the server-wide routing settings the pages show. Call
// before Handler.
func (s *Server) SetRouting(rt traefik.Routing) { s.routing = rt }

// SetSignInBackoff turns on the per-username lock after repeated failed
// password sign-ins. Call before Handler.
func (s *Server) SetSignInBackoff(on bool) {
	if on {
		s.signIn = newSignInLimiter(time.Now)
		return
	}
	s.signIn = nil
}

// syncCerts refreshes local-CA certs after a domain change; best-effort — a
// route reaches Traefik on the next deploy anyway, minting can lag a beat.
func (s *Server) syncCerts(ctx context.Context) {
	if s.certSync == nil {
		return
	}
	if err := s.certSync.Sync(ctx); err != nil {
		log.Printf("local CA sync: %v", err)
	}
}

// parseTemplates builds one template set per page, each combining base.tmpl with
// the page template (so every page can define its own "content" block).
func (s *Server) parseTemplates() error {
	pages := []string{"login", "setup", "noaccess", "gate_signout", "gate_refused", "overview", "projects", "project", "apps", "app", "database", "databases", "deployment", "containers", "github_connect", "settings", "restore", "placeholder", "templates", "domains", "volumes", "metrics", "users", "account_setup", "account"}
	s.pages = make(map[string]*template.Template, len(pages))
	for _, p := range pages {
		t := template.New("base").Funcs(templateFuncs())
		// appform.tmpl is a shared partial (the create-app form, used by the
		// Apps and project-detail pages); parsing it into every set is harmless.
		t, err := t.ParseFS(templatesFS, "templates/base.tmpl", "templates/appform.tmpl", "templates/hint.tmpl", "templates/copybtn.tmpl", "templates/accesscell.tmpl", "templates/domainform.tmpl", "templates/volumeform.tmpl", "templates/backups.tmpl", "templates/"+p+".tmpl")
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
	for _, rt := range s.routes() {
		if rt.level == levelUnset {
			panic("server: route " + rt.pattern + " has no access level")
		}
		mux.HandleFunc(rt.pattern, s.guard(rt))
	}

	// SameSite=Lax alone is not CSRF protection here: PR previews and apps on
	// sibling subdomains are same-site with the admin host, so their pages
	// would ride the session cookie. Refuse any unsafe request whose fetch
	// metadata (or Origin) says it came from another origin. Webhooks are
	// server-to-server and carry neither header, so they pass.
	return http.NewCrossOriginProtection().Handler(limitBodies(mux))
}

// maxRequestBody bounds every request body. The largest legitimate ones (a
// pasted compose file, a webhook, which reads at most 1 MiB anyway) are far
// smaller; unbounded, an unauthenticated multipart POST was parsed in full,
// spilling to temp files.
const maxRequestBody = 1 << 20

// limitBodies refuses a body declared over maxRequestBody and cuts off an
// undeclared (chunked) one once it passes the limit.
func limitBodies(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ContentLength > maxRequestBody {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
		next.ServeHTTP(w, r)
	})
}

// render executes the named page template with the base layout. It renders into
// a buffer first so a template error mid-execution becomes a clean 500 instead
// of a corrupt, half-written 200 (whose second WriteHeader the net/http server
// logs as a "superfluous response.WriteHeader call"). Pages are small, so the
// extra buffer is negligible.
func (s *Server) render(w http.ResponseWriter, r *http.Request, status int, page string, data map[string]any) {
	t, ok := s.pages[page]
	if !ok {
		http.Error(w, "unknown page: "+page, http.StatusInternalServerError)
		return
	}
	if data == nil {
		data = map[string]any{}
	}
	// What the person may do. A public page has no person, so these stay zero.
	me, _ := userFrom(r)
	data["UpdateAvailable"] = me.Role.AtLeast(core.RoleSystemAdmin) && s.updateAvailable() // the navigation badge

	// No template needs the hash, so it never reaches one.
	me.PasswordHash = ""
	data["Me"] = me
	data["IsSystemAdmin"] = me.Role.AtLeast(core.RoleSystemAdmin)
	data["CanManage"] = me.Role.AtLeast(core.RoleProjectAdmin)
	data["CanChange"] = s.canChange(r)
	data["HasRole"] = me.Role.AtLeast(core.RoleProjectViewer)
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, "base", data); err != nil {
		// Nothing has been written to w yet, so a real 500 still reaches the
		// client. Log the actual error, which the old direct-to-w path hid.
		log.Printf("server: render %q: %v", page, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

func templateFuncs() template.FuncMap {
	return template.FuncMap{
		// stateClass maps a deployment status to its CSS class.
		"stateClass": func(s core.DeployStatus) string { return "state-" + string(s) },
		// isStatus reports whether a deployment status equals a plain string —
		// html/template's eq cannot compare a named DeployStatus to a literal.
		"isStatus": func(s core.DeployStatus, want string) bool { return string(s) == want },
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
		// fmtCPU renders thousandths of a CPU as 1, 0.5 or 1.5.
		"fmtCPU": fmtCPU,
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
