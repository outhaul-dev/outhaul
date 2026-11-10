package server

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/outhaul-dev/outhaul/internal/core"
)

func memberCount(t *testing.T, e *testEnv, projectID int64) int {
	t.Helper()
	ms, err := e.store.ListProjectMembers(context.Background(), projectID)
	if err != nil {
		t.Fatal(err)
	}
	return len(ms)
}

func userID(t *testing.T, e *testEnv, name string) int64 {
	t.Helper()
	u, err := e.store.GetUserByName(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	return u.ID
}

func TestProjectAdminSharesAndUnshares(t *testing.T) {
	e := newTestEnv(t)
	p := seedProject(t, e, "alpha")
	admin := e.signInAs(t, "boss", core.RoleProjectAdmin, p.Project.ID)
	viewer := e.signInAs(t, "watcher", core.RoleProjectViewer)
	vid := userID(t, e, "watcher")
	path := "/projects/" + itoa(p.Project.ID)

	if got := e.getAs(t, viewer, path).StatusCode; got != http.StatusNotFound {
		t.Fatalf("before sharing = %d, want 404", got)
	}
	page := body(t, e.getAs(t, admin, path))
	if !strings.Contains(page, "System admins can see every project and aren't listed here.") {
		t.Error("People panel missing the system admin sentence")
	}
	if !strings.Contains(page, `<option value="`+itoa(vid)+`"`) {
		t.Error("the dropdown doesn't offer the viewer")
	}
	resp := e.postAs(t, admin, path+"/members", url.Values{"user_id": {itoa(vid)}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("share = %d, want 303", resp.StatusCode)
	}
	if got := e.getAs(t, viewer, path).StatusCode; got != http.StatusOK {
		t.Fatalf("after sharing = %d, want 200", got)
	}
	page = body(t, e.getAs(t, admin, path))
	if !strings.Contains(page, "watcher") || !strings.Contains(page, "Project viewer") {
		t.Error("People panel doesn't list the viewer with a role label")
	}
	resp = e.postAs(t, admin, path+"/members/"+itoa(vid)+"/delete", nil)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("unshare = %d, want 303", resp.StatusCode)
	}
	if got := e.getAs(t, viewer, path).StatusCode; got != http.StatusNotFound {
		t.Fatalf("after unsharing = %d, want 404", got)
	}
}

func TestCannotShareWithNoRoleAccount(t *testing.T) {
	e := newTestEnv(t)
	p := seedProject(t, e, "alpha")
	admin := e.signInAs(t, "boss", core.RoleProjectAdmin, p.Project.ID)
	e.signInAs(t, "nobody", core.RoleNone)
	nid := userID(t, e, "nobody")
	path := "/projects/" + itoa(p.Project.ID)

	if page := body(t, e.getAs(t, admin, path)); strings.Contains(page, `<option value="`+itoa(nid)+`"`) {
		t.Error("the dropdown offers a no-role account")
	}
	if got := e.postAs(t, admin, path+"/members", url.Values{"user_id": {itoa(nid)}}).StatusCode; got != http.StatusBadRequest {
		t.Fatalf("share with no-role = %d, want 400", got)
	}
	if on, _ := e.store.IsProjectMember(context.Background(), p.Project.ID, nid); on {
		t.Error("a no-role account was added")
	}
}

func TestMemberCannotShare(t *testing.T) {
	e := newTestEnv(t)
	p := seedProject(t, e, "alpha")
	member := e.signInAs(t, "worker", core.RoleProjectMember, p.Project.ID)
	e.signInAs(t, "watcher", core.RoleProjectViewer)
	path := "/projects/" + itoa(p.Project.ID)
	if got := e.postAs(t, member, path+"/members", url.Values{"user_id": {itoa(userID(t, e, "watcher"))}}).StatusCode; got != http.StatusForbidden {
		t.Fatalf("member share = %d, want 403", got)
	}
	if page := body(t, e.getAs(t, member, path)); strings.Contains(page, path+"/members") {
		t.Error("a member is shown sharing controls")
	}
}

func TestSharingRefusesPeopleWhoCantBeListed(t *testing.T) {
	e := newTestEnv(t)
	p := seedProject(t, e, "alpha")
	admin := e.signInAs(t, "boss", core.RoleProjectAdmin, p.Project.ID)
	e.signInAs(t, "root", core.RoleSystemAdmin)
	e.signInAs(t, "watcher", core.RoleProjectViewer)
	path := "/projects/" + itoa(p.Project.ID) + "/members"

	for name, form := range map[string]url.Values{
		"system admin": {"user_id": {itoa(userID(t, e, "root"))}},
		"unknown":      {"user_id": {"99999"}},
		"missing":      {},
		"garbage":      {"user_id": {"abc"}},
	} {
		if got := e.postAs(t, admin, path, form).StatusCode; got != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", name, got)
		}
	}
	if n := memberCount(t, e, p.Project.ID); n != 1 {
		t.Fatalf("members = %d, want 1", n)
	}
	form := url.Values{"user_id": {itoa(userID(t, e, "watcher"))}}
	for i := 0; i < 2; i++ {
		if got := e.postAs(t, admin, path, form).StatusCode; got != http.StatusSeeOther {
			t.Fatalf("share #%d = %d, want 303", i+1, got)
		}
	}
	if n := memberCount(t, e, p.Project.ID); n != 2 {
		t.Fatalf("members = %d, want 2", n)
	}
}

func TestProjectAdminCanLeaveAProject(t *testing.T) {
	e := newTestEnv(t)
	p := seedProject(t, e, "alpha")
	admin := e.signInAs(t, "boss", core.RoleProjectAdmin, p.Project.ID)
	resp := e.postAs(t, admin, "/projects/"+itoa(p.Project.ID)+"/members/"+itoa(userID(t, e, "boss"))+"/delete", nil)
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/projects" {
		t.Fatalf("leave = %d to %q, want 303 to /projects", resp.StatusCode, resp.Header.Get("Location"))
	}
	if on, _ := e.store.IsProjectMember(context.Background(), p.Project.ID, userID(t, e, "boss")); on {
		t.Error("still a member")
	}
}

func TestProjectAdminOnAnotherProjectCannotShareThisOne(t *testing.T) {
	e := newTestEnv(t)
	a := seedProject(t, e, "alpha")
	b := seedProject(t, e, "beta")
	other := e.signInAs(t, "boss", core.RoleProjectAdmin, b.Project.ID)
	e.signInAs(t, "watcher", core.RoleProjectViewer)
	got := e.postAs(t, other, "/projects/"+itoa(a.Project.ID)+"/members", url.Values{"user_id": {itoa(userID(t, e, "watcher"))}}).StatusCode
	if got != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", got)
	}
}
