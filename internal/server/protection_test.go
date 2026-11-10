package server

import (
	"context"
	"errors"
	"html"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/outhaul-dev/outhaul/internal/core"
	"github.com/outhaul-dev/outhaul/internal/docker"
	"github.com/outhaul-dev/outhaul/internal/traefik"
)

// testGateAddress is the forward-auth address the tests' container labels carry.
const testGateAddress = "http://host.docker.internal:8080/gate/check"

// fakeProxy stands in for the infraController's reconcile.
type fakeProxy struct {
	calls int
	err   error
	at    func() // runs during each call
}

func (f *fakeProxy) ReconcileProxy(context.Context) error {
	f.calls++
	if f.at != nil {
		f.at()
	}
	return f.err
}

// newProtectionEnv is a signed-in server where sign-in can be turned on: it
// has a public URL, TLS and a proxy to reconcile.
func newProtectionEnv(t *testing.T) (*testEnv, *fakeProxy) {
	t.Helper()
	e := newTestEnv(t)
	e.srv.tlsEnabled = true
	px := &fakeProxy{}
	e.srv.SetProxyControl(px)
	e.completeSetup(t)
	return e, px
}

// labelled is app's container as a deploy of rows labels it.
func labelled(id string, app core.App, rows []core.Domain, state string) docker.Container {
	return docker.Container{ID: id, Name: appContainerPrefix + app.Name, State: state,
		Labels: traefik.AppLabels(app, rows, deployAppPort, true, "le", testGateAddress)}
}

// runContainers makes cs the app's containers: what the fake runtime lists,
// and the first is what it finds by name.
func (e *testEnv) runContainers(cs ...docker.Container) {
	e.runtime.stack = cs
	e.runtime.container = nil
	if len(cs) > 0 {
		c := cs[0]
		e.runtime.container = &c
	}
}

// liveWeb creates the nixpacks app "web", whose one row is web.test (open),
// with a running container labelled from that row and, unless image is "",
// a live deployment of image.
func liveWeb(t *testing.T, e *testEnv, image string) (core.App, core.Domain, core.Deployment) {
	t.Helper()
	ctx := context.Background()
	app, err := e.store.CreateApp(ctx, core.App{Name: "web", RepoURL: "https://x/y.git", Domain: "web.test"})
	if err != nil {
		t.Fatal(err)
	}
	rows := domainsOf(t, e, app.ID)
	var live core.Deployment
	if image != "" {
		live = seedFinishedDeploy(t, e, app.ID, image)
	}
	e.runContainers(labelled("c1", app, rows, "running"))
	return app, rows[0], live
}

func domainsOf(t *testing.T, e *testEnv, appID int64) []core.Domain {
	t.Helper()
	rows, err := e.store.ListDomains(context.Background(), appID)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func deploymentsOf(t *testing.T, e *testEnv, appID int64) []core.Deployment {
	t.Helper()
	deps, err := e.store.ListDeploymentsForApp(context.Background(), appID)
	if err != nil {
		t.Fatal(err)
	}
	return deps
}

func setProtected(t *testing.T, e *testEnv, d core.Domain, on bool) core.Domain {
	t.Helper()
	if err := e.store.SetDomainProtected(context.Background(), d.AppID, d.ID, on); err != nil {
		t.Fatal(err)
	}
	d.Protected = on
	return d
}

func domainOf(t *testing.T, e *testEnv, d core.Domain) core.Domain {
	t.Helper()
	got, err := e.store.GetDomain(context.Background(), d.AppID, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// domainForm is what the domain dialog submits for d, with the switch on or
// off. A disabled switch submits nothing; see withoutSwitch.
func domainForm(d core.Domain, protected bool) url.Values {
	f := url.Values{"host_kind": {"custom"}, "host": {d.Host}, "path": {d.Path},
		"internal_path": {d.InternalPath}, "protected_offered": {"1"}}
	if d.TLS {
		f.Set("tls", "on")
	}
	if d.Service != "" {
		f.Set("service", d.Service)
		f.Set("port", strconv.Itoa(d.Port))
	}
	if protected {
		f.Set("protected", "on")
	}
	return f
}

// saveDomain adds d (no ID) or edits it, as the dialog does.
func (e *testEnv) saveDomain(t *testing.T, appID int64, d core.Domain, form url.Values) *http.Response {
	t.Helper()
	path := "/apps/" + itoa(appID) + "/domains"
	if d.ID != 0 {
		path += "/" + itoa(d.ID)
	}
	return e.postForm(t, path, form)
}

func wantStatus(t *testing.T, what string, resp *http.Response, want int) string {
	t.Helper()
	b := body(t, resp)
	if resp.StatusCode != want {
		t.Fatalf("%s: status = %d, want %d; body = %q", what, resp.StatusCode, want, b)
	}
	return b
}

// The switch needs a public URL, where browsers are sent to sign in, and HTTPS
// for visitors: the server's TLS, which serves a protected row over HTTPS
// whatever its own TLS flag, or a tunnel, whose edge terminates TLS.
func TestAvailability(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name                string
		publicURL           string
		tls, tunnel, rowTLS bool
		ok                  bool
		reason              string
	}{
		{"TLS server, the row's TLS on", "https://x", true, false, true, true, ""},
		{"TLS server, the row's TLS off", "https://x", true, false, false, true, ""},
		{"no public URL", "", true, false, true, false, "Set OUTHAUL_PUBLIC_URL so people can be sent to sign in."},
		{"no HTTPS", "https://x", false, false, true, false, "Protected routes need HTTPS. Turn on HTTPS for the server, or use a tunnel."},
		{"a tunnel", "https://x", false, true, false, true, ""},
		{"a tunnel, no public URL", "", false, true, false, false, "Set OUTHAUL_PUBLIC_URL so people can be sent to sign in."},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newTestEnv(t)
			e.srv.publicURL, e.srv.tlsEnabled = c.publicURL, c.tls
			if c.tunnel {
				if err := e.store.SetCloudflareToken(ctx, "tok"); err != nil {
					t.Fatal(err)
				}
			}
			ok, reason := e.srv.protectionAvailability(ctx, core.Domain{Host: "wiki.example.com", TLS: c.rowTLS})
			if ok != c.ok || reason != c.reason {
				t.Errorf("protectionAvailability = %v, %q; want %v, %q", ok, reason, c.ok, c.reason)
			}
		})
	}

	// A row on Outhaul's own host can't be protected: it can't be edited at all.
	e := newTestEnv(t)
	e.srv.tlsEnabled = true
	if ok, reason := e.srv.protectionAvailability(ctx, core.Domain{Host: "Slip.Example.com"}); ok || reason != ownHostRefusal {
		t.Errorf("own host: protectionAvailability = %v, %q; want false, %q", ok, reason, ownHostRefusal)
	}
}

