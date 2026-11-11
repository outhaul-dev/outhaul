package server

import (
	"bytes"
	"context"
	"errors"
	"log"
	"net/http"
	"strings"
	"testing"

	"github.com/outhaul-dev/outhaul/internal/core"
	"github.com/outhaul-dev/outhaul/internal/docker"
	"github.com/outhaul-dev/outhaul/internal/traefik"
)

func headerRouting() traefik.Routing {
	return traefik.Routing{
		TLS: true, CertResolver: "le", GateAddress: testGateAddress,
		HeaderName: "X-Azure-FDID", HeaderValue: "abc-123",
	}
}

func TestRedeployStaleReusesTheRunningImage(t *testing.T) {
	e, _ := newProtectionEnv(t)
	e.srv.SetRouting(headerRouting())
	app, _, live := liveWeb(t, e, "outhaul/web:1")

	apps, err := e.srv.RedeployStale(context.Background())
	if err != nil {
		t.Fatalf("RedeployStale: %v", err)
	}
	if len(apps) != 1 || apps[0] != app.Name {
		t.Fatalf("apps = %v, want [%s]", apps, app.Name)
	}
	deps := deploymentsOf(t, e, app.ID)
	if len(deps) != 2 || deps[0].Status != core.StatusQueued {
		t.Fatalf("deployments = %+v, want a new queued one", deps)
	}
	if deps[0].Image != "outhaul/web:1" || deps[0].RollbackOf != live.ID {
		t.Errorf("queued deploy = image %q, rollback of %d; want the running image, deployment #%d", deps[0].Image, deps[0].RollbackOf, live.ID)
	}
	if e.deployer.notified == 0 {
		t.Error("the deployer was not notified")
	}
}

