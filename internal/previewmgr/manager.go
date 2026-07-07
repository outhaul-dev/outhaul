// Package previewmgr orchestrates GitHub-PR preview environments: spawning an
// ephemeral child app per PR, redeploying it on new commits, and tearing it
// down when the PR closes. It creates rows and enqueues deployments through the
// normal pipeline; it never builds or runs containers itself.
package previewmgr

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"time"

	"github.com/james-smart/outhaul/internal/core"
	"github.com/james-smart/outhaul/internal/dbaas"
	"github.com/james-smart/outhaul/internal/webhook"
)

// Store is the slice of *store.Store the manager needs.
type Store interface {
	AppsByGithubRepo(ctx context.Context, repo string) ([]core.App, error)
	GetApp(ctx context.Context, id int64) (core.App, error)
	GetPreviewConfig(ctx context.Context, appID int64) (core.PreviewConfig, error)
	GetPreviewByPR(ctx context.Context, parentID int64, pr int) (core.App, error)
	ListPreviewsForParent(ctx context.Context, parentID int64) ([]core.App, error)
	ListPreviews(ctx context.Context) ([]core.App, error)
	LastDeploymentAt(ctx context.Context, appID int64) (time.Time, bool, error)
	CreateApp(ctx context.Context, app core.App) (core.App, error)
	SetPreviewStatus(ctx context.Context, appID int64, status string) error
	ListEnv(ctx context.Context, appID int64) ([]core.EnvVar, error)
	SetEnvScoped(ctx context.Context, appID int64, key, value string, isSecret bool, scope string) error
	ListAttachments(ctx context.Context, appID int64) ([]core.Attachment, error)
	AttachDatabase(ctx context.Context, appID, databaseID int64, envVar string) (core.Attachment, error)
	GetDatabase(ctx context.Context, id int64) (core.Database, error)
	ListDomains(ctx context.Context, appID int64) ([]core.Domain, error)
	AddDomain(ctx context.Context, d core.Domain) (core.Domain, error)
	CreateDeployment(ctx context.Context, appID int64) (core.Deployment, error)
	DeleteApp(ctx context.Context, id int64) error
}

// Notifier wakes the deploy worker.
type Notifier interface{ Notify() }

// DBProvisioner creates and destroys the isolated databases previews use.
//
// Destroy removes the preview's database container, data, and store row.
// teardown calls it only AFTER the child app (and thus its attachment rows) is
// deleted, so the row delete is FK-safe.
type DBProvisioner interface {
	Provision(ctx context.Context, d core.Database) (core.Database, error)
	Destroy(ctx context.Context, d core.Database) error
}

// GithubCommenter posts the sticky PR comment.
type GithubCommenter interface {
	UpsertPRComment(ctx context.Context, token, repo string, pr int, body string) error
}

// TokenSource yields an installation token for API calls (nil disables comments).
type TokenSource func(ctx context.Context) (token string, ok bool, err error)

// Docker tears down a preview's containers/stack.
type Docker interface {
	RemoveApp(ctx context.Context, appName string) error
}

// Manager owns preview lifecycle.
type Manager struct {
	store    Store
	notifier Notifier
	dbprov   DBProvisioner
	docker   Docker
	gh       GithubCommenter
	token    TokenSource
	serverIP string
}

func New(st Store, n Notifier, db DBProvisioner, dk Docker, gh GithubCommenter, ts TokenSource, serverIP string) *Manager {
	return &Manager{store: st, notifier: n, dbprov: db, docker: dk, gh: gh, token: ts, serverIP: serverIP}
}

// Handle routes one pull_request event to the right lifecycle action for every
// enabled app targeting the PR's base repo.
func (m *Manager) Handle(ctx context.Context, ev webhook.PullRequestEvent) error {
	apps, err := m.store.AppsByGithubRepo(ctx, ev.BaseRepoFullName)
	if err != nil {
		return err
	}
	for _, app := range apps {
		if app.Ephemeral {
			continue // never nest previews of previews
		}
		cfg, err := m.store.GetPreviewConfig(ctx, app.ID)
		if err != nil {
			return err
		}
		if !cfg.Enabled {
			continue
		}
		if err := m.handleApp(ctx, app, cfg, ev); err != nil {
			log.Printf("previewmgr: app %d PR %d %s: %v", app.ID, ev.Number, ev.Action, err)
		}
	}
	return nil
}