// rowGate counts the containers that define a row's router: all of them must
// have the gate, since during a cutover a request may reach either. A
// health-check container has no routers, so it doesn't count.
func TestRowGate(t *testing.T) {
	app := core.App{Name: "web"}
	open := core.Domain{ID: 1, Host: "web.test"}
	shut := open
	shut.Protected = true
	router := traefik.RouterName(app.Name, open.ID)
	healthCheck := docker.Container{ID: "hc", State: "running", Labels: map[string]string{
		"traefik.enable": "false", "outhaul.managed": "true", "outhaul.app": app.Name}}

	for _, c := range []struct {
		name           string
		cs             []docker.Container
		defined, gated bool
	}{
		{"no containers", nil, false, false},
		{"only a health check", []docker.Container{healthCheck}, false, false},
		{"gated", []docker.Container{labelled("a", app, []core.Domain{shut}, "running"), healthCheck}, true, true},
		{"open", []docker.Container{labelled("a", app, []core.Domain{open}, "running")}, true, false},
		{"a cutover, only the new one gated", []docker.Container{
			labelled("old", app, []core.Domain{open}, "running"), labelled("new", app, []core.Domain{shut}, "running")}, true, false},
		{"a cutover, both gated", []docker.Container{
			labelled("old", app, []core.Domain{shut}, "running"), labelled("new", app, []core.Domain{shut}, "running")}, true, true},
		{"another row's router only", []docker.Container{labelled("a", app, []core.Domain{{ID: 2, Host: "x.test", Protected: true}}, "running")}, false, false},
	} {
		defined, gated := rowGate(c.cs, router)
		if defined != c.defined || gated != c.gated {
			t.Errorf("%s: rowGate = %v, %v; want %v, %v", c.name, defined, gated, c.defined, c.gated)
		}
	}
}

func TestSavingRefusesUnavailableSwitch(t *testing.T) {
	e, px := newProtectionEnv(t)
	app, row, _ := liveWeb(t, e, "img:1")
	before := len(deploymentsOf(t, e, app.ID))

	e.srv.publicURL = ""
	b := wantStatus(t, "add a protected row with no public URL", e.saveDomain(t, app.ID, core.Domain{Host: "new.test"}, domainForm(core.Domain{Host: "new.test"}, true)), http.StatusBadRequest)
	if !strings.Contains(b, "OUTHAUL_PUBLIC_URL") {
		t.Errorf("refusal = %q, want it to say what's missing", b)
	}
	if rows := domainsOf(t, e, app.ID); len(rows) != 1 {
		t.Errorf("rows = %+v, want the refused row not added", rows)
	}
	wantStatus(t, "turn a row on with no public URL", e.saveDomain(t, app.ID, row, domainForm(row, true)), http.StatusBadRequest)
	if got := domainOf(t, e, row); got.Protected {
		t.Error("the row was turned on with no public URL")
	}

	e.srv.publicURL = "https://slip.example.com"
	e.srv.tlsEnabled = false
	b = wantStatus(t, "turn a row on with no HTTPS", e.saveDomain(t, app.ID, row, domainForm(row, true)), http.StatusBadRequest)
	if !strings.Contains(b, "need HTTPS") {
		t.Errorf("refusal = %q, want it to say HTTPS is needed", b)
	}
	if got := domainOf(t, e, row); got.Protected {
		t.Error("the row was turned on with no HTTPS")
	}
	if n := len(deploymentsOf(t, e, app.ID)); n != before || px.calls != 0 {
		t.Errorf("deployments %d -> %d, proxy reconciles %d; a refused save should do neither", before, n, px.calls)
	}
}

