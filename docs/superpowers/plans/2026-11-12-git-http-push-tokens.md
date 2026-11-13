# git push over HTTPS with push tokens (R18) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `git push`, `git fetch` and `git clone` for push apps over HTTPS on the admin host, signed in with an expiring push token and nothing else, off unless `OUTHAUL_GIT_HTTP=on`.

**Architecture:** A new package `internal/githttp` is the HTTP transport, as `internal/gitssh` is the SSH one: it checks each request (host, token, SSO rule, role, app), then runs `git receive-pack` or `git upload-pack` with `--stateless-rpc` and flushes every write. Tokens live in a new `push_tokens` table as SHA-256 hashes. The existing `post-receive` hook and bridge deploy the push; the hook relays two values from the transport so the bridge can log the push and print the build log's address. `OUTHAUL_UPLOAD_TIMEOUT` lifts Traefik's 60-second read timeout and stands alone.

**Tech Stack:** Go (`go.mod`), `net/http` standard router, SQLite through `internal/store` with embedded migrations, the `git` CLI, Traefik v3.7.6 flags from `internal/traefik`.

**Spec:** `docs/superpowers/specs/2026-11-12-git-http-push-tokens-design.md`. Read it before a task; this plan names the files, signatures and tests, and the spec says why.

## Global Constraints

- **Test first.** Write the failing test, run it, see it fail for the right reason, then write the code. Unit tests use the real store on a temp file and the fakes the package already has.
- **Opt-in.** With `OUTHAUL_GIT_HTTP` and `OUTHAUL_UPLOAD_TIMEOUT` unset nothing changes: no route, no panel, no Traefik flag, `TestProxySpecGolden` passes without `-update`, an SSH push prints what it prints today.
- **A token is never logged**, nor its hash, the `Authorization` header or the username typed. It is never put in a URL, a process argument or an environment variable. Only its row id and label may appear.
- **Only a live push token signs these paths in.** No session cookie is read on `/git/`; a password is never compared with anything.
- **Exact values:** token `ohp_` + 64 lowercase hex (32 bytes of `crypto/rand`); stored as SHA-256 hex; compared with `subtle.ConstantTimeCompare`; challenge `WWW-Authenticate: Basic realm="outhaul"`; defaults 7 days, 30 days maximum, SSO age `7d`; at most 25 tokens an account; expired tokens deleted 30 days after expiry; failed attempts 10 at once and one more a minute, IPv6 by /64, at most 10,000 addresses; keep-alive 5 seconds.
- **Migrations:** never edit a shipped one. The new one is `0033_push_tokens.sql`; if `main` has taken 0033 by then, use the next free number.
- **Settings** go through the `parser` in `internal/config/config.go`, and `Validate` refuses bad values, naming the variable.
- **New routes** go in `routes()` with an access level.
- **This repository is public:** `example.com` names only, in code, tests and docs.
- **Commits:** `feat(scope): summary` or `fix(scope): summary`; the body says what a person saw and what changed; end with `Co-Authored-By: Claude Fable 5.2 <noreply@anthropic.com>`. Never commit the `outhaul` binary. Don't push, and don't merge into `main`.
- **Running tests:** `/tmp` holds the Go build cache under a quota, so test one package at a time: `go test ./internal/store/`. `internal/server` takes about three minutes. Before a task is done: `go build ./... && go vet ./...` and the tests of every package it touched.
- **Docs style:** plain sentences, as the existing pages are written. No value from a token, a header or a secret in an example.

## Review Focus

Inputs the spec implies but doesn't spell out, most likely first. Each has a test in the task named.

1. A push bigger than 1 MiB sent chunked (no `Content-Length`): it arrives whole, and every other route still refuses such a body. Task 6.
2. A token pasted with a space or newline around it: it works; the password is trimmed of ASCII whitespace before the shape check. Task 6.
3. The create form with a bad label or days value (`""`, 65 characters, a tab, a label already used, days `0`, `31`, `abc`, `""`): the page comes back with status 400 and a message, and no row is made. Task 3.
4. A client that goes away mid-push: the `git` child ends and no goroutine is left waiting. Task 6.
5. A request body with `Content-Encoding: gzip`, and one that claims gzip but isn't: the first reaches git unpacked, the second answers 400. Task 6.

---

### Task 1: `OUTHAUL_UPLOAD_TIMEOUT`

**Files:**
- Modify: `internal/config/config.go`, `internal/config/config_test.go`
- Modify: `internal/traefik/proxy.go`, `internal/traefik/proxy_test.go`
- Modify: `main.go` (`infraController.proxyConfig`, and a function for the startup line), `main_test.go`
- Modify: `README.md` (configuration table), `docs/BEHIND-A-PROXY.md`

**Interfaces:**
- Produces: `config.Config.UploadTimeout time.Duration` (0 = unset); `traefik.ProxyConfig.UploadTimeout time.Duration`.
- Produces: `func uploadTimeoutHint(gitHTTP bool, uploadTimeout time.Duration) string` in `main.go`, beside `trustedAllWarning`. `cfg.GitHTTP` doesn't exist until Task 2, so this task only writes and tests the function; Task 6 calls it from `serve`.

- [ ] **Step 1: Failing config tests** in `internal/config/config_test.go`:

```go
func TestUploadTimeout(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  time.Duration
		bad   bool
	}{
		{"", 0, false},
		{"10m", 10 * time.Minute, false},
		{"600s", 600 * time.Second, false},
		{"1s", time.Second, false},
		{"500ms", 0, true},
		{"1500ms", 0, true}, // not whole seconds
		{"0", 0, true},
		{"-5s", 0, true},
		{"ten", 0, true},
	} {
		c := Load(func(k string) string {
			if k == "OUTHAUL_UPLOAD_TIMEOUT" {
				return tc.value
			}
			return ""
		})
		err := c.Validate()
		if tc.bad {
			if err == nil || !strings.Contains(err.Error(), "OUTHAUL_UPLOAD_TIMEOUT") {
				t.Errorf("%q: want a refusal naming the variable, got %v", tc.value, err)
			}
			continue
		}
		if err != nil || c.UploadTimeout != tc.want {
			t.Errorf("%q: UploadTimeout=%v err=%v, want %v", tc.value, c.UploadTimeout, err, tc.want)
		}
	}
}
```

- [ ] **Step 2:** Run `go test ./internal/config/ -run TestUploadTimeout`. Expected: FAIL, `c.UploadTimeout undefined`.
- [ ] **Step 3:** Add the field and a parser method `func (p *parser) uploadTimeout(key string) time.Duration`: unset gives 0; otherwise `time.ParseDuration`, at least one second and a whole number of seconds, or `p.reject(key, v, "a duration of whole seconds, such as 10m")`.
- [ ] **Step 4:** Run the test. Expected: PASS.
- [ ] **Step 5: Failing Traefik tests** in `internal/traefik/proxy_test.go`, modelled on `TestProxySpecTrustedEntrypoints` and `TestProxySpecHashChangesWithTrust`:
  - `TestProxySpecUploadTimeout`: with `UploadTimeout: 10 * time.Minute`, the command holds `--entrypoints.<ep>.transport.respondingtimeouts.readtimeout=600s` for exactly the entrypoints the spec declares: `web` and `websecure` with ACME; `web` only in tunnel mode; `websecure` only with `Routing.HTTPOff` and ACME.
  - `TestProxySpecHashChangesWithUploadTimeout`: the `outhaul.config-hash` label differs between 0 and 10 minutes, and between 10 and 20.
  - `TestProxySpecNoUploadTimeoutFlag`: with 0, no argument contains `respondingtimeouts`.