func (m *Manager) handleApp(ctx context.Context, parent core.App, cfg core.PreviewConfig, ev webhook.PullRequestEvent) error {
	switch ev.Action {
	case "opened", "reopened":
		return m.spawn(ctx, parent, cfg, ev)
	case "synchronize":
		return m.redeploy(ctx, parent, ev)
	case "closed":
		return m.teardown(ctx, parent, ev.Number, ev.BaseRepoFullName, cfg)
	}
	return nil
}

func (m *Manager) spawn(ctx context.Context, parent core.App, cfg core.PreviewConfig, ev webhook.PullRequestEvent) error {
	if ev.IsFork && !cfg.AllowForkPRs {
		return nil
	}
	if _, err := m.store.GetPreviewByPR(ctx, parent.ID, ev.Number); err == nil {
		return m.redeploy(ctx, parent, ev)
	}
	live, err := m.store.ListPreviewsForParent(ctx, parent.ID)
	if err != nil {
		return err
	}
	if len(live) >= cfg.MaxConcurrent {
		m.comment(ctx, cfg, ev.BaseRepoFullName, ev.Number,
			fmt.Sprintf("Preview skipped: max concurrent previews (%d) reached.", cfg.MaxConcurrent))
		return nil
	}

	child := parent
	child.ID = 0
	child.Name = core.PreviewAppName(parent.Name, ev.Number)
	child.Branch = ev.HeadRef
	child.Domain = ""
	child.ParentID = parent.ID
	child.PRNumber = ev.Number
	child.Ephemeral = true
	child.PreviewStatus = core.PreviewBuilding
	child, err = m.store.CreateApp(ctx, child)
	if err != nil {
		return err
	}

	// Any failure after the app row exists rolls the child back, so a later
	// re-open (which routes to redeploy) never inherits a half-built preview.
	if err := m.buildPreview(ctx, parent, child, cfg, ev); err != nil {
		m.cleanupChild(ctx, child)
		return err
	}

	m.notifier.Notify()
	m.comment(ctx, cfg, ev.BaseRepoFullName, ev.Number, buildingComment(ev.HeadSHA))
	return nil
}

// buildPreview populates a freshly-created child app: env, isolated databases,
// domain rows, the preview URL var, and the first deployment. It returns the
// first error; the caller rolls the child back on failure.
func (m *Manager) buildPreview(ctx context.Context, parent, child core.App, cfg core.PreviewConfig, ev webhook.PullRequestEvent) error {
	if err := m.copyEnv(ctx, parent.ID, child.ID, ev.Number); err != nil {
		return err
	}
	if err := m.provisionDatabases(ctx, parent, child); err != nil {
		return err
	}
	if err := m.mintDomains(ctx, parent, child, cfg, ev.Number); err != nil {
		return err
	}
	if err := m.injectPreviewURL(ctx, child.ID); err != nil {
		return err
	}
	if _, err := m.store.CreateDeployment(ctx, child.ID); err != nil {
		return err
	}
	return nil
}

// cleanupChild best-effort destroys a partially-built preview: its provisioned
// databases (via current attachments) and its app row. Used to roll back a
// spawn that failed after the child app row was created. Like teardown, it
// captures the DBs while attachments exist, deletes the app row (dropping the
// attachment rows), then destroys the now-unreferenced DB rows FK-safely.
func (m *Manager) cleanupChild(ctx context.Context, child core.App) {
	atts, _ := m.store.ListAttachments(ctx, child.ID)
	dbs := make([]core.Database, 0, len(atts))
	for _, a := range atts {
		if db, err := m.store.GetDatabase(ctx, a.DatabaseID); err == nil {
			dbs = append(dbs, db)
		}
	}
	if err := m.store.DeleteApp(ctx, child.ID); err != nil {
		log.Printf("previewmgr: rollback delete app %s: %v", child.Name, err)
	}
	for _, db := range dbs {
		if err := m.dbprov.Destroy(ctx, db); err != nil {
			log.Printf("previewmgr: rollback destroy db %s: %v", db.Name, err)
		}
	}
}

