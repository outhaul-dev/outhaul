package server

import (
	"context"
	"html"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/outhaul-dev/outhaul/internal/core"
)

// sharedHosts is a server where the default project's app "wiki" has
// wiki.test (protected), shut.test/admin (protected), open.test/x (open) and
// blog.test (open), and the project "other" has the app "shop", with
// shop.test and open.test/z (both open). mo is a member of "other" only.
type sharedHosts struct {
	e         *testEnv
	wiki      core.App
	shop      core.App
	shopRow   core.Domain // shop.test
	sharedRow core.Domain // open.test/z
	other     core.Project
	mo        string // mo's session
}

func newSharedHosts(t *testing.T) sharedHosts {
	t.Helper()
	ctx := context.Background()
	e, _ := newProtectionEnv(t)
	wiki, err := e.store.CreateApp(ctx, core.App{Name: "wiki", RepoURL: "https://x/wiki.git", Kind: core.KindCompose})
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []core.Domain{
		{Host: "wiki.test", Protected: true}, {Host: "shut.test", Path: "/admin", Protected: true},
		{Host: "open.test", Path: "/x"}, {Host: "blog.test"},
	} {
		d.AppID, d.Service, d.Port = wiki.ID, "web", 80
		addRow(t, e, d)
	}
	other, err := e.store.CreateProject(ctx, core.Project{Name: "other"})
	if err != nil {
		t.Fatal(err)
	}
	shop, err := e.store.CreateApp(ctx, core.App{Name: "shop", RepoURL: "https://x/shop.git", Kind: core.KindCompose, ProjectID: other.ID})
	if err != nil {
		t.Fatal(err)
	}
	row := addRow(t, e, core.Domain{AppID: shop.ID, Host: "shop.test", Service: "web", Port: 80})
	shared := addRow(t, e, core.Domain{AppID: shop.ID, Host: "open.test", Path: "/z", Service: "web", Port: 80})
	return sharedHosts{e: e, wiki: wiki, shop: shop, shopRow: row, sharedRow: shared, other: other,
		mo: e.signInAs(t, "mo", core.RoleProjectMember, other.ID)}
}

// A sign-in on a protected address is good for the whole host, so someone
// who isn't a system admin can't put a route on a host where another
// project's app requires sign-in, or require sign-in where another project's
// app has any route. A system admin can; apps in one project are unaffected.
func TestProtectedHostBelongsToOneProject(t *testing.T) {
	const refusal = "Another project already uses this address, so a system admin has to set this up."
	const (
		add    = iota
		edit   // shop.test
		shared // open.test/z
	)
	type save struct {
		row        int
		host, path string
		protected  bool
	}
	for _, c := range []struct {
		name    string
		save    save
		refused bool
	}{
		{"add an open path under another project's protected host", save{host: "wiki.test", path: "/hooks"}, true},
		{"add an open host where another project protects a path", save{host: "shut.test"}, true},
		{"add a protected row where another project has an open row", save{host: "open.test", path: "/y", protected: true}, true},
		{"add an open row where another project has only open rows", save{host: "open.test", path: "/y"}, false},
		{"add a protected row on a host nobody else uses", save{host: "new.test", protected: true}, false},
		{"move a row onto another project's protected host", save{edit, "wiki.test", "/hooks", false}, true},
		{"move a protected row where another project has an open row", save{edit, "blog.test", "/shop", true}, true},
		{"turn sign-in on where another project has an open row", save{shared, "open.test", "/z", true}, true},
		{"edit an open row where another project has only open rows", save{shared, "open.test", "/zz", false}, false},
		{"edit a row on a host nobody else uses", save{edit, "shop.test", "/v2", false}, false},
	} {
		for _, who := range []string{"member", "admin"} {
			t.Run(c.name+"/"+who, func(t *testing.T) {
				h := newSharedHosts(t)
				path := "/apps/" + itoa(h.shop.ID) + "/domains"
				f := domainForm(core.Domain{Host: c.save.host, Path: c.save.path, Service: "web", Port: 80}, c.save.protected)
				switch c.save.row {
				case edit:
					path += "/" + itoa(h.shopRow.ID)
				case shared:
					path += "/" + itoa(h.sharedRow.ID)
				}
				var resp *http.Response
				if who == "admin" {
					resp = h.e.postForm(t, path, f)
				} else {
					resp = h.e.postAs(t, h.mo, path, f)
				}
				refused := c.refused && who == "member"
				b := body(t, resp)
				if refused {
					if resp.StatusCode != http.StatusBadRequest || strings.TrimSpace(b) != refusal {
						t.Errorf("status %d, body %q; want 400 %q", resp.StatusCode, b, refusal)
					}
				} else if resp.StatusCode != http.StatusSeeOther {
					t.Errorf("status %d, body %q; want the save (303)", resp.StatusCode, b)
				}
				rows := domainsOf(t, h.e, h.shop.ID)
				saved := false
				for _, d := range rows {
					if d.Host == c.save.host && d.Path == c.save.path && d.Protected == c.save.protected {
						saved = true
					}
				}
				if saved == refused {
					t.Errorf("shop's rows = %+v; saved %v, want %v", rows, saved, !refused)
				}
			})
		}
	}

	// A member of wiki's own project can share wiki's protected host.
	t.Run("same project", func(t *testing.T) {
		h := newSharedHosts(t)
		ctx := context.Background()
		notes, err := h.e.store.CreateApp(ctx, core.App{Name: "notes", RepoURL: "https://x/notes.git", Kind: core.KindCompose})
		if err != nil {
			t.Fatal(err)
		}
		pat := h.e.signInAs(t, "pat", core.RoleProjectMember, core.DefaultProjectID)
		f := domainForm(core.Domain{Host: "wiki.test", Path: "/notes", Service: "web", Port: 80}, true)
		wantStatus(t, "add a protected path under wiki's host", h.e.postAs(t, pat, "/apps/"+itoa(notes.ID)+"/domains", f), http.StatusSeeOther)
	})
}

