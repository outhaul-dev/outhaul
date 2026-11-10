package server

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/outhaul-dev/outhaul/internal/core"
	"github.com/outhaul-dev/outhaul/internal/store"
)

var setupLinkRE = regexp.MustCompile(`/account/setup\?token=([0-9a-f]+)`)

func createAccount(t *testing.T, e *testEnv, admin, name, role string) (id int64, token string) {
	t.Helper()
	resp := e.postAs(t, admin, "/users", url.Values{"username": {name}, "role": {role}})
	html := body(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /users %s = %d: %s", name, resp.StatusCode, html)
	}
	m := setupLinkRE.FindStringSubmatch(html)
	if m == nil {
		t.Fatalf("no setup link in response: %s", html)
	}
	u, err := e.store.GetUserByName(context.Background(), name)
	if err != nil {
		t.Fatalf("GetUserByName: %v", err)
	}
	return u.ID, m[1]
}

func TestCreatePasswordAccountShowsLinkOnce(t *testing.T) {
	e := newTestEnv(t)
	admin := e.signInAs(t, "root", core.RoleSystemAdmin)
	_, token := createAccount(t, e, admin, "reception", "none")
	if body(t, e.getAs(t, admin, "/users")) == "" {
		t.Fatal("empty page")
	}
	if strings.Contains(body(t, e.getAs(t, admin, "/users")), token) {
		t.Error("GET /users shows the token again")
	}
}

func TestSetupLinkSetsPasswordAndSignsIn(t *testing.T) {
	e := newTestEnv(t)
	admin := e.signInAs(t, "root", core.RoleSystemAdmin)
	_, token := createAccount(t, e, admin, "reception", "project_viewer")

	resp := e.getAs(t, "", "/account/setup?token="+token)
	html := body(t, resp)
	if resp.StatusCode != http.StatusOK || !strings.Contains(html, "reception") {
		t.Fatalf("GET setup = %d, want 200 naming reception: %s", resp.StatusCode, html)
	}
	if resp := e.postAs(t, "", "/account/setup", url.Values{"token": {token}, "password": {"short"}}); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("short password = %d, want 400", resp.StatusCode)
	}
	resp = e.postAs(t, "", "/account/setup", url.Values{"token": {token}, "password": {"correct horse"}})
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/" {
		t.Fatalf("POST setup = %d to %q, want 303 to /", resp.StatusCode, resp.Header.Get("Location"))
	}
	var session string
	for _, c := range resp.Cookies() {
		if c.Name == sessionCookie {
			session = c.Value
		}
	}
	if session == "" {
		t.Fatal("no session cookie")
	}
	if got := e.getAs(t, session, "/projects").StatusCode; got != http.StatusOK {
		t.Errorf("new session GET /projects = %d, want 200", got)
	}
	resp = e.postAs(t, "", "/login", url.Values{"username": {"reception"}, "password": {"correct horse"}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("login = %d, want 303", resp.StatusCode)
	}
	u, _ := e.store.GetUserByName(context.Background(), "reception")
	if u.LastSignInAt == nil {
		t.Error("sign-in not recorded")
	}

	for _, resp := range []*http.Response{
		e.getAs(t, "", "/account/setup?token="+token),
		e.postAs(t, "", "/account/setup", url.Values{"token": {token}, "password": {"another one"}}),
	} {
		html := body(t, resp)
		if !strings.Contains(html, "This link has expired or has already been used.") {
			t.Errorf("reused link: %d %s", resp.StatusCode, html)
		}
		if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusSeeOther {
			t.Errorf("reused link status = %d", resp.StatusCode)
		}
	}
	if resp := e.getAs(t, "", "/account/setup?token=nonsense"); resp.StatusCode == http.StatusOK {
		t.Error("unknown token accepted")
	}
}