- [ ] **Step 6:** Run `go test ./internal/traefik/`. Expected: the three new tests FAIL; `TestProxySpecGolden` passes.
- [ ] **Step 7:** Add `UploadTimeout` to `ProxyConfig` with a comment, and in `proxySpec`, after the entrypoints are known, append the flag for each one, valued `strconv.FormatInt(int64(pc.UploadTimeout/time.Second), 10) + "s"`.
- [ ] **Step 8:** Run `go test ./internal/traefik/`. Expected: PASS, the golden test included, without `-update`.
- [ ] **Step 9: Failing `main` tests** in `main_test.go`: `TestProxyConfigCarriesUploadTimeout` (modelled on `TestProxyConfigCarriesTrustedProxies`: `pc.UploadTimeout == cfg.UploadTimeout`, tunnel on and off), and `TestUploadTimeoutHint`: `uploadTimeoutHint(true, 0)` names `OUTHAUL_UPLOAD_TIMEOUT` and "60 seconds"; `uploadTimeoutHint(true, time.Minute)`, `uploadTimeoutHint(false, 0)` are `""`.
- [ ] **Step 10:** Implement both; run `go test .`. Expected: PASS.
- [ ] **Step 11: Docs.** `README.md`: a row `OUTHAUL_UPLOAD_TIMEOUT`, default "(unset: 60 seconds)", "How long Traefik waits for a whole request, body included. Set it when uploads through Outhaul take more than a minute, for every app and for git over HTTPS; changing it recreates Traefik once". `docs/BEHIND-A-PROXY.md`: a short section "Uploads that take over a minute" with what was measured (spec, "Uploads that take over a minute").
- [ ] **Step 12:** `go build ./... && go vet ./...`, then commit:

```
feat(proxy): OUTHAUL_UPLOAD_TIMEOUT lifts Traefik's 60-second read timeout
```

---

### Task 2: Settings, and the `push_tokens` store

**Files:**
- Modify: `internal/config/config.go`, `internal/config/config_test.go`
- Create: `internal/core/pushtoken.go`, `internal/core/pushtoken_test.go`
- Create: `internal/store/migrations/0033_push_tokens.sql`
- Create: `internal/store/pushtokens.go`, `internal/store/pushtokens_test.go`
- Modify: `internal/store/snapshot.go` (`stripSessions`), `internal/store/sso.go` (`endSSOSessions`), and their test files

**Interfaces:**
- Produces, in `config.Config`: `GitHTTP bool`, `PushTokenDefaultDays int`, `PushTokenMaxDays int`, `PushTokenSSOMaxAge time.Duration`.
- Produces, in `internal/core/pushtoken.go`:

```go
const PushTokenPrefix = "ohp_"
const MaxPushTokensPerUser = 25
const PushTokenKeepExpired = 30 * 24 * time.Hour

type PushToken struct {
	ID, UserID           int64
	Label                string
	CreatedAt, ExpiresAt time.Time
	LastUsedAt           *time.Time
	LastUsedIP           string
}

// EndsAt is the earlier of ExpiresAt and CreatedAt plus maxDays days.
func (t PushToken) EndsAt(maxDays int) time.Time
// Expired reports whether now is at or past EndsAt.
func (t PushToken) Expired(now time.Time, maxDays int) bool

// PushTokenRow is a token with its owner's Display name, for the Users page.
type PushTokenRow struct {
	PushToken
	Owner string
}

// ValidPushTokenShape: exactly "ohp_" and 64 of 0-9 a-f.
func ValidPushTokenShape(s string) bool
// CleanPushTokenLabel trims spaces; ok is false unless 1 to 64 characters
// remain and none is a control character.
func CleanPushTokenLabel(s string) (label string, ok bool)
```

- Produces, in `internal/store/pushtokens.go`:

```go
var ErrPushTokenLabel = errors.New("push token: label must be 1 to 64 characters")
var ErrPushTokenLabelTaken = errors.New("push token: label already used")
var ErrPushTokenLimit = errors.New("push token: account has 25 tokens")

// CreatePushToken makes a token for the account and returns it once. Only
// its SHA-256 is stored.
func (s *Store) CreatePushToken(ctx context.Context, userID int64, label string, now, expires time.Time) (raw string, t core.PushToken, err error)
// FindPushToken returns the row for raw, expired or not. A value of the wrong
// shape is not looked up.
func (s *Store) FindPushToken(ctx context.Context, raw string) (core.PushToken, bool, error)
// GetPushToken returns sql.ErrNoRows when the row is gone.
func (s *Store) GetPushToken(ctx context.Context, id int64) (core.PushToken, error)
func (s *Store) ListPushTokensForUser(ctx context.Context, userID int64, now time.Time) ([]core.PushToken, error)
func (s *Store) ListPushTokens(ctx context.Context, now time.Time) ([]core.PushTokenRow, error)
// DeletePushTokenForUser deletes nothing when the id is another account's.
func (s *Store) DeletePushTokenForUser(ctx context.Context, userID, id int64) error
// TouchPushToken records a use, at most once a minute unless ip changed.
func (s *Store) TouchPushToken(ctx context.Context, id int64, at time.Time, ip string) error
```

- [ ] **Step 1: Failing config tests.** `TestGitHTTPSettings`, a table over `Load` + `Validate`:

| Environment | Result |
|---|---|
| nothing | `GitHTTP` false, no error |
| `OUTHAUL_GIT_HTTP=on`, `OUTHAUL_PUBLIC_URL=https://outhaul.example.com` | on; default days 7, max 30, SSO age 168h |
| `OUTHAUL_GIT_HTTP=on`, no public URL | error names `OUTHAUL_GIT_HTTP` and `OUTHAUL_PUBLIC_URL` |
| `OUTHAUL_GIT_HTTP=on`, `OUTHAUL_PUBLIC_URL=http://outhaul.example.com` | error names `https` |
| on, `OUTHAUL_PUSH_TOKEN_MAX_DAYS=3` | max 3, default 3 |
| on, max `0`, `366`, `x` | error names `OUTHAUL_PUSH_TOKEN_MAX_DAYS` |
| on, default `14`, max `10` | error names `OUTHAUL_PUSH_TOKEN_DEFAULT_DAYS` |
| on, default `0`, `x` | error names it |
| on, `OUTHAUL_PUSH_TOKEN_SSO_MAX_AGE=2d` | 48h |
| on, SSO age `36h` | 36h |
| on, SSO age `30m`, `0d`, `1.5d`, `week` | error names `OUTHAUL_PUSH_TOKEN_SSO_MAX_AGE` |
| off, each of the three token variables set in turn | error: that variable needs `OUTHAUL_GIT_HTTP=on` |
| `OUTHAUL_GIT_HTTP=On` | on (yes/no settings ignore case) |

