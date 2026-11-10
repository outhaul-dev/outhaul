package previewmgr

import (
	"bytes"
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/outhaul-dev/outhaul/internal/core"
	"github.com/outhaul-dev/outhaul/internal/secret"
	"github.com/outhaul-dev/outhaul/internal/store"
	"github.com/outhaul-dev/outhaul/internal/webhook"
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
	hang          bool   // Provision blocks until ctx is done, like a stalled image pull
	during        func() // if set, runs at the start of each Provision
}

func (p *fakeDBProvisioner) Provision(ctx context.Context, d core.Database) (core.Database, error) {
	if p.during != nil {
		p.during()
	}
	if p.hang {
		<-ctx.Done()
		return core.Database{}, ctx.Err()
	}
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

// Destroy mirrors the real adapter: it removes the store row (in real life the
// container + data too). teardown/cleanupChild call it only after the app — and
// thus the attachment rows — are gone, so DeleteDatabase's attachment guard and
// the FK are satisfied.
func (p *fakeDBProvisioner) Destroy(ctx context.Context, db core.Database) error {
	if err := p.st.DeleteDatabase(ctx, db.ID); err != nil {
		return err
	}
	p.destroyed++
	return nil
}

// fakeCommenter records every PR comment posted, keyed by "repo#pr", and
// answers PR-state lookups from prStates ("open" when unlisted, stateErr if set).
type fakeCommenter struct {
	comments map[string][]string
	prStates map[string]string
	stateErr error
}

func (c *fakeCommenter) PullRequestState(_ context.Context, _, repo string, pr int) (string, error) {
	if c.stateErr != nil {
		return "", c.stateErr
	}
	if st, ok := c.prStates[repo+"#"+strconv.Itoa(pr)]; ok {
		return st, nil
	}
	return "open", nil
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
	st                *store.Store
	mgr               *Manager
	notifier          *fakeNotifier
	docker            *fakeDocker
	dbprov            *fakeDBProvisioner
	gh                *fakeCommenter
	sourceID          int64 // git source every seeded parent app belongs to
	lastTokenSourceID int64 // sourceID the fake TokenSource was last called with
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
	src, err := st.CreateGithubAppSource(context.Background(), core.GithubAppCreds{
		AppID: 77, Slug: "outhaul-test", PrivateKey: "PEM",
		WebhookSecret: "whs", ClientID: "cid", ClientSecret: "csec",
	})
	if err != nil {
		t.Fatalf("CreateGithubAppSource: %v", err)
	}
	if err := st.BindGithubInstallation(context.Background(), src.ID, 9001, "acme-corp", "Organization"); err != nil {
		t.Fatalf("BindGithubInstallation: %v", err)
	}
	h.sourceID = src.ID
	ts := func(_ context.Context, sourceID int64) (string, bool, error) {
		h.lastTokenSourceID = sourceID
		return "tok", true, nil
	}
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
		Name:        name,
		RepoURL:     "https://github.com/" + repo + ".git",
		Domain:      name + ".example",
		Source:      core.SourceGithub,
		GithubRepo:  repo,
		GitSourceID: h.sourceID,
		Branch:      "main",
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

	if err := h.mgr.Handle(ctx, h.sourceID, prEvent("opened", 42, "feature-x", repo, false)); err != nil {
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

func TestSpawnDedupesWholeHostDomains(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	repo := "acme/web"
	parent := h.seedGithubApp(t, "web", repo, nil)
	// A single-container app commonly has its auto sslip domain plus an added
	// custom domain: two whole-host rows (Service "" / Path ""). Both derive the
	// SAME preview host, so mintDomains must collapse them to one row instead of
	// hitting UNIQUE(app_id, host, path) and rolling the whole spawn back.
	if _, err := h.st.AddDomain(ctx, core.Domain{
		AppID: parent.ID, Host: "custom.example", Port: 8080, TLS: true,
	}); err != nil {
		t.Fatalf("AddDomain custom: %v", err)
	}
	parentDoms, _ := h.st.ListDomains(ctx, parent.ID)
	if len(parentDoms) != 2 {
		t.Fatalf("parent domains = %d, want 2 (setup)", len(parentDoms))
	}

	if err := h.mgr.Handle(ctx, h.sourceID, prEvent("opened", 42, "feature-x", repo, false)); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	// The preview spawned (not rolled back).
	child, err := h.st.GetPreviewByPR(ctx, parent.ID, 42)
	if err != nil {
		t.Fatalf("preview should have spawned despite duplicate derived hosts: %v", err)
	}
	// Exactly one deduped domain row.
	childDoms, err := h.st.ListDomains(ctx, child.ID)
	if err != nil {
		t.Fatalf("ListDomains child: %v", err)
	}
	if len(childDoms) != 1 {
		t.Fatalf("child domains = %d, want 1 (deduped)", len(childDoms))
	}
	if want := "web-pr-42.1.2.3.4.sslip.io"; childDoms[0].Host != want {
		t.Errorf("child domain host = %q, want %q", childDoms[0].Host, want)
	}
}

func TestSpawnForkPRGated(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	repo := "acme/web"
	parent := h.seedGithubApp(t, "web", repo, nil) // AllowForkPRs defaults false

	if err := h.mgr.Handle(ctx, h.sourceID, prEvent("opened", 7, "fork-branch", repo, true)); err != nil {
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

	if err := h.mgr.Handle(ctx, h.sourceID, prEvent("opened", 1, "b1", repo, false)); err != nil {
		t.Fatalf("Handle PR1: %v", err)
	}
	if _, err := h.st.GetPreviewByPR(ctx, parent.ID, 1); err != nil {
		t.Fatalf("PR1 child missing: %v", err)
	}

	if err := h.mgr.Handle(ctx, h.sourceID, prEvent("opened", 2, "b2", repo, false)); err != nil {
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

// Since synchronize spawns a missing preview, a PR stuck behind the cap would
// re-post the "skipped" notice on every push. Only opening the PR says so.
func TestCapNoticeNotRepeatedOnPush(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	repo := "acme/web"
	h.seedGithubApp(t, "web", repo, func(c *core.PreviewConfig) { c.MaxConcurrent = 1 })

	for _, ev := range []webhook.PullRequestEvent{
		prEvent("opened", 1, "b1", repo, false),
		prEvent("opened", 2, "b2", repo, false),
		prEvent("synchronize", 2, "b2", repo, false),
		prEvent("synchronize", 2, "b2", repo, false),
	} {
		if err := h.mgr.Handle(ctx, h.sourceID, ev); err != nil {
			t.Fatalf("Handle %s: %v", ev.Action, err)
		}
	}
	if got := h.gh.comments[repo+"#2"]; len(got) != 1 {
		t.Errorf("PR2 comments = %d (%v), want the single notice from opening", len(got), got)
	}
}

func TestClosedTearsDown(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	repo := "acme/web"
	parent := h.seedGithubApp(t, "web", repo, nil)
	h.seedAttachment(t, parent, "web-db", "DATABASE_URL")

	if err := h.mgr.Handle(ctx, h.sourceID, prEvent("opened", 42, "feature-x", repo, false)); err != nil {
		t.Fatalf("Handle opened: %v", err)
	}
	child, err := h.st.GetPreviewByPR(ctx, parent.ID, 42)
	if err != nil {
		t.Fatalf("child missing before close: %v", err)
	}
	childAtts, err := h.st.ListAttachments(ctx, child.ID)
	if err != nil || len(childAtts) != 1 {
		t.Fatalf("child attachments = %+v (err %v), want 1", childAtts, err)
	}
	childDBID := childAtts[0].DatabaseID

	if err := h.mgr.Handle(ctx, h.sourceID, prEvent("closed", 42, "feature-x", repo, false)); err != nil {
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
	// FK-safe ordering worked end to end: the preview's DB row is gone (it could
	// only be deleted after the app's attachment row was dropped).
	if _, err := h.st.GetDatabase(ctx, childDBID); err == nil {
		t.Error("preview database row should be deleted after teardown")
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

	if err := h.mgr.Handle(ctx, h.sourceID, prEvent("opened", 42, "feature-x", repo, false)); err != nil {
		t.Fatalf("Handle opened: %v", err)
	}
	child, err := h.st.GetPreviewByPR(ctx, parent.ID, 42)
	if err != nil {
		t.Fatalf("child missing: %v", err)
	}

	if err := h.mgr.Handle(ctx, h.sourceID, prEvent("synchronize", 42, "feature-x", repo, false)); err != nil {
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

// PRs opened before previews could fire (the App lacked pull_request events, or
// previews were off) have no child. Their next push must create one rather than
// be silently dropped until the PR is closed and reopened.
func TestSynchronizeSpawnsMissingPreview(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	repo := "acme/web"
	parent := h.seedGithubApp(t, "web", repo, nil)

	if err := h.mgr.Handle(ctx, h.sourceID, prEvent("synchronize", 42, "feature-x", repo, false)); err != nil {
		t.Fatalf("Handle synchronize: %v", err)
	}
	child, err := h.st.GetPreviewByPR(ctx, parent.ID, 42)
	if err != nil {
		t.Fatalf("synchronize without a preview created no child: %v", err)
	}
	if deps, _ := h.st.ListDeploymentsForApp(ctx, child.ID); len(deps) != 1 {
		t.Errorf("child deployments = %d, want 1", len(deps))
	}
	if !h.notifier.notified {
		t.Error("deploy worker was not woken")
	}
}

func TestSynchronizeRespectsForkGate(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	repo := "acme/web"
	parent := h.seedGithubApp(t, "web", repo, nil) // AllowForkPRs defaults false

	if err := h.mgr.Handle(ctx, h.sourceID, prEvent("synchronize", 7, "fork-branch", repo, true)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if _, err := h.st.GetPreviewByPR(ctx, parent.ID, 7); err == nil {
		t.Fatal("synchronize from a gated fork PR created a child")
	}
}

func TestTeardownFailureLeavesAppForRetry(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	repo := "acme/web"
	parent := h.seedGithubApp(t, "web", repo, nil)
	h.seedAttachment(t, parent, "web-db", "DATABASE_URL")

	if err := h.mgr.Handle(ctx, h.sourceID, prEvent("opened", 42, "feature-x", repo, false)); err != nil {
		t.Fatalf("Handle opened: %v", err)
	}
	child, err := h.st.GetPreviewByPR(ctx, parent.ID, 42)
	if err != nil {
		t.Fatalf("child missing before close: %v", err)
	}
	childAtts, err := h.st.ListAttachments(ctx, child.ID)
	if err != nil || len(childAtts) != 1 {
		t.Fatalf("child attachments = %+v (err %v), want 1", childAtts, err)
	}
	childDBID := childAtts[0].DatabaseID

	// Container removal fails: teardown must abort before deleting any rows.
	h.docker.fail = true
	if err := h.mgr.Handle(ctx, h.sourceID, prEvent("closed", 42, "feature-x", repo, false)); err != nil {
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
	// (c) The gated-failure path deleted nothing: the attachment row, the DB row,
	// and the DB itself all survive intact for a clean retry.
	atts, err := h.st.ListAttachments(ctx, child.ID)
	if err != nil {
		t.Fatalf("ListAttachments child: %v", err)
	}
	if len(atts) != 1 {
		t.Errorf("child attachments = %d, want 1 (preserved for retry)", len(atts))
	}
	if _, err := h.st.GetDatabase(ctx, childDBID); err != nil {
		t.Errorf("preview database row should survive a gated teardown failure: %v", err)
	}
	if h.dbprov.destroyed != 0 {
		t.Errorf("destroyed = %d, want 0 (nothing deleted on the gated path)", h.dbprov.destroyed)
	}
}

func TestSpawnRollsBackOnFailure(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	repo := "acme/web"
	parent := h.seedGithubApp(t, "web", repo, nil)
	h.seedAttachment(t, parent, "web-db", "DATABASE_URL") // makes provisionDatabases run

	h.dbprov.failProvision = true
	if err := h.mgr.Handle(ctx, h.sourceID, prEvent("opened", 42, "feature-x", repo, false)); err != nil {
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
	// (c) The PR is told the preview could not be created, instead of silence.
	got := h.gh.comments[repo+"#42"]
	if len(got) != 1 || !strings.Contains(got[0], "could not be created") {
		t.Errorf("PR comments = %v, want one 'could not be created' notice", got)
	}
}

// A preview host another app already serves (a custom domain, or a colliding
// [service-]app label) makes the spawn fail; the PR comment says why.
func TestSpawnExplainsHostCollision(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	repo := "acme/web"
	h.seedGithubApp(t, "web", repo, nil)
	if _, err := h.st.CreateApp(ctx, core.App{Name: "squatter", RepoURL: "https://x/y.git", Domain: "web-pr-42." + serverIP + ".sslip.io"}); err != nil {
		t.Fatal(err)
	}

	if err := h.mgr.Handle(ctx, h.sourceID, prEvent("opened", 42, "feature-x", repo, false)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	got := h.gh.comments[repo+"#42"]
	if len(got) != 1 || !strings.Contains(got[0], "already used by another app") {
		t.Errorf("PR comments = %v, want a notice that the preview host is taken", got)
	}
}

// A preview host that is Outhaul's own address is refused like a taken one:
// the spawn rolls back and the PR is told why. No other host is minted.
func TestSpawnRefusesOuthaulsOwnHost(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	repo := "acme/web"
	parent := h.seedGithubApp(t, "web", repo, nil)
	h.st.SetReservedHost("web-pr-42." + serverIP + ".sslip.io")

	if err := h.mgr.Handle(ctx, h.sourceID, prEvent("opened", 42, "feature-x", repo, false)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if _, err := h.st.GetPreviewByPR(ctx, parent.ID, 42); err == nil {
		t.Error("the preview was kept although its host was refused")
	}
	if live, _ := h.st.ListPreviewsForParent(ctx, parent.ID); len(live) != 0 {
		t.Errorf("previews = %+v, want none", live)
	}
	if h.notifier.notified {
		t.Error("a deploy was started")
	}
	got := h.gh.comments[repo+"#42"]
	if len(got) != 1 || !strings.Contains(got[0], "already used") || !strings.Contains(got[0], "Outhaul itself") {
		t.Errorf("PR comments = %v, want one saying the hostname is used by Outhaul itself", got)
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

	if err := h.mgr.Handle(ctx, h.sourceID, prEvent("opened", 42, "feature-x", repo, false)); err != nil {
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

	if err := h.mgr.Handle(ctx, h.sourceID, prEvent("opened", 42, "feature-x", repo, false)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(h.gh.comments) != 0 {
		t.Errorf("expected no PR comments with PostPRComment=false, got %v", h.gh.comments)
	}
}

func TestOnDeployFinishedPostsReadyComment(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	repo := "acme/web"
	parent := h.seedGithubApp(t, "web", repo, nil)

	if err := h.mgr.Handle(ctx, h.sourceID, prEvent("opened", 42, "feature-x", repo, false)); err != nil {
		t.Fatalf("Handle opened: %v", err)
	}
	child, err := h.st.GetPreviewByPR(ctx, parent.ID, 42)
	if err != nil {
		t.Fatalf("GetPreviewByPR: %v", err)
	}

	h.mgr.OnDeployFinished(ctx, child, true)

	after, err := h.st.GetPreviewByPR(ctx, parent.ID, 42)
	if err != nil {
		t.Fatalf("GetPreviewByPR after: %v", err)
	}
	if after.PreviewStatus != core.PreviewReady {
		t.Errorf("child status = %q, want %q", after.PreviewStatus, core.PreviewReady)
	}

	got := h.gh.comments[repo+"#42"]
	if len(got) == 0 {
		t.Fatal("expected a ready comment for the parent repo#PR")
	}
	last := got[len(got)-1]
	if !strings.Contains(last, "Preview ready") {
		t.Errorf("last comment = %q, want a \"Preview ready\" notice", last)
	}
	if !strings.Contains(last, "web-pr-42.1.2.3.4.sslip.io") {
		t.Errorf("ready comment = %q, want the preview URL", last)
	}
	if h.lastTokenSourceID != h.sourceID {
		t.Errorf("comment token minted for source %d, want the parent's source %d — a comment must never be posted with another account's token", h.lastTokenSourceID, h.sourceID)
	}
}

func TestOnDeployFinishedFailedComment(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	repo := "acme/web"
	parent := h.seedGithubApp(t, "web", repo, nil)

	if err := h.mgr.Handle(ctx, h.sourceID, prEvent("opened", 42, "feature-x", repo, false)); err != nil {
		t.Fatalf("Handle opened: %v", err)
	}
	child, err := h.st.GetPreviewByPR(ctx, parent.ID, 42)
	if err != nil {
		t.Fatalf("GetPreviewByPR: %v", err)
	}

	h.mgr.OnDeployFinished(ctx, child, false)

	after, err := h.st.GetPreviewByPR(ctx, parent.ID, 42)
	if err != nil {
		t.Fatalf("GetPreviewByPR after: %v", err)
	}
	if after.PreviewStatus != core.PreviewFailed {
		t.Errorf("child status = %q, want %q", after.PreviewStatus, core.PreviewFailed)
	}

	got := h.gh.comments[repo+"#42"]
	if len(got) == 0 {
		t.Fatal("expected a failed comment for the parent repo#PR")
	}
	if last := got[len(got)-1]; !strings.Contains(last, "Preview build failed") {
		t.Errorf("last comment = %q, want a \"Preview build failed\" notice", last)
	}
}

func TestOnDeployFinishedIgnoresNonPreview(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	repo := "acme/web"
	parent := h.seedGithubApp(t, "web", repo, nil) // non-ephemeral

	h.mgr.OnDeployFinished(ctx, parent, true)

	if len(h.gh.comments) != 0 {
		t.Errorf("expected no comments for a non-preview app, got %v", h.gh.comments)
	}
	after, err := h.st.GetApp(ctx, parent.ID)
	if err != nil {
		t.Fatalf("GetApp: %v", err)
	}
	if after.PreviewStatus != "" {
		t.Errorf("non-preview app status = %q, want unchanged (empty)", after.PreviewStatus)
	}
}

func TestDestroyByIDRejectsNonPreview(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	repo := "acme/web"
	parent := h.seedGithubApp(t, "web", repo, nil)
	other := h.seedGithubApp(t, "api", "acme/api", nil)

	if err := h.mgr.Handle(ctx, h.sourceID, prEvent("opened", 42, "feature-x", repo, false)); err != nil {
		t.Fatalf("Handle opened: %v", err)
	}
	child, err := h.st.GetPreviewByPR(ctx, parent.ID, 42)
	if err != nil {
		t.Fatalf("GetPreviewByPR: %v", err)
	}

	// (a) Target is a normal (non-ephemeral) app: reject and delete nothing.
	if err := h.mgr.DestroyByID(ctx, parent.ID, parent.ID); err == nil {
		t.Error("DestroyByID on a non-preview app should error")
	}
	if _, err := h.st.GetApp(ctx, parent.ID); err != nil {
		t.Errorf("parent app should still exist after a rejected destroy: %v", err)
	}

	// (b) Right app kind but wrong parent: still reject, delete nothing.
	if err := h.mgr.DestroyByID(ctx, other.ID, child.ID); err == nil {
		t.Error("DestroyByID with a mismatched parent should error")
	}
	if _, err := h.st.GetPreviewByPR(ctx, parent.ID, 42); err != nil {
		t.Errorf("child should survive a mismatched-parent destroy: %v", err)
	}

	// (c) The real preview under its real parent tears down: child gone.
	if err := h.mgr.DestroyByID(ctx, parent.ID, child.ID); err != nil {
		t.Fatalf("DestroyByID on a real preview: %v", err)
	}
	if _, err := h.st.GetPreviewByPR(ctx, parent.ID, 42); err == nil {
		t.Error("child should be gone after DestroyByID")
	}
	if !h.docker.removed[child.Name] {
		t.Errorf("docker.RemoveApp not called for %q", child.Name)
	}
	// A manual destroy posts the "destroyed" comment via the parent's repo (not "").
	got := h.gh.comments[repo+"#42"]
	if len(got) == 0 || got[len(got)-1] != "Preview environment destroyed." {
		t.Errorf("DestroyByID comment = %v, want a \"Preview environment destroyed.\" notice", got)
	}
}

// --- protection --------------------------------------------------------------

// fakeProxy stands in for the proxy reconcile turning sign-in on needs. at, if
// set, runs during each call. left is how long the context of the last call
// had before its deadline (0: none).
type fakeProxy struct {
	mu    sync.Mutex
	calls int
	left  time.Duration
	err   error
	at    func()
}

func (p *fakeProxy) ReconcileProxy(ctx context.Context) error {
	p.mu.Lock()
	p.calls++
	if d, ok := ctx.Deadline(); ok {
		p.left = time.Until(d)
	}
	at, err := p.at, p.err
	p.mu.Unlock()
	if at != nil {
		at()
	}
	return err
}

// seen returns calls and left.
func (p *fakeProxy) seen() (int, time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls, p.left
}

// parentRow adds a row to parent, protected or not.
func (h *harness) parentRow(t *testing.T, parent core.App, d core.Domain) core.Domain {
	t.Helper()
	d.AppID = parent.ID
	if d.Port == 0 {
		d.Port = 8080
	}
	added, err := h.st.AddDomain(context.Background(), d)
	if err != nil {
		t.Fatalf("AddDomain %s%s: %v", d.Host, d.Path, err)
	}
	return added
}

// protectPrimary turns sign-in on for parent's seeded row (its Domain).
func (h *harness) protectPrimary(t *testing.T, parent core.App) {
	t.Helper()
	ctx := context.Background()
	rows, err := h.st.ListDomains(ctx, parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range rows {
		if d.Host == parent.Domain && d.Path == "" {
			if err := h.st.SetDomainProtected(ctx, parent.ID, d.ID, true); err != nil {
				t.Fatal(err)
			}
			return
		}
	}
	t.Fatalf("parent %s has no row for %s", parent.Name, parent.Domain)
}

// previewRows returns the preview's rows by path ("" for the whole host).
func (h *harness) previewRows(t *testing.T, parent core.App, pr int) map[string]core.Domain {
	t.Helper()
	ctx := context.Background()
	child, err := h.st.GetPreviewByPR(ctx, parent.ID, pr)
	if err != nil {
		t.Fatalf("no preview for PR %d: %v", pr, err)
	}
	rows, err := h.st.ListDomains(ctx, child.ID)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]core.Domain{}
	for _, d := range rows {
		out[d.Path] = d
	}
	return out
}

// A preview's row requires sign-in when its parent's row does, a fork's
// preview included. The proxy is reconciled with the rows saved and before
// the preview's first deployment is queued, as turning sign-in on in the
// dialog does, so Traefik can reach the gate.
func TestMintDomainsCopiesProtection(t *testing.T) {
	for _, fork := range []bool{false, true} {
		name := "same repo"
		if fork {
			name = "fork"
		}
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			ctx := context.Background()
			px := &fakeProxy{}
			h.mgr.SetProxyControl(px)
			parent := h.seedGithubApp(t, "web", "acme/web", allowForks)
			h.protectPrimary(t, parent)
			h.parentRow(t, parent, core.Domain{Host: parent.Domain, Path: "/hooks"})
			// What the preview's own rows and deployments were when the
			// proxy was reconciled.
			deploysAt, savedAt := -1, false
			px.at = func() {
				child, err := h.st.GetPreviewByPR(ctx, parent.ID, 42)
				if err != nil {
					return
				}
				deps, _ := h.st.ListDeploymentsForApp(ctx, child.ID)
				deploysAt = len(deps)
				rows, _ := h.st.ListDomains(ctx, child.ID)
				savedAt = len(rows) == 2
				for _, d := range rows {
					savedAt = savedAt && d.Protected == (d.Path == "")
				}
			}

			if err := h.mgr.Handle(ctx, h.sourceID, prEvent("opened", 42, "feature-x", "acme/web", fork)); err != nil {
				t.Fatalf("Handle: %v", err)
			}
			rows := h.previewRows(t, parent, 42)
			if len(rows) != 2 || !rows[""].Protected || rows["/hooks"].Protected {
				t.Errorf("preview rows = %+v, want the whole host protected and /hooks open", rows)
			}
			if calls, left := px.seen(); calls != 1 || deploysAt != 0 || !savedAt || left <= 0 || left > defaultGateHostWait {
				t.Errorf("reconciles = %d, deployments then = %d, preview's rows saved then = %v, deadline in %v; want 1, 0, true, at most %v",
					calls, deploysAt, savedAt, left, defaultGateHostWait)
			}
		})
	}
}

// A preview of a parent with no protected row asks for no reconcile.
func TestOpenPreviewNeedsNoReconcile(t *testing.T) {
	h := newHarness(t)
	px := &fakeProxy{}
	h.mgr.SetProxyControl(px)
	parent := h.seedGithubApp(t, "web", "acme/web", nil)
	if err := h.mgr.Handle(context.Background(), h.sourceID, prEvent("opened", 42, "feature-x", "acme/web", false)); err != nil {
		t.Fatal(err)
	}
	if rows := h.previewRows(t, parent, 42); rows[""].Protected {
		t.Error("an open parent's preview requires sign-in")
	}
	if px.calls != 0 {
		t.Errorf("reconciles = %d, want 0", px.calls)
	}
}

// When the proxy can't be reconciled now (the reconcile fails, there is no
// proxy, or the reconcile doesn't finish in time), a preview that requires
// sign-in is still made, protected, and deployed: the parent's protected row
// normally keeps the gate's host mapped. The log says so, and what the
// mapping is for. The spawn doesn't wait for the reconcile longer than
// gateHostWait, so it holds the queue no longer.
func TestProtectedPreviewSpawnsWhenTheProxyCantBeUpdated(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	for _, c := range []struct {
		name  string
		proxy *fakeProxy
	}{
		{"the reconcile fails", &fakeProxy{err: errors.New("update the proxy (Traefik): docker is down")}},
		{"no proxy", nil},
		{"the reconcile doesn't finish in time", &fakeProxy{at: func() { <-release }}},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			ctx := context.Background()
			h.mgr.gateHostWait = 50 * time.Millisecond
			if c.proxy != nil {
				h.mgr.SetProxyControl(c.proxy)
			}
			var logged bytes.Buffer
			log.SetOutput(&logged)
			t.Cleanup(func() { log.SetOutput(os.Stderr) })
			parent := h.seedGithubApp(t, "web", "acme/web", nil)
			h.protectPrimary(t, parent)
			start := time.Now()
			if err := h.mgr.Handle(ctx, h.sourceID, prEvent("opened", 42, "feature-x", "acme/web", false)); err != nil {
				t.Fatal(err)
			}
			if took := time.Since(start); took > 10*time.Second {
				t.Errorf("the spawn took %v, waiting out the reconcile", took)
			}
			if rows := h.previewRows(t, parent, 42); !rows[""].Protected {
				t.Errorf("preview rows = %+v, want the whole host protected", rows)
			}
			if !h.notifier.notified {
				t.Error("no deploy was started")
			}
			if got := h.gh.comments["acme/web#42"]; len(got) != 1 || strings.Contains(got[0], "could not be created") {
				t.Errorf("PR comments = %v, want one saying the preview is building", got)
			}
			if c.proxy != nil {
				if _, left := c.proxy.seen(); left <= 0 || left > h.mgr.gateHostWait {
					t.Errorf("the reconcile's deadline was in %v, want at most %v", left, h.mgr.gateHostWait)
				}
			}
			const why = "deploying it anyway: its parent's protected row normally keeps the gate's host mapped to Outhaul; without that, Traefik would look the gate's host name up on the Docker network"
			if !strings.Contains(logged.String(), why) {
				t.Errorf("the log should say %q; got:\n%s", why, logged.String())
			}
		})
	}
}

// A save of the parent's domains can give a preview its protection while it
// is being made (protectPreviews adds or turns on the preview's row) before
// the preview manager saves the rows it minted. The minted row then merges
// with that one, requiring sign-in if either does, and the preview is kept.
func TestSaveDuringSpawnKeepsThePreview(t *testing.T) {
	const host = "web-pr-42.1.2.3.4.sslip.io"
	for _, c := range []struct {
		name                  string
		parentBefore, protect bool // the parent's row is protected before the spawn; protect it during
		row                   bool // the row added during the spawn is protected
	}{
		{"a save protects the parent and the preview", false, true, true},
		{"an open row already there is turned on", true, false, false},
		{"a protected row already there stays on", false, false, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			ctx := context.Background()
			h.mgr.SetProxyControl(&fakeProxy{})
			parent := h.seedGithubApp(t, "web", "acme/web", nil)
			h.seedAttachment(t, parent, "webdb", "DATABASE_URL")
			if c.parentBefore {
				h.protectPrimary(t, parent)
			}
			h.dbprov.during = func() {
				child, err := h.st.GetPreviewByPR(ctx, parent.ID, 42)
				if err != nil {
					t.Fatalf("no preview while it is being made: %v", err)
				}
				if c.protect {
					h.protectPrimary(t, parent)
				}
				if _, err := h.st.AddDomain(ctx, core.Domain{AppID: child.ID, Host: host, Port: 8080, Protected: c.row}); err != nil {
					t.Fatal(err)
				}
			}
			if err := h.mgr.Handle(ctx, h.sourceID, prEvent("opened", 42, "feature-x", "acme/web", false)); err != nil {
				t.Fatal(err)
			}
			if rows := h.previewRows(t, parent, 42); len(rows) != 1 || rows[""].Host != host || !rows[""].Protected {
				t.Errorf("preview rows = %+v, want one, on %s, protected", rows, host)
			}
			if !h.notifier.notified {
				t.Error("no deploy was started")
			}
		})
	}
}

// Several parent rows can collapse into one preview row (the preview host
// derives only from the service, path and PR). It requires sign-in when any
// of them does, whichever comes first.
func TestCollapsedPreviewRowIsProtectedIfAnyParentRowIs(t *testing.T) {
	for _, other := range []string{"a-custom.example", "z-custom.example"} {
		t.Run(other, func(t *testing.T) {
			h := newHarness(t)
			ctx := context.Background()
			h.mgr.SetProxyControl(&fakeProxy{})
			parent := h.seedGithubApp(t, "web", "acme/web", nil)
			shut := h.parentRow(t, parent, core.Domain{Host: other})
			if err := h.st.SetDomainProtected(ctx, parent.ID, shut.ID, true); err != nil {
				t.Fatal(err)
			}
			if err := h.mgr.Handle(ctx, h.sourceID, prEvent("opened", 42, "feature-x", "acme/web", false)); err != nil {
				t.Fatal(err)
			}
			rows := h.previewRows(t, parent, 42)
			if len(rows) != 1 || !rows[""].Protected {
				t.Errorf("preview rows = %+v, want one row, protected", rows)
			}
		})
	}
}

// Turning a parent's row off leaves its previews as they are; a preview
// made after that is open.
func TestTurningParentOffLeavesPreviews(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.mgr.SetProxyControl(&fakeProxy{})
	parent := h.seedGithubApp(t, "web", "acme/web", nil)
	h.protectPrimary(t, parent)
	if err := h.mgr.Handle(ctx, h.sourceID, prEvent("opened", 42, "feature-x", "acme/web", false)); err != nil {
		t.Fatal(err)
	}
	rows, _ := h.st.ListDomains(ctx, parent.ID)
	if err := h.st.SetDomainProtected(ctx, parent.ID, rows[0].ID, false); err != nil {
		t.Fatal(err)
	}
	if err := h.mgr.Handle(ctx, h.sourceID, prEvent("synchronize", 42, "feature-x", "acme/web", false)); err != nil {
		t.Fatal(err)
	}
	if err := h.mgr.Handle(ctx, h.sourceID, prEvent("opened", 43, "feature-y", "acme/web", false)); err != nil {
		t.Fatal(err)
	}
	if !h.previewRows(t, parent, 42)[""].Protected {
		t.Error("the existing preview was opened, or a new commit opened it")
	}
	if h.previewRows(t, parent, 43)[""].Protected {
		t.Error("a preview made after the parent was opened requires sign-in")
	}
}

// A preview's links (OUTHAUL_PREVIEW_URL and the PR comment) use the rule the
// pages use: https when the row's TLS flag is on, or when it is protected and
// protected rows are served over HTTPS (a TLS server, or a tunnel).
func TestPreviewLinksFollowProtection(t *testing.T) {
	for _, c := range []struct {
		name           string
		protected      bool
		protectedHTTPS bool
		scheme         string
	}{
		{"protected, served over HTTPS", true, true, "https://"},
		{"protected, nothing serves HTTPS", true, false, "http://"},
		{"open, TLS off", false, true, "http://"},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			ctx := context.Background()
			h.mgr.SetProxyControl(&fakeProxy{})
			h.mgr.SetProtectedHTTPS(func(context.Context) bool { return c.protectedHTTPS })
			parent := h.seedGithubApp(t, "web", "acme/web", nil)
			rows, _ := h.st.ListDomains(ctx, parent.ID)
			d := rows[0]
			d.TLS, d.Protected = false, c.protected
			if err := h.st.UpdateDomain(ctx, d); err != nil {
				t.Fatal(err)
			}
			if err := h.mgr.Handle(ctx, h.sourceID, prEvent("opened", 42, "feature-x", "acme/web", false)); err != nil {
				t.Fatal(err)
			}
			child, err := h.st.GetPreviewByPR(ctx, parent.ID, 42)
			if err != nil {
				t.Fatal(err)
			}
			want := c.scheme + "web-pr-42.1.2.3.4.sslip.io"
			envs, _ := h.st.ListEnv(ctx, child.ID)
			if !hasEnv(envs, "OUTHAUL_PREVIEW_URL", want) {
				t.Errorf("OUTHAUL_PREVIEW_URL = %+v, want %s", envs, want)
			}
			h.mgr.OnDeployFinished(ctx, child, true)
			got := h.gh.comments["acme/web#42"]
			line := "- " + want + "\n"
			if c.protected {
				line = "- " + want + " (requires sign-in)\n"
			}
			if len(got) == 0 || !strings.Contains(got[len(got)-1], line) {
				t.Errorf("ready comment = %q, want the line %q", got, line)
			}
		})
	}
}

// The ready comment links each of the preview's routes, path included, and
// says which require sign-in: here only /admin does.
func TestReadyCommentSaysWhichLinksRequireSignIn(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.mgr.SetProxyControl(&fakeProxy{})
	h.mgr.SetProtectedHTTPS(func(context.Context) bool { return true })
	parent := h.seedGithubApp(t, "web", "acme/web", nil)
	rows, _ := h.st.ListDomains(ctx, parent.ID)
	d := rows[0]
	d.TLS = false
	if err := h.st.UpdateDomain(ctx, d); err != nil {
		t.Fatal(err)
	}
	h.parentRow(t, parent, core.Domain{Host: parent.Domain, Path: "/admin", Protected: true})
	if err := h.mgr.Handle(ctx, h.sourceID, prEvent("opened", 42, "feature-x", "acme/web", false)); err != nil {
		t.Fatal(err)
	}
	child, err := h.st.GetPreviewByPR(ctx, parent.ID, 42)
	if err != nil {
		t.Fatal(err)
	}
	h.mgr.OnDeployFinished(ctx, child, true)
	got := h.gh.comments["acme/web#42"]
	want := "- http://web-pr-42.1.2.3.4.sslip.io\n- https://web-pr-42.1.2.3.4.sslip.io/admin (requires sign-in)\n"
	if len(got) == 0 || !strings.Contains(got[len(got)-1], want) {
		t.Errorf("ready comment = %q, want the links %q", got, want)
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

// A child is a copy of its parent; inheriting the parent's webhook token would
// give two apps the same /webhooks/app/<token> URL, and the lookup could route
// the parent's deliveries to a preview.
func TestSpawnDoesNotCopyWebhookCredentials(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	parent, err := h.st.CreateApp(ctx, core.App{
		Name: "web", RepoURL: "https://github.com/acme/web.git", Domain: "web.example",
		Source: core.SourceGithub, GithubRepo: "acme/web", GitSourceID: h.sourceID, Branch: "main",
		WebhookToken: "parent-token", WebhookSecret: "parent-secret",
	})
	if err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	cfg := core.DefaultPreviewConfig(parent.ID)
	cfg.Enabled = true
	if err := h.st.SetPreviewConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}

	if err := h.mgr.Handle(ctx, h.sourceID, prEvent("opened", 42, "feature-x", "acme/web", false)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	child, err := h.st.GetPreviewByPR(ctx, parent.ID, 42)
	if err != nil {
		t.Fatalf("child missing: %v", err)
	}
	if child.WebhookToken != "" || child.WebhookSecret != "" {
		t.Errorf("child webhook token=%q secret=%q, want both empty", child.WebhookToken, child.WebhookSecret)
	}
	if got, err := h.st.AppByWebhookToken(ctx, "parent-token"); err != nil || got.ID != parent.ID {
		t.Errorf("parent token resolves to %+v (err %v), want the parent", got, err)
	}
}

// flakyPreviewLookup fails GetPreviewByPR, standing in for a transient DB error.
type flakyPreviewLookup struct{ *store.Store }

func (flakyPreviewLookup) GetPreviewByPR(context.Context, int64, int) (core.App, error) {
	return core.App{}, context.DeadlineExceeded
}

// DestroyByID already holds the child. Re-finding it by PR number (and treating
// a failed lookup as "nothing to destroy") let a transient error report success
// with the preview still running — and a parent delete then went ahead.
func TestDestroyByIDDestroysTheChildItHolds(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	parent := h.seedGithubApp(t, "web", "acme/web", nil)
	if err := h.mgr.Handle(ctx, h.sourceID, prEvent("opened", 42, "feature-x", "acme/web", false)); err != nil {
		t.Fatal(err)
	}
	child, err := h.st.GetPreviewByPR(ctx, parent.ID, 42)
	if err != nil {
		t.Fatal(err)
	}
	h.mgr.store = flakyPreviewLookup{h.st}

	err = h.mgr.DestroyByID(ctx, parent.ID, child.ID)
	if _, gone := h.st.GetApp(ctx, child.ID); err == nil && gone == nil {
		t.Fatal("DestroyByID reported success but the preview still exists")
	}
	if !h.docker.removed[child.Name] {
		t.Errorf("containers for %s were not removed", child.Name)
	}
}
