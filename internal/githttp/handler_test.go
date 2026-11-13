package githttp

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/outhaul-dev/outhaul/internal/core"
	"github.com/outhaul-dev/outhaul/internal/githook"
	"github.com/outhaul-dev/outhaul/internal/gitrepo"
	"github.com/outhaul-dev/outhaul/internal/logstream"
	"github.com/outhaul-dev/outhaul/internal/store"
)

const (
	testAddr  = "203.0.113.9"  // where every request comes from unless it says otherwise
	otherAddr = "198.51.100.4" // another client
)

// countingStore counts FindPushToken calls, so a test can show a password
// was never looked up.
type countingStore struct {
	*store.Store
	finds atomic.Int64
}

func (c *countingStore) FindPushToken(ctx context.Context, raw string) (core.PushToken, bool, error) {
	c.finds.Add(1)
	return c.Store.FindPushToken(ctx, raw)
}

// fakeWorker stands in for the deploy worker: told a deployment is queued, it
// marks it running and closes its log, as a quick successful build would.
type fakeWorker struct {
	st    *store.Store
	br    *logstream.Broker
	delay time.Duration // how long it stays silent before it starts
	mu    sync.Mutex
	wg    sync.WaitGroup
}

func (w *fakeWorker) Notify() {
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		time.Sleep(w.delay)
		w.mu.Lock()
		defer w.mu.Unlock()
		ctx := context.Background()
		for {
			dep, err := w.st.NextClaimable(ctx, true)
			if err != nil || dep == nil {
				return
			}
			if ok, err := w.st.ClaimDeployment(ctx, dep.ID); err != nil || !ok {
				return
			}
			w.br.Publish(dep.ID, "→ building image")
			w.st.SetStatus(ctx, dep.ID, core.StatusBuilding, core.StatusDeploying, "")
			w.st.SetStatus(ctx, dep.ID, core.StatusDeploying, core.StatusRunning, "")
			w.br.Close(dep.ID)
		}
	}()
}

// harnessConfig is what a test may change before the harness's server starts.
type harnessConfig struct {
	naming     core.Naming
	protectNew bool
	keepAlive  int
	// workerDelay keeps the fake worker silent that long after it is told
	// of a deployment, so the hook waits on the bridge with nothing to say.
	workerDelay time.Duration
	// stamp, when set, is told when each handler starts, writes and returns.
	stamp func(path, event string)
	// after, when set, is called after each handler returns.
	after      func(r *http.Request)
	afterStart func(*os.Process)
	afterWait  func(*os.ProcessState)
}

// harness is a Handler behind a real HTTP server, a real store, real bare
// repositories and a real hook bridge, with a fake deploy worker.
type harness struct {
	t      *testing.T
	st     *store.Store
	cs     *countingStore
	dbPath string
	root   string
	repos  *gitrepo.Manager
	h      *Handler
	srv    *httptest.Server
	url    string
	clock  *fakeClock

	plain    atomic.Bool // PlainHTTP's answer
	notReady atomic.Bool // Ready's answer, negated

	streams     context.Context // ends every StreamContext, as shutdown does
	stopStreams context.CancelFunc
}

func newHarness(t *testing.T, opts ...func(*harnessConfig)) *harness {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	var cfg harnessConfig
	for _, o := range opts {
		o(&cfg)
	}

	// The server's git and the hook read HOME; keep them off the
	// developer's own configuration.
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(home, ".gitconfig"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")

	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := store.Open(dbPath, nil)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	// A unix socket's path is short; t.TempDir's can be too long.
	sockDir, err := os.MkdirTemp("", "gh")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sockDir) })
	sock := filepath.Join(sockDir, "hook.sock")
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "git")
	repos := gitrepo.New(root, bin, sock)

	br := logstream.New()
	worker := &fakeWorker{st: st, br: br, delay: cfg.workerDelay}
	t.Cleanup(worker.wg.Wait)

	srv := httptest.NewUnstartedServer(nil)
	base := "http://" + srv.Listener.Addr().String()

	bridge := githook.NewBridge(st, br, worker, repos, cfg.naming, cfg.protectNew, 30*time.Second)
	bridge.SetPublicURL(base)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen on the hook socket: %v", err)
	}
	bridgeCtx, stopBridge := context.WithCancel(context.Background())
	t.Cleanup(stopBridge)
	go bridge.Serve(bridgeCtx, ln)

	h := &harness{t: t, st: st, cs: &countingStore{Store: st}, dbPath: dbPath, root: root, repos: repos, srv: srv, url: base}
	h.clock = &fakeClock{t: time.Now()}
	h.streams, h.stopStreams = context.WithCancel(context.Background())
	t.Cleanup(h.stopStreams)

	h.h = New(Options{
		Store:     h.cs,
		Repos:     repos,
		PublicURL: base,
		MaxDays:   30,
		SSOMaxAge: 7 * 24 * time.Hour,
		Limiter:   NewLimiter(h.clock.now),
		ClientAddr: func(r *http.Request) netip.Addr {
			if v := r.Header.Get("X-Test-Addr"); v != "" {
				a, _ := netip.ParseAddr(v)
				return a
			}
			return netip.MustParseAddr(testAddr)
		},
		OnPublicHost: func(r *http.Request) bool { return r.Host == srv.Listener.Addr().String() },
		PlainHTTP:    func(*http.Request) bool { return h.plain.Load() },
		StreamContext: func(r *http.Request) (context.Context, context.CancelFunc) {
			ctx, cancel := context.WithCancel(r.Context())
			stop := context.AfterFunc(h.streams, cancel)
			return ctx, func() { stop(); cancel() }
		},
		Ready:     func() bool { return !h.notReady.Load() },
		Now:       h.clock.now,
		KeepAlive: cfg.keepAlive,
	})
	h.h.afterStart = cfg.afterStart
	h.h.afterWait = cfg.afterWait

	wrap := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if cfg.stamp != nil {
				path := r.URL.Path
				cfg.stamp(path, "start")
				w = &stampedWriter{ResponseWriter: w, stamp: func() { cfg.stamp(path, "write") }}
				defer cfg.stamp(path, "end")
			}
			if cfg.after != nil {
				defer cfg.after(r)
			}
			next(w, r)
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /git/{repo}/info/refs", wrap(h.h.InfoRefs))
	mux.HandleFunc("POST /git/{repo}/git-receive-pack", wrap(h.h.Service))
	mux.HandleFunc("POST /git/{repo}/git-upload-pack", wrap(h.h.Service))
	srv.Config.Handler = mux
	srv.Start()
	t.Cleanup(srv.Close)
	return h
}

