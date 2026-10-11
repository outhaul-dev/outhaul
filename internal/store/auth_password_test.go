package store

import (
	"context"
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