- [ ] **Step 2:** Run `go test ./internal/config/ -run TestGitHTTPSettings`. Expected: FAIL to compile.
- [ ] **Step 3:** Implement in `Load` (parser methods) and `Validate` (the cross-setting rules), following how `OUTHAUL_SSO_GROUPS_CLAIM` is refused without its mapping. Run. Expected: PASS.
- [ ] **Step 4: Failing core tests** `internal/core/pushtoken_test.go`: `ValidPushTokenShape` (`ohp_`+64 hex true; 63, 65, upper-case hex, `ohq_`, `""`, a trailing newline false); `CleanPushTokenLabel` (`" ci "` gives `"ci"`; `""`, `"   "`, 65 characters, `"a\tb"`, `"a\x00b"` not ok; 64 characters and `"déploiement"` ok); `EndsAt` (created day 0, expires day 20: max 30 gives day 20, max 10 gives day 10); `Expired` true at exactly `EndsAt`.
- [ ] **Step 5:** Run, see FAIL; implement; run, see PASS.
- [ ] **Step 6: Failing store tests** `internal/store/pushtokens_test.go`, with `newTestStore(t)`:
  - `TestCreatePushTokenStoresOnlyTheHash`: `raw` passes `core.ValidPushTokenShape`; two calls give different values; reading the file's `push_tokens` row through a raw query, `token_hash` equals `hex(sha256(raw))`, and no column of any row holds `raw` or `raw[4:]`.
  - `TestFindPushToken`: finds by `raw`; a flipped last character isn't found; `"hunter2"` isn't found; an expired row is still returned (the caller decides).
  - `TestPushTokenLabelRules`: a second token with the same label on one account gives `ErrPushTokenLabelTaken`; the same label on another account is fine; `"  "` gives `ErrPushTokenLabel`.
  - `TestPushTokenLimit`: the 26th gives `ErrPushTokenLimit`, expired ones counted.
  - `TestListAndDeletePushTokens`: an account lists only its own, newest first; `DeletePushTokenForUser` with another account's id deletes nothing; with its own, `FindPushToken` then says not found.
  - `TestExpiredPushTokensArePruned`: a row expired 31 days before `now` is gone after `ListPushTokensForUser`, `ListPushTokens` and `CreatePushToken`; one expired 29 days before stays, listed.
  - `TestDeleteUserDeletesPushTokens`: after `DeleteUser`, `FindPushToken` says not found.
  - `TestTouchPushToken`: first touch sets time and address; a touch 30 seconds later from the same address leaves the time; 30 seconds later from another address writes both; 61 seconds later from the same address writes the time.
  - `TestListPushTokensNamesOwner`: `Owner` is the account's `Display()`, for a password account and an SSO account.
  - `TestMigration0033`: `openMigratedBefore(t, "0033")` then a normal open: the table exists and is empty, and existing rows of `users` and `push_keys` are untouched.
  - In the snapshot test file: `TestSnapshotHoldsNoPushTokens` (a store with one token; the snapshot's `push_tokens` is empty and the token value's hash isn't in the file's bytes), and the same for `StripSessions`.
  - In the SSO test file: `TestEndingSSOSignInsDeletesSSOPushTokens`: one password account and one SSO account, a token each; after `ClearSSOSettings`, and separately after `SetSSOSettings(..., true)` and a `SyncSSOStartup` that reports a change, the SSO account's token is gone and the password account's stays; `SetSSOSettings(..., false)` deletes none.
- [ ] **Step 7:** Run `go test ./internal/store/`. Expected: FAIL to compile.
- [ ] **Step 8:** Write the migration:

```sql
CREATE TABLE push_tokens (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    label        TEXT    NOT NULL,
    token_hash   TEXT    NOT NULL UNIQUE,
    created_at   TEXT    NOT NULL,
    expires_at   TEXT    NOT NULL,
    last_used_at TEXT    NOT NULL DEFAULT '',
    last_used_ip TEXT    NOT NULL DEFAULT '',
    UNIQUE (user_id, label)
);
CREATE INDEX push_tokens_user ON push_tokens(user_id);
```

  Match the time format and nullability the neighbouring tables use (`fmtTime`, `scanTime`); if `push_keys` stores an unset time another way, do it that way.
- [ ] **Step 9:** Implement `pushtokens.go`. `CreatePushToken` reads 32 bytes of `crypto/rand`, checks the label with `core.CleanPushTokenLabel`, counts the account's rows and inserts, in one transaction. `FindPushToken` hashes with the store's `hashToken`, selects by `token_hash`, then compares the stored and computed hashes with `subtle.ConstantTimeCompare`. The three functions the prune test names first delete rows with `expires_at` more than `core.PushTokenKeepExpired` before `now`. Add `push_tokens` to the tables `stripSessions` empties and fix its comment; add to `endSSOSessions` a delete of `push_tokens` whose `user_id` is an SSO account.
- [ ] **Step 10:** Run `go test ./internal/store/ ./internal/core/ ./internal/config/` one package at a time. Expected: PASS.
- [ ] **Step 11:** `go build ./... && go vet ./...`; `go test .` (restore-state tests live in the root package). Commit:

```
feat(store): push tokens, stored as hashes, and the settings for git over HTTPS
```

---

### Task 3: The Account and Users panels

**Files:**
- Create: `internal/server/pushtokens.go`, `internal/server/pushtokens_test.go`
- Modify: `internal/server/server.go` (a field), `internal/server/routes.go`, `internal/server/account.go` (`renderAccount`), `internal/server/users.go` (`usersPage`)
- Modify: `internal/server/templates/account.tmpl`, `internal/server/templates/users.tmpl`
- Modify: `main.go` (call the setter), `internal/server/access_test.go` if its route census lists every route

**Interfaces:**
- Consumes: Task 2's store functions and `config` fields.
- Produces:

```go
// GitHTTPOptions turns git over HTTPS on. Call SetGitHTTP before Handler.
type GitHTTPOptions struct {
	DefaultDays, MaxDays int
	SSOMaxAge            time.Duration
}
func (s *Server) SetGitHTTP(o GitHTTPOptions)
func (s *Server) gitHTTPOn() bool
```

  Task 6 adds fields to `GitHTTPOptions`; keep it a struct passed by value.
- Routes, appended in `routes()` only when `s.gitHTTPOn()`:
  - `POST /account/push-tokens`, `levelAccount`, `s.accountPage(s.handleAddPushToken)`
  - `POST /account/push-tokens/{id}/delete`, `levelAccount`, `s.accountPage(s.handleDeletePushToken)`
  - `POST /users/{id}/push-tokens/{tokenID}/delete`, `levelSystemAdmin`, `s.handleAdminDeletePushToken`
- Form fields: `label`, `days`.

- [ ] **Step 1: Failing tests** in `internal/server/pushtokens_test.go`. Add a helper `newGitHTTPEnv(t)` that builds what `newTestEnv` builds and calls `SetGitHTTP(GitHTTPOptions{DefaultDays: 7, MaxDays: 30, SSOMaxAge: 7 * 24 * time.Hour})` before `Handler()` (refactor `newTestEnv` to take an optional `func(*Server)` if that is the smallest change).
  - `TestPushTokenRoutesAbsentWhenOff`: with `newTestEnv`, a signed-in `POST /account/push-tokens` answers 404, and `GET /account` has no "Push tokens".
  - `TestCreatePushTokenShowsItOnce`: signed in, `POST /account/push-tokens` with `label=laptop&days=7` answers 200 (not a redirect) with `Cache-Control: no-store`; the body holds exactly one `ohp_[0-9a-f]{64}` and "It won't be shown again"; `store.FindPushToken` finds that value, owned by the signed-in account, expiring 7 days after now within a minute. A following `GET /account` lists `laptop` and holds no `ohp_`.
  - `TestCreatePushTokenRefusals` (Review Focus 3): for label `""`, 65 `a`s, `"a\tb"`, an existing label, and days `0`, `31`, `abc`, `""`: status 400, the page with an error message, and the account's token count unchanged. The 26th token: 400.
  - `TestDeleteOwnPushToken`: the owner's delete redirects (303) to `/account` and the row is gone; another signed-in account posting the same id gets the redirect and the row stays.
  - `TestAccountPagePushTokens`: an expired token shows "expired"; a used one shows its time and address; an SSO account (use `ssoAccount`) sees "Your tokens work until" and a password account doesn't.
  - `TestUsersPagePushTokens`: a system admin's `GET /users` lists another account's token with its owner and label and no `ohp_`; `POST /users/{id}/push-tokens/{tokenID}/delete` removes it and redirects to `/users`; a project admin gets 403 on that route; a `tokenID` that belongs to a different `{id}` deletes nothing.
  - `TestViewerSeesNoTokenValueElsewhere`: after creating a token, no page among `/account`, `/users`, `/` contains its value or its hash.
