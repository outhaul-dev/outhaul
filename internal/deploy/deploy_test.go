package deploy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/outhaul-dev/outhaul/internal/builder"
	"github.com/outhaul-dev/outhaul/internal/compose"
	"github.com/outhaul-dev/outhaul/internal/config"
	"github.com/outhaul-dev/outhaul/internal/core"
	"github.com/outhaul-dev/outhaul/internal/docker"
	"github.com/outhaul-dev/outhaul/internal/github"
	"github.com/outhaul-dev/outhaul/internal/gitsource"
	"github.com/outhaul-dev/outhaul/internal/logstream"
	"github.com/outhaul-dev/outhaul/internal/secret"
	"github.com/outhaul-dev/outhaul/internal/store"
	"github.com/outhaul-dev/outhaul/internal/traefik"
)

// --- test doubles ---

type fakeBuilder struct {
	err     error
	lastReq builder.BuildRequest

	// Blocking seams for the build-cap tests: started closes on the first
	// Build; release, when non-nil, holds every Build until it closes.
	started chan struct{}
	release chan struct{}
	once    sync.Once
	mu      sync.Mutex
	builds  int
}

func (f *fakeBuilder) Name() string { return "fake" }
func (f *fakeBuilder) Build(ctx context.Context, req builder.BuildRequest, out io.Writer) error {
	f.mu.Lock()
	f.lastReq = req
	f.builds++
	f.mu.Unlock()
	if f.started != nil {
		f.once.Do(func() { close(f.started) })
	}
	if f.release != nil {
		select {
		case <-f.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if f.err != nil {
		return f.err
	}
	fmt.Fprintf(out, "built %s\n", req.ImageTag)
	return nil
}

type fakeCloner struct {
	err    error
	cloned []string
	files  map[string]string // repo files to materialize in the clone dir
	links  map[string]string // repo symlinks to materialize: name -> target

	tags     []string // what ListTags returns
	tagsErr  error
	listed   int      // ListTags call count
	branches []string // spec.Branch of every Clone
}

func (f *fakeCloner) ListTags(_ context.Context, _ CloneSpec) ([]string, error) {
	f.listed++
	if f.tagsErr != nil {
		return nil, f.tagsErr
	}
	return f.tags, nil
}

func (f *fakeCloner) Clone(_ context.Context, spec CloneSpec, dir string, out io.Writer) error {
	f.branches = append(f.branches, spec.Branch)
	if f.err != nil {
		return f.err
	}
	f.cloned = append(f.cloned, spec.URL)
	for name, content := range f.files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			return err
		}
	}
	for name, target := range f.links {
		if err := os.Symlink(target, filepath.Join(dir, filepath.FromSlash(name))); err != nil {
			return err
		}
	}
	fmt.Fprintf(out, "cloned %s into %s\n", spec.URL, dir)
	return nil
}

// --- harness ---

type harness struct {
	store      *store.Store
	docker     *docker.Fake
	builder    *fakeBuilder // the nixpacks-slot builder
	dockerfile *fakeBuilder // the dockerfile-slot builder
	compose    *compose.Fake
	cloner     *fakeCloner
	broker     *logstream.Broker
	worker     *Worker
}

func newHarness(t *testing.T) *harness { return newHarnessWith(t, nil) }

// newHarnessWith is newHarness with a chance to change the worker's config
// before it is built (the build cap is read once, in NewWorker).
func newHarnessWith(t *testing.T, tweak func(*config.Config)) *harness {
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

	h := &harness{
		store:      st,
		docker:     docker.NewFake(),
		builder:    &fakeBuilder{},
		dockerfile: &fakeBuilder{},
		compose:    &compose.Fake{},
		cloner:     &fakeCloner{},
		broker:     logstream.New(),
	}
	cfg := config.Config{DataDir: t.TempDir(), Network: "outhaul"}
	if tweak != nil {
		tweak(&cfg)
	}
	h.worker = NewWorker(st, h.docker, Builders{Nixpacks: h.builder, Dockerfile: h.dockerfile},
		h.compose, h.cloner, h.broker,
		gitsource.NewRegistry(gitsource.NewGithubApp(&github.Fake{Token: "ghs_test"})), cfg)
	h.worker.healthCheck = func(context.Context, string, time.Duration) bool { return true }
	return h
}

