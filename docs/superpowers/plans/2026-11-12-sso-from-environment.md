# SSO from the Environment, and Roles from Groups — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Connect SSO from `OUTHAUL_*` variables, with the client secret read from a root-owned file, and let an opt-in mapping give each SSO account its role from the provider's groups at every sign-in.

**Architecture:** `config` parses and validates the six new variables through the existing `parser`, and `serve` reads the secret file once. `Server` gains one `SSOEnvironment` value installed before `Handler`; `ssoSettings` returns it instead of the stored rows while it is set. `sso` reads the named groups claim from the verified ID token; a pure `core.RoleForGroups` picks the highest role, and `store.FindOrCreateSSOUser` applies it in the account's own transaction. A plain `settings` record of the issuer and client ID last run with lets startup end SSO sessions exactly as saving on Settings does.

**Tech Stack:** Go 1.26.4, `net/http`, `github.com/coreos/go-oidc/v3`, SQLite (`modernc.org/sqlite`), `html/template`.

**Spec:** `docs/superpowers/specs/2026-11-11-client-ips-streams-sign-in-design.md` — §R16 (lines 541–703), its settings table (lines 95–142), tests (lines 760–769), docs (lines 782–795), and "Left to the builder" (lines 843–858). R12, R14 and R15 are separate series and are not part of this plan.

## Global Constraints

- Go 1.26.4 (`go.mod`). Use the repo's conventions: `go vet ./...`, `go test ./...`, and `go test -race -timeout 30m ./...` before calling the change done.
- Every new `OUTHAUL_*` setting goes through the `parser` in `internal/config/config.go`; `Config.Validate` (run first by `outhaul serve`) names malformed values and refuses startup.
- The client secret comes from a file or a systemd credential. It must never appear in the environment, a command line, a log line, a page, or the database when it was given as a file. Errors name the variable and the path, never the content.
- Group names are never logged.
- Migrations are append-only: never edit `0001`–`0030`; add `0031_role_from_groups.sql`. If another series lands a migration first, take the next free number and update this plan's references.
- R16 adds no sealed column, so `sealedColumns` in `internal/store/sealed.go` does not change. The stored `sso_client_secret` keeps counting in `HasSealedValues`.
- SSO accounts are keyed by issuer and subject and are never linked by email; an SSO account has no password and can't use the password form. The last password system admin can't be removed or demoted.
- A password account's role is never changed by anything a provider sends.
- Roles are guardrails for trusted people, not containment.
- Commits are `feat(scope): summary` or `fix(scope): summary`; the body says what a user saw go wrong and what changed.
- Test first: write the failing test, watch it fail for the right reason, then implement.

## Review Focus

- **A groups claim that isn't an array of strings** (a number, an object, an array holding a number, `null`, absent): it must read as *no groups*, never refuse an otherwise valid sign-in, and never panic. Pinned by Task 4's `TestExchangeReadsTheGroupsClaim`.
- **A mapping entry with a space, `#`, or a colon in the group's name** (`role:eng: platform`, `project_member:outhaul engine#1`): the group keeps those characters, the comma is the only separator, and `firstField` is never used. Pinned by Task 1's `TestLoadSSORoleGroups`.
- **A secret file that is empty, holds only a newline, is a directory, is missing, or is larger than 4 KiB**: startup refuses and names the variable, and the content never appears in the error. Pinned by Task 2's `TestReadSSOSecret`.
- **Saving or removing SSO on Settings while the environment sets it** (a stale tab, a direct POST): both answer 409 naming the three variables, and the stored settings and sessions stay as they were. Pinned by Task 5's `TestSaveAndDeleteRefusedWhenTheEnvironmentSetsSSO`.
- **Restarting with a different issuer or client ID from either source**: every SSO sign-in and SSO route session ends, password sessions survive, and a second restart with the same values ends nothing. Pinned by Task 3's `TestSyncSSOStartupEndsSessionsWhenTheIssuerChanges` and `TestSetAndClearSSOSettingsMaintainTheRunRecord`.

---

### Task 1: The settings, parsed and validated

**Files:**
- Modify: `internal/core/role.go` (after `AssignableRoles`, line 15)
- Modify: `internal/sso/sso.go:142-165` (`validate`)
- Modify: `internal/config/config.go` (fields `:30-69`, `Load` `:78-120`, `Validate` `:198-264`, parser methods after `ip` `:637-644`)
- Test: `internal/sso/sso_test.go`, `internal/config/config_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces:
  - `core.GroupRole struct { Group string; Role Role }`
  - `sso.ValidIssuer(issuer string) error` — the issuer rule the Settings form already uses.
  - `config.Config` fields `SSOIssuer`, `SSOClientID`, `SSOClientSecretFile string`, `SSORoleGroups []core.GroupRole`, `SSOGroupsClaim string`, `SSOScopes []string`.

- [ ] **Step 1: Write the failing tests for the exported issuer rule**

In `internal/sso/sso_test.go`:

```go
func TestValidIssuer(t *testing.T) {
	for _, in := range []string{
		"https://idp.example.com",
		"https://idp.example.com/oauth2/default",
		"http://localhost:8080",
		"http://127.0.0.1:9000",
	} {
		if err := ValidIssuer(in); err != nil {
			t.Errorf("ValidIssuer(%q) = %v, want nil", in, err)
		}
	}
	for _, in := range []string{"", " ", "idp.example.com", "https://", "http://idp.example.com", "http://10.0.0.1", "ftp://idp.example.com"} {
		if err := ValidIssuer(in); err == nil {
			t.Errorf("ValidIssuer(%q) = nil, want an error", in)
		}
	}
}
```

- [ ] **Step 2: Run it and watch it fail**

Run: `go test ./internal/sso -run TestValidIssuer -v`
Expected: FAIL, `undefined: ValidIssuer`.

- [ ] **Step 3: Export the rule**

In `internal/sso/sso.go`, move the URL half of `validate` into:

```go
// ValidIssuer reports whether issuer is an absolute URL starting with https://,
// or with http:// on a loopback host. Settings and startup share it.
func ValidIssuer(issuer string) error
```

`validate` keeps its own empty-field checks and calls `ValidIssuer`, wrapping its error with `fmt.Errorf("sso: %w", err)` so `Discover`'s messages read as today. `validate`'s client-id, secret and redirect checks stay in place.

- [ ] **Step 4: Run it and watch it pass**

Run: `go test ./internal/sso -v`
Expected: PASS, including `TestDiscoverRefusesBadSettings`.

- [ ] **Step 5: Write the failing config tests**

In `internal/config/config_test.go`:

```go
func TestLoadSSOSettingsFromEnvironment(t *testing.T) {
	c := Load(func(k string) string {
		return map[string]string{
			"OUTHAUL_PUBLIC_URL":             "https://outhaul.example.com",
			"OUTHAUL_SSO_ISSUER":             "https://idp.example.com",
			"OUTHAUL_SSO_CLIENT_ID":          "cid",
			"OUTHAUL_SSO_CLIENT_SECRET_FILE": "sso-client-secret",
			"OUTHAUL_SSO_SCOPES":             "groups, offline_access",
		}[k]
	})
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if c.SSOIssuer != "https://idp.example.com" || c.SSOClientID != "cid" || c.SSOClientSecretFile != "sso-client-secret" {
		t.Errorf("provider = %q %q %q", c.SSOIssuer, c.SSOClientID, c.SSOClientSecretFile)
	}
	if len(c.SSOScopes) != 2 || c.SSOScopes[0] != "groups" || c.SSOScopes[1] != "offline_access" {
		t.Errorf("scopes = %v", c.SSOScopes)
	}
}

func TestValidateSSOFromEnvironmentIsAllOrNone(t *testing.T) {
	all := map[string]string{
		"OUTHAUL_PUBLIC_URL":             "https://outhaul.example.com",
		"OUTHAUL_SSO_ISSUER":             "https://idp.example.com",
		"OUTHAUL_SSO_CLIENT_ID":          "cid",
		"OUTHAUL_SSO_CLIENT_SECRET_FILE": "sso-client-secret",
	}
	for omitted := range map[string]bool{
		"OUTHAUL_SSO_ISSUER": true, "OUTHAUL_SSO_CLIENT_ID": true, "OUTHAUL_SSO_CLIENT_SECRET_FILE": true,
	} {
		env := map[string]string{}
		for k, v := range all {
			env[k] = v
		}
		delete(env, omitted)
		err := Load(func(k string) string { return env[k] }).Validate()
		if err == nil || !strings.Contains(err.Error(), "OUTHAUL_SSO_ISSUER, OUTHAUL_SSO_CLIENT_ID and OUTHAUL_SSO_CLIENT_SECRET_FILE") {
			t.Errorf("without %s: %v", omitted, err)
		}
	}
}