func TestAccountWithoutPasswordCannotSignIn(t *testing.T) {
	e := newTestEnv(t)
	admin := e.signInAs(t, "root", core.RoleSystemAdmin)
	createAccount(t, e, admin, "reception", "none")
	resp := e.postAs(t, "", "/login", url.Values{"username": {"reception"}, "password": {"anything"}})
	html := body(t, resp)
	if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(html, "Invalid username or password.") {
		t.Fatalf("login = %d %s, want 401 with the usual message", resp.StatusCode, html)
	}
	resp = e.postAs(t, "", "/login", url.Values{"username": {"reception"}, "password": {""}})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("empty password = %d, want 401", resp.StatusCode)
	}
}

func TestResetPasswordEndsSessionsAndReplacesLink(t *testing.T) {
	e := newTestEnv(t)
	admin := e.signInAs(t, "root", core.RoleSystemAdmin)
	id, used := createAccount(t, e, admin, "reception", "project_viewer")
	if resp := e.postAs(t, "", "/account/setup", url.Values{"token": {used}, "password": {"old password"}}); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("setup = %d", resp.StatusCode)
	}
	// A link issued and not yet used, which the reset must replace.
	first := NewToken()
	if err := e.store.CreateSetupLink(context.Background(), id, first, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got := e.getAs(t, "", "/account/setup?token="+first).StatusCode; got != http.StatusOK {
		t.Fatalf("unused link before reset = %d, want 200", got)
	}
	session := ""
	resp := e.postAs(t, "", "/login", url.Values{"username": {"reception"}, "password": {"old password"}})
	for _, c := range resp.Cookies() {
		if c.Name == sessionCookie {
			session = c.Value
		}
	}
	if got := e.getAs(t, session, "/projects").StatusCode; got != http.StatusOK {
		t.Fatalf("before reset = %d", got)
	}

	resp = e.postAs(t, admin, "/users/"+itoa(id)+"/reset", nil)
	html := body(t, resp)
	m := setupLinkRE.FindStringSubmatch(html)
	if resp.StatusCode != http.StatusOK || m == nil {
		t.Fatalf("reset = %d, link %v: %s", resp.StatusCode, m, html)
	}
	second := m[1]
	if got := e.getAs(t, session, "/projects"); got.StatusCode != http.StatusSeeOther || got.Header.Get("Location") != "/login" {
		t.Errorf("old session = %d to %q, want 303 to /login", got.StatusCode, got.Header.Get("Location"))
	}
	if got := e.postAs(t, "", "/login", url.Values{"username": {"reception"}, "password": {"old password"}}).StatusCode; got != http.StatusUnauthorized {
		t.Errorf("old password login = %d, want 401", got)
	}
	if got := e.getAs(t, "", "/account/setup?token="+first).StatusCode; got == http.StatusOK {
		t.Error("earlier link still works")
	}
	if got := e.getAs(t, "", "/account/setup?token="+second).StatusCode; got != http.StatusOK {
		t.Errorf("new link = %d, want 200", got)
	}
}

func TestResetPasswordRefusesYourselfAndTheLastAdmin(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	a := e.signInAs(t, "alice", core.RoleSystemAdmin)
	b := e.signInAs(t, "bob", core.RoleSystemAdmin)
	for n, sess := range map[string]string{"alice": a, "bob": b} {
		u, _ := e.store.GetUserByName(ctx, n)
		if err := e.store.UpdateUserPassword(ctx, u.ID, "$argon2id$x", sess); err != nil {
			t.Fatal(err)
		}
	}
	alice, _ := e.store.GetUserByName(ctx, "alice")
	resp := e.postAs(t, a, "/users/"+itoa(alice.ID)+"/reset", nil)
	if html := body(t, resp); resp.StatusCode != http.StatusBadRequest || !strings.Contains(html, "reset your own password here") {
		t.Errorf("own reset = %d %s, want 400 with the refusal message", resp.StatusCode, html)
	}
	after, _ := e.store.GetUserByName(ctx, "alice")
	if after.PasswordHash != "$argon2id$x" {
		t.Error("own password changed")
	}
	if got := e.getAs(t, a, "/users").StatusCode; got != http.StatusOK {
		t.Errorf("own session = %d, want 200", got)
	}

	// An SSO system admin cannot reset the only password system admin.
	e2 := newTestEnv(t)
	sso := ssoAccount(t, e2, "sso-admin@example.com", core.RoleSystemAdmin)
	only := e2.signInAs(t, "only", core.RoleSystemAdmin)
	u, _ := e2.store.GetUserByName(ctx, "only")
	if err := e2.store.UpdateUserPassword(ctx, u.ID, "$argon2id$x", only); err != nil {
		t.Fatal(err)
	}
	resp = e2.postAs(t, sso, "/users/"+itoa(u.ID)+"/reset", nil)
	if html := body(t, resp); resp.StatusCode != http.StatusBadRequest || !strings.Contains(html, lastAdminCopy) {
		t.Errorf("last admin reset = %d %s", resp.StatusCode, html)
	}
}