func (h *harness) app(t *testing.T, name string) core.App {
	t.Helper()
	app, err := h.store.CreateApp(context.Background(), core.App{
		Name: name, RepoURL: "https://example.com/" + name + ".git", Domain: name + ".test",
	})
	if err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	return app
}

// claimedDeployment creates a deployment and moves it to building, as the
// dispatcher would before invoking the pipeline.
func (h *harness) claimedDeployment(t *testing.T, appID int64) core.Deployment {
	t.Helper()
	d, err := h.store.CreateDeployment(context.Background(), appID)
	if err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}
	ok, err := h.store.ClaimDeployment(context.Background(), d.ID)
	if err != nil || !ok {
		t.Fatalf("ClaimDeployment: ok=%v err=%v", ok, err)
	}
	d.Status = core.StatusBuilding
	return d
}

func (h *harness) status(t *testing.T, id int64) core.Deployment {
	t.Helper()
	d, err := h.store.GetDeployment(context.Background(), id)
	if err != nil {
		t.Fatalf("GetDeployment: %v", err)
	}
	return d
}

// --- pipeline tests (deterministic: call runPipeline directly) ---

func TestPipelineHappyPath(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	app := h.app(t, "web")
	dep := h.claimedDeployment(t, app.ID)

	h.worker.runPipeline(ctx, dep)

	got := h.status(t, dep.ID)
	if got.Status != core.StatusRunning {
		t.Fatalf("status = %q (reason %q), want running", got.Status, got.Reason)
	}
	if got.Image == "" {
		t.Error("expected built image to be recorded")
	}
	c, _ := h.docker.FindContainer(ctx, "outhaul-app-web")
	if c == nil || !c.Running() {
		t.Fatalf("app container not running: %+v", c)
	}
	if c.Labels["traefik.http.routers.outhaul-web-d1.rule"] != (traefik.Routing{}).Rule("web.test", "") {
		t.Errorf("missing/incorrect traefik rule label: %v", c.Labels)
	}
	spec := lastCreatedNamed(t, h, "outhaul-app-web")
	if !contains(spec.Networks, "outhaul") {
		t.Errorf("container not on outhaul network: %v", spec.Networks)
	}
	if !containsPrefix(spec.Env, "PORT=") {
		t.Errorf("container missing PORT env: %v", spec.Env)
	}
}

func TestPipelineSupersedesPriorRunningDeploy(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	app := h.app(t, "web")

	// First deploy ships and holds traffic.
	first := h.claimedDeployment(t, app.ID)
	h.worker.runPipeline(ctx, first)
	if got := h.status(t, first.ID); got.Status != core.StatusRunning {
		t.Fatalf("first status = %q, want running", got.Status)
	}

	// Second deploy takes over; the first must be retired to superseded so it no
	// longer reads as "running" across the UI.
	second := h.claimedDeployment(t, app.ID)
	h.worker.runPipeline(ctx, second)

	if got := h.status(t, first.ID); got.Status != core.StatusSuperseded {
		t.Errorf("first status = %q, want superseded", got.Status)
	}
	if got := h.status(t, second.ID); got.Status != core.StatusRunning {
		t.Errorf("second status = %q, want running", got.Status)
	}
}

func TestPipelineCloneFailureMarksFailed(t *testing.T) {
	h := newHarness(t)
	h.cloner.err = errors.New("host unreachable")
	app := h.app(t, "web")
	dep := h.claimedDeployment(t, app.ID)

	h.worker.runPipeline(context.Background(), dep)

	got := h.status(t, dep.ID)
	if got.Status != core.StatusFailed {
		t.Fatalf("status = %q, want failed", got.Status)
	}
	if !strings.Contains(got.Reason, "clone") {
		t.Errorf("reason = %q, want it to mention clone", got.Reason)
	}
	if len(h.docker.Created) != 0 {
		t.Error("no container should be created when clone fails")
	}
}

// A public app's repository that asks for credentials is private or missing.
// The failure must say so, and which sources can clone it, instead of git's
// "could not read Username".
func TestPipelineCloneNeedingCredentialsExplains(t *testing.T) {
	h := newHarness(t)
	h.cloner.err = fmt.Errorf("git clone failed: %w", ErrCredentialsRequired)
	app := h.app(t, "web")
	dep := h.claimedDeployment(t, app.ID)

	h.worker.runPipeline(context.Background(), dep)

	got := h.status(t, dep.ID)
	if got.Status != core.StatusFailed {
		t.Fatalf("status = %q, want failed", got.Status)
	}
	for _, want := range []string{"private or does not exist", "GitHub App", "SSH deploy key"} {
		if !strings.Contains(got.Reason, want) {
			t.Errorf("reason = %q, want it to mention %q", got.Reason, want)
		}
	}
}

