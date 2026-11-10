package store

import (
	"context"
	"testing"
	"time"

	"github.com/outhaul-dev/outhaul/internal/core"
)

func TestSSOSecretIsSealedAndCountsAsSealed(t *testing.T) {
	ctx := context.Background()
	s := openWithBox(t)
	if _, ok, err := s.GetSSOSettings(ctx); err != nil || ok {
		t.Fatalf("empty: ok=%v err=%v", ok, err)
	}
	want := SSOSettings{Issuer: "https://idp.example.com", ClientID: "cid", ClientSecret: "s3cr3t-value"}
	if err := s.SetSSOSettings(ctx, want, false); err != nil {
		t.Fatal(err)
	}
	raw, ok, err := s.GetSetting(ctx, "sso_client_secret")
	if err != nil || !ok {
		t.Fatalf("raw secret row: ok=%v err=%v", ok, err)
	}
	if raw == want.ClientSecret || raw == "" {
		t.Fatalf("secret stored unsealed: %q", raw)
	}
	got, ok, err := s.GetSSOSettings(ctx)
	if err != nil || !ok || got != want {
		t.Fatalf("got %+v ok=%v err=%v; want %+v", got, ok, err, want)
	}
	if has, err := s.HasSealedValues(ctx); err != nil || !has {
		t.Fatalf("HasSealedValues = %v, %v; want true", has, err)
	}
	if err := s.ClearSSOSettings(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.GetSSOSettings(ctx); ok {
		t.Fatal("settings still there after clear")
	}
	if has, _ := s.HasSealedValues(ctx); has {
		t.Fatal("HasSealedValues true after clear")
	}
}

func TestEndSSOSessionsLeavesPasswordSessions(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	sso := insertSSOAdmin(t, s, "sub1")
	pw, err := s.CreatePasswordAccount(ctx, "pat", core.RoleSystemAdmin)
	if err != nil {
		t.Fatal(err)
	}
	for tok, uid := range map[string]int64{"sso-tok": sso, "pw-tok": pw.ID} {
		if err := s.CreateSession(ctx, core.Session{Token: tok, UserID: uid, ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.EndSSOSessions(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetSession(ctx, "sso-tok"); err == nil {
		t.Error("SSO session survived")
	}
	if _, err := s.GetSession(ctx, "pw-tok"); err != nil {
		t.Errorf("password session ended: %v", err)
	}
}

// failSessionDeletes makes every DELETE on sessions fail, standing in for a
// store that breaks halfway through a change.
func failSessionDeletes(t *testing.T, s *Store) {
	t.Helper()
	if _, err := s.db.Exec(`CREATE TRIGGER fail_session_delete BEFORE DELETE ON sessions
		BEGIN SELECT RAISE(ABORT, 'sessions are read-only'); END`); err != nil {
		t.Fatal(err)
	}
}

func TestSavingSSOSettingsEndsSessionsInTheSameTransaction(t *testing.T) {
	ctx := context.Background()
	s := openWithBox(t)
	old := SSOSettings{Issuer: "https://idp.example.com", ClientID: "cid", ClientSecret: "sec"}
	if err := s.SetSSOSettings(ctx, old, false); err != nil {
		t.Fatal(err)
	}
	sso := insertSSOAdmin(t, s, "sub1")
	if err := s.CreateSession(ctx, core.Session{Token: "sso-tok", UserID: sso, ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}

	// Without ending sessions, the session stays.
	if err := s.SetSSOSettings(ctx, SSOSettings{Issuer: old.Issuer, ClientID: old.ClientID, ClientSecret: "sec2"}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetSession(ctx, "sso-tok"); err != nil {
		t.Fatalf("session ended by a save that keeps sessions: %v", err)
	}

	// If the sessions can't be ended, the new settings aren't saved either, so
	// a retry still sees the change and ends them.
	failSessionDeletes(t, s)
	next := SSOSettings{Issuer: "https://other.example.com", ClientID: "cid", ClientSecret: "sec2"}
	if err := s.SetSSOSettings(ctx, next, true); err == nil {
		t.Fatal("save succeeded although its sessions couldn't be ended")
	}
	if got, _, _ := s.GetSSOSettings(ctx); got.Issuer != old.Issuer {
		t.Errorf("issuer = %q after a failed save, want %q", got.Issuer, old.Issuer)
	}
	if err := s.ClearSSOSettings(ctx); err == nil {
		t.Fatal("clear succeeded although its sessions couldn't be ended")
	}
	if _, ok, _ := s.GetSSOSettings(ctx); !ok {
		t.Error("settings cleared although the sessions weren't ended")
	}
	if _, err := s.db.Exec(`DROP TRIGGER fail_session_delete`); err != nil {
		t.Fatal(err)
	}

	if err := s.SetSSOSettings(ctx, next, true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetSession(ctx, "sso-tok"); err == nil {
		t.Error("session survived a save that ends sessions")
	}
	if err := s.CreateSession(ctx, core.Session{Token: "sso-tok2", UserID: sso, ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := s.ClearSSOSettings(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetSession(ctx, "sso-tok2"); err == nil {
		t.Error("session survived clearing the settings")
	}
}
