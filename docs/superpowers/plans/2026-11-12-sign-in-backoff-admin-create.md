# Sign-in Backoff and `outhaul admin create` (R15) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add an opt-in, in-memory lock per username after repeated failed password sign-ins, and a host command, `outhaul admin create NAME`, that creates a password system admin so a lock can never keep every operator out.

**Architecture:** The command opens the existing database without creating or migrating it, hashes the password with the same code the UI uses, and inserts with `store.CreateUser`, the first-boot path. The lock is a pair of maps in `internal/server/backoff.go`, keyed by the SHA-256 of the typed name; `checkPassword` counts an attempt after the store lookup and before the hash slot, clears it on success, and gives it back when no hash slot came free. Both password forms render a 429 with `Retry-After`.

**Tech Stack:** Go 1.26, `modernc.org/sqlite` v1.53.0, `golang.org/x/crypto/argon2`, `golang.org/x/term` v0.44.0 (already in `go.sum`; this plan makes it a direct dependency).

**Spec:** `docs/superpowers/specs/2026-11-11-client-ips-streams-sign-in-design.md`, section "R15: Sign-in backoff, and a way back in", and the R15 entries under "Tests", "Docs", "Order of work" and "What only the real host can show".

**Before you start:** This series is independent of the R14 (long streams) and R16 (SSO from configuration) plans. Do the tasks in order: the command first, then the setting and the limiter.

## Global Constraints

- `OUTHAUL_SIGNIN_BACKOFF` is `on` or `off`, default `off`. With it off, sign-in behaves exactly as it does today: no counting, no lock, no new response.
- The lock lives in memory only. There is no migration, no database write and no new store method for it.
- The exact table: 1 to 4 failures are checked at once; 5 is refused for 30 seconds; 6, 7, 8 and 9 for 1, 2, 4 and 8 minutes; 10 or more for 15 minutes.
- A refused attempt is not checked, takes no hash slot, is not counted and does not extend the wait.
- The limiter is checked in `checkPassword`, after `GetUserByName` and before `withHashSlot`. The two-at-a-time hashing limit stays.
- The key is the SHA-256 of the typed name, never the name itself. The unknown-name map holds at most 10,000 entries and evicts the oldest failure; the account-name map has no cap beyond one entry per password account.
- `outhaul admin create` never creates or migrates a database. It refuses a missing file and a `schema_migrations` set that is not exactly this binary's.
- No password or hash goes on a command line, into a log line, onto a page, or into a test's failure output.
- This repository is public: use `example.com` names only.
- Test first: write the failing test, run it, see it fail for the stated reason, then implement.
- After every task: `go build ./... && go vet ./... && go test ./...`. Before calling the plan done: `go test -race -timeout 30m ./...`.
- Commits look like `feat(scope): summary` or `fix(scope): summary`, with a body saying what the user saw go wrong and what changed.

## Review Focus

The inputs and failures most likely to bite a person that the spec does not spell out. Each has its test in the task named.

1. A locked name with the right password is refused with a wait, and the refused attempts do not make the wait longer (Task 5).
2. A password account that has not set a password yet, and a name with no account, are counted the same way and land in the map the spec says; neither moves a real account's count (Tasks 4, 5).
3. A flood of 10,001 made-up names cannot evict a real account's entry (Task 4).
4. `outhaul admin create` while the running server holds the database open waits for its write and succeeds; it never creates or migrates a database (Tasks 1, 2).
5. root, or any user other than the database file's owner, is refused and told the `sudo -u` line (Task 2).

## File map

| File | Responsibility | Tasks |
|---|---|---|
| `internal/store/store.go` | shared `openSQLite`, `OpenExisting`, exact migration-set check | 1 |
| `internal/store/open_existing_test.go` (new) | missing file, older/newer migrations, live-database write | 1 |
| `admin.go` (new) | `runAdmin`, `adminCreate`, the terminal/stdin password prompt, owner check | 2 |
| `admin_test.go` (new) | success, refusals, usage, owner, first account | 2 |
| `main.go` | dispatch `admin`, usage line, `serve` setter | 2, 5 |
| `internal/server/users.go`, `internal/server/auth.go` | export `ValidUsername` and `ValidateCredentials`; clear the lock after a setup link | 2, 5 |
| `README.md`, `docs/SSO.md`, `AGENTS.md`, `ARCHITECTURE.md` | the command, the setting, the kept decisions | 2, 3, 5 |
| `internal/config/config.go`, `internal/config/config_test.go` | `SignInBackoff` from `OUTHAUL_SIGNIN_BACKOFF` | 3 |
| `internal/server/backoff.go` (new) | the two maps, the table, the locked error | 4 |
| `internal/server/backoff_test.go` (new) | table, hour, cap, give-back, concurrency, messages | 4 |
| `internal/server/server.go`, `internal/server/gate_signin.go` | the setter, `checkPassword`, 429 in both forms | 5 |
| `internal/server/sign_in_backoff_test.go` (new) | `/login`, `/gate/sign-in`, setup link, account page, busy slot | 5 |

---

## Task 1: Open an existing database without creating or migrating it

**Files:**
- Modify: `internal/store/store.go`
- Create: `internal/store/open_existing_test.go`
- Test: `internal/store/open_existing_test.go`

**Interfaces:**
- Consumes: `migrationsFS` in `internal/store/migrate.go`.
- Produces: `func OpenExisting(path string) (*Store, error)`. It opens the file read-write (`mode=rw`) with the same pragmas as `Open`, one connection, and no secret box; it returns an error before anything else if the file is missing, and closes the database and returns an error unless `schema_migrations` holds exactly the embedded migration names.
- `func Open(path string, box *secret.Box) (*Store, error)` keeps its signature and creates/migrates as before.

- [ ] **Step 1: Write the failing tests**

Create `internal/store/open_existing_test.go`:

```go
package store

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/outhaul-dev/outhaul/internal/core"
)

func tempDBPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "outhaul.db")
}

func createCurrentDB(t *testing.T, path string) {
	t.Helper()
	s, err := Open(path, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

func rawDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+url.PathEscape(path))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	return db
}

func lastMigration(t *testing.T) string {
	t.Helper()
	names, err := fs.Glob(migrationsFS, "migrations/*.sql")
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(names)
	return names[len(names)-1]
}

func TestOpenExistingRefusesMissingDatabase(t *testing.T) {
	path := tempDBPath(t)
	if _, err := OpenExisting(path); err == nil {
		t.Fatal("OpenExisting accepted a missing database")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Error("OpenExisting created the database")
	}
}

func TestOpenExistingRefusesOtherMigrationSets(t *testing.T) {
	t.Run("older", func(t *testing.T) {
		path := tempDBPath(t)
		createCurrentDB(t, path)
		db := rawDB(t, path)
		missing := lastMigration(t)
		if _, err := db.Exec(`DELETE FROM schema_migrations WHERE name = ?`, missing); err != nil {
			t.Fatal(err)
		}
		db.Close()

		_, err := OpenExisting(path)
		if err == nil || !strings.Contains(err.Error(), "installed binary") {
			t.Fatalf("OpenExisting = %v, want a refusal naming the installed binary", err)
		}
		db = rawDB(t, path)
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE name = ?`, missing).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("OpenExisting applied %s: an existing database must never be migrated", missing)
		}
	})

	t.Run("newer", func(t *testing.T) {
		path := tempDBPath(t)
		createCurrentDB(t, path)
		db := rawDB(t, path)
		if _, err := db.Exec(`INSERT INTO schema_migrations (name, applied_at) VALUES ('migrations/9999_future.sql', datetime('now'))`); err != nil {
			t.Fatal(err)
		}
		db.Close()

		_, err := OpenExisting(path)
		if err == nil || !strings.Contains(err.Error(), "installed binary") {
			t.Fatalf("OpenExisting = %v, want a refusal naming the installed binary", err)
		}
	})
}

func TestOpenExistingCreatesAccountsOnACurrentDatabase(t *testing.T) {
	path := tempDBPath(t)
	createCurrentDB(t, path)
	s, err := OpenExisting(path)
	if err != nil {
		t.Fatalf("OpenExisting: %v", err)
	}
	defer s.Close()

	u, err := s.CreateUser(context.Background(), "alice", "hash")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if u.Role != core.RoleSystemAdmin {
		t.Errorf("role = %q, want %q", u.Role, core.RoleSystemAdmin)
	}
	got, err := s.GetUserByName(context.Background(), "alice")
	if err != nil || got.ID != u.ID {
		t.Fatalf("GetUserByName = %+v, %v; want id %d", got, err, u.ID)
	}
}

func TestOpenExistingWritesWhileTheServerHoldsTheDatabaseOpen(t *testing.T) {
	path := tempDBPath(t)
	server, err := Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	cmd, err := OpenExisting(path)
	if err != nil {
		t.Fatalf("OpenExisting while the server holds the database: %v", err)
	}
	defer cmd.Close()

	if _, err := cmd.CreateUser(context.Background(), "alice", "hash"); err != nil {
		t.Fatalf("CreateUser while the server holds the database: %v", err)
	}
	if _, err := server.GetUserByName(context.Background(), "alice"); err != nil {
		t.Errorf("the server cannot read the account the command made: %v", err)
	}
}
```

The refusal checks use `strings.Contains`; `strings` is in the imports above.

- [ ] **Step 2: Run the tests to see them fail**

Run: `go test ./internal/store -run OpenExisting`

Expected: FAIL — `undefined: OpenExisting`.

- [ ] **Step 3: Implement `OpenExisting`**

In `internal/store/store.go`, split the DSN and connection setup out of `Open`:

```go
// openSQLite opens path with the pragmas every Outhaul connection uses. With
// create false it opens an existing file read-write and never creates one.
func openSQLite(path string, create bool) (*sql.DB, error) {
	dsn := "file:" + url.PathEscape(path)
	if create {
		dsn += "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
	} else {
		dsn += "?mode=rw&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	return db, nil
}
```

Change `Open` to call `openSQLite(path, true)`; keep the existing `Store{db: db, box: box}` and `migrate` error wrapping.

Add:

```go
// OpenExisting opens an Outhaul database that already exists, without creating
// or migrating it. It opens no secret box: callers that only create an account
// never touch a sealed value. The file's schema_migrations must hold exactly
// this binary's migrations, or nothing is changed.
func OpenExisting(path string) (*Store, error) {
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("no Outhaul database at %s; install Outhaul first", path)
		}
		return nil, err
	}
	db, err := openSQLite(path, false)
	if err != nil {
		return nil, err
	}
	if err := checkMigrationsCurrent(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &Store{db: db}, nil
}

// checkMigrationsCurrent refuses a database whose migration set is not exactly
// the one this binary embeds: an older file would need a migration this command
// must not run, and a newer file records a migration this binary doesn't know.
func checkMigrationsCurrent(db *sql.DB) error {
	rows, err := db.Query(`SELECT name FROM schema_migrations`)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "no such table") {
			return errors.New("no schema_migrations: this is not an Outhaul database")
		}
		return err
	}
	defer rows.Close()
	applied := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		applied[name] = true
	}
	if err := rows.Err(); err != nil {
		return err
	}

	names, err := fs.Glob(migrationsFS, "migrations/*.sql")
	if err != nil {
		return err
	}
	known := make(map[string]bool, len(names))
	for _, name := range names {
		known[name] = true
	}
	for name := range applied {
		if !known[name] {
			return fmt.Errorf("the database records migration %s, which this binary doesn't have; use the installed binary", name)
		}
	}
	for _, name := range names {
		if !applied[name] {
			return fmt.Errorf("the database is missing migration %s; start Outhaul once to migrate it, then use the installed binary", name)
		}
	}
	return nil
}
```

Add the imports `errors`, `io/fs`, `os` and `strings` to `store.go`.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/store -run OpenExisting`

Expected: PASS, including the live-database write.

- [ ] **Step 5: Run the package and commit**

Run: `go test ./internal/store`

```bash
git add internal/store/store.go internal/store/open_existing_test.go
git commit -m "feat(store): open an existing database without creating or migrating it" -m "The host admin command needs the database as the running server left it. OpenExisting refuses a missing file and a migration set that isn't exactly this binary's, and writes while the server holds the file open."
```

---

## Task 2: `outhaul admin create NAME`