// The Public Git URL hint is for public apps only: an SSH app given an https
// URL also clones without credentials, and gets the plain reason.
func TestPipelineCloneNeedingCredentialsHintIsForPublicApps(t *testing.T) {
	h := newHarness(t)
	h.cloner.err = fmt.Errorf("git clone failed: %w", ErrCredentialsRequired)
	app, err := h.store.CreateApp(context.Background(), core.App{
		Name: "web", RepoURL: "https://example.com/web.git", Domain: "web.test",
		Source: core.SourceSSH, SSHPrivateKey: "PRIVATE-KEY", SSHPublicKey: "ssh-ed25519 AAAA",
	})
	if err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	dep := h.claimedDeployment(t, app.ID)

	h.worker.runPipeline(context.Background(), dep)

	got := h.status(t, dep.ID)
	if got.Status != core.StatusFailed {
		t.Fatalf("status = %q, want failed", got.Status)
	}
	if !strings.Contains(got.Reason, "asked for credentials") || strings.Contains(got.Reason, "Public Git URL") {
		t.Errorf("reason = %q, want the plain credentials reason", got.Reason)
	}
}

func TestPipelineBuildFailureMarksFailed(t *testing.T) {
	h := newHarness(t)
	h.builder.err = errors.New("nixpacks exploded")
	app := h.app(t, "web")
	dep := h.claimedDeployment(t, app.ID)

	h.worker.runPipeline(context.Background(), dep)

	got := h.status(t, dep.ID)
	if got.Status != core.StatusFailed {
		t.Fatalf("status = %q, want failed", got.Status)
	}
	if !strings.Contains(got.Reason, "build") {
		t.Errorf("reason = %q, want it to mention build", got.Reason)
	}
}

func TestPipelineUnhealthyKeepsOldContainerAndFails(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	app := h.app(t, "web")

	oldID, _ := h.docker.CreateContainer(ctx, docker.ContainerSpec{Name: "outhaul-app-web", Image: "old:1"})
	h.docker.StartContainer(ctx, oldID)

	h.worker.healthCheck = func(context.Context, string, time.Duration) bool { return false }

	dep := h.claimedDeployment(t, app.ID)
	h.worker.runPipeline(ctx, dep)

	if got := h.status(t, dep.ID); got.Status != core.StatusFailed {
		t.Fatalf("status = %q, want failed", got.Status)
	}
	if c, _ := h.docker.FindContainer(ctx, "outhaul-app-web"); c == nil || c.Image != "old:1" {
		t.Errorf("old container should survive an unhealthy deploy, got %+v", c)
	}
}

func TestPipelineHealthyCutsOver(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	app := h.app(t, "web")
	oldID, _ := h.docker.CreateContainer(ctx, docker.ContainerSpec{Name: "outhaul-app-web", Image: "old:1"})
	h.docker.StartContainer(ctx, oldID)

	dep := h.claimedDeployment(t, app.ID) // healthy by harness default

	h.worker.runPipeline(ctx, dep)

	if got := h.status(t, dep.ID); got.Status != core.StatusRunning {
		t.Fatalf("status = %q (%q), want running", got.Status, got.Reason)
	}
	if _, ok := h.docker.Containers[oldID]; ok {
		t.Error("old container not removed after healthy cutover")
	}
	c, _ := h.docker.FindContainer(ctx, "outhaul-app-web")
	if c == nil || !c.Running() {
		t.Fatalf("canonical container not running: %+v", c)
	}
	if c.Labels["traefik.enable"] != "true" {
		t.Errorf("canonical container should have traefik enabled: %v", c.Labels)
	}
	if tmp, _ := h.docker.FindContainer(ctx, "outhaul-deploy-"+itoa64(dep.ID)); tmp != nil {
		t.Error("temp container was not cleaned up")
	}
}

func itoa64(n int64) string { return fmt.Sprintf("%d", n) }

