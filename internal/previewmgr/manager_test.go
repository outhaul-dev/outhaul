package previewmgr

import (
	"context"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/james-smart/outhaul/internal/core"
	"github.com/james-smart/outhaul/internal/secret"
	"github.com/james-smart/outhaul/internal/store"
	"github.com/james-smart/outhaul/internal/webhook"
)

// --- fakes -----------------------------------------------------------------

// fakeNotifier records whether the deploy worker was woken.
type fakeNotifier struct{ notified bool }

func (n *fakeNotifier) Notify() { n.notified = true }

// fakeDocker records which app names it was asked to remove; it can be told to
// fail on any RemoveApp so we can exercise the teardown-failure path.
type fakeDocker struct {
	removed map[string]bool
	fail    bool
}

func (d *fakeDocker) RemoveApp(_ context.Context, name string) error {
	if d.removed == nil {
		d.removed = map[string]bool{}
	}
	d.removed[name] = true
	if d.fail {
		return context.DeadlineExceeded
	}
	return nil
}

// fakeDBProvisioner persists provisioned databases into the REAL store so the
// manager's subsequent AttachDatabase (which checks FK + same project) finds a
// real row. Provision inserts the row and marks it running; Destroy records the
// call. Counters let tests assert how many DBs were created/destroyed.
type fakeDBProvisioner struct {
	st            *store.Store
	provisioned   int
	destroyed     int
	failProvision bool
}

func (p *fakeDBProvisioner) Provision(ctx context.Context, d core.Database) (core.Database, error) {
	if p.failProvision {
		return core.Database{}, context.DeadlineExceeded
	}
	created, err := p.st.CreateDatabase(ctx, d)
	if err != nil {
		return core.Database{}, err
	}
	if err := p.st.SetDatabaseStatus(ctx, created.ID, core.DBRunning, ""); err != nil {
		return core.Database{}, err
	}
	created.Status = core.DBRunning
	p.provisioned++
	return created, nil
}

func (p *fakeDBProvisioner) Destroy(_ context.Context, _ core.Database) error {
	p.destroyed++
	return nil
}

// fakeCommenter records every PR comment posted, keyed by "repo#pr".
type fakeCommenter struct {
	comments map[string][]string
}

func (c *fakeCommenter) UpsertPRComment(_ context.Context, _, repo string, pr int, body string) error {
	if c.comments == nil {
		c.comments = map[string][]string{}
	}
	key := repo + "#" + strconv.Itoa(pr)
	c.comments[key] = append(c.comments[key], body)
	return nil
}

// --- harness ---------------------------------------------------------------

type harness struct {
	st       *store.Store
	mgr      *Manager
	notifier *fakeNotifier
	docker   *fakeDocker
	dbprov   *fakeDBProvisioner
	gh       *fakeCommenter
}

const serverIP = "1.2.3.4"

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
		st:       st,
		notifier: &fakeNotifier{},
		docker:   &fakeDocker{removed: map[string]bool{}},
		dbprov:   &fakeDBProvisioner{st: st},
		gh:       &fakeCommenter{comments: map[string][]string{}},
	}
	ts := func(context.Context) (string, bool, error) { return "tok", true, nil }
	h.mgr = New(st, h.notifier, h.dbprov, h.docker, h.gh, ts, serverIP)
	return h
}

// seedGithubApp creates a GitHub-connected parent app, a routed domain row so
// mintDomains has something to copy, and an enabled preview config (sslip base:
// BaseDomain=""). cfg lets a test tweak the config before it is stored.
func (h *harness) seedGithubApp(t *testing.T, name, repo string, tweak func(*core.PreviewConfig)) core.App {
	t.Helper()
	ctx := context.Background()
	app, err := h.st.CreateApp(ctx, core.App{
		Name:       name,
		RepoURL:    "https://github.com/" + repo + ".git",
		Domain:     name + ".example",
		Source:     core.SourceGithub,
		GithubRepo: repo,
		Branch:     "main",
	})
	if err != nil {
		t.Fatalf("CreateApp parent: %v", err)
	}
	// CreateApp already seeds a domain row from the primary Domain; the parent
	// therefore has one routed row (Service "", Port 8080).
	cfg := core.DefaultPreviewConfig(app.ID)
	cfg.Enabled = true
	cfg.BaseDomain = ""
	if tweak != nil {
		tweak(&cfg)
	}
	if err := h.st.SetPreviewConfig(ctx, cfg); err != nil {
		t.Fatalf("SetPreviewConfig: %v", err)
	}
	return app
}