// Note 4: on a TLS server a protected row is served over HTTPS even with its
// own TLS flag off, so that flag doesn't stop the switch.
func TestProtectedRowWithTLSOffIsAccepted(t *testing.T) {
	e, _ := newProtectionEnv(t)
	app, row, _ := liveWeb(t, e, "img:1")
	row = setProtected(t, e, row, true)
	e.runContainers(labelled("c1", app, []core.Domain{row}, "running"))

	off := row
	off.TLS = false
	wantStatus(t, "turn HTTPS off on a protected row", e.saveDomain(t, app.ID, row, domainForm(off, true)), http.StatusSeeOther)
	got := domainOf(t, e, row)
	if !got.Protected || got.TLS || !got.WantsTLS() {
		t.Errorf("row = %+v, want protected, its TLS flag off, and still served over HTTPS", got)
	}
}

func TestUnavailableRowKeepsItsProtection(t *testing.T) {
	e, _ := newProtectionEnv(t)
	app, row, _ := liveWeb(t, e, "img:1")
	row = setProtected(t, e, row, true)
	e.runContainers(labelled("c1", app, []core.Domain{row}, "running"))
	before := len(deploymentsOf(t, e, app.ID))

	e.srv.tlsEnabled = false // HTTPS is gone, so the switch is disabled and submits nothing
	wantStatus(t, "edit with the switch disabled", e.saveDomain(t, app.ID, row, domainForm(row, false)), http.StatusSeeOther)
	if got := domainOf(t, e, row); !got.Protected {
		t.Error("an edit with the switch disabled turned protection off")
	}
	if n := len(deploymentsOf(t, e, app.ID)); n != before {
		t.Errorf("deployments %d -> %d; nothing changed, so nothing should deploy", before, n)
	}
	page := html.UnescapeString(body(t, e.get(t, "/apps/"+itoa(app.ID))))
	if !strings.Contains(page, "Visitors can't get through to the domains that require sign-in") ||
		!strings.Contains(page, "Protected routes need HTTPS") {
		t.Error("the app page should warn that its protected rows can't be reached, and say why")
	}
}

// A form that never offered the switch (a page from before it, a script)
// keeps the row's protection.
func TestEditWithoutTheSwitchKeepsProtection(t *testing.T) {
	e, _ := newProtectionEnv(t)
	app, row, _ := liveWeb(t, e, "img:1")
	row = setProtected(t, e, row, true)
	form := domainForm(row, false)
	form.Del("protected_offered")
	wantStatus(t, "edit from a form with no switch", e.saveDomain(t, app.ID, row, form), http.StatusSeeOther)
	if got := domainOf(t, e, row); !got.Protected {
		t.Error("an edit from a form with no switch turned protection off")
	}
}

func TestSwitchOnRunningAppQueuesImageReuse(t *testing.T) {
	e, _ := newProtectionEnv(t)
	app, row, live := liveWeb(t, e, "img:1")
	notified := e.deployer.notified

	wantStatus(t, "turn the switch on", e.saveDomain(t, app.ID, row, domainForm(row, true)), http.StatusSeeOther)
	if got := domainOf(t, e, row); !got.Protected {
		t.Fatal("the row wasn't turned on")
	}
	deps := deploymentsOf(t, e, app.ID)
	if len(deps) != 2 {
		t.Fatalf("deployments = %+v, want one queued", deps)
	}
	if d := deps[0]; d.Status != core.StatusQueued || d.Image != "img:1" || d.RollbackOf != live.ID {
		t.Errorf("queued %+v, want a reuse of img:1 from #%d", d, live.ID)
	}
	if e.deployer.notified == notified {
		t.Error("the deployer wasn't told about the queued deployment")
	}
}

func TestNoReusableImageRunsNormalDeploy(t *testing.T) {
	t.Run("compose", func(t *testing.T) {
		e, _ := newProtectionEnv(t)
		app := seedComposeStack(t, e)
		seedFinishedDeploy(t, e, app.ID, "img:1") // a compose stack never reuses one
		admin := core.Domain{Host: "shop.example.com", Path: "/admin", Service: "web", Port: 3000}
		wantStatus(t, "add a protected row", e.saveDomain(t, app.ID, core.Domain{}, domainForm(admin, true)), http.StatusSeeOther)
		deps := deploymentsOf(t, e, app.ID)
		if len(deps) != 2 || deps[0].Image != "" || deps[0].RollbackOf != 0 || deps[0].Status != core.StatusQueued {
			t.Errorf("deployments = %+v, want a normal deploy queued", deps)
		}
	})
	t.Run("the image was pruned", func(t *testing.T) {
		e, _ := newProtectionEnv(t)
		app, row, _ := liveWeb(t, e, "img:1")
		if err := e.store.MarkImagePruned(context.Background(), "img:1"); err != nil {
			t.Fatal(err)
		}
		wantStatus(t, "turn the switch on", e.saveDomain(t, app.ID, row, domainForm(row, true)), http.StatusSeeOther)
		deps := deploymentsOf(t, e, app.ID)
		if len(deps) != 2 || deps[0].Image != "" || deps[0].RollbackOf != 0 {
			t.Errorf("deployments = %+v, want a normal deploy queued", deps)
		}
	})
}