func TestPipelineTempRemovedWhenOldRemovalFails(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	app := h.app(t, "web")
	oldID, _ := h.docker.CreateContainer(ctx, docker.ContainerSpec{Name: "outhaul-app-web", Image: "old:1"})
	h.docker.StartContainer(ctx, oldID)
	// Fail removal of the OLD container only.
	h.docker.FailRemove = func(id string) error {
		if id == oldID {
			return errors.New("daemon busy")
		}
		return nil
	}
	dep := h.claimedDeployment(t, app.ID)
	h.worker.runPipeline(ctx, dep)

	if got := h.status(t, dep.ID); got.Status != core.StatusFailed {
		t.Fatalf("status = %q, want failed", got.Status)
	}
	if tmp, _ := h.docker.FindContainer(ctx, "outhaul-deploy-"+itoa64(dep.ID)); tmp != nil {
		t.Error("temp container leaked when old removal failed")
	}
}

func TestPipelineCutoverFailureReportsAppDown(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	app := h.app(t, "web")
	// Fail creation of the CANONICAL container (the second create, enabled labels).
	h.docker.FailCreate = func(spec docker.ContainerSpec) error {
		if spec.Name == "outhaul-app-web" && spec.Labels["traefik.enable"] == "true" {
			return errors.New("daemon hiccup")
		}
		return nil
	}
	dep := h.claimedDeployment(t, app.ID)
	h.worker.runPipeline(ctx, dep)

	got := h.status(t, dep.ID)
	if got.Status != core.StatusFailed {
		t.Fatalf("status = %q, want failed", got.Status)
	}
	if !strings.Contains(got.Reason, "cutover") {
		t.Errorf("reason = %q, want it to mention cutover (app down)", got.Reason)
	}
	if tmp, _ := h.docker.FindContainer(ctx, "outhaul-deploy-"+itoa64(dep.ID)); tmp != nil {
		t.Error("temp container leaked after cutover-create failure")
	}
}

func TestPipelineAbortsIfAppDeletedDuringDeploy(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	app := h.app(t, "web")
	// Simulate the app being deleted during the health poll.
	h.worker.healthCheck = func(context.Context, string, time.Duration) bool {
		_ = h.store.DeleteApp(ctx, app.ID)
		return true
	}
	dep := h.claimedDeployment(t, app.ID)
	h.worker.runPipeline(ctx, dep)

	if c, _ := h.docker.FindContainer(ctx, "outhaul-app-web"); c != nil {
		t.Error("canonical container was created for a deleted app (orphan)")
	}
	if tmp, _ := h.docker.FindContainer(ctx, "outhaul-deploy-"+itoa64(dep.ID)); tmp != nil {
		t.Error("temp container leaked after app-deleted abort")
	}
}

func TestPipelineClosesLogStream(t *testing.T) {
	h := newHarness(t)
	app := h.app(t, "web")
	dep := h.claimedDeployment(t, app.ID)

	history, ch, cancel := h.broker.Subscribe(dep.ID)
	defer cancel()
	_ = history

	h.worker.runPipeline(context.Background(), dep)

	// Drain; the channel must eventually close (pipeline calls broker.Close).
	deadline := time.After(2 * time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return // closed as expected
			}
		case <-deadline:
			t.Fatal("log stream was not closed after pipeline finished")
		}
	}
}

// --- cancellation ---

func TestCancelQueuedDeployment(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	app := h.app(t, "web")
	d, _ := h.store.CreateDeployment(ctx, app.ID)

	ok, err := h.worker.Cancel(ctx, d.ID)
	if err != nil || !ok {
		t.Fatalf("Cancel: ok=%v err=%v", ok, err)
	}
	if got := h.status(t, d.ID); got.Status != core.StatusCancelled {
		t.Errorf("status = %q, want cancelled", got.Status)
	}
}

func TestCancelBuildingDeployment(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	app := h.app(t, "web")
	dep := h.claimedDeployment(t, app.ID) // building

	ok, err := h.worker.Cancel(ctx, dep.ID)
	if err != nil || !ok {
		t.Fatalf("Cancel: ok=%v err=%v", ok, err)
	}
	if got := h.status(t, dep.ID); got.Status != core.StatusCancelled {
		t.Errorf("status = %q, want cancelled", got.Status)
	}
}

