package server

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/james-smart/outhaul/internal/blobstore"
	"github.com/james-smart/outhaul/internal/compose"
	"github.com/james-smart/outhaul/internal/core"
	"github.com/james-smart/outhaul/internal/docker"
	"github.com/james-smart/outhaul/internal/github"
	"github.com/james-smart/outhaul/internal/logstream"
	"github.com/james-smart/outhaul/internal/secret"
	"github.com/james-smart/outhaul/internal/store"
)

type fakeDeployer struct {
	notified  int
	cancelled []int64
}

func (f *fakeDeployer) Notify() { f.notified++ }
func (f *fakeDeployer) Cancel(_ context.Context, id int64) (bool, error) {
	f.cancelled = append(f.cancelled, id)
	return true, nil
}

// fakeDatabases records lifecycle calls from the handlers; it never touches a
// store or container, so tests assert on the calls (and set row state
// themselves when a page needs it).
type fakeDatabases struct {
	provisioned []core.Database
	started     []int64
	stopped     []int64
	removed     []int64
	failWith    error // returned by Start/Stop/Remove when set
}

func (f *fakeDatabases) Provision(d core.Database) { f.provisioned = append(f.provisioned, d) }
func (f *fakeDatabases) Start(_ context.Context, d core.Database) error {
	if f.failWith != nil {
		return f.failWith
	}
	f.started = append(f.started, d.ID)
	return nil
}
func (f *fakeDatabases) Stop(_ context.Context, d core.Database) error {
	if f.failWith != nil {
		return f.failWith
	}
	f.stopped = append(f.stopped, d.ID)
	return nil
}
func (f *fakeDatabases) Remove(_ context.Context, d core.Database) error {
	if f.failWith != nil {
		return f.failWith
	}
	f.removed = append(f.removed, d.ID)
	return nil
}

// fakeBackups records backup-manager calls from the handlers.
type fakeBackups struct {
	ran     []int64 // backup IDs passed to RunNow
	tested  []string
	testErr error // returned by TestDestination when set

	restored   []string           // "<backupID> <key>" per RestoreNow call
	objects    []blobstore.Object // returned by ListRestoreObjects
	listErr    error
	restoreDir string // returned by RestoreDir ("" falls back to "dir")
}

func (f *fakeBackups) RunNow(b core.Backup) { f.ran = append(f.ran, b.ID) }
func (f *fakeBackups) RestoreNow(b core.Backup, objectKey string) {
	f.restored = append(f.restored, fmt.Sprintf("%d %s", b.ID, objectKey))
}
func (f *fakeBackups) ListRestoreObjects(context.Context, core.Backup) ([]blobstore.Object, error) {
	return f.objects, f.listErr
}
func (f *fakeBackups) RestoreDir(context.Context, core.Backup) (string, error) {
	if f.restoreDir == "" {
		return "dir", nil
	}
	return f.restoreDir, nil
}
func (f *fakeBackups) TestDestination(_ context.Context, d core.Destination) error {
	f.tested = append(f.tested, d.Name)
	return f.testErr
}

type fakeRuntime struct {
	container *docker.Container
	stack     []docker.Container // returned by ListContainers (compose apps)
	started   []string
	stopped   []string
	removed   []string
	logs      map[string]string       // container ID -> content for ContainerLogs
	logTails  []int                   // tail values passed to ContainerLogs
	stats     map[string]docker.Stats // container ID -> sample for ContainerStats

	removedImages []string // refs passed to RemoveImage
}

func (f *fakeRuntime) FindContainer(_ context.Context, name string) (*docker.Container, error) {
	return f.container, nil
}
func (f *fakeRuntime) ListContainers(_ context.Context, _ map[string]string) ([]docker.Container, error) {
	return f.stack, nil
}
func (f *fakeRuntime) StartContainer(_ context.Context, id string) error {
	f.started = append(f.started, id)
	return nil
}
func (f *fakeRuntime) StopContainer(_ context.Context, id string, _ time.Duration) error {
	f.stopped = append(f.stopped, id)
	return nil
}
func (f *fakeRuntime) RemoveContainer(_ context.Context, id string, _ bool) error {
	f.removed = append(f.removed, id)
	return nil
}
func (f *fakeRuntime) ContainerLogs(_ context.Context, id string, tail int) (io.ReadCloser, error) {
	f.logTails = append(f.logTails, tail)
	content, ok := f.logs[id]
	if !ok {
		return nil, fmt.Errorf("no such container: %s", id)
	}
	return io.NopCloser(strings.NewReader(content)), nil
}