**Files:**
- Create: `admin.go`, `admin_test.go`
- Modify: `main.go`, `internal/server/users.go`, `internal/server/auth.go`, `internal/server/sso_login_test.go`
- Modify: `README.md`, `docs/SSO.md`, `AGENTS.md`, `ARCHITECTURE.md`
- Modify: `go.mod`, `go.sum` (promote `golang.org/x/term`)
- Test: `admin_test.go`

**Interfaces:**
- Consumes: `store.OpenExisting` (Task 1), `store.CreateUser`, `server.HashPassword`, `server.VerifyPassword`.
- Produces:
  - `func runAdmin(args []string, getenv config.Getenv, stdin io.Reader, stdout, stderr io.Writer) int` — usage error 2, every other refusal 1, success 0.
  - `func adminCreate(ctx context.Context, cfg config.Config, name string, stdin io.Reader, out io.Writer) error`.
  - `func readAdminPassword(in io.Reader, out io.Writer) (string, error)`.
  - `func checkDatabaseOwner(path, name string, euid int) error`.
  - Exported `func ValidUsername(name string) error` and `func ValidateCredentials(username, password string) error` in `internal/server` (renamed from `validUsername` and `validateCredentials`; update their callers).

- [ ] **Step 1: Write the failing tests**

Create `admin_test.go`:

```go
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
}
```

- [ ] **Step 2: Run the tests to see them fail**

Run: `go test . -run 'AdminCreate|AdminUsage|CheckDatabaseOwner'`

Expected: FAIL — `undefined: runAdmin`, `checkDatabaseOwner`, and `server.ValidUsername`/`ValidateCredentials` once the test compiles.

- [ ] **Step 3: Export the UI checks**

In `internal/server/users.go`, rename `validUsername` to `ValidUsername`, add the doc comment:
`// ValidUsername reports whether name may be a password account's username.`
Update its caller in `users.go`, and the test at `internal/server/sso_login_test.go:1198`.

In `internal/server/auth.go`, rename `validateCredentials` to `ValidateCredentials`, add the doc comment:
`// ValidateCredentials enforces the minimal username and password rules.`
Update its two callers in `auth.go` and `users.go`.

- [ ] **Step 4: Create admin.go**

Create `admin.go`:

```go
package main

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"strconv"
	"strings"
	"syscall"

	"github.com/outhaul-dev/outhaul/internal/config"
	"github.com/outhaul-dev/outhaul/internal/server"
	"github.com/outhaul-dev/outhaul/internal/store"
	"golang.org/x/term"
)

// runAdmin implements `outhaul admin create NAME`. A usage error is 2, every
// other failure 1.
func runAdmin(args []string, getenv config.Getenv, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) != 2 || args[0] != "create" {
		fmt.Fprintln(stderr, "usage: outhaul admin create NAME")
		return 2
	}
	cfg := config.Load(getenv)
	if err := adminCreate(context.Background(), cfg, args[1], stdin, stdout); err != nil {
		fmt.Fprintf(stderr, "outhaul: %v\n", err)
		return 1
	}
	return 0
}

// adminCreate creates a password system admin in the database the server uses.
// It never creates or migrates the database, and it writes nothing to
// Outhaul's log.
func adminCreate(ctx context.Context, cfg config.Config, name string, stdin io.Reader, out io.Writer) error {
	if err := server.ValidUsername(name); err != nil {
		return err
	}
	path := cfg.DBPath()
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("no Outhaul database at %s; install Outhaul first", path)
		}
		return err
	}
	if err := checkDatabaseOwner(path, name, os.Geteuid()); err != nil {
		return err
	}
	st, err := store.OpenExisting(path)
	if err != nil {
		return err
	}
	defer st.Close()
	if _, err := st.GetUserByName(ctx, name); err == nil {
		return fmt.Errorf("There is already an account called %s.", name)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}

	password, err := readAdminPassword(stdin, out)
	if err != nil {
		return err
	}
	if err := server.ValidateCredentials(name, password); err != nil {
		return err
	}
	hash, err := server.HashPassword(password)
	if err != nil {
		return err
	}
	if _, err := st.CreateUser(ctx, name, hash); err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return fmt.Errorf("There is already an account called %s.", name)
		}
		return err
	}
	fmt.Fprintf(out, "Created system admin %q.\n", name)
	return nil
}

// readAdminPassword asks twice without echo at a terminal. When standard input
// isn't a terminal, the first line of it is the password.
func readAdminPassword(in io.Reader, out io.Writer) (string, error) {
	if f, ok := in.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		fmt.Fprint(out, "Password: ")
		first, err := term.ReadPassword(int(f.Fd()))
		fmt.Fprintln(out)
		if err != nil {
			return "", err
		}
		fmt.Fprint(out, "Password again: ")
		second, err := term.ReadPassword(int(f.Fd()))
		fmt.Fprintln(out)
		if err != nil {
			return "", err
		}
		if string(first) != string(second) {
			return "", errors.New("the two passwords don't match")
		}
		return string(first), nil
	}

	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	if line == "" {
		return "", errors.New("no password on standard input")
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// checkDatabaseOwner refuses a user other than the database file's owner, so
// root can't leave files in the data directory the service can't write. The
// refusal names the sudo -u line that works.
func checkDatabaseOwner(path, name string, euid int) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	uid := int(st.Uid)
	if uid == euid {
		return nil
	}
	owner := strconv.Itoa(uid)
	if u, err := user.LookupId(owner); err == nil {
		owner = u.Username
	}
	return fmt.Errorf("run it as %s, the account that owns the database: sudo -u %s outhaul admin create %s", owner, owner, name)
}
```

Run `go get golang.org/x/term@v0.44.0` (it is already in `go.sum`; this only makes it a direct requirement), then `go mod tidy`.

- [ ] **Step 5: Dispatch the command from main**

In `main.go`, dispatch after `restore-state`:

```go
	if len(os.Args) >= 2 && os.Args[1] == "admin" {
		os.Exit(runAdmin(os.Args[2:], os.Getenv, os.Stdin, os.Stdout, os.Stderr))
	}
```

Add to `usage`:

```go
	fmt.Fprintln(os.Stderr, "       outhaul admin create NAME")
```

