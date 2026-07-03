package deploy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/slipwaydev/slipway/internal/builder"
	"github.com/slipwaydev/slipway/internal/config"
	"github.com/slipwaydev/slipway/internal/core"
	"github.com/slipwaydev/slipway/internal/docker"
	"github.com/slipwaydev/slipway/internal/github"
	"github.com/slipwaydev/slipway/internal/logstream"
	"github.com/slipwaydev/slipway/internal/secret"
	"github.com/slipwaydev/slipway/internal/store"
)

// --- test doubles ---

type fakeBuilder struct {
	err     error
	lastReq builder.BuildRequest
}

func (f *fakeBuilder) Name() string { return "fake" }
func (f *fakeBuilder) Build(_ context.Context, req builder.BuildRequest, out io.Writer) error {
	f.lastReq = req
	if f.err != nil {
		return f.err
	}
	fmt.Fprintf(out, "built %s\n", req.ImageTag)
	return nil
}

type fakeCloner struct {
	err    error
	cloned []string
}

func (f *fakeCloner) Clone(_ context.Context, spec CloneSpec, dir string, out io.Writer) error {
	if f.err != nil {
		return f.err
	}
	f.cloned = append(f.cloned, spec.URL)
	fmt.Fprintf(out, "cloned %s into %s\n", spec.URL, dir)
	return nil
}

// --- harness ---

type harness struct {
	store   *store.Store
	docker  *docker.Fake
	builder *fakeBuilder
	cloner  *fakeCloner
	broker  *logstream.Broker
	worker  *Worker
}

func newHarness(t *testing.T) *harness {
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
		store:   st,
		docker:  docker.NewFake(),
		builder: &fakeBuilder{},
		cloner:  &fakeCloner{},
		broker:  logstream.New(),
	}
	cfg := config.Config{DataDir: t.TempDir(), Network: "slipway"}
	h.worker = NewWorker(st, h.docker, h.builder, h.cloner, h.broker, &github.Fake{}, cfg)
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
	c, _ := h.docker.FindContainer(ctx, "slipway-app-web")
	if c == nil || !c.Running() {
		t.Fatalf("app container not running: %+v", c)
	}
	if c.Labels["traefik.http.routers.slipway-web.rule"] != "Host(`web.test`)" {
		t.Errorf("missing/incorrect traefik rule label: %v", c.Labels)
	}
	spec := lastCreatedNamed(t, h, "slipway-app-web")
	if !contains(spec.Networks, "slipway") {
		t.Errorf("container not on slipway network: %v", spec.Networks)
	}
	if !containsPrefix(spec.Env, "PORT=") {
		t.Errorf("container missing PORT env: %v", spec.Env)
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

	oldID, _ := h.docker.CreateContainer(ctx, docker.ContainerSpec{Name: "slipway-app-web", Image: "old:1"})
	h.docker.StartContainer(ctx, oldID)

	h.worker.healthCheck = func(context.Context, string, time.Duration) bool { return false }

	dep := h.claimedDeployment(t, app.ID)
	h.worker.runPipeline(ctx, dep)

	if got := h.status(t, dep.ID); got.Status != core.StatusFailed {
		t.Fatalf("status = %q, want failed", got.Status)
	}
	if c, _ := h.docker.FindContainer(ctx, "slipway-app-web"); c == nil || c.Image != "old:1" {
		t.Errorf("old container should survive an unhealthy deploy, got %+v", c)
	}
}

func TestPipelineHealthyCutsOver(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	app := h.app(t, "web")
	oldID, _ := h.docker.CreateContainer(ctx, docker.ContainerSpec{Name: "slipway-app-web", Image: "old:1"})
	h.docker.StartContainer(ctx, oldID)

	dep := h.claimedDeployment(t, app.ID) // healthy by harness default

	h.worker.runPipeline(ctx, dep)

	if got := h.status(t, dep.ID); got.Status != core.StatusRunning {
		t.Fatalf("status = %q (%q), want running", got.Status, got.Reason)
	}
	if _, ok := h.docker.Containers[oldID]; ok {
		t.Error("old container not removed after healthy cutover")
	}
	c, _ := h.docker.FindContainer(ctx, "slipway-app-web")
	if c == nil || !c.Running() {
		t.Fatalf("canonical container not running: %+v", c)
	}
	if c.Labels["traefik.enable"] != "true" {
		t.Errorf("canonical container should have traefik enabled: %v", c.Labels)
	}
	if tmp, _ := h.docker.FindContainer(ctx, "slipway-deploy-"+itoa64(dep.ID)); tmp != nil {
		t.Error("temp container was not cleaned up")
	}
}

func itoa64(n int64) string { return fmt.Sprintf("%d", n) }

func TestPipelineTempRemovedWhenOldRemovalFails(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	app := h.app(t, "web")
	oldID, _ := h.docker.CreateContainer(ctx, docker.ContainerSpec{Name: "slipway-app-web", Image: "old:1"})
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
	if tmp, _ := h.docker.FindContainer(ctx, "slipway-deploy-"+itoa64(dep.ID)); tmp != nil {
		t.Error("temp container leaked when old removal failed")
	}
}

func TestPipelineCutoverFailureReportsAppDown(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	app := h.app(t, "web")
	// Fail creation of the CANONICAL container (the second create, enabled labels).
	h.docker.FailCreate = func(spec docker.ContainerSpec) error {
		if spec.Name == "slipway-app-web" && spec.Labels["traefik.enable"] == "true" {
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
	if tmp, _ := h.docker.FindContainer(ctx, "slipway-deploy-"+itoa64(dep.ID)); tmp != nil {
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

	if c, _ := h.docker.FindContainer(ctx, "slipway-app-web"); c != nil {
		t.Error("canonical container was created for a deleted app (orphan)")
	}
	if tmp, _ := h.docker.FindContainer(ctx, "slipway-deploy-"+itoa64(dep.ID)); tmp != nil {
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
	spec := lastCreatedNamed(t, h, "slipway-app-web")
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

	spec := lastCreatedNamed(t, h, "slipway-app-web")
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
