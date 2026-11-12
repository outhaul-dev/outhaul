package main

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/outhaul-dev/outhaul/internal/config"
	"github.com/outhaul-dev/outhaul/internal/core"
	"github.com/outhaul-dev/outhaul/internal/server"
	"github.com/outhaul-dev/outhaul/internal/store"
)

func adminConfig(t *testing.T) (config.Config, config.Getenv) {
	t.Helper()
	dir := t.TempDir()
	getenv := func(k string) string {
		if k == "OUTHAUL_DATA_DIR" {
			return dir
		}
		return ""
	}
	return config.Load(getenv), getenv
}

func createAdminDB(t *testing.T, cfg config.Config) {
	t.Helper()
	st, err := store.Open(cfg.DBPath(), nil)
	if err != nil {
		t.Fatalf("create the database: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestAdminCreateMakesASystemAdmin(t *testing.T) {
	cfg, getenv := adminConfig(t)
	createAdminDB(t, cfg)
	var out, errOut strings.Builder

	code := runAdmin([]string{"create", "alice"}, getenv,
		strings.NewReader("password123\na second line is ignored\n"), &out, &errOut)
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errOut.String())
	}
	if out.String() != "Created system admin \"alice\".\n" {
		t.Errorf("stdout = %q, want the success line only", out.String())
	}
	if strings.Contains(out.String()+errOut.String(), "password123") {
		t.Error("the password was printed")
	}

	st, err := store.Open(cfg.DBPath(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	u, err := st.GetUserByName(context.Background(), "alice")
	if err != nil {
		t.Fatalf("GetUserByName: %v", err)
	}
	if u.Role != core.RoleSystemAdmin {
		t.Errorf("role = %q, want %q", u.Role, core.RoleSystemAdmin)
	}
	ok, err := server.VerifyPassword(u.PasswordHash, "password123")
	if err != nil || !ok {
		t.Errorf("the first line of standard input is not the password: ok=%v err=%v", ok, err)
	}
	if has, err := st.HasUser(context.Background()); err != nil || !has {
		t.Errorf("HasUser = %v, %v; the first account must stop the setup URL", has, err)
	}
}

func TestAdminCreateRefuses(t *testing.T) {
	t.Run("an existing name", func(t *testing.T) {
		cfg, getenv := adminConfig(t)
		createAdminDB(t, cfg)
		var out, errOut strings.Builder
		if code := runAdmin([]string{"create", "alice"}, getenv, strings.NewReader("password123\n"), &out, &errOut); code != 0 {
			t.Fatalf("first create: exit %d, %q", code, errOut.String())
		}
		out.Reset()
		errOut.Reset()
		code := runAdmin([]string{"create", "alice"}, getenv, strings.NewReader("otherpass1\n"), &out, &errOut)
		if code != 1 || !strings.Contains(errOut.String(), "There is already an account called alice.") {
			t.Errorf("second create = %d, %q; want 1 and the existing-name refusal", code, errOut.String())
		}
		st, err := store.Open(cfg.DBPath(), nil)
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		u, err := st.GetUserByName(context.Background(), "alice")
		if err != nil {
			t.Fatal(err)
		}
		if ok, _ := server.VerifyPassword(u.PasswordHash, "password123"); !ok {
			t.Error("the refused create changed the existing account's password")
		}
	})

	t.Run("a short password", func(t *testing.T) {
		cfg, getenv := adminConfig(t)
		createAdminDB(t, cfg)
		var out, errOut strings.Builder
		code := runAdmin([]string{"create", "alice"}, getenv, strings.NewReader("short\n"), &out, &errOut)
		if code != 1 || !strings.Contains(errOut.String(), "password must be at least 8 characters") {
			t.Errorf("short password = %d, %q", code, errOut.String())
		}
	})

	t.Run("a missing database", func(t *testing.T) {
		cfg, getenv := adminConfig(t)
		var out, errOut strings.Builder
		code := runAdmin([]string{"create", "alice"}, getenv, strings.NewReader("password123\n"), &out, &errOut)
		if code != 1 || !strings.Contains(errOut.String(), cfg.DBPath()) {
			t.Errorf("missing database = %d, %q; want 1 naming %s", code, errOut.String(), cfg.DBPath())
		}
		if _, err := os.Stat(cfg.DBPath()); !errors.Is(err, os.ErrNotExist) {
			t.Error("the command created a database")
		}
	})

	t.Run("migrations this binary doesn't have", func(t *testing.T) {
		cfg, getenv := adminConfig(t)
		createAdminDB(t, cfg)
		db, err := sql.Open("sqlite", "file:"+url.PathEscape(cfg.DBPath()))
		if err != nil {
			t.Fatal(err)
		}
		var name string
		if err := db.QueryRow(`SELECT name FROM schema_migrations ORDER BY name DESC LIMIT 1`).Scan(&name); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`DELETE FROM schema_migrations WHERE name = ?`, name); err != nil {
			t.Fatal(err)
		}
		db.Close()

		var out, errOut strings.Builder
		code := runAdmin([]string{"create", "alice"}, getenv, strings.NewReader("password123\n"), &out, &errOut)
		if code != 1 || !strings.Contains(errOut.String(), "installed binary") {
			t.Errorf("mismatched migrations = %d, %q; want 1 naming the installed binary", code, errOut.String())
		}
	})
}

func TestAdminUsageErrors(t *testing.T) {
	_, getenv := adminConfig(t)
	for _, args := range [][]string{
		nil,
		{"create"},
		{"create", "alice", "bob"},
		{"delete", "alice"},
	} {
		var out, errOut strings.Builder
		code := runAdmin(args, getenv, strings.NewReader("password123\n"), &out, &errOut)
		if code != 2 {
			t.Errorf("runAdmin(%q) = %d, want 2", args, code)
		}
		if !strings.Contains(errOut.String(), "usage: outhaul admin create NAME") {
			t.Errorf("runAdmin(%q) stderr = %q, want the usage line", args, errOut.String())
		}
	}
}

func TestCheckDatabaseOwnerRefusesAnotherUser(t *testing.T) {
	path := filepath.Join(t.TempDir(), "outhaul.db")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := checkDatabaseOwner(path, "alice", os.Geteuid()); err != nil {
		t.Errorf("the owner was refused: %v", err)
	}
	err := checkDatabaseOwner(path, "alice", os.Geteuid()+1)
	if err == nil || !strings.Contains(err.Error(), "sudo -u") || !strings.Contains(err.Error(), "admin create alice") {
		t.Errorf("another user = %v, want a refusal with the sudo -u line", err)
	}
	if err != nil && (!strings.Contains(err.Error(), "OUTHAUL_DATA_DIR="+filepath.Dir(path)) || !strings.Contains(err.Error(), " env ")) {
		t.Errorf("another user = %v, want the line to pass OUTHAUL_DATA_DIR=%s through env", err, filepath.Dir(path))
	}
}