const lastAdminCopy = "last system admin who can sign in with a password"

// ssoAccount inserts an SSO account directly and gives it a session.
func ssoAccount(t *testing.T, e *testEnv, email string, role core.Role) (session string) {
	t.Helper()
	id := insertSSOUser(t, e, email, role)
	session = NewToken()
	if err := e.store.CreateSession(context.Background(), core.Session{Token: session, UserID: id, ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	return session
}

func TestUsersPageRefusesLastPasswordAdmin(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	admin := e.signInAs(t, "root", core.RoleSystemAdmin)
	u, _ := e.store.GetUserByName(ctx, "root")
	if err := e.store.UpdateUserPassword(ctx, u.ID, "$argon2id$x", admin); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		path string
		form url.Values
	}{
		{"/users/" + itoa(u.ID) + "/role", url.Values{"role": {"project_viewer"}}},
		{"/users/" + itoa(u.ID) + "/delete", nil},
	} {
		resp := e.postAs(t, admin, c.path, c.form)
		if html := body(t, resp); resp.StatusCode != http.StatusBadRequest || !strings.Contains(html, lastAdminCopy) {
			t.Errorf("POST %s = %d %s", c.path, resp.StatusCode, html)
		}
		got, err := e.store.GetUser(ctx, u.ID)
		if err != nil || got.Role != core.RoleSystemAdmin {
			t.Errorf("after POST %s: %v, role %q", c.path, err, got.Role)
		}
	}
}

func TestUsersPageChangesRoleAndRemoves(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	admin := e.signInAs(t, "root", core.RoleSystemAdmin)
	id, _ := createAccount(t, e, admin, "reception", "none")

	resp := e.postAs(t, admin, "/users/"+itoa(id)+"/role", url.Values{"role": {"project_member"}})
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/users" {
		t.Fatalf("role change = %d to %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if u, _ := e.store.GetUser(ctx, id); u.Role != core.RoleProjectMember {
		t.Errorf("role = %q", u.Role)
	}
	if got := e.postAs(t, admin, "/users/"+itoa(id)+"/role", url.Values{"role": {"emperor"}}).StatusCode; got != http.StatusBadRequest {
		t.Errorf("unknown role = %d, want 400", got)
	}
	if got := e.postAs(t, admin, "/users/9999/role", url.Values{"role": {"none"}}).StatusCode; got != http.StatusNotFound {
		t.Errorf("unknown user = %d, want 404", got)
	}
	resp = e.postAs(t, admin, "/users/"+itoa(id)+"/delete", nil)
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/users" {
		t.Fatalf("delete = %d to %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if _, err := e.store.GetUser(ctx, id); err != sql.ErrNoRows {
		t.Errorf("GetUser after delete = %v, want ErrNoRows", err)
	}
}

func TestUsernameRules(t *testing.T) {
	e := newTestEnv(t)
	admin := e.signInAs(t, "root", core.RoleSystemAdmin)
	for _, name := range []string{"ab", "sso:x", "  ", "root"} {
		resp := e.postAs(t, admin, "/users", url.Values{"username": {name}, "role": {"none"}})
		html := body(t, resp)
		if resp.StatusCode != http.StatusBadRequest || !strings.Contains(html, "alert-error") || setupLinkRE.MatchString(html) {
			t.Errorf("username %q = %d, want 400 with an error and no link", name, resp.StatusCode)
		}
	}
	resp := e.postAs(t, admin, "/users", url.Values{"username": {"valid"}, "role": {"bogus"}})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("unknown role = %d, want 400", resp.StatusCode)
	}
}