func (f *fakeRuntime) ContainerStats(_ context.Context, id string) (docker.Stats, error) {
	st, ok := f.stats[id]
	if !ok {
		return docker.Stats{}, fmt.Errorf("no such container: %s", id)
	}
	return st, nil
}

func (f *fakeRuntime) RemoveImage(_ context.Context, ref string) error {
	f.removedImages = append(f.removedImages, ref)
	return nil
}

type testEnv struct {
	srv       *Server
	deployer  *fakeDeployer
	runtime   *fakeRuntime
	compose   *compose.Fake
	databases *fakeDatabases
	backups   *fakeBackups
	broker    *logstream.Broker
	store     *store.Store
	gh        *github.Fake
	http      *httptest.Server
	client    *http.Client

	sessionToken string // set by login(); attach to requests via authed()
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	box, err := secret.Load(filepath.Join(t.TempDir(), "key"))
	if err != nil {
		t.Fatalf("secret.Load: %v", err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), box)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	dep := &fakeDeployer{}
	rt := &fakeRuntime{}
	cp := &compose.Fake{}
	dbm := &fakeDatabases{}
	bk := &fakeBackups{}
	br := logstream.New()
	gh := &github.Fake{}
	srv, err := New(st, dep, rt, cp, dbm, bk, br, gh, "https://slip.example.com", "203.0.113.7", false, "SETUPTOKEN")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	jar, _ := cookiejar.New(nil)
	client := &http.Client{
		Jar: jar,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse // don't follow; assert redirects ourselves
		},
	}
	return &testEnv{srv: srv, deployer: dep, runtime: rt, compose: cp, databases: dbm, backups: bk, broker: br, store: st, gh: gh, http: ts, client: client}
}

func (e *testEnv) get(t *testing.T, path string) *http.Response {
	t.Helper()
	resp, err := e.client.Get(e.http.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	return resp
}

func (e *testEnv) postForm(t *testing.T, path string, form url.Values) *http.Response {
	t.Helper()
	resp, err := e.client.PostForm(e.http.URL+path, form)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	return resp
}

func body(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(b)
}

// completeSetup runs the first-boot flow and leaves the client authenticated.
func (e *testEnv) completeSetup(t *testing.T) {
	t.Helper()
	resp := e.postForm(t, "/setup", url.Values{
		"token":    {"SETUPTOKEN"},
		"username": {"admin"},
		"password": {"supersecret"},
	})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("setup submit status = %d, want 303", resp.StatusCode)
	}
	resp.Body.Close()
}

// login completes first-boot setup (creating the admin and a session) and
// remembers the session token so authed() can attach it to requests built
// directly with httptest.NewRequest (bypassing e.http/e.client, which is
// needed by tests that also want the raw httptest.ResponseRecorder).
func (e *testEnv) login(t *testing.T) {
	t.Helper()
	e.completeSetup(t)
	u, err := url.Parse(e.http.URL)
	if err != nil {
		t.Fatalf("parse test server URL: %v", err)
	}
	for _, c := range e.client.Jar.Cookies(u) {
		if c.Name == sessionCookie {
			e.sessionToken = c.Value
			return
		}
	}
	t.Fatal("session cookie not found after login")
}

// authed attaches the session cookie obtained via login() to req.
func (e *testEnv) authed(req *http.Request) {
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: e.sessionToken})
}

func TestHealthz(t *testing.T) {
	e := newTestEnv(t)
	resp := e.get(t, "/healthz")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := body(t, resp); got != "ok" {
		t.Errorf("body = %q, want ok", got)
	}
}

func TestUnauthenticatedRedirectsToLogin(t *testing.T) {
	e := newTestEnv(t)
	resp := e.get(t, "/")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/login" {
		t.Errorf("Location = %q, want /login", loc)
	}
}

func TestLoginRedirectsToSetupWhenNoUser(t *testing.T) {
	e := newTestEnv(t)
	resp := e.get(t, "/login")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/setup" {
		t.Fatalf("got %d -> %q, want 303 -> /setup", resp.StatusCode, resp.Header.Get("Location"))
	}
}

func TestSetupRejectsBadToken(t *testing.T) {
	e := newTestEnv(t)
	resp := e.get(t, "/setup?token=WRONG")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	if !strings.Contains(body(t, resp), "Invalid or missing setup token") {
		t.Error("expected invalid-token message")
	}
}

func TestSetupFormWithGoodToken(t *testing.T) {
	e := newTestEnv(t)
	resp := e.get(t, "/setup?token=SETUPTOKEN")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(body(t, resp), "Create the admin account") {
		t.Error("expected setup form")
	}
}