func TestCancelRunningDeploymentRejected(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	app := h.app(t, "web")
	dep := h.claimedDeployment(t, app.ID)
	h.worker.runPipeline(ctx, dep) // -> running

	ok, err := h.worker.Cancel(ctx, dep.ID)
	if err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if ok {
		t.Error("cancelling a running deployment should be rejected")
	}
}

// --- dispatcher loop ---

func TestDispatcherProcessesQueuedDeployment(t *testing.T) {
	h := newHarness(t)
	ctx, stop := context.WithCancel(context.Background())
	defer stop()

	done := make(chan struct{})
	go func() { h.worker.Run(ctx); close(done) }()

	app := h.app(t, "web")
	d, _ := h.store.CreateDeployment(context.Background(), app.ID)
	h.worker.Notify()

	waitForStatus(t, h, d.ID, core.StatusRunning, 3*time.Second)

	stop()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not shut down")
	}
}

func TestDispatcherRunsDifferentAppsConcurrently(t *testing.T) {
	h := newHarness(t)
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	go h.worker.Run(ctx)

	web := h.app(t, "web")
	api := h.app(t, "api")
	dw, _ := h.store.CreateDeployment(context.Background(), web.ID)
	da, _ := h.store.CreateDeployment(context.Background(), api.ID)
	h.worker.Notify()

	waitForStatus(t, h, dw.ID, core.StatusRunning, 3*time.Second)
	waitForStatus(t, h, da.ID, core.StatusRunning, 3*time.Second)
}

func waitForStatus(t *testing.T, h *harness, id int64, want core.DeployStatus, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if h.status(t, id).Status == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	got := h.status(t, id)
	t.Fatalf("deployment %d status = %q (reason %q), want %q within %s", id, got.Status, got.Reason, want, timeout)
}

// --- git args ---

func TestCloneArgs(t *testing.T) {
	got := cloneArgs(CloneSpec{URL: "https://example.com/r.git"}, "/work/dep-1")
	want := []string{"clone", "--depth", "1", "--single-branch", "--", "https://example.com/r.git", "/work/dep-1"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("cloneArgs() = %v, want %v", got, want)
	}
}

func TestGitCloneMissingBinary(t *testing.T) {
	g := &Git{Bin: "git-does-not-exist-7a1b"}
	err := g.Clone(context.Background(), CloneSpec{URL: "https://example.com/r.git"}, t.TempDir(), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "git") {
		t.Fatalf("expected a git-not-found error, got %v", err)
	}
}

func TestPipelineInjectsEnvSecretsRuntimeOnly(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	app := h.app(t, "web")
	if err := h.store.SetEnv(ctx, app.ID, "LOG_LEVEL", "debug", false); err != nil {
		t.Fatalf("SetEnv: %v", err)
	}
	if err := h.store.SetEnv(ctx, app.ID, "API_KEY", "s3cr3t", true); err != nil {
		t.Fatalf("SetEnv: %v", err)
	}
	dep := h.claimedDeployment(t, app.ID)

	h.worker.runPipeline(ctx, dep)

	// Build env: non-secret present, secret absent, PORT present.
	be := h.builder.lastReq.Env
	if be["LOG_LEVEL"] != "debug" {
		t.Errorf("build env missing LOG_LEVEL: %v", be)
	}
	if _, ok := be["API_KEY"]; ok {
		t.Error("secret leaked into build env")
	}
	if be["PORT"] == "" {
		t.Error("build env missing PORT")
	}

	// Runtime env (on the canonical container spec): all three present.
	spec := lastCreatedNamed(t, h, "outhaul-app-web")
	if !contains(spec.Env, "LOG_LEVEL=debug") {
		t.Errorf("runtime env missing LOG_LEVEL: %v", spec.Env)
	}
	if !contains(spec.Env, "API_KEY=s3cr3t") {
		t.Errorf("runtime env missing secret API_KEY: %v", spec.Env)
	}
	if !containsPrefix(spec.Env, "PORT=") {
		t.Errorf("runtime env missing PORT: %v", spec.Env)
	}
}

