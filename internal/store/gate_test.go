package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/outhaul-dev/outhaul/internal/core"
)

const (
	wikiHost = "wiki.example.com"
	docsHost = "docs.example.com"

	// testNonce stands in for the hash of the pending cookie in the browser
	// that asked for a code.
	testNonce = "nonce-hash"
)

// gateSessionFor makes a live gateway session and one route session on wikiHost.
func gateSessionFor(t *testing.T, s *Store, subject, kind, display, gateTok, routeTok string) {
	t.Helper()
	ctx := context.Background()
	if err := s.CreateGateSession(ctx, gateTok, core.GateSession{
		Subject: subject, Kind: kind, Display: display, ExpiresAt: time.Now().Add(8 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateGateCode(ctx, "code-"+routeTok, gateTok, wikiHost, testNonce, time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RedeemGateCode(ctx, "code-"+routeTok, wikiHost, testNonce, routeTok, time.Now().Add(8*time.Hour)); err != nil {
		t.Fatal(err)
	}
}

func wantRouteOK(t *testing.T, s *Store, tok string) {
	t.Helper()
	if _, err := s.RouteSessionUser(context.Background(), tok, wikiHost); err != nil {
		t.Fatalf("route session %s should work: %v", tok, err)
	}
}

func wantRouteGone(t *testing.T, s *Store, tok string) {
	t.Helper()
	if _, err := s.RouteSessionUser(context.Background(), tok, wikiHost); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("route session %s: err = %v; want sql.ErrNoRows", tok, err)
	}
}

func TestCodeWorksOnceOnItsOwnHost(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	if err := s.CreateGateSession(ctx, "g1", core.GateSession{Subject: "user:1", Kind: core.SignInPassword, Display: "dana", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateGateCode(ctx, "c1", "g1", wikiHost, testNonce, time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RedeemGateCode(ctx, "c1", docsHost, testNonce, "r-wrong", time.Now().Add(time.Hour)); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("wrong host: err = %v", err)
	}
	if _, err := s.RouteSessionUser(ctx, "r-wrong", docsHost); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("wrong-host redeem made a route session: %v", err)
	}
	display, err := s.RedeemGateCode(ctx, "c1", "WIKI.example.com", testNonce, "r1", time.Now().Add(time.Hour))
	if err != nil || display != "dana" {
		t.Fatalf("own host: %q, %v", display, err)
	}
	if _, err := s.RedeemGateCode(ctx, "c1", wikiHost, testNonce, "r2", time.Now().Add(time.Hour)); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("second redeem: err = %v", err)
	}
	wantRouteGone(t, s, "r2")
}

// A code works only alongside the nonce of the browser that asked for it. A
// missing or different nonce is refused like an unknown code, and leaves the
// code for the browser it belongs to.
func TestCodeWorksOnlyWithItsNonce(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	if err := s.CreateGateSession(ctx, "g1", core.GateSession{Subject: "user:1", Kind: core.SignInPassword, Display: "dana", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateGateCode(ctx, "c1", "g1", wikiHost, "nonce-a", time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	var stored string
	if err := s.db.QueryRowContext(ctx, `SELECT nonce_hash FROM gate_codes`).Scan(&stored); err != nil || stored != "nonce-a" {
		t.Fatalf("stored nonce = %q, %v; want nonce-a", stored, err)
	}
	for i, n := range []string{"", "nonce-b", "nonce-a ", "NONCE-A", "nonce-"} {
		tok := "r-wrong-" + string(rune('a'+i))
		if _, err := s.RedeemGateCode(ctx, "c1", wikiHost, n, tok, time.Now().Add(time.Hour)); !errors.Is(err, sql.ErrNoRows) {
			t.Errorf("nonce %q: err = %v; want sql.ErrNoRows", n, err)
		}
		wantRouteGone(t, s, tok)
	}
	display, err := s.RedeemGateCode(ctx, "c1", wikiHost, "nonce-a", "r1", time.Now().Add(time.Hour))
	if err != nil || display != "dana" {
		t.Fatalf("its own nonce, after the refusals: %q, %v", display, err)
	}
	if _, err := s.RedeemGateCode(ctx, "c1", wikiHost, "nonce-a", "r2", time.Now().Add(time.Hour)); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("second redeem: err = %v", err)
	}
}

func TestCodeExpires(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	if err := s.CreateGateSession(ctx, "g1", core.GateSession{Subject: "user:1", Kind: core.SignInPassword, Display: "dana", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateGateCode(ctx, "c1", "g1", wikiHost, testNonce, time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RedeemGateCode(ctx, "c1", wikiHost, testNonce, "r1", time.Now().Add(time.Hour)); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("err = %v", err)
	}
	wantRouteGone(t, s, "r1")
}

func TestCodeOfExpiredGateSessionIsRefused(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	gateSessionFor(t, s, "user:1", core.SignInPassword, "dana", "g1", "r1")
	if err := s.CreateGateCode(ctx, "c2", "g1", wikiHost, testNonce, time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE gate_sessions SET expires_at = ?`, fmtTime(time.Now().Add(-time.Second))); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RedeemGateCode(ctx, "c2", wikiHost, testNonce, "r2", time.Now().Add(time.Hour)); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("err = %v", err)
	}
}

func TestRouteCookieWorksOnlyOnItsHost(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	gateSessionFor(t, s, "user:1", core.SignInPassword, "dana", "g1", "r1")
	if d, err := s.RouteSessionUser(ctx, "r1", wikiHost); err != nil || d != "dana" {
		t.Fatalf("own host: %q, %v", d, err)
	}
	if d, err := s.RouteSessionUser(ctx, "r1", "WIKI.example.com"); err != nil || d != "dana" {
		t.Fatalf("upper-case host: %q, %v", d, err)
	}
	if _, err := s.RouteSessionUser(ctx, "r1", docsHost); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("other host: err = %v", err)
	}
	if _, err := s.RouteSessionUser(ctx, "nope", wikiHost); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("unknown token: err = %v", err)
	}
}

func TestRouteSessionExpires(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	gateSessionFor(t, s, "user:1", core.SignInPassword, "dana", "g1", "r1")
	if _, err := s.db.ExecContext(ctx, `UPDATE route_sessions SET expires_at = ?`, fmtTime(time.Now().Add(-time.Second))); err != nil {
		t.Fatal(err)
	}
	wantRouteGone(t, s, "r1")
}

func TestEndingGatewaySessionEndsRouteSessions(t *testing.T) {
	ctx := context.Background()
	for name, end := range map[string]func(*Store) error{
		"DeleteGateSession":            func(s *Store) error { return s.DeleteGateSession(ctx, "g1") },
		"DeleteGateSessionsForSubject": func(s *Store) error { return s.DeleteGateSessionsForSubject(ctx, "user:1") },
		"DeleteGateSessionsOfKind":     func(s *Store) error { return s.DeleteGateSessionsOfKind(ctx, core.SignInPassword) },
	} {
		t.Run(name, func(t *testing.T) {
			s := newTestStore(t)
			gateSessionFor(t, s, "user:1", core.SignInPassword, "dana", "g1", "r1")
			gateSessionFor(t, s, "user:2", core.SignInSSO, "eli", "g2", "r2")
			if err := s.CreateGateCode(ctx, "open-code", "g1", wikiHost, testNonce, time.Now().Add(time.Minute)); err != nil {
				t.Fatal(err)
			}
			wantRouteOK(t, s, "r1")
			if err := end(s); err != nil {
				t.Fatal(err)
			}
			wantRouteGone(t, s, "r1")
			if _, err := s.RedeemGateCode(ctx, "open-code", wikiHost, testNonce, "r9", time.Now().Add(time.Hour)); !errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("code outlived its gateway session: %v", err)
			}
			if name != "DeleteGateSessionsOfKind" {
				// Another person's sessions survive. (OfKind ends both here: the
				// two sessions differ in kind, so r2 survives only in the
				// password case, checked below.)
				wantRouteOK(t, s, "r2")
				if _, err := s.GetGateSession(ctx, "g2"); err != nil {
					t.Fatalf("other gateway session: %v", err)
				}
			} else {
				wantRouteOK(t, s, "r2") // the other kind is untouched
			}
		})
	}
}

func TestEmptySubjectOrKindDeletesNothing(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	// CreateGateSession refuses these, so plant the rows with SQL.
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO gate_sessions (token_hash, subject, kind, display, created_at, expires_at) VALUES (?, '', '', 'x', ?, ?)`,
		hashToken("blank"), fmtTime(time.Now()), fmtTime(time.Now().Add(time.Hour))); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteGateSessionsForSubject(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteGateSessionsOfKind(ctx, ""); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM gate_sessions`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("rows = %d, %v; want the planted one to survive", n, err)
	}
}

func TestGateWritesRefuseEmptyValues(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	ok := core.GateSession{Subject: "user:1", Kind: core.SignInPassword, Display: "dana", ExpiresAt: time.Now().Add(time.Hour)}
	for name, tc := range map[string]struct {
		tok  string
		sess core.GateSession
	}{
		"empty token":   {"", ok},
		"empty subject": {"t", core.GateSession{Kind: ok.Kind, Display: ok.Display, ExpiresAt: ok.ExpiresAt}},
		"empty display": {"t", core.GateSession{Subject: ok.Subject, Kind: ok.Kind, ExpiresAt: ok.ExpiresAt}},
		"other kind":    {"t", core.GateSession{Subject: ok.Subject, Kind: "token", Display: ok.Display, ExpiresAt: ok.ExpiresAt}},
		"empty kind":    {"t", core.GateSession{Subject: ok.Subject, Display: ok.Display, ExpiresAt: ok.ExpiresAt}},
	} {
		err := s.CreateGateSession(ctx, tc.tok, tc.sess)
		if err == nil || errors.Is(err, sql.ErrNoRows) {
			t.Errorf("%s: err = %v; want a refusal", name, err)
		}
	}
	if err := s.CreateGateSession(ctx, "g1", ok); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateGateCode(ctx, "", "g1", wikiHost, testNonce, time.Now().Add(time.Minute)); err == nil || errors.Is(err, sql.ErrNoRows) {
		t.Errorf("empty code: err = %v", err)
	}
	if err := s.CreateGateCode(ctx, "c0", "g1", wikiHost, "", time.Now().Add(time.Minute)); err == nil || errors.Is(err, sql.ErrNoRows) {
		t.Errorf("empty nonce: err = %v", err)
	}
	if err := s.CreateGateCode(ctx, "c1", "g1", wikiHost, testNonce, time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RedeemGateCode(ctx, "", wikiHost, testNonce, "r1", time.Now().Add(time.Hour)); err == nil || errors.Is(err, sql.ErrNoRows) {
		t.Errorf("empty redeem code: err = %v", err)
	}
	if _, err := s.RedeemGateCode(ctx, "c1", wikiHost, testNonce, "", time.Now().Add(time.Hour)); err == nil || errors.Is(err, sql.ErrNoRows) {
		t.Errorf("empty route token: err = %v", err)
	}
	// A browser with no pending cookie is refused like any wrong browser.
	if _, err := s.RedeemGateCode(ctx, "c1", wikiHost, "", "r1", time.Now().Add(time.Hour)); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("empty nonce at redeem: err = %v; want sql.ErrNoRows", err)
	}
	// The refused redeem left the code usable.
	if _, err := s.RedeemGateCode(ctx, "c1", wikiHost, testNonce, "r1", time.Now().Add(time.Hour)); err != nil {
		t.Errorf("code was spent by a refused redeem: %v", err)
	}
}

func TestNewCodeReplacesEarlierUnredeemedCodesForSameHostAndNonce(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	for _, g := range []string{"g1", "g2"} {
		if err := s.CreateGateSession(ctx, g, core.GateSession{Subject: "user:1", Kind: core.SignInPassword, Display: "dana", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	exp := time.Now().Add(time.Minute)
	for _, c := range []struct{ code, gate, host string }{
		{"old", "g1", wikiHost}, {"other-host", "g1", docsHost}, {"other-gate", "g2", wikiHost}, {"new", "g1", wikiHost},
	} {
		if err := s.CreateGateCode(ctx, c.code, c.gate, c.host, testNonce, exp); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.RedeemGateCode(ctx, "old", wikiHost, testNonce, "r-old", time.Now().Add(time.Hour)); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("old code survived: %v", err)
	}
	for i, c := range []struct{ code, host string }{{"other-host", docsHost}, {"other-gate", wikiHost}, {"new", wikiHost}} {
		if _, err := s.RedeemGateCode(ctx, c.code, c.host, testNonce, "r"+string(rune('a'+i)), time.Now().Add(time.Hour)); err != nil {
			t.Errorf("%s should still work: %v", c.code, err)
		}
	}
}

// Two tabs opening one app at once each get a code for their own browser
// nonce (their pending cookies differ): neither voids the other's. A new code
// for the same nonce replaces the earlier one, and expired codes go.
func TestCodesForOtherNoncesSurvive(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	if err := s.CreateGateSession(ctx, "g1", core.GateSession{Subject: "user:1", Kind: core.SignInPassword, Display: "dana", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	exp := time.Now().Add(time.Minute)
	for _, c := range []struct{ code, nonce string }{{"tab1-old", "n1"}, {"tab1", "n1"}, {"tab2", "n2"}, {"tab3", "n3"}} {
		if err := s.CreateGateCode(ctx, c.code, "g1", wikiHost, c.nonce, exp); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.CreateGateCode(ctx, "stale", "g1", docsHost, "n9", time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateGateCode(ctx, "tab4", "g1", wikiHost, "n4", exp); err != nil {
		t.Fatal(err)
	}
	var stale int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM gate_codes WHERE code_hash = ?`, hashToken("stale")).Scan(&stale); err != nil {
		t.Fatal(err)
	}
	if stale != 0 {
		t.Error("an expired code was kept")
	}
	if _, err := s.RedeemGateCode(ctx, "tab1-old", wikiHost, "n1", "r0", time.Now().Add(time.Hour)); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("the code a newer one for the same nonce replaced still works: %v", err)
	}
	for i, c := range []struct{ code, nonce string }{{"tab1", "n1"}, {"tab2", "n2"}, {"tab3", "n3"}, {"tab4", "n4"}} {
		if _, err := s.RedeemGateCode(ctx, c.code, wikiHost, c.nonce, "r"+string(rune('a'+i)), time.Now().Add(time.Hour)); err != nil {
			t.Errorf("%s (nonce %s) should work: %v", c.code, c.nonce, err)
		}
	}
}

// A gateway session past its expiry is no session, even before the cleanup
// has deleted its row.
func TestGateSessionExpires(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	if err := s.CreateGateSession(ctx, "g1", core.GateSession{Subject: "user:1", Kind: core.SignInPassword, Display: "dana", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetGateSession(ctx, "g1"); err != nil {
		t.Fatalf("a live session: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE gate_sessions SET expires_at = ? WHERE token_hash = ?`,
		fmtTime(time.Now().Add(-time.Second)), hashToken("g1")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetGateSession(ctx, "g1"); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("an expired session: err = %v, want sql.ErrNoRows", err)
	}
}

// julianday reads the stored text to the millisecond, so the cleanup's
// boundary is pinned at one millisecond, not one nanosecond.
func TestCleanupBoundaryAgreesWithGo(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	now := time.Now().UTC().Truncate(time.Millisecond)
	for _, r := range []struct {
		tok string
		exp time.Time
	}{{"past", now.Add(-time.Millisecond)}, {"future", now.Add(time.Millisecond)}} {
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO gate_sessions (token_hash, subject, kind, display, created_at, expires_at) VALUES (?, 'user:1', 'password', 'd', ?, ?)`,
			r.tok, fmtTime(now), fmtTime(r.exp)); err != nil {
			t.Fatal(err)
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := deleteExpiredGate(ctx, tx, now); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var got []string
	rows, err := s.db.QueryContext(ctx, `SELECT token_hash FROM gate_sessions`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var h string
		rows.Scan(&h)
		got = append(got, h)
	}
	if len(got) != 1 || got[0] != "future" {
		t.Fatalf("left %v; want only the row expiring a millisecond ahead", got)
	}
}

func TestRouteSessionCantOutliveItsGatewaySession(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	gateSessionFor(t, s, "user:1", core.SignInPassword, "dana", "g1", "r1")
	if _, err := s.db.ExecContext(ctx, `UPDATE gate_sessions SET expires_at = ?`, fmtTime(time.Now().Add(-time.Second))); err != nil {
		t.Fatal(err)
	}
	wantRouteGone(t, s, "r1")
}

func TestRouteSessionExpiryIsCappedByGatewaySession(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	gateExp := time.Now().Add(time.Hour).UTC()
	if err := s.CreateGateSession(ctx, "g1", core.GateSession{Subject: "user:1", Kind: core.SignInPassword, Display: "dana", ExpiresAt: gateExp}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateGateCode(ctx, "c1", "g1", wikiHost, testNonce, time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RedeemGateCode(ctx, "c1", wikiHost, testNonce, "r1", time.Now().Add(8*time.Hour)); err != nil {
		t.Fatal(err)
	}
	var exp string
	if err := s.db.QueryRowContext(ctx, `SELECT expires_at FROM route_sessions`).Scan(&exp); err != nil {
		t.Fatal(err)
	}
	got, _ := parseTime(exp)
	if !got.Equal(gateExp) {
		t.Fatalf("route expiry %v; want the gateway's %v", got, gateExp)
	}
}

func TestCreateGateSessionCleansUpExpired(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	gateSessionFor(t, s, "user:1", core.SignInPassword, "dana", "old", "r-old")
	if err := s.CreateGateCode(ctx, "stale", "old", wikiHost, testNonce, time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	gateSessionFor(t, s, "user:2", core.SignInPassword, "eli", "live", "r-live")
	if err := s.CreateGateCode(ctx, "expired-code", "live", wikiHost, testNonce, time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	past := fmtTime(time.Now().Add(-time.Second))
	if _, err := s.db.ExecContext(ctx, `UPDATE gate_sessions SET expires_at = ? WHERE subject = 'user:1'`, past); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE route_sessions SET expires_at = ? WHERE token_hash = ?`, past, hashToken("r-live")); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateGateSession(ctx, "new", core.GateSession{Subject: "user:3", Kind: core.SignInPassword, Display: "fay", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	for table, want := range map[string]int{"gate_sessions": 2, "route_sessions": 0, "gate_codes": 0} {
		var n int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != want {
			t.Errorf("%s has %d rows; want %d", table, n, want)
		}
	}
}

func TestRemovingAccountEndsItsGateSessions(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	admin, err := s.CreateUser(ctx, "root", "hash")
	if err != nil {
		t.Fatal(err)
	}
	pw, err := s.CreatePasswordAccount(ctx, "dana", core.RoleProjectMember)
	if err != nil {
		t.Fatal(err)
	}
	sso, err := s.FindOrCreateSSOUser(ctx, "https://idp", "sub-1", "eli@example.com")
	if err != nil {
		t.Fatal(err)
	}
	gateSessionFor(t, s, admin.GateSubject(), core.SignInPassword, "root", "g0", "r0")
	gateSessionFor(t, s, pw.GateSubject(), core.SignInPassword, "dana", "g1", "r1")
	ssoSubject := core.SSOGateSubject("https://idp", "sub-1")
	if sso.GateSubject() != ssoSubject {
		t.Fatalf("GateSubject() = %q; want %q", sso.GateSubject(), ssoSubject)
	}
	gateSessionFor(t, s, ssoSubject, core.SignInSSO, "eli@example.com", "g2", "r2")
	if err := s.DeleteUser(ctx, pw.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteUser(ctx, sso.ID); err != nil {
		t.Fatal(err)
	}
	wantRouteGone(t, s, "r1")
	wantRouteGone(t, s, "r2")
	wantRouteOK(t, s, "r0")
}

func TestChangingPasswordEndsGateSessions(t *testing.T) {
	ctx := context.Background()
	for name, change := range map[string]func(*Store, core.User) error{
		"UpdateUserPassword": func(s *Store, u core.User) error { return s.UpdateUserPassword(ctx, u.ID, "new", "keep") },
		"EndUserSessions":    func(s *Store, u core.User) error { return s.EndUserSessions(ctx, u.ID) },
		"ResetUserPassword": func(s *Store, u core.User) error {
			return s.ResetUserPassword(ctx, u.ID, "setup-tok", time.Now().Add(time.Hour))
		},
		"CompleteSetupLink": func(s *Store, u core.User) error {
			if err := s.CreateSetupLink(ctx, u.ID, "setup-tok", time.Now().Add(time.Hour)); err != nil {
				return err
			}
			_, err := s.CompleteSetupLink(ctx, "setup-tok", "new")
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			s := newTestStore(t)
			if _, err := s.CreateUser(ctx, "root", "hash"); err != nil { // so the last-admin guard passes
				t.Fatal(err)
			}
			u, err := s.CreatePasswordAccount(ctx, "dana", core.RoleProjectMember)
			if err != nil {
				t.Fatal(err)
			}
			other, err := s.CreatePasswordAccount(ctx, "eli", core.RoleProjectMember)
			if err != nil {
				t.Fatal(err)
			}
			gateSessionFor(t, s, u.GateSubject(), core.SignInPassword, "dana", "g1", "r1")
			gateSessionFor(t, s, other.GateSubject(), core.SignInPassword, "eli", "g2", "r2")
			if err := change(s, u); err != nil {
				t.Fatal(err)
			}
			wantRouteGone(t, s, "r1")
			wantRouteOK(t, s, "r2")
		})
	}
}

func TestChangingSSOSettingsEndsGateSessions(t *testing.T) {
	ctx := context.Background()
	v := SSOSettings{Issuer: "https://idp.example.com", ClientID: "cid", ClientSecret: "sec"}
	setup := func(t *testing.T) *Store {
		s := openWithBox(t)
		gateSessionFor(t, s, core.SSOGateSubject("https://idp", "sub"), core.SignInSSO, "eli", "g1", "r1")
		gateSessionFor(t, s, "user:1", core.SignInPassword, "dana", "g2", "r2")
		return s
	}
	t.Run("set and end", func(t *testing.T) {
		s := setup(t)
		if err := s.SetSSOSettings(ctx, v, true); err != nil {
			t.Fatal(err)
		}
		wantRouteGone(t, s, "r1")
		wantRouteOK(t, s, "r2")
	})
	t.Run("clear", func(t *testing.T) {
		s := setup(t)
		if err := s.ClearSSOSettings(ctx); err != nil {
			t.Fatal(err)
		}
		wantRouteGone(t, s, "r1")
		wantRouteOK(t, s, "r2")
	})
	t.Run("set and keep", func(t *testing.T) {
		s := setup(t)
		if err := s.SetSSOSettings(ctx, v, false); err != nil {
			t.Fatal(err)
		}
		wantRouteOK(t, s, "r1")
		wantRouteOK(t, s, "r2")
	})
}

func TestGateTokensAreHashed(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	gateSessionFor(t, s, "user:1", core.SignInPassword, "dana", "raw-gate-token", "raw-route-token")
	if err := s.CreateGateCode(ctx, "raw-live-code", "raw-gate-token", wikiHost, testNonce, time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`SELECT token_hash || '|' || subject || '|' || kind || '|' || display FROM gate_sessions`,
		`SELECT token_hash || '|' || gate_token_hash || '|' || host || '|' || display FROM route_sessions`,
		`SELECT code_hash || '|' || gate_token_hash || '|' || host FROM gate_codes`,
	} {
		rows, err := s.db.QueryContext(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for rows.Next() {
			var row string
			if err := rows.Scan(&row); err != nil {
				t.Fatal(err)
			}
			n++
			for _, raw := range []string{"raw-gate-token", "raw-route-token", "raw-live-code", "code-raw-route-token"} {
				if strings.Contains(row, raw) {
					t.Errorf("%q holds raw %q", q, raw)
				}
			}
		}
		rows.Close()
		if n == 0 {
			t.Errorf("%q: no rows", q)
		}
	}
}