// seedAttachment creates a managed database in the parent's project and attaches
// it to the parent under envVar.
func (h *harness) seedAttachment(t *testing.T, parent core.App, dbName, envVar string) core.Database {
	t.Helper()
	ctx := context.Background()
	db, err := h.st.CreateDatabase(ctx, core.Database{
		ProjectID: parent.ProjectID,
		Name:      dbName,
		Engine:    core.EnginePostgres,
		Image:     "postgres:16",
		Username:  "app",
		Password:  "pw",
		DBName:    "app",
	})
	if err != nil {
		t.Fatalf("CreateDatabase: %v", err)
	}
	if _, err := h.st.AttachDatabase(ctx, parent.ID, db.ID, envVar); err != nil {
		t.Fatalf("AttachDatabase parent: %v", err)
	}
	return db
}

func prEvent(action string, number int, headRef, baseRepo string, isFork bool) webhook.PullRequestEvent {
	return webhook.PullRequestEvent{
		Action:           action,
		Number:           number,
		HeadRef:          headRef,
		HeadSHA:          "abcdef1234567890",
		BaseRepoFullName: baseRepo,
		IsFork:           isFork,
	}
}

// --- tests -----------------------------------------------------------------

func TestSpawnCreatesChildAppDomainsAndDeploys(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	repo := "acme/web"
	parent := h.seedGithubApp(t, "web", repo, nil)
	h.seedAttachment(t, parent, "web-db", "DATABASE_URL")

	if err := h.mgr.Handle(ctx, prEvent("opened", 42, "feature-x", repo, false)); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	child, err := h.st.GetPreviewByPR(ctx, parent.ID, 42)
	if err != nil {
		t.Fatalf("GetPreviewByPR: %v", err)
	}
	if child.Name != "web-pr-42" {
		t.Errorf("child name = %q, want web-pr-42", child.Name)
	}
	if !child.Ephemeral {
		t.Error("child should be ephemeral")
	}
	if child.Branch != "feature-x" {
		t.Errorf("child branch = %q, want feature-x", child.Branch)
	}
	if child.PreviewStatus != core.PreviewBuilding {
		t.Errorf("child status = %q, want building", child.PreviewStatus)
	}

	// Fresh DB provisioned and attached under the same env var.
	if h.dbprov.provisioned != 1 {
		t.Errorf("provisioned = %d, want 1", h.dbprov.provisioned)
	}
	atts, err := h.st.ListAttachments(ctx, child.ID)
	if err != nil {
		t.Fatalf("ListAttachments child: %v", err)
	}
	if len(atts) != 1 || atts[0].EnvVar != "DATABASE_URL" {
		t.Fatalf("child attachments = %+v, want one DATABASE_URL", atts)
	}

	// Domain row minted with the sslip host.
	doms, err := h.st.ListDomains(ctx, child.ID)
	if err != nil {
		t.Fatalf("ListDomains child: %v", err)
	}
	if len(doms) != 1 {
		t.Fatalf("child domains = %d, want 1", len(doms))
	}
	if want := "web-pr-42.1.2.3.4.sslip.io"; doms[0].Host != want {
		t.Errorf("child domain host = %q, want %q", doms[0].Host, want)
	}

	// Preview env marker copied in.
	envs, err := h.st.ListEnv(ctx, child.ID)
	if err != nil {
		t.Fatalf("ListEnv child: %v", err)
	}
	if !hasEnv(envs, "OUTHAUL_PREVIEW", "true") {
		t.Errorf("child env missing OUTHAUL_PREVIEW=true: %+v", envs)
	}
	if !hasEnv(envs, "OUTHAUL_PR_NUMBER", "42") {
		t.Errorf("child env missing OUTHAUL_PR_NUMBER=42: %+v", envs)
	}
	if !hasEnv(envs, "OUTHAUL_PREVIEW_URL", "https://web-pr-42.1.2.3.4.sslip.io") {
		t.Errorf("child env missing OUTHAUL_PREVIEW_URL: %+v", envs)
	}

	// Deployment enqueued + worker notified.
	deps, err := h.st.ListDeploymentsForApp(ctx, child.ID)
	if err != nil {
		t.Fatalf("ListDeploymentsForApp: %v", err)
	}
	if len(deps) != 1 {
		t.Errorf("child deployments = %d, want 1", len(deps))
	}
	if !h.notifier.notified {
		t.Error("notifier was not fired")
	}
}

