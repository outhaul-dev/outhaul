package server

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/outhaul-dev/outhaul/internal/core"
)

var listPages = []string{"/", "/projects", "/apps", "/databases", "/domains"}

func TestListPagesShowOnlySharedProjects(t *testing.T) {
	e := newTestEnv(t)
	a := seedProject(t, e, "zebra")
	b := seedProject(t, e, "quokka")
	token := e.signInAs(t, "dana", core.RoleProjectMember, a.Project.ID)

	for _, path := range listPages {
		resp := e.getAs(t, token, path)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s: status %d", path, resp.StatusCode)
		}
		got := body(t, resp)
		if !strings.Contains(got, "zebra") {
			t.Errorf("GET %s: missing the shared project", path)
		}
		if strings.Contains(got, "quokka") {
			t.Errorf("GET %s: shows a project the person is not on", path)
		}
	}

	got := body(t, e.getAs(t, token, "/"))
	if !regexp.MustCompile(`Apps</div><div class="stat-value">1<`).MatchString(got) {
		t.Errorf("overview should count one app; body:\n%s", got)
	}
	if strings.Contains(got, b.App.Name) || !strings.Contains(got, a.App.Name) {
		t.Errorf("overview recent deployments should list only the shared project's")
	}
}

func TestPersonWithNoSharedProjectsSeesEmptyPages(t *testing.T) {
	e := newTestEnv(t)
	seedProject(t, e, "zebra")
	token := e.signInAs(t, "dana", core.RoleProjectMember)
	before, _ := e.store.ListApps(context.Background())

	for _, path := range listPages {
		resp := e.getAs(t, token, path)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s: status %d", path, resp.StatusCode)
		}
		if got := body(t, resp); strings.Contains(got, "zebra") {
			t.Errorf("GET %s: shows a project the person is not on", path)
		}
	}
	if got := body(t, e.getAs(t, token, "/apps")); strings.Contains(got, "+ New app") {
		t.Errorf("/apps offers a New app button with no project to put it in")
	}
	if got := body(t, e.getAs(t, token, "/templates")); strings.Contains(got, `action="/templates/`) {
		t.Errorf("/templates offers a deploy form with no project to deploy to")
	}

	resp := e.postAs(t, token, "/apps", url.Values{"name": {"web"}, "repo_url": {"https://example.com/x.git"}})
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("POST /apps with no project: status %d, want 404", resp.StatusCode)
	}
	after, _ := e.store.ListApps(context.Background())
	if len(after) != len(before) {
		t.Errorf("an app was created: %d -> %d", len(before), len(after))
	}
}

func TestCreateAppFormOffersOnlyTheirProjects(t *testing.T) {
	e := newTestEnv(t)
	a := seedProject(t, e, "zebra")
	b := seedProject(t, e, "quokka")
	token := e.signInAs(t, "dana", core.RoleProjectMember, a.Project.ID)

	got := body(t, e.getAs(t, token, "/apps"))
	if !strings.Contains(got, `<option value="`+itoa(a.Project.ID)+`"`) {
		t.Errorf("form is missing the shared project")
	}
	for _, id := range []int64{b.Project.ID, core.DefaultProjectID} {
		if strings.Contains(got, `<option value="`+itoa(id)+`"`) {
			t.Errorf("form offers project %d, which the person is not on", id)
		}
	}
}

func TestCreatingAProjectSharesItWithTheCreator(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	for _, tc := range []struct {
		role   core.Role
		member bool
	}{{core.RoleProjectAdmin, true}, {core.RoleSystemAdmin, false}} {
		token := e.signInAs(t, "u-"+string(tc.role), tc.role)
		name := "made-by-" + strings.ReplaceAll(string(tc.role), "_", "-")
		resp := e.postAs(t, token, "/projects", url.Values{"name": {name}})
		resp.Body.Close()
		if resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("%s: POST /projects status %d", tc.role, resp.StatusCode)
		}
		projects, err := e.store.ListProjects(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var made core.Project
		for _, p := range projects {
			if p.Name == name {
				made = p
			}
		}
		if made.ID == 0 {
			t.Fatalf("%s: project not created", tc.role)
		}
		u, err := e.store.GetUserByName(ctx, "u-"+string(tc.role))
		if err != nil {
			t.Fatal(err)
		}
		on, err := e.store.IsProjectMember(ctx, made.ID, u.ID)
		if err != nil {
			t.Fatal(err)
		}
		if on != tc.member {
			t.Errorf("%s: member = %v, want %v", tc.role, on, tc.member)
		}
	}
}

func TestDefaultProjectIsNotImplied(t *testing.T) {
	e := newTestEnv(t)
	other := seedProject(t, e, "zebra")
	token := e.signInAs(t, "dana", core.RoleProjectMember, other.Project.ID)

	resp := e.postAs(t, token, "/templates/uptime-kuma", url.Values{"name": {"kuma"}})
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("not on Default: status %d, want 404", resp.StatusCode)
	}

	u, err := e.store.GetUserByName(context.Background(), "dana")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.store.AddProjectMember(context.Background(), core.DefaultProjectID, u.ID); err != nil {
		t.Fatal(err)
	}
	resp = e.postAs(t, token, "/templates/uptime-kuma", url.Values{"name": {"kuma"}})
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("on Default: status %d, want 303", resp.StatusCode)
	}
}
