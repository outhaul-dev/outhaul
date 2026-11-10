package server

import (
	"context"
	"errors"
	"html"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/outhaul-dev/outhaul/internal/core"
	"github.com/outhaul-dev/outhaul/internal/docker"
	"github.com/outhaul-dev/outhaul/internal/traefik"
)

func TestAccessStateFor(t *testing.T) {
	app := core.App{ID: 1, Name: "web"}
	row := func(protected bool) core.Domain {
		return core.Domain{ID: 7, AppID: 1, Host: "web.test", Protected: protected}
	}
	// ctr is a container labelled as a deploy of the given rows labels it.
	ctr := func(state string, rows ...core.Domain) docker.Container {
		return docker.Container{ID: "c", State: state,
			Labels: traefik.AppLabels(app, rows, 3000, true, "le", testGateAddress)}
	}
	gated := ctr("running", row(true))
	open := ctr("running", row(false))
	dep := func(id int64, s core.DeployStatus) *core.Deployment { return &core.Deployment{ID: id, Status: s} }
	// at is row(true) at another address: what the row was before an edit
	// moved it, so its router in the labels names that address.
	at := func(host, path string) core.Domain {
		d := row(true)
		d.Host, d.Path = host, path
		return d
	}
	movedHost := ctr("running", at("old.test", ""))
	movedPath := ctr("running", at("web.test", "/old"))
	webPath := func(protected bool) core.Domain { d := row(protected); d.Path = "/old"; return d }

	cases := []struct {
		name       string
		d          core.Domain
		cs         []docker.Container
		latest     *core.Deployment
		label      string
		class      string
		note       string
		deployment int64
	}{
		{"on, gated", row(true), []docker.Container{gated}, nil, "Sign-in required", "state-running", "", 0},
		{"on, lacks gate, deploying", row(true), []docker.Container{open}, dep(5, core.StatusDeploying), "Not protected yet", "state-building", "redeploying", 5},
		{"on, lacks gate, queued", row(true), []docker.Container{open}, dep(5, core.StatusQueued), "Not protected yet", "state-building", "redeploying", 5},
		{"on, lacks gate, building", row(true), []docker.Container{open}, dep(5, core.StatusBuilding), "Not protected yet", "state-building", "redeploying", 5},
		{"on, lacks gate, failed", row(true), []docker.Container{open}, dep(6, core.StatusFailed), "Not protected yet", "state-failed", "the last deploy failed", 6},
		{"on, new row absent from labels", row(true), []docker.Container{ctr("running")}, nil, "Not protected yet", "state-building", "", 0},
		{"off, gated", row(false), []docker.Container{gated}, nil, "Sign-in required", "state-running", "until the redeploy finishes", 0},
		{"off, open", row(false), []docker.Container{open}, nil, "Open", "state-stopped", "", 0},
		{"on, not running", row(true), []docker.Container{ctr("exited", row(true))}, nil, "Sign-in required", "state-running", "applies on the next deploy", 0},
		{"off, no containers", row(false), nil, nil, "Open", "state-stopped", "applies on the next deploy", 0},
		{"on, only a stopped container has the gate", row(true), []docker.Container{ctr("exited", row(true)), open}, nil, "Not protected yet", "state-building", "", 0},
		{"on, cutover, only the newer has the gate", row(true), []docker.Container{open, gated}, dep(5, core.StatusDeploying), "Not protected yet", "state-building", "redeploying", 5},
		{"on, cutover, both have it", row(true), []docker.Container{gated, gated}, nil, "Sign-in required", "state-running", "", 0},
		// The row's host or path was edited: the running container has its
		// router, gated, on the old address, which isn't the row's.
		{"on, host moved, running", row(true), []docker.Container{movedHost}, nil, "Not protected yet", "state-building", "", 0},
		{"on, path moved, running", row(true), []docker.Container{movedPath}, nil, "Not protected yet", "state-building", "", 0},
		{"on, host moved, redeploying", row(true), []docker.Container{movedHost}, dep(5, core.StatusBuilding), "Not protected yet", "state-building", "redeploying", 5},
		{"on, host moved, the deploy failed", row(true), []docker.Container{movedHost}, dep(6, core.StatusFailed), "Not protected yet", "state-failed", "the last deploy failed", 6},
		{"on, path moved, the deploy failed", row(true), []docker.Container{movedPath}, dep(6, core.StatusFailed), "Not protected yet", "state-failed", "the last deploy failed", 6},
		{"on, host moved, stopped", row(true), []docker.Container{ctr("exited", at("old.test", ""))}, nil, "Sign-in required", "state-running", "applies on the next deploy", 0},
		{"off, host moved, the old address gated", row(false), []docker.Container{movedHost}, nil, "Open", "state-stopped", "applies on the next deploy", 0},
		{"on, path cleared, running", row(true), []docker.Container{ctr("running", webPath(true))}, nil, "Not protected yet", "state-building", "", 0},
		// The same address in another spelling is the row's.
		{"on, labels from before the traversal term", row(true), []docker.Container{oldForm(gated)}, nil, "Sign-in required", "state-running", "", 0},
		{"off, labels from before the traversal term", row(false), []docker.Container{oldForm(open)}, nil, "Open", "state-stopped", "", 0},
		{"on, the path's labels from before the term", webPath(true), []docker.Container{oldForm(ctr("running", webPath(true)))}, nil, "Sign-in required", "state-running", "", 0},
		{"on, the host in other case", row(true), []docker.Container{ctr("running", at("WEB.Test", ""))}, nil, "Sign-in required", "state-running", "", 0},
		// An open row no running container serves yet.
		{"off, a new row, running", row(false), []docker.Container{ctr("running")}, nil, "Open", "state-stopped", "applies on the next deploy", 0},
		{"on, a running container lacks the router, a stopped one has it gated", row(true), []docker.Container{ctr("running"), ctr("exited", row(true))}, nil, "Not protected yet", "state-building", "", 0},
		// A restarting container is about to answer again, as its labels say.
		{"on, restarting without the gate", row(true), []docker.Container{ctr("restarting", row(false))}, nil, "Not protected yet", "state-building", "", 0},
		{"on, restarting with the gate", row(true), []docker.Container{ctr("restarting", row(true))}, nil, "Sign-in required", "state-running", "", 0},
		{"off, restarting", row(false), []docker.Container{ctr("restarting", row(false))}, nil, "Open", "state-stopped", "", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := accessStateFor(c.d, app.Name, c.cs, c.latest)
			want := accessState{Label: c.label, Class: c.class, Note: c.note, DeploymentID: c.deployment}
			if got != want {
				t.Errorf("accessStateFor = %+v, want %+v", got, want)
			}
		})
	}
}