// copyEnv copies the parent's non-prod env into the child (shared+preview),
// preserving scope. Prod-only vars are dropped here AND again at deploy time.
// It also injects the preview marker vars OUTHAUL_PREVIEW and OUTHAUL_PR_NUMBER.
func (m *Manager) copyEnv(ctx context.Context, parentID, childID int64, pr int) error {
	vars, err := m.store.ListEnv(ctx, parentID)
	if err != nil {
		return err
	}
	for _, v := range core.EnvForScope(vars, true) {
		if err := m.store.SetEnvScoped(ctx, childID, v.Key, v.Value, v.IsSecret, v.Scope); err != nil {
			return err
		}
	}
	_ = m.store.SetEnvScoped(ctx, childID, "OUTHAUL_PREVIEW", "true", false, core.ScopeShared)
	_ = m.store.SetEnvScoped(ctx, childID, "OUTHAUL_PR_NUMBER", strconv.Itoa(pr), false, core.ScopeShared)
	return nil
}

// injectPreviewURL sets OUTHAUL_PREVIEW_URL from the child's first minted domain
// (scheme per that domain's TLS). Apps with no routed domain get no URL var.
func (m *Manager) injectPreviewURL(ctx context.Context, childID int64) error {
	doms, err := m.store.ListDomains(ctx, childID)
	if err != nil {
		return err
	}
	if len(doms) == 0 {
		return nil
	}
	scheme := "http://"
	if doms[0].TLS {
		scheme = "https://"
	}
	return m.store.SetEnvScoped(ctx, childID, "OUTHAUL_PREVIEW_URL", scheme+doms[0].Host, false, core.ScopeShared)
}

// provisionDatabases creates a fresh empty database per parent attachment and
// attaches it to the child under the same env var. Compose apps whose DB is a
// stack service have no attachments and get isolation from the recreated stack.
func (m *Manager) provisionDatabases(ctx context.Context, parent, child core.App) error {
	atts, err := m.store.ListAttachments(ctx, parent.ID)
	if err != nil {
		return err
	}
	for _, a := range atts {
		src, err := m.store.GetDatabase(ctx, a.DatabaseID)
		if err != nil {
			return err
		}
		fresh := core.Database{
			ProjectID: child.ProjectID,
			Name:      fmt.Sprintf("%s-%s", child.Name, src.Name),
			Engine:    src.Engine,
			Image:     src.Image,
			Username:  src.Username,
			Password:  dbaas.NewPassword(),
			DBName:    src.DBName,
		}
		created, err := m.dbprov.Provision(ctx, fresh)
		if err != nil {
			return err
		}
		if _, err := m.store.AttachDatabase(ctx, child.ID, created.ID, a.EnvVar); err != nil {
			_ = m.dbprov.Destroy(ctx, created) // don't leak the just-created DB
			return err
		}
	}
	return nil
}