func TestWaitingBanner(t *testing.T) {
	e := newTestEnv(t)
	admin := e.signInAs(t, "aaa", core.RoleSystemAdmin)
	insertSSOUser(t, e, "zed@example.com", core.RoleNone)
	html := body(t, e.getAs(t, admin, "/users"))
	if !strings.Contains(html, "1 person signed in and is waiting for a role.") {
		t.Errorf("no banner: %s", html)
	}
	if strings.Index(html, "zed@example.com") > strings.Index(html, "aaa") {
		t.Error("waiting person not listed first")
	}
	if !strings.Contains(html, `"No role" means the person can pass protected routes but can't use Outhaul.`) {
		t.Error("no note about No role")
	}
}

// insertSSOUser adds an SSO account the way a first sign-in would.
func insertSSOUser(t *testing.T, e *testEnv, email string, role core.Role) int64 {
	t.Helper()
	db := e.rawDB(t)
	res, err := db.Exec(`INSERT INTO users (username, password_hash, created_at, role, sign_in, sso_issuer, sso_subject, email)
		VALUES (?, '', ?, ?, 'sso', 'https://idp', ?, ?)`,
		"sso:"+email, time.Now().UTC().Format(time.RFC3339Nano), string(role), email, email)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}

func TestCopyButtonHandlerIsOnEveryPageWithOne(t *testing.T) {
	e := newTestEnv(t)
	admin := e.signInAs(t, "root", core.RoleSystemAdmin)
	resp := e.postAs(t, admin, "/users", url.Values{"username": {"reception"}, "role": {"none"}})
	html := body(t, resp)
	if !strings.Contains(html, `class="copybtn"`) {
		t.Fatal("no copy button on the one-time link")
	}
	if n := strings.Count(html, "closest('.copybtn')"); n != 1 {
		t.Errorf("the one-time link page has %d copy handlers, want exactly 1", n)
	}
	sp := seedProject(t, e, "acme")
	if n := strings.Count(body(t, e.getAs(t, admin, "/apps/"+itoa(sp.App.ID))), "closest('.copybtn')"); n != 1 {
		t.Errorf("the app page has %d copy handlers, want exactly 1", n)
	}
}

func selectedRoles(html string) []string {
	dialog := html[strings.Index(html, `id="user-dialog"`):]
	var out []string
	for _, m := range regexp.MustCompile(`<option value="([a-z_]+)" selected>`).FindAllStringSubmatch(dialog, -1) {
		out = append(out, m[1])
	}
	return out
}

func TestNewAccountRoleDefaultsToNoRole(t *testing.T) {
	e := newTestEnv(t)
	admin := e.signInAs(t, "root", core.RoleSystemAdmin)
	if got := selectedRoles(body(t, e.getAs(t, admin, "/users"))); len(got) != 1 || got[0] != "none" {
		t.Errorf("plain GET selects %v, want only none", got)
	}
	resp := e.postAs(t, admin, "/users", url.Values{"username": {"x"}, "role": {"project_viewer"}})
	html := body(t, resp)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if got := selectedRoles(html); len(got) != 1 || got[0] != "project_viewer" {
		t.Errorf("error re-render selects %v, want only project_viewer", got)
	}
}

