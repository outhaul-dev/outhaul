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
	if err := s.SetSSOSettings(ctx, want); err != nil {
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
