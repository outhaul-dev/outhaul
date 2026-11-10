package server

import (
	"context"
	"errors"
	"html"
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
		{"off, host moved, the old address gated", row(false), []docker.Container{movedHost}, nil, "Open", "state-stopped", "", 0},
		{"on, path cleared, running", row(true), []docker.Container{ctr("running", webPath(true))}, nil, "Not protected yet", "state-building", "", 0},
		// The same address in another spelling is the row's.
		{"on, labels from before the traversal term", row(true), []docker.Container{oldForm(gated)}, nil, "Sign-in required", "state-running", "", 0},
		{"off, labels from before the traversal term", row(false), []docker.Container{oldForm(open)}, nil, "Open", "state-stopped", "", 0},
		{"on, the path's labels from before the term", webPath(true), []docker.Container{oldForm(ctr("running", webPath(true)))}, nil, "Sign-in required", "state-running", "", 0},
		{"on, the host in other case", row(true), []docker.Container{ctr("running", at("WEB.Test", ""))}, nil, "Sign-in required", "state-running", "", 0},
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