- [ ] **Step 6: Update the documentation**

In `README.md`, after the "Restoring Outhaul's own state" section, add:

````md
### Creating an admin from the host

`outhaul admin create` makes a password system admin when nobody can sign in,
for example while a username is locked after repeated failed sign-ins:

```sh
sudo -u outhaul outhaul admin create alice
```

Run it as the account that owns the database (`outhaul` on a normal install);
it refuses other users, root included, and prints the `sudo -u` line to use. It
asks for the password twice without echo, or reads the first line of standard
input when there is no terminal, and never takes the password as an argument.
It creates an account only: it does not reset a password, unlock a name, or
create a database. Set `OUTHAUL_DATA_DIR` if the data directory isn't the
default.
````

In `docs/SSO.md`, "Keeping a password system admin", replace the sentence saying
there is no command-line way back in with:

````md
There is a command-line way back in. On the host, as the account that owns the
database:

```sh
sudo -u outhaul outhaul admin create NAME
```

That creates a new password system admin who can sign in at once. It doesn't
reset an existing password or clear a sign-in lock; a lock ends by itself within
15 minutes.
````

In `AGENTS.md`, "Roles", replace `The last password system admin can't be removed or demoted, because there is no recovery command.` with:

```md
The last password system admin can't be removed or demoted: it is how people get
in when SSO is down, and the only other way, `outhaul admin create`, needs a
shell on the host.
```

In `ARCHITECTURE.md`, "Auth", after the first-boot setup sentence, add:

```md
A system admin can also be created on the host with `outhaul admin create`.
```

- [ ] **Step 7: Run the tests and verify the usage line**

Run: `go build ./... && go vet ./... && go test . ./internal/server ./internal/store`

Run: `go run . 2>&1 | grep 'outhaul admin create NAME'`

Expected: the tests pass, and the usage line prints.

- [ ] **Step 8: Commit**

```bash
git add admin.go admin_test.go main.go go.mod go.sum \
  internal/server/users.go internal/server/auth.go internal/server/sso_login_test.go \
  README.md docs/SSO.md AGENTS.md ARCHITECTURE.md
git commit -m "feat(admin): create a password system admin from the host" -m "A locked name, a forgotten password or a broken SSO provider had no way back in. admin create opens the existing database without migrating it, asks for the password without echo, and makes a system admin the server reads at once."
```

---

## Task 3: The `OUTHAUL_SIGNIN_BACKOFF` setting

**Files:**
- Modify: `internal/config/config.go`, `internal/config/config_test.go`, `README.md`
- Test: `internal/config/config_test.go`

**Interfaces:**
- Produces: `Config.SignInBackoff bool`, true for `on`, `true`, `yes` or `1` in any case; false by default and for `off`, `false`, `no` or `0`. A malformed value leaves it false and is reported by `Validate`.

- [ ] **Step 1: Write the failing tests**

Append to `internal/config/config_test.go`:

```go
func TestLoadSignInBackoff(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"", false},
		{"on", true},
		{"off", false},
		{"TRUE", true},
		{"yes", true},
		{"no", false},
	} {
		c := Load(func(k string) string {
			if k == "OUTHAUL_SIGNIN_BACKOFF" {
				return tc.in
			}
			return ""
		})
		if c.SignInBackoff != tc.want {
			t.Errorf("OUTHAUL_SIGNIN_BACKOFF=%q -> %v, want %v", tc.in, c.SignInBackoff, tc.want)
		}
		if err := c.Validate(); err != nil {
			t.Errorf("OUTHAUL_SIGNIN_BACKOFF=%q: Validate: %v", tc.in, err)
		}
	}
}

func TestValidateRejectsMalformedSignInBackoff(t *testing.T) {
	c := Load(func(k string) string {
		if k == "OUTHAUL_SIGNIN_BACKOFF" {
			return "maybe"
		}
		return ""
	})
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "OUTHAUL_SIGNIN_BACKOFF") {
		t.Errorf("Validate = %v, want an error naming OUTHAUL_SIGNIN_BACKOFF", err)
	}
}
```

- [ ] **Step 2: Run the tests to see them fail**

Run: `go test ./internal/config -run SignInBackoff`

Expected: FAIL — `c.SignInBackoff` is undefined.

- [ ] **Step 3: Implement the setting**

In `Config`, next to `AdminDirect`:

```go
	SignInBackoff bool // OUTHAUL_SIGNIN_BACKOFF: lock a username after repeated failed password sign-ins
```

In `Load`:

```go
		SignInBackoff:      p.bool("OUTHAUL_SIGNIN_BACKOFF"),
```

In `README.md`'s configuration table, add after `OUTHAUL_ADMIN_DIRECT`:

```md
| `OUTHAUL_SIGNIN_BACKOFF` | `off` | `on` locks a username after repeated failed password sign-ins: 30 seconds, then 1, 2, 4 and 8 minutes, then 15; in memory. The way back in is `outhaul admin create` |
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/config -run SignInBackoff`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/config/config.go internal/config/config_test.go README.md
git commit -m "feat(config): OUTHAUL_SIGNIN_BACKOFF, off by default" -m "The sign-in lock is opt-in. The setting takes the same yes/no values as the others, and a typo stops startup through Validate instead of silently leaving it off."
```

---

## Task 4: The sign-in limiter

**Files:**
- Create: `internal/server/backoff.go`, `internal/server/backoff_test.go`
- Test: `internal/server/backoff_test.go`

**Interfaces:**
- Produces:
  - `const maxUnknownSignInNames = 10_000`
  - `type signInLimiter struct { now func() time.Time; ... }`
  - `func newSignInLimiter(now func() time.Time) *signInLimiter`
  - `func (l *signInLimiter) allow(name string, known bool) (wait time.Duration, ok bool)` — `ok` false means refused for `wait`; an allowed attempt is already counted.
  - `func (l *signInLimiter) clear(name string)`
  - `func (l *signInLimiter) giveBack(name string, known bool)`
  - `func lockFor(failures int) time.Duration`
  - `type signInLockedError struct { wait time.Duration }`, with `Error() string` and `retryAfter() int` (whole seconds, rounded up).

- [ ] **Step 1: Write the failing tests**

Create `internal/server/backoff_test.go`. Use this fake clock helper:

```go
func newTestLimiter() (*signInLimiter, *time.Time) {
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	return newSignInLimiter(func() time.Time { return now }), &now
}
```

The table test:

```go
func TestSignInLimiterLockTable(t *testing.T) {
	for _, tc := range []struct {
		failures int
		wait     time.Duration
	}{
		{0, 0}, {1, 0}, {2, 0}, {3, 0}, {4, 0},
		{5, 30 * time.Second},
		{6, time.Minute},
		{7, 2 * time.Minute},
		{8, 4 * time.Minute},
		{9, 8 * time.Minute},
		{10, 15 * time.Minute},
		{15, 15 * time.Minute},
	} {
		l, now := newTestLimiter()
		for i := 0; i < tc.failures; i++ {
			// Move past the wait the previous failure set. The last
			// failure's wait must still be live for the check below.
			*now = now.Add(lockFor(i))
			wait, ok := l.allow("admin", true)
			if !ok {
				t.Fatalf("%d failures: attempt %d was refused for %v", tc.failures, i+1, wait)
			}
		}
		wait, ok := l.allow("admin", true)
		if tc.wait == 0 {
			if !ok {
				t.Errorf("%d failures: the next attempt was refused; want it checked", tc.failures)
			}
		} else if ok || wait != tc.wait {
			t.Errorf("%d failures: next = (%v, %v), want refused for %v", tc.failures, wait, ok, tc.wait)
		}
	}
}
```

Add these tests:

```go
func TestSignInLimiterHourClearsAName(t *testing.T) {
	l, now := newTestLimiter()
	for i := 0; i < 4; i++ {
		l.allow("admin", true)
	}
	*now = now.Add(time.Hour + time.Second)
	if _, ok := l.allow("admin", true); !ok {
		t.Error("the first attempt after an hour was refused; the count should start again")
	}
	if f := l.known[sha256.Sum256([]byte("admin"))]; f == nil || f.count != 1 {
		t.Errorf("after an hour the record is %+v, want one fresh failure", f)
	}
}

func TestSignInLimiterSuccessClearsAName(t *testing.T) {
	l, _ := newTestLimiter()
	for i := 0; i < 5; i++ {
		l.allow("admin", true)
	}
	l.clear("admin")
	if wait, ok := l.allow("admin", true); !ok {
		t.Errorf("after a success the next attempt was refused for %v", wait)
	}
}

func TestSignInLimiterGiveBack(t *testing.T) {
	l, _ := newTestLimiter()
	for i := 0; i < 4; i++ {
		l.allow("admin", true)
	}
	if wait, ok := l.allow("admin", true); !ok || wait != 0 {
		t.Fatalf("the fifth attempt = (%v, %v), want it counted and checked", wait, ok)
	}
	l.giveBack("admin", true)
	if wait, ok := l.allow("admin", true); !ok {
		t.Errorf("after a give-back the next attempt was refused for %v; the attempt that never ran was still counted", wait)
	}
}

func TestRefusedAttemptsDoNotExtendTheLock(t *testing.T) {
	l, now := newTestLimiter()
	for i := 0; i < 5; i++ {
		l.allow("admin", true)
	}
	for i := 0; i < 20; i++ {
		if _, ok := l.allow("admin", true); ok {
			t.Fatal("a refused attempt was allowed")
		}
	}
	*now = now.Add(31 * time.Second)
	if wait, ok := l.allow("admin", true); !ok {
		t.Errorf("after the 30-second wait a burst of refused attempts still locked the name for %v", wait)
	}
}

func TestConcurrentAttemptsAfterFourFailuresCheckOnlyOne(t *testing.T) {
	l, _ := newTestLimiter()
	for i := 0; i < 4; i++ {
		l.allow("admin", true)
	}
	var wg sync.WaitGroup
	allowed := make(chan struct{}, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, ok := l.allow("admin", true); ok {
				allowed <- struct{}{}
			}
		}()
	}
	wg.Wait()
	close(allowed)
	if n := len(allowed); n != 1 {
		t.Errorf("%d simultaneous attempts were checked after 4 failures, want exactly 1", n)
	}
}

func TestUnknownNamesAreCappedAndTheOldestGoes(t *testing.T) {
	l, now := newTestLimiter()
	for i := 0; i < maxUnknownSignInNames; i++ {
		l.allow(fmt.Sprintf("name-%d", i), false)
		*now = now.Add(time.Millisecond)
	}
	if len(l.unknown) != maxUnknownSignInNames {
		t.Fatalf("unknown map holds %d, want %d", len(l.unknown), maxUnknownSignInNames)
	}
	l.allow("one-more", false)
	if len(l.unknown) != maxUnknownSignInNames {
		t.Errorf("unknown map holds %d after the flood, want %d", len(l.unknown), maxUnknownSignInNames)
	}
	if l.unknown[sha256.Sum256([]byte("name-0"))] != nil {
		t.Error("the oldest unknown name survived the cap")
	}
}

func TestAFloodOfUnknownNamesLeavesARealAccountLocked(t *testing.T) {
	l, _ := newTestLimiter()
	for i := 0; i < 5; i++ {
		l.allow("admin", true)
	}
	for i := 0; i < maxUnknownSignInNames+1; i++ {
		l.allow(fmt.Sprintf("name-%d", i), false)
	}
	if wait, ok := l.allow("admin", true); ok {
		t.Errorf("a flood of made-up names unlocked a real account (wait %v)", wait)
	}
}

func TestALongNameCostsOneHashedKey(t *testing.T) {
	l, _ := newTestLimiter()
	l.allow(strings.Repeat("x", 1<<20), false)
	if len(l.unknown) != 1 {
		t.Errorf("unknown map holds %d entries for one name", len(l.unknown))
	}
}