func TestResetPasswordRefusesSSOAccountAndMakesNoLink(t *testing.T) {
	e := newTestEnv(t)
	admin := e.signInAs(t, "root", core.RoleSystemAdmin)
	id := insertSSOUser(t, e, "dana@example.com", core.RoleProjectMember)
	resp := e.postAs(t, admin, "/users/"+itoa(id)+"/reset", nil)
	if html := body(t, resp); resp.StatusCode != http.StatusBadRequest || setupLinkRE.MatchString(html) {
		t.Fatalf("reset of an SSO account = %d %s, want 400 and no link", resp.StatusCode, html)
	}
	var links int
	if err := e.rawDB(t).QueryRow(`SELECT COUNT(*) FROM setup_links WHERE user_id = ?`, id).Scan(&links); err != nil || links != 0 {
		t.Fatalf("setup links for the SSO account = %d, %v; want 0", links, err)
	}
}

func TestRemovedPersonsSessionStopsWorking(t *testing.T) {
	e := newTestEnv(t)
	admin := e.signInAs(t, "root", core.RoleSystemAdmin)
	other := e.signInAs(t, "gone", core.RoleProjectViewer)
	if got := e.getAs(t, other, "/projects").StatusCode; got != http.StatusOK {
		t.Fatalf("before removal GET /projects = %d, want 200", got)
	}
	u, _ := e.store.GetUserByName(context.Background(), "gone")
	if resp := e.postAs(t, admin, "/users/"+itoa(u.ID)+"/delete", nil); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("remove = %d, want 303", resp.StatusCode)
	}
	resp := e.getAs(t, other, "/projects")
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login" {
		t.Fatalf("after removal GET /projects = %d to %q, want 303 to /login", resp.StatusCode, resp.Header.Get("Location"))
	}
}

func TestLastAdminRefusedWhileSecondAdminHasNoPasswordYet(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	admin := e.signInAs(t, "root", core.RoleSystemAdmin)
	root, _ := e.store.GetUserByName(ctx, "root")
	if err := e.store.UpdateUserPassword(ctx, root.ID, "$argon2id$x", admin); err != nil {
		t.Fatal(err)
	}
	createAccount(t, e, admin, "newbie", "system_admin")

	page := body(t, e.getAs(t, admin, "/users"))
	if !strings.Contains(page, "Password not set yet") {
		t.Errorf("Users page doesn't say the new account has no password yet: %s", page)
	}
	if strings.Count(page, "Password not set yet") != 1 {
		t.Errorf("want the note on one row only, got %d", strings.Count(page, "Password not set yet"))
	}
	resp := e.postAs(t, admin, "/users/"+itoa(root.ID)+"/role", url.Values{"role": {"project_viewer"}})
	html := body(t, resp)
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(html, lastAdminCopy) ||
		!strings.Contains(html, "wait until they have set their password") {
		t.Fatalf("demote = %d %s, want 400 with the last-admin message", resp.StatusCode, html)
	}
}

// The account can change between targetUser and the store call: removed, or
// (for a reset) found to sign in with SSO. Neither is a server failure.
func TestRefuseAnswersARaceWithoutA500(t *testing.T) {
	e := newTestEnv(t)
	admin := core.User{ID: 1, Username: "root", Role: core.RoleSystemAdmin, SignIn: core.SignInPassword}
	call := func(err error) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		e.srv.refuse(rec, withUser(httptest.NewRequest(http.MethodPost, "/users/7/reset", nil), admin), err)
		return rec
	}
	for _, err := range []error{sql.ErrNoRows, fmt.Errorf("reset: %w", sql.ErrNoRows)} {
		if rec := call(err); rec.Code != http.StatusNotFound || strings.Contains(rec.Body.String(), "no rows") {
			t.Errorf("%v: %d %q, want 404 without the store's error", err, rec.Code, rec.Body.String())
		}
	}
	for _, err := range []error{store.ErrNotPasswordAccount, fmt.Errorf("reset: %w", store.ErrNotPasswordAccount)} {
		rec := call(err)
		page := rec.Body.String()
		if rec.Code != http.StatusBadRequest || !strings.Contains(page, "signs in with SSO") || strings.Contains(page, "store:") {
			t.Errorf("%v: %d, want 400 with the SSO message and not the store's error: %s", err, rec.Code, page)
		}
	}
}