- [ ] **Step 2:** Run `go test ./internal/server/ -run 'PushToken|UsersPagePushTokens|AccountPagePushTokens|ViewerSeesNoToken'`. Expected: FAIL to compile.
- [ ] **Step 3:** Implement. The create handler parses `days` as a whole number from 1 to `MaxDays`, calls `CreatePushToken(ctx, me.ID, label, now, now.Add(days))`, and on success renders the account page itself with the token in `NewPushToken`; errors re-render with 400. `renderAccount` adds `GitHTTP`, `PushTokens` (each with `EndsAt(MaxDays)` and whether it is expired), `TokenDefaultDays`, `TokenMaxDays`, and for an SSO account `SSOTokensUntil` (`LastSignInAt` plus `SSOMaxAge`). `usersPage` adds `GitHTTP` and `PushTokens` from `ListPushTokens`.
- [ ] **Step 4: Templates.** `account.tmpl`: a "Push tokens" panel under "Push keys", inside `{{if .GitHTTP}}`: the shown-once box (`copywrap` and `copybtn`, as `users.tmpl` shows a setup link) with "Copy it now. It won't be shown again."; the table (label, created, expires or "expired", last used with address or "never", a revoke button); the form with `label` and "Expires in [n] days (at most N)" as a number input with `min=1`, `max`, and the default as its value; and the SSO line "Your tokens work until <time>. Sign in with SSO again to move that on." `users.tmpl`: a "Push tokens" panel inside `{{if .GitHTTP}}`, with owner, label, created, expires, last used, revoke; and both notes about removing someone say push tokens go with push keys only when `.GitHTTP` (the text is unchanged when off).
- [ ] **Step 5:** `main.go`: after `srv.SetRepos(repos)`, `if cfg.GitHTTP { srv.SetGitHTTP(server.GitHTTPOptions{DefaultDays: cfg.PushTokenDefaultDays, MaxDays: cfg.PushTokenMaxDays, SSOMaxAge: cfg.PushTokenSSOMaxAge}) }`.
- [ ] **Step 6:** Run `go test ./internal/server/` (about three minutes) and `go test .`. Expected: PASS, the existing route and access tests included.
- [ ] **Step 7:** `go build ./... && go vet ./...`. Commit:

```
feat(account): create, list and revoke push tokens
```

---

### Task 4: The hook says where a push came from

**Files:**
- Modify: `internal/githook/wire.go`, `internal/githook/client.go`, `internal/githook/bridge.go`
- Modify: `internal/githook/bridge_test.go`, and a new `internal/githook/origin_test.go`
- Modify: `main.go` (`runGitHook`, `startGitPush`)

**Interfaces:**
- Consumes: `store.GetPushToken`, `store.GetUser` (Task 2).
- Produces, in `internal/githook`:

```go
const EnvTokenID = "OUTHAUL_PUSH_TOKEN_ID"       // the push_tokens row id, never the token
const EnvClientAddr = "OUTHAUL_PUSH_CLIENT_ADDR" // the client's IP address

// Origin is what the HTTPS transport knew about a push. The zero value is a
// push over SSH.
type Origin struct {
	TokenID int64
	Addr    string
}
func (o Origin) HTTPS() bool // TokenID != 0

// OriginFromEnv reads the two variables. An id that isn't a positive whole
// number gives the zero Origin; an Addr that netip.ParseAddr refuses is dropped.
func OriginFromEnv(getenv func(string) string) Origin

func RunHookFrom(sockPath, app string, o Origin, stdin io.Reader, stdout io.Writer) (int, error)
// RunHook stays, as RunHookFrom with the zero Origin.

func (b *Bridge) SetPublicURL(u string) // call before Serve; "" prints no build-log line
```

- The bridge's `Store` interface gains `GetPushToken(ctx, id int64) (core.PushToken, error)` and `GetUser(ctx, id int64) (core.User, error)`.
- Wire format: after `app <name>`, two optional lines `token <id>` and `addr <ip>`, written only when `Origin.HTTPS()`.

- [ ] **Step 1: Failing tests.**
  - `TestRequestRoundTripWithOrigin`: `writeRequest` then `readRequest` gives back `Origin{TokenID: 42, Addr: "203.0.113.9"}`; with the zero Origin the bytes written are exactly what today's `writeRequest` writes (compare with a literal).
  - `TestReadRequestRefusesBadOrigin`: `token x`, `token -1`, `addr not-an-ip` each give an error.
  - `TestOriginFromEnv`: both set; id `""`, `abc`, `0`, `-3` give the zero Origin; a bad address is dropped and the id kept.
  - `TestBridgeLogsAnHTTPSPush`: with `bridgeEnv`, a password account `alice`, a token labelled `laptop`, a push app `api`, `SetPublicURL("https://outhaul.example.com")`, and `log.SetOutput` to a buffer (restored in cleanup): after `runHandle` with that Origin and one ref update to `refs/heads/main` with new commit `abc123`, the log holds one line containing `alice`, `laptop`, the token id, `203.0.113.9`, `api`, `refs/heads/main` and `abc123`; the output's first line is `→ build log: https://outhaul.example.com/deployments/<id>`.
  - `TestBridgeSSHPushUnchanged`: the same push with the zero Origin: no `build log` line in the output and nothing logged.
  - `TestBridgeLogsAPushWhoseTokenIsGone`: the token row deleted before the push: the push still deploys and the log line holds the token id and "removed".
  - `TestBridgeLogsAPushThatDeploysNothing`: an HTTPS push to a branch that isn't the deploy branch is still logged, and prints no build-log line.
- [ ] **Step 2:** Run `go test ./internal/githook/`. Expected: FAIL to compile.
- [ ] **Step 3:** Implement. The log line is written once per request with an HTTPS origin, after the request is read and the app resolved, with `log.Printf`; it holds the account's `Display()` and id, the label and id, the address, the app, and each ref with its new commit. The build-log line is the first thing `runDeploy` writes, before the history, on both the branch and the tag path.
- [ ] **Step 4:** `main.go`: `runGitHook` calls `githook.RunHookFrom(args[1], args[0], githook.OriginFromEnv(os.Getenv), os.Stdin, os.Stdout)`; `startGitPush` calls `bridge.SetPublicURL(cfg.PublicURL)` when `cfg.GitHTTP`.
- [ ] **Step 5:** Run `go test ./internal/githook/` and `go test .`. Expected: PASS, with every existing bridge test untouched.
- [ ] **Step 6:** `go build ./... && go vet ./...`. Commit:

```
feat(githook): log an HTTPS push and print its build log's address
```

---

### Task 5: The shared role check, the client address, the limit

**Files:**
- Create: `internal/pushauth/pushauth.go`, `internal/pushauth/pushauth_test.go`
- Modify: `internal/gitssh/server.go` (`authorize` calls it)
- Create: `internal/githttp/clientaddr.go`, `internal/githttp/clientaddr_test.go`
- Create: `internal/githttp/limiter.go`, `internal/githttp/limiter_test.go`

**Interfaces:**
- Produces:

```go
// package pushauth
type Store interface {
	GetUser(ctx context.Context, id int64) (core.User, error)
	IsProjectMember(ctx context.Context, projectID, userID int64) (bool, error)
}
// Allowed reports whether the account may push to and fetch from apps in the
// project: a system admin always; anyone else with project member or above
// and a place on the project. An account that no longer exists may not. The
// role is read now.
func Allowed(ctx context.Context, st Store, userID, projectID int64) (bool, error)
```

```go
// package githttp
// Proxies is what Outhaul knows about the proxies in front of it.
type Proxies struct {
	Trusted  []netip.Prefix // OUTHAUL_TRUSTED_PROXIES; an address is a /32 or /128
	TrustAll bool           // OUTHAUL_TRUSTED_PROXIES=all
	// Traefik is Outhaul's own Traefik on the Docker network; ok is false
	// while that isn't known.
	Traefik func(ctx context.Context) (addr netip.Addr, ok bool)
}
func ParseTrusted(entries []string) ([]netip.Prefix, error)
// ClientAddr is the address a request is counted and recorded under.
func (p Proxies) ClientAddr(r *http.Request) netip.Addr

func NewLimiter(now func() time.Time) *Limiter
// Fail records a failed attempt from addr. limited is true when the address
// had none left; wait is then how long until it has one.
func (l *Limiter) Fail(addr netip.Addr) (wait time.Duration, limited bool)
```

- [ ] **Step 1: Failing `pushauth` tests** with a real store: a system admin on no project is allowed; a project member on the project is allowed and on another isn't; a viewer on the project isn't; an account with no role isn't; a deleted account isn't (and no error); a store error is returned.
- [ ] **Step 2:** Run, see FAIL; implement by moving the body of `gitssh.authorize` after the key lookup; make `authorize` call it and map `false` to `errPushDenied`. Run `go test ./internal/pushauth/ ./internal/gitssh/` one at a time. Expected: PASS, the existing `gitssh` tests unchanged.
- [ ] **Step 3: Failing client-address tests**, a table. `T` is Traefik at `172.18.0.2`; the request's `RemoteAddr` and `X-Forwarded-For` are given; expected client:

| Trust | RemoteAddr | X-Forwarded-For | Client |
|---|---|---|---|
| unset | `198.51.100.7:5000` (not Traefik) | `1.2.3.4` | `198.51.100.7` |
| unset | `T` | `1.2.3.4, 203.0.113.9` | `203.0.113.9` |
| unset | `T` | (none) | `172.18.0.2` |
| `192.0.2.0/24` | `T` | `203.0.113.9, 192.0.2.10` | `203.0.113.9` |
| `192.0.2.0/24` | `T` | `6.6.6.6, 203.0.113.9, 192.0.2.10, 192.0.2.11` | `203.0.113.9` |
| `192.0.2.0/24` | `T` | `192.0.2.10, 192.0.2.11` (all trusted) | `192.0.2.10` |
| `192.0.2.0/24` | `T` | `6.6.6.6, 203.0.113.9` (peer not trusted) | `203.0.113.9` |
| all | `T` | `203.0.113.9, 198.51.100.1` | `203.0.113.9` |
| all | `T` | `198.51.100.1` | `198.51.100.1` |
| unset | `T` | `1.2.3.4, 203.0.113.9:4711` | `203.0.113.9` |
| unset | `T` | `1.2.3.4, [2001:db8::1]:4711` | `2001:db8::1` |
| `192.0.2.0/24` | `T` | `203.0.113.9, garbage, 192.0.2.10` | `192.0.2.10` (the walk ends at the last good entry) |
| unset | `T`, Traefik unknown (`ok` false) | `203.0.113.9` | `172.18.0.2` |
| unset | `[::ffff:172.18.0.2]:9` (IPv4-mapped) | `203.0.113.9` | `203.0.113.9` |

  Also: several `X-Forwarded-For` header lines are read as one list in order; `ParseTrusted` turns `192.0.2.7` into `192.0.2.7/32` and refuses `nonsense`.
- [ ] **Step 4:** Run, see FAIL; implement; run `go test ./internal/githttp/`. Expected: PASS.
- [ ] **Step 5: Failing limiter tests** with an injected clock:
  - ten `Fail` calls from one address are not limited; the eleventh is, with `wait` between 59 and 60 seconds;
  - one minute later one more is allowed, then limited again; after eleven minutes of quiet, ten are allowed;
  - `2001:db8:1:2::1` and `2001:db8:1:2:ffff::9` share an allowance; `2001:db8:1:3::1` has its own;
  - `::ffff:203.0.113.9` and `203.0.113.9` share one;
  - another address is unaffected;
  - after 10,001 distinct addresses have each failed once, the map holds at most 10,000 and the one touched longest ago is the one that went;
  - concurrent `Fail` calls pass under `go test -race`.
- [ ] **Step 6:** Run, see FAIL; implement on the model of `internal/server/backoff.go` (a mutex, the injected clock, a capped map). An address back at its full ten is dropped from the map. Run `go test -race ./internal/githttp/`. Expected: PASS.
- [ ] **Step 7:** `go build ./... && go vet ./...`. Commit:

```
feat(githttp): the client address, the failed-attempt limit, and one role check for both transports
```

---

### Task 6: The transport, its routes, and the body cap's exception

