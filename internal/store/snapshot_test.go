package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/outhaul-dev/outhaul/internal/core"
)

func openCopy(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestSnapshotUnderWrites(t *testing.T) {
	s := openWithBox(t)
	ctx := context.Background()
	if err := s.SetSetting(ctx, "k", "0"); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 1; i < 100; i++ {
			if err := s.SetSetting(ctx, "k", strconv.Itoa(i)); err != nil {
				t.Errorf("SetSetting: %v", err)
				return
			}
		}
	}()

	path := filepath.Join(t.TempDir(), "copy.db")
	if err := s.Snapshot(ctx, path); err != nil {
		t.Fatal(err)
	}
	wg.Wait()

	db := openCopy(t, path)
	var integrity string
	if err := db.QueryRow(`PRAGMA integrity_check`).Scan(&integrity); err != nil {
		t.Fatal(err)
	}
	if integrity != "ok" {
		t.Fatalf("integrity_check = %q", integrity)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM settings WHERE key = 'k'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("settings rows = %d, want 1", n)
	}
	var v string
	if err := db.QueryRow(`SELECT value FROM settings WHERE key = 'k'`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	if i, err := strconv.Atoi(v); err != nil || i < 0 || i > 99 {
		t.Fatalf("settings value = %q, want one of the written values", v)
	}
	for _, side := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(path + side); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s exists beside the copy", side)
		}
	}
}

func TestSnapshotRemovesSessions(t *testing.T) {
	s := openWithBox(t)
	ctx := context.Background()
	u, err := s.CreateUser(ctx, "admin", "hash")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSession(ctx, core.Session{
		Token: "super-secret-token", UserID: u.ID, ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	gateToken := "gate-raw-token"
	if err := s.CreateGateSession(ctx, gateToken, core.GateSession{
		Subject: u.GateSubject(), Kind: core.SignInPassword, Display: "dana",
		ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	gateHash := hashToken(gateToken)
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO route_sessions (token_hash, gate_token_hash, host, display, expires_at) VALUES (?, ?, ?, ?, ?)`,
		"route-hash", gateHash, "web.test", "dana", fmtTime(time.Now().Add(time.Hour))); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO gate_codes (code_hash, gate_token_hash, host, nonce_hash, expires_at) VALUES (?, ?, ?, ?, ?)`,
		"code-hash", gateHash, "web.test", "nonce-hash", fmtTime(time.Now().Add(time.Hour))); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(t.TempDir(), "copy.db")
	if err := s.Snapshot(ctx, path); err != nil {
		t.Fatal(err)
	}

	db := openCopy(t, path)
	for _, table := range []string{"sessions", "gate_sessions", "route_sessions", "gate_codes"} {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("%s has %d rows in the copy", table, n)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("super-secret-token")) {
		t.Error("the copy still contains the session token's bytes")
	}
}

func TestSnapshotIsOneFile(t *testing.T) {
	s := openWithBox(t)
	path := filepath.Join(t.TempDir(), "copy.db")
	if err := s.Snapshot(context.Background(), path); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("mode = %o, want 600", mode)
	}
	for _, side := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(path + side); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s exists beside the copy", side)
		}
	}
}

func TestSnapshotRefusesAnExistingTarget(t *testing.T) {
	s := openWithBox(t)
	path := filepath.Join(t.TempDir(), "copy.db")
	if err := os.WriteFile(path, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.Snapshot(context.Background(), path); err == nil {
		t.Fatal("Snapshot over an existing file succeeded, want an error")
	}
	if data, _ := os.ReadFile(path); string(data) != "keep me" {
		t.Errorf("existing file changed: %q", data)
	}
}

func TestSnapshotHoldsNoPushTokens(t *testing.T) {
	s := openWithBox(t)
	ctx := context.Background()
	u, err := s.CreateUser(ctx, "admin", "hash")
	if err != nil {
		t.Fatal(err)
	}
	raw, _, err := s.CreatePushToken(ctx, u.ID, "ci", tokenNow, tokenNow.AddDate(0, 0, 7))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "copy.db")
	if err := s.Snapshot(ctx, path); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := openCopy(t, path).QueryRow(`SELECT COUNT(*) FROM push_tokens`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("the copy holds %d push tokens", n)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte(hashToken(raw))) {
		t.Error("the copy still contains the token's hash")
	}
}

func TestStripSessionsRemovesPushTokens(t *testing.T) {
	s := openWithBox(t)
	ctx := context.Background()
	u, err := s.CreateUser(ctx, "admin", "hash")
	if err != nil {
		t.Fatal(err)
	}
	raw, _, err := s.CreatePushToken(ctx, u.ID, "ci", tokenNow, tokenNow.AddDate(0, 0, 7))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.StripSessions(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.FindPushToken(ctx, raw); ok || err != nil {
		t.Errorf("FindPushToken after StripSessions = %v, %v", ok, err)
	}
}