func TestRedeployStaleBuildsAComposeApp(t *testing.T) {
	e, _ := newProtectionEnv(t)
	e.srv.SetRouting(headerRouting())
	app, err := e.store.CreateApp(context.Background(), core.App{
		Name: "shop", RepoURL: "https://x/y.git", Kind: core.KindCompose, ComposePath: "docker-compose.yml",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.AddDomain(context.Background(), core.Domain{AppID: app.ID, Host: "shop.example.com", Service: "web", Port: 3000}); err != nil {
		t.Fatal(err)
	}
	c := composeContainer(app, "c1", "web", "running", true)
	c.Labels["traefik.enable"] = "true"
	e.runtime.stack = []docker.Container{c}

	apps, err := e.srv.RedeployStale(context.Background())
	if err != nil {
		t.Fatalf("RedeployStale: %v", err)
	}
	if len(apps) != 1 || apps[0] != "shop" {
		t.Fatalf("apps = %v, want [shop]", apps)
	}
	deps := deploymentsOf(t, e, app.ID)
	if len(deps) != 1 || deps[0].Status != core.StatusQueued || deps[0].Image != "" {
		t.Errorf("deployments = %+v, want one normal queued deploy", deps)
	}
}

func TestRedeployStaleSkips(t *testing.T) {
	e, _ := newProtectionEnv(t)
	rt := headerRouting()
	e.srv.SetRouting(rt)
	ctx := context.Background()

	stopped, err := e.store.CreateApp(ctx, core.App{Name: "stopped", RepoURL: "https://x/y.git", Domain: "stopped.test"})
	if err != nil {
		t.Fatal(err)
	}
	stoppedRows := domainsOf(t, e, stopped.ID)
	stoppedC := labelled("c-stopped", stopped, stoppedRows, "exited")

	current, err := e.store.CreateApp(ctx, core.App{Name: "current", RepoURL: "https://x/y.git", Domain: "current.test"})
	if err != nil {
		t.Fatal(err)
	}
	currentRows := domainsOf(t, e, current.ID)
	currentC := docker.Container{ID: "c-current", Name: "outhaul-app-current", State: "running",
		Labels: traefik.AppLabels(current, currentRows, deployAppPort, rt)}

	queued, err := e.store.CreateApp(ctx, core.App{Name: "queued", RepoURL: "https://x/y.git", Domain: "queued.test"})
	if err != nil {
		t.Fatal(err)
	}
	queuedRows := domainsOf(t, e, queued.ID)
	queuedC := labelled("c-queued", queued, queuedRows, "running")
	if _, err := e.store.CreateDeployment(ctx, queued.ID); err != nil {
		t.Fatal(err)
	}

	e.runtime.stack = []docker.Container{stoppedC, currentC, queuedC}
	apps, err := e.srv.RedeployStale(ctx)
	if err != nil {
		t.Fatalf("RedeployStale: %v", err)
	}
	if len(apps) != 0 {
		t.Errorf("apps = %v, want none", apps)
	}
	if deps := deploymentsOf(t, e, stopped.ID); len(deps) != 0 {
		t.Errorf("stopped app got deployments: %+v", deps)
	}
	if deps := deploymentsOf(t, e, current.ID); len(deps) != 0 {
		t.Errorf("current app got deployments: %+v", deps)
	}
	if deps := deploymentsOf(t, e, queued.ID); len(deps) != 1 {
		t.Errorf("queued app deployments = %+v, want only the one already queued", deps)
	}
}

func TestRedeployStaleDoesNothingWithNothingSet(t *testing.T) {
	e, _ := newProtectionEnv(t)
	e.srv.SetRouting(traefik.Routing{TLS: true})
	app, _, _ := liveWeb(t, e, "outhaul/web:1")
	apps, err := e.srv.RedeployStale(context.Background())
	if err != nil {
		t.Fatalf("RedeployStale: %v", err)
	}
	if len(apps) != 0 {
		t.Errorf("apps = %v, want none", apps)
	}
	if deps := deploymentsOf(t, e, app.ID); len(deps) != 1 {
		t.Errorf("deployments = %+v, want only the seeded running one", deps)
	}
}

func TestRedeployStaleAfterTheSettingIsRemoved(t *testing.T) {
	e, _ := newProtectionEnv(t)
	e.srv.SetRouting(traefik.Routing{})
	ctx := context.Background()
	app, err := e.store.CreateApp(ctx, core.App{Name: "web", RepoURL: "https://x/y.git", Domain: "web.test"})
	if err != nil {
		t.Fatal(err)
	}
	rows := domainsOf(t, e, app.ID)
	labels := traefik.AppLabels(app, rows, deployAppPort, headerRouting())
	labels[traefik.IngressLabel] = "0123456789ab"
	e.runtime.stack = []docker.Container{{ID: "c1", Name: "outhaul-app-web", State: "running", Labels: labels}}

	apps, err := e.srv.RedeployStale(ctx)
	if err != nil {
		t.Fatalf("RedeployStale: %v", err)
	}
	if len(apps) != 1 || apps[0] != "web" {
		t.Fatalf("apps = %v, want [web]", apps)
	}
	deps := deploymentsOf(t, e, app.ID)
	if len(deps) != 1 || deps[0].Status != core.StatusQueued {
		t.Errorf("deployments = %+v, want one queued deploy", deps)
	}
}

func TestRedeployStaleNamesAContainerWithNoApp(t *testing.T) {
	e, _ := newProtectionEnv(t)
	e.srv.SetRouting(headerRouting())
	e.runtime.stack = []docker.Container{{
		ID: "c-ghost", Name: "ghost-container", State: "running",
		Labels: map[string]string{"traefik.enable": "true", "outhaul.managed": "true", "outhaul.app": "ghost"},
	}}
	var logged bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(prev) })

	apps, err := e.srv.RedeployStale(context.Background())
	if err != nil {
		t.Fatalf("RedeployStale: %v", err)
	}
	if len(apps) != 0 {
		t.Errorf("apps = %v, want none", apps)
	}
	if !strings.Contains(logged.String(), "ghost-container") {
		t.Errorf("the log does not name the container:\n%s", logged.String())
	}
}

func TestRedeployStaleReportsAListError(t *testing.T) {
	e, _ := newProtectionEnv(t)
	e.srv.SetRouting(headerRouting())
	e.runtime.listErr = errors.New("docker unreachable")
	apps, err := e.srv.RedeployStale(context.Background())
	if err == nil {
		t.Fatal("RedeployStale should report a list error")
	}
	if len(apps) != 0 {
		t.Errorf("apps = %v, want none", apps)
	}
}

