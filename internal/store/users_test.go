package store

import (
	"context"
	"database/sql"
	"errors"
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