// stampedWriter tells the test when each write to the response happens.
type stampedWriter struct {
	http.ResponseWriter
	stamp func()
}

func (s *stampedWriter) Write(p []byte) (int, error) {
	s.stamp()
	return s.ResponseWriter.Write(p)
}

func (s *stampedWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// account makes a password account with role, a member of each project.
func (h *harness) account(name string, role core.Role, projects ...int64) core.User {
	h.t.Helper()
	ctx := context.Background()
	u, err := h.st.CreatePasswordAccount(ctx, name, role)
	if err != nil {
		h.t.Fatalf("CreatePasswordAccount(%s): %v", name, err)
	}
	for _, p := range projects {
		if err := h.st.AddProjectMember(ctx, p, u.ID); err != nil {
			h.t.Fatalf("AddProjectMember: %v", err)
		}
	}
	return u
}

// token makes a token for the account, created at created and expiring at expires.
func (h *harness) token(u core.User, label string, created, expires time.Time) (string, core.PushToken) {
	h.t.Helper()
	raw, tok, err := h.st.CreatePushToken(context.Background(), u.ID, label, created, expires)
	if err != nil {
		h.t.Fatalf("CreatePushToken: %v", err)
	}
	return raw, tok
}

// liveToken is a token made now that lasts a week.
func (h *harness) liveToken(u core.User) string {
	h.t.Helper()
	now := time.Now()
	raw, _ := h.token(u, "laptop "+strconv.FormatInt(now.UnixNano(), 10), now, now.Add(7*24*time.Hour))
	return raw
}

func (h *harness) pushApp(name string) core.App {
	h.t.Helper()
	app, err := h.st.CreateApp(context.Background(), core.App{Name: name, Source: core.SourcePush, Branch: "main", Kind: core.KindNixpacks})
	if err != nil {
		h.t.Fatalf("CreateApp(%s): %v", name, err)
	}
	return app
}

func (h *harness) remote(app string) string { return h.url + "/git/" + app + ".git" }

// rawDB is a second connection to the store's file, for a column no store
// function sets to a chosen time.
func (h *harness) rawDB() *sql.DB {
	h.t.Helper()
	db, err := sql.Open("sqlite", "file:"+url.PathEscape(h.dbPath)+"?_pragma=busy_timeout(5000)")
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { db.Close() })
	return db
}

// ssoAccount makes an SSO system admin; signedIn, when set, is its last sign-in.
func (h *harness) ssoAccount(subject string, signedIn *time.Time) core.User {
	h.t.Helper()
	ctx := context.Background()
	u, _, err := h.st.FindOrCreateSSOUser(ctx, "https://id.example.com", subject, subject+"@example.com", core.RoleSystemAdmin, true)
	if err != nil {
		h.t.Fatalf("FindOrCreateSSOUser: %v", err)
	}
	if signedIn != nil {
		if _, err := h.rawDB().Exec(`UPDATE users SET last_sign_in_at = ? WHERE id = ?`, signedIn.UTC().Format(time.RFC3339Nano), u.ID); err != nil {
			h.t.Fatal(err)
		}
	}
	return u
}

type reqOpt func(*http.Request)

func basic(password string) reqOpt { return func(r *http.Request) { r.SetBasicAuth("x", password) } }
func fromAddr(a string) reqOpt     { return func(r *http.Request) { r.Header.Set("X-Test-Addr", a) } }
func onHost(host string) reqOpt    { return func(r *http.Request) { r.Host = host } }
func withHeader(k, v string) reqOpt {
	return func(r *http.Request) { r.Header.Set(k, v) }
}

var testClient = &http.Client{Timeout: 30 * time.Second}

// request sends one request and returns the response and its whole body.
func (h *harness) request(method, path string, body io.Reader, opts ...reqOpt) (*http.Response, string) {
	h.t.Helper()
	if method == "" {
		method = http.MethodGet
	}
	req, err := http.NewRequest(method, h.url+path, body)
	if err != nil {
		h.t.Fatal(err)
	}
	for _, o := range opts {
		o(req)
	}
	resp, err := testClient.Do(req)
	if err != nil {
		h.t.Fatalf("%s %s: %v", method, path, err)
	}
	b, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		h.t.Fatalf("read the body of %s %s: %v", method, path, err)
	}
	return resp, string(b)
}

const receiveRefs = "/git/api.git/info/refs?service=git-receive-pack"