func TestValidateSSOFromEnvironmentNeedsPublicURL(t *testing.T) {
	err := Load(func(k string) string {
		return map[string]string{
			"OUTHAUL_SSO_ISSUER":             "https://idp.example.com",
			"OUTHAUL_SSO_CLIENT_ID":          "cid",
			"OUTHAUL_SSO_CLIENT_SECRET_FILE": "sso-client-secret",
		}[k]
	}).Validate()
	if err == nil || !strings.Contains(err.Error(), "OUTHAUL_PUBLIC_URL") {
		t.Fatalf("Validate = %v, want it to name OUTHAUL_PUBLIC_URL", err)
	}
}

func TestLoadSSOSecretFileValue(t *testing.T) {
	cases := []struct {
		in   string
		want string
		bad  bool
	}{
		{"/etc/outhaul/sso-client-secret", "/etc/outhaul/sso-client-secret", false},
		{"sso-client-secret", "sso-client-secret", false},
		{"secrets/sso-client-secret", "", true},
		{"../sso-client-secret", "", true},
	}
	for _, tc := range cases {
		c := Load(func(k string) string {
			if k == "OUTHAUL_SSO_CLIENT_SECRET_FILE" {
				return tc.in
			}
			return ""
		})
		if tc.bad {
			if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "OUTHAUL_SSO_CLIENT_SECRET_FILE") {
				t.Errorf("%q: Validate = %v, want it refused", tc.in, err)
			}
			continue
		}
		if c.SSOClientSecretFile != tc.want {
			t.Errorf("%q parsed as %q, want %q", tc.in, c.SSOClientSecretFile, tc.want)
		}
	}
}

func TestLoadSSORoleGroups(t *testing.T) {
	c := Load(func(k string) string {
		return map[string]string{
			"OUTHAUL_SSO_ROLE_GROUPS": " system_admin : outhaul-admins , project_admin:eng: platform , project_member:outhaul engine#1 , project_viewer:viewers ",
		}[k]
	})
	want := []core.GroupRole{
		{Group: "outhaul-admins", Role: core.RoleSystemAdmin},
		{Group: "eng: platform", Role: core.RoleProjectAdmin},
		{Group: "outhaul engine#1", Role: core.RoleProjectMember},
		{Group: "viewers", Role: core.RoleProjectViewer},
	}
	if len(c.SSORoleGroups) != len(want) {
		t.Fatalf("mapping = %v, want %v", c.SSORoleGroups, want)
	}
	for i := range want {
		if c.SSORoleGroups[i] != want[i] {
			t.Errorf("entry %d = %+v, want %+v", i, c.SSORoleGroups[i], want[i])
		}
	}
}

func TestLoadSSORoleGroupsRejects(t *testing.T) {
	for _, in := range []string{
		"wizard:eng",                       // unknown role
		"none:eng",                         // not one of AssignableRoles
		"eng",                              // no colon
		"project_member:",                  // empty group
		"project_member:eng,project_viewer:eng", // the same group twice
		"project_member:" + strings.Repeat("g", 257),
	} {
		err := Load(func(k string) string {
			if k == "OUTHAUL_SSO_ROLE_GROUPS" {
				return in
			}
			return ""
		}).Validate()
		if err == nil || !strings.Contains(err.Error(), "OUTHAUL_SSO_ROLE_GROUPS") {
			t.Errorf("%q: Validate = %v, want it refused", in, err)
		}
	}
}

func TestLoadSSOScopesRejects(t *testing.T) {
	err := Load(func(k string) string {
		if k == "OUTHAUL_SSO_SCOPES" {
			return `say"hi`
		}
		return ""
	}).Validate()
	if err == nil || !strings.Contains(err.Error(), "OUTHAUL_SSO_SCOPES") {
		t.Fatalf("Validate = %v, want it refused", err)
	}
}

func TestLoadSSOGroupsClaim(t *testing.T) {
	with := func(mapping, claim string) Config {
		return Load(func(k string) string {
			return map[string]string{
				"OUTHAUL_SSO_ROLE_GROUPS": mapping,
				"OUTHAUL_SSO_GROUPS_CLAIM": claim,
			}[k]
		})
	}
	if c := with("project_member:eng", ""); c.SSOGroupsClaim != "groups" {
		t.Errorf("claim = %q, want the groups default", c.SSOGroupsClaim)
	}
	if c := with("project_member:eng", "teams"); c.SSOGroupsClaim != "teams" || c.Validate() != nil {
		t.Errorf("claim = %q, Validate = %v", c.SSOGroupsClaim, c.Validate())
	}
	if err := with("", "teams").Validate(); err == nil || !strings.Contains(err.Error(), "OUTHAUL_SSO_GROUPS_CLAIM") {
		t.Errorf("claim without mapping: %v, want a refusal", err)
	}
}
```

Also add one case to `TestValidateRejectsMalformedValues`:

```go
{"OUTHAUL_SSO_ISSUER", "http://idp.example.com"},
```

- [ ] **Step 6: Run the tests and watch them fail**

Run: `go test ./internal/config -run 'SSO' -v`
Expected: FAIL — `undefined: core.GroupRole` and undefined `Config.SSO*` fields.

- [ ] **Step 7: Implement the type, fields and parser methods**

`internal/core/role.go`:

```go
// GroupRole maps one of the provider's groups to the role it gives. The
// mapping is read from OUTHAUL_SSO_ROLE_GROUPS; order is the order the pairs
// were written in.
type GroupRole struct {
	Group string
	Role  Role
}
```

`internal/config/config.go` gains the fields:

```go
SSOIssuer           string           // OUTHAUL_SSO_ISSUER; with the next two, connects SSO from the environment
SSOClientID         string           // OUTHAUL_SSO_CLIENT_ID
SSOClientSecretFile string           // OUTHAUL_SSO_CLIENT_SECRET_FILE: absolute path or systemd credential name
SSOGroupsClaim      string           // OUTHAUL_SSO_GROUPS_CLAIM; "groups" when the mapping is set
SSOScopes           []string         // OUTHAUL_SSO_SCOPES; extra scopes to request
SSORoleGroups       []core.GroupRole // OUTHAUL_SSO_ROLE_GROUPS; group -> role, in order
```

In `Load`, after the existing entries:

```go
SSOIssuer:           p.ssoIssuer("OUTHAUL_SSO_ISSUER"),
SSOClientID:         strings.TrimSpace(getenv("OUTHAUL_SSO_CLIENT_ID")),
SSOClientSecretFile: p.ssoSecretFile("OUTHAUL_SSO_CLIENT_SECRET_FILE"),
SSORoleGroups:       p.ssoRoleGroups("OUTHAUL_SSO_ROLE_GROUPS"),
SSOScopes:           p.scopes("OUTHAUL_SSO_SCOPES"),
```

and after the struct literal, before `c.problems = p.problems`:

```go
switch claim := strings.TrimSpace(getenv("OUTHAUL_SSO_GROUPS_CLAIM")); {
case claim != "" && len(c.SSORoleGroups) == 0:
	p.problems = append(p.problems, errors.New("OUTHAUL_SSO_GROUPS_CLAIM needs OUTHAUL_SSO_ROLE_GROUPS, the mapping whose groups it names"))
case claim != "":
	c.SSOGroupsClaim = claim
case len(c.SSORoleGroups) > 0:
	c.SSOGroupsClaim = "groups"
}
```

`Validate` gains:

```go
set := 0
for _, v := range []string{c.SSOIssuer, c.SSOClientID, c.SSOClientSecretFile} {
	if v != "" {
		set++
	}
}
if set != 0 && set != 3 {
	errs = append(errs, errors.New("OUTHAUL_SSO_ISSUER, OUTHAUL_SSO_CLIENT_ID and OUTHAUL_SSO_CLIENT_SECRET_FILE must be set together"))
}
if c.SSOIssuer != "" && !c.PublicURLSet() {
	errs = append(errs, errors.New("OUTHAUL_SSO_ISSUER needs OUTHAUL_PUBLIC_URL, where the provider sends people back"))
}
```

Parser methods (below `ip`):

- `ssoIssuer(key)`: trim; empty stays empty; `sso.ValidIssuer` failure appends `fmt.Errorf("%s=%q: %v", key, v, err)`; success returns the trimmed value.
- `ssoSecretFile(key)`: trim; empty stays empty; `filepath.IsAbs(v)` returns it; otherwise a value containing `/` is reported as `want an absolute path, or a systemd credential name with no /`; anything else returns it unchanged.
- `ssoRoleGroups(key)`: an all-whitespace value is no mapping. Otherwise `strings.Split(raw, ",")` — **not** `firstField`. For each entry: trim it; `strings.Cut(entry, ":")` (first colon); trim the role and the group; the role must be one of `core.AssignableRoles` (explicitly not `none`); the group must be non-empty and at most 256 bytes; a group seen twice is a problem. Append `core.GroupRole{Group: group, Role: role}`. Problems name the variable and the offending entry; group names are not secret.
- `scopes(key)`: `strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || unicode.IsSpace(r) })`; each scope must be 1+ bytes, all in `0x21..0x7e`, with no `"` or `\`; otherwise a problem naming the variable with no value echoed.