func TestStoppedOrNeverDeployedQueuesNothing(t *testing.T) {
	t.Run("never deployed", func(t *testing.T) {
		e, _ := newProtectionEnv(t)
		app, row, _ := liveWeb(t, e, "")
		e.runContainers()
		wantStatus(t, "turn the switch on", e.saveDomain(t, app.ID, row, domainForm(row, true)), http.StatusSeeOther)
		if !domainOf(t, e, row).Protected {
			t.Error("the row wasn't turned on")
		}
		if deps := deploymentsOf(t, e, app.ID); len(deps) != 0 {
			t.Errorf("deployments = %+v, want none", deps)
		}
	})
	t.Run("stopped", func(t *testing.T) {
		e, _ := newProtectionEnv(t)
		app, row, _ := liveWeb(t, e, "img:1")
		e.runContainers(labelled("c1", app, []core.Domain{row}, "exited"))
		wantStatus(t, "turn the switch on", e.saveDomain(t, app.ID, row, domainForm(row, true)), http.StatusSeeOther)
		if deps := deploymentsOf(t, e, app.ID); len(deps) != 1 {
			t.Errorf("deployments = %+v, want only the old one", deps)
		}
		wantStatus(t, "add a protected row", e.saveDomain(t, app.ID, core.Domain{}, domainForm(core.Domain{Host: "new.test"}, true)), http.StatusSeeOther)
		if deps := deploymentsOf(t, e, app.ID); len(deps) != 1 {
			t.Errorf("deployments = %+v, want only the old one", deps)
		}
	})
}

func TestInFlightDeployIsNotUndone(t *testing.T) {
	for _, st := range []core.DeployStatus{core.StatusQueued, core.StatusBuilding, core.StatusDeploying} {
		t.Run("a normal deploy "+string(st), func(t *testing.T) {
			e, _ := newProtectionEnv(t)
			app, row, _ := liveWeb(t, e, "img:1")
			ctx := context.Background()
			inFlight, err := e.store.CreateDeployment(ctx, app.ID)
			if err != nil {
				t.Fatal(err)
			}
			if st != core.StatusQueued {
				if ok, err := e.store.ClaimDeployment(ctx, inFlight.ID); err != nil || !ok {
					t.Fatalf("claim: %v %v", ok, err)
				}
			}
			if st == core.StatusDeploying {
				mustTransition(t, e, inFlight.ID, core.StatusBuilding, core.StatusDeploying)
			}
			wantStatus(t, "turn the switch on", e.saveDomain(t, app.ID, row, domainForm(row, true)), http.StatusSeeOther)
			deps := deploymentsOf(t, e, app.ID)
			if d := deps[0]; d.ID == inFlight.ID || d.Image != "" || d.RollbackOf != 0 {
				t.Errorf("queued %+v, want another normal deploy, not a reuse of img:1 after #%d", d, inFlight.ID)
			}
		})
	}
	t.Run("a rollback", func(t *testing.T) {
		e, _ := newProtectionEnv(t)
		app, row, _ := liveWeb(t, e, "")
		old := seedFinishedDeploy(t, e, app.ID, "img:0")
		seedFinishedDeploy(t, e, app.ID, "img:1")
		rb, err := e.store.CreateRollback(context.Background(), app.ID, "img:0", old.ID)
		if err != nil {
			t.Fatal(err)
		}
		wantStatus(t, "turn the switch on", e.saveDomain(t, app.ID, row, domainForm(row, true)), http.StatusSeeOther)
		deps := deploymentsOf(t, e, app.ID)
		if d := deps[0]; d.ID == rb.ID || d.Image != "img:0" || d.RollbackOf != old.ID {
			t.Errorf("queued %+v, want img:0 again (from #%d), so the rollback isn't undone", d, old.ID)
		}
	})
}