// git runs the real git client in dir with its configuration and credential
// helper in a temporary directory: it asks for a password through an
// askpass script that answers token.
func git(t *testing.T, dir, token string, args ...string) (string, error) {
	t.Helper()
	home := t.TempDir()
	askpass := filepath.Join(home, "askpass.sh")
	script := "#!/bin/sh\ncase \"$1\" in\nUsername*) echo x ;;\n*) echo '" + token + "' ;;\nesac\n"
	if err := os.WriteFile(askpass, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", append([]string{"-c", "user.name=Test", "-c", "user.email=test@example.com"}, args...)...)
	cmd.Dir = dir
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + home,
		"GIT_CONFIG_GLOBAL=" + filepath.Join(home, ".gitconfig"),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=" + askpass,
		"LC_ALL=C",
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// mustGit is git that fails the test on an error, and returns the trimmed output.
func mustGit(t *testing.T, dir, token string, args ...string) string {
	t.Helper()
	out, err := git(t, dir, token, args...)
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(out)
}

// workTree is a new repository on branch main with one commit.
func workTree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	mustGit(t, dir, "", "init", "-q", "-b", "main")
	commit(t, dir, "README", "hello\n")
	return dir
}

// commit writes name and commits it, returning the commit's id.
func commit(t *testing.T, dir, name, content string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, dir, "", "add", name)
	mustGit(t, dir, "", "commit", "-q", "-m", "change "+name)
	return mustGit(t, dir, "", "rev-parse", "HEAD")
}

func noCacheHeaders(t *testing.T, resp *http.Response) {
	t.Helper()
	want := map[string]string{
		"Expires":       "Fri, 01 Jan 1980 00:00:00 GMT",
		"Pragma":        "no-cache",
		"Cache-Control": "no-cache, max-age=0, must-revalidate",
	}
	for k, v := range want {
		if got := resp.Header.Get(k); got != v {
			t.Errorf("%d answer: %s = %q, want %q", resp.StatusCode, k, got, v)
		}
	}
}

func TestPushDeploys(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	admin := h.account("root", core.RoleSystemAdmin)
	tok := h.liveToken(admin)
	app := h.pushApp("api")
	work := workTree(t)

	out := mustGit(t, work, tok, "push", h.remote("api"), "main")
	for _, want := range []string{"→ build log: " + h.url + "/deployments/", "✓ deployed"} {
		if !strings.Contains(out, want) {
			t.Errorf("push output lacks %q:\n%s", want, out)
		}
	}
	deps, err := h.st.ListDeploymentsForApp(ctx, app.ID)
	if err != nil || len(deps) == 0 {
		t.Fatalf("deployments = %v, %v; want one", deps, err)
	}
	pt, ok, err := h.st.FindPushToken(ctx, tok)
	if err != nil || !ok {
		t.Fatalf("FindPushToken: %v %v", ok, err)
	}
	if pt.LastUsedAt == nil || pt.LastUsedIP != testAddr {
		t.Errorf("last use = %v from %q, want a time from %s", pt.LastUsedAt, pt.LastUsedIP, testAddr)
	}
}

// A hook left stale (by a moved binary or socket) is rewritten by the next
// push, as over SSH.
func TestPushRepairsAStaleHook(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tok := h.liveToken(h.account("root", core.RoleSystemAdmin))
	app := h.pushApp("api")
	work := workTree(t)
	mustGit(t, work, tok, "push", h.remote("api"), "main")

	dir, err := h.repos.Path("api")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "hooks", "post-receive"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	before, err := h.st.ListDeploymentsForApp(ctx, app.ID)
	if err != nil {
		t.Fatal(err)
	}

	commit(t, work, "second.txt", "again\n")
	out := mustGit(t, work, tok, "push", h.remote("api"), "main")
	if !strings.Contains(out, "→ build log: "+h.url+"/deployments/") {
		t.Errorf("the second push's output lacks the build-log line:\n%s", out)
	}
	after, err := h.st.ListDeploymentsForApp(ctx, app.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before)+1 {
		t.Errorf("deployments: %d before the second push, %d after; want one more", len(before), len(after))
	}
}

// refusal is one request the handler must refuse.
type refusal struct {
	name      string
	method    string
	path      string
	opts      []reqOpt
	before    func() // run before the request
	after     func() // run after it
	status    int
	challenge bool
	exact     string // the whole body, when set
	contains  string // part of the body, when set
	noLookup  bool   // FindPushToken must not be called
}

// refusalFixture is what the refusals were made with.
type refusalFixture struct {
	good, member, ssoStale string
	ssoStaleUser           core.User
	tokens                 []string // every token value the cases use
}