Add `"unicode"` and `"path/filepath"` to `config`'s imports if not already present, plus `internal/core` and `internal/sso`.

- [ ] **Step 8: Run the tests and watch them pass**

Run: `go test ./internal/config ./internal/sso ./internal/core -v`
Expected: PASS.

- [ ] **Step 9: Commit**

```bash
git add internal/core/role.go internal/sso/sso.go internal/config/config.go \
        internal/config/config_test.go internal/sso/sso_test.go
git commit -m "feat(config): read SSO settings, scopes and group roles from the environment"
```

---

### Task 2: The client secret file

**Files:**
- Modify: `main.go` (near `serve`, after `newUpdates` around line 441)
- Test: `main_test.go`

**Interfaces:**
- Consumes: `config.Config.SSOClientSecretFile` from Task 1.
- Produces: `readSSOSecret(value, credentialsDir string) (string, error)` in package `main`.

- [ ] **Step 1: Write the failing test**

In `main_test.go`:

```go
func TestReadSSOSecret(t *testing.T) {
	dir := t.TempDir()
	creds := t.TempDir()
	write := func(dir, name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	abs := write(dir, "secret", "abs-value\n")
	write(dir, "lf-only", "\n")
	write(dir, "empty", "")
	write(dir, "big", strings.Repeat("x", 4097))
	write(dir, "crlf", "crlf-value\r\n")
	write(creds, "sso-client-secret", "cred-value\n")

	got, err := readSSOSecret(abs, creds)
	if err != nil || got != "abs-value" {
		t.Fatalf("absolute path = %q, %v; want abs-value", got, err)
	}
	got, err = readSSOSecret("sso-client-secret", creds)
	if err != nil || got != "cred-value" {
		t.Fatalf("credential = %q, %v; want cred-value", got, err)
	}
	if got, err := readSSOSecret(filepath.Join(dir, "crlf"), ""); err != nil || got != "crlf-value" {
		t.Fatalf("crlf = %q, %v", got, err)
	}
	if got, err := readSSOSecret("", creds); err != nil || got != "" {
		t.Fatalf("unset = %q, %v; want empty", got, err)
	}
	for _, c := range []struct{ value, credDir string }{
		{filepath.Join(dir, "missing"), ""},
		{filepath.Join(dir, "lf-only"), ""},
		{filepath.Join(dir, "empty"), ""},
		{filepath.Join(dir, "big"), ""},
		{"sso-client-secret", ""}, // a credential name with no CREDENTIALS_DIRECTORY
	} {
		_, err := readSSOSecret(c.value, c.credDir)
		if err == nil || !strings.Contains(err.Error(), "OUTHAUL_SSO_CLIENT_SECRET_FILE") {
			t.Errorf("%q: err = %v, want it to name the variable", c.value, err)
		}
	}
	// The content never appears in an error, even for a refused file.
	if _, err := readSSOSecret(filepath.Join(dir, "big"), ""); err != nil && strings.Contains(err.Error(), "xxxx") {
		t.Errorf("error repeats the file's content: %v", err)
	}
}
```

- [ ] **Step 2: Run it and watch it fail**

Run: `go test . -run TestReadSSOSecret -v`
Expected: FAIL, `undefined: readSSOSecret`.

- [ ] **Step 3: Implement it**

In `main.go`:

```go
// maxSSOSecretBytes bounds the client secret file, so a wrong path can't read
// a large file into memory.
const maxSSOSecretBytes = 4096

// readSSOSecret reads OUTHAUL_SSO_CLIENT_SECRET_FILE. An absolute path is read
// as it is; any other value is a systemd credential name under
// credentialsDir (CREDENTIALS_DIRECTORY). One trailing newline is dropped. An
// error names the variable and the path, never the content.
func readSSOSecret(value, credentialsDir string) (string, error)
```

Rules: empty `value` → `"", nil`. A non-absolute value that is `.` or `..` is refused, so a credential name can't walk out of `CREDENTIALS_DIRECTORY`. A non-absolute value with `credentialsDir == ""` → error saying it is a systemd credential and `CREDENTIALS_DIRECTORY` is not set. Read with `os.ReadFile`; errors wrap with the variable's name. More than 4096 bytes → refused. Trim one `"\n"`, then one `"\r"`; empty after that → refused.

Call it in `serve` straight after `cfg.Validate()`:

```go
if _, err := readSSOSecret(cfg.SSOClientSecretFile, os.Getenv("CREDENTIALS_DIRECTORY")); err != nil {
	return err
}
```

Task 5 replaces this with `ssoClientSecret, err := readSSOSecret(...)` and keeps the value for `SetSSOEnvironment`, so this task leaves the build green on its own.

- [ ] **Step 4: Run it and watch it pass**

Run: `go test . -run TestReadSSOSecret -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add main.go main_test.go
git commit -m "feat(sso): read the client secret from a root-owned file"
```

---

### Task 3: Remember the run's issuer and client ID, and end sessions when they change

**Files:**
- Modify: `internal/store/sso.go`
- Test: `internal/store/sso_test.go`

**Interfaces:**
- Consumes: `store.SSOSettings` (existing).
- Produces:
  - `store.SyncSSOStartup(ctx context.Context, v SSOSettings, set bool) (ended bool, err error)`
  - Settings keys `sso_last_issuer`, `sso_last_client_id`, written by `SetSSOSettings` and removed by `ClearSSOSettings`.

- [ ] **Step 1: Write the failing tests**

In `internal/store/sso_test.go`, add:

```go
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
	if err := s.SetSSOSettings(ctx, SSOSettings{Issuer: iss, ClientID: "cid", ClientSecret: "sec"}, false); err != nil {
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
```

- [ ] **Step 2: Run them and watch them fail**

Run: `go test ./internal/store -run 'SyncSSOStartup|RunRecord' -v`
Expected: FAIL, `undefined: (*Store).SyncSSOStartup`.

- [ ] **Step 3: Implement**

In `internal/store/sso.go`, add the keys beside the existing ones:

```go
ssoLastIssuerKey   = "sso_last_issuer"
ssoLastClientIDKey = "sso_last_client_id"
```

`SyncSSOStartup`:

```go
// SyncSSOStartup records the issuer and client ID this run uses and, when they
// differ from the last run's, ends every SSO sign-in in the same transaction.
// With no record yet, the stored settings are the baseline. set is true when
// the environment gave the provider; when it is false the stored settings are
// the ones in force, if any. With neither, the record is removed.
func (s *Store) SyncSSOStartup(ctx context.Context, v SSOSettings, set bool) (bool, error)
```

Algorithm:

1. Read `sso_last_issuer`/`sso_last_client_id` with `GetSetting`. `hasRecord` is whether the issuer row exists.
2. Read the stored issuer and client ID rows directly (not `GetSSOSettings`, so the secret is not decrypted). `hasStored` is whether the issuer row exists.
3. The settings in force: `v` when `set`; when `set` is false, the stored pair if `hasStored`. `inForce` is whether either gave one.
4. The baseline: the record if `hasRecord`, else the stored pair if `hasStored`.
5. Open one transaction. If `inForce`, upsert both record rows — always, so the record names the run even when nothing changed; else delete both. If the baseline differs from the settings in force, call `endSSOSessions(ctx, tx)`. Commit and return whether sessions were ended.