func TestSignInLockedErrorCopy(t *testing.T) {
	for _, tc := range []struct {
		wait   time.Duration
		copy   string
		retry  int
	}{
		{30 * time.Second, "Too many failed sign-ins for this username. Try again in less than a minute.", 30},
		{time.Minute, "Too many failed sign-ins for this username. Try again in 1 minute.", 60},
		{4 * time.Minute, "Too many failed sign-ins for this username. Try again in 4 minutes.", 240},
	} {
		e := &signInLockedError{wait: tc.wait}
		if got := e.Error(); got != tc.copy {
			t.Errorf("wait %v: Error() = %q, want %q", tc.wait, got, tc.copy)
		}
		if got := e.retryAfter(); got != tc.retry {
			t.Errorf("wait %v: retryAfter() = %d, want %d", tc.wait, got, tc.retry)
		}
	}
}
```

Imports: `crypto/sha256`, `fmt`, `strings`, `sync`, `testing`, `time`.

- [ ] **Step 2: Run the tests to see them fail**

Run: `go test ./internal/server -run 'SignInLimiter|RefusedAttempts|ConcurrentAttempts|UnknownNames|FloodOfUnknown|LongName|SignInLockedError'`

Expected: FAIL — `undefined: signInLimiter`, `newSignInLimiter`, `lockFor`, `signInLockedError`, `maxUnknownSignInNames`.

- [ ] **Step 3: Implement the limiter**

Create `internal/server/backoff.go`:

```go
package server

import (
	"crypto/sha256"
	"fmt"
	"sync"
	"time"
)

// maxUnknownSignInNames bounds the map for names that are not password
// accounts, so a flood of made-up names cannot grow memory without limit.
const maxUnknownSignInNames = 10_000

// signInFailures is one name's failed attempts. count is the failures in a
// row, last the most recent, and until when the name may next be tried.
type signInFailures struct {
	count int
	last  time.Time
	until time.Time
}

// signInLimiter counts failed password sign-ins per typed username. Names are
// kept as their SHA-256, so a name as long as the request cap allows still
// costs 32 bytes. The zero value is not ready; use newSignInLimiter.
type signInLimiter struct {
	mu      sync.Mutex
	known   map[[32]byte]*signInFailures // names GetUserByName finds
	unknown map[[32]byte]*signInFailures // every other name, capped
	now     func() time.Time
}

func newSignInLimiter(now func() time.Time) *signInLimiter {
	return &signInLimiter{
		known:   map[[32]byte]*signInFailures{},
		unknown: map[[32]byte]*signInFailures{},
		now:     now,
	}
}

// lockFor is how long a name is refused after failures failed sign-ins:
// 1 to 4 are not refused, 5 for 30 seconds, 6 to 9 for 1, 2, 4 and 8 minutes,
// and 10 or more for 15 minutes.
func lockFor(failures int) time.Duration {
	switch {
	case failures < 5:
		return 0
	case failures == 5:
		return 30 * time.Second
	case failures == 6:
		return time.Minute
	case failures == 7:
		return 2 * time.Minute
	case failures == 8:
		return 4 * time.Minute
	case failures == 9:
		return 8 * time.Minute
	default:
		return 15 * time.Minute
	}
}

// allow counts one attempt for name. ok false means it is refused for wait and
// is not counted. An allowed attempt is counted before its password is
// checked, and sets any wait the table gives it.
func (l *signInLimiter) allow(name string, known bool) (wait time.Duration, ok bool) {
	key := sha256.Sum256([]byte(name))
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()

	m := l.unknown
	if known {
		m = l.known
	}
	if f, exists := m[key]; exists {
		switch {
		case now.Sub(f.last) >= time.Hour:
			delete(m, key)
		case now.Before(f.until):
			return f.until.Sub(now), false
		}
	}
	if !known && len(l.unknown) >= maxUnknownSignInNames {
		if _, exists := l.unknown[key]; !exists {
			l.evictOldestUnknown()
		}
	}
	f := m[key]
	if f == nil {
		f = &signInFailures{}
		m[key] = f
	}
	f.count++
	f.last = now
	f.until = now.Add(lockFor(f.count))
	return 0, true
}

// clear forgets name, after a successful sign-in or a completed setup link.
func (l *signInLimiter) clear(name string) {
	key := sha256.Sum256([]byte(name))
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.known, key)
	delete(l.unknown, key)
}

// giveBack undoes an allowed attempt whose password was never checked because
// no hash slot came free.
func (l *signInLimiter) giveBack(name string, known bool) {
	key := sha256.Sum256([]byte(name))
	l.mu.Lock()
	defer l.mu.Unlock()
	m := l.unknown
	if known {
		m = l.known
	}
	f := m[key]
	if f == nil {
		return
	}
	f.count--
	if f.count <= 0 {
		delete(m, key)
		return
	}
	f.until = f.last.Add(lockFor(f.count))
}

// evictOldestUnknown removes the unknown name whose last failure is oldest.
// Ties are arbitrary; all such names are equally valueless.
func (l *signInLimiter) evictOldestUnknown() {
	var (
		oldestKey [32]byte
		oldest    time.Time
		first     = true
	)
	for key, f := range l.unknown {
		if first || f.last.Before(oldest) {
			oldestKey, oldest, first = key, f.last, false
		}
	}
	delete(l.unknown, oldestKey)
}

// signInLockedError is a refused sign-in: the name is locked and wait remains.
type signInLockedError struct{ wait time.Duration }

func (e *signInLockedError) Error() string {
	seconds := e.retryAfter()
	if seconds < 60 {
		return "Too many failed sign-ins for this username. Try again in less than a minute."
	}
	minutes := (seconds + 59) / 60
	if minutes == 1 {
		return "Too many failed sign-ins for this username. Try again in 1 minute."
	}
	return fmt.Sprintf("Too many failed sign-ins for this username. Try again in %d minutes.", minutes)
}