func refusalCases(t *testing.T, h *harness) ([]refusal, refusalFixture) {
	t.Helper()
	ctx := context.Background()
	now := time.Now()
	h.pushApp("api")
	other, err := h.st.CreateProject(ctx, core.Project{Name: "Other"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.st.CreateApp(ctx, core.App{Name: "stack", Source: core.SourceGithub, Kind: core.KindCompose,
		ComposePath: "compose.yaml", GithubRepo: "example/stack", Branch: "main"}); err != nil {
		t.Fatalf("CreateApp(stack): %v", err)
	}

	admin := h.account("root", core.RoleSystemAdmin)
	good := h.liveToken(admin)
	if _, err := h.st.CreateUser(ctx, "pat", "$argon2id$v=19$m=65536,t=1,p=4$c2FsdHNhbHQ$aGFzaGhhc2g"); err != nil {
		t.Fatal(err)
	}
	expired, _ := h.token(admin, "expired", now.Add(-48*time.Hour), now.Add(-24*time.Hour))
	old, _ := h.token(admin, "old", now.AddDate(0, 0, -31), now.AddDate(1, 0, 0))
	revoked, revokedRow := h.token(admin, "revoked", now, now.Add(time.Hour))
	if err := h.st.DeletePushTokenForUser(ctx, admin.ID, revokedRow.ID); err != nil {
		t.Fatal(err)
	}
	member := h.liveToken(h.account("mo", core.RoleProjectMember, other.ID))
	viewer := h.liveToken(h.account("vi", core.RoleProjectViewer, core.DefaultProjectID))
	eightDays := now.Add(-8 * 24 * time.Hour)
	ssoStaleUser := h.ssoAccount("sam", &eightDays)
	ssoStale := h.liveToken(ssoStaleUser)
	ssoNever := h.liveToken(h.ssoAccount("nia", nil))
	session := strings.Repeat("5e", 32)
	if err := h.st.CreateSession(ctx, core.Session{Token: session, UserID: admin.ID, ExpiresAt: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	unknown := core.PushTokenPrefix + strings.Repeat("ab", 32)

	ghost := "/git/ghost.git/info/refs?service=git-receive-pack"
	cases := []refusal{
		{name: "no credentials", path: receiveRefs, status: 401, challenge: true,
			exact: "outhaul: sign in with any username and a push token as the password. Create one at " + h.url + "/account\n"},
		{name: "the account's own password", path: receiveRefs, opts: []reqOpt{basic("correct horse battery staple")},
			status: 401, challenge: true, exact: "outhaul: that push token isn't valid. Create one at " + h.url + "/account\n", noLookup: true},
		{name: "an unknown token", path: receiveRefs, opts: []reqOpt{basic(unknown)}, status: 401, challenge: true, contains: "that push token isn't valid"},
		{name: "an expired token", path: receiveRefs, opts: []reqOpt{basic(expired)}, status: 401, challenge: true,
			contains: "outhaul: that push token expired on " + now.Add(-24*time.Hour).UTC().Format("2 January 2006") + ". Create a new one at " + h.url + "/account\n"},
		{name: "a token older than the maximum", path: receiveRefs, opts: []reqOpt{basic(old)}, status: 401, challenge: true, contains: "expired on"},
		{name: "a revoked token", path: receiveRefs, opts: []reqOpt{basic(revoked)}, status: 401, challenge: true, contains: "that push token isn't valid"},
		{name: "a session cookie and no token", path: receiveRefs, opts: []reqOpt{func(r *http.Request) {
			r.AddCookie(&http.Cookie{Name: "outhaul_session", Value: session})
		}}, status: 401, challenge: true, contains: "sign in with any username"},
		{name: "a member not on the app's project", path: receiveRefs, opts: []reqOpt{basic(member)}, status: 403,
			exact: "outhaul: you don't have permission to use 'api'\n"},
		{name: "a viewer on the app's project", path: receiveRefs, opts: []reqOpt{basic(viewer)}, status: 403,
			exact: "outhaul: you don't have permission to use 'api'\n"},
		{name: "a name with no app, by a member not on the default project", path: ghost, opts: []reqOpt{basic(member)}, status: 403,
			exact: "outhaul: you don't have permission to use 'ghost'\n"},
		{name: "an SSO sign-in eight days old", path: receiveRefs, opts: []reqOpt{basic(ssoStale)}, status: 403,
			exact: "outhaul: your last SSO sign-in to Outhaul was more than 7 days ago. Sign in at " + h.url + "/login, then push again.\n"},
		{name: "an SSO account that never signed in", path: receiveRefs, opts: []reqOpt{basic(ssoNever)}, status: 403, contains: "your last SSO sign-in"},
		{name: "an app that isn't a push app", path: "/git/stack.git/info/refs?service=git-receive-pack", opts: []reqOpt{basic(good)}, status: 404,
			exact: "outhaul: app 'stack' is not push-deployable\n"},
		{name: "another host", path: receiveRefs, opts: []reqOpt{basic(good), onHost("elsewhere.example.com")}, status: 404},
		{name: "plain HTTP", path: receiveRefs, opts: []reqOpt{basic(good)}, status: 403,
			before: func() { h.plain.Store(true) }, after: func() { h.plain.Store(false) },
			exact: "outhaul: this remote uses http://. Change it to " + h.url + "/git/api.git\n"},
		{name: "plain HTTP to a name that isn't valid", path: "/git/Bad.git/info/refs?service=git-receive-pack", opts: []reqOpt{basic(good)}, status: 403,
			before: func() { h.plain.Store(true) }, after: func() { h.plain.Store(false) },
			exact: "outhaul: this remote uses http://. Change it to " + h.url + "/git/\n"},
		{name: "the bridge isn't listening", path: receiveRefs, opts: []reqOpt{basic(good)}, status: 503,
			before: func() { h.notReady.Store(true) }, after: func() { h.notReady.Store(false) },
			exact: "outhaul: git push is not available; see the server log\n"},
		{name: "info/refs with no service", path: "/git/api.git/info/refs", opts: []reqOpt{basic(good)}, status: 404},
		{name: "a POST that isn't git's", method: http.MethodPost, path: "/git/api.git/git-receive-pack",
			opts: []reqOpt{basic(good), withHeader("Content-Type", "text/plain")}, status: 415},
	}
	return cases, refusalFixture{
		good: good, member: member, ssoStale: ssoStale, ssoStaleUser: ssoStaleUser,
		tokens: []string{good, expired, old, revoked, member, viewer, ssoStale, ssoNever, unknown},
	}
}

func (h *harness) check(t *testing.T, c refusal) {
	t.Helper()
	if c.before != nil {
		c.before()
	}
	if c.after != nil {
		defer c.after()
	}
	finds := h.cs.finds.Load()
	resp, body := h.request(c.method, c.path, nil, c.opts...)
	if resp.StatusCode != c.status {
		t.Errorf("status = %d, want %d; body %q", resp.StatusCode, c.status, body)
	}
	challenge := resp.Header.Get("WWW-Authenticate")
	if c.challenge && challenge != `Basic realm="outhaul"` {
		t.Errorf("WWW-Authenticate = %q, want the challenge", challenge)
	}
	if !c.challenge && challenge != "" {
		t.Errorf("WWW-Authenticate = %q on a %d, want none", challenge, resp.StatusCode)
	}
	if c.exact != "" && body != c.exact {
		t.Errorf("body = %q, want %q", body, c.exact)
	}
	if c.contains != "" && !strings.Contains(body, c.contains) {
		t.Errorf("body = %q, want it to hold %q", body, c.contains)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/plain; charset=utf-8" {
		t.Errorf("Content-Type = %q, want text/plain", ct)
	}
	if !strings.HasSuffix(body, "\n") || strings.Count(body, "\n") != 1 {
		t.Errorf("body %q isn't one line ending in a newline", body)
	}
	if c.noLookup && h.cs.finds.Load() != finds {
		t.Error("the password was looked up as a token")
	}
	noCacheHeaders(t, resp)
}

func TestRefused(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	cases, fx := refusalCases(t, h)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { h.check(t, c) })
	}

	if h.repos.Exists("ghost") {
		t.Error("a refused push created a repository")
	}
	if _, err := h.st.GetAppByName(ctx, "ghost"); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("GetAppByName(ghost) = %v, want no rows", err)
	}

	// The same token works once its owner has signed in with SSO again.
	if err := h.st.TouchUserSignIn(ctx, fx.ssoStaleUser.ID); err != nil {
		t.Fatal(err)
	}
	if resp, body := h.request("", receiveRefs, nil, basic(fx.ssoStale)); resp.StatusCode != http.StatusOK {
		t.Errorf("after signing in again: %d %q, want 200", resp.StatusCode, body)
	}

	// What git prints for a refusal.
	out, err := git(t, workTree(t), fx.member, "push", h.remote("api"), "main")
	if err == nil {
		t.Fatalf("a member not on the project pushed:\n%s", out)
	}
	if !strings.Contains(out, "outhaul: you don't have permission to use 'api'") {
		t.Errorf("git didn't print the refusal:\n%s", out)
	}
}

func TestRefusalsHaveNoCacheHeaders(t *testing.T) {
	h := newHarness(t)
	h.pushApp("api")
	tok := h.liveToken(h.account("root", core.RoleSystemAdmin))

	resp, _ := h.request("", receiveRefs, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no credentials: %d, want 401", resp.StatusCode)
	}
	noCacheHeaders(t, resp)

	resp, body := h.request("", receiveRefs, nil, basic(tok))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("a good token: %d %q, want 200", resp.StatusCode, body)
	}
	noCacheHeaders(t, resp)
	if ct := resp.Header.Get("Content-Type"); ct != "application/x-git-receive-pack-advertisement" {
		t.Errorf("Content-Type = %q", ct)
	}
}