The false-`set` fallback is what keeps a Settings-connected install signed in: `serve` passes `set=false` when the environment gives no provider, but the stored settings are still in force and must not count as "removed".

`SetSSOSettings` upserts the two record rows in its transaction, after the three provider rows. `ClearSSOSettings` deletes them in its transaction, with the provider rows.

- [ ] **Step 4: Run them and watch them pass**

Run: `go test ./internal/store -run 'SSO' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/store/sso.go internal/store/sso_test.go
git commit -m "feat(store): record the SSO settings each run and end sessions when they change"
```

---

### Task 4: Read the groups claim, and request extra scopes

**Files:**
- Modify: `internal/sso/sso.go` (`Config`, `Identity`, `Discover`, `Exchange`, `provider`)
- Modify: `internal/sso/fake.go` only if a comment needs it (its `Identity` field already carries groups)
- Test: `internal/sso/sso_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces:
  - `sso.Config.GroupsClaim string`, `sso.Config.Scopes []string`
  - `sso.Identity.Groups []string`
  - `groupsFrom(raw json.RawMessage) []string` (unexported)

- [ ] **Step 1: Write the failing tests**

In `internal/sso/sso_test.go`:

```go
func exchangeWith(t *testing.T, p *idp, cfg Config) (Identity, error) {
	t.Helper()
	prov, err := Discover(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	return prov.Exchange(context.Background(), "code-1", testNonce, testVerif)
}

func TestExchangeReadsTheGroupsClaim(t *testing.T) {
	cases := []struct {
		name      string
		claimName string // the claim Config tells Exchange to read
		groups    any    // the token's "groups"; nil leaves it out
		teams     any    // the token's "teams"; nil leaves it out
		want      []string
	}{
		{"array", "groups", []string{"eng", "admins"}, nil, []string{"eng", "admins"}},
		{"one string", "groups", "eng", nil, []string{"eng"}},
		{"number", "groups", 7, nil, nil},
		{"object", "groups", map[string]any{"a": "b"}, nil, nil},
		{"mixed array", "groups", []any{"eng", 2}, nil, nil},
		{"absent", "groups", nil, nil, nil},
		{"another claim", "teams", []string{"eng"}, []string{"eng"}, []string{"eng"}},
		{"configured claim absent", "teams", []string{"eng"}, nil, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := newIDP(t)
			if c.groups != nil {
				p.claims["groups"] = c.groups
			}
			if c.teams != nil {
				p.claims["teams"] = c.teams
			}
			cfg := p.cfg()
			cfg.GroupsClaim = c.claimName
			got, err := exchangeWith(t, p, cfg)
			if err != nil {
				t.Fatal(err)
			}
			if len(got.Groups) != len(c.want) {
				t.Fatalf("groups = %v, want %v", got.Groups, c.want)
			}
			for i := range c.want {
				if got.Groups[i] != c.want[i] {
					t.Errorf("groups = %v, want %v", got.Groups, c.want)
				}
			}
		})
	}
}

func TestExchangeReadsNoGroupsWithoutAClaimName(t *testing.T) {
	p := newIDP(t)
	p.claims["groups"] = []string{"eng"}
	got, err := exchange(t, p) // p.cfg() has no GroupsClaim
	if err != nil || got.Groups != nil {
		t.Fatalf("got %+v, %v; want no groups", got.Groups, err)
	}
}