**Files:**
- Create: `internal/githttp/handler.go`, `internal/githttp/handler_test.go`, `internal/githttp/main_test.go`
- Modify: `internal/server/pushtokens.go` or a new `internal/server/githttp.go` (options, the plain-HTTP test, the routes' wiring), `internal/server/routes.go`, `internal/server/server.go` (`limitBodies`, `Handler`), with tests in `internal/server/githttp_test.go`
- Modify: `main.go` (`startGitPush` reports the bridge; Traefik's address; the setter; the startup line), `main_test.go`

**Interfaces:**
- Consumes: Task 2 (`FindPushToken`, `TouchPushToken`, `core.PushToken.Expired`), Task 4 (`githook.EnvTokenID`, `githook.EnvClientAddr`), Task 5 (`pushauth.Allowed`, `Proxies`, `Limiter`), `gitrepo.Manager` (`Path`, `Exists`, `Init`), `core.ValidAppName`, `core.DefaultProjectID`, `core.SourcePush`.
- Produces, in `internal/githttp/handler.go`:

```go
type Store interface {
	pushauth.Store
	FindPushToken(ctx context.Context, raw string) (core.PushToken, bool, error)
	TouchPushToken(ctx context.Context, id int64, at time.Time, ip string) error
	GetAppByName(ctx context.Context, name string) (core.App, error)
}

type Options struct {
	Store         Store
	Repos         *gitrepo.Manager
	PublicURL     string        // https://outhaul.example.com, no trailing slash
	MaxDays       int           // OUTHAUL_PUSH_TOKEN_MAX_DAYS
	SSOMaxAge     time.Duration // OUTHAUL_PUSH_TOKEN_SSO_MAX_AGE
	Limiter       *Limiter
	ClientAddr    func(*http.Request) netip.Addr
	OnPublicHost  func(*http.Request) bool
	PlainHTTP     func(*http.Request) bool // known to be plain HTTP; nil means never
	StreamContext func(*http.Request) (context.Context, context.CancelFunc)
	Ready         func() bool          // the hook bridge is listening; nil means yes
	Now           func() time.Time     // nil means time.Now
	KeepAlive     int                  // seconds; 0 means 5
}
func New(o Options) *Handler
func (h *Handler) InfoRefs(w http.ResponseWriter, r *http.Request) // GET /git/{repo}/info/refs
func (h *Handler) Service(w http.ResponseWriter, r *http.Request)  // POST /git/{repo}/git-receive-pack and /git-upload-pack
```

- Produces, in `internal/server`: `GitHTTPOptions` gains `ClientAddr func(*http.Request) netip.Addr` and `BridgeReady func() bool`; routes `GET /git/{repo}/info/refs`, `POST /git/{repo}/git-receive-pack`, `POST /git/{repo}/git-upload-pack`, all `levelPublic`, only when `s.gitHTTPOn()`; `func (s *Server) gitPlainHTTP(r *http.Request) bool`.
- Produces, in `main.go`: `startGitPush` also returns whether the bridge is listening; `func traefikAddr(cfg config.Config, dc docker.Client) func(context.Context) (netip.Addr, bool)`.

**The order of checks** (spec, "Signing in"), the same for all three requests:

1. `Ready()` false: 503 `outhaul: git push is not available; see the server log`.
2. Not `OnPublicHost`: 404, no challenge. `PlainHTTP`: 403, no challenge.
3. `info/refs` whose `service` isn't `git-receive-pack` or `git-upload-pack`: 404 (the dumb protocol), no challenge. A `POST` whose `Content-Type` isn't `application/x-git-<service>-request`: 415.
4. No `Authorization: Basic`: 401 and the challenge. Not a failed attempt.
5. The password, trimmed of ASCII whitespace, is a live token: right shape, found, not `Expired(now, MaxDays)`. Otherwise `Limiter.Fail(addr)`: limited gives 429 with `Retry-After` in whole seconds, rounded up, and no challenge; not limited gives 401 and the challenge. A token of the wrong shape is never passed to the store.
6. `TouchPushToken(id, now, addr)`.
7. The owner is an SSO account whose `LastSignInAt` is nil or older than `SSOMaxAge`: 403.
8. The name (`{repo}` with one trailing `.git` removed) fails `core.ValidAppName`: 404 `outhaul: that isn't a valid app name`.
9. `pushauth.Allowed(owner, project)` false, where the project is the app's, or `core.DefaultProjectID` when no app has the name: 403.
10. The app exists and isn't a push app: 404. A fetch or clone of a name with no app: 404, nothing created.
11. `Repos.Init` when the repository doesn't exist (a push only, or an existing push app), then git.

**The messages**, each `text/plain; charset=utf-8`, one line ending in a newline, with `<url>` as `PublicURL`:

| When | Body |
|---|---|
| Plain HTTP | `outhaul: this remote uses http://. Change it to <url>/git/<app>.git` |
| No credentials | `outhaul: sign in with any username and a push token as the password. Create one at <url>/account` |
| Unknown or wrong-shaped token | `outhaul: that push token isn't valid. Create one at <url>/account` |
| Expired token | `outhaul: that push token expired on <2 January 2006>. Create a new one at <url>/account` |
| Limited | `outhaul: too many failed sign-ins from your address. Try again in <wait>.` |
| SSO sign-in too old | `outhaul: your last SSO sign-in to Outhaul was more than <age> ago. Sign in at <url>/login, then push again.` |
| Role | `outhaul: you don't have permission to use '<app>'` |
| Not a push app | `outhaul: app '<app>' is not push-deployable` |
| No such app, on a fetch | `outhaul: there is no app called '<app>'` |

`<age>` is whole days when the setting is whole days ("7 days"), otherwise the duration. The plain-HTTP line names the app only when the name is valid; otherwise it ends at `<url>/git/`.

**Running git:**

- `info/refs`: `git -c receive.keepAlive=N -c uploadpack.keepAlive=N <receive-pack|upload-pack> --stateless-rpc --advertise-refs <dir>`. Body: the pkt-line `# service=git-<service>\n` and `0000`, then git's output; with `Git-Protocol: version=2` on an upload-pack request, git's output alone. `Content-Type: application/x-git-<service>-advertisement`.
- `POST`: the same command without `--advertise-refs`; the request body is stdin (through `gzip.NewReader` when `Content-Encoding: gzip`; a body that isn't gzip answers 400 before git starts), stdout is the response, flushed after every write with `http.ResponseController`. `Content-Type: application/x-git-<service>-result`.
- Environment: `PATH`, `HOME`, `GIT_PROTOCOL` from `Git-Protocol` when it holds only letters, digits, `=` and `:`; on `git-receive-pack` also `githook.EnvTokenID=<row id>` and `githook.EnvClientAddr=<address>`.
- Every answer, refusals included, carries `Expires: Fri, 01 Jan 1980 00:00:00 GMT`, `Pragma: no-cache`, `Cache-Control: no-cache, max-age=0, must-revalidate`.
- The command runs under `StreamContext(r)`; when the context ends the child is killed and waited for. Git's stderr goes to the log, at most five lines a request. A refusal is logged at most once a minute per kind, with the client address and never the credentials.

- [ ] **Step 1: The test harness.** `internal/githttp/main_test.go`:

```go
// The hook gitrepo writes runs `<bin> git-hook <app> <socket>`. In these
// tests <bin> is this test binary, so a push reaches a real bridge.
func TestMain(m *testing.M) {
	if len(os.Args) == 4 && os.Args[1] == "git-hook" {
		code, err := githook.RunHookFrom(os.Args[3], os.Args[2], githook.OriginFromEnv(os.Getenv), os.Stdin, os.Stdout)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
		}
		os.Exit(code)
	}
	os.Exit(m.Run())
}
```

  And in `handler_test.go` a helper `newHarness(t)` that skips when `git` is missing and builds: a real store; `gitrepo.New(dir, os.Args[0], sock)`; a `githook.Bridge` served on `sock` with `SetPublicURL`; a fake worker goroutine as `TestRunHookOverUnixSocketDeploys` has, which marks each new deployment running and closes its log; a `Handler` behind `httptest.NewServer` on a mux with the three patterns, with `PublicURL` the test server's URL, `OnPublicHost` comparing `r.Host` with it, `ClientAddr` from a header the test sets (`X-Test-Addr`, default `203.0.113.9`), and a clock the test can move. A helper `git(t, dir, token, args...)` runs the real client with `HOME` and `GIT_CONFIG_GLOBAL` in a temp dir, `GIT_CONFIG_NOSYSTEM=1`, `GIT_TERMINAL_PROMPT=0`, and a `GIT_ASKPASS` script that prints the token, and returns its combined output and error.

- [ ] **Step 2: Failing tests**, the brief's acceptance list:
  - `TestPushDeploys`: a system admin's token; `git push <url>/git/api.git main` to an existing push app succeeds; a deployment row exists for the app; the output holds `→ build log: ` and `✓ deployed`; the token's `LastUsedAt` and `LastUsedIP` (`203.0.113.9`) are set.
  - `TestRefused`, a table driving raw requests to `info/refs?service=git-receive-pack` (and one `git push` to check what git prints): no credentials gives 401, the challenge header exactly `Basic realm="outhaul"`, and the body above; the account's own password as the Basic password gives 401 and a counting store wrapper saw no `FindPushToken` call; an unknown well-formed token 401; an expired one 401 with "expired on"; one created 31 days ago with a far expiry and `MaxDays` 30: 401; a revoked one 401; a valid session cookie and no `Authorization` 401 (make the session with the store, as the server tests do); a project member who isn't on the app's project 403, a viewer who is 403; a name with no app, pushed by a project member not on the default project: 403, and afterwards `Repos.Exists` is false and `GetAppByName` finds nothing; an SSO account whose `last_sign_in_at` is 8 days old 403 with the SSO body, and after `TouchUserSignIn` 200 with the same token; an SSO account that never signed in 403; a compose-from-GitHub app (not `SourcePush`) 404; another `Host` 404 with no `WWW-Authenticate`; `PlainHTTP` returning true 403 with no `WWW-Authenticate`; `Ready` false 503; `info/refs` with no `service` 404; a `POST` with `Content-Type: text/plain` 415.
  - `TestRefusalsHaveNoCacheHeaders`: the three headers on a 401 and on a 200.
  - `TestFirstPushCreatesTheApp`: with the bridge built with a naming suffix (`core.Naming{DomainSuffix…}` as `bridge_test.go` builds one) and `protectNew` true, a system admin's first push of `newapp` creates the app with host `newapp.<suffix>` and its domain row requires sign-in.
  - `TestCloneAndFetch`: after a push, `git clone` gives the pushed commit, with `-c protocol.version=2` and with `-c protocol.version=0`; a second push then `git fetch` in the clone brings the new commit. A clone of a name with no app fails with "there is no app called", and `Repos.Exists` stays false.
  - `TestProtocolV2Advertisement`: a raw `info/refs?service=git-upload-pack` with `Git-Protocol: version=2` has no `# service=` line; without the header it starts `001e# service=git-upload-pack\n0000`.
  - `TestQuietHookKeepsTheResponseMoving`: `KeepAlive: 1`; the pushed repository's `hooks/post-receive` replaced by a script that sleeps 4 seconds and prints nothing; the handler wrapped so each `Write` to the response is timestamped; during the push no gap between writes is longer than 2.5 seconds.
  - `TestFailedAttemptsAreLimitedByAddress`: ten bad tokens from `203.0.113.9` each give 401; the eleventh gives 429 with a `Retry-After` between 1 and 60 and no `WWW-Authenticate`; a good token from `203.0.113.9` still gets 200; a bad token from `198.51.100.4` gets 401; a request with no credentials from `203.0.113.9` still gets 401, not 429.
  - `TestNoTokenInAnyLogLine`: `log.SetOutput` to a buffer across a push, a clone, and each refusal above made with a real token value where one applies; the buffer contains neither any token's value, nor its last 64 characters, nor its SHA-256 hex, nor the Basic header's base64.
  - `TestPastedTokenWithWhitespace` (Review Focus 2): the password `" <token>\n"` gets 200.
  - `TestOddRepoNames`: `App.git`, `a..b.git`, `x.git.git`, `-x.git`, and a 300-character name, each with a good token: 404, and nothing is created under the git root.
  - `TestGzipRequestBody` (Review Focus 5): a `git-upload-pack` `POST` whose body is the gzip of a body that works unpacked gets the same answer; a body that says gzip and isn't gets 400.
  - `TestClientGoingAwayEndsGit` (Review Focus 4): a `git-receive-pack` `POST` whose body never ends, cancelled after the handler has started git (a hook or a channel in the test tells when): the handler returns within two seconds and no `git` child of the test process is left (check the process the handler started has been waited for; expose that to the test with an unexported hook if needed).
- [ ] **Step 3:** Run `go test ./internal/githttp/`. Expected: FAIL to compile.
- [ ] **Step 4:** Implement `handler.go` to the order, messages and commands above. Run `go test -race ./internal/githttp/`. Expected: PASS.
- [ ] **Step 5: Failing server tests** in `internal/server/githttp_test.go`:
  - `TestGitRoutesAbsentWhenOff`: `GET /git/api.git/info/refs?service=git-upload-pack` answers 404 on `newTestEnv`.
  - `TestGitRoutesPresentWhenOn`: on a server with `SetGitHTTP` and `SetRepos`, the same request with `Host: slip.example.com` answers 401 with the challenge; with a signed-in session cookie and no `Authorization` it still answers 401.
  - `TestGitRoutesPassCrossOriginProtection`: a `POST /git/api.git/git-receive-pack` with `Sec-Fetch-Site: cross-site` is refused by the cross-origin check (403), and one without it reaches the handler (401).
  - `TestGitPushBodyIsNotCapped` (Review Focus 1): through `srv.Handler()` behind `httptest`, a real `git push` of a commit holding a 3 MiB file of random bytes, with `-c http.postBuffer=65536` so the body is chunked, succeeds, and the bare repository has the commit. In the same test, `POST /login` with a 2 MiB body still answers 413.
  - `TestGitPlainHTTP`: `gitPlainHTTP` is true only with `tlsEnabled`, the tunnel off, and `X-Forwarded-Proto: http`; false with the header `https`, with no header, with the tunnel on, with `tlsEnabled` false, and when the tunnel setting can't be read.
  - `TestRouteCensus`: if a test lists every route's level, the three new ones are there as public.
- [ ] **Step 6:** Run, see FAIL; implement. `SetGitHTTP` keeps the options; `Handler()` builds the `githttp.Handler` (the store, `s.repos`, `s.publicURL`, a new `Limiter`, `s.onPublicHost`, `s.gitPlainHTTP`, `s.streamContext`) and `routes()` appends the three routes. `limitBodies` becomes a method that leaves a `POST` to `/git/{repo}/git-receive-pack` or `/git-upload-pack` alone when git over HTTPS is on, and caps everything else as today.
- [ ] **Step 7: `main.go`.** `startGitPush` returns the SSH server and whether the bridge is listening. `traefikAddr` returns `cfg.TraefikIP` when set, and otherwise asks `dc.ContainerIP(ctx, "outhaul-traefik", cfg.Network)`, remembering an answer (or a failure) for a minute. `SetGitHTTP` gets `ClientAddr` from a `githttp.Proxies` built from `cfg.TrustedProxies` (`ParseTrusted`), `cfg.TrustAllProxies` and `traefikAddr`, and `BridgeReady`. `serve` logs `uploadTimeoutHint(cfg.GitHTTP, cfg.UploadTimeout)` when it isn't empty. Tests in `main_test.go`: `TestTraefikAddr` (the configured address wins; the Docker answer is used and asked for once within a minute, with `docker.Fake`; an error gives `ok` false).
- [ ] **Step 8:** Run `go test ./internal/server/`, `go test -race ./internal/githttp/`, `go test .`. Expected: PASS.
- [ ] **Step 9:** `go build ./... && go vet ./...`. Commit:

```
feat(githttp): git push, fetch and clone over HTTPS with a push token
```

---

### Task 7: The app page, and the docs

**Files:**
- Modify: `internal/server/pushdeploy.go`, `internal/server/handlers.go` (the app page's data), `internal/server/templates/app.tmpl`, `internal/server/templates/account.tmpl` (the push-keys text), with tests in `internal/server/githttp_test.go`
- Create: `docs/GIT-HTTP.md`
- Modify: `README.md`, `docs/BEHIND-A-PROXY.md`, `docs/SSO.md`, `docs/STATE-BACKUP.md`, `ARCHITECTURE.md`, `AGENTS.md`

**Interfaces:**
- Consumes: `s.gitHTTPOn()`, `s.sshAddr()`, `s.pushRemote(app)`.
- Produces: `func (s *Server) pushHTTPSRemote(app core.App) string` (`<public URL>/git/<name>.git`, `""` when off); template data `PushHTTPSRemote`.

- [ ] **Step 1: Failing tests** `TestAppPagePushRemotes`, for a push app seen by someone who can change it:

| git over HTTPS | SSH server | The page shows |
|---|---|---|
| off | running | the SSH lines, exactly as today; no `/git/` |
| on | running | `git remote add outhaul https://slip.example.com/git/<app>.git`, the token note, and the SSH lines |
| on | off (`sshAddr() == ""`) | the HTTPS lines and note; no `ssh://` |
| off | off | what today's page shows |

  A viewer sees neither block. A tag-deploy app shows the tag line under the HTTPS lines too. `TestAccountPushKeysTextNamesHTTPS`: with both on, the Account page's push-keys paragraph also names the `https://…/git/` remote; with git over HTTPS off its text is today's, byte for byte. The note reads: "Git asks for a username and a password. Type any username, and a push token from Account → Push tokens as the password. Don't put the token in the remote's address."
- [ ] **Step 2:** Run, see FAIL; implement; run `go test ./internal/server/ -run 'AppPagePushRemotes|AppDetail|Push'`. Expected: PASS. Then the whole package.
- [ ] **Step 3: `docs/GIT-HTTP.md`**, in the style of `docs/BEHIND-A-PROXY.md`. Sections, each from the spec:
  1. What it is, and turning it on (`OUTHAUL_GIT_HTTP=on`, what it needs, the four settings, `OUTHAUL_UPLOAD_TIMEOUT`).
  2. Creating a token; it is shown once; expiry; the 25-token and label rules.
  3. Pushing: the remote, any username, the token as the password. Storing it: `git config --global credential.helper osxkeychain`, Git Credential Manager, `credential.helper store` with what that file is, and `cache`. Why not `https://user:token@…` (shell history, `.git/config`, process lists, proxy logs).
  4. In CI: a `GIT_ASKPASS` script that prints a secret variable; what an expired token looks like (`remote: outhaul: that push token expired on …`), and that tokens always expire.
  5. What each refusal means: the messages table, and that git forgets the token after a 401 and keeps it after a 403.
  6. SSO accounts: the rule, the setting, the message.
  7. Revoking; what a system admin sees; what removes tokens (deleting the account, removing or changing SSO, a restore).
  8. Failed attempts: ten, one a minute, per address; a good token is never held back.
  9. The build in your terminal: lines as they are written, git's keep-alive every five seconds, the build log's address first, and the limit of `OUTHAUL_HEALTH_TIMEOUT` plus five minutes on following a deploy.
  10. Behind a CDN: redirect HTTP to HTTPS for the admin host; don't cache or buffer `/git/`; a WAF that inspects bodies may need an exclusion for `/git/`; `OUTHAUL_UPLOAD_TIMEOUT`; `git config http.postBuffer` for a CDN that refuses a chunked upload; `OUTHAUL_TRUSTED_PROXIES` so a token's last use shows the pusher's address.
  11. What is the same as SSH and what isn't (a clone of a name with no app creates nothing; fetches aren't logged).
  12. What hasn't been checked on a real host: the spec's nine items.
- [ ] **Step 4: The other pages.** `README.md`: rows for `OUTHAUL_GIT_HTTP` and the three token settings, and a line in the push-to-deploy section pointing at the new page. `docs/BEHIND-A-PROXY.md`: a pointer. `docs/SSO.md`: the SSO rule, and that removing or changing SSO deletes SSO accounts' tokens. `docs/STATE-BACKUP.md`: the "What a rebuild loses" row for sessions also names push tokens ("everyone signs in again and creates new push tokens"). `ARCHITECTURE.md`: "Push-to-deploy" gains the HTTPS transport, the package layout gains `githttp/` and `pushauth/`, and the locked "Auth" entry gains "A push token signs `git` in over HTTPS and nothing else (opt-in)." `AGENTS.md`, "Decisions to keep": one entry "Push tokens" (Basic only, never a cookie or password; hashed; always expire; 401 for a bad token so git forgets it, 403 for the SSO rule and the role so it keeps it; a live token is never held back by the limit; stripped from state backups; deleted when SSO sign-ins are ended), one sentence that Outhaul believes `X-Forwarded-For` only from its own Traefik and only `githttp` reads it, a sentence under "Login" that failed push tokens are counted per address, and `OUTHAUL_UPLOAD_TIMEOUT` under "Behind a TLS-terminating proxy". Under "Build, run, test" nothing changes unless Task 8 adds a flag.
- [ ] **Step 5:** Check every relative link in the changed pages resolves (`grep -o '](\([^)#]*\)' ` and `ls`), and that no page holds a string matching `ohp_[0-9a-f]{64}`.
- [ ] **Step 6:** `go build ./... && go vet ./...`, `go test ./internal/server/`. Commit:

```
feat(app): show the HTTPS remote, and document git over HTTPS
```

---

### Task 8: End to end through the stand-in CDN

**Files:**
- Modify: `deploy/test/integration_proxy.sh`, and `deploy/test/Dockerfile.proxytest` only if `git` or a credential helper is missing from the image

**Interfaces:**
- Consumes: the whole feature. The script already runs a real Outhaul and Traefik behind nginx with a required header, creates a push app through the stand-in, and seeds its bare repository by hand.

- [ ] **Step 1:** Read the script from the top. Add `OUTHAUL_GIT_HTTP=on` and `OUTHAUL_UPLOAD_TIMEOUT=10m` to the environment its Outhaul starts with, beside the other `OUTHAUL_*` lines.
- [ ] **Step 2:** Add a section `== git push over HTTPS`, after the app exists and before the script's teardown, using the script's own `pass`, `fail`, `die` and `ce` helpers:
  1. Create a token: `POST $public/account/push-tokens` with `label=proxytest&days=1` and the admin cookie jar; take the one `ohp_[0-9a-f]{64}` from the response into a shell variable inside the container (never echo it).
  2. Put it in a credential helper: `git config --global credential.helper store` and a `~/.git-credentials` line for the public host written with `printf`, mode 600. Point git at the stand-in with `git config --global http.curloptResolve` or the script's existing `--connect-to` equivalent (`-c http.curloptResolve=$publichost:443:127.0.0.1`, with the stand-in's port), and its CA with `http.sslCAInfo`.
  3. `git push $public/git/app.git main` from `/work/repo` with one new commit: exit 0, and the output holds `build log:` and the deploy's result. Pass: "a token in a credential helper pushes and deploys through the stand-in".
  4. `git clone $public/git/app.git /work/clone`: the new commit is its head. Pass: "git clone works through the stand-in".
  5. The same `info/refs` request sent straight to Traefik with no required header: 404. Pass: "git without the required header matches no router".
  6. With a wrong token in the helper: `git push` fails and prints `that push token isn't valid`.
  7. The Outhaul log inside the container (the script already knows where it is) holds the push's log line with `proxytest`, and no `ohp_`.
- [ ] **Step 3:** `shellcheck -s sh --severity=warning deploy/test/*.sh` (CI's version is 0.9.0: `docker run --rm -v "$PWD:/mnt" koalaman/shellcheck:v0.9.0 -s sh --severity=warning deploy/test/integration_proxy.sh`). Expected: no findings.
- [ ] **Step 4:** Run `sh deploy/test/integration_proxy.sh`. It builds a privileged container with its own Docker daemon and takes several minutes. Expected: every line `ok`, the new ones included. If the machine can't run it (no privileged containers), say so in the report with the error, and don't claim it passed.
- [ ] **Step 5:** If the run found a defect in the feature, fix it test-first in the package that owns it, and say what it was.
- [ ] **Step 6:** Commit:

```
test(proxy): push and clone over HTTPS through the stand-in CDN
```

---

## After the last task

- `go build ./... && go vet ./...`
- `for p in $(go list ./...); do go test "$p" | tail -1; done`
- `for p in $(go list ./...); do go test -race -timeout 30m "$p" | tail -1; done` (`internal/server` takes more than ten minutes under `-race`)
- `sh deploy/test/run.sh` and the shellcheck line from `AGENTS.md`, since `deploy/` changed
- `git status` shows no `outhaul` binary and nothing untracked that shouldn't be
