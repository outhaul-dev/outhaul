package server

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/outhaul-dev/outhaul/internal/core"
)

func TestTemplatesGallery(t *testing.T) {
	e := newTestEnv(t)
	e.completeSetup(t)

	page := body(t, e.get(t, "/templates"))
	for _, want := range []string{"Uptime Kuma", "Grafana", "Umami", "action=\"/templates/uptime-kuma\""} {
		if !strings.Contains(page, want) {
			t.Errorf("gallery missing %q", want)
		}
	}
}

func TestDeployTemplate(t *testing.T) {
	e := newTestEnv(t)
	e.completeSetup(t)

	resp := e.postForm(t, "/templates/uptime-kuma", url.Values{"name": {"kuma"}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("deploy = %d, want 303; body=%s", resp.StatusCode, body(t, resp))
	}
	loc := resp.Header.Get("Location")
	resp.Body.Close()
	if !strings.HasPrefix(loc, "/deployments/") {
		t.Errorf("redirect = %q, want the new deployment's log page", loc)
	}

	ctx := context.Background()
	app, err := e.store.GetAppByName(ctx, "kuma")
	if err != nil {
		t.Fatal(err)
	}
	if app.Source != core.SourceTemplate || app.Kind != core.KindCompose || app.TemplateID != "uptime-kuma" {
		t.Errorf("app = source %q kind %q template %q, want template/compose/uptime-kuma",
			app.Source, app.Kind, app.TemplateID)
	}
	if !strings.Contains(app.ComposeRaw, "louislam/uptime-kuma") {
		t.Errorf("ComposeRaw missing the template's compose content: %q", app.ComposeRaw)
	}

	domains, err := e.store.ListDomains(ctx, app.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(domains) != 1 || domains[0].Service != "uptime-kuma" || domains[0].Port != 3001 {
		t.Fatalf("domains = %+v, want one for uptime-kuma:3001", domains)
	}
	// The test env's server IP (203.0.113.7) must land in the generated host.
	if !strings.HasSuffix(domains[0].Host, "-203-0-113-7.sslip.io") || !strings.HasPrefix(domains[0].Host, "kuma-") {
		t.Errorf("generated domain = %q, want kuma-<hash>-203-0-113-7.sslip.io", domains[0].Host)
	}

	// One click = the first deployment is already queued.
	dep, err := e.store.LatestDeploymentForApp(ctx, app.ID)
	if err != nil || dep == nil {
		t.Fatalf("no deployment enqueued (err %v)", err)
	}
	if e.deployer.notified == 0 {
		t.Error("deployer was not notified")
	}
}

func TestDeployTemplateGeneratedEnv(t *testing.T) {
	e := newTestEnv(t)
	e.completeSetup(t)

	e.postForm(t, "/templates/umami", url.Values{"name": {"stats"}}).Body.Close()
	app, err := e.store.GetAppByName(context.Background(), "stats")
	if err != nil {
		t.Fatal(err)
	}
	env, err := e.store.ListEnv(context.Background(), app.ID)
	if err != nil {
		t.Fatal(err)
	}
	byKey := map[string]core.EnvVar{}
	for _, v := range env {
		byKey[v.Key] = v
	}
	sec, ok := byKey["APP_SECRET"]
	if !ok || sec.Value == "" || sec.Value == "${base64:64}" {
		t.Errorf("APP_SECRET not generated: %+v", sec)
	}
	if !sec.IsSecret {
		t.Error("APP_SECRET should be stored as a secret")
	}
}

func TestDeployTemplateValidation(t *testing.T) {
	e := newTestEnv(t)
	e.completeSetup(t)

	resp := e.postForm(t, "/templates/no-such-template", url.Values{})
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown template = %d, want 404", resp.StatusCode)
	}
	resp.Body.Close()

	resp = e.postForm(t, "/templates/grafana", url.Values{"name": {"Bad Name!"}})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("bad name = %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()

	// Duplicate names surface on the gallery, not as a 500.
	e.postForm(t, "/templates/grafana", url.Values{"name": {"dash"}}).Body.Close()
	resp = e.postForm(t, "/templates/grafana", url.Values{"name": {"dash"}})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("duplicate name = %d, want 400", resp.StatusCode)
	}
	if !strings.Contains(body(t, resp), "Could not create app") {
		t.Error("duplicate-name error not shown on the gallery")
	}
}

// A template deploy whose generated host another app already serves leaves no
// app behind: the app and its rows are one transaction.
func TestDeployTemplateLeavesNothingOnATakenHost(t *testing.T) {
	e := newTestEnv(t)
	e.completeSetup(t)
	// The template "wordpress" under no suffix gives a random sslip name, so
	// the collision is forced with the app name and a suffix instead.
	e.srv.naming = core.Naming{Suffix: "shared.demo.example.com"}
	squat, _ := e.store.CreateApp(context.Background(), core.App{
		Name: "squat", RepoURL: "https://x/y.git", Kind: core.KindCompose, ComposePath: "docker-compose.yml",
	})
	if _, err := e.store.AddDomain(context.Background(), core.Domain{
		AppID: squat.ID, Host: "blog.shared.demo.example.com", Service: "wordpress", Port: 80,
	}); err != nil {
		t.Fatal(err)
	}

	r := e.postForm(t, "/templates/wordpress", url.Values{"name": {"blog"}})
	if r.StatusCode < 400 {
		t.Fatalf("a taken host was accepted: %d", r.StatusCode)
	}
	r.Body.Close()
	if _, err := e.store.GetAppByName(context.Background(), "blog"); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("app survived a refused route: %v", err)
	}
}

