package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/outhaul-dev/outhaul/internal/core"
)

func TestAccountsMigrationKeepsExistingAdmin(t *testing.T) {
	db := openMigratedBefore(t, "0027")
	if _, err := db.Exec(`INSERT INTO users (id, username, password_hash, created_at) VALUES (3, 'admin', 'h', '2026-07-04T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO push_keys (label, fingerprint, public_key, created_at) VALUES ('k', 'SHA256:a', 'pk', '2026-07-04T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if err := migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	var role, signIn string
	if err := db.QueryRow(`SELECT role, sign_in FROM users WHERE id = 3`).Scan(&role, &signIn); err != nil {
		t.Fatal(err)
	}
	if role != "system_admin" || signIn != "password" {
		t.Fatalf("role/sign_in = %q/%q", role, signIn)
	}
	var owner int64
	if err := db.QueryRow(`SELECT user_id FROM push_keys`).Scan(&owner); err != nil || owner != 3 {
		t.Fatalf("push key owner = %d, %v; want 3", owner, err)
	}
}

func insertSSOAdmin(t *testing.T, s *Store, sub string) int64 {
	t.Helper()
	res, err := s.db.Exec(`INSERT INTO users (username, password_hash, created_at, role, sign_in, sso_issuer, sso_subject, email)
		VALUES (?, '', ?, 'system_admin', 'sso', 'https://idp', ?, ?)`,
		"sso:"+sub, fmtTime(time.Now().UTC()), sub, sub+"@example.com")
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}

func TestLastPasswordAdminCannotBeRemovedOrDemoted(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	admin, err := s.CreateUser(ctx, "admin", "hash")
	if err != nil {
		t.Fatal(err)
	}
	if admin.Role != core.RoleSystemAdmin {
		t.Fatalf("CreateUser role = %q", admin.Role)
	}
	check := func(want bool) {
		t.Helper()
		e1 := s.SetUserRole(ctx, admin.ID, core.RoleProjectViewer)
		e2 := s.DeleteUser(ctx, admin.ID)
		if want {
			if !errors.Is(e1, ErrLastPasswordAdmin) || !errors.Is(e2, ErrLastPasswordAdmin) {
				t.Fatalf("errors = %v, %v; want ErrLastPasswordAdmin", e1, e2)
			}
			got, err := s.GetUser(ctx, admin.ID)
			if err != nil || got.Role != core.RoleSystemAdmin {
				t.Fatalf("admin changed: %+v, %v", got, err)
			}
		} else if e1 != nil || e2 != nil {
			t.Fatalf("errors = %v, %v; want none", e1, e2)
		}
	}
	check(true)
	insertSSOAdmin(t, s, "x")
	check(true)
	second, err := s.CreatePasswordAccount(ctx, "second", core.RoleSystemAdmin)
	if err != nil {
		t.Fatal(err)
	}
	// "second" has not redeemed a setup link, so it cannot sign in.
	check(true)
	if err := s.UpdateUserPassword(ctx, second.ID, "hash-2", ""); err != nil {
		t.Fatal(err)
	}
	check(false)
}

func TestDeleteUserRemovesSessionsAndKeys(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, err := s.CreateUser(ctx, "admin", "h"); err != nil {
		t.Fatal(err)
	}
	u, err := s.CreatePasswordAccount(ctx, "bob", core.RoleProjectMember)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSession(ctx, core.Session{Token: "tok", UserID: u.ID, ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddPushKey(ctx, core.PushKey{UserID: u.ID, Label: "l", Fingerprint: "SHA256:b", PublicKey: "pk"}); err != nil {
		t.Fatal(err)
	}
	keys, err := s.ListPushKeysForUser(ctx, u.ID)
	if err != nil || len(keys) != 1 || keys[0].UserID != u.ID {
		t.Fatalf("keys = %+v, %v", keys, err)
	}
	if err := s.DeleteUser(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetSession(ctx, "tok"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("GetSession err = %v", err)
	}
	if keys, _ := s.ListPushKeysForUser(ctx, u.ID); len(keys) != 0 {
		t.Fatalf("keys left: %+v", keys)
	}
	if _, err := s.GetUser(ctx, u.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("GetUser err = %v", err)
	}
}

func TestDeletePushKeyForUserOnlyDeletesOwn(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	a, _ := s.CreatePasswordAccount(ctx, "a", core.RoleProjectMember)
	b, _ := s.CreatePasswordAccount(ctx, "b", core.RoleProjectMember)
	pk, err := s.AddPushKey(ctx, core.PushKey{UserID: a.ID, Label: "l", Fingerprint: "SHA256:c", PublicKey: "pk"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DeletePushKeyForUser(ctx, b.ID, pk.ID); err != nil {
		t.Fatal(err)
	}
	if keys, _ := s.ListPushKeysForUser(ctx, a.ID); len(keys) != 1 {
		t.Fatalf("another user's delete removed the key")
	}
	if err := s.DeletePushKeyForUser(ctx, a.ID, pk.ID); err != nil {
		t.Fatal(err)
	}
	if keys, _ := s.ListPushKeysForUser(ctx, a.ID); len(keys) != 0 {
		t.Fatalf("own delete failed")
	}
}

func TestPasswordAccountCannotSignInBeforeSetup(t *testing.T) {
	s := newTestStore(t)
	u, err := s.CreatePasswordAccount(context.Background(), "reception", core.RoleNone)
	if err != nil {
		t.Fatal(err)
	}
	if u.PasswordHash != "" || u.Role != core.RoleNone || u.SignIn != core.SignInPassword {
		t.Fatalf("user = %+v", u)
	}
}

func TestGetUserByNameFindsOnlyPasswordAccounts(t *testing.T) {
	s := newTestStore(t)
	insertSSOAdmin(t, s, "x")
	if _, err := s.GetUserByName(context.Background(), "sso:x"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("err = %v, want ErrNoRows", err)
	}
}

func TestListUsersOrderAndProjectCount(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	s.CreateUser(ctx, "zed", "h")
	s.CreatePasswordAccount(ctx, "amy", core.RoleProjectViewer)
	sso := insertSSOAdmin(t, s, "carl")
	if err := s.SetUserRole(ctx, sso, core.RoleNone); err != nil {
		t.Fatal(err)
	}
	rows, err := s.ListUsers(ctx)
	if err != nil || len(rows) != 3 {
		t.Fatalf("rows = %+v, %v", rows, err)
	}
	got := []string{rows[0].Display(), rows[1].Display(), rows[2].Display()}
	if got[0] != "carl@example.com" || got[1] != "amy" || got[2] != "zed" {
		t.Fatalf("order = %v", got)
	}
}

func TestEndUserSessionsAndTouchSignIn(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	u, _ := s.CreateUser(ctx, "admin", "h")
	s.CreateSession(ctx, core.Session{Token: "t", UserID: u.ID, ExpiresAt: time.Now().Add(time.Hour)})
	if err := s.EndUserSessions(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetSession(ctx, "t"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("session survived: %v", err)
	}
	if err := s.TouchUserSignIn(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetUser(ctx, u.ID)
	if got.LastSignInAt == nil {
		t.Fatal("LastSignInAt not set")
	}
}

func newProject(t *testing.T, s *Store, name string) core.Project {
	t.Helper()
	p, err := s.CreateProject(context.Background(), core.Project{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func userNames(us []core.User) []string {
	var out []string
	for _, u := range us {
		out = append(out, u.Username)
	}
	return out
}

func TestProjectMembersCascadeWithProject(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	u, err := s.CreatePasswordAccount(ctx, "m", core.RoleProjectMember)
	if err != nil {
		t.Fatal(err)
	}
	p := newProject(t, s, "p")
	for i := 0; i < 2; i++ { // idempotent
		if err := s.AddProjectMember(ctx, p.ID, u.ID); err != nil {
			t.Fatal(err)
		}
	}
	if ok, err := s.IsProjectMember(ctx, p.ID, u.ID); err != nil || !ok {
		t.Fatalf("IsProjectMember = %v, %v", ok, err)
	}
	if ids, err := s.ProjectIDsForUser(ctx, u.ID); err != nil || len(ids) != 1 || ids[0] != p.ID {
		t.Fatalf("ProjectIDsForUser = %v, %v", ids, err)
	}
	if err := s.DeleteProject(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.IsProjectMember(ctx, p.ID, u.ID); ok {
		t.Fatal("still a member after project delete")
	}
	if ids, err := s.ProjectIDsForUser(ctx, u.ID); err != nil || len(ids) != 0 {
		t.Fatalf("ProjectIDsForUser = %v, %v", ids, err)
	}
}

func TestRemoveProjectMember(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	u, _ := s.CreatePasswordAccount(ctx, "m", core.RoleProjectMember)
	p := newProject(t, s, "p")
	if err := s.AddProjectMember(ctx, p.ID, u.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveProjectMember(ctx, p.ID, u.ID); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.IsProjectMember(ctx, p.ID, u.ID); ok {
		t.Fatal("still a member")
	}
}

func TestCreateProjectWithMember(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	u, _ := s.CreatePasswordAccount(ctx, "m", core.RoleProjectAdmin)
	p, err := s.CreateProjectWithMember(ctx, core.Project{Name: "mine"}, u.ID)
	if err != nil || p.ID == 0 {
		t.Fatalf("CreateProjectWithMember = %+v, %v", p, err)
	}
	if ok, _ := s.IsProjectMember(ctx, p.ID, u.ID); !ok {
		t.Fatal("creator is not a member")
	}
}

func TestListShareableUsersOnlyHasProjectRoles(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, err := s.CreateUser(ctx, "admin", "h"); err != nil {
		t.Fatal(err)
	}
	m, _ := s.CreatePasswordAccount(ctx, "m", core.RoleProjectMember)
	if _, err := s.CreatePasswordAccount(ctx, "v", core.RoleProjectViewer); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreatePasswordAccount(ctx, "n", core.RoleNone); err != nil {
		t.Fatal(err)
	}
	p := newProject(t, s, "p")
	if err := s.AddProjectMember(ctx, p.ID, m.ID); err != nil {
		t.Fatal(err)
	}
	got, err := s.ListShareableUsers(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if names := userNames(got); len(names) != 1 || names[0] != "v" {
		t.Fatalf("shareable = %v, want [v]", names)
	}
}

func TestSystemAdminsArentListedOnProjects(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, err := s.CreateUser(ctx, "admin", "h"); err != nil {
		t.Fatal(err)
	}
	m, _ := s.CreatePasswordAccount(ctx, "m", core.RoleProjectMember)
	p := newProject(t, s, "p")
	if err := s.AddProjectMember(ctx, p.ID, m.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.ListProjectMembers(ctx, p.ID); len(got) != 1 {
		t.Fatalf("members = %v", userNames(got))
	}
	if err := s.SetUserRole(ctx, m.ID, core.RoleSystemAdmin); err != nil {
		t.Fatal(err)
	}
	if got, err := s.ListProjectMembers(ctx, p.ID); err != nil || len(got) != 0 {
		t.Fatalf("members = %v, %v; want none", userNames(got), err)
	}
}

func TestResetStopsTheOldPasswordAtOnce(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	admin, err := s.CreateUser(ctx, "admin", "h")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSession(ctx, core.Session{Token: "a", UserID: admin.ID, ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	// Only password system admin: refused, nothing changes.
	if err := s.ResetUserPassword(ctx, admin.ID, "tok", time.Now().Add(time.Hour)); !errors.Is(err, ErrLastPasswordAdmin) {
		t.Fatalf("err = %v, want ErrLastPasswordAdmin", err)
	}
	if got, _ := s.GetUser(ctx, admin.ID); got.PasswordHash != "h" {
		t.Fatalf("hash = %q", got.PasswordHash)
	}
	if _, err := s.GetSession(ctx, "a"); err != nil {
		t.Fatalf("session lost: %v", err)
	}

	u, _ := s.CreatePasswordAccount(ctx, "bob", core.RoleProjectMember)
	if err := s.UpdateUserPassword(ctx, u.ID, "old", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSession(ctx, core.Session{Token: "b", UserID: u.ID, ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSetupLink(ctx, u.ID, "earlier", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.ResetUserPassword(ctx, u.ID, "fresh", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetUser(ctx, u.ID); got.PasswordHash != "" {
		t.Fatalf("hash = %q, want blank", got.PasswordHash)
	}
	if _, err := s.GetSession(ctx, "b"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("session survived: %v", err)
	}
	if _, err := s.SetupLinkUser(ctx, "earlier"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("earlier link err = %v", err)
	}
	if got, err := s.SetupLinkUser(ctx, "fresh"); err != nil || got.ID != u.ID {
		t.Fatalf("fresh link = %+v, %v", got, err)
	}
}

func TestSetupLinkWorksOnce(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	u, _ := s.CreatePasswordAccount(ctx, "bob", core.RoleProjectMember)
	if err := s.CreateSetupLink(ctx, u.ID, "raw", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSession(ctx, core.Session{Token: "s", UserID: u.ID, ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if got, err := s.SetupLinkUser(ctx, "raw"); err != nil || got.ID != u.ID {
		t.Fatalf("SetupLinkUser = %+v, %v", got, err)
	}
	if got, err := s.CompleteSetupLink(ctx, "raw", "h2"); err != nil || got.ID != u.ID {
		t.Fatalf("CompleteSetupLink = %+v, %v", got, err)
	}
	if got, _ := s.GetUser(ctx, u.ID); got.PasswordHash != "h2" {
		t.Fatalf("hash = %q", got.PasswordHash)
	}
	if _, err := s.GetSession(ctx, "s"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("session survived: %v", err)
	}
	if _, err := s.CompleteSetupLink(ctx, "raw", "h3"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("second use err = %v", err)
	}
	if got, _ := s.GetUser(ctx, u.ID); got.PasswordHash != "h2" {
		t.Fatalf("hash = %q after reuse", got.PasswordHash)
	}
}

func TestSetupLinkExpiresAndIsReplaced(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	u, _ := s.CreatePasswordAccount(ctx, "bob", core.RoleProjectMember)
	if err := s.CreateSetupLink(ctx, u.ID, "old", time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetupLinkUser(ctx, "old"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("expired err = %v", err)
	}
	if _, err := s.CompleteSetupLink(ctx, "old", "x"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("expired complete err = %v", err)
	}
	if err := s.CreateSetupLink(ctx, u.ID, "a", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSetupLink(ctx, u.ID, "b", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetupLinkUser(ctx, "a"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("replaced link err = %v", err)
	}
	if _, err := s.SetupLinkUser(ctx, "b"); err != nil {
		t.Fatal(err)
	}
}

func TestSetupLinkTokenIsNotStoredInPlain(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	u, _ := s.CreatePasswordAccount(ctx, "bob", core.RoleProjectMember)
	if err := s.CreateSetupLink(ctx, u.ID, "raw", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	var h string
	if err := s.db.QueryRowContext(ctx, `SELECT token_hash FROM setup_links`).Scan(&h); err != nil {
		t.Fatal(err)
	}
	if h == "raw" || len(h) != 64 {
		t.Fatalf("token_hash = %q", h)
	}
}

func TestRecentDeploymentsInProjects(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	pa, pb := newProject(t, s, "a"), newProject(t, s, "b")
	var depA core.Deployment
	for _, p := range []core.Project{pa, pb} {
		app, err := s.CreateApp(ctx, core.App{Name: "app-" + p.Name, ProjectID: p.ID, RepoURL: "https://example.com/r.git", Domain: p.Name + ".example.com"})
		if err != nil {
			t.Fatal(err)
		}
		d, err := s.CreateDeployment(ctx, app.ID)
		if err != nil {
			t.Fatal(err)
		}
		if p.ID == pa.ID {
			depA = d
		}
	}
	got, err := s.ListRecentDeploymentsInProjects(ctx, []int64{pa.ID}, 10)
	if err != nil || len(got) != 1 || got[0].ID != depA.ID {
		t.Fatalf("list = %+v, %v", got, err)
	}
	if n, err := s.CountDeploymentsInProjects(ctx, []int64{pa.ID}); err != nil || n != 1 {
		t.Fatalf("count = %d, %v", n, err)
	}
	if n, err := s.CountDeploymentsInProjects(ctx, []int64{pa.ID, pb.ID}); err != nil || n != 2 {
		t.Fatalf("count both = %d, %v", n, err)
	}
	if got, err := s.ListRecentDeploymentsInProjects(ctx, nil, 10); err != nil || len(got) != 0 {
		t.Fatalf("empty list = %+v, %v", got, err)
	}
	if n, err := s.CountDeploymentsInProjects(ctx, []int64{}); err != nil || n != 0 {
		t.Fatalf("empty count = %d, %v", n, err)
	}
}

func TestStoreRefusesPasswordOperationsOnSSOAccount(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, err := s.CreateUser(ctx, "admin", "hash"); err != nil {
		t.Fatal(err)
	}
	sso := insertSSOAdmin(t, s, "dana")
	exp := time.Now().Add(time.Hour)

	if err := s.CreateSetupLink(ctx, sso, "tok", exp); !errors.Is(err, ErrNotPasswordAccount) {
		t.Fatalf("CreateSetupLink(sso) = %v, want ErrNotPasswordAccount", err)
	}
	if err := s.ResetUserPassword(ctx, sso, "tok", exp); !errors.Is(err, ErrNotPasswordAccount) {
		t.Fatalf("ResetUserPassword(sso) = %v, want ErrNotPasswordAccount", err)
	}
	if err := s.UpdateUserPassword(ctx, sso, "hash", ""); !errors.Is(err, ErrNotPasswordAccount) {
		t.Fatalf("UpdateUserPassword(sso) = %v, want ErrNotPasswordAccount", err)
	}
	for name, fn := range map[string]func() error{
		"CreateSetupLink":    func() error { return s.CreateSetupLink(ctx, 9999, "t", exp) },
		"ResetUserPassword":  func() error { return s.ResetUserPassword(ctx, 9999, "t", exp) },
		"UpdateUserPassword": func() error { return s.UpdateUserPassword(ctx, 9999, "h", "") },
	} {
		if err := fn(); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("%s(missing) = %v, want sql.ErrNoRows", name, err)
		}
	}

	// A link row that exists anyway (planted directly) must not be usable.
	if _, err := s.db.Exec(`INSERT INTO setup_links (token_hash, user_id, expires_at, created_at) VALUES (?, ?, ?, ?)`,
		hashToken("planted"), sso, fmtTime(exp), fmtTime(time.Now().UTC())); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetupLinkUser(ctx, "planted"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("SetupLinkUser(sso) = %v, want sql.ErrNoRows", err)
	}
	if _, err := s.CompleteSetupLink(ctx, "planted", "newhash"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("CompleteSetupLink(sso) = %v, want sql.ErrNoRows", err)
	}
	var hash string
	if err := s.db.QueryRow(`SELECT password_hash FROM users WHERE id = ?`, sso).Scan(&hash); err != nil || hash != "" {
		t.Fatalf("sso password_hash = %q, %v; want empty", hash, err)
	}
}

func TestSetUserRoleValidates(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, err := s.CreateUser(ctx, "admin", "hash"); err != nil {
		t.Fatal(err)
	}
	sso := insertSSOAdmin(t, s, "eve")
	if err := s.SetUserRole(ctx, sso, core.Role("wizard")); err == nil {
		t.Fatal("SetUserRole with an unknown role succeeded")
	}
	if err := s.SetUserRole(ctx, 9999, core.RoleNone); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("SetUserRole(missing) = %v, want sql.ErrNoRows", err)
	}
	u, _ := s.GetUser(ctx, sso)
	if u.Role != core.RoleSystemAdmin {
		t.Fatalf("role changed to %q", u.Role)
	}
}

const testIssuer = "https://idp.example.com"

func TestSSOFirstSignInCreatesAccount(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	u, _, err := s.FindOrCreateSSOUser(ctx, testIssuer, "sub-1", "dana@example.com", core.RoleNone, false)
	if err != nil {
		t.Fatal(err)
	}
	if u.ID == 0 || u.Username != ssoTestUsername(testIssuer, "sub-1") || u.Role != core.RoleNone || u.SignIn != core.SignInSSO ||
		u.SSOIssuer != testIssuer || u.SSOSubject != "sub-1" || u.Email != "dana@example.com" || u.PasswordHash != "" {
		t.Fatalf("user = %+v", u)
	}
	got, err := s.GetUser(ctx, u.ID)
	if err != nil || got.Username != u.Username || got.Email != u.Email {
		t.Fatalf("stored %+v, %v", got, err)
	}
}

func TestSSOSecondSignInKeepsRoleAndRefreshesEmail(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	first, _, err := s.FindOrCreateSSOUser(ctx, testIssuer, "sub-1", "old@example.com", core.RoleNone, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetUserRole(ctx, first.ID, core.RoleProjectMember); err != nil {
		t.Fatal(err)
	}
	again, _, err := s.FindOrCreateSSOUser(ctx, testIssuer, "sub-1", "new@example.com", core.RoleNone, false)
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != first.ID || again.Role != core.RoleProjectMember || again.Email != "new@example.com" {
		t.Fatalf("second sign-in = %+v, want id %d, role kept, email refreshed", again, first.ID)
	}
	// The account is the issuer and subject: either one differing is someone else.
	for _, c := range [][2]string{{testIssuer, "sub-2"}, {"https://other.example.com", "sub-1"}} {
		other, _, err := s.FindOrCreateSSOUser(ctx, c[0], c[1], "new@example.com", core.RoleNone, false)
		if err != nil {
			t.Fatal(err)
		}
		if other.ID == first.ID || other.Role != core.RoleNone {
			t.Errorf("%v reached the first account: %+v", c, other)
		}
	}
}

// The account is found by issuer and subject alone, whatever its username.
func TestSSOSignInFindsTheAccountByIssuerAndSubject(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	res, err := s.db.Exec(`INSERT INTO users (username, password_hash, created_at, role, sign_in, sso_issuer, sso_subject, email)
		VALUES ('legacy-name', '', ?, 'project_viewer', 'sso', ?, 'sub-1', 'old@example.com')`, fmtTime(time.Now().UTC()), testIssuer)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	u, _, err := s.FindOrCreateSSOUser(ctx, testIssuer, "sub-1", "new@example.com", core.RoleNone, false)
	if err != nil {
		t.Fatal(err)
	}
	if u.ID != id || u.Role != core.RoleProjectViewer || u.Email != "new@example.com" {
		t.Fatalf("got %+v, want account %d with its role and the new email", u, id)
	}
}

func TestSSOSignInNeverLinksByEmail(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	pw, err := s.CreatePasswordAccount(ctx, "dana@example.com", core.RoleSystemAdmin)
	if err != nil {
		t.Fatal(err)
	}
	u, _, err := s.FindOrCreateSSOUser(ctx, testIssuer, "sub-1", "dana@example.com", core.RoleNone, false)
	if err != nil {
		t.Fatal(err)
	}
	if u.ID == pw.ID || u.Role != core.RoleNone || u.SignIn != core.SignInSSO {
		t.Fatalf("SSO sign-in reached the password account: %+v", u)
	}
	if got, _ := s.GetUser(ctx, pw.ID); got.SignIn != core.SignInPassword || got.Role != core.RoleSystemAdmin || got.Email != "" {
		t.Fatalf("password account changed: %+v", got)
	}
}

// ssoTestUsername is the username an SSO account gets, worked out
// independently of the store.
func ssoTestUsername(issuer, subject string) string {
	sum := sha256.Sum256([]byte(issuer + "\x00" + subject))
	return "sso:" + hex.EncodeToString(sum[:])
}

// An issuer and subject are joined so that no two pairs share a username,
// whatever characters they hold.
func TestSSOUsernamesDontCollide(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	a, _, err := s.FindOrCreateSSOUser(ctx, "https://idp.example.com/a|b", "c", "one@example.com", core.RoleNone, false)
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := s.FindOrCreateSSOUser(ctx, "https://idp.example.com/a", "b|c", "two@example.com", core.RoleNone, false)
	if err != nil {
		t.Fatalf("second account: %v", err)
	}
	if a.ID == b.ID || a.Username == b.Username {
		t.Fatalf("two identities share an account or a username: %+v, %+v", a, b)
	}
	if a.Username != ssoTestUsername("https://idp.example.com/a|b", "c") {
		t.Errorf("username = %q", a.Username)
	}
}

// A password account that already has the SSO account's username is never
// reached or changed.
func TestSSOSignInNeverReachesAPasswordAccountByUsername(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	pw, err := s.CreateUser(ctx, ssoTestUsername(testIssuer, "sub-1"), "hash")
	if err != nil {
		t.Fatal(err)
	}
	if u, _, err := s.FindOrCreateSSOUser(ctx, testIssuer, "sub-1", "dana@example.com", core.RoleNone, false); err == nil {
		t.Fatalf("signed in as %+v", u)
	} else if strings.Contains(err.Error(), "password") {
		t.Errorf("error %q names a cause the store doesn't know", err)
	}
	if got, _ := s.GetUser(ctx, pw.ID); got.SignIn != core.SignInPassword || got.Email != "" || got.SSOSubject != "" {
		t.Fatalf("password account changed: %+v", got)
	}
}

func TestSSOSignInRefusesAnEmptyIdentity(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	for _, c := range [][3]string{{"", "sub", "a@example.com"}, {testIssuer, "", "a@example.com"}, {testIssuer, "sub", ""}} {
		if u, _, err := s.FindOrCreateSSOUser(ctx, c[0], c[1], c[2], core.RoleNone, false); err == nil {
			t.Errorf("%q made %+v", c, u)
		}
	}
	if has, _ := s.HasUser(ctx); has {
		t.Error("an account was created")
	}
}

func TestSSOFirstSignInsRacingMakeOneAccount(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	const n = 8
	ids := make([]int64, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			u, _, err := s.FindOrCreateSSOUser(ctx, testIssuer, "sub-1", "dana@example.com", core.RoleNone, false)
			ids[i], errs[i] = u.ID, err
		}()
	}
	wg.Wait()
	for i := range ids {
		if errs[i] != nil || ids[i] != ids[0] {
			t.Fatalf("sign-in %d = id %d, %v; want id %d", i, ids[i], errs[i], ids[0])
		}
	}
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM users WHERE sign_in = 'sso'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("SSO accounts = %d, %v; want 1", count, err)
	}
}

func TestSSOSignInAppliesAndRemovesAGroupRole(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	u, prev, err := s.FindOrCreateSSOUser(ctx, testIssuer, "sub-1", "dana@example.com", core.RoleProjectMember, true)
	if err != nil {
		t.Fatal(err)
	}
	if u.Role != core.RoleProjectMember || !u.RoleFromGroups || prev != core.RoleNone {
		t.Fatalf("first sign-in = %+v, previous %q", u, prev)
	}
	again, prev, err := s.FindOrCreateSSOUser(ctx, testIssuer, "sub-1", "dana@example.com", core.RoleProjectMember, true)
	if err != nil || again.Role != core.RoleProjectMember || prev != core.RoleProjectMember {
		t.Fatalf("second sign-in = %+v, previous %q, %v", again, prev, err)
	}
	// The provider stops sending the group: a group-given role goes.
	none, prev, err := s.FindOrCreateSSOUser(ctx, testIssuer, "sub-1", "dana@example.com", core.RoleNone, true)
	if err != nil || none.Role != core.RoleNone || none.RoleFromGroups || prev != core.RoleProjectMember {
		t.Fatalf("after leaving the group = %+v, previous %q, %v", none, prev, err)
	}
}

func TestSSOSignInKeepsAHandGivenRoleWhenNoGroupMaps(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	u, _, err := s.FindOrCreateSSOUser(ctx, testIssuer, "sub-1", "dana@example.com", core.RoleNone, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetUserRole(ctx, u.ID, core.RoleProjectAdmin); err != nil {
		t.Fatal(err)
	}
	again, prev, err := s.FindOrCreateSSOUser(ctx, testIssuer, "sub-1", "dana@example.com", core.RoleNone, true)
	if err != nil || again.Role != core.RoleProjectAdmin || prev != core.RoleProjectAdmin || again.RoleFromGroups {
		t.Fatalf("hand-given role = %+v, previous %q, %v", again, prev, err)
	}
}

func TestSetUserRoleClearsTheGroupMark(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	u, _, err := s.FindOrCreateSSOUser(ctx, testIssuer, "sub-1", "dana@example.com", core.RoleProjectMember, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetUserRole(ctx, u.ID, core.RoleProjectViewer); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetUser(ctx, u.ID)
	if err != nil || got.Role != core.RoleProjectViewer || got.RoleFromGroups {
		t.Fatalf("after a hand-set role = %+v, %v", got, err)
	}
}

// With the mapping off, a group-given role stays as it is.
func TestSSOSignInLeavesTheRoleAloneWithNoMapping(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, _, err := s.FindOrCreateSSOUser(ctx, testIssuer, "sub-1", "dana@example.com", core.RoleProjectMember, true); err != nil {
		t.Fatal(err)
	}
	u, _, err := s.FindOrCreateSSOUser(ctx, testIssuer, "sub-1", "dana@example.com", core.RoleNone, false)
	if err != nil || u.Role != core.RoleProjectMember || !u.RoleFromGroups {
		t.Fatalf("with no mapping = %+v, %v", u, err)
	}
}