func TestSpawnForkPRGated(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	repo := "acme/web"
	parent := h.seedGithubApp(t, "web", repo, nil) // AllowForkPRs defaults false

	if err := h.mgr.Handle(ctx, prEvent("opened", 7, "fork-branch", repo, true)); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if _, err := h.st.GetPreviewByPR(ctx, parent.ID, 7); err == nil {
		t.Fatal("fork PR should not have created a child")
	}
	if h.notifier.notified {
		t.Error("notifier should not fire for a gated fork PR")
	}
}

func TestSpawnRespectsMaxConcurrent(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	repo := "acme/web"
	parent := h.seedGithubApp(t, "web", repo, func(c *core.PreviewConfig) { c.MaxConcurrent = 1 })

	if err := h.mgr.Handle(ctx, prEvent("opened", 1, "b1", repo, false)); err != nil {
		t.Fatalf("Handle PR1: %v", err)
	}
	if _, err := h.st.GetPreviewByPR(ctx, parent.ID, 1); err != nil {
		t.Fatalf("PR1 child missing: %v", err)
	}

	if err := h.mgr.Handle(ctx, prEvent("opened", 2, "b2", repo, false)); err != nil {
		t.Fatalf("Handle PR2: %v", err)
	}
	if _, err := h.st.GetPreviewByPR(ctx, parent.ID, 2); err == nil {
		t.Fatal("PR2 should have been skipped by max concurrent")
	}

	got := h.gh.comments[repo+"#2"]
	if len(got) != 1 {
		t.Fatalf("PR2 comments = %v, want one max-concurrent message", got)
	}
	if !strings.Contains(got[0], "max concurrent") {
		t.Errorf("PR2 comment = %q, want a max concurrent notice", got[0])
	}
}

func TestClosedTearsDown(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	repo := "acme/web"
	parent := h.seedGithubApp(t, "web", repo, nil)
	h.seedAttachment(t, parent, "web-db", "DATABASE_URL")

	if err := h.mgr.Handle(ctx, prEvent("opened", 42, "feature-x", repo, false)); err != nil {
		t.Fatalf("Handle opened: %v", err)
	}
	child, err := h.st.GetPreviewByPR(ctx, parent.ID, 42)
	if err != nil {
		t.Fatalf("child missing before close: %v", err)
	}

	if err := h.mgr.Handle(ctx, prEvent("closed", 42, "feature-x", repo, false)); err != nil {
		t.Fatalf("Handle closed: %v", err)
	}

	if _, err := h.st.GetPreviewByPR(ctx, parent.ID, 42); err == nil {
		t.Fatal("child app should be gone after close")
	}
	if !h.docker.removed[child.Name] {
		t.Errorf("docker.RemoveApp not called for %q", child.Name)
	}
	if h.dbprov.destroyed != 1 {
		t.Errorf("destroyed = %d, want 1", h.dbprov.destroyed)
	}

	got := h.gh.comments[repo+"#42"]
	if len(got) == 0 || got[len(got)-1] != "Preview environment destroyed." {
		t.Errorf("teardown comment = %v, want a \"Preview environment destroyed.\" notice", got)
	}
}

func TestSynchronizeRedeploys(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	repo := "acme/web"
	parent := h.seedGithubApp(t, "web", repo, nil)

	if err := h.mgr.Handle(ctx, prEvent("opened", 42, "feature-x", repo, false)); err != nil {
		t.Fatalf("Handle opened: %v", err)
	}
	child, err := h.st.GetPreviewByPR(ctx, parent.ID, 42)
	if err != nil {
		t.Fatalf("child missing: %v", err)
	}

	if err := h.mgr.Handle(ctx, prEvent("synchronize", 42, "feature-x", repo, false)); err != nil {
		t.Fatalf("Handle synchronize: %v", err)
	}

	// No second child.
	children, err := h.st.ListPreviewsForParent(ctx, parent.ID)
	if err != nil {
		t.Fatalf("ListPreviewsForParent: %v", err)
	}
	if len(children) != 1 {
		t.Fatalf("children = %d, want 1", len(children))
	}

	// A second deployment enqueued on the same child.
	deps, err := h.st.ListDeploymentsForApp(ctx, child.ID)
	if err != nil {
		t.Fatalf("ListDeploymentsForApp: %v", err)
	}
	if len(deps) != 2 {
		t.Errorf("child deployments = %d, want 2", len(deps))
	}
}