// mintDomains creates a preview domain row per routed parent domain (compose:
// one per service; single-container: the primary row). Internal-only services
// keep no row.
func (m *Manager) mintDomains(ctx context.Context, parent, child core.App, cfg core.PreviewConfig, pr int) error {
	parentDomains, err := m.store.ListDomains(ctx, parent.ID)
	if err != nil {
		return err
	}
	for _, d := range parentDomains {
		host := core.PreviewHost(cfg, parent.Name, pr, d.Service, m.serverIP)
		if _, err := m.store.AddDomain(ctx, core.Domain{
			AppID: child.ID, Host: host, Service: d.Service, Port: d.Port,
			Path: d.Path, InternalPath: d.InternalPath, TLS: d.TLS,
		}); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) redeploy(ctx context.Context, parent core.App, ev webhook.PullRequestEvent) error {
	child, err := m.store.GetPreviewByPR(ctx, parent.ID, ev.Number)
	if err != nil {
		return nil
	}
	if _, err := m.store.CreateDeployment(ctx, child.ID); err != nil {
		return err
	}
	m.notifier.Notify()
	cfg, _ := m.store.GetPreviewConfig(ctx, parent.ID)
	m.comment(ctx, cfg, ev.BaseRepoFullName, ev.Number, buildingComment(ev.HeadSHA))
	return nil
}

// teardown removes a preview's containers, databases, domains, and app row.
//
// Ordering matters: attachments.database_id references databases with no cascade,
// so a database row can only be deleted once its attachment row is gone. We
// therefore (1) capture the DBs while attachments still exist, (2) do the
// failure-prone container teardown behind a gate — on any failure we leave every
// row intact and mark teardown_failed for a clean retry — and only then (3)
// DeleteApp (which drops the attachment rows) before destroying the DB rows, so
// each Destroy is FK-safe.
func (m *Manager) teardown(ctx context.Context, parent core.App, pr int, repo string, cfg core.PreviewConfig) error {
	child, err := m.store.GetPreviewByPR(ctx, parent.ID, pr)
	if err != nil {
		return nil
	}

	failed := false
	// Capture the databases to destroy while the attachment rows still exist.
	atts, _ := m.store.ListAttachments(ctx, child.ID)
	dbs := make([]core.Database, 0, len(atts))
	for _, a := range atts {
		db, err := m.store.GetDatabase(ctx, a.DatabaseID)
		if err != nil {
			log.Printf("previewmgr: lookup db %d for teardown: %v", a.DatabaseID, err)
			failed = true
			continue
		}
		dbs = append(dbs, db)
	}

	if err := m.docker.RemoveApp(ctx, child.Name); err != nil {
		log.Printf("previewmgr: remove containers for %s: %v", child.Name, err)
		failed = true
	}

	// Gated phase: if anything failed, leave the app row, attachment rows, and
	// DB rows all intact so a retry re-runs from a clean state.
	if failed {
		_ = m.store.SetPreviewStatus(ctx, child.ID, core.PreviewTeardownFailed)
		return fmt.Errorf("teardown of %s had failures; left for retry", child.Name)
	}

	// FK-safe row deletion: DeleteApp removes the attachment rows first, then the
	// now-unreferenced DB rows can be destroyed.
	if err := m.store.DeleteApp(ctx, child.ID); err != nil {
		return err
	}
	for _, db := range dbs {
		if err := m.dbprov.Destroy(ctx, db); err != nil {
			// The app is already gone; a leftover DB container is a rare, logged
			// edge and must not fail the whole teardown.
			log.Printf("previewmgr: destroy db %s after app delete: %v", db.Name, err)
		}
	}
	m.comment(ctx, cfg, repo, pr, "Preview environment destroyed.")
	return nil
}

// DestroyByID tears down a single preview child app by id (manual destroy from
// the UI). parentID guards that childID really is a preview of that parent.
func (m *Manager) DestroyByID(ctx context.Context, parentID, childID int64) error {
	child, err := m.store.GetApp(ctx, childID)
	if err != nil {
		return err
	}
	if child.ParentID != parentID || !child.Ephemeral {
		return fmt.Errorf("app %d is not a preview of app %d", childID, parentID)
	}
	parent, err := m.store.GetApp(ctx, parentID)
	if err != nil {
		return err
	}
	cfg, err := m.store.GetPreviewConfig(ctx, parentID)
	if err != nil {
		return err
	}
	return m.teardown(ctx, parent, child.PRNumber, "", cfg) // repo "" => no PR comment
}

// OnDeployFinished updates a preview's PR comment and status after its
// deployment finishes. A no-op for non-preview apps. Safe to pass to
// deploy.Worker.SetDeployHook.
func (m *Manager) OnDeployFinished(ctx context.Context, app core.App, success bool) {
	if !app.Ephemeral {
		return
	}
	status := core.PreviewReady
	if !success {
		status = core.PreviewFailed
	}
	if err := m.store.SetPreviewStatus(ctx, app.ID, status); err != nil {
		log.Printf("previewmgr: set preview status for %s: %v", app.Name, err)
	}
	parent, err := m.store.GetApp(ctx, app.ParentID)
	if err != nil {
		log.Printf("previewmgr: OnDeployFinished load parent %d: %v", app.ParentID, err)
		return
	}
	cfg, err := m.store.GetPreviewConfig(ctx, parent.ID)
	if err != nil {
		return
	}
	var body string
	if success {
		var urls []string
		if ds, _ := m.store.ListDomains(ctx, app.ID); len(ds) > 0 {
			for _, d := range ds {
				scheme := "http://"
				if d.TLS {
					scheme = "https://"
				}
				urls = append(urls, scheme+d.Host)
			}
		}
		body = ReadyComment(urls, app.Branch)
	} else {
		body = FailedComment(app.Branch)
	}
	m.comment(ctx, cfg, parent.GithubRepo, app.PRNumber, body)
}

func (m *Manager) comment(ctx context.Context, cfg core.PreviewConfig, repo string, pr int, body string) {
	if !cfg.PostPRComment || m.token == nil || repo == "" {
		return
	}
	tok, ok, err := m.token(ctx)
	if err != nil || !ok {
		return
	}
	if err := m.gh.UpsertPRComment(ctx, tok, repo, pr, body); err != nil {
		log.Printf("previewmgr: PR comment %s#%d: %v", repo, pr, err)
	}
}