// Creating an app puts its first route on a host: refused, for someone who
// isn't a system admin, where another project's app requires sign-in.
func TestCreatingAnAppOnAnotherProjectsProtectedHost(t *testing.T) {
	const refusal = "Another project already uses this address, so a system admin has to set this up."
	for _, kind := range []string{core.KindNixpacks, core.KindCompose} {
		for _, who := range []string{"member", "admin"} {
			t.Run(kind+"/"+who, func(t *testing.T) {
				h := newSharedHosts(t)
				f := url.Values{"name": {"intruder"}, "kind": {kind}, "source": {"public"},
					"repo_url": {"https://x/intruder.git"}, "branch": {"main"}, "domain": {"shut.test"},
					"project_id": {itoa(h.other.ID)}}
				if kind == core.KindCompose {
					f.Set("compose_service", "web")
					f.Set("compose_port", "80")
				}
				var resp *http.Response
				if who == "admin" {
					resp = h.e.postForm(t, "/apps", f)
				} else {
					resp = h.e.postAs(t, h.mo, "/apps", f)
				}
				b := html.UnescapeString(body(t, resp))
				_, err := h.e.store.GetAppByName(context.Background(), "intruder")
				if who == "admin" {
					if resp.StatusCode != http.StatusSeeOther || err != nil {
						t.Errorf("status %d (app: %v), want it created", resp.StatusCode, err)
					}
					return
				}
				if resp.StatusCode != http.StatusBadRequest || !strings.Contains(b, refusal) {
					t.Errorf("status %d, want 400 saying %q:\n%s", resp.StatusCode, refusal, b)
				}
				if err == nil {
					t.Error("the app was created")
				}
			})
		}
	}
}

// A route another app already serves is refused naming that app only to
// someone who may see its project; anyone else is told "another app".
func TestTakenRouteNamesOnlyAVisibleApp(t *testing.T) {
	for _, c := range []struct {
		name string
		save func(t *testing.T, h sharedHosts, tok string) *http.Response
	}{
		{"add", func(t *testing.T, h sharedHosts, tok string) *http.Response {
			f := domainForm(core.Domain{Host: "blog.test", Service: "web", Port: 80}, false)
			return h.e.postAs(t, tok, "/apps/"+itoa(h.shop.ID)+"/domains", f)
		}},
		{"edit", func(t *testing.T, h sharedHosts, tok string) *http.Response {
			f := domainForm(core.Domain{Host: "blog.test", Service: "web", Port: 80}, false)
			return h.e.postAs(t, tok, "/apps/"+itoa(h.shop.ID)+"/domains/"+itoa(h.shopRow.ID), f)
		}},
		{"create", func(t *testing.T, h sharedHosts, tok string) *http.Response {
			return h.e.postAs(t, tok, "/apps", url.Values{"name": {"intruder"}, "kind": {core.KindNixpacks}, "source": {"public"},
				"repo_url": {"https://x/intruder.git"}, "domain": {"blog.test"}, "project_id": {itoa(h.other.ID)}})
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newSharedHosts(t)
			both := h.e.signInAs(t, "bo", core.RoleProjectMember, core.DefaultProjectID, h.other.ID)
			for _, v := range []struct {
				who, tok string
				named    bool
			}{{"mo", h.mo, false}, {"bo", both, true}} {
				b := html.UnescapeString(body(t, c.save(t, h, v.tok)))
				want := "blog.test is served by another app"
				if v.named {
					want = `blog.test is served by app "wiki"`
				}
				if !strings.Contains(b, want) || !v.named && strings.Contains(b, `app "wiki"`) {
					t.Errorf("%s: the refusal should say %q, and name wiki only then:\n%s", v.who, want, b)
				}
			}
		})
	}
}