func TestFullFlowSetupCreateAppDeploy(t *testing.T) {
	e := newTestEnv(t)
	e.completeSetup(t)

	// Authenticated home page renders.
	resp := e.get(t, "/")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("home status = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(body(t, resp), "Apps") {
		t.Error("home should render the apps page")
	}

	// Create an app.
	resp = e.postForm(t, "/apps", url.Values{
		"name":     {"web"},
		"repo_url": {"https://example.com/web.git"},
		"domain":   {"web.example.com"},
	})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("create app status = %d, want 303; body=%s", resp.StatusCode, body(t, resp))
	}
	resp.Body.Close()

	app, err := e.store.GetAppByName(context.Background(), "web")
	if err != nil {
		t.Fatalf("app not persisted: %v", err)
	}

	// Trigger a deploy.
	resp = e.postForm(t, "/apps/"+itoa(app.ID)+"/deploy", url.Values{})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("deploy status = %d, want 303", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	resp.Body.Close()
	if !strings.HasPrefix(loc, "/deployments/") {
		t.Fatalf("deploy redirect = %q, want /deployments/...", loc)
	}
	if e.deployer.notified == 0 {
		t.Error("worker was not notified after deploy")
	}

	// Deployment detail page renders.
	resp = e.get(t, loc)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("deployment page status = %d", resp.StatusCode)
	}
	if !strings.Contains(body(t, resp), "Build &amp; deploy log") {
		t.Error("deployment page missing log panel")
	}
}

func TestCreateAppRejectsInvalidName(t *testing.T) {
	e := newTestEnv(t)
	e.completeSetup(t)

	resp := e.postForm(t, "/apps", url.Values{
		"name":     {"Web App!"},
		"repo_url": {"https://example.com/web.git"},
		"domain":   {"web.example.com"},
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if !strings.Contains(body(t, resp), "lowercase") {
		t.Error("expected a name-validation message")
	}
}

func TestSSEReplaysHistoryAndCloses(t *testing.T) {
	e := newTestEnv(t)
	e.completeSetup(t)

	app, _ := e.store.CreateApp(context.Background(), core.App{Name: "web", RepoURL: "https://x/y.git", Domain: "web.test"})
	dep, _ := e.store.CreateDeployment(context.Background(), app.ID)

	e.broker.Publish(dep.ID, "cloning...")
	e.broker.Publish(dep.ID, "building...")
	e.broker.Close(dep.ID) // terminal: SSE should replay then send done

	resp := e.get(t, "/deployments/"+itoa(dep.ID)+"/logs")
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
	got := body(t, resp)
	for _, want := range []string{"data: cloning...", "data: building...", "event: done"} {
		if !strings.Contains(got, want) {
			t.Errorf("SSE body missing %q; got:\n%s", want, got)
		}
	}
}

func TestCancelDelegatesToWorker(t *testing.T) {
	e := newTestEnv(t)
	e.completeSetup(t)

	app, _ := e.store.CreateApp(context.Background(), core.App{Name: "web", RepoURL: "https://x/y.git", Domain: "web.test"})
	dep, _ := e.store.CreateDeployment(context.Background(), app.ID)

	resp := e.postForm(t, "/deployments/"+itoa(dep.ID)+"/cancel", url.Values{})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("cancel status = %d, want 303", resp.StatusCode)
	}
	resp.Body.Close()
	if len(e.deployer.cancelled) != 1 || e.deployer.cancelled[0] != dep.ID {
		t.Errorf("worker.Cancel not called with %d: %v", dep.ID, e.deployer.cancelled)
	}
}

// itoa is a tiny local helper for building request paths.
func itoa(id int64) string { return strconv.FormatInt(id, 10) }

// appForm builds a minimal valid create-app form submission.
func appForm(name, domain string) url.Values {
	return url.Values{"name": {name}, "domain": {domain}, "source": {"public"}, "repo_url": {"https://github.com/o/" + name + ".git"}, "branch": {"main"}}
}

func TestAppDetailShowsConnectAndStats(t *testing.T) {
	e := newTestEnv(t)
	e.login(t)
	e.postForm(t, "/apps", appForm("web", "web.example.com")).Body.Close()
	app, _ := e.store.GetAppByName(context.Background(), "web")

	page := body(t, e.get(t, "/apps/"+itoa(app.ID)))
	if !strings.Contains(page, `id="metric-cpu"`) {
		t.Error("app detail should show the live metric stats")
	}
	if !strings.Contains(page, "/webhooks/app/"+app.WebhookSecret) {
		t.Error("app detail should show the connect-repo webhook URL")
	}
}

func TestAppsListShowsBranch(t *testing.T) {
	e := newTestEnv(t)
	e.login(t)
	e.postForm(t, "/apps", appForm("web", "web.example.com")).Body.Close()
	app, err := e.store.GetAppByName(context.Background(), "web")
	if err != nil {
		t.Fatal(err)
	}
	// Set a distinctive branch that does NOT appear anywhere in the create form
	// (whose branch input defaults to "main"), so the assertion below only
	// passes if the apps table actually renders the app's branch.
	e.postForm(t, "/apps/"+itoa(app.ID)+"/settings", url.Values{"branch": {"release-9x"}}).Body.Close()

	page := body(t, e.get(t, "/apps"))
	if !strings.Contains(page, "release-9x") {
		t.Error("apps list should render each app's branch in the table")
	}
}

func TestEnvAddListAndMask(t *testing.T) {
	e := newTestEnv(t)
	e.completeSetup(t)
	app, _ := e.store.CreateApp(context.Background(), core.App{Name: "web", RepoURL: "https://x/y.git", Domain: "web.test"})

	resp := e.postForm(t, "/apps/"+itoa(app.ID)+"/env", url.Values{
		"key": {"LOG_LEVEL"}, "value": {"info"},
	})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("add env status = %d, want 303; body=%s", resp.StatusCode, body(t, resp))
	}
	resp.Body.Close()
	resp = e.postForm(t, "/apps/"+itoa(app.ID)+"/env", url.Values{
		"key": {"API_KEY"}, "value": {"s3cr3t"}, "secret": {"on"},
	})
	resp.Body.Close()

	page := body(t, e.get(t, "/apps/"+itoa(app.ID)))
	if !strings.Contains(page, "LOG_LEVEL") || !strings.Contains(page, "info") {
		t.Error("normal env var not shown")
	}
	if !strings.Contains(page, "API_KEY") {
		t.Error("secret key name should be shown")
	}
	if strings.Contains(page, "s3cr3t") {
		t.Error("secret VALUE leaked into the page")
	}
}