func TestRestartDeploysOnAStaleStamp(t *testing.T) {
	e, _ := newProtectionEnv(t)
	e.login(t)
	app, _, _ := liveWeb(t, e, "outhaul/web:1")
	e.srv.SetRouting(headerRouting())

	resp := e.postForm(t, "/apps/"+itoa(app.ID)+"/restart", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || !strings.HasPrefix(resp.Header.Get("Location"), "/deployments/") {
		t.Fatalf("restart: status %d, location %q; want a redirect to the new deployment", resp.StatusCode, resp.Header.Get("Location"))
	}
	if len(e.runtime.started) != 0 {
		t.Errorf("StartContainer was called: %v", e.runtime.started)
	}
}

func TestAppPageSaysNotServed(t *testing.T) {
	e, _ := newProtectionEnv(t)
	e.login(t)
	app, _, _ := liveWeb(t, e, "outhaul/web:1")
	page := func() string { return body(t, e.get(t, "/apps/"+itoa(app.ID))) }
	const notice = "This app isn't being served"

	e.srv.SetRouting(headerRouting())
	if got := page(); !strings.Contains(got, notice) {
		t.Errorf("a stale container should say the app isn't served")
	}

	rows := domainsOf(t, e, app.ID)
	e.runtime.stack[0].Labels = traefik.AppLabels(app, rows, deployAppPort, headerRouting())
	if got := page(); strings.Contains(got, notice) {
		t.Errorf("a current stamp should not say the app isn't served")
	}

	e.srv.SetRouting(traefik.Routing{TLS: true, HTTPOff: true})
	if got := page(); !strings.Contains(got, notice) {
		t.Errorf("a container labelled for web should say the app isn't served with HTTP off")
	}
	e.runtime.stack[0].Labels = traefik.AppLabels(app, rows, deployAppPort, traefik.Routing{TLS: true, HTTPOff: true})
	if got := page(); strings.Contains(got, notice) {
		t.Errorf("a container labelled for websecure should not say the app isn't served with HTTP off")
	}
}

func TestAccessOfAnIgnoredContainer(t *testing.T) {
	e, _ := newProtectionEnv(t)
	e.login(t)
	app, row, live := liveWeb(t, e, "outhaul/web:1")
	setProtected(t, e, row, true)
	e.srv.SetRouting(headerRouting())

	page := body(t, e.get(t, "/apps/"+itoa(app.ID)))
	want := accessStateFor(domainOf(t, e, row), app.Name, nil, &live)
	if !strings.Contains(page, want.Label) || !strings.Contains(page, want.Note) {
		t.Errorf("page does not show %q with %q, as a stopped app's row does", want.Label, want.Note)
	}
}

func TestIgnoredContainerIsNoLeftoverRoute(t *testing.T) {
	e, _ := newProtectionEnv(t)
	e.srv.SetRouting(headerRouting())
	ctx := context.Background()
	owner, err := e.store.CreateApp(ctx, core.App{Name: "owner", RepoURL: "https://x/y.git", Domain: "shared.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	other, err := e.store.CreateApp(ctx, core.App{Name: "other", RepoURL: "https://x/y.git"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.AddDomain(ctx, core.Domain{AppID: other.ID, Host: "old.example.com", Port: 3000}); err != nil {
		t.Fatal(err)
	}
	labels := traefik.AppLabels(other, []core.Domain{{ID: 1, Host: "shared.example.com", Port: 3000}}, deployAppPort,
		traefik.Routing{TLS: true, CertResolver: "le", GateAddress: testGateAddress})
	e.runtime.stack = []docker.Container{{ID: "c-other", Name: "outhaul-app-other", State: "running", Labels: labels}}

	got, err := e.srv.leftoverRoutes(ctx, owner, map[string]bool{"shared.example.com": true}, projectScope{all: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("leftoverRoutes = %+v, want none: the other app's container is ignored", got)
	}
}

func TestNoPageShowsTheHeader(t *testing.T) {
	e, _ := newProtectionEnv(t)
	e.login(t)
	app, _, _ := liveWeb(t, e, "outhaul/web:1")
	e.srv.SetRouting(traefik.Routing{
		TLS: true, CertResolver: "le", GateAddress: testGateAddress,
		HeaderName: "X-Probe-Id", HeaderValue: "abc-123",
	})
	for _, path := range []string{"/apps/" + itoa(app.ID), "/domains", "/settings"} {
		page := body(t, e.get(t, path))
		for _, secret := range []string{"X-Probe-Id", "abc-123"} {
			if strings.Contains(page, secret) {
				t.Errorf("%s contains %q", path, secret)
			}
		}
	}
}
