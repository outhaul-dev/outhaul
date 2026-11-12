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

// ssoSessionUser inserts an SSO account with a live session.
func ssoSessionUser(t *testing.T, s *Store, subject string) (int64, string) {
	t.Helper()
	res, err := s.db.Exec(`INSERT INTO users (username, password_hash, created_at, role, sign_in, sso_issuer, sso_subject, email)
		VALUES (?, '', ?, 'none', 'sso', 'https://idp.example.com', ?, ?)`,
		ssoUsername("https://idp.example.com", subject), fmtTime(time.Now().UTC()), subject, subject+"@example.com")
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	tok := "tok-" + subject
	if err := s.CreateSession(context.Background(), core.Session{Token: tok, UserID: id, ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	return id, tok
}

func TestSyncSSOStartupKeepsSessionsWhenNothingChanged(t *testing.T) {
	ctx := context.Background()
	s := openWithBox(t)
	const iss = "https://idp.example.com"
	if err := s.SetSSOSettings(ctx, SSOSettings{Issuer: iss, ClientID: "cid", ClientSecret: "sec"}, false); err != nil {
		t.Fatal(err)
	}
	_, tok := ssoSessionUser(t, s, "sub-1")
	ended, err := s.SyncSSOStartup(ctx, SSOSettings{Issuer: iss, ClientID: "cid"}, true)
	if err != nil || ended {
		t.Fatalf("SyncSSOStartup = %v, %v; want no change", ended, err)
	}
	if _, err := s.GetSession(ctx, tok); err != nil {
		t.Error("the session ended although nothing changed")
	}
}

func TestSyncSSOStartupEndsSessionsWhenTheIssuerChanges(t *testing.T) {
	ctx := context.Background()
	s := openWithBox(t)
	if err := s.SetSSOSettings(ctx, SSOSettings{Issuer: "https://old.example.com", ClientID: "cid", ClientSecret: "sec"}, false); err != nil {
		t.Fatal(err)
	}
	_, tok := ssoSessionUser(t, s, "sub-1")
	ended, err := s.SyncSSOStartup(ctx, SSOSettings{Issuer: "https://new.example.com", ClientID: "cid"}, true)
	if err != nil || !ended {
		t.Fatalf("SyncSSOStartup = %v, %v; want it to end sessions", ended, err)
	}
	if _, err := s.GetSession(ctx, tok); err == nil {
		t.Error("the session survived an issuer change")
	}
	// The record now names the new issuer, so the next start changes nothing.
	if ended, err := s.SyncSSOStartup(ctx, SSOSettings{Issuer: "https://new.example.com", ClientID: "cid"}, true); err != nil || ended {
		t.Fatalf("second SyncSSOStartup = %v, %v; want no change", ended, err)
	}
}

func TestSyncSSOStartupEndsSessionsWhenOnlyTheClientIDChanges(t *testing.T) {
	ctx := context.Background()
	s := openWithBox(t)
	const iss = "https://idp.example.com"
	if err := s.SetSSOSettings(ctx, SSOSettings{Issuer: iss, ClientID: "old", ClientSecret: "sec"}, false); err != nil {
		t.Fatal(err)
	}
	_, tok := ssoSessionUser(t, s, "sub-1")
	if ended, err := s.SyncSSOStartup(ctx, SSOSettings{Issuer: iss, ClientID: "new"}, true); err != nil || !ended {
		t.Fatalf("SyncSSOStartup = %v, %v; want it to end sessions", ended, err)
	}
	if _, err := s.GetSession(ctx, tok); err == nil {
		t.Error("the session survived a client-ID change")
	}
}

// Before the record existed, the stored settings are the baseline.
func TestSyncSSOStartupUsesStoredSettingsAsTheFirstBaseline(t *testing.T) {
	ctx := context.Background()
	s := openWithBox(t)
	const iss = "https://idp.example.com"
	for k, v := range map[string]string{"sso_issuer": iss, "sso_client_id": "cid"} {
		if err := s.SetSetting(ctx, k, v); err != nil {
			t.Fatal(err)
		}
	}
	_, tok := ssoSessionUser(t, s, "sub-1")
	if ended, err := s.SyncSSOStartup(ctx, SSOSettings{Issuer: iss, ClientID: "cid"}, true); err != nil || ended {
		t.Fatalf("same settings = %v, %v; want kept", ended, err)
	}
	if _, err := s.GetSession(ctx, tok); err != nil {
		t.Error("a session ended although the settings matched")
	}
}

// A Settings-connected install passes set=false at boot: the stored settings
// are in force and nobody may be signed out.
func TestSyncSSOStartupWithSettingsFromThePageKeepsSessions(t *testing.T) {
	ctx := context.Background()
	s := openWithBox(t)
	const iss = "https://idp.example.com"
	if err := s.SetSSOSettings(ctx, SSOSettings{Issuer: iss, ClientID: "cid", ClientSecret: "sec"}, false); err != nil {
		t.Fatal(err)
	}
	_, tok := ssoSessionUser(t, s, "sub-1")
	if ended, err := s.SyncSSOStartup(ctx, SSOSettings{}, false); err != nil || ended {
		t.Fatalf("SyncSSOStartup = %v, %v; want the stored settings kept", ended, err)
	}
	if _, err := s.GetSession(ctx, tok); err != nil {
		t.Error("a session ended although the stored settings were unchanged")
	}
}

// An install from before this record, connected on Settings, upgrades with no
// record and no environment: the stored settings are both in force and the
// baseline.
func TestSyncSSOStartupUpgradesSettingsFromThePageWithoutSigningAnyoneOut(t *testing.T) {
	ctx := context.Background()
	s := openWithBox(t)
	const iss = "https://idp.example.com"
	for k, v := range map[string]string{"sso_issuer": iss, "sso_client_id": "cid"} {
		if err := s.SetSetting(ctx, k, v); err != nil {
			t.Fatal(err)
		}
	}
	_, tok := ssoSessionUser(t, s, "sub-1")
	if ended, err := s.SyncSSOStartup(ctx, SSOSettings{}, false); err != nil || ended {
		t.Fatalf("SyncSSOStartup = %v, %v; want the stored settings kept", ended, err)
	}
	if _, err := s.GetSession(ctx, tok); err != nil {
		t.Error("a session ended on the upgrade")
	}
	if got, ok, _ := s.GetSetting(ctx, "sso_last_issuer"); !ok || got != iss {
		t.Errorf("record = %q, %v; want the stored issuer", got, ok)
	}
}

func TestSyncSSOStartupWithNoSettingsEndsSessionsAndForgetsTheRecord(t *testing.T) {
	ctx := context.Background()
	s := openWithBox(t)
	const iss = "https://idp.example.com"
	// A previous run took its provider from the environment: a run record, and
	// no stored settings.
	if _, err := s.SyncSSOStartup(ctx, SSOSettings{Issuer: iss, ClientID: "cid"}, true); err != nil {
		t.Fatal(err)
	}
	_, tok := ssoSessionUser(t, s, "sub-1")
	ended, err := s.SyncSSOStartup(ctx, SSOSettings{}, false)
	if err != nil || !ended {
		t.Fatalf("SyncSSOStartup = %v, %v; want it to end sessions", ended, err)
	}
	if _, err := s.GetSession(ctx, tok); err == nil {
		t.Error("the session survived SSO being unset")
	}
	if _, ok, _ := s.GetSetting(ctx, "sso_last_issuer"); ok {
		t.Error("the run record was kept")
	}
	if ended, err := s.SyncSSOStartup(ctx, SSOSettings{}, false); err != nil || ended {
		t.Fatalf("second SyncSSOStartup = %v, %v; want no change", ended, err)
	}
}

// A host that never had SSO has nothing to report ending, though the record
// is still written for the next start.
func TestSyncSSOStartupFirstEverEnvironmentRunReportsNothing(t *testing.T) {
	ctx := context.Background()
	s := openWithBox(t)
	const iss = "https://idp.example.com"
	ended, err := s.SyncSSOStartup(ctx, SSOSettings{Issuer: iss, ClientID: "cid"}, true)
	if err != nil || ended {
		t.Fatalf("SyncSSOStartup = %v, %v; want false on a first-ever run", ended, err)
	}
	if got, ok, _ := s.GetSetting(ctx, "sso_last_issuer"); !ok || got != iss {
		t.Errorf("recorded issuer = %q, %v; want %q", got, ok, iss)
	}
	if got, ok, _ := s.GetSetting(ctx, "sso_last_client_id"); !ok || got != "cid" {
		t.Errorf("recorded client ID = %q, %v; want cid", got, ok)
	}
}

func TestSyncSSOStartupLeavesPasswordSessionsAlone(t *testing.T) {
	ctx := context.Background()
	s := openWithBox(t)
	if err := s.SetSSOSettings(ctx, SSOSettings{Issuer: "https://old.example.com", ClientID: "cid", ClientSecret: "sec"}, false); err != nil {
		t.Fatal(err)
	}
	u, err := s.CreatePasswordAccount(ctx, "pat", core.RoleSystemAdmin)
	if err != nil {
		t.Fatal(err)
	}
	tok := "password-session"
	if err := s.CreateSession(ctx, core.Session{Token: tok, UserID: u.ID, ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SyncSSOStartup(ctx, SSOSettings{Issuer: "https://new.example.com", ClientID: "cid"}, true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetSession(ctx, tok); err != nil {
		t.Error("a password session ended with the SSO settings")
	}
}

func TestSetAndClearSSOSettingsMaintainTheRunRecord(t *testing.T) {
	ctx := context.Background()
	s := openWithBox(t)
	const iss = "https://idp.example.com"
	if err := s.SetSSOSettings(ctx, SSOSettings{Issuer: iss, ClientID: "cid", ClientSecret: "sec"}, false); err != nil {
		t.Fatal(err)
	}
	if got, ok, _ := s.GetSetting(ctx, "sso_last_issuer"); !ok || got != iss {
		t.Fatalf("recorded issuer = %q, %v", got, ok)
	}
	if ended, err := s.SyncSSOStartup(ctx, SSOSettings{Issuer: iss, ClientID: "cid"}, true); err != nil || ended {
		t.Fatalf("a save was counted again at the next start: %v, %v", ended, err)
	}
	if err := s.ClearSSOSettings(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.GetSetting(ctx, "sso_last_issuer"); ok {
		t.Error("clearing SSO kept the run record")
	}
}