func TestEnvRejectsBadKey(t *testing.T) {
	e := newTestEnv(t)
	e.completeSetup(t)
	app, _ := e.store.CreateApp(context.Background(), core.App{Name: "web", RepoURL: "https://x/y.git", Domain: "web.test"})

	resp := e.postForm(t, "/apps/"+itoa(app.ID)+"/env", url.Values{"key": {"bad-key"}, "value": {"x"}})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()

	resp = e.postForm(t, "/apps/"+itoa(app.ID)+"/env", url.Values{"key": {"PORT"}, "value": {"9"}})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("PORT should be rejected, status = %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestEnvDelete(t *testing.T) {
	e := newTestEnv(t)
	e.completeSetup(t)
	app, _ := e.store.CreateApp(context.Background(), core.App{Name: "web", RepoURL: "https://x/y.git", Domain: "web.test"})
	e.store.SetEnv(context.Background(), app.ID, "K", "v", false)

	resp := e.postForm(t, "/apps/"+itoa(app.ID)+"/env/delete", url.Values{"key": {"K"}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("delete status = %d, want 303", resp.StatusCode)
	}
	resp.Body.Close()
	vars, _ := e.store.ListEnv(context.Background(), app.ID)
	if len(vars) != 0 {
		t.Errorf("env not deleted: %v", vars)
	}
}

func TestEnvDeleteMissingApp404(t *testing.T) {
	e := newTestEnv(t)
	e.completeSetup(t)
	resp := e.postForm(t, "/apps/9999/env/delete", url.Values{"key": {"K"}})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestStopApp(t *testing.T) {
	e := newTestEnv(t)
	e.completeSetup(t)
	app, _ := e.store.CreateApp(context.Background(), core.App{Name: "web", RepoURL: "https://x/y.git", Domain: "web.test"})
	e.runtime.container = &docker.Container{ID: "ctr1", Name: "outhaul-app-web", State: "running"}

	resp := e.postForm(t, "/apps/"+itoa(app.ID)+"/stop", url.Values{})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("stop status = %d, want 303", resp.StatusCode)
	}
	resp.Body.Close()
	if len(e.runtime.stopped) != 1 || e.runtime.stopped[0] != "ctr1" {
		t.Errorf("StopContainer not called: %v", e.runtime.stopped)
	}
}

func TestRestartApp(t *testing.T) {
	e := newTestEnv(t)
	e.completeSetup(t)
	app, _ := e.store.CreateApp(context.Background(), core.App{Name: "web", RepoURL: "https://x/y.git", Domain: "web.test"})
	e.runtime.container = &docker.Container{ID: "ctr1", Name: "outhaul-app-web", State: "running"}

	resp := e.postForm(t, "/apps/"+itoa(app.ID)+"/restart", url.Values{})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("restart status = %d, want 303", resp.StatusCode)
	}
	resp.Body.Close()
	if len(e.runtime.stopped) != 1 || len(e.runtime.started) != 1 {
		t.Errorf("restart should stop then start: stopped=%v started=%v", e.runtime.stopped, e.runtime.started)
	}
}

func TestStopAppNoContainer409(t *testing.T) {
	e := newTestEnv(t)
	e.completeSetup(t)
	app, _ := e.store.CreateApp(context.Background(), core.App{Name: "web", RepoURL: "https://x/y.git", Domain: "web.test"})
	// e.runtime.container stays nil → no container
	resp := e.postForm(t, "/apps/"+itoa(app.ID)+"/stop", url.Values{})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("stop with no container = %d, want 409", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestDeleteApp(t *testing.T) {
	e := newTestEnv(t)
	e.completeSetup(t)
	app, _ := e.store.CreateApp(context.Background(), core.App{Name: "web", RepoURL: "https://x/y.git", Domain: "web.test"})
	e.runtime.container = &docker.Container{ID: "ctr1", Name: "outhaul-app-web", State: "running"}

	resp := e.postForm(t, "/apps/"+itoa(app.ID)+"/delete", url.Values{})
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/apps" {
		t.Fatalf("delete redirect = %d -> %q, want 303 -> /apps", resp.StatusCode, resp.Header.Get("Location"))
	}
	resp.Body.Close()
	if len(e.runtime.removed) != 1 {
		t.Errorf("container not removed: %v", e.runtime.removed)
	}
	if _, err := e.store.GetApp(context.Background(), app.ID); err == nil {
		t.Error("app row not deleted")
	}
}

func TestDeleteAppRemovesItsImages(t *testing.T) {
	e := newTestEnv(t)
	e.completeSetup(t)
	app, _ := e.store.CreateApp(context.Background(), core.App{Name: "web", RepoURL: "https://x/y.git", Domain: "web.test"})
	seedFinishedDeploy(t, e, app.ID, "outhaul/web:1")
	src := seedFinishedDeploy(t, e, app.ID, "outhaul/web:2")
	// A rollback row repeats a tag; delete must not remove it twice.
	if _, err := e.store.CreateRollback(context.Background(), app.ID, src.Image, src.ID); err != nil {
		t.Fatal(err)
	}

	resp := e.postForm(t, "/apps/"+itoa(app.ID)+"/delete", url.Values{})
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("delete = %d, want 303", resp.StatusCode)
	}

	// Deployments come back newest first; each distinct tag exactly once.
	want := []string{"outhaul/web:2", "outhaul/web:1"}
	if !reflect.DeepEqual(e.runtime.removedImages, want) {
		t.Errorf("removed images = %v, want %v", e.runtime.removedImages, want)
	}
}

func TestDeleteAppWhenContainerGone(t *testing.T) {
	e := newTestEnv(t)
	e.completeSetup(t)
	app, _ := e.store.CreateApp(context.Background(), core.App{Name: "web", RepoURL: "https://x/y.git", Domain: "web.test"})
	// container is nil (already gone); delete must still remove the row and redirect to /
	resp := e.postForm(t, "/apps/"+itoa(app.ID)+"/delete", url.Values{})
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/apps" {
		t.Fatalf("delete = %d -> %q, want 303 -> /apps", resp.StatusCode, resp.Header.Get("Location"))
	}
	resp.Body.Close()
	if len(e.runtime.removed) != 0 {
		t.Errorf("no container should be removed when none exists: %v", e.runtime.removed)
	}
	if _, err := e.store.GetApp(context.Background(), app.ID); err == nil {
		t.Error("app row not deleted")
	}
}

func TestAppSettingsHasHints(t *testing.T) {
	e := newTestEnv(t)
	e.login(t)
	e.postForm(t, "/apps", appForm("web", "web.example.com")).Body.Close()
	web, _ := e.store.GetAppByName(context.Background(), "web")

	page := body(t, e.get(t, "/apps/"+itoa(web.ID)))
	if !strings.Contains(page, `class="hint"`) {
		t.Error("app page should render (i) field hints")
	}
}