func TestFirstPushCreatesTheApp(t *testing.T) {
	h := newHarness(t, func(c *harnessConfig) {
		c.naming = core.Naming{Suffix: "shared.demo.example.com"}
		c.protectNew = true
	})
	ctx := context.Background()
	tok := h.liveToken(h.account("root", core.RoleSystemAdmin))

	out := mustGit(t, workTree(t), tok, "push", h.remote("newapp"), "main")
	if !strings.Contains(out, "✓ deployed newapp.shared.demo.example.com") {
		t.Errorf("first push:\n%s", out)
	}
	app, err := h.st.GetAppByName(ctx, "newapp")
	if err != nil {
		t.Fatalf("the first push created no app: %v", err)
	}
	if app.Source != core.SourcePush || app.Domain != "newapp.shared.demo.example.com" {
		t.Errorf("app = source %q, domain %q", app.Source, app.Domain)
	}
	rows, err := h.st.ListDomains(ctx, app.ID)
	if err != nil || len(rows) != 1 || !rows[0].Protected {
		t.Errorf("rows = %+v, %v; want one that requires sign-in", rows, err)
	}
}

// pointBareHead makes app's bare HEAD name ref, as `git init --bare` leaves it
// naming the host's default branch.
func (h *harness) pointBareHead(app, ref string) string {
	h.t.Helper()
	dir, err := h.repos.Path(app)
	if err != nil {
		h.t.Fatal(err)
	}
	mustGit(h.t, h.t.TempDir(), "", "--git-dir", dir, "symbolic-ref", "HEAD", ref)
	return dir
}