func TestWhichSavesQueueADeploy(t *testing.T) {
	type save struct {
		name      string
		compose   bool
		protected bool                            // the row before the save
		add       bool                            // add a new row instead of editing
		change    func(d core.Domain) core.Domain // the submitted row
		on        bool                            // the submitted switch
		want      bool
	}
	same := func(d core.Domain) core.Domain { return d }
	newRow := func(core.Domain) core.Domain { return core.Domain{Host: "new.test"} }
	cases := []save{
		{name: "add a protected row", add: true, change: newRow, on: true, want: true},
		{name: "add an open row", add: true, change: newRow, on: false, want: false},
		{name: "turn the switch on", change: same, on: true, want: true},
		{name: "turn the switch off", protected: true, change: same, on: false, want: true},
		{name: "edit the host of a protected row", protected: true, change: func(d core.Domain) core.Domain { d.Host = "moved.test"; return d }, on: true, want: true},
		{name: "edit the path of a protected row", protected: true, change: func(d core.Domain) core.Domain { d.Path = "/docs"; return d }, on: true, want: true},
		{name: "save a protected row unchanged", protected: true, change: same, on: true, want: false},
		{name: "edit the host of an open row", change: func(d core.Domain) core.Domain { d.Host = "moved.test"; return d }, on: false, want: false},
		{name: "edit the port of an open row", compose: true, change: func(d core.Domain) core.Domain { d.Port = 4000; return d }, on: false, want: false},
		{name: "edit the port of a protected row", compose: true, protected: true, change: func(d core.Domain) core.Domain { d.Port = 4000; return d }, on: true, want: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e, _ := newProtectionEnv(t)
			var (
				app core.App
				row core.Domain
			)
			if c.compose {
				app = seedComposeStack(t, e)
				row = domainsOf(t, e, app.ID)[0]
			} else {
				app, row, _ = liveWeb(t, e, "img:1")
			}
			if c.protected {
				row = setProtected(t, e, row, true)
			}
			if !c.compose {
				e.runContainers(labelled("c1", app, []core.Domain{row}, "running"))
			}
			before := len(deploymentsOf(t, e, app.ID))
			target := row
			if c.add {
				target = core.Domain{}
			}
			wantStatus(t, c.name, e.saveDomain(t, app.ID, target, domainForm(c.change(row), c.on)), http.StatusSeeOther)
			if got := len(deploymentsOf(t, e, app.ID)) > before; got != c.want {
				t.Errorf("queued a deploy = %v, want %v", got, c.want)
			}
		})
	}
}

func TestRestartRedeploysOnMismatch(t *testing.T) {
	restart := func(t *testing.T, e *testEnv, app core.App) *http.Response {
		t.Helper()
		resp := e.postForm(t, "/apps/"+itoa(app.ID)+"/restart", url.Values{})
		resp.Body.Close()
		if resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("restart = %d, want 303", resp.StatusCode)
		}
		return resp
	}
	redeployed := func(t *testing.T, e *testEnv, app core.App, before int, resp *http.Response) {
		t.Helper()
		deps := deploymentsOf(t, e, app.ID)
		if len(deps) != before+1 {
			t.Fatalf("deployments %d -> %d, want one queued", before, len(deps))
		}
		if loc := resp.Header.Get("Location"); loc != deploymentPath(deps[0].ID) {
			t.Errorf("redirect = %q, want the queued deployment %q", loc, deploymentPath(deps[0].ID))
		}
		if len(e.runtime.started) != 0 || len(e.runtime.stopped) != 0 {
			t.Errorf("started %v, stopped %v: the old container must not be restarted", e.runtime.started, e.runtime.stopped)
		}
		if n := len(e.compose.CallsFor("restart")); n != 0 {
			t.Errorf("compose restart calls = %d, want none", n)
		}
	}
	restarted := func(t *testing.T, e *testEnv, app core.App) {
		t.Helper()
		before := len(deploymentsOf(t, e, app.ID))
		if loc := restart(t, e, app).Header.Get("Location"); loc != "/apps/"+itoa(app.ID) {
			t.Errorf("redirect = %q, want the app page", loc)
		}
		if n := len(deploymentsOf(t, e, app.ID)); n != before {
			t.Errorf("deployments %d -> %d, want a plain restart", before, n)
		}
		if len(e.runtime.started) != 1 || e.runtime.started[0] != "c1" {
			t.Errorf("started = %v, want the container restarted", e.runtime.started)
		}
	}

	t.Run("rows protected, labels lack the gate", func(t *testing.T) {
		e, _ := newProtectionEnv(t)
		app, row, live := liveWeb(t, e, "img:1")
		setProtected(t, e, row, true)
		before := len(deploymentsOf(t, e, app.ID))
		redeployed(t, e, app, before, restart(t, e, app))
		if d := deploymentsOf(t, e, app.ID)[0]; d.Image != "img:1" || d.RollbackOf != live.ID {
			t.Errorf("queued %+v, want a reuse of the live image", d)
		}
	})
	t.Run("labels match the rows", func(t *testing.T) {
		e, _ := newProtectionEnv(t)
		app, row, _ := liveWeb(t, e, "img:1")
		row = setProtected(t, e, row, true)
		e.runContainers(labelled("c1", app, []core.Domain{row}, "running"))
		restarted(t, e, app)
	})
	t.Run("compose: the stack is redeployed, not restarted", func(t *testing.T) {
		e, _ := newProtectionEnv(t)
		app := seedComposeStack(t, e)
		row := domainsOf(t, e, app.ID)[0]
		open := traefik.AppLabels(app, []core.Domain{row}, row.Port, true, "le", testGateAddress)
		for k, v := range open {
			e.runtime.stack[0].Labels[k] = v
		}
		setProtected(t, e, row, true)
		before := len(deploymentsOf(t, e, app.ID))
		redeployed(t, e, app, before, restart(t, e, app))
	})
	t.Run("a stopped container without the gate, the switch on", func(t *testing.T) {
		e, _ := newProtectionEnv(t)
		app, row, _ := liveWeb(t, e, "img:1")
		e.runContainers(labelled("c1", app, []core.Domain{row}, "exited"))
		setProtected(t, e, row, true)
		before := len(deploymentsOf(t, e, app.ID))
		redeployed(t, e, app, before, restart(t, e, app))
	})
	t.Run("the switch off, the container still gated", func(t *testing.T) {
		e, _ := newProtectionEnv(t)
		app, row, _ := liveWeb(t, e, "img:1")
		gated := row
		gated.Protected = true
		e.runContainers(labelled("c1", app, []core.Domain{gated}, "running"))
		before := len(deploymentsOf(t, e, app.ID))
		redeployed(t, e, app, before, restart(t, e, app))
	})
	t.Run("a protected row added since the last deploy", func(t *testing.T) {
		e, _ := newProtectionEnv(t)
		app, _, _ := liveWeb(t, e, "img:1")
		if _, err := e.store.AddDomain(context.Background(), core.Domain{AppID: app.ID, Host: "web.test", Path: "/admin", Port: deployAppPort, Protected: true}); err != nil {
			t.Fatal(err)
		}
		before := len(deploymentsOf(t, e, app.ID))
		redeployed(t, e, app, before, restart(t, e, app))
	})
	t.Run("an open row labelled before protected routes", func(t *testing.T) {
		e, _ := newProtectionEnv(t)
		app, row, _ := liveWeb(t, e, "img:1")
		r := traefik.RouterName(app.Name, row.ID)
		e.runContainers(docker.Container{ID: "c1", Name: appContainerPrefix + app.Name, State: "running", Labels: map[string]string{
			"traefik.enable": "true", "outhaul.managed": "true", "outhaul.app": app.Name,
			"traefik.http.routers." + r + ".rule":                      "Host(`web.test`)",
			"traefik.http.routers." + r + ".entrypoints":               "web",
			"traefik.http.routers." + r + ".service":                   r,
			"traefik.http.services." + r + ".loadbalancer.server.port": "8080",
		}})
		restarted(t, e, app)
	})
	t.Run("an open row added since the last deploy", func(t *testing.T) {
		e, _ := newProtectionEnv(t)
		app, _, _ := liveWeb(t, e, "img:1")
		if _, err := e.store.AddDomain(context.Background(), core.Domain{AppID: app.ID, Host: "new.test", Port: deployAppPort}); err != nil {
			t.Fatal(err)
		}
		restarted(t, e, app)
	})
	t.Run("never deployed: nothing to restart, and nothing deploys", func(t *testing.T) {
		e, _ := newProtectionEnv(t)
		app, row, _ := liveWeb(t, e, "")
		e.runContainers()
		setProtected(t, e, row, true)
		resp := e.postForm(t, "/apps/"+itoa(app.ID)+"/restart", url.Values{})
		resp.Body.Close()
		if resp.StatusCode != http.StatusConflict {
			t.Errorf("restart = %d, want 409 (deploy it first)", resp.StatusCode)
		}
		if deps := deploymentsOf(t, e, app.ID); len(deps) != 0 {
			t.Errorf("deployments = %+v, want none", deps)
		}
	})
}

