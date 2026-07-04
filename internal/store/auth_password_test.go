package store

import (
	"context"
	"testing"
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
	if err := st.UpdateUserPassword(ctx, u.ID, "hash-2"); err != nil {
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