// retryAfter is the wait in whole seconds, rounded up for Retry-After.
func (e *signInLockedError) retryAfter() int {
	return int((e.wait + time.Second - 1) / time.Second)
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/server -run 'SignInLimiter|RefusedAttempts|ConcurrentAttempts|UnknownNames|FloodOfUnknown|LongName|SignInLockedError'`

Expected: PASS. Run with `-race` too:
`go test -race ./internal/server -run 'ConcurrentAttempts'`

- [ ] **Step 5: Commit**

```bash
git add internal/server/backoff.go internal/server/backoff_test.go
git commit -m "feat(server): an in-memory limiter for repeated failed sign-ins" -m "Password guessing was bounded only by two concurrent hashes. Count failures per typed name, refuse the next attempt for 30 seconds to 15 minutes, and cap the made-up-name map so a flood cannot evict a real account."
```

---

## Task 5: Wire the limiter into both sign-in forms

**Files:**
- Modify: `internal/server/server.go`, `internal/server/gate_signin.go`, `internal/server/auth.go`, `internal/server/users.go`, `main.go`, `AGENTS.md`
- Create: `internal/server/sign_in_backoff_test.go`
- Test: `internal/server/sign_in_backoff_test.go`

**Interfaces:**
- Consumes: `signInLimiter`, `signInLockedError`, `lockFor` (Task 4); `Config.SignInBackoff` (Task 3).
- Produces:
  - `func (s *Server) SetSignInBackoff(on bool)` — installs a fresh limiter with `time.Now`, or nil; call before `Handler`.
  - `checkPassword` counts before the hash slot, gives back on `errHashBusy`, and clears on success.
  - Both `handleLoginSubmit` and `handleGateSignIn` answer a locked attempt with 429, `Retry-After`, and the locked message.
  - `handleAccountSetupSubmit` clears the name after `CompleteSetupLink`.

- [ ] **Step 1: Write the failing tests**

Create `internal/server/sign_in_backoff_test.go`. Helpers:

```go
func backoffEnv(t *testing.T) (*testEnv, *time.Time) {
	t.Helper()
	e := newTestEnv(t)
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	e.srv.SetSignInBackoff(true)
	e.srv.signIn.now = func() time.Time { return now }
	return e, &now
}

func postLogin(t *testing.T, e *testEnv, name, password string) (*http.Response, string) {
	t.Helper()
	resp := e.postForm(t, "/login", url.Values{"username": {name}, "password": {password}})
	return resp, body(t, resp)
}
```

The main test:

```go
func TestLoginRefusesALockedNameWithTheRightPassword(t *testing.T) {
	e, now := backoffEnv(t)
	passwordAdmin(t, e, "admin", "supersecret")

	for i := 0; i < 5; i++ {
		resp, page := postLogin(t, e, "admin", "wrong")
		if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(page, "Invalid username or password.") {
			t.Fatalf("wrong attempt %d = %d %q, want 401", i+1, resp.StatusCode, page)
		}
	}

	resp, page := postLogin(t, e, "admin", "supersecret")
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("the right password during a lock = %d, want 429", resp.StatusCode)
	}
	if got := resp.Header.Get("Retry-After"); got != "30" {
		t.Errorf("Retry-After = %q, want 30", got)
	}
	if !strings.Contains(page, "Too many failed sign-ins for this username. Try again in less than a minute.") {
		t.Errorf("locked page = %q", page)
	}

	*now = now.Add(31 * time.Second)
	resp, _ = postLogin(t, e, "admin", "supersecret")
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("after the wait = %d, want 303", resp.StatusCode)
	}
}
```

Add the remaining tests:

- `TestLoginLockShowsTheWaitInMinutes`: five wrong attempts; advance 31s; 6th wrong; advance 61s; 7th wrong; advance 121s; 8th wrong; then the right password is a 429 whose body says `Try again in 4 minutes.` and whose `Retry-After` is `240`.
- `TestUnknownUsernameLocksTheSameWay`: five wrong attempts for `nobody`; the sixth is 429 with the same copy and `Retry-After: 30`.
- `TestBackoffOffDoesNotLock`: default env, six wrong attempts for `admin`, every one 401; `e.srv.signIn == nil`.
- `TestBusyHashSlotGivesTheAttemptBack`: `passwordAdmin`; backoff env; `e.srv.hashWait = 50 * time.Millisecond`; fill `e.srv.hashSlots`; make five requests that are 503; drain the slots; the right password is 303 (the never-checked attempts were given back and did not lock).
- `TestAccountPasswordCheckIsNotCounted`: `backoffEnv`; `e.login(t)`; six POSTs to `/account/password` with the wrong current password and a valid new one, every one 400; `e.srv.signIn.known` and `.unknown` are both empty; a wrong `/login` is 401, not 429.
- `TestStoreFailureCountsAsAnUnknownName`: `backoffEnv`; `e.store.Close()`; one wrong `/login` is 401 and `len(e.srv.signIn.unknown) == 1` (the lookup failure must not bypass the limiter or reveal that the name isn't known).
- `TestPasswordAccountWithoutAPasswordIsKnown`: `backoffEnv`; `e.store.CreatePasswordAccount(ctx, "bob", core.RoleProjectMember)`; five wrong logins for `bob` are 401 and leave an entry in `e.srv.signIn.known`, none in `.unknown`; then add `maxUnknownSignInNames+1` unknown names directly through `e.srv.signIn.allow`, and `allow("bob", true)` is still refused.

The gate test:

```go
func TestGateSignInIsLockedToo(t *testing.T) {
	e := gateSignInEnv(t)
	passwordAccount(t, e, "nora", core.RoleNone, "supersecret")
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	e.srv.SetSignInBackoff(true)
	e.srv.signIn.now = func() time.Time { return now }

	for i := 0; i < 5; i++ {
		resp := e.gateRequest(t, http.MethodPost, "/gate/sign-in", "",
			signInForm("nora", "wrong", "https://wiki.example.com/", testNonce))
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("wrong gate attempt %d = %d, want 401", i+1, resp.StatusCode)
		}
	}
	resp := e.gateRequest(t, http.MethodPost, "/gate/sign-in", "",
		signInForm("nora", "supersecret", "https://wiki.example.com/", testNonce))
	page := body(t, resp)
	if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") != "30" {
		t.Errorf("locked gate sign-in = %d, Retry-After %q; want 429 and 30", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
	if !strings.Contains(page, "Too many failed sign-ins for this username.") {
		t.Errorf("gate locked page = %q", page)
	}
}
```

The setup-link test:

```go
func TestSetupLinkClearsTheLock(t *testing.T) {
	e, _ := backoffEnv(t)
	ctx := context.Background()
	u, err := e.store.CreatePasswordAccount(ctx, "bob", core.RoleProjectMember)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		resp, _ := postLogin(t, e, "bob", "wrong")
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("wrong attempt %d = %d, want 401", i+1, resp.StatusCode)
		}
	}
	if err := e.store.CreateSetupLink(ctx, u.ID, "setup-token", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	resp := e.postForm(t, "/account/setup", url.Values{
		"token": {"setup-token"}, "password": {"newpassword1"},
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("setup link = %d, want 303", resp.StatusCode)
	}
	resp, _ = postLogin(t, e, "bob", "wrong")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("after a setup link a wrong password = %d, want 401 (the lock was cleared)", resp.StatusCode)
	}
}
```

- [ ] **Step 2: Run the tests to see them fail**

Run: `go test ./internal/server -run 'LoginRefusesALocked|LoginLockShows|UnknownUsernameLocks|BackoffOff|BusyHashSlot|AccountPasswordCheck|StoreFailureCounts|PasswordAccountWithout|GateSignInIsLocked|SetupLinkClears'`

Expected: FAIL — `SetSignInBackoff` and `Server.signIn` are undefined; no response is 429.

- [ ] **Step 3: Add the server fields and setter**

In `Server` (`server.go`), next to `hashSlots`:

```go
	signIn    *signInLimiter // nil unless OUTHAUL_SIGNIN_BACKOFF=on
	signInLog onceAMinute    // the "locked" line; see checkPassword