func TestCloneAndFetch(t *testing.T) {
	logged := &lockedBuffer{}
	prev := log.Writer()
	log.SetOutput(logged)
	t.Cleanup(func() { log.SetOutput(prev) })

	h := newHarness(t)
	tok := h.liveToken(h.account("root", core.RoleSystemAdmin))
	h.pushApp("api")
	work := workTree(t)
	first := mustGit(t, work, "", "rev-parse", "HEAD")
	mustGit(t, work, tok, "push", h.remote("api"), "main")

	var clone string
	for _, v := range []string{"2", "0"} {
		// Whatever the host's git names by default, HEAD names a branch
		// that doesn't exist; a plain clone still checks out the app's.
		h.pointBareHead("api", "refs/heads/nonesuch")
		parent := t.TempDir()
		out := mustGit(t, parent, tok, "-c", "protocol.version="+v, "clone", "-q", h.remote("api"), "c")
		if strings.Contains(out, "nonexistent ref") {
			t.Errorf("protocol %s: the clone warned:\n%s", v, out)
		}
		clone = filepath.Join(parent, "c")
		if got, err := git(t, clone, "", "rev-parse", "HEAD"); err != nil || strings.TrimSpace(got) != first {
			t.Errorf("protocol %s: the clone's HEAD is %q (%v), want %s", v, strings.TrimSpace(got), err, first)
		}
		if data, err := os.ReadFile(filepath.Join(clone, "README")); err != nil || string(data) != "hello\n" {
			t.Errorf("protocol %s: the clone's README = %q, %v; want the pushed file", v, data, err)
		}
	}

	second := commit(t, work, "more", "more\n")
	mustGit(t, work, tok, "push", h.remote("api"), "main")
	mustGit(t, clone, tok, "fetch", "-q", "origin")
	if got := mustGit(t, clone, "", "rev-parse", "origin/main"); got != second {
		t.Errorf("after fetch origin/main = %s, want %s", got, second)
	}

	out, err := git(t, t.TempDir(), tok, "clone", h.remote("nothing"), "n")
	if err == nil || !strings.Contains(out, "outhaul: there is no app called 'nothing'") {
		t.Errorf("clone of a name with no app: %v\n%s", err, out)
	}
	if h.repos.Exists("nothing") {
		t.Error("a clone of a name with no app created a repository")
	}

	// An app whose branch has no commits yet: HEAD is left alone, and the
	// clone isn't refused.
	h.pushApp("web")
	mustGit(t, work, tok, "push", h.remote("web"), "main:dev")
	dir := h.pointBareHead("web", "refs/heads/nonesuch")
	mustGit(t, t.TempDir(), tok, "clone", "-q", h.remote("web"), "w")
	if got := mustGit(t, t.TempDir(), "", "--git-dir", dir, "symbolic-ref", "HEAD"); got != "refs/heads/nonesuch" {
		t.Errorf("web's HEAD = %s, want it left at refs/heads/nonesuch", got)
	}
	if strings.Contains(logged.String(), "HEAD") {
		t.Errorf("the log names HEAD:\n%s", logged.String())
	}
}

func TestProtocolV2Advertisement(t *testing.T) {
	h := newHarness(t)
	tok := h.liveToken(h.account("root", core.RoleSystemAdmin))
	h.pushApp("api")
	const uploadRefs = "/git/api.git/info/refs?service=git-upload-pack"

	resp, body := h.request("", uploadRefs, nil, basic(tok), withHeader("Git-Protocol", "version=2"))
	if resp.StatusCode != http.StatusOK || strings.Contains(body, "# service=") || !strings.Contains(body, "version 2") {
		t.Errorf("version 2: %d %q", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/x-git-upload-pack-advertisement" {
		t.Errorf("Content-Type = %q", ct)
	}
	resp, body = h.request("", uploadRefs, nil, basic(tok))
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(body, "001e# service=git-upload-pack\n0000") {
		t.Errorf("version 0: %d %q", resp.StatusCode, body)
	}
}

func TestQuietHookKeepsTheResponseMoving(t *testing.T) {
	var mu sync.Mutex
	var stamps []time.Time
	// The real hook waits on the bridge, and the worker says nothing for
	// four seconds after the build-log line.
	h := newHarness(t, func(c *harnessConfig) {
		c.keepAlive = 1
		c.workerDelay = 4 * time.Second
		c.stamp = func(path, event string) {
			if !strings.HasSuffix(path, "/git-receive-pack") {
				return
			}
			mu.Lock()
			stamps = append(stamps, time.Now())
			mu.Unlock()
		}
	})
	tok := h.liveToken(h.account("root", core.RoleSystemAdmin))
	h.pushApp("api")

	out := mustGit(t, workTree(t), tok, "push", h.remote("api"), "main")
	for _, want := range []string{"→ build log: ", "✓ deployed"} {
		if !strings.Contains(out, want) {
			t.Errorf("push output lacks %q:\n%s", want, out)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(stamps) < 3 {
		t.Fatalf("the push's POST made %d writes", len(stamps)-2)
	}
	if total := stamps[len(stamps)-1].Sub(stamps[0]); total < 4*time.Second {
		t.Fatalf("the POST took %s; the worker is silent for 4s", total)
	}
	for i := 1; i < len(stamps); i++ {
		if gap := stamps[i].Sub(stamps[i-1]); gap > 2500*time.Millisecond {
			t.Errorf("the response went quiet for %s", gap)
		}
	}
}

func TestFailedAttemptsAreLimitedByAddress(t *testing.T) {
	h := newHarness(t)
	h.pushApp("api")
	good := h.liveToken(h.account("root", core.RoleSystemAdmin))
	bad := core.PushTokenPrefix + strings.Repeat("0f", 32)

	for i := 1; i <= 10; i++ {
		if resp, _ := h.request("", receiveRefs, nil, basic(bad)); resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("bad token %d: %d, want 401", i, resp.StatusCode)
		}
	}
	resp, body := h.request("", receiveRefs, nil, basic(bad))
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("the eleventh: %d %q, want 429", resp.StatusCode, body)
	}
	if n, err := strconv.Atoi(resp.Header.Get("Retry-After")); err != nil || n < 1 || n > 60 {
		t.Errorf("Retry-After = %q, want 1 to 60 seconds", resp.Header.Get("Retry-After"))
	}
	if resp.Header.Get("WWW-Authenticate") != "" {
		t.Error("a 429 carries the challenge")
	}
	if !strings.HasPrefix(body, "outhaul: too many failed sign-ins from your address. Try again in ") {
		t.Errorf("429 body = %q", body)
	}

	if resp, body := h.request("", receiveRefs, nil, basic(good)); resp.StatusCode != http.StatusOK {
		t.Errorf("a good token from the limited address: %d %q, want 200", resp.StatusCode, body)
	}
	if resp, _ := h.request("", receiveRefs, nil, basic(bad), fromAddr(otherAddr)); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("a bad token from another address: %d, want 401", resp.StatusCode)
	}
	if resp, _ := h.request("", receiveRefs, nil); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no credentials from the limited address: %d, want 401", resp.StatusCode)
	}
}