func TestDiscoverRequestsExtraScopes(t *testing.T) {
	p := newIDP(t)
	cfg := p.cfg()
	cfg.Scopes = []string{"groups", "offline_access"}
	prov, err := Discover(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(prov.AuthURL("s", "n", "v"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Fields(u.Query().Get("scope")); strings.Join(got, " ") != "openid email profile groups offline_access" {
		t.Fatalf("scope = %q", u.Query().Get("scope"))
	}
	// Without extras the request is unchanged.
	u, _ = url.Parse(discover(t, p).AuthURL("s", "n", "v"))
	if got := strings.Fields(u.Query().Get("scope")); strings.Join(got, " ") != "openid email profile" {
		t.Fatalf("default scope = %q", u.Query().Get("scope"))
	}
}
```

- [ ] **Step 2: Run them and watch them fail**

Run: `go test ./internal/sso -run 'GroupsClaim|ExtraScopes|WithoutAClaimName' -v`
Expected: FAIL, undefined fields `GroupsClaim`, `Scopes`, `Groups`.

- [ ] **Step 3: Implement**

`Config` gains `GroupsClaim string` and `Scopes []string`; `Identity` gains `Groups []string`. `provider` gains `groupsClaim string`.

`Discover`:

```go
Scopes: append([]string{oidc.ScopeOpenID, "email", "profile"}, cfg.Scopes...),
```

and stores `cfg.GroupsClaim` on the provider.

`Exchange`, after the existing email/label claims decode and only when `p.groupsClaim != ""`:

```go
var raw map[string]json.RawMessage
if err := idt.Claims(&raw); err != nil {
	return Identity{}, fmt.Errorf("sso: reading id_token claims: %w", err)
}
groups = groupsFrom(raw[p.groupsClaim])
```

`groupsFrom` accepts an array of strings or one string; a missing, `null`, mixed, or other-typed value is `nil`. Do not trim or fold case: groups are compared exactly, later.

Return `Identity{..., Groups: groups}`.

- [ ] **Step 4: Run them and watch them pass**

Run: `go test ./internal/sso -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/sso/sso.go internal/sso/sso_test.go
git commit -m "feat(sso): read the provider's groups claim and request extra scopes"
```

---

### Task 5: Use the environment's settings, and check the provider at startup

**Files:**
- Modify: `internal/server/server.go` (struct beside the other `sso*` fields, `:140-150`)
- Modify: `internal/server/sso_settings.go`
- Modify: `internal/server/sso_login.go:207-219` (`ssoConnected`)
- Modify: `internal/server/templates/settings.tmpl:221-256`
- Modify: `main.go` (wiring around `:352-406`)
- Test: `internal/server/sso_settings_test.go`, `internal/server/sso_login_test.go`

**Interfaces:**
- Consumes: `config.Config.SSO*` (Task 1), `readSSOSecret` (Task 2), `store.SyncSSOStartup` (Task 3), `sso.Config.GroupsClaim`/`Scopes` (Task 4).
- Produces:
  - `server.SSOEnvironment` with `FromEnvironment bool`, `Issuer`, `ClientID`, `ClientSecret`, `GroupsClaim string`, `Scopes []string`, `RoleGroups []core.GroupRole`.
  - `(*Server).SetSSOEnvironment(v SSOEnvironment)` — call before `Handler`.
  - `(*Server).ssoSettings(ctx) (store.SSOSettings, bool, error)`, `(*Server).ssoConfig(v store.SSOSettings) sso.Config`, `(*Server).ProbeSSO(ctx)`.
  - Panel keys `FromEnvironment`, `StoredAlso` (mapping keys come in Task 7).

- [ ] **Step 1: Write the failing tests**

In `internal/server/sso_settings_test.go`:

```go
func TestSSOFromTheEnvironmentWins(t *testing.T) {
	e, seen := ssoEnv(t)
	saveSSO(t, e, "https://stored.example.com", "stored", "sec").Body.Close()
	e.srv.ssoEnv = SSOEnvironment{
		FromEnvironment: true,
		Issuer:          "https://env.example.com",
		ClientID:        "env-id",
		ClientSecret:    "env-secret",
	}

	page := body(t, e.get(t, "/settings"))
	for _, want := range []string{
		"https://env.example.com",
		"env-id",
		"The client secret was read from a file when Outhaul started.",
		"kept, but not used",
		"https://slip.example.com/auth/sso/callback",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the settings page lacks %q", want)
		}
	}
	if strings.Contains(page, `action="/settings/sso"`) {
		t.Error("the environment's settings still get an editable form")
	}

	*seen = nil // the save above ran its own discovery
	p, err := e.srv.ssoProvider(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if p.issuer != "https://env.example.com" || p.clientID != "env-id" || len(*seen) != 1 ||
		(*seen)[0].Issuer != "https://env.example.com" || (*seen)[0].ClientSecret != "env-secret" {
		t.Fatalf("discovery saw %+v, want the environment's settings", *seen)
	}
}

func TestSaveAndDeleteRefusedWhenTheEnvironmentSetsSSO(t *testing.T) {
	e, _ := ssoEnv(t)
	saveSSO(t, e, "https://stored.example.com", "stored", "sec").Body.Close()
	e.srv.ssoEnv = SSOEnvironment{FromEnvironment: true, Issuer: "https://env.example.com", ClientID: "env-id", ClientSecret: "env-secret"}

	resp := saveSSO(t, e, "https://other.example.com", "other", "sec")
	body := body(t, resp)
	if resp.StatusCode != http.StatusConflict || !strings.Contains(body, "OUTHAUL_SSO_ISSUER") {
		t.Errorf("save = %d %s, want 409 naming the variables", resp.StatusCode, body)
	}
	resp = e.postForm(t, "/settings/sso/delete", url.Values{})
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("delete = %d, want 409", resp.StatusCode)
	}
	resp.Body.Close()
	if got, ok, _ := e.store.GetSSOSettings(context.Background()); !ok || got.Issuer != "https://stored.example.com" {
		t.Errorf("the stored settings changed: %+v, %v", got, ok)
	}
}

func TestSignInWorksWithTheEnvironmentAlone(t *testing.T) {
	e := newTestEnv(t)
	e.completeSetup(t)
	e.srv.publicURL = e.http.URL
	e.srv.ssoEnv = SSOEnvironment{FromEnvironment: true, Issuer: testIssuer, ClientID: "cid", ClientSecret: "sec"}
	fake := fakeIdentity("sub-1", "dana@example.com")
	e.srv.discover = func(context.Context, sso.Config) (sso.Provider, error) { return fake, nil }

	b := newBrowser(e)
	b.signIn(t)
	accounts := ssoAccounts(t, e)
	if len(accounts) != 1 || accounts[0].Email != "dana@example.com" {
		t.Fatalf("accounts = %+v", accounts)
	}
}

func TestProbeSSOUsesTheEnvironmentSettings(t *testing.T) {
	e, seen := ssoEnv(t)
	e.srv.ssoEnv = SSOEnvironment{FromEnvironment: true, Issuer: testIssuer, ClientID: "cid", ClientSecret: "sec",
		GroupsClaim: "groups", Scopes: []string{"groups"}}
	e.srv.ProbeSSO(context.Background())
	if len(*seen) != 1 || (*seen)[0].GroupsClaim != "groups" || len((*seen)[0].Scopes) != 1 {
		t.Fatalf("discovery saw %+v", *seen)
	}
}
```

Add `core` to that file's imports for the `SSOEnvironment` literal only if a role is included (Task 7 does that).

- [ ] **Step 2: Run them and watch them fail**

Run: `go test ./internal/server -run 'FromTheEnvironment|EnvironmentSetsSSO|EnvironmentAlone|ProbeSSO' -v`
Expected: FAIL, `undefined: SSOEnvironment`.

- [ ] **Step 3: Implement the server side**

In `server.go`, add beside the other `sso*` fields:

```go
ssoEnv SSOEnvironment // set once before Handler; the zero value means "not from the environment"
```

In `sso_settings.go`, the type, setter and accessors:

```go
// SSOEnvironment is the SSO configuration the process environment gave at
// startup. FromEnvironment says the issuer, client ID and secret were set and
// win over the stored settings. The groups claim, scopes and role mapping apply
// to either source.
type SSOEnvironment struct {
	FromEnvironment                bool
	Issuer, ClientID, ClientSecret string
	GroupsClaim                    string
	Scopes                         []string
	RoleGroups                     []core.GroupRole
}

// SetSSOEnvironment installs the environment's SSO configuration. Call it
// before Handler.
func (s *Server) SetSSOEnvironment(v SSOEnvironment) { s.ssoEnv = v }

// ssoSettings is the provider in force: the environment's while it is set,
// otherwise the stored one.
func (s *Server) ssoSettings(ctx context.Context) (store.SSOSettings, bool, error) {
	if s.ssoEnv.FromEnvironment {
		return store.SSOSettings{Issuer: s.ssoEnv.Issuer, ClientID: s.ssoEnv.ClientID, ClientSecret: s.ssoEnv.ClientSecret}, true, nil
	}
	return s.store.GetSSOSettings(ctx)
}

// ssoConfig is the discovery and exchange config for settings, carrying the
// environment's claim and scopes.
func (s *Server) ssoConfig(v store.SSOSettings) sso.Config {
	return sso.Config{
		Issuer: v.Issuer, ClientID: v.ClientID, ClientSecret: v.ClientSecret,
		RedirectURL: s.ssoRedirectURI(),
		GroupsClaim: s.ssoEnv.GroupsClaim, Scopes: s.ssoEnv.Scopes,
	}
}
```

Change the five `store.GetSSOSettings` callers the spec names to `s.ssoSettings` (`ssoProvider`, `ssoStillCurrent`, `ssoPanel`, `handleSaveSSO`, and `ssoConnected` in `sso_login.go`). `ssoProvider` and `handleSaveSSO` build their `sso.Config` with `s.ssoConfig(cfg)`.

`handleSaveSSO` and `handleDeleteSSO` start with:

```go
if s.ssoEnv.FromEnvironment {
	http.Error(w, "Single sign-on is set from the environment (OUTHAUL_SSO_ISSUER, OUTHAUL_SSO_CLIENT_ID, OUTHAUL_SSO_CLIENT_SECRET_FILE). Change it there and restart Outhaul.", http.StatusConflict)
	return
}
```

`ssoPanel` returns the environment's values as text when set: `FromEnvironment: true`, `Issuer`, `ClientID`, `HasSecret: true`, `Configured: true`, and `StoredAlso: true` when `s.store.GetSSOSettings` reports stored settings. It still returns `RedirectURI`.

`ProbeSSO`:

```go
// ProbeSSO runs discovery once at startup, so the log says at once whether the
// provider answered. It never stops Outhaul: a failure is logged, and sign-in
// reports it again.
func (s *Server) ProbeSSO(ctx context.Context) {
	if _, err := s.ssoProvider(ctx); err != nil {
		if errors.Is(err, errSSONotConfigured) {
			return
		}
		log.Printf("sso: the provider did not answer at startup: %v", err)
		return
	}
	log.Printf("sso: the provider answered at startup")
}
```

- [ ] **Step 4: Update the template**

In `settings.tmpl`'s SSO section, wrap the existing form, confirmation and Remove button in `{{if .FromEnvironment}} … {{else}} … {{end}}`. The environment branch shows the issuer, client ID and redirect URI in a small table or definition list, then:

```html
<p class="muted env-note">The client secret was read from a file when Outhaul started.</p>
{{if .StoredAlso}}<p class="muted env-note">Settings saved here are kept, but not used while the environment sets them.</p>{{end}}
```

Keep the existing warning that everyone the provider lets in reaches every protected route.

- [ ] **Step 5: Wire it in `main.go`**

Replace Task 2's `if _, err := readSSOSecret(...)` with `ssoClientSecret, err := readSSOSecret(...)`, keeping the same error return, so the value can go to `SetSSOEnvironment`.

After the store is opened (`defer st.Close()`), before `reserveAdminHost`:

```go
ssoSet := cfg.SSOIssuer != ""
ended, err := st.SyncSSOStartup(context.Background(), store.SSOSettings{Issuer: cfg.SSOIssuer, ClientID: cfg.SSOClientID}, ssoSet)
if err != nil {
	return fmt.Errorf("record the SSO settings this run uses: %w", err)
}
if ended {
	log.Printf("sso: the issuer or client ID changed since the last run, so every SSO sign-in has ended; if the issuer changed, everyone returns as a new account")
}
if ssoSet {
	log.Printf("sso: using the provider from the environment: %s", cfg.SSOIssuer)
}
```

`set` here means "the environment gave the provider". The store falls back to its stored settings when it is false, so a Settings-connected install keeps its sessions across the restart; do not compute the effective settings again in `main`.

After `server.New` and the other setters, before `printSetupHint`:

```go
srv.SetSSOEnvironment(server.SSOEnvironment{
	FromEnvironment: ssoSet,
	Issuer:          cfg.SSOIssuer,
	ClientID:        cfg.SSOClientID,
	ClientSecret:    ssoClientSecret,
	GroupsClaim:     cfg.SSOGroupsClaim,
	Scopes:          cfg.SSOScopes,
	RoleGroups:      cfg.SSORoleGroups,
})
```

and after `printSetupHint(srv, cfg, setupToken)`:

```go
go srv.ProbeSSO(workerCtx)
```

- [ ] **Step 6: Run the tests and watch them pass**

Run: `go test ./internal/server -run 'SSO' -v`
Expected: PASS. Then `go build ./...`.

- [ ] **Step 7: Commit**

```bash
git add internal/server/server.go internal/server/sso_settings.go internal/server/sso_login.go \
        internal/server/templates/settings.tmpl internal/server/sso_settings_test.go main.go
git commit -m "feat(server): use SSO settings from the environment"
```

---

### Task 6: Give an SSO account its role from the groups

**Files:**
- Modify: `internal/core/role.go` (`RoleForGroups`)
- Modify: `internal/core/user.go` (`User.RoleFromGroups`)
- Create: `internal/store/migrations/0031_role_from_groups.sql`
- Modify: `internal/store/users.go` (`userCols`, `scanUser`, `FindOrCreateSSOUser`, `SetUserRole`)
- Modify: `internal/server/sso_login.go:428-437` (apply the role in `handleSSOCallback`)
- Test: `internal/core/role_test.go`, `internal/store/users_test.go`, `internal/store/gate_test.go`, `internal/store/migrate_role_from_groups_test.go`, `internal/server/sso_login_test.go`, `internal/server/gate_signin_test.go`

**Interfaces:**
- Consumes: `core.GroupRole`, `sso.Identity.Groups`, `SSOEnvironment.RoleGroups`, `ssoSettings`.
- Produces:
  - `core.RoleForGroups(groups []string, mapping []GroupRole) Role`
  - `core.User.RoleFromGroups bool`
  - `store.FindOrCreateSSOUser(ctx, issuer, subject, email string, groupRole core.Role, groupsMapped bool) (core.User, core.Role, error)` — the second value is the role before this sign-in.

- [ ] **Step 1: Write the failing core test**

In `internal/core/role_test.go`:

```go
func TestRoleForGroups(t *testing.T) {
	mapping := []core.GroupRole{
		{Group: "eng", Role: core.RoleProjectMember},
		{Group: "admins", Role: core.RoleSystemAdmin},
		{Group: "all", Role: core.RoleProjectViewer},
	}
	cases := []struct {
		groups []string
		want   core.Role
	}{
		{nil, core.RoleNone},
		{[]string{}, core.RoleNone},
		{[]string{"eng"}, core.RoleProjectMember},
		{[]string{"eng", "admins"}, core.RoleSystemAdmin},
		{[]string{"admins", "all"}, core.RoleSystemAdmin},
		{[]string{"ENG"}, core.RoleNone}, // exact comparison
		{[]string{"nobody"}, core.RoleNone},
	}
	for _, c := range cases {
		if got := core.RoleForGroups(c.groups, mapping); got != c.want {
			t.Errorf("RoleForGroups(%v) = %q, want %q", c.groups, got, c.want)
		}
	}
	if got := core.RoleForGroups([]string{"eng"}, nil); got != core.RoleNone {
		t.Errorf("no mapping = %q, want none", got)
	}
}
```

- [ ] **Step 2: Run it and watch it fail**

Run: `go test ./internal/core -run TestRoleForGroups -v`
Expected: FAIL, `undefined: core.RoleForGroups`.

- [ ] **Step 3: Implement it**

```go
// RoleForGroups returns the highest role any of groups maps to, or RoleNone
// when none does. Names are compared exactly. Several entries may give the same
// role; one group appears at most once, which Config.Validate enforces.
func RoleForGroups(groups []string, mapping []GroupRole) Role
```

For each entry in `mapping`, in order, if `groups` holds the entry's group exactly and the entry's role ranks above the best so far, keep it. Return the best (ranked with the existing `rank`).

- [ ] **Step 4: Run it and watch it pass**

Run: `go test ./internal/core -v`
Expected: PASS.

- [ ] **Step 5: Write the failing migration test**

New file `internal/store/migrate_role_from_groups_test.go`, following `migrate_webhook_token_test.go`:

```go
// Accounts that existed before the column reads as "not from a group".
func TestRoleFromGroupsDefaultsOffForExistingAccounts(t *testing.T) {
	db := openMigratedBefore(t, "0031")
	if _, err := db.Exec(`INSERT INTO users (username, password_hash, created_at, role)
	                      VALUES ('old', '', '2026-11-01T00:00:00Z', 'system_admin')`); err != nil {
		t.Fatal(err)
	}
	if err := migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	var fromGroups int
	if err := db.QueryRow(`SELECT role_from_groups FROM users WHERE username = 'old'`).Scan(&fromGroups); err != nil {
		t.Fatal(err)
	}
	if fromGroups != 0 {
		t.Errorf("role_from_groups = %d, want 0", fromGroups)
	}
}
```

- [ ] **Step 6: Run it and watch it fail**

Run: `go test ./internal/store -run TestRoleFromGroupsDefaultsOff -v`
Expected: FAIL, `no such column: role_from_groups`.

- [ ] **Step 7: Add the migration and the data plumbing**

`internal/store/migrations/0031_role_from_groups.sql`:

```sql
ALTER TABLE users ADD COLUMN role_from_groups INTEGER NOT NULL DEFAULT 0;
```

`core/user.go`: add to `User`

```go
RoleFromGroups bool // Role came from the provider's groups at the last sign-in
```

`store/users.go`: append `role_from_groups` to `userCols` and scan it into `&u.RoleFromGroups`. `database/sql` converts SQLite's 0/1 into a bool.

- [ ] **Step 8: Write the failing store tests**

In `internal/store/users_test.go`, and update every existing call site to the new signature (`core.RoleNone, false`, and `_,` for the new second return). The call sites are all in `users_test.go` plus `gate_test.go:475`. Add:

```go
func TestSSOSignInAppliesAndRemovesAGroupRole(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	u, prev, err := s.FindOrCreateSSOUser(ctx, testIssuer, "sub-1", "dana@example.com", core.RoleProjectMember, true)
	if err != nil {
		t.Fatal(err)
	}
	if u.Role != core.RoleProjectMember || !u.RoleFromGroups || prev != core.RoleNone {
		t.Fatalf("first sign-in = %+v, previous %q", u, prev)
	}
	again, prev, err := s.FindOrCreateSSOUser(ctx, testIssuer, "sub-1", "dana@example.com", core.RoleProjectMember, true)
	if err != nil || again.Role != core.RoleProjectMember || prev != core.RoleProjectMember {
		t.Fatalf("second sign-in = %+v, previous %q, %v", again, prev, err)
	}
	// The provider stops sending the group: a group-given role goes.
	none, prev, err := s.FindOrCreateSSOUser(ctx, testIssuer, "sub-1", "dana@example.com", core.RoleNone, true)
	if err != nil || none.Role != core.RoleNone || none.RoleFromGroups || prev != core.RoleProjectMember {
		t.Fatalf("after leaving the group = %+v, previous %q, %v", none, prev, err)
	}
}

func TestSSOSignInKeepsAHandGivenRoleWhenNoGroupMaps(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	u, _, err := s.FindOrCreateSSOUser(ctx, testIssuer, "sub-1", "dana@example.com", core.RoleNone, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetUserRole(ctx, u.ID, core.RoleProjectAdmin); err != nil {
		t.Fatal(err)
	}
	again, prev, err := s.FindOrCreateSSOUser(ctx, testIssuer, "sub-1", "dana@example.com", core.RoleNone, true)
	if err != nil || again.Role != core.RoleProjectAdmin || prev != core.RoleProjectAdmin || again.RoleFromGroups {
		t.Fatalf("hand-given role = %+v, previous %q, %v", again, prev, err)
	}
}

func TestSetUserRoleClearsTheGroupMark(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	u, _, err := s.FindOrCreateSSOUser(ctx, testIssuer, "sub-1", "dana@example.com", core.RoleProjectMember, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetUserRole(ctx, u.ID, core.RoleProjectViewer); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetUser(ctx, u.ID)
	if err != nil || got.Role != core.RoleProjectViewer || got.RoleFromGroups {
		t.Fatalf("after a hand-set role = %+v, %v", got, err)
	}
}

// With the mapping off, a group-given role stays as it is.
func TestSSOSignInLeavesTheRoleAloneWithNoMapping(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, _, err := s.FindOrCreateSSOUser(ctx, testIssuer, "sub-1", "dana@example.com", core.RoleProjectMember, true); err != nil {
		t.Fatal(err)
	}
	u, _, err := s.FindOrCreateSSOUser(ctx, testIssuer, "sub-1", "dana@example.com", core.RoleNone, false)
	if err != nil || u.Role != core.RoleProjectMember || !u.RoleFromGroups {
		t.Fatalf("with no mapping = %+v, %v", u, err)
	}
}
```

- [ ] **Step 9: Run them and watch them fail**

Run: `go test ./internal/store -run 'SSOSignIn|SetUserRoleClears' -v`
Expected: FAIL — too many arguments to `FindOrCreateSSOUser`.

- [ ] **Step 10: Implement the store change**

`FindOrCreateSSOUser` reads the account once after the insert and email refresh (`prev`), applies the mapping when `groupsMapped`, then re-reads and returns `(u, prev.Role, nil)`:

```go
if groupsMapped {
	if groupRole != core.RoleNone {
		_, err = tx.ExecContext(ctx, `UPDATE users SET role = ?, role_from_groups = 1
			WHERE sign_in = 'sso' AND sso_issuer = ? AND sso_subject = ?`, string(groupRole), issuer, subject)
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE users SET role = CASE WHEN role_from_groups = 1 THEN ? ELSE role END,
			role_from_groups = 0
			WHERE sign_in = 'sso' AND sso_issuer = ? AND sso_subject = ?`, string(core.RoleNone), issuer, subject)
	}
	if err != nil {
		return core.User{}, core.RoleNone, err
	}
}
```

`SetUserRole` becomes `UPDATE users SET role = ?, role_from_groups = 0 WHERE id = ?`.

Document the table in the function's comment: a mapped role always wins and is marked; no mapped role removes only a role that a group gave; a hand-given role stays.

- [ ] **Step 11: Run the store tests**

Run: `go test ./internal/store -v`
Expected: PASS.

- [ ] **Step 12: Write the failing server tests**

In `internal/server/sso_login_test.go`:

```go
func TestSSOSignInTakesTheRoleFromGroups(t *testing.T) {
	fake := fakeIdentity("sub-1", "dana@example.com")
	fake.Identity.Groups = []string{"eng", "admins"}
	e := ssoLoginEnv(t, fake)
	e.srv.ssoEnv.RoleGroups = []core.GroupRole{
		{Group: "eng", Role: core.RoleProjectMember},
		{Group: "admins", Role: core.RoleSystemAdmin},
	}

	newBrowser(e).signIn(t)

	accounts := ssoAccounts(t, e)
	if len(accounts) != 1 || accounts[0].Role != core.RoleSystemAdmin || !accounts[0].RoleFromGroups {
		t.Fatalf("accounts = %+v, want a system admin marked from groups", accounts)
	}
}

func TestSSOSignInLogsARoleChangeWithoutGroupNames(t *testing.T) {
	var logged bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(prev) })

	fake := fakeIdentity("sub-1", "dana@example.com")
	e := ssoLoginEnv(t, fake)
	e.srv.ssoEnv.RoleGroups = []core.GroupRole{{Group: "outhaul-admins", Role: core.RoleSystemAdmin}}
	newBrowser(e).signIn(t) // creates the account with its role
	logged.Reset()
	fake.Identity.Groups = nil
	newBrowser(e).signIn(t) // the group is gone
	out := logged.String()
	if !strings.Contains(out, "role changed from") || !strings.Contains(out, "0 group(s)") {
		t.Errorf("log = %q, want one role-change line with the group count", out)
	}
	if strings.Contains(out, "outhaul-admins") {
		t.Errorf("log names a group: %q", out)
	}
}
```

The second sign-in replaces the browser's session; use a fresh browser as shown.

In `internal/server/gate_signin_test.go`, extend the existing `TestSSORouteVisitCreatesNoAccount` (line 674): before the visit, give the fake groups and install a mapping:

```go
fake := fakeIdentity("sub-1", "dana@example.com")
fake.Identity.Groups = []string{"admins"}
e := ssoLoginEnv(t, fake)
e.srv.ssoEnv.RoleGroups = []core.GroupRole{{Group: "admins", Role: core.RoleSystemAdmin}}
```

The test's existing assertions (no new account, no new Outhaul session) then also prove the mapping doesn't run for a route visit. No new test is needed; if the existing test creates its fake inline, change it there. This one is a regression guard, not a failing-first test: `finishRouteSSO` already changes no role, and Step 14 must not add one.

- [ ] **Step 13: Run them and watch them fail**

Run: `go test ./internal/server -run 'RoleFromGroups|RoleChange' -v`
Expected: FAIL (roles are not applied yet).

- [ ] **Step 14: Apply the mapping in `handleSSOCallback`**

Replace the `FindOrCreateSSOUser` call with:

```go
role := core.RoleForGroups(id.Groups, s.ssoEnv.RoleGroups)
mapped := len(s.ssoEnv.RoleGroups) > 0
user, previous, err := s.store.FindOrCreateSSOUser(r.Context(), id.Issuer, id.Subject, id.Email, role, mapped)
if err != nil {
	log.Printf("sso: finding the account for a sign-in: %v", err)
	ssoRefused(w, r, "sso_failed")
	return
}
if mapped && user.Role != previous {
	log.Printf("sso: account %d (%s): role changed from %q to %q at sign-in; the groups claim held %d group(s)",
		user.ID, user.Display(), previous, user.Role, len(id.Groups))
}
```

`finishRouteSSO` is untouched, so a route visit still creates no account and changes no role.

- [ ] **Step 15: Run the tests and watch them pass**

Run: `go test ./internal/server ./internal/store ./internal/core -v`
Expected: PASS.

- [ ] **Step 16: Commit**

```bash
git add internal/core/role.go internal/core/role_test.go internal/core/user.go \
        internal/store/migrations/0031_role_from_groups.sql \
        internal/store/users.go internal/store/users_test.go internal/store/gate_test.go \
        internal/store/migrate_role_from_groups_test.go \
        internal/server/sso_login.go internal/server/sso_login_test.go internal/server/gate_signin_test.go
git commit -m "feat(server): give SSO accounts a role from their groups"
```

---

### Task 7: Show where the settings and a role came from

**Files:**
- Modify: `internal/server/sso_settings.go` (`ssoPanel` mapping fields)
- Modify: `internal/server/templates/settings.tmpl`
- Modify: `internal/server/users.go:28-75` (`userRow`)
- Modify: `internal/server/templates/users.tmpl:29-37`
- Test: `internal/server/sso_settings_test.go`, `internal/server/users_test.go`

**Interfaces:**
- Consumes: `SSOEnvironment.RoleGroups`/`GroupsClaim`/`Scopes` (Task 5), `core.User.RoleFromGroups` (Task 6).
- Produces: panel keys `RoleGroups` (`[]core.GroupRole`), `GroupsClaim` and `Scopes` (a joined string); `userRow.FromGroups bool`.

- [ ] **Step 1: Write the failing tests**

In `internal/server/sso_settings_test.go`:

```go
func TestSettingsListTheGroupMapping(t *testing.T) {
	e, _ := ssoEnv(t)
	e.srv.ssoEnv = SSOEnvironment{
		FromEnvironment: true, Issuer: "https://idp.example.com", ClientID: "cid", ClientSecret: "sec",
		GroupsClaim: "groups", Scopes: []string{"groups"},
		RoleGroups: []core.GroupRole{
			{Group: "outhaul-admins", Role: core.RoleSystemAdmin},
			{Group: "outhaul-engineers", Role: core.RoleProjectAdmin},
		},
	}
	page := body(t, e.get(t, "/settings"))
	for _, want := range []string{"outhaul-admins", "System admin", "outhaul-engineers", "Project admin", "Groups claim", "groups"} {
		if !strings.Contains(page, want) {
			t.Errorf("the settings page lacks %q", want)
		}
	}
}
```

In `internal/server/users_test.go`:

```go
func TestUsersPageMarksARoleFromGroups(t *testing.T) {
	e := newTestEnv(t)
	e.completeSetup(t)
	id := insertSSOUser(t, e, "dana@example.com", core.RoleProjectMember)
	if _, err := e.rawDB(t).Exec(`UPDATE users SET role_from_groups = 1 WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	page := body(t, e.get(t, "/users"))
	if !strings.Contains(page, "From an SSO group. A change here lasts until they next sign in.") {
		t.Error("the role of a group-given account is not marked")
	}
}
```

- [ ] **Step 2: Run them and watch them fail**

Run: `go test ./internal/server -run 'GroupMapping|MarksARoleFromGroups' -v`
Expected: FAIL.

- [ ] **Step 3: Implement the panel data**

At the end of `ssoPanel`, before returning:

```go
if len(s.ssoEnv.RoleGroups) > 0 {
	p["RoleGroups"] = s.ssoEnv.RoleGroups
	p["GroupsClaim"] = s.ssoEnv.GroupsClaim
}
if len(s.ssoEnv.Scopes) > 0 {
	p["Scopes"] = strings.Join(s.ssoEnv.Scopes, " ")
}
```

`userRow` gains `FromGroups bool`, set from `u.RoleFromGroups`, and the users template shows, under the role form:

```html
{{if .FromGroups}}<p class="muted row-note">From an SSO group. A change here lasts until they next sign in.</p>{{end}}
```

- [ ] **Step 4: Add the settings panel block**

After the form/environment branch in the SSO section:

```html
{{if .RoleGroups}}
<h3>Roles from groups</h3>
<p class="muted env-note">At each sign-in to Outhaul, the highest of these groups decides the person's role. This comes from the environment and can't be changed here. A <span class="mono">project_member</span> is on no project; map <span class="mono">project_admin</span> to a group for people who should start their own.</p>
<table class="table">
  <tbody>
    {{range .RoleGroups}}<tr><td class="mono">{{.Group}}</td><td>{{.Role.Label}}</td></tr>{{end}}
  </tbody>
</table>
<p class="muted env-note">Groups claim: <span class="mono">{{.GroupsClaim}}</span>{{if .Scopes}}. Extra scopes: <span class="mono">{{.Scopes}}</span>{{end}}</p>
{{end}}
```

- [ ] **Step 5: Run the tests and watch them pass**

Run: `go test ./internal/server -run 'SSO|Users' -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/server/sso_settings.go internal/server/templates/settings.tmpl \
        internal/server/users.go internal/server/templates/users.tmpl \
        internal/server/sso_settings_test.go internal/server/users_test.go
git commit -m "feat(ui): show the SSO environment settings and a role's source"
```

---

### Task 8: The documentation

**Files:**
- Modify: `docs/SSO.md`
- Modify: `README.md:222-257` (configuration table)
- Modify: `ARCHITECTURE.md:35-44` (Auth) and a new section after "Protected routes (done)"
- Modify: `AGENTS.md` ("Decisions to keep", after the SSO accounts bullet)

**Interfaces:**
- Consumes: everything above; no code.
- Produces: documentation that matches the behaviour.

- [ ] **Step 1: `docs/SSO.md`**

Add two sections after "Worked example: Okta":

1. **Connecting SSO from the environment.** The three variables, all or none, needing `OUTHAUL_PUBLIC_URL`; the environment wins and stored settings are kept but unused; the panel is read-only and saving/removing there is refused; the secret file (absolute path, or a credential name; root-owned 0600; systemd credential drop-in):

```ini
# /etc/systemd/system/outhaul.service.d/sso.conf
[Service]
LoadCredential=sso-client-secret:/etc/outhaul/sso-client-secret
```

```sh
# /etc/outhaul.env
OUTHAUL_SSO_ISSUER=https://idp.example.com
OUTHAUL_SSO_CLIENT_ID=<client id>
OUTHAUL_SSO_CLIENT_SECRET_FILE=sso-client-secret
```

Say the secret never enters the environment or the database, one trailing newline is dropped, and a change takes effect at the next restart. Say a changed issuer or client ID, from either source, ends every SSO sign-in at startup, and a changed issuer makes everyone a new account.

2. **Roles from groups.** `OUTHAUL_SSO_ROLE_GROUPS` (pairs, first colon splits, spaces trimmed, no commas in group names, one role per group), `OUTHAUL_SSO_GROUPS_CLAIM` (default `groups`), `OUTHAUL_SSO_SCOPES` (nothing requested by default; some providers need `groups`). The rule: highest mapped role wins, compared exactly; groups come from the ID token only; no claim means no groups; applied only at a sign-in to Outhaul, never for a protected route. The table from the spec (mapped → that role; no longer mapped and the role came from a group → no role; otherwise unchanged). A hand-set role lasts until the next sign-in for people in a mapped group. Removal shows at the next sign-in (8-hour sessions); delete the account to cut them off at once. `project_member` shares no project. A `system_admin` mapping means whoever can change that group at the provider controls Outhaul's admins. Password accounts are never affected, and the last-password-admin rule stands.

Also add Okta steps for the groups claim (written from Okta's documentation): add a `groups` claim with a filter that matches the Outhaul application, and add the `groups` scope; then set `OUTHAUL_SSO_SCOPES=groups`.

Adjust "First sign-in and roles": with the mapping set, the account arrives with its role and does not wait on the Users page.

- [ ] **Step 2: `README.md`**

Add rows in the configuration table, after `OUTHAUL_PUBLIC_URL`:

```markdown
| `OUTHAUL_SSO_ISSUER`    | (empty)              | OIDC issuer URL; with the next two, connects SSO from the environment, and the environment wins over Settings — see docs/SSO.md |
| `OUTHAUL_SSO_CLIENT_ID` | (empty)              | Client ID for that provider                          |
| `OUTHAUL_SSO_CLIENT_SECRET_FILE` | (empty)      | Absolute path, or a systemd credential name, holding the client secret; never a secret in the environment |
| `OUTHAUL_SSO_ROLE_GROUPS` | (empty)            | `role:group` pairs, comma-separated; gives SSO accounts a role at each sign-in — see docs/SSO.md |
| `OUTHAUL_SSO_GROUPS_CLAIM` | `groups`          | The ID token claim holding group names; needs `OUTHAUL_SSO_ROLE_GROUPS` |
| `OUTHAUL_SSO_SCOPES`    | (empty)              | Extra scopes to request at the provider, separated by commas or spaces |
```

- [ ] **Step 3: `ARCHITECTURE.md`**

In the Auth bullet, change "configured on Settings" to "configured on Settings or in the environment", and "starts with no role" to "starts with no role unless one of its provider groups is mapped to one".

Add a section after "Protected routes (done)":

```markdown
### SSO from configuration, and roles from groups (opt-in)

`OUTHAUL_SSO_ISSUER`, `OUTHAUL_SSO_CLIENT_ID` and
`OUTHAUL_SSO_CLIENT_SECRET_FILE` connect the provider without the Settings
page. The environment wins while it is set; stored settings are kept, unused.
The secret is read once at startup and never written to the database. A plain
`settings` record of the issuer and client ID last run with lets startup end
every SSO sign-in when they change, exactly as saving on Settings does.

`OUTHAUL_SSO_ROLE_GROUPS` maps the provider's groups to Outhaul's roles at each
sign-in to Outhaul. The highest mapped role wins. A mapped role always applies;
one that came from a group goes when no group maps any more; a hand-given role
lasts until the next sign-in. Protected-route sign-ins neither create an
account nor change a role, and a password account's role is never touched. This
replaces the 2026-11-02 design's "taking roles or groups from the SSO provider"
non-goal, opt-in. See `docs/SSO.md`.
```

- [ ] **Step 4: `AGENTS.md`**

Two bullets in "Decisions to keep":

```markdown
- **SSO from the environment.** `OUTHAUL_SSO_ISSUER`,
  `OUTHAUL_SSO_CLIENT_ID` and `OUTHAUL_SSO_CLIENT_SECRET_FILE` win over the
  stored settings while set; the secret is read once in `serve` and never
  written to the database, a log line or a page. Saving or removing SSO on
  Settings answers 409 while they are set. `SyncSSOStartup` records the issuer
  and client ID last run with, and ends every SSO sign-in when they change.
- **Roles from groups.** `OUTHAUL_SSO_ROLE_GROUPS` maps provider groups to
  roles at each sign-in to Outhaul only. The highest wins and names are
  compared exactly; `users.role_from_groups` marks the accounts it applies to,
  and `SetUserRole` clears the mark. A mapped role always wins at the next
  sign-in; one that came from a group goes when no group maps any more. It
  never touches a password account, and protected-route sign-ins don't run it.
```

- [ ] **Step 5: Run the full suite**

```bash
gofmt -l . && go build ./... && go vet ./... && go test ./...
go test -race -timeout 30m ./...
```

Expected: `gofmt -l` prints nothing; build, vet and both test runs pass.

- [ ] **Step 6: Commit**

```bash
git add docs/SSO.md README.md ARCHITECTURE.md AGENTS.md
git commit -m "docs(sso): settings from the environment and roles from groups"
```
