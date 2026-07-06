package server

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/james-smart/outhaul/internal/core"
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