func TestPipelineIgnoresUserPort(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	app := h.app(t, "web")
	if err := h.store.SetEnv(ctx, app.ID, "PORT", "3000", false); err != nil {
		t.Fatalf("SetEnv: %v", err)
	}
	dep := h.claimedDeployment(t, app.ID)
	h.worker.runPipeline(ctx, dep)

	spec := lastCreatedNamed(t, h, "outhaul-app-web")
	ports := 0
	for _, e := range spec.Env {
		if e == "PORT=3000" {
			t.Errorf("user PORT leaked into runtime env: %v", spec.Env)
		}
		if len(e) >= 5 && e[:5] == "PORT=" {
			ports++
		}
	}
	if ports != 1 {
		t.Errorf("expected exactly one PORT entry, got %d: %v", ports, spec.Env)
	}
	if h.builder.lastReq.Env["PORT"] != "8080" {
		t.Errorf("build PORT = %q, want 8080", h.builder.lastReq.Env["PORT"])
	}
}

// lastCreatedNamed returns the most recent Created spec with the given name.
func lastCreatedNamed(t *testing.T, h *harness, name string) docker.ContainerSpec {
	t.Helper()
	for i := len(h.docker.Created) - 1; i >= 0; i-- {
		if h.docker.Created[i].Name == name {
			return h.docker.Created[i]
		}
	}
	t.Fatalf("no created container named %q; created=%v", name, h.docker.Created)
	return docker.ContainerSpec{}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

func containsPrefix(ss []string, prefix string) bool {
	for _, s := range ss {
		if strings.HasPrefix(s, prefix) {
			return true
		}
	}
	return false
}

// TestNixpacksAppRoutesEveryDomainRow: a single-container app with more than
// one domain row gets one router per row on its canonical container. The
// harness has no ACME email (TLSEnabled() is false), so each row contributes
// exactly one ".rule" label; if TLS were enabled a row would also gain a
// "-tls" router sharing the same rule, which is why this counts distinct
// domain-row router prefixes rather than raw ".rule" keys.
func TestNixpacksAppRoutesEveryDomainRow(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	app := h.app(t, "web") // seeds web.test as domain row 1
	if _, err := h.store.AddDomain(ctx, core.Domain{AppID: app.ID, Host: "alias.test", Port: 8080, TLS: true}); err != nil {
		t.Fatalf("AddDomain: %v", err)
	}
	dep := h.claimedDeployment(t, app.ID)

	h.worker.runPipeline(ctx, dep)

	c, _ := h.docker.FindContainer(ctx, "outhaul-app-web")
	if c == nil {
		t.Fatal("app container missing")
	}
	routers := map[string]bool{}
	for k := range c.Labels {
		if !strings.HasPrefix(k, "traefik.http.routers.outhaul-web-d") || !strings.HasSuffix(k, ".rule") {
			continue
		}
		router := strings.TrimSuffix(strings.TrimPrefix(k, "traefik.http.routers."), ".rule")
		router = strings.TrimSuffix(router, "-tls") // collapse a TLS mirror onto its base router
		routers[router] = true
	}
	if len(routers) != 2 {
		t.Errorf("distinct domain-row routers = %d, want one per domain row (2): %v", len(routers), c.Labels)
	}
}

func TestRoutingHonoursTunnel(t *testing.T) {
	ctx := context.Background()
	box, err := secret.Load(filepath.Join(t.TempDir(), "key"))
	if err != nil {
		t.Fatalf("secret.Load: %v", err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), box)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	w := &Worker{store: st, cfg: config.Config{ACMEEmail: "ops@example.com"}} // TLSEnabled() == true

	if !w.routing(ctx).TLS {
		t.Fatal("expected TLS on when ACME set and no tunnel")
	}
	if err := st.SetCloudflareToken(ctx, "tok"); err != nil {
		t.Fatalf("SetCloudflareToken: %v", err)
	}
	if w.routing(ctx).TLS {
		t.Fatal("expected TLS off once the tunnel is enabled")
	}
}

// A protected row reaches the container's labels with a gate pointing at the
// configured admin port; an open row strips the identity header.
func TestPipelineLabelsCarryTheGate(t *testing.T) {
	h := newHarness(t)
	h.worker.cfg.ListenAddr = ":9123"
	ctx := context.Background()
	app := h.app(t, "web") // seeds web.test as domain row 1
	if err := h.store.SetDomainProtected(ctx, app.ID, 1, true); err != nil {
		t.Fatalf("SetDomainProtected: %v", err)
	}
	if _, err := h.store.AddDomain(ctx, core.Domain{AppID: app.ID, Host: "web.test", Path: "/webhooks", Port: 8080}); err != nil {
		t.Fatalf("AddDomain: %v", err)
	}
	dep := h.claimedDeployment(t, app.ID)

	h.worker.runPipeline(ctx, dep)

	c, _ := h.docker.FindContainer(ctx, "outhaul-app-web")
	if c == nil {
		t.Fatal("app container missing")
	}
	if !traefik.RowHasGate(c.Labels, "outhaul-web-d1") {
		t.Errorf("protected row lacks its gate: %v", c.Labels)
	}
	if got := c.Labels["traefik.http.middlewares.outhaul-web-d1-gate.forwardauth.address"]; got != "http://host.docker.internal:9123/gate/check" {
		t.Errorf("gate address = %q, want the configured admin port", got)
	}
	if traefik.RowHasGate(c.Labels, "outhaul-web-d2") {
		t.Errorf("open row has a gate: %v", c.Labels)
	}
	if got := c.Labels["traefik.http.routers.outhaul-web-d2.middlewares"]; got != "outhaul-web-d2-noident" {
		t.Errorf("open row middlewares = %q, want the identity strip", got)
	}
}

// The build log's last line follows the server's routing settings: an
// HTTPS-only install says https.
func TestDoneLineIsHTTPSWhenHTTPOff(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  config.Config
		want string
	}{
		{"HTTPS only", config.Config{LocalCA: true, HTTPOff: true}, "is live at https://"},
		{"ordinary", config.Config{}, "is live at http://"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.worker.cfg.LocalCA = tc.cfg.LocalCA
			h.worker.cfg.HTTPOff = tc.cfg.HTTPOff
			ctx := context.Background()
			app := h.app(t, "web")
			dep := h.claimedDeployment(t, app.ID)
			h.worker.runPipeline(ctx, dep)
			if got := h.status(t, dep.ID); got.Status != core.StatusRunning {
				t.Fatalf("status = %q (reason %q), want running", got.Status, got.Reason)
			}
			history, _, cancel := h.broker.Subscribe(dep.ID)
			defer cancel()
			if len(history) == 0 {
				t.Fatal("no build log")
			}
			last := history[len(history)-1]
			if !strings.Contains(last, tc.want) {
				t.Errorf("last log line = %q, want it to contain %q", last, tc.want)
			}
		})
	}
}

