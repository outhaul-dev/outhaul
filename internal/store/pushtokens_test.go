package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/outhaul-dev/outhaul/internal/core"
)

var tokenNow = time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)

func tokenAccount(t *testing.T, s *Store, name string) core.User {
	t.Helper()
	u, err := s.CreatePasswordAccount(context.Background(), name, core.RoleSystemAdmin)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func mustToken(t *testing.T, s *Store, userID int64, label string, now, expires time.Time) (string, core.PushToken) {
	t.Helper()
	raw, tok, err := s.CreatePushToken(context.Background(), userID, label, now, expires)
	if err != nil {
		t.Fatalf("CreatePushToken(%q): %v", label, err)
	}
	return raw, tok
}

func TestCreatePushTokenStoresOnlyTheHash(t *testing.T) {
	s := newTestStore(t)
	u := tokenAccount(t, s, "pat")
	raw, tok := mustToken(t, s, u.ID, "ci", tokenNow, tokenNow.AddDate(0, 0, 7))
	raw2, _ := mustToken(t, s, u.ID, "laptop", tokenNow, tokenNow.AddDate(0, 0, 7))
	if !core.ValidPushTokenShape(raw) || !core.ValidPushTokenShape(raw2) {
		t.Fatalf("shapes: %q %q", raw, raw2)
	}
	if raw == raw2 {
		t.Fatal("two tokens were equal")
	}
	if tok.ID == 0 || tok.UserID != u.ID || tok.Label != "ci" || !tok.CreatedAt.Equal(tokenNow) || tok.LastUsedAt != nil {
		t.Errorf("token = %+v", tok)
	}

	var hash string
	if err := s.db.QueryRow(`SELECT token_hash FROM push_tokens WHERE id = ?`, tok.ID).Scan(&hash); err != nil {
		t.Fatal(err)
	}
	if hash != hashToken(raw) {
		t.Errorf("token_hash = %q, want the SHA-256 of the value", hash)
	}
	rows, err := s.db.Query(`SELECT id, user_id, label, token_hash, created_at, expires_at, COALESCE(last_used_at, ''), last_used_ip FROM push_tokens`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		cols := make([]string, 8)
		ptrs := make([]any, 8)
		for i := range cols {
			ptrs[i] = &cols[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		for _, c := range cols {
			for _, r := range []string{raw, raw[4:], raw2, raw2[4:]} {
				if strings.Contains(c, r) {
					t.Errorf("a column holds the token value: %q", c)
				}
			}
		}
	}
}

func TestFindPushToken(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	u := tokenAccount(t, s, "pat")
	raw, tok := mustToken(t, s, u.ID, "ci", tokenNow, tokenNow.Add(-time.Hour)) // already expired

	got, ok, err := s.FindPushToken(ctx, raw)
	if err != nil || !ok || got.ID != tok.ID || got.UserID != u.ID {
		t.Fatalf("FindPushToken = %+v, %v, %v", got, ok, err)
	}
	if !got.ExpiresAt.Equal(tok.ExpiresAt) {
		t.Errorf("an expired row must be returned as it is: %v", got.ExpiresAt)
	}
	flipped := raw[:len(raw)-1] + "0"
	if raw[len(raw)-1] == '0' {
		flipped = raw[:len(raw)-1] + "1"
	}
	for _, in := range []string{flipped, "hunter2", "", raw + "\n"} {
		if _, ok, err := s.FindPushToken(ctx, in); ok || err != nil {
			t.Errorf("FindPushToken(%q) = %v, %v; want not found", in, ok, err)
		}
	}
}

func TestGetPushToken(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	u := tokenAccount(t, s, "pat")
	_, tok := mustToken(t, s, u.ID, "ci", tokenNow, tokenNow.AddDate(0, 0, 7))
	got, err := s.GetPushToken(ctx, tok.ID)
	if err != nil || got.Label != "ci" {
		t.Fatalf("GetPushToken = %+v, %v", got, err)
	}
	if _, err := s.GetPushToken(ctx, tok.ID+100); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("missing row: %v, want sql.ErrNoRows", err)
	}
}

func TestPushTokenLabelRules(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	a, b := tokenAccount(t, s, "pat"), tokenAccount(t, s, "sam")
	exp := tokenNow.AddDate(0, 0, 7)
	mustToken(t, s, a.ID, "ci", tokenNow, exp)
	if _, _, err := s.CreatePushToken(ctx, a.ID, " ci ", tokenNow, exp); !errors.Is(err, ErrPushTokenLabelTaken) {
		t.Errorf("same label: %v, want ErrPushTokenLabelTaken", err)
	}
	mustToken(t, s, b.ID, "ci", tokenNow, exp)
	if _, _, err := s.CreatePushToken(ctx, a.ID, "  ", tokenNow, exp); !errors.Is(err, ErrPushTokenLabel) {
		t.Errorf("blank label: %v, want ErrPushTokenLabel", err)
	}
}

func TestPushTokenLimit(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	u := tokenAccount(t, s, "pat")
	for i := 0; i < core.MaxPushTokensPerUser; i++ {
		// Every other one is expired: expired ones count.
		exp := tokenNow.AddDate(0, 0, 7)
		if i%2 == 0 {
			exp = tokenNow.Add(-time.Hour)
		}
		mustToken(t, s, u.ID, fmt.Sprintf("t%d", i), tokenNow, exp)
	}
	if _, _, err := s.CreatePushToken(ctx, u.ID, "one-more", tokenNow, tokenNow.AddDate(0, 0, 7)); !errors.Is(err, ErrPushTokenLimit) {
		t.Errorf("26th: %v, want ErrPushTokenLimit", err)
	}
	other := tokenAccount(t, s, "sam")
	mustToken(t, s, other.ID, "t0", tokenNow, tokenNow.AddDate(0, 0, 7))
}

func TestListAndDeletePushTokens(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	a, b := tokenAccount(t, s, "pat"), tokenAccount(t, s, "sam")
	exp := tokenNow.AddDate(0, 0, 7)
	rawA1, t1 := mustToken(t, s, a.ID, "first", tokenNow, exp)
	_, t2 := mustToken(t, s, a.ID, "second", tokenNow.Add(time.Minute), exp)
	rawB, tb := mustToken(t, s, b.ID, "mine", tokenNow, exp)

	list, err := s.ListPushTokensForUser(ctx, a.ID, tokenNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].ID != t2.ID || list[1].ID != t1.ID {
		t.Fatalf("list = %+v, want second then first", list)
	}

	if err := s.DeletePushTokenForUser(ctx, a.ID, tb.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.FindPushToken(ctx, rawB); !ok {
		t.Error("another account's id deleted a token")
	}
	if err := s.DeletePushTokenForUser(ctx, a.ID, t1.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.FindPushToken(ctx, rawA1); ok {
		t.Error("the token survived its own account's delete")
	}
}

func TestExpiredPushTokensArePruned(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	u := tokenAccount(t, s, "pat")
	setup := func() (old, recent core.PushToken) {
		_, old = mustToken(t, s, u.ID, fmt.Sprintf("old%d", time.Now().UnixNano()), tokenNow.AddDate(0, 0, -60), tokenNow.AddDate(0, 0, -31))
		_, recent = mustToken(t, s, u.ID, fmt.Sprintf("recent%d", time.Now().UnixNano()), tokenNow.AddDate(0, 0, -60), tokenNow.AddDate(0, 0, -29))
		return old, recent
	}
	exists := func(id int64) bool {
		_, err := s.GetPushToken(ctx, id)
		return err == nil
	}

	for name, run := range map[string]func() error{
		"ListPushTokensForUser": func() error { _, err := s.ListPushTokensForUser(ctx, u.ID, tokenNow); return err },
		"ListPushTokens":        func() error { _, err := s.ListPushTokens(ctx, tokenNow); return err },
		"CreatePushToken": func() error {
			_, _, err := s.CreatePushToken(ctx, u.ID, fmt.Sprintf("fresh%d", time.Now().UnixNano()), tokenNow, tokenNow.AddDate(0, 0, 7))
			return err
		},
	} {
		old, recent := setup()
		if !exists(old.ID) {
			t.Fatalf("%s: setup", name)
		}
		if err := run(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if exists(old.ID) {
			t.Errorf("%s: a token expired 31 days ago is still there", name)
		}
		if !exists(recent.ID) {
			t.Errorf("%s: a token expired 29 days ago was removed", name)
		}
	}
	list, err := s.ListPushTokensForUser(ctx, u.ID, tokenNow)
	if err != nil || len(list) == 0 {
		t.Fatalf("recent expired tokens must still be listed: %v %v", list, err)
	}
}

func TestDeleteUserDeletesPushTokens(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	tokenAccount(t, s, "pat") // the last password admin stays
	gone, err := s.CreatePasswordAccount(ctx, "sam", core.RoleNone)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := mustToken(t, s, gone.ID, "ci", tokenNow, tokenNow.AddDate(0, 0, 7))
	if err := s.DeleteUser(ctx, gone.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.FindPushToken(ctx, raw); ok || err != nil {
		t.Errorf("FindPushToken after DeleteUser = %v, %v", ok, err)
	}
}

func TestTouchPushToken(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	u := tokenAccount(t, s, "pat")
	_, tok := mustToken(t, s, u.ID, "ci", tokenNow, tokenNow.AddDate(0, 0, 7))
	check := func(step string, wantAt time.Time, wantIP string) {
		t.Helper()
		got, err := s.GetPushToken(ctx, tok.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.LastUsedAt == nil || !got.LastUsedAt.Equal(wantAt) || got.LastUsedIP != wantIP {
			t.Errorf("%s: last used = %v from %q, want %v from %q", step, got.LastUsedAt, got.LastUsedIP, wantAt, wantIP)
		}
	}
	if err := s.TouchPushToken(ctx, tok.ID, tokenNow, "192.0.2.1"); err != nil {
		t.Fatal(err)
	}
	check("first", tokenNow, "192.0.2.1")

	if err := s.TouchPushToken(ctx, tok.ID, tokenNow.Add(30*time.Second), "192.0.2.1"); err != nil {
		t.Fatal(err)
	}
	check("30s, same address", tokenNow, "192.0.2.1")

	if err := s.TouchPushToken(ctx, tok.ID, tokenNow.Add(30*time.Second), "192.0.2.2"); err != nil {
		t.Fatal(err)
	}
	check("30s, new address", tokenNow.Add(30*time.Second), "192.0.2.2")

	if err := s.TouchPushToken(ctx, tok.ID, tokenNow.Add(91*time.Second), "192.0.2.2"); err != nil {
		t.Fatal(err)
	}
	check("61s later, same address", tokenNow.Add(91*time.Second), "192.0.2.2")
}

func TestListPushTokensNamesOwner(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	pw := tokenAccount(t, s, "pat")
	sso := insertSSOAdmin(t, s, "sub1")
	exp := tokenNow.AddDate(0, 0, 7)
	mustToken(t, s, pw.ID, "a", tokenNow, exp)
	mustToken(t, s, sso, "b", tokenNow, exp)
	rows, err := s.ListPushTokens(ctx, tokenNow)
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows = %+v, %v", rows, err)
	}
	owners := map[string]string{}
	for _, r := range rows {
		owners[r.Label] = r.Owner
	}
	if owners["a"] != "pat" || owners["b"] != "sub1@example.com" {
		t.Errorf("owners = %v", owners)
	}
}

func TestMigration0033(t *testing.T) {
	db := openMigratedBefore(t, "0033")
	if _, err := db.Exec(`INSERT INTO users (id, username, password_hash, created_at) VALUES (3, 'pat', 'h', '2026-07-04T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO push_keys (label, fingerprint, public_key, created_at, user_id) VALUES ('k', 'SHA256:x', 'ssh-ed25519 AAAA', '2026-07-04T00:00:00Z', 3)`); err != nil {
		t.Fatal(err)
	}
	if err := migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	for table, want := range map[string]int{"push_tokens": 0, "users": 1, "push_keys": 1} {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
			t.Fatalf("%s: %v", table, err)
		}
		if n != want {
			t.Errorf("%s has %d rows, want %d", table, n, want)
		}
	}
}
