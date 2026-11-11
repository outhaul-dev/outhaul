package deploy

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/outhaul-dev/outhaul/internal/compose"
	"github.com/outhaul-dev/outhaul/internal/core"
	"github.com/outhaul-dev/outhaul/internal/traefik"
)

// composeApp creates a compose-kind app whose fake clone contains a compose
// file at the given path. A non-empty domain is published on service web:3000.
func (h *harness) composeApp(t *testing.T, name, composePath, domain string) core.App {
	t.Helper()
	h.cloner.files = map[string]string{composePath: "services:\n  web:\n    image: nginx\n"}
	app, err := h.store.CreateApp(context.Background(), core.App{
		Name: name, RepoURL: "https://example.com/" + name + ".git",
		Kind: core.KindCompose, ComposePath: composePath,
	})
	if err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	if domain != "" {
		if _, err := h.store.AddDomain(context.Background(), core.Domain{
			AppID: app.ID, Host: domain, Service: "web", Port: 3000,
		}); err != nil {
			t.Fatalf("AddDomain: %v", err)
		}
	}
	return app
}

func TestComposePipelineHappyPath(t *testing.T) {
	h := newHarness(t)
	app := h.composeApp(t, "shop", "docker-compose.yml", "shop.example.com")

	// Capture generated files while the work dir still exists.
	var envContent, overrideContent string
	h.compose.Hook = func(c compose.Call) {
		if c.Verb != "up" {
			return
		}
		if b, err := os.ReadFile(filepath.Join(c.Dir, ".env")); err == nil {
			envContent = string(b)
		}
		if b, err := os.ReadFile(filepath.Join(c.Dir, compose.OverrideFile)); err == nil {
			overrideContent = string(b)
		}
	}
	if err := h.store.SetEnv(context.Background(), app.ID, "API_KEY", "s3cr3t", true); err != nil {
		t.Fatal(err)
	}

	dep := h.claimedDeployment(t, app.ID)
	h.worker.runPipeline(context.Background(), dep)

	got := h.status(t, dep.ID)
	if got.Status != core.StatusRunning {
		t.Fatalf("status = %q (reason %q), want running", got.Status, got.Reason)
	}

	builds, ups := h.compose.CallsFor("build"), h.compose.CallsFor("up")
	if len(builds) != 1 || len(ups) != 1 {
		t.Fatalf("compose calls: build=%d up=%d, want 1 each", len(builds), len(ups))
	}
	if ups[0].Project != "outhaul-shop" {
		t.Errorf("project = %q, want outhaul-shop", ups[0].Project)
	}
	wantFiles := []string{"docker-compose.yml", compose.OverrideFile}
	if len(ups[0].Files) != 2 || ups[0].Files[0] != wantFiles[0] || ups[0].Files[1] != wantFiles[1] {
		t.Errorf("up files = %v, want %v", ups[0].Files, wantFiles)
	}
	if !strings.Contains(envContent, `API_KEY="s3cr3t"`) {
		t.Errorf(".env missing the secret var; got:\n%s", envContent)
	}
	if !strings.Contains(overrideContent, "Host(`shop.example.com`)") {
		t.Errorf("override missing the domain rule; got:\n%s", overrideContent)
	}
	// The compose path never touches single-container machinery.
	if len(h.docker.Created) != 0 {
		t.Errorf("no containers should be created directly: %v", h.docker.Created)
	}
	if h.builder.lastReq.ImageTag != "" {
		t.Error("nixpacks builder must not run for a compose app")
	}
}

// TestComposePipelineMultipleDomains: every published domain lands in the one
// generated override, each with its own Host rule.
func TestComposePipelineMultipleDomains(t *testing.T) {
	h := newHarness(t)
	app := h.composeApp(t, "shop", "docker-compose.yml", "shop.example.com")
	if _, err := h.store.AddDomain(context.Background(), core.Domain{
		AppID: app.ID, Host: "api.example.com", Service: "api", Port: 8080,
	}); err != nil {
		t.Fatal(err)
	}

	var overrideContent string
	h.compose.Hook = func(c compose.Call) {
		if c.Verb != "up" {
			return
		}
		if b, err := os.ReadFile(filepath.Join(c.Dir, compose.OverrideFile)); err == nil {
			overrideContent = string(b)
		}
	}

	dep := h.claimedDeployment(t, app.ID)
	h.worker.runPipeline(context.Background(), dep)

	if got := h.status(t, dep.ID); got.Status != core.StatusRunning {
		t.Fatalf("status = %q (%q), want running", got.Status, got.Reason)
	}
	for _, host := range []string{"Host(`shop.example.com`)", "Host(`api.example.com`)"} {
		if !strings.Contains(overrideContent, host) {
			t.Errorf("override missing %s; got:\n%s", host, overrideContent)
		}
	}
}