// twoRowApp is the app "web" whose container's labels gate its first row and
// not its second, whatever the stored switch says: row one is stored open and
// row two is stored protected.
func twoRowApp(t *testing.T, e *testEnv) core.App {
	t.Helper()
	ctx := context.Background()
	app, err := e.store.CreateApp(ctx, core.App{Name: "web", RepoURL: "https://x/y.git", Domain: "one.test"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := e.store.AddDomain(ctx, core.Domain{AppID: app.ID, Host: "two.test", Port: 3000})
	if err != nil {
		t.Fatal(err)
	}
	rows := domainsOf(t, e, app.ID)
	labelRows := append([]core.Domain(nil), rows...)
	for i := range labelRows {
		labelRows[i].Protected = labelRows[i].ID != second.ID
	}
	setProtected(t, e, second, true)
	e.runContainers(labelled("c1", app, labelRows, "running"))
	return app
}

func accessPagePath(page string, app core.App) string {
	if page == "domains" {
		return "/domains"
	}
	return "/apps/" + itoa(app.ID)
}

func TestAppPageAndDomainsPageShowAccess(t *testing.T) {
	for _, page := range []string{"app", "domains"} {
		t.Run(page, func(t *testing.T) {
			e, _ := newProtectionEnv(t)
			app := twoRowApp(t, e)
			got := html.UnescapeString(body(t, e.get(t, accessPagePath(page, app))))
			if !strings.Contains(got, "Sign-in required") || !strings.Contains(got, "Not protected yet") {
				t.Errorf("%s page lacks the two states from the labels:\n%s", page, got)
			}
			if strings.Contains(got, "Unknown") {
				t.Errorf("%s page says Unknown though Docker answered", page)
			}
		})
	}
}

func TestAccessUnknownWhenDockerCantBeAsked(t *testing.T) {
	for _, page := range []string{"app", "domains"} {
		t.Run(page, func(t *testing.T) {
			e, _ := newProtectionEnv(t)
			app := twoRowApp(t, e)
			e.runtime.listErr = errors.New("docker down")
			got := body(t, e.get(t, accessPagePath(page, app)))
			if !strings.Contains(got, "Unknown") {
				t.Errorf("want Unknown rows:\n%s", got)
			}
			if strings.Contains(got, "Sign-in required") || strings.Contains(got, "Not protected yet") || strings.Contains(got, ">Open<") {
				t.Errorf("must not claim a state when Docker can't be asked:\n%s", got)
			}
		})
	}
}

func TestViewerSeesAccessButNoControls(t *testing.T) {
	e, _ := newProtectionEnv(t)
	app := twoRowApp(t, e)
	app, err := e.store.GetApp(context.Background(), app.ID)
	if err != nil {
		t.Fatal(err)
	}
	viewer := e.signInAs(t, "viewer", core.RoleProjectViewer, app.ProjectID)
	got := body(t, e.getAs(t, viewer, "/apps/"+itoa(app.ID)))
	if !strings.Contains(got, "Sign-in required") || !strings.Contains(got, "Not protected yet") {
		t.Errorf("a viewer should see the access column:\n%s", got)
	}
	if strings.Contains(got, "openDomainDialog(") {
		t.Errorf("a viewer must see no edit control")
	}
}

func TestProtectedRowLinksOverHTTPSWithTLSFlagOff(t *testing.T) {
	e, _ := newProtectionEnv(t)
	app := twoRowApp(t, e) // row two is protected with its TLS flag off
	for _, page := range []string{"app", "domains"} {
		got := body(t, e.get(t, accessPagePath(page, app)))
		if !strings.Contains(got, `href="https://two.test`) {
			t.Errorf("%s: protected row should link over https:\n%s", page, got)
		}
		if strings.Contains(got, `href="http://two.test`) {
			t.Errorf("%s: protected row linked over http", page)
		}
	}
}

// A protected row with its TLS flag off is served over HTTPS on a TLS server
// and through a tunnel, whose edge terminates TLS; with neither, nothing
// serves it over HTTPS (and sign-in can't be turned on). Its link and badge
// say which, on the app page and the Domains page.
func TestProtectedRowSchemeFollowsTheMode(t *testing.T) {
	rowOf := regexp.MustCompile(`(?s)<tr>\s*<td><a href="[^"]*two\.test.*?</tr>`)
	for _, c := range []struct {
		name        string
		tls, tunnel bool
		https       bool
	}{
		{"a TLS server", true, false, true},
		{"a tunnel", false, true, true},
		{"neither", false, false, false},
	} {
		for _, page := range []string{"app", "domains"} {
			t.Run(c.name+", "+page+" page", func(t *testing.T) {
				e, _ := newProtectionEnv(t)
				app := twoRowApp(t, e) // row two is protected with its TLS flag off
				e.srv.tlsEnabled = c.tls
				if c.tunnel {
					if err := e.store.SetCloudflareToken(context.Background(), "tok"); err != nil {
						t.Fatal(err)
					}
				}
				row := rowOf.FindString(body(t, e.get(t, accessPagePath(page, app))))
				if row == "" {
					t.Fatal("no row for two.test")
				}
				scheme, badge := "http://", ">http<"
				if c.https {
					scheme, badge = "https://", ">TLS<"
				}
				if !strings.Contains(row, `href="`+scheme+`two.test`) || !strings.Contains(row, badge) {
					t.Errorf("row = %s\nwant a %s link and %s", row, scheme, badge)
				}
			})
		}
	}
}

func TestDomainsPageAsksDockerOnce(t *testing.T) {
	e, _ := newProtectionEnv(t)
	twoRowApp(t, e)
	e.runtime.listCalls = 0
	body(t, e.get(t, "/domains"))
	if e.runtime.listCalls != 1 {
		t.Errorf("ListContainers calls = %d, want 1", e.runtime.listCalls)
	}
}

// strayRoute finds a router, on another container or not a current row, that
// answers part of a protected row's address without the gate.
func TestStrayRoute(t *testing.T) {
	web := core.App{Name: "web"}
	shut := core.Domain{ID: 1, AppID: 1, Host: "web.test", Protected: true}
	admin := core.Domain{ID: 2, AppID: 1, Host: "web.test", Path: "/admin", Protected: true}
	other := core.App{Name: "hooks"}
	// route is one container of app with one row's labels, running.
	route := func(app core.App, d core.Domain) docker.Container {
		return docker.Container{ID: app.Name + "-c", State: "running",
			Labels: traefik.AppLabels(app, []core.Domain{d}, 3000, true, "le", testGateAddress)}
	}
	open := func(id int64, host, path string) core.Domain {
		return core.Domain{ID: id, AppID: 2, Host: host, Path: path}
	}
	custom := func(rule string) docker.Container {
		return docker.Container{ID: "custom", State: "running", Labels: map[string]string{
			"traefik.enable": "true", "outhaul.managed": "true", "outhaul.app": "hooks",
			"traefik.http.routers.mine.rule": rule}}
	}
	disabled := route(other, open(9, "web.test", "/x"))
	disabled.Labels["traefik.enable"] = "false"
	rowsOf := func(ds ...core.Domain) map[string]core.Domain {
		m := map[string]core.Domain{}
		for _, d := range ds {
			name := web.Name
			if d.AppID == 2 {
				name = other.Name
			}
			m[traefik.RouterName(name, d.ID)] = d
		}
		return m
	}
	for _, c := range []struct {
		name  string
		d     core.Domain
		cs    []docker.Container
		rows  map[string]core.Domain
		owner string // "" when nothing is found
	}{
		{"only the row's own router", shut, []docker.Container{route(web, shut)}, rowsOf(shut), ""},
		{"a current open row of another app", shut, []docker.Container{route(web, shut), route(other, open(9, "web.test", "/hooks"))}, rowsOf(shut, open(9, "web.test", "/hooks")), ""},
		{"another app's removed open row", shut, []docker.Container{route(web, shut), route(other, open(9, "web.test", "/hooks"))}, rowsOf(shut), "hooks"},
		{"another app's open row, since moved", shut, []docker.Container{route(other, open(9, "web.test", "/hooks"))}, rowsOf(shut, open(9, "web.test", "/webhooks")), "hooks"},
		{"this app's removed open row", shut, []docker.Container{route(web, core.Domain{ID: 3, AppID: 1, Host: "web.test", Path: "/hooks"})}, rowsOf(shut), "web"},
		{"the same path", admin, []docker.Container{route(other, open(9, "web.test", "/admin"))}, rowsOf(admin), "hooks"},
		{"a path under the row's", admin, []docker.Container{route(other, open(9, "web.test", "/admin/x"))}, rowsOf(admin), "hooks"},
		{"a path sharing the row's prefix", admin, []docker.Container{route(other, open(9, "web.test", "/administrator"))}, rowsOf(admin), "hooks"},
		{"a path above the row's", admin, []docker.Container{route(other, open(9, "web.test", ""))}, rowsOf(admin), ""},
		{"a path in other case", admin, []docker.Container{route(other, open(9, "web.test", "/ADMIN"))}, rowsOf(admin), ""},
		{"another host", shut, []docker.Container{route(other, open(9, "x.test", ""))}, rowsOf(shut), ""},
		{"the host in other case", shut, []docker.Container{route(other, open(9, "WEB.test", "/x"))}, rowsOf(shut), "hooks"},
		{"a gated router", shut, []docker.Container{route(other, core.Domain{ID: 9, AppID: 2, Host: "web.test", Path: "/x", Protected: true})}, rowsOf(shut), ""},
		{"a current protected row without the gate yet", shut, []docker.Container{route(other, open(9, "web.test", "/x"))}, rowsOf(shut, core.Domain{ID: 9, AppID: 2, Host: "web.test", Path: "/x", Protected: true}), "hooks"},
		{"labels from before the traversal term", shut, []docker.Container{oldForm(route(other, open(9, "web.test", "/hooks")))}, rowsOf(shut), "hooks"},
		{"an orphaned container", shut, []docker.Container{route(core.App{Name: "gone"}, open(9, "web.test", "/x"))}, rowsOf(shut), "gone"},
		{"a container Traefik doesn't route", shut, []docker.Container{disabled}, rowsOf(shut), ""},
		{"a stopped container", shut, []docker.Container{{ID: "s", State: "exited", Labels: route(other, open(9, "web.test", "/x")).Labels}}, rowsOf(shut), ""},
		{"a restarting container", shut, []docker.Container{{ID: "s", State: "restarting", Labels: route(other, open(9, "web.test", "/x")).Labels}}, rowsOf(shut), "hooks"},
		{"a rule it can't read, naming the host", shut, []docker.Container{custom("Host(`web.test`) && Path(`/x`)")}, rowsOf(shut), "hooks"},
		{"a rule it can't read, naming another host", shut, []docker.Container{custom("Host(`x.test`) && Path(`/x`)")}, rowsOf(shut), ""},
		{"this app's is named first", shut, []docker.Container{route(other, open(9, "web.test", "/x")), route(web, core.Domain{ID: 3, AppID: 1, Host: "web.test", Path: "/hooks"})}, rowsOf(shut), "web"},
	} {
		owner, found := strayRoute(c.d, web.Name, c.cs, c.rows)
		if found != (c.owner != "") || owner != c.owner {
			t.Errorf("%s: strayRoute = %q, %v; want %q", c.name, owner, found, c.owner)
		}
	}
}

// A protected row whose own router has the gate still isn't protected while
// any other running container answers part of its address without it. Both
// pages say so, naming the app as far as the viewer may see it.
func TestOtherRoutesOnAProtectedAddress(t *testing.T) {
	const (
		ownNote     = "an old route of this app still answers part of this address"
		hooksNote   = "hooks still answers part of this address"
		anotherNote = "another app still answers part of this address"
	)
	ctx := context.Background()
	// setup leaves web's row web.test protected, and its container gated,
	// running; it returns web.
	type setup func(t *testing.T, e *testEnv, web core.App, row core.Domain)
	hooksApp := func(t *testing.T, e *testEnv, project int64) core.App {
		t.Helper()
		a, err := e.store.CreateApp(ctx, core.App{Name: "hooks", RepoURL: "https://x/hooks.git", ProjectID: project})
		if err != nil {
			t.Fatal(err)
		}
		seedFinishedDeploy(t, e, a.ID, "hooks:1")
		return a
	}
	failDeploy := func(t *testing.T, e *testEnv, appID int64) {
		t.Helper()
		d, err := e.store.CreateDeployment(ctx, appID)
		if err != nil {
			t.Fatal(err)
		}
		if ok, err := e.store.ClaimDeployment(ctx, d.ID); err != nil || !ok {
			t.Fatalf("claim: %v %v", ok, err)
		}
		mustTransition(t, e, d.ID, core.StatusBuilding, core.StatusFailed)
	}
	// leftover is hooks with an open /hooks row on web.test that its running
	// container still serves, though the row is gone and its redeploy failed.
	leftover := func(project int64) setup {
		return func(t *testing.T, e *testEnv, web core.App, row core.Domain) {
			hooks := hooksApp(t, e, project)
			hook := addRow(t, e, core.Domain{AppID: hooks.ID, Host: "web.test", Path: "/hooks", Port: deployAppPort})
			e.runContainers(labelled("c1", web, []core.Domain{row}, "running"), labelled("h1", hooks, []core.Domain{hook}, "running"))
			if err := e.store.DeleteDomain(ctx, hooks.ID, hook.ID); err != nil {
				t.Fatal(err)
			}
			failDeploy(t, e, hooks.ID)
		}
	}
	for _, c := range []struct {
		name   string
		setup  setup
		viewer bool   // seen by a viewer of web's project only
		note   string // "" when the row requires sign-in
	}{
		{"an open exception removed, the deploy failed", func(t *testing.T, e *testEnv, web core.App, row core.Domain) {
			hook := addRow(t, e, core.Domain{AppID: web.ID, Host: "web.test", Path: "/hooks", Port: deployAppPort})
			e.runContainers(labelled("c1", web, []core.Domain{row, hook}, "running"))
			if err := e.store.DeleteDomain(ctx, web.ID, hook.ID); err != nil {
				t.Fatal(err)
			}
			failDeploy(t, e, web.ID)
		}, false, ownNote},
		{"another app's leftover, its redeploy failed", leftover(core.DefaultProjectID), false, hooksNote},
		{"another app's leftover, in a project the viewer doesn't share", nil, true, anotherNote},
		{"an orphaned container", func(t *testing.T, e *testEnv, web core.App, row core.Domain) {
			gone := labelled("g1", core.App{Name: "gone"}, []core.Domain{{ID: 99, Host: "web.test", Path: "/x"}}, "running")
			e.runContainers(labelled("c1", web, []core.Domain{row}, "running"), gone)
		}, false, anotherNote},
		{"a current open row of another app", func(t *testing.T, e *testEnv, web core.App, row core.Domain) {
			hooks := hooksApp(t, e, core.DefaultProjectID)
			hook := addRow(t, e, core.Domain{AppID: hooks.ID, Host: "web.test", Path: "/hooks", Port: deployAppPort})
			e.runContainers(labelled("c1", web, []core.Domain{row}, "running"), labelled("h1", hooks, []core.Domain{hook}, "running"))
		}, false, ""},
		{"web stopped, another app's leftover", func(t *testing.T, e *testEnv, web core.App, row core.Domain) {
			leftover(core.DefaultProjectID)(t, e, web, row)
			e.runtime.stack[0].State = "exited"
			e.runtime.container.State = "exited"
		}, false, hooksNote},
	} {
		for _, page := range []string{"app", "domains"} {
			t.Run(c.name+"/"+page, func(t *testing.T) {
				e, _ := newProtectionEnv(t)
				web, row, _ := liveWeb(t, e, "img:1")
				row = setProtected(t, e, row, true)
				get := func(path string) string { return body(t, e.get(t, path)) }
				if c.viewer {
					p, err := e.store.CreateProject(ctx, core.Project{Name: "elsewhere"})
					if err != nil {
						t.Fatal(err)
					}
					leftover(p.ID)(t, e, web, row)
					tok := e.signInAs(t, "vera", core.RoleProjectViewer, web.ProjectID)
					get = func(path string) string { return body(t, e.getAs(t, tok, path)) }
				} else {
					c.setup(t, e, web, row)
				}
				got := html.UnescapeString(get(accessPagePath(page, web)))
				if c.note == "" {
					if !strings.Contains(got, accessSignIn) || strings.Contains(got, accessNotYet) {
						t.Errorf("want %q and never %q:\n%s", accessSignIn, accessNotYet, got)
					}
					return
				}
				if !strings.Contains(got, accessNotYet) || !strings.Contains(got, c.note) {
					t.Errorf("want %q, noting %q:\n%s", accessNotYet, c.note, got)
				}
				if strings.Contains(got, accessSignIn) {
					t.Errorf("the page still says %q:\n%s", accessSignIn, got)
				}
				if c.note == anotherNote && strings.Contains(got, ">hooks<") {
					t.Errorf("the page names an app the viewer can't see:\n%s", got)
				}
			})
		}
	}
}

// A container Docker is restarting serves its labels again once it is up, so
// a protection save redeploys its app as it does a running one.
func TestProtectionSaveRedeploysARestartingApp(t *testing.T) {
	e, _ := newProtectionEnv(t)
	app, row, _ := liveWeb(t, e, "img:1")
	e.runtime.stack[0].State = "restarting"
	e.runtime.container.State = "restarting"
	before := len(deploymentsOf(t, e, app.ID))
	wantStatus(t, "turn the switch on", e.saveDomain(t, app.ID, row, domainForm(row, true)), http.StatusSeeOther)
	if n := len(deploymentsOf(t, e, app.ID)); n != before+1 {
		t.Errorf("deployments %d -> %d, want one queued", before, n)
	}
}

// An app in a project the person can't see is named only generically, with
// no word of what it will deploy, so the line says it may deploy its latest
// code. Several on one host make one line.
func TestHiddenAppLines(t *testing.T) {
	one := otherRedeploy{Host: "web.test", Hidden: true, App: core.App{ID: 1, Name: "zephyr"}, Ships: "the latest code from moonbeam"}
	two := one
	two.App.ID = 2
	if got, want := one.Sentence(), "Requiring sign-in on web.test also redeploys an app in a project you don't share. It may deploy its latest code."; got != want {
		t.Errorf("Sentence = %q, want %q", got, want)
	}
	lines := alsoLines([]otherRedeploy{one, two})
	if len(lines) != 1 || lines[0].Sentence != "Requiring sign-in on web.test also redeploys 2 apps in projects you don't share. They may deploy their latest code." {
		t.Errorf("alsoLines = %+v", lines)
	}
	if lines := alsoLines([]otherRedeploy{one}); len(lines) != 1 || lines[0].Sentence != one.Sentence() {
		t.Errorf("alsoLines of one = %+v, want its Sentence", lines)
	}
}