// A template deploy on a host where another project's app requires sign-in is
// refused for anyone but a system admin, before anything is written.
func TestDeployTemplateRefusesAnotherProjectsHost(t *testing.T) {
	e := newTestEnv(t)
	e.completeSetup(t)
	e.srv.naming = core.Naming{Suffix: "shared.demo.example.com"}

	other, err := e.store.CreateProject(context.Background(), core.Project{Name: "Other"})
	if err != nil {
		t.Fatal(err)
	}
	theirs, err := e.store.CreateApp(context.Background(), core.App{
		Name: "theirs", ProjectID: other.ID, RepoURL: "https://x/y.git", Domain: "blog.shared.demo.example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := e.store.ListDomains(context.Background(), theirs.ID)
	if err != nil || len(rows) != 1 {
		t.Fatalf("theirs' rows = %v (%v)", rows, err)
	}
	if err := e.store.SetDomainProtected(context.Background(), theirs.ID, rows[0].ID, true); err != nil {
		t.Fatal(err)
	}

	// A project member on the default project, not a system admin: only an
	// admin may put a route where another project's app requires sign-in.
	tok := e.signInAs(t, "member", core.RoleProjectMember, core.DefaultProjectID)
	r := e.postAs(t, tok, "/templates/wordpress", url.Values{
		"name": {"blog"}, "project_id": {itoa(core.DefaultProjectID)},
	})
	if r.StatusCode < 400 || !strings.Contains(body(t, r), "Another project") {
		t.Fatalf("another project's protected host: status %d, want the shared-host refusal", r.StatusCode)
	}
	if _, err := e.store.GetAppByName(context.Background(), "blog"); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("app survived a refused host: %v", err)
	}
}

func TestTemplateAppPage(t *testing.T) {
	e := newTestEnv(t)
	e.completeSetup(t)

	e.postForm(t, "/templates/uptime-kuma", url.Values{"name": {"kuma"}}).Body.Close()
	app, _ := e.store.GetAppByName(context.Background(), "kuma")

	page := body(t, e.get(t, "/apps/"+itoa(app.ID)))
	if !strings.Contains(page, "uptime-kuma") {
		t.Error("app page missing the template id")
	}
	// No repo to configure: the deploy-settings and connect-repo panels vanish.
	if strings.Contains(page, `name="branch"`) || strings.Contains(page, "Connect this repo") {
		t.Error("template app page should not offer repo settings")
	}

	// And the apps list badges it.
	list := body(t, e.get(t, "/apps"))
	if !strings.Contains(list, ">Template</span>") {
		t.Error("apps list missing the Template badge")
	}

	// The project page's app table mirrors the apps list for template apps.
	proj := body(t, e.get(t, "/projects/"+itoa(app.ProjectID)))
	if !strings.Contains(proj, "template: uptime-kuma") || !strings.Contains(proj, ">Template</span>") {
		t.Error("project page missing the template badge or repo cell")
	}
}

// With the setting on, every row a template deploy writes requires sign-in.
func TestDeployTemplateProtectsItsRows(t *testing.T) {
	e, _ := newProtectionEnv(t)
	e.srv.protectNew = true
	r := e.postForm(t, "/templates/wordpress", url.Values{"name": {"blog"}, "project_id": {itoa(core.DefaultProjectID)}})
	if r.StatusCode >= 400 {
		t.Fatalf("deploy template: status %d, body %s", r.StatusCode, body(t, r))
	}
	r.Body.Close()
	app, err := e.store.GetAppByName(context.Background(), "blog")
	if err != nil {
		t.Fatal(err)
	}
	rows, _ := e.store.ListDomains(context.Background(), app.ID)
	if len(rows) == 0 {
		t.Fatal("no rows were written")
	}
	for _, d := range rows {
		if !d.Protected {
			t.Errorf("row %s%s is open", d.Host, d.Path)
		}
	}
	if e.deployer.notified == 0 {
		t.Error("the first deploy was not queued")
	}
}

// The template form offers no switch: a request that doesn't say is protected.
func TestDeployTemplateWithoutTheFormsFieldIsProtected(t *testing.T) {
	e, _ := newProtectionEnv(t)
	e.srv.protectNew = true
	r := e.postForm(t, "/templates/wordpress", url.Values{"name": {"blog"}})
	if r.StatusCode >= 400 {
		t.Fatalf("deploy template: status %d, body %s", r.StatusCode, body(t, r))
	}
	r.Body.Close()
	app, _ := e.store.GetAppByName(context.Background(), "blog")
	rows, _ := e.store.ListDomains(context.Background(), app.ID)
	for _, d := range rows {
		if !d.Protected {
			t.Errorf("row %s%s is open", d.Host, d.Path)
		}
	}
}

// A failed gate-host reconcile removes the app and queues no deployment.
func TestDeployTemplateRefusesWhenTheGateHostFails(t *testing.T) {
	e, px := newProtectionEnv(t)
	e.srv.protectNew = true
	px.err = errors.New("docker unreachable")
	r := e.postForm(t, "/templates/wordpress", url.Values{"name": {"blog"}})
	if r.StatusCode < 400 || !strings.Contains(body(t, r), "docker unreachable") {
		t.Fatalf("deploy template: status %d, want a refusal naming the cause", r.StatusCode)
	}
	if _, err := e.store.GetAppByName(context.Background(), "blog"); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("app survived a failed gate host: %v", err)
	}
}