func TestComposePipelineNestedPathAndNoDomain(t *testing.T) {
	h := newHarness(t)
	app := h.composeApp(t, "shop", "deploy/compose.yml", "")

	var hadOverride bool
	var envPath string
	h.compose.Hook = func(c compose.Call) {
		if c.Verb != "up" {
			return
		}
		_, err := os.Stat(filepath.Join(c.Dir, "deploy", compose.OverrideFile))
		hadOverride = err == nil
		if _, err := os.Stat(filepath.Join(c.Dir, "deploy", ".env")); err == nil {
			envPath = "deploy/.env"
		}
	}

	dep := h.claimedDeployment(t, app.ID)
	h.worker.runPipeline(context.Background(), dep)

	if got := h.status(t, dep.ID); got.Status != core.StatusRunning {
		t.Fatalf("status = %q (%q), want running", got.Status, got.Reason)
	}
	ups := h.compose.CallsFor("up")
	if len(ups) != 1 || len(ups[0].Files) != 1 || ups[0].Files[0] != "deploy/compose.yml" {
		t.Fatalf("up files = %+v, want just deploy/compose.yml (no domain, no override)", ups)
	}
	if hadOverride {
		t.Error("override must not be written when the app has no domain")
	}
	if envPath != "deploy/.env" {
		t.Error(".env must be written next to the compose file, not the repo root")
	}
}

func TestComposePipelineMissingFileFails(t *testing.T) {
	h := newHarness(t)
	app := h.composeApp(t, "shop", "docker-compose.yml", "")
	h.cloner.files = map[string]string{"README.md": "no compose here"}

	dep := h.claimedDeployment(t, app.ID)
	h.worker.runPipeline(context.Background(), dep)

	got := h.status(t, dep.ID)
	if got.Status != core.StatusFailed {
		t.Fatalf("status = %q, want failed", got.Status)
	}
	if !strings.Contains(got.Reason, "docker-compose.yml") || !strings.Contains(got.Reason, "not found") {
		t.Errorf("reason = %q, want it to name the missing compose file", got.Reason)
	}
	if len(h.compose.Calls) != 0 {
		t.Errorf("no compose command should run: %v", h.compose.Calls)
	}
}

func TestComposePipelineBuildFailureMarksFailed(t *testing.T) {
	h := newHarness(t)
	app := h.composeApp(t, "shop", "docker-compose.yml", "")
	h.compose.FailBuild = errors.New("service web has neither an image nor a build context")

	dep := h.claimedDeployment(t, app.ID)
	h.worker.runPipeline(context.Background(), dep)

	got := h.status(t, dep.ID)
	if got.Status != core.StatusFailed || !strings.Contains(got.Reason, "build failed") {
		t.Fatalf("status = %q (%q), want failed with build reason", got.Status, got.Reason)
	}
	if len(h.compose.CallsFor("up")) != 0 {
		t.Error("up must not run after a failed build")
	}
}

func TestComposePipelineUpFailureMarksFailed(t *testing.T) {
	h := newHarness(t)
	app := h.composeApp(t, "shop", "docker-compose.yml", "")
	h.compose.FailUp = errors.New("container outhaul-shop-web-1 is unhealthy")

	dep := h.claimedDeployment(t, app.ID)
	h.worker.runPipeline(context.Background(), dep)

	got := h.status(t, dep.ID)
	if got.Status != core.StatusFailed {
		t.Fatalf("status = %q, want failed", got.Status)
	}
	if !strings.Contains(got.Reason, "stack failed to become ready") {
		t.Errorf("reason = %q", got.Reason)
	}
}