// While a request header is required, an image whose labels would give its
// container a router Outhaul didn't write (and that lacks the header term) is
// refused, whether it was just built or arrived with the deployment.
func TestHeaderRequiredRefusesAnImageThatRoutesItself(t *testing.T) {
	for _, reuse := range []bool{false, true} {
		name := "built"
		if reuse {
			name = "reused"
		}
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.worker.cfg.RequireHeaderName = "X-Azure-FDID"
			h.worker.cfg.RequireHeaderValue = "abc-123"
			ctx := context.Background()
			app := h.app(t, "web")
			dep := h.claimedDeployment(t, app.ID)
			image := fmt.Sprintf("outhaul/%s:%d", app.Name, dep.ID)
			if reuse {
				image = "outhaul/web:reused"
				dep.Image = image
				dep.RollbackOf = 1
			}
			h.docker.ImageLabelsByRef = map[string]map[string]string{
				image: {"traefik.http.routers.x.rule": "Host(`evil.example.com`)"},
			}
			h.worker.runPipeline(ctx, dep)
			got := h.status(t, dep.ID)
			if got.Status != core.StatusFailed || !strings.Contains(got.Reason, "traefik.http.routers.x.rule") {
				t.Fatalf("status = %q (%q), want failed naming the label", got.Status, got.Reason)
			}
			if !strings.Contains(got.Reason, "while a request header is required") {
				t.Errorf("reason = %q, want the reason the image was inspected", got.Reason)
			}
			for _, spec := range h.docker.Created {
				if spec.Labels["traefik.enable"] == "true" {
					t.Errorf("a routed container was created from the refused image: %s", spec.Name)
				}
			}
		})
	}
}