func TestWarningNamesWhatWillShip(t *testing.T) {
	ctx := context.Background()
	e, _ := newProtectionEnv(t)
	create := func(a core.App) core.App {
		t.Helper()
		app, err := e.store.CreateApp(ctx, a)
		if err != nil {
			t.Fatal(err)
		}
		return app
	}

	shop := create(core.App{Name: "shop", RepoURL: "https://x/shop.git", Kind: core.KindCompose, ComposePath: "docker-compose.yml"})
	if got := e.srv.protectionWarning(ctx, shop); !strings.Contains(got, "This will deploy the latest code from main") {
		t.Errorf("compose on main: warning = %q", got)
	}
	tagged := create(core.App{Name: "tagged", RepoURL: "https://x/t.git", Kind: core.KindCompose, ComposePath: "docker-compose.yml",
		DeployTrigger: core.TriggerTag, TagPattern: "v*"})
	if got := e.srv.protectionWarning(ctx, tagged); !strings.Contains(got, "This will deploy the newest tag matching v*") {
		t.Errorf("tag mode: warning = %q", got)
	}
	tmpl := create(core.App{Name: "blog", Source: core.SourceTemplate, Kind: core.KindCompose, TemplateID: "ghost", ComposeRaw: "services: {}\n"})
	if got := e.srv.protectionWarning(ctx, tmpl); got != "" {
		t.Errorf("template app (it redeploys its snapshot): warning = %q, want none", got)
	}

	web := create(core.App{Name: "web", RepoURL: "https://x/y.git", Domain: "web.test", Branch: "trunk"})
	if got := e.srv.protectionWarning(ctx, web); !strings.Contains(got, "This will deploy the latest code from trunk") {
		t.Errorf("never deployed: warning = %q", got)
	}
	seedFinishedDeploy(t, e, web.ID, "img:1")
	if got := e.srv.protectionWarning(ctx, web); got != "" {
		t.Errorf("a usable image: warning = %q, want none", got)
	}
	if _, err := e.store.CreateDeployment(ctx, web.ID); err != nil {
		t.Fatal(err)
	}
	if got := e.srv.protectionWarning(ctx, web); !strings.Contains(got, "This will deploy the latest code from trunk") {
		t.Errorf("a normal deploy in flight: warning = %q", got)
	}
}