func TestComposePipelineCancelDuringBuildAborts(t *testing.T) {
	h := newHarness(t)
	app := h.composeApp(t, "shop", "docker-compose.yml", "")
	dep := h.claimedDeployment(t, app.ID)
	// Operator cancels while `compose build` runs: the row flips to cancelled,
	// so the guarded building->deploying transition must abort the pipeline.
	h.compose.Hook = func(c compose.Call) {
		if c.Verb == "build" {
			if _, err := h.store.SetStatus(context.Background(), dep.ID, core.StatusBuilding, core.StatusCancelled, "cancelled by operator"); err != nil {
				t.Errorf("cancel: %v", err)
			}
		}
	}

	h.worker.runPipeline(context.Background(), dep)

	if got := h.status(t, dep.ID); got.Status != core.StatusCancelled {
		t.Fatalf("status = %q, want cancelled to survive", got.Status)
	}
	if len(h.compose.CallsFor("up")) != 0 {
		t.Error("up must not run after a cancel")
	}
}

func TestComposePipelineAbortsIfAppDeletedDuringBuild(t *testing.T) {
	h := newHarness(t)
	app := h.composeApp(t, "shop", "docker-compose.yml", "")
	dep := h.claimedDeployment(t, app.ID)
	h.compose.Hook = func(c compose.Call) {
		if c.Verb == "build" {
			_ = h.store.DeleteApp(context.Background(), app.ID)
		}
	}

	h.worker.runPipeline(context.Background(), dep)

	if len(h.compose.CallsFor("up")) != 0 {
		t.Error("up must not run for a deleted app")
	}
}

// The compose override carries the gate for a protected row, at the
// configured admin port.
func TestComposePipelineOverrideCarriesTheGate(t *testing.T) {
	h := newHarness(t)
	h.worker.cfg.ListenAddr = ":9123"
	app := h.composeApp(t, "shop", "docker-compose.yml", "shop.example.com")
	ds, err := h.store.ListDomains(context.Background(), app.ID)
	if err != nil || len(ds) != 1 {
		t.Fatalf("ListDomains = %v (%v)", ds, err)
	}
	if err := h.store.SetDomainProtected(context.Background(), app.ID, ds[0].ID, true); err != nil {
		t.Fatal(err)
	}
	var overrideContent string
	h.compose.Hook = func(c compose.Call) {
		if c.Verb != "up" {
			return
		}
		if b, err := os.ReadFile(filepath.Join(c.Dir, compose.OverrideFile)); err == nil {
			overrideContent = string(b)
		}
	}

	dep := h.claimedDeployment(t, app.ID)
	h.worker.runPipeline(context.Background(), dep)

	if got := h.status(t, dep.ID); got.Status != core.StatusRunning {
		t.Fatalf("status = %q (%q), want running", got.Status, got.Reason)
	}
	router := traefik.RouterName("shop", ds[0].ID)
	for _, want := range []string{
		`"traefik.http.middlewares.` + router + `-gate.forwardauth.address": "http://host.docker.internal:9123/gate/check"`,
		`"traefik.http.routers.` + router + `.middlewares": "` + router + `-gate,` + router + `-noalias"`,
		`"traefik.http.middlewares.` + router + `-gate.forwardauth.trustforwardheader": "false"`,
	} {
		if !strings.Contains(overrideContent, want) {
			t.Errorf("override missing %s; got:\n%s", want, overrideContent)
		}
	}
}

// A server that has not opted into the container policy never asks Compose to
// normalise the stack, so its deploys are unchanged. ConfigJSON is left unset:
// a config call would fail the parse too, but the call count is the direct
// check.
func TestComposePipelineWithoutPolicyNeverReadsModel(t *testing.T) {
	h := newHarness(t)
	app := h.composeApp(t, "shop", "docker-compose.yml", "shop.example.com")
	inspected := false
	h.docker.FailImageLabels = func(string) error {
		inspected = true
		return nil
	}

	dep := h.claimedDeployment(t, app.ID)
	h.worker.runPipeline(context.Background(), dep)

	if got := h.status(t, dep.ID); got.Status != core.StatusRunning {
		t.Fatalf("status = %q (%q), want running", got.Status, got.Reason)
	}
	if calls := h.compose.CallsFor("config"); len(calls) != 0 {
		t.Errorf("compose config ran without the policy: %+v", calls)
	}
	if calls := h.compose.CallsFor("pull"); len(calls) != 0 {
		t.Errorf("compose pull ran without the policy: %+v", calls)
	}
	if inspected {
		t.Error("an image was inspected without the policy")
	}
}

