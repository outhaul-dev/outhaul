package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/outhaul-dev/outhaul/internal/core"
)

func TestUpdateUserPassword(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	u, err := st.CreateUser(ctx, "admin", "hash-1")
	if err != nil {
		t.Fatal(err)
	}
	got, err := st.GetUser(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Username != "admin" || got.PasswordHash != "hash-1" {
		t.Fatalf("GetUser = %+v, want admin/hash-1", got)
	}
	if err := st.UpdateUserPassword(ctx, u.ID, "hash-2", ""); err != nil {
		t.Fatal(err)
	}
	got, err = st.GetUser(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.PasswordHash != "hash-2" {
		t.Errorf("PasswordHash = %q, want hash-2", got.PasswordHash)
	}
}

func TestUpdateUserPasswordRevokesOtherSessions(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	u, err := st.CreateUser(ctx, "admin", "hash-1")
	if err != nil {
		t.Fatal(err)
	}
	other, err := st.CreateUser(ctx, "second", "hash-x")
	if err != nil {
		t.Fatal(err)
	}
	exp := time.Now().Add(time.Hour)
	for _, s := range []core.Session{
		{Token: "mine", UserID: u.ID, ExpiresAt: exp},
		{Token: "leaked", UserID: u.ID, ExpiresAt: exp},
		{Token: "theirs", UserID: other.ID, ExpiresAt: exp},
	} {
		if err := st.CreateSession(ctx, s); err != nil {
			t.Fatal(err)
		}
	}

	if err := st.UpdateUserPassword(ctx, u.ID, "hash-2", "mine"); err != nil {
		t.Fatal(err)
	}
	for token, want := range map[string]bool{"mine": true, "leaked": false, "theirs": true} {
		_, err := st.GetSession(ctx, token)
		if got := err == nil; got != want {
			t.Errorf("session %q valid = %v, want %v", token, got, want)
		}
	}
}

// A password sign-in checks the password, slowly, and then makes a session. A
// reset or change that commits in between must not leave a session for the
// old password, so the session is made only if the account still has the hash
// that was checked.
func TestPasswordSessionNeedsThePasswordThatWasChecked(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	if _, err := st.CreateUser(ctx, "admin", "h1"); err != nil {
		t.Fatal(err)
	}
	bob, err := st.CreatePasswordAccount(ctx, "bob", core.RoleProjectMember)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateUserPassword(ctx, bob.ID, "b1", ""); err != nil {
		t.Fatal(err)
	}
	session := func(token string) core.Session {
		return core.Session{Token: token, UserID: bob.ID, ExpiresAt: time.Now().Add(time.Hour)}
	}
	refused := func(what, token, checked string) {
		t.Helper()
		if err := st.CreatePasswordSession(ctx, session(token), checked); !errors.Is(err, ErrPasswordChanged) {
			t.Errorf("%s: err = %v, want ErrPasswordChanged", what, err)
		}
		if _, err := st.GetSession(ctx, token); !errors.Is(err, sql.ErrNoRows) {
			t.Errorf("%s: a session was stored (err %v)", what, err)
		}
	}

	if err := st.CreatePasswordSession(ctx, session("ok"), "b1"); err != nil {
		t.Fatalf("with the hash that was checked: %v", err)
	}
	if _, err := st.GetSession(ctx, "ok"); err != nil {
		t.Fatalf("session not stored: %v", err)
	}

	if err := st.UpdateUserPassword(ctx, bob.ID, "b2", ""); err != nil {
		t.Fatal(err)
	}
	refused("after a password change", "changed", "b1")

	if err := st.ResetUserPassword(ctx, bob.ID, "tok", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	refused("after a reset", "reset", "b2")
	refused("with a blank hash", "blank", "")

	if err := st.DeleteUser(ctx, bob.ID); err != nil {
		t.Fatal(err)
	}
	refused("after the account is deleted", "gone", "b2")

	sso := insertSSOAdmin(t, st, "dana")
	if err := st.CreatePasswordSession(ctx, core.Session{Token: "sso", UserID: sso, ExpiresAt: time.Now().Add(time.Hour)}, ""); !errors.Is(err, ErrPasswordChanged) {
		t.Errorf("for an SSO account: err = %v, want ErrPasswordChanged", err)
	}
}