// Under strict, an image that carries traefik.* labels would route itself
// through the container Docker copies those labels onto, so it never runs.
// Docker's label matching is case-insensitive for Traefik's keys.
func TestStrictPolicyRefusesAnImageWithTraefikLabels(t *testing.T) {
	for name, key := range map[string]string{
		"lowercase key":  "traefik.enable",
		"mixed-case key": "Traefik.HTTP.Routers.Evil.Rule",
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.worker.cfg.ContainerPolicy = "strict"
			app := h.app(t, "web")
			dep := h.claimedDeployment(t, app.ID)
			h.docker.ImageLabelsByRef = map[string]map[string]string{
				fmt.Sprintf("outhaul/%s:%d", app.Name, dep.ID): {key: "true"},
			}

			h.worker.runPipeline(context.Background(), dep)

			got := h.status(t, dep.ID)
			if got.Status != core.StatusFailed || !strings.Contains(got.Reason, "traefik labels") {
				t.Fatalf("status = %q (%q), want failed naming the traefik labels", got.Status, got.Reason)
			}
			if !strings.Contains(got.Reason, key) {
				t.Errorf("reason = %q, want it to name the label %q", got.Reason, key)
			}
			if !strings.Contains(got.Reason, "strict container policy") {
				t.Errorf("reason = %q, want the reason the image was inspected", got.Reason)
			}
			for _, spec := range h.docker.Created {
				if spec.Name == "outhaul-app-"+app.Name || strings.HasPrefix(spec.Name, "outhaul-deploy-") {
					t.Errorf("a container was created from the refused image: %s", spec.Name)
				}
			}
		})
	}
}

func TestNoHeaderAllowsAnImageWithTraefikLabels(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	app := h.app(t, "web")
	dep := h.claimedDeployment(t, app.ID)
	image := fmt.Sprintf("outhaul/%s:%d", app.Name, dep.ID)
	h.docker.ImageLabelsByRef = map[string]map[string]string{
		image: {"traefik.http.routers.x.rule": "Host(`own.example.com`)"},
	}
	h.worker.runPipeline(ctx, dep)
	if got := h.status(t, dep.ID); got.Status != core.StatusRunning {
		t.Fatalf("status = %q (%q), want running", got.Status, got.Reason)
	}
}

// A rollback arrives with an image and builds nothing, so the check runs once
// before the building -> deploying transition and leaves the live container
// alone.
func TestStrictPolicyRefusesARollbackImageWithTraefikLabels(t *testing.T) {
	h := newHarness(t)
	h.worker.cfg.ContainerPolicy = "strict"
	ctx := context.Background()
	app := h.app(t, "web")

	oldID, err := h.docker.CreateContainer(ctx, docker.ContainerSpec{Name: "outhaul-app-web", Image: "outhaul/web:9"})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.docker.StartContainer(ctx, oldID); err != nil {
		t.Fatal(err)
	}
	h.docker.ImageLabelsByRef = map[string]map[string]string{
		"outhaul/web:7": {"traefik.enable": "true"},
	}
	createdBefore := len(h.docker.Created)

	dep := h.claimedRollback(t, app.ID, "outhaul/web:7", 7)
	h.worker.runPipeline(ctx, dep)

	got := h.status(t, dep.ID)
	if got.Status != core.StatusFailed || !strings.Contains(got.Reason, "traefik labels") {
		t.Fatalf("status = %q (%q), want failed naming the traefik labels", got.Status, got.Reason)
	}
	if !strings.Contains(got.Reason, "strict container policy") {
		t.Errorf("reason = %q, want the reason the image was inspected", got.Reason)
	}
	if len(h.docker.Created) != createdBefore {
		t.Errorf("a container was created from the refused image: %v", h.docker.Created[createdBefore:])
	}
	if c, _ := h.docker.FindContainer(ctx, "outhaul-app-web"); c == nil || c.Image != "outhaul/web:9" {
		t.Errorf("the live container should survive a refused rollback, got %+v", c)
	}
}

// A server without the policy inspects no image: its deploys are unchanged.
func TestPipelineWithoutPolicyNeverInspectsTheImage(t *testing.T) {
	h := newHarness(t)
	app := h.app(t, "web")
	dep := h.claimedDeployment(t, app.ID)
	inspected := false
	h.docker.FailImageLabels = func(string) error {
		inspected = true
		return nil
	}

	h.worker.runPipeline(context.Background(), dep)

	if got := h.status(t, dep.ID); got.Status != core.StatusRunning {
		t.Fatalf("status = %q (%q), want running", got.Status, got.Reason)
	}
	if inspected {
		t.Error("the image was inspected without the policy")
	}
}