func TestComposePipelineRefusesAHostPathBeforeBuilding(t *testing.T) {
	h := newHarness(t)
	h.worker.cfg.ContainerPolicy = "strict"
	h.compose.ConfigJSON = []byte(`{
		"name": "outhaul-shop",
		"services": {
			"web": {
				"image": "nginx",
				"volumes": [{"type": "bind", "source": "/srv/data", "target": "/data"}]
			}
		}
	}`)
	app := h.composeApp(t, "shop", "docker-compose.yml", "")

	dep := h.claimedDeployment(t, app.ID)
	h.worker.runPipeline(context.Background(), dep)

	got := h.status(t, dep.ID)
	if got.Status != core.StatusFailed {
		t.Fatalf("status = %q, want failed", got.Status)
	}
	for _, want := range []string{`service "web"`, "volumes", "bind mount"} {
		if !strings.Contains(got.Reason, want) {
			t.Errorf("reason = %q, want it to mention %q", got.Reason, want)
		}
	}
	if n := len(h.compose.CallsFor("build")); n != 0 {
		t.Errorf("build calls = %d, want 0", n)
	}
	if n := len(h.compose.CallsFor("up")); n != 0 {
		t.Errorf("up calls = %d, want 0", n)
	}
	want := `service "web": volumes: a bind mount is a host path. Use a named volume.`
	if log := h.logOf(dep); !strings.Contains(log, want) {
		t.Errorf("log missing %q; got:\n%s", want, log)
	}
}

func TestComposePipelineLogsEveryViolationAndKeepsTheJSONOut(t *testing.T) {
	h := newHarness(t)
	h.worker.cfg.ContainerPolicy = "strict"
	h.compose.ConfigJSON = []byte(`{
		"name": "outhaul-shop",
		"services": {
			"web": {
				"image": "nginx",
				"environment": {"TOKEN": "super-secret"},
				"ports": ["8080:80"],
				"privileged": true
			}
		}
	}`)
	app := h.composeApp(t, "shop", "docker-compose.yml", "")

	dep := h.claimedDeployment(t, app.ID)
	h.worker.runPipeline(context.Background(), dep)

	got := h.status(t, dep.ID)
	if got.Status != core.StatusFailed {
		t.Fatalf("status = %q, want failed", got.Status)
	}
	log := h.logOf(dep)
	for _, want := range []string{
		`service "web": ports: publishing a host port is not allowed.`,
		`service "web": privileged: a privileged container is not allowed.`,
	} {
		if !strings.Contains(log, want) {
			t.Errorf("log missing %q; got:\n%s", want, log)
		}
	}
	if !strings.Contains(got.Reason, "(and 1 more)") {
		t.Errorf("reason = %q, want it to count the other violation", got.Reason)
	}
	for _, text := range []string{log, got.Reason} {
		if strings.Contains(text, "super-secret") {
			t.Errorf("the model's values leaked: %q", text)
		}
	}
}

func TestComposePipelineReportsAFileComposeCannotLoad(t *testing.T) {
	h := newHarness(t)
	h.worker.cfg.ContainerPolicy = "strict"
	h.compose.FailConfig = errors.New("services.web.ports must be a list")
	app := h.composeApp(t, "shop", "docker-compose.yml", "")

	dep := h.claimedDeployment(t, app.ID)
	h.worker.runPipeline(context.Background(), dep)

	got := h.status(t, dep.ID)
	if got.Status != core.StatusFailed {
		t.Fatalf("status = %q, want failed", got.Status)
	}
	if !strings.Contains(got.Reason, "read Compose file: services.web.ports must be a list") {
		t.Errorf("reason = %q, want the compose error", got.Reason)
	}
	if n := len(h.compose.CallsFor("build")); n != 0 {
		t.Errorf("build calls = %d, want 0", n)
	}
	if n := len(h.compose.CallsFor("up")); n != 0 {
		t.Errorf("up calls = %d, want 0", n)
	}
}