// The app page says so when Restart will deploy instead, and names what will ship.
func TestRestartSaysItWillRedeploy(t *testing.T) {
	e, _ := newProtectionEnv(t)
	app := seedComposeStack(t, e)
	row := domainsOf(t, e, app.ID)[0]
	page := html.UnescapeString(body(t, e.get(t, "/apps/"+itoa(app.ID))))
	if strings.Contains(page, "Restart will deploy") {
		t.Error("labels and rows agree, yet the page says Restart will deploy")
	}
	setProtected(t, e, row, true)
	page = html.UnescapeString(body(t, e.get(t, "/apps/"+itoa(app.ID))))
	if !strings.Contains(page, "Restart will deploy") || !strings.Contains(page, "This will deploy the latest code from main") {
		t.Error("the page should say Restart will deploy, and what will ship")
	}
}

func TestDomainDialogHasSwitch(t *testing.T) {
	e, _ := newProtectionEnv(t)
	app := seedComposeStack(t, e)
	row := setProtected(t, e, domainsOf(t, e, app.ID)[0], true)
	path := "/apps/" + itoa(app.ID)
	switchTag := regexp.MustCompile(`<input[^>]*name="protected"[^>]*>`)

	raw := body(t, e.get(t, path))
	page := html.UnescapeString(raw)
	text := html.UnescapeString(regexp.MustCompile(`<[^>]*>`).ReplaceAllString(raw, ""))
	if !strings.Contains(text, "People must sign in to Outhaul before they reach this address. Everyone who can sign in gets through, and the app receives their name in the X-Outhaul-User header.") {
		t.Error("the switch's description is missing")
	}
	for _, want := range []string{
		"Require sign-in",
		"3 · HTTPS and sign-in",
		"redeploys this app",
		"This will deploy the latest code from main",
		"always served over HTTPS",
		`name="protected_offered"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("app page lacks %q", want)
		}
	}
	tag := switchTag.FindString(raw)
	if tag == "" || strings.Contains(tag, "disabled") {
		t.Errorf("switch = %q, want it there and enabled", tag)
	}
	if !strings.Contains(page, `"id":"`+itoa(row.ID)+`"`) || !strings.Contains(page, `"protected":"1"`) {
		t.Error("the edit button should pass the row's protection to the dialog")
	}

	for _, c := range []struct {
		publicURL string
		reason    string
	}{
		{"", "Set OUTHAUL_PUBLIC_URL so people can be sent to sign in."},
		{"https://slip.example.com", "Protected routes need HTTPS. Turn on HTTPS for the server, or use a tunnel."},
	} {
		e.srv.publicURL, e.srv.tlsEnabled = c.publicURL, false
		raw = body(t, e.get(t, path))
		if tag := switchTag.FindString(raw); !strings.Contains(tag, "disabled") {
			t.Errorf("public URL %q, no TLS: switch = %q, want it disabled", c.publicURL, tag)
		}
		if !strings.Contains(html.UnescapeString(raw), c.reason) {
			t.Errorf("public URL %q, no TLS: the page should say %q", c.publicURL, c.reason)
		}
	}

	// Only people who can change the app see the dialog.
	e.srv.publicURL, e.srv.tlsEnabled = "https://slip.example.com", true
	viewer := e.signInAs(t, "viewer", core.RoleProjectViewer, app.ProjectID)
	resp := e.getAs(t, viewer, path)
	if page := body(t, resp); resp.StatusCode != http.StatusOK || strings.Contains(page, "Require sign-in") {
		t.Errorf("viewer: status %d; want 200 without the switch", resp.StatusCode)
	}
}

// Note 1: turning protection on reconciles the proxy, with the row saved, so
// Traefik maps the gate's host before the app's deploy is queued.
func TestTurningOnReconcilesTheProxyFirst(t *testing.T) {
	e, px := newProtectionEnv(t)
	app, row, _ := liveWeb(t, e, "img:1")
	ctx := context.Background()
	var deploysAtReconcile int
	var protectedAtReconcile bool
	px.at = func() {
		deploysAtReconcile = len(deploymentsOf(t, e, app.ID))
		protectedAtReconcile, _ = e.store.AnyDomainProtected(ctx)
	}

	wantStatus(t, "turn the switch on", e.saveDomain(t, app.ID, row, domainForm(row, true)), http.StatusSeeOther)
	if px.calls != 1 || deploysAtReconcile != 1 || !protectedAtReconcile {
		t.Errorf("reconciles = %d, deployments at the reconcile = %d, row protected then = %v; want 1, 1 (before the deploy), true",
			px.calls, deploysAtReconcile, protectedAtReconcile)
	}
	if n := len(deploymentsOf(t, e, app.ID)); n != 2 {
		t.Errorf("deployments = %d, want the deploy queued after the reconcile", n)
	}

	wantStatus(t, "add a protected row", e.saveDomain(t, app.ID, core.Domain{}, domainForm(core.Domain{Host: "admin.test"}, true)), http.StatusSeeOther)
	if px.calls != 2 || deploysAtReconcile != 2 {
		t.Errorf("reconciles = %d, deployments at the reconcile = %d; want 2, 2", px.calls, deploysAtReconcile)
	}

	wantStatus(t, "add an open row", e.saveDomain(t, app.ID, core.Domain{}, domainForm(core.Domain{Host: "open.test"}, false)), http.StatusSeeOther)
	if px.calls != 2 {
		t.Errorf("an open row reconciled the proxy (%d calls)", px.calls)
	}
}

// Note 1: when the reconcile fails, the save is refused and the row stays as
// it was. With no proxy to reconcile, sign-in can't be turned on at all.
func TestProxyFailureRefusesTheSave(t *testing.T) {
	e, px := newProtectionEnv(t)
	app, row, _ := liveWeb(t, e, "img:1")
	px.err = errors.New("docker is down")
	notified := e.deployer.notified

	b := wantStatus(t, "add a protected row", e.saveDomain(t, app.ID, core.Domain{}, domainForm(core.Domain{Host: "admin.test"}, true)), http.StatusServiceUnavailable)
	if !strings.Contains(b, "docker is down") {
		t.Errorf("refusal = %q, want it to say why", b)
	}
	if rows := domainsOf(t, e, app.ID); len(rows) != 1 {
		t.Errorf("rows = %+v, want the protected row taken back", rows)
	}

	moved := row
	moved.Host = "moved.test"
	wantStatus(t, "turn a row on", e.saveDomain(t, app.ID, row, domainForm(moved, true)), http.StatusServiceUnavailable)
	if got := domainOf(t, e, row); got.Protected || got.Host != "web.test" {
		t.Errorf("row = %+v, want it as it was", got)
	}
	if n := len(deploymentsOf(t, e, app.ID)); n != 1 || e.deployer.notified != notified {
		t.Errorf("deployments = %d, notified %d times; want nothing queued", n, e.deployer.notified-notified)
	}

	e.srv.SetProxyControl(nil)
	wantStatus(t, "turn a row on with no proxy", e.saveDomain(t, app.ID, row, domainForm(row, true)), http.StatusServiceUnavailable)
	if domainOf(t, e, row).Protected {
		t.Error("the row was turned on with no proxy to reconcile")
	}
}

// protectionSyncer records whether any row was protected each time the local
// CA synced.
type protectionSyncer struct {
	e    *testEnv
	seen []bool
}

func (p *protectionSyncer) Sync(ctx context.Context) error {
	on, err := p.e.store.AnyDomainProtected(ctx)
	p.seen = append(p.seen, on)
	return err
}

// Note 3: every save that changes protection syncs the local CA, so a
// protected row with its TLS flag off gets its certificate at once.
func TestProtectionSaveSyncsCerts(t *testing.T) {
	e, _ := newProtectionEnv(t)
	app, row, _ := liveWeb(t, e, "img:1")
	ps := &protectionSyncer{e: e}
	e.srv.SetCertSync(ps)

	off := row
	off.TLS = false
	wantStatus(t, "turn the switch on, TLS off", e.saveDomain(t, app.ID, row, domainForm(off, true)), http.StatusSeeOther)
	if len(ps.seen) == 0 || !ps.seen[len(ps.seen)-1] {
		t.Fatalf("syncs = %v, want one after the row was protected", ps.seen)
	}
	wantStatus(t, "turn the switch off", e.saveDomain(t, app.ID, row, domainForm(off, false)), http.StatusSeeOther)
	if len(ps.seen) < 2 || ps.seen[len(ps.seen)-1] {
		t.Errorf("syncs = %v, want one after the row was opened", ps.seen)
	}
}

// Note 5: a row on Outhaul's own host can't be protected; the existing
// refusals stand, and nothing reconciles or deploys.
func TestOwnHostRowCantBeProtected(t *testing.T) {
	e, px := newProtectionEnv(t)
	app, _, _ := liveWeb(t, e, "img:1")
	own, err := e.store.AddDomain(context.Background(), core.Domain{AppID: app.ID, Host: "slip.example.com", Path: "/x", Port: deployAppPort})
	if err != nil {
		t.Fatal(err)
	}
	before := len(deploymentsOf(t, e, app.ID))
	b := wantStatus(t, "turn on a row on the own host", e.saveDomain(t, app.ID, own, domainForm(own, true)), http.StatusBadRequest)
	if !strings.Contains(html.UnescapeString(b), ownHostRowRefusal) {
		t.Errorf("refusal = %q", b)
	}
	b = wantStatus(t, "add a protected row on the own host", e.saveDomain(t, app.ID, core.Domain{}, domainForm(core.Domain{Host: "slip.example.com", Path: "/y"}, true)), http.StatusBadRequest)
	if !strings.Contains(html.UnescapeString(b), ownHostRefusal) {
		t.Errorf("refusal = %q", b)
	}
	if domainOf(t, e, own).Protected || px.calls != 0 || len(deploymentsOf(t, e, app.ID)) != before {
		t.Error("a row on Outhaul's own host was protected, or something reconciled or deployed")
	}
}