func TestTeardownFailureLeavesAppForRetry(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	repo := "acme/web"
	parent := h.seedGithubApp(t, "web", repo, nil)
	h.seedAttachment(t, parent, "web-db", "DATABASE_URL")

	if err := h.mgr.Handle(ctx, prEvent("opened", 42, "feature-x", repo, false)); err != nil {
		t.Fatalf("Handle opened: %v", err)
	}
	child, err := h.st.GetPreviewByPR(ctx, parent.ID, 42)
	if err != nil {
		t.Fatalf("child missing before close: %v", err)
	}

	// Container removal fails: teardown must abort before deleting the app row.
	h.docker.fail = true
	if err := h.mgr.Handle(ctx, prEvent("closed", 42, "feature-x", repo, false)); err != nil {
		t.Fatalf("Handle closed: %v", err)
	}

	// (a) The app row survives so the sweeper can retry.
	after, err := h.st.GetPreviewByPR(ctx, parent.ID, 42)
	if err != nil {
		t.Fatalf("child app should survive a failed teardown: %v", err)
	}
	if after.ID != child.ID {
		t.Fatalf("surviving child id = %d, want %d", after.ID, child.ID)
	}
	// (b) It is marked teardown_failed.
	if after.PreviewStatus != core.PreviewTeardownFailed {
		t.Errorf("child status = %q, want %q", after.PreviewStatus, core.PreviewTeardownFailed)
	}
	// (c) The attachment (and thus the resource wiring) is still present, so a
	// retry re-runs teardown against the same resources rather than orphaning.
	atts, err := h.st.ListAttachments(ctx, child.ID)
	if err != nil {
		t.Fatalf("ListAttachments child: %v", err)
	}
	if len(atts) != 1 {
		t.Errorf("child attachments = %d, want 1 (preserved for retry)", len(atts))
	}
}

func TestSpawnRollsBackOnFailure(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	repo := "acme/web"
	parent := h.seedGithubApp(t, "web", repo, nil)
	h.seedAttachment(t, parent, "web-db", "DATABASE_URL") // makes provisionDatabases run

	h.dbprov.failProvision = true
	if err := h.mgr.Handle(ctx, prEvent("opened", 42, "feature-x", repo, false)); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	// (a) The child was rolled back — no dangling row.
	if _, err := h.st.GetPreviewByPR(ctx, parent.ID, 42); err == nil {
		t.Fatal("child should have been rolled back after a failed spawn")
	}
	// (b) No deploy was enqueued and the worker was not woken.
	if h.notifier.notified {
		t.Error("notifier should not fire when spawn fails")
	}
	// (c) No comment posted for a preview that never came up.
	if len(h.gh.comments) != 0 {
		t.Errorf("expected no PR comments on a failed spawn, got %v", h.gh.comments)
	}
}

func TestSpawnDropsProdScopedEnv(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	repo := "acme/web"
	parent := h.seedGithubApp(t, "web", repo, nil)
	if err := h.st.SetEnvScoped(ctx, parent.ID, "SHARED_VAR", "s", false, core.ScopeShared); err != nil {
		t.Fatalf("SetEnvScoped shared: %v", err)
	}
	if err := h.st.SetEnvScoped(ctx, parent.ID, "PROD_SECRET", "x", true, core.ScopeProd); err != nil {
		t.Fatalf("SetEnvScoped prod: %v", err)
	}

	if err := h.mgr.Handle(ctx, prEvent("opened", 42, "feature-x", repo, false)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	child, err := h.st.GetPreviewByPR(ctx, parent.ID, 42)
	if err != nil {
		t.Fatalf("child missing: %v", err)
	}

	envs, err := h.st.ListEnv(ctx, child.ID)
	if err != nil {
		t.Fatalf("ListEnv child: %v", err)
	}
	if !hasEnv(envs, "SHARED_VAR", "s") {
		t.Errorf("child env missing SHARED_VAR: %+v", envs)
	}
	if !hasEnv(envs, "OUTHAUL_PREVIEW", "true") {
		t.Errorf("child env missing OUTHAUL_PREVIEW: %+v", envs)
	}
	for _, v := range envs {
		if v.Key == "PROD_SECRET" {
			t.Errorf("prod-scoped var leaked into preview: %+v", v)
		}
	}
}

func TestSpawnSuppressesCommentWhenDisabled(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	repo := "acme/web"
	h.seedGithubApp(t, "web", repo, func(c *core.PreviewConfig) { c.PostPRComment = false })

	if err := h.mgr.Handle(ctx, prEvent("opened", 42, "feature-x", repo, false)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(h.gh.comments) != 0 {
		t.Errorf("expected no PR comments with PostPRComment=false, got %v", h.gh.comments)
	}
}

// --- small helpers ---------------------------------------------------------

func hasEnv(vars []core.EnvVar, key, val string) bool {
	for _, v := range vars {
		if v.Key == key && v.Value == val {
			return true
		}
	}
	return false
}