// Strict mode pulls every image `up` will use, then refuses a service whose
// image routes itself. Docker copies the image's labels onto the container and
// Traefik reads them.
func TestComposePolicyPullsAndRefusesATraefikImage(t *testing.T) {
	h := newHarness(t)
	h.worker.cfg.ContainerPolicy = "strict"
	h.compose.ConfigJSON = []byte(`{
		"name": "outhaul-shop",
		"services": {
			"web": {"image": "nginx:1"},
			"db": {"image": "postgres:16"}
		}
	}`)
	h.docker.ImageLabelsByRef = map[string]map[string]string{
		"nginx:1": {"traefik.http.routers.evil.rule": "Host(`shop.example.com`)"},
	}
	app := h.composeApp(t, "shop", "docker-compose.yml", "")

	dep := h.claimedDeployment(t, app.ID)
	h.worker.runPipeline(context.Background(), dep)

	got := h.status(t, dep.ID)
	if got.Status != core.StatusFailed {
		t.Fatalf("status = %q (%q), want failed", got.Status, got.Reason)
	}
	for _, want := range []string{`service "web"`, "nginx:1", "traefik labels", "traefik.http.routers.evil.rule"} {
		if !strings.Contains(got.Reason, want) {
			t.Errorf("reason = %q, want it to mention %q", got.Reason, want)
		}
	}
	if pulls := h.compose.CallsFor("pull"); len(pulls) != 1 {
		t.Errorf("pull calls = %d, want 1", len(pulls))
	}
	if n := len(h.compose.CallsFor("up")); n != 0 {
		t.Errorf("up calls = %d, want 0", n)
	}
}

// A service image can still be missing after pull (pull_policy: never, say).
// That fails the deploy naming the service and image rather than surfacing
// late at `up`.
func TestComposePolicyRefusesAnImageThatIsNotOnTheServer(t *testing.T) {
	h := newHarness(t)
	h.worker.cfg.ContainerPolicy = "strict"
	h.compose.ConfigJSON = []byte(`{
		"name": "outhaul-shop",
		"services": {
			"web": {"image": "nginx:1"}
		}
	}`)
	h.docker.FailImageLabels = func(ref string) error {
		if ref == "nginx:1" {
			return errors.New("no such image")
		}
		return nil
	}
	app := h.composeApp(t, "shop", "docker-compose.yml", "")

	dep := h.claimedDeployment(t, app.ID)
	h.worker.runPipeline(context.Background(), dep)

	got := h.status(t, dep.ID)
	if got.Status != core.StatusFailed {
		t.Fatalf("status = %q (%q), want failed", got.Status, got.Reason)
	}
	for _, want := range []string{`service "web"`, "nginx:1", "no such image"} {
		if !strings.Contains(got.Reason, want) {
			t.Errorf("reason = %q, want it to mention %q", got.Reason, want)
		}
	}
	if n := len(h.compose.CallsFor("up")); n != 0 {
		t.Errorf("up calls = %d, want 0", n)
	}
}

// A failed pull fails the deploy before any image is inspected.
func TestComposePolicyReportsAPullFailure(t *testing.T) {
	h := newHarness(t)
	h.worker.cfg.ContainerPolicy = "strict"
	h.compose.ConfigJSON = []byte(`{
		"name": "outhaul-shop",
		"services": {
			"web": {"image": "nginx:1"}
		}
	}`)
	h.compose.FailPull = errors.New("registry unreachable")
	inspected := false
	h.docker.FailImageLabels = func(string) error {
		inspected = true
		return nil
	}
	app := h.composeApp(t, "shop", "docker-compose.yml", "")

	dep := h.claimedDeployment(t, app.ID)
	h.worker.runPipeline(context.Background(), dep)

	got := h.status(t, dep.ID)
	if got.Status != core.StatusFailed || !strings.Contains(got.Reason, "registry unreachable") {
		t.Fatalf("status = %q (%q), want failed with the pull error", got.Status, got.Reason)
	}
	if inspected {
		t.Error("an image was inspected after a failed pull")
	}
	if n := len(h.compose.CallsFor("up")); n != 0 {
		t.Errorf("up calls = %d, want 0", n)
	}
}