// lockedBuffer is a log destination several goroutines may write to.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestNoTokenInAnyLogLine(t *testing.T) {
	logged := &lockedBuffer{}
	prev := log.Writer()
	log.SetOutput(logged)
	t.Cleanup(func() { log.SetOutput(prev) })

	h := newHarness(t)
	cases, fx := refusalCases(t, h)

	work := workTree(t)
	mustGit(t, work, fx.good, "push", h.remote("api"), "main")
	mustGit(t, t.TempDir(), fx.good, "clone", "-q", h.remote("api"), "c")
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { h.check(t, c) })
	}
	for _, name := range []string{"Bad.git", "a..b.git"} {
		h.request("", "/git/"+name+"/info/refs?service=git-receive-pack", nil, basic(fx.good))
	}
	h.request(http.MethodPost, "/git/api.git/git-upload-pack", strings.NewReader("not gzip"), basic(fx.good),
		withHeader("Content-Type", "application/x-git-upload-pack-request"), withHeader("Content-Encoding", "gzip"))
	for i := 0; i < 12; i++ {
		h.request("", receiveRefs, nil, basic(fx.tokens[1]))
	}

	out := logged.String()
	if !strings.Contains(out, "githttp:") {
		t.Fatalf("nothing was logged, so this test shows nothing:\n%s", out)
	}
	for _, tok := range fx.tokens {
		sum := sha256.Sum256([]byte(tok))
		for what, v := range map[string]string{
			"the token":            tok,
			"its last 64":          tok[len(tok)-64:],
			"its SHA-256":          hex.EncodeToString(sum[:]),
			"the Basic credential": base64.StdEncoding.EncodeToString([]byte("x:" + tok)),
		} {
			if strings.Contains(out, v) {
				t.Errorf("the log holds %s", what)
			}
		}
	}
}

func TestPastedTokenWithWhitespace(t *testing.T) {
	h := newHarness(t)
	h.pushApp("api")
	tok := h.liveToken(h.account("root", core.RoleSystemAdmin))
	resp, body := h.request("", receiveRefs, nil, basic(" "+tok+"\n"))
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(body, "001f# service=git-receive-pack\n0000") {
		t.Errorf("a pasted token with spaces and a newline: %d %q, want 200 and the advertisement", resp.StatusCode, body)
	}
}

// A request whose address can't be read is still served: its failures are
// counted under the zero address, and its use is recorded with no address.
func TestUnknownClientAddress(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.pushApp("api")
	tok := h.liveToken(h.account("root", core.RoleSystemAdmin))
	unknown := fromAddr("not an address")

	if resp, body := h.request("", receiveRefs, nil, basic(tok), unknown); resp.StatusCode != http.StatusOK {
		t.Fatalf("a good token from an unknown address: %d %q, want 200", resp.StatusCode, body)
	}
	pt, _, err := h.st.FindPushToken(ctx, tok)
	if err != nil {
		t.Fatal(err)
	}
	if pt.LastUsedAt == nil || pt.LastUsedIP != "" {
		t.Errorf("last use = %v from %q, want a time and no address", pt.LastUsedAt, pt.LastUsedIP)
	}

	bad := core.PushTokenPrefix + strings.Repeat("0e", 32)
	for i := 1; i <= 10; i++ {
		if resp, _ := h.request("", receiveRefs, nil, basic(bad), unknown); resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("bad token %d: %d, want 401", i, resp.StatusCode)
		}
	}
	if resp, _ := h.request("", receiveRefs, nil, basic(bad), unknown); resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("the eleventh from an unknown address: %d, want 429", resp.StatusCode)
	}
}

func TestGitEnv(t *testing.T) {
	t.Setenv("PATH", "/usr/bin:/bin")
	t.Setenv("HOME", "/var/lib/outhaul")
	req := func(proto string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/git/api.git/git-receive-pack", nil)
		if proto != "" {
			r.Header.Set("Git-Protocol", proto)
		}
		return r
	}
	addr := netip.MustParseAddr(testAddr)
	cases := []struct {
		name    string
		r       *http.Request
		service string
		addr    netip.Addr
		want    []string
	}{
		{"a push", req(""), serviceReceive, addr,
			[]string{"PATH=/usr/bin:/bin", "HOME=/var/lib/outhaul", "OUTHAUL_PUSH_TOKEN_ID=7", "OUTHAUL_PUSH_CLIENT_ADDR=" + testAddr}},
		{"a push from an unknown address", req(""), serviceReceive, netip.Addr{},
			[]string{"PATH=/usr/bin:/bin", "HOME=/var/lib/outhaul", "OUTHAUL_PUSH_TOKEN_ID=7"}},
		{"a fetch in version 2", req("version=2"), serviceUpload, addr,
			[]string{"PATH=/usr/bin:/bin", "HOME=/var/lib/outhaul", "GIT_PROTOCOL=version=2"}},
		{"a protocol header with other characters", req("version=2;x"), serviceUpload, addr,
			[]string{"PATH=/usr/bin:/bin", "HOME=/var/lib/outhaul"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := gitEnv(c.r, c.service, 7, c.addr); !slices.Equal(got, c.want) {
				t.Errorf("gitEnv = %q, want %q", got, c.want)
			}
		})
	}
}