```

Add the setter near `SetHTTPSPort`/`SetRouting`:

```go
// SetSignInBackoff turns on the per-username lock after repeated failed
// password sign-ins. Call before Handler.
func (s *Server) SetSignInBackoff(on bool) {
	if on {
		s.signIn = newSignInLimiter(time.Now)
		return
	}
	s.signIn = nil
}
```

- [ ] **Step 4: Count the attempt in checkPassword**

In `checkPassword` (`gate_signin.go`), replace the body with:

```go
func (s *Server) checkPassword(ctx context.Context, username, password string) (core.User, error) {
	user, err := s.store.GetUserByName(ctx, username)
	known := err == nil
	if s.signIn != nil {
		wait, ok := s.signIn.allow(username, known)
		if !ok {
			if known && s.signInLog.allow(time.Now()) {
				log.Printf("server: sign-in for %q is locked after repeated failures", logText(username))
			}
			return core.User{}, &signInLockedError{wait: wait}
		}
	}
	if err != nil || user.PasswordHash == "" || user.SignIn != core.SignInPassword {
		return core.User{}, errBadCredentials
	}
	var ok bool
	if busy := s.withHashSlot(ctx, func() { ok, err = VerifyPassword(user.PasswordHash, password) }); busy != nil {
		if s.signIn != nil {
			s.signIn.giveBack(username, known)
		}
		return core.User{}, busy
	}
	if err != nil || !ok {
		return core.User{}, errBadCredentials
	}
	if s.signIn != nil {
		s.signIn.clear(username)
	}
	return user, nil
}
```

- [ ] **Step 5: Answer a lock with 429 on both password forms**

In `handleLoginSubmit` (`auth.go`), before the `errHashBusy` branch:

```go
	var locked *signInLockedError
	if errors.As(err, &locked) {
		w.Header().Set("Retry-After", strconv.Itoa(locked.retryAfter()))
		s.loginPage(w, r, http.StatusTooManyRequests, locked.Error())
		return
	}
```

Add `strconv` to `auth.go`'s imports.

In `handleGateSignIn` (`gate_signin.go`), before the `errHashBusy` branch:

```go
	var locked *signInLockedError
	if errors.As(err, &locked) {
		w.Header().Set("Retry-After", strconv.Itoa(locked.retryAfter()))
		s.gateLoginPage(w, r, http.StatusTooManyRequests, host, rd, n, locked.Error())
		return
	}
```

- [ ] **Step 6: Clear the name after a setup link**

In `handleAccountSetupSubmit` (`users.go`), after `CompleteSetupLink` succeeds and before `s.login`:

```go
	if s.signIn != nil {
		s.signIn.clear(done.Username)
	}
```

- [ ] **Step 7: Turn the setting on in serve**

In `serve` (`main.go`), before `Handler` is called:

```go
	srv.SetSignInBackoff(cfg.SignInBackoff)
```

- [ ] **Step 8: Run the tests to verify they pass**

Run: `go test ./internal/server -run 'LoginRefusesALocked|LoginLockShows|UnknownUsernameLocks|BackoffOff|BusyHashSlot|AccountPasswordCheck|StoreFailureCounts|PasswordAccountWithout|GateSignInIsLocked|SetupLinkClears'`

Expected: PASS.

- [ ] **Step 9: Update the kept decision**

In `AGENTS.md`, "Login", replace the whole entry with:

```md
- **Login.** There is no per-IP backoff: behind Traefik every request shares
  one remote address. A per-username backoff exists only with
  `OUTHAUL_SIGNIN_BACKOFF=on`. It is in memory, and a lock ends by itself
  within 15 minutes. Someone who keeps guessing at a name can hold that name
  locked, so the way back in is a new account: `outhaul admin create`, run on
  the host. The two-at-a-time limit on hashing always applies.
```

- [ ] **Step 10: Run the whole suite and commit**

Run: `go build ./... && go vet ./... && go test ./...`

Run: `go test -race -timeout 30m ./...`

```bash
git add internal/server/server.go internal/server/gate_signin.go internal/server/auth.go \
  internal/server/users.go internal/server/sign_in_backoff_test.go main.go AGENTS.md
git commit -m "feat(server): lock a username after repeated failed password sign-ins" -m "OUTHAUL_SIGNIN_BACKOFF=on now refuses the next attempt for 30 seconds to 15 minutes, on /login and a protected route alike, with a Retry-After. The right password works once the wait ends, and a completed setup link clears the name."
```

---

## Manual check on a real host (not CI)

The spec's item 7 in "What only the real host can show" needs a running install and a terminal; no test in this repository can show it.

1. With `OUTHAUL_SIGNIN_BACKOFF=on`, send five wrong passwords for one name. The sixth attempt, with the right password, is refused with a wait and a `Retry-After`; after the wait the right password works.
2. While that name is locked, run `sudo -u outhaul outhaul admin create NAME` on the host. The new account signs in at once.
3. Run the command as root or another user: it refuses and prints the `sudo -u outhaul outhaul admin create NAME` line.
4. At the terminal, confirm the two password prompts show no characters, and a mismatch is refused.