// Another app's old open route on the host is refused, and nothing is written.
func TestDeployTemplateRefusesAnotherAppsOldRoute(t *testing.T) {
	e, _ := newProtectionEnv(t)
	e.srv.protectNew = true
	e.srv.naming = core.Naming{Suffix: "shared.demo.example.com"}
	leftoverHooks(t, e, []core.Domain{{Host: "blog.shared.demo.example.com", Port: 80}}, nil)

	r := e.postForm(t, "/templates/wordpress", url.Values{"name": {"blog"}})
	if r.StatusCode < 400 || !strings.Contains(body(t, r), "hooks") {
		t.Fatalf("deploy template: status %d, want a refusal naming the app that must redeploy", r.StatusCode)
	}
	if _, err := e.store.GetAppByName(context.Background(), "blog"); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("app survived a refused host: %v", err)
	}
}

// With the setting off the rows are open, as they were.
func TestDeployTemplateWithoutTheSettingLeavesRowsOpen(t *testing.T) {
	e := newTestEnv(t)
	e.completeSetup(t)
	r := e.postForm(t, "/templates/wordpress", url.Values{"name": {"blog"}})
	if r.StatusCode >= 400 {
		t.Fatalf("deploy template: status %d, body %s", r.StatusCode, body(t, r))
	}
	r.Body.Close()
	app, _ := e.store.GetAppByName(context.Background(), "blog")
	rows, _ := e.store.ListDomains(context.Background(), app.ID)
	for _, d := range rows {
		if d.Protected {
			t.Errorf("row %s%s is protected with the setting off", d.Host, d.Path)
		}
	}
}