func TestOddRepoNames(t *testing.T) {
	h := newHarness(t)
	tok := h.liveToken(h.account("root", core.RoleSystemAdmin))
	for _, name := range []string{"App.git", "a..b.git", "x.git.git", "-x.git", strings.Repeat("a", 300)} {
		if core.ValidAppName(strings.TrimSuffix(name, ".git")) {
			t.Fatalf("%q is a valid app name once .git is removed; the test wants names that aren't", name)
		}
		resp, body := h.request("", "/git/"+name+"/info/refs?service=git-receive-pack", nil, basic(tok))
		if resp.StatusCode != http.StatusNotFound || body != "outhaul: that isn't a valid app name\n" {
			t.Errorf("%.20s: %d %q, want 404", name, resp.StatusCode, body)
		}
	}
	entries, err := os.ReadDir(h.root)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("the git root holds %d entries, want none", len(entries))
	}
}

func TestGzipRequestBody(t *testing.T) {
	h := newHarness(t)
	tok := h.liveToken(h.account("root", core.RoleSystemAdmin))
	h.pushApp("api")
	mustGit(t, workTree(t), tok, "push", h.remote("api"), "main")

	const lsRefs = "0014command=ls-refs\n0000"
	post := func(body []byte, encoding string) (*http.Response, string) {
		opts := []reqOpt{basic(tok), withHeader("Content-Type", "application/x-git-upload-pack-request"), withHeader("Git-Protocol", "version=2")}
		if encoding != "" {
			opts = append(opts, withHeader("Content-Encoding", encoding))
		}
		return h.request(http.MethodPost, "/git/api.git/git-upload-pack", bytes.NewReader(body), opts...)
	}
	plainResp, plain := post([]byte(lsRefs), "")
	if plainResp.StatusCode != http.StatusOK || !strings.Contains(plain, "refs/heads/main") {
		t.Fatalf("ls-refs: %d %q", plainResp.StatusCode, plain)
	}
	if ct := plainResp.Header.Get("Content-Type"); ct != "application/x-git-upload-pack-result" {
		t.Errorf("Content-Type = %q", ct)
	}
	var zipped bytes.Buffer
	zw := gzip.NewWriter(&zipped)
	zw.Write([]byte(lsRefs))
	zw.Close()
	if resp, body := post(zipped.Bytes(), "gzip"); resp.StatusCode != http.StatusOK || body != plain {
		t.Errorf("gzip: %d %q, want 200 %q", resp.StatusCode, body, plain)
	}
	if resp, body := post([]byte(lsRefs), "gzip"); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("a body that says gzip and isn't: %d %q, want 400", resp.StatusCode, body)
	}
}

// A request body that never ends keeps git reading. Whether the client goes
// away or the server shuts down, git is killed and waited for, and the handler
// returns.
func TestClientGoingAwayEndsGit(t *testing.T) {
	for _, how := range []string{"the client goes away", "shutdown"} {
		t.Run(how, func(t *testing.T) {
			started := make(chan *os.Process, 1)
			waited := make(chan *os.ProcessState, 1)
			returned := make(chan struct{}, 1)
			h := newHarness(t, func(c *harnessConfig) {
				c.afterStart = func(p *os.Process) { started <- p }
				c.afterWait = func(ps *os.ProcessState) { waited <- ps }
				c.after = func(r *http.Request) {
					if strings.HasSuffix(r.URL.Path, "/git-receive-pack") {
						returned <- struct{}{}
					}
				}
			})
			tok := h.liveToken(h.account("root", core.RoleSystemAdmin))
			h.pushApp("api")

			body, feed := io.Pipe()
			t.Cleanup(func() { feed.Close() })
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.url+"/git/api.git/git-receive-pack", body)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/x-git-receive-pack-request")
			req.SetBasicAuth("x", tok)
			go func() {
				if resp, err := testClient.Do(req); err == nil {
					io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
				}
			}()
			go feed.Write([]byte("00")) // half a pkt-line: git waits for the rest

			var proc *os.Process
			select {
			case proc = <-started:
			case <-time.After(10 * time.Second):
				t.Fatal("git never started")
			}
			if how == "shutdown" {
				h.stopStreams()
			} else {
				cancel()
			}
			deadline := time.After(2 * time.Second)
			select {
			case <-returned:
			case <-deadline:
				t.Fatal("the handler didn't return within two seconds")
			}
			select {
			case ps := <-waited:
				if ps == nil {
					t.Error("git wasn't waited for")
				}
			default:
				t.Error("the handler returned before git was waited for")
			}
			if err := syscall.Kill(proc.Pid, 0); !errors.Is(err, syscall.ESRCH) {
				t.Errorf("git (pid %d) is still there: %v", proc.Pid, err)
			}
		})
	}
}
