# Protected Routes Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A domain row can require sign-in, so someone building an app doesn't need to write their own auth. This is part 3 of 3 and needs parts 1 and 2 (`2026-11-02-accounts-roles.md`, `2026-11-02-sso-login.md`).

**Architecture:** A protected row adds a Traefik forward-auth middleware, defined in that app's labels, which calls Outhaul's `/gate/check`. A browser with no sign-in goes to Outhaul to sign in, then returns with a one-time code that becomes a cookie for that host only. Row status comes from the labels on the running container, so the UI can't disagree with Traefik.

**Tech Stack:** Go 1.26, Traefik v3.7.6 forward auth, SQLite.

**Spec:** `docs/superpowers/specs/2026-11-02-sso-roles-protected-routes-design.md`

## Global Constraints

- Test first. Commits look like `feat(scope): summary`, and every commit message ends with `Co-Authored-By: Claude Fable 5.2 <noreply@anthropic.com>`.
- The gate is defined **in labels**, never in the file provider. A router referencing an undefined `@file` middleware falls through to an open router, which was tested on `traefik:v3.7.6`.
- Header name: `X-Outhaul-User`. Reserved query parameter: `__outhaul_code`. Route cookie: `__Host-outhaul_route`. Gateway cookie: `__Host-outhaul_gate` (`Path=/`, `Secure`, no `Domain`) when the public URL is `https://`, so an app on a sibling subdomain can't plant one; `outhaul_gate` when it is `http://`, which only local development uses. The choice follows the public URL, not the request: in tunnel mode a request looks like plain HTTP.
- Route sessions and gateway sessions last 8 hours. One-time codes last 60 seconds and work once.
- Tokens and codes are stored as SHA-256 hex only.
- Long-lived handlers aren't involved: `/gate/check` must answer quickly and never stream.
- The UI says "SSO", and the switch reads exactly `Require sign-in`.
- This migration is `0028`. Existing rows default to open, and no app redeploys on upgrade.
- Every new route goes in `routes()` with a level. A new public route is also added to `TestPublicRoutesAreExactlyThese`, and a new path wildcard to the value map in `TestEveryRouteRefusesTheWrongPeople` (both in `internal/server/access_test.go`).
- Docker: only Tasks 1 and 8 start containers, and only ones named `outhaul-gatetest-*`. Nothing touches `outhaul-traefik` or the `outhaul` network.
- A one-time code is only ever sent to a URL the gate will answer. Whoever holds an unredeemed code can sign in as that person on that host.

## Review Focus

- A stopped app has its switch turned on, then someone presses Restart: the old container would come back public, so Restart deploys instead. Pinned in Task 5.
- The switch changes while another deploy is queued or building: reusing the last image would run after that deploy and silently put the old code back, so it follows what's in flight instead. Pinned in Task 5.
- During a cutover, an old container without the gate runs next to a new one with it: the row doesn't show as protected until the old one has gone. Pinned in Task 6.
- `/gate/start` receives a URL for an unprotected host, `http://`, `//evil.example`, a host with a port or different case, or no URL at all: refused unless its normalized host has a protected row. Pinned in Task 4.
- A row is turned off while the app still trusts the header: the open route strips a forged `X-Outhaul-User`. Pinned in Tasks 1 and 8.
- `/gate/start` receives a URL on a protected host whose path belongs to an open row, maybe another app's: refused, or the code would reach that app unredeemed. Pinned in Tasks 1 and 4.
- A request for `https://wiki.example.com//evil.example/` reaches the gate from someone signed in: the redirect stays on `wiki.example.com`. Pinned in Task 3.

---

### Task 1: The column and the labels

**Files:**
- Create: `internal/store/migrations/0028_protected_routes.sql`, `deploy/test/integration_gate.sh`, `deploy/test/gatelabels/main.go`
- Modify: `internal/core/app.go`, `internal/store/domains.go`, `internal/traefik/labels.go`, `internal/compose/override.go`, `internal/deploy/pipeline.go`, `internal/deploy/pipeline_compose.go`, `internal/config/config.go`
- Test: `internal/traefik/labels_test.go`, `internal/compose/compose_test.go`, `internal/store/domains_test.go`, `internal/config/config_test.go`

**Interfaces:**
- Produces:
  - `core.Domain.Protected bool`
  - `(*Store) RouteIsProtected(ctx, host, urlPath string) (bool, error)`: looks at every app's rows on that host (compared lower-cased) whose `path` is a plain string prefix of `urlPath`, the way Traefik's `PathPrefix` matches, and reports whether the one with the longest path is protected. No matching row gives false.
  - `(*Store) SetDomainProtected(ctx, appID, domainID int64, protected bool) error`
  - `func (c config.Config) GateAddress() string`: `"http://host.docker.internal:" + c.AdminPort() + "/gate/check"`
  - `traefik.RouteLabels(router, host string, port int, urlPath, internalPath string, tlsEnabled bool, certResolver string, protected bool, gateAddress string) map[string]string`
  - `traefik.AppLabels(app, domains, port, globalTLS, certResolver, gateAddress string)`, `compose.Override(app, domains, network, tlsEnabled, certResolver, gateAddress string)`
  - `func traefik.RowHasGate(labels map[string]string, router string) bool`
  - `const traefik.IdentityHeader = "X-Outhaul-User"`

- [ ] **Step 1: Write the failing tests**

```go
func TestProtectedRouteDefinesItsGateInLabels(t *testing.T) {
	l := RouteLabels("r", "wiki.example.com", 8080, "", "", true, "le", true, "http://host.docker.internal:8080/gate/check")
	want := map[string]string{
		"traefik.http.middlewares.r-gate.forwardauth.address":             "http://host.docker.internal:8080/gate/check",
		"traefik.http.middlewares.r-gate.forwardauth.authresponseheaders": "X-Outhaul-User",
		"traefik.http.routers.r.middlewares":                              "r-gate",
		"traefik.http.routers.r-tls.middlewares":                          "r-gate",
	}
	// assert each key/value
	// no value anywhere contains "@file"
}

func TestGateRunsBeforePathRewrites(t *testing.T) {
	l := RouteLabels("r", "h", 8080, "/a", "/", true, "", true, "addr")
	// routers.r.middlewares == "r-gate,r-strip"
}

func TestOpenRouteStripsIdentityHeader(t *testing.T) {
	l := RouteLabels("r", "h", 8080, "", "", false, "", false, "addr")
	// middlewares.r-noident.headers.customrequestheaders.X-Outhaul-User == ""  (key present)
	// routers.r.middlewares == "r-noident"; no forwardauth label
	// With a path rewrite: "r-noident,r-strip"
}

func TestRowHasGate(t *testing.T) {
	// True for a protected row's labels, with and without TLS.
	// False for an open row's labels and for a missing router.
	// False if the gate is listed on r but not r-tls, or listed with no forward-auth address label.
}

func TestProtectedMigrationKeepsRowsOpen(t *testing.T) {
	// openMigratedBefore(t, "0028") with a domain row: after migrating, Protected is false.
}

func TestComposeOverrideCarriesGate(t *testing.T) {
	// A protected row gives the service the forwardauth labels; the empty strip value renders as "" in YAML.
}

func TestRouteIsProtected(t *testing.T) {
	// Rows: wiki.example.com "" protected; wiki.example.com "/webhooks" open, on ANOTHER app;
	// shop.example.com "" open; shop.example.com "/admin" protected.
	// wiki "/" and "/page" => true;  wiki "/webhooks", "/webhooks/x", "/webhooksfoo" => false
	// shop "/admin", "/admin/x", "/administrator" => true;  shop "/" and "/cart" => false
	// "WIKI.example.com" "/page" => true;  an unknown host => false
}

func TestGateAddress(t *testing.T) { /* ListenAddr ":9000" => ".../host.docker.internal:9000/gate/check" */ }
```

Update existing label and pipeline assertions for the new `r-noident` middleware and the changed signatures.

The labels then have to be proved on real Traefik in this task, before seven more tasks are built on them. An empty label value is how the header is removed, and whether Traefik's Docker provider keeps an empty value has to be seen, not assumed. Write `deploy/test/integration_gate.sh`: POSIX `sh`, not run by CI, cleaning up with a `trap`. Every container and network it creates is named `outhaul-gatetest-*`. It starts `traefik:v3.7.6` on a loopback port, an echo backend, and a small `python:3-slim` gate stub on a scratch Docker network. Labels come from `deploy/test/gatelabels/main.go`, which prints `traefik.RouteLabels` output, so the test covers the real label code. Assert:
- a protected path under an open host returns the stub's 302 with its `Set-Cookie` reaching the client;
- a 200 from the stub forwards `X-Outhaul-User: dana`, and a client-sent forged value is replaced;
- an open row strips a client-sent `X-Outhaul-User`, and still serves;
- `/webhooks` open under a protected host reaches the backend with no gate call, and so does `/webhooksfoo` (this pins the prefix rule `RouteIsProtected` copies);
- the stub receives the browser's `Cookie` and `Accept` headers, and `X-Forwarded-Host`, `-Uri` and `-Method` that a client-sent value can't override;
- with the stub stopped, a protected path returns 500 and an open path still returns 200.

If Traefik drops the empty header value, don't work round it silently: report it, because the design for stripping the header has to change.

- [ ] **Step 2: Run the tests and watch them fail**

Run: `go test ./internal/traefik/ ./internal/compose/ ./internal/store/ ./internal/config/`
Expected: compile errors.

- [ ] **Step 3: Implement**

Migration: `ALTER TABLE domains ADD COLUMN protected INTEGER NOT NULL DEFAULT 0`, plus the tables Task 2 uses: `gate_sessions(token_hash PK, subject, kind, display, created_at, expires_at)`; `route_sessions(token_hash PK, gate_token_hash REFERENCES gate_sessions ON DELETE CASCADE, host, display, expires_at)`; `gate_codes(code_hash PK, gate_token_hash REFERENCES gate_sessions ON DELETE CASCADE, host, expires_at)`. Add `protected` to `domainCols`, `scanDomain`, `addDomainTx` and `UpdateDomain`. In `RouteLabels`, the first middleware is `-gate` or `-noident`, then `rewriteMiddlewares` as today. The worker passes `w.cfg.GateAddress()` at both call sites.

- [ ] **Step 4: Run the tests and watch them pass**

Run: `go test ./internal/... && sh deploy/test/integration_gate.sh`
Expected: PASS, and `docker ps -a --filter name=outhaul-gatetest` shows nothing left over.

Run: `docker run --rm -v "$PWD:/mnt" koalaman/shellcheck:v0.9.0 -s sh --severity=warning /mnt/deploy/test/integration_gate.sh`
Expected: no output.

- [ ] **Step 5: Commit**

```bash
git add internal deploy/test/integration_gate.sh deploy/test/gatelabels
git commit -m "feat(traefik): protected rows carry a forward-auth gate in their labels"
```

### Task 2: Gateway and route sessions in the store

**Files:**
- Create: `internal/store/gate.go`
- Modify: `internal/store/users.go`, `internal/store/auth.go`, `internal/store/sso.go`, `internal/core/user.go`
- Test: `internal/store/gate_test.go`

**Interfaces:**
- Produces:
  - `core.GateSession{ Subject, Kind, Display string; ExpiresAt time.Time }`; `core.SignInSSO` and `core.SignInPassword` are used as `Kind`
  - `func (u core.User) GateSubject() string`: `"user:<id>"` for a password account, `core.SSOGateSubject(u.SSOIssuer, u.SSOSubject)` for SSO
  - `func core.SSOGateSubject(issuer, subject string) string`: `"sso:<issuer>|<subject>"`, also used for route visits, which have no account
  - `(*Store) CreateGateSession(ctx, rawToken string, sess core.GateSession) error`: also deletes expired gateway sessions
  - `(*Store) GetGateSession(ctx, rawToken string) (core.GateSession, error)`
  - `(*Store) DeleteGateSession(ctx, rawToken string) error`, `(*Store) DeleteGateSessionsForSubject(ctx, subject string) error`, `(*Store) DeleteGateSessionsOfKind(ctx, kind string) error`
  - `(*Store) CreateGateCode(ctx, rawCode, rawGateToken, host string, expiresAt time.Time) error`
  - `(*Store) RedeemGateCode(ctx, rawCode, host, rawRouteToken string, routeExpires time.Time) (display string, err error)`: one transaction that checks the code, deletes it, and creates the route session
  - `(*Store) RouteSessionUser(ctx, rawToken, host string) (display string, err error)`: `sql.ErrNoRows` if unknown, expired, for another host, or its gateway session has been deleted or has expired

- [ ] **Step 1: Write the failing tests**

```go
func TestCodeWorksOnceOnItsOwnHost(t *testing.T) {
	// Code for wiki.example.com. Redeem on docs.example.com => sql.ErrNoRows, and the code still works on its own host once.
	// A second redeem => sql.ErrNoRows.
}
func TestCodeExpires(t *testing.T) { /* expiresAt in the past => sql.ErrNoRows */ }
func TestRouteCookieWorksOnlyOnItsHost(t *testing.T) {
	// RouteSessionUser(token, "wiki...") => "dana"; (token, "docs...") => sql.ErrNoRows.
}
func TestEndingGatewaySessionEndsRouteSessions(t *testing.T) {
	// Each of DeleteGateSession, DeleteGateSessionsForSubject and DeleteGateSessionsOfKind
	// makes RouteSessionUser return sql.ErrNoRows.
}
func TestRouteSessionCantOutliveItsGatewaySession(t *testing.T) {
	// The route session has 8h left and its gateway session has expired => sql.ErrNoRows.
	// This is what keeps "removed at the provider loses access within 8 hours" true.
}
func TestRemovingAccountEndsItsGateSessions(t *testing.T) { /* password and SSO accounts, by GateSubject */ }
func TestChangingPasswordEndsGateSessions(t *testing.T) {
	// Each of UpdateUserPassword, CompleteSetupLink, ResetUserPassword and EndUserSessions.
}
func TestEndSSOSessionsEndsGateSessions(t *testing.T) { /* SSO kind gone, password kind kept */ }
func TestGateTokensAreHashed(t *testing.T) { /* raw values absent from the three tables */ }
```

- [ ] **Step 2: Run the tests and watch them fail**

Run: `go test ./internal/store/ -run 'TestCode|TestRouteCookie|TestEnding|TestRemovingAccount|TestChangingPassword|TestEndSSO|TestGateTokens'`
Expected: compile errors.

- [ ] **Step 3: Implement**

`DeleteUser`, `UpdateUserPassword`, `CompleteSetupLink`, `ResetUserPassword` and `EndUserSessions` delete by `GateSubject()` in their existing transactions. `EndSSOSessions` also deletes SSO-kind gateway sessions. Share one `hashToken` with setup links.

- [ ] **Step 4: Run the tests and watch them pass**

Run: `go test ./internal/store/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/store internal/core
git commit -m "feat(store): gateway sessions, host-bound route sessions and one-time codes"
```

### Task 3: `/gate/check`

**Files:**
- Create: `internal/server/gate.go`
- Modify: `internal/server/routes.go`
- Test: `internal/server/gate_check_test.go`

**Interfaces:**
- Consumes: `RedeemGateCode`, `RouteSessionUser`.
- Produces:
  - Route at `levelPublic`: `GET /gate/check`
  - `const gateCodeParam = "__outhaul_code"`, `const routeCookie = "__Host-outhaul_route"`, `const gateSessionTTL = 8 * time.Hour`
  - `func normalizeHost(h string) string`: lower-cased, port removed
  - `func (s *Server) gateStartURL(returnURL string) string`

- [ ] **Step 1: Write the failing tests**

Call the endpoint directly with `X-Forwarded-Host`, `X-Forwarded-Uri`, `X-Forwarded-Method` and `Accept`, as Traefik would.

```go
func TestGateCheckPassesAValidCookie(t *testing.T) {
	// 200, X-Outhaul-User == display.
}
func TestGateCheckNormalizesHost(t *testing.T) { /* "WIKI.example.com:443" matches a cookie for wiki.example.com */ }
func TestGateCheckRefusesCookieFromAnotherHost(t *testing.T) { /* treated as no cookie */ }
func TestGateCheckRedirectsBrowsers(t *testing.T) {
	// No cookie, GET, Accept text/html => 302 to https://slip.example.com/gate/start?rd=<escaped https://wiki.example.com/page?x=1>
}
func TestGateCheckBuildsHTTPSEvenBehindPlainProto(t *testing.T) { /* X-Forwarded-Proto: http => rd still https:// */ }
func TestGateCheckGives401ToNonBrowsers(t *testing.T) {
	// Each => 401, no Location: POST with Accept text/html; GET with Accept application/json; GET with Upgrade: websocket.
}
func TestGateCheckRedeemsCode(t *testing.T) {
	// X-Forwarded-Uri "/page?x=1&__outhaul_code=C" => 302 to "https://wiki.example.com/page?x=1" with Set-Cookie:
	// name __Host-outhaul_route, Path=/, Secure, HttpOnly, SameSite=Lax, no Domain.
	// Max-Age matches 8h.
}
func TestGateCheckBadCodeRestartsSignIn(t *testing.T) {
	// A reused, expired or other-host code: no cookie, 302 to gate/start with rd lacking __outhaul_code.
	// The same bad code with a valid route cookie: 302 to the URL without the code, no sign-in.
}
func TestGateCheckFailsClosedWithoutPublicURL(t *testing.T) { /* publicURL "" and no cookie => 503 */ }
func TestGateCheckNeverRedirectsOffTheHost(t *testing.T) {
	// X-Forwarded-Uri "//evil.example/?__outhaul_code=C" and "/\\evil.example/?__outhaul_code=C",
	// each with a good code, and with a bad code plus a valid cookie:
	// Location starts with "https://wiki.example.com/" every time.
	// The parameter given twice is removed both times.
}
func TestGateCheckRepliesArentCached(t *testing.T) { /* every 302 and 401 carries Cache-Control: no-store */ }
```

- [ ] **Step 2: Run the tests and watch them fail**

Run: `go test ./internal/server/ -run TestGateCheck`
Expected: FAIL with 404.

- [ ] **Step 3: Implement in this order**

A code parameter is handled first. Then the cookie. Then the browser test: method `GET` or `HEAD`, `Accept` containing `text/html`, and no `Upgrade` header. Traefik may drop `Upgrade` before calling the gate, so the `Accept` test has to catch WebSockets by itself: browsers don't send `text/html` on a WebSocket handshake. Every redirect to the app's host is absolute, built as `"https://" + normalizeHost(host) + uri`, never the bare URI: a path beginning `//` would be read as another site. Never log tokens or codes. Trust `X-Forwarded-Host`, `-Uri` and `-Method` because Traefik overwrites them; this is safe because a caller who reaches the port directly gains nothing without a valid cookie for that host.

- [ ] **Step 4: Run the tests and watch them pass**

Run: `go test ./internal/server/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/server
git commit -m "feat(gate): answer Traefik's forward-auth check"
```

### Task 4: Signing in and out for a route

**Files:**
- Modify: `internal/server/gate.go`, `internal/server/auth.go`, `internal/server/sso_login.go`, `internal/server/routes.go`, `internal/server/templates/login.tmpl`, `internal/server/templates/noaccess.tmpl`, `internal/server/templates/users.tmpl`
- Create: `internal/server/templates/gate_signout.tmpl`
- Test: `internal/server/gate_signin_test.go`

**Interfaces:**
- Consumes: Task 2's store methods, `RouteIsProtected`, `ssoFlow.Return`.
- Produces:
  - Routes at `levelPublic`: `GET /gate/start`, `POST /gate/sign-in`, `GET /gate/sign-out`, `POST /gate/sign-out`
  - `func (s *Server) gateCookieName() string`: `__Host-outhaul_gate` when the public URL is `https://`, else `outhaul_gate`, using the helper part 2 added for the SSO flow cookie; `const gateCodeTTL = 60 * time.Second`
  - `func (s *Server) validReturnURL(ctx, raw string) (host string, ok bool)`
  - `func (s *Server) startGateSession(w, r, subject, kind, display string) (rawToken string, err error)`
  - `func (s *Server) redirectWithCode(w, r, rawGateToken, returnURL string)`

- [ ] **Step 1: Write the failing tests**

```go
func TestGateStartRefusesBadReturnURLs(t *testing.T) {
	// Each => 400 "This address isn't protected by Outhaul": empty; http://wiki.example.com/;
	// //evil.example/; https://unprotected.example.com/; https://evil.example/wiki.example.com;
	// https://wiki.example.com@evil.example/.
	// https://WIKI.example.com:443/x is accepted.
}
func TestGateStartRefusesOpenPathsOnAProtectedHost(t *testing.T) {
	// wiki.example.com protected, with /webhooks open on another app; shop.example.com open with /admin protected.
	// Refused: https://wiki.example.com/webhooks/x, https://shop.example.com/, https://shop.example.com/cart,
	// https://shop.example.com/admin/../cart, https://shop.example.com/admin/%2e%2e/cart,
	// https://shop.example.com/admin%2f..%2fcart, https://shop.example.com/admin\..\cart.
	// Accepted: https://wiki.example.com/page, https://shop.example.com/admin/users?tab=1.
	// Refused even with a valid outhaul_gate cookie: no code is minted.
}
func TestGateStartShowsLoginNamingTheDestination(t *testing.T) {
	// Body has "Sign in to continue to", "wiki.example.com", and the SSO button when connected.
}
func TestPasswordSignInForRouteReturnsWithCode(t *testing.T) {
	// A RoleNone password account. POST /gate/sign-in with rd => 303 to https://wiki.../page?__outhaul_code=...
	// and outhaul_gate cookie; no sessions row is created.
	// A wrong password => 401 login page still naming the destination.
}
func TestExistingGatewaySessionSkipsThePrompt(t *testing.T) { /* GET /gate/start with outhaul_gate => straight 303 with a code */ }
func TestAdminLoginAlsoCoversRoutes(t *testing.T) {
	// POST /login creates outhaul_gate; then GET /gate/start => 303 with a code.
}
func TestSSORouteVisitCreatesNoAccount(t *testing.T) {
	// GET /auth/sso/start?rd=... then callback with a Fake identity => 303 with a code, gateway session Kind sso,
	// ListUsers unchanged, no sessions row.
}
func TestSSOStartRefusesBadReturnURL(t *testing.T) { /* unprotected rd => 400 */ }
func TestSSOCallbackChecksReturnURLAgain(t *testing.T) {
	// A flow cookie edited to return to https://wiki.example.com/webhooks/x (open): no code is minted.
	// A row turned off between start and callback: the same.
}
func TestSSOFailureOnRouteVisitKeepsDestination(t *testing.T) {
	// A flow with a return URL, then Exchange fails: 303 to /gate/start?rd=...&error=..., showing the error
	// and still naming the destination.
}
func TestSignOutEndsRouteAccess(t *testing.T) {
	// POST /gate/sign-out: outhaul_gate cleared; RouteSessionUser => sql.ErrNoRows.
	// GET /gate/sign-out shows a button and says SSO may sign you straight back in.
}
func TestLogoutEndsRouteAccess(t *testing.T) { /* POST /logout does the same, and ends the Outhaul session */ }
func TestRemovedAccountLosesRouteAccessAtOnce(t *testing.T) { /* POST /users/{id}/delete => /gate/check 302s */ }
```

- [ ] **Step 2: Run the tests and watch them fail**

Run: `go test ./internal/server/ -run 'TestGateStart|TestPasswordSignInForRoute|TestExistingGateway|TestAdminLogin|TestSSORouteVisit|TestSSOStartRefuses|TestSSOCallbackChecks|TestSSOFailureOnRouteVisit|TestSignOut|TestLogoutEnds|TestRemovedAccount'`
Expected: FAIL.

- [ ] **Step 3: Implement**

`validReturnURL` parses with `net/url`, requires scheme `https`, an empty `User`, and a non-empty host. It refuses a path the browser or Traefik might read differently from Outhaul: `u.EscapedPath()` containing `\`, `%2e`, `%2f` or `%5c` in either case, or a `u.Path` that `path.Clean` would change (a trailing slash aside). An empty path counts as `/`. Then it checks `RouteIsProtected(normalizeHost(u.Host), u.Path)`. `redirectWithCode` calls `validReturnURL` itself, so no caller can mint a code for a URL that wasn't checked. Move password checking out of `handleLoginSubmit` into a helper that `/gate/sign-in` also calls, including the hash slots and the refusal of SSO accounts. `s.login` also starts a gateway session. The SSO callback branches on `flow.Return`: when set, it starts a gateway session with subject `core.SSOGateSubject(issuer, subject)` and redirects with a code; when empty, it behaves as in part 2 and also starts a gateway session. A failed callback whose flow has a return URL goes back to `/gate/start`; one with no usable flow goes to `/login`. The gateway cookie is `HttpOnly`, `SameSite=Lax`, `Path=/`, 8 hours, and `Secure` when the public URL is `https://`.

Update copy now that routes exist: `noaccess.tmpl` adds "You can still use apps on protected routes.", and the Users note reads `"No role" means the person can pass protected routes but can't use Outhaul.`

- [ ] **Step 4: Run the tests and watch them pass**

Run: `go test ./internal/server/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/server
git commit -m "feat(gate): sign in and out for protected routes"
```

### Task 5: The switch, and applying a change

**Files:**
- Create: `internal/server/protection.go`
- Modify: `internal/server/handlers.go`, `internal/server/templates/domainform.tmpl`, `internal/server/templates/app.tmpl`
- Test: `internal/server/protection_test.go`

**Interfaces:**
- Consumes: `core.Domain.Protected`, `traefik.RowHasGate`, `traefik.RouterName`, `Runtime.ListContainers`, `store.CreateRollback`, `store.CreateDeployment`.
- Produces:
  - `func (s *Server) protectionAvailability(ctx, d core.Domain) (ok bool, reason string)`
  - `func (s *Server) appContainers(ctx, app core.App) ([]docker.Container, error)`: `ListContainers(ctx, {"outhaul.app": app.Name})`, stopped ones included
  - `func rowGate(containers []docker.Container, router string) (defined, gated bool)`: `defined` when any container has that router's rule label, and `gated` only when every container that defines it passes `traefik.RowHasGate`. A health-check container has no router labels, so it doesn't count.
  - `func (s *Server) queueProtectionDeploy(ctx, app core.App) (core.Deployment, error)`: chooses how to deploy, as set out in Step 3
  - `func (s *Server) applyProtection(ctx, app core.App) (*core.Deployment, error)`: calls `queueProtectionDeploy` only when a container is running
  - `func (s *Server) protectionWarning(ctx, app core.App) string`
  - `func protectionMismatch(domains []core.Domain, appName string, containers []docker.Container) bool`: for each row, `d.Protected != gated`, except that an open row with no router yet isn't a mismatch

- [ ] **Step 1: Write the failing tests**

```go
func TestAvailability(t *testing.T) {
	// Table (publicURL, tlsEnabled, tunnel, d.TLS => ok):
	// "https://x", true, false, true => true
	// "https://x", true, false, false => false, reason mentions HTTPS
	// "", true, false, true => false, reason mentions the public address
	// "https://x", false, false, true => false
	// "https://x", false, true, false => true  (the tunnel's edge terminates TLS)
}
func TestSavingRefusesUnavailableSwitch(t *testing.T) {
	// POST protected=on with no public URL => 400; row stays open.
	// Turning HTTPS off on a protected row, keeping the switch on => 400; row unchanged.
}
func TestUnavailableRowKeepsItsProtection(t *testing.T) {
	// A protected row, then availability lost. An edit that submits no switch value keeps Protected true,
	// and the page shows a warning.
}
func TestSwitchOnRunningAppQueuesImageReuse(t *testing.T) {
	// Nixpacks app with a running deployment image "img:1" and a running container.
	// Turn switch on => a rollback deployment with Image "img:1"; the deployer was notified.
}
func TestNoReusableImageRunsNormalDeploy(t *testing.T) {
	// Compose app, and a nixpacks app whose image is pruned: a normal deployment is queued.
}
func TestStoppedOrNeverDeployedQueuesNothing(t *testing.T) { /* no deployment row added */ }
func TestInFlightDeployIsNotUndone(t *testing.T) {
	// A running app on image "img:1":
	// a normal deployment queued, building or deploying, then the switch => a normal deployment, not a rollback to img:1.
	// a rollback to "img:0" queued, then the switch => a rollback to "img:0".
}
func TestWhichSavesQueueADeploy(t *testing.T) {
	// On a running app: add protected row => yes; add open row => no; switch off => yes;
	// edit host of a protected row => yes; edit path of a protected row => yes;
	// edit the port of an open row => no.
}
func TestRestartRedeploysOnMismatch(t *testing.T) {
	// Rows say protected, container labels lack the gate:
	// POST /restart => no StartContainer call; a deployment is queued. Matching labels => a normal restart.
	// A compose app likewise skips compose.Restart.
	// A STOPPED container that lacks the gate, with the switch on => a deployment, not StartContainer.
	// An open row whose labels predate this feature (no strip middleware) => a normal restart.
}
func TestWarningNamesWhatWillShip(t *testing.T) {
	// Compose on branch main => "This will deploy the latest code from main".
	// Tag mode pattern v* => "This will deploy the newest tag matching v*".
	// Nixpacks with a usable image => "". Template app => "" (it redeploys its snapshot).
}
func TestDomainDialogHasSwitch(t *testing.T) {
	// The app page has "Require sign-in", step label "3 · HTTPS and sign-in",
	// and the warning text from protectionWarning. When unavailable the input is disabled with the reason.
}
```

- [ ] **Step 2: Run the tests and watch them fail**

Run: `go test ./internal/server/ -run 'TestAvailability|TestSavingRefuses|TestUnavailableRow|TestSwitchOn|TestNoReusable|TestStopped|TestInFlight|TestWhichSaves|TestRestartRedeploys|TestWarningNames|TestDomainDialog'`
Expected: FAIL.

- [ ] **Step 3: Implement**

Availability: `reachable := s.publicURL != "" && (s.tlsEnabled || tunnelOn)`, and `visitorHTTPS := tunnelOn || (d.TLS && s.tlsEnabled)`.

`queueProtectionDeploy` reads `ListDeploymentsForApp` (newest first) and picks the first that applies:

1. Compose app: `CreateDeployment`.
2. The newest queued, building or deploying row is a rollback (`RollbackOf != 0`): `CreateRollback` with that same image, so the rollback isn't undone.
3. A normal deploy is queued, building or deploying: `CreateDeployment`. Deployments run in order, so reusing the last image here would replace the new code with the old.
4. The newest deployment with `Status == core.StatusRunning`, a non-empty `Image` and `!ImagePruned`: `CreateRollback` with that image.
5. Otherwise: `CreateDeployment`.

It then calls `s.deployer.Notify()`. `handleAddDomain` and `handleUpdateDomain` call `applyProtection` under the rules in `TestWhichSavesQueueADeploy`, then `syncCerts`. `handleRestartApp` calls `appContainers` and `protectionMismatch` before `appLifecycle`, and on a mismatch calls `queueProtectionDeploy` and redirects to that deployment.

The dialog copy comes from the mockup. The switch uses the mockup's CSS, added to `static/app.css`. The inline script shows the warning when the switch is on, or when an edited row was protected. Without JS the warning stays visible, so nothing ships unseen. `openDomainDialog` carries a `protected` value, and the edit button in `app.tmpl` passes it.

- [ ] **Step 4: Run the tests and watch them pass**

Run: `go test ./internal/server/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/server
git commit -m "feat(domains): a switch to require sign-in, applied by redeploying"
```

### Task 6: What the row shows

**Files:**
- Modify: `internal/server/protection.go`, `internal/server/handlers.go`, `internal/server/domains_page.go`, `internal/server/templates/app.tmpl`, `internal/server/templates/domains.tmpl`
- Test: `internal/server/protection_status_test.go`

**Interfaces:**
- Consumes: `appContainers`, `rowGate`.
- Produces:
  - `type accessState struct { Label, Class, Note string; DeploymentID int64 }`
  - `func accessStateFor(d core.Domain, appName string, containers []docker.Container, latest *core.Deployment) accessState`: uses only the running containers, and "not running" means there are none

- [ ] **Step 1: Write the failing test**

```go
func TestAccessStateFor(t *testing.T) {
	// Setting on,  running, has gate           => "Sign-in required", state-running
	// Setting on,  running, lacks gate, latest queued/building/deploying
	//                                          => "Not protected yet", state-building, Note "redeploying", DeploymentID set
	// Setting on,  running, lacks gate, latest failed
	//                                          => "Not protected yet", Note "the last deploy failed", DeploymentID set
	// A new row (router absent from labels), setting on => "Not protected yet"
	// Setting off, running, has gate           => "Sign-in required", Note "until the redeploy finishes"
	// Setting off, running, lacks gate         => "Open", state-stopped
	// Either setting, not running              => the setting's label, Note "applies on the next deploy"
	// Setting on, two running containers during a cutover, only the newer has the gate => "Not protected yet"
}

func TestAppPageAndDomainsPageShowAccess(t *testing.T) {
	// fakeRuntime.stack has labels with the gate for one row and not another: both pages show
	// "Sign-in required" and "Not protected yet", from the labels, not the column.
	// Docker unreachable (ListContainers error): the row shows "Unknown", never "Sign-in required".
}
```

- [ ] **Step 2: Run the tests and watch them fail**

Run: `go test ./internal/server/ -run 'TestAccessStateFor|TestAppPageAndDomainsPage'`
Expected: compile error.

- [ ] **Step 3: Implement**

Add an Access column to the Domains tables in `app.tmpl` and `domains.tmpl`, linking `DeploymentID` to `/deployments/{id}`. `handleDomainsList` makes one `ListContainers(ctx, {"outhaul.managed": "true"})` call and groups running containers by `outhaul.app`. Replace "Domain changes apply on this app's next deploy." with the mockup's: "Changing this redeploys the app. Other domain changes apply on this app's next deploy."

- [ ] **Step 4: Run the tests and watch them pass**

Run: `go test ./internal/server/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/server
git commit -m "feat(domains): show protection from the running container's labels"
```

### Task 7: Previews inherit protection

**Files:**
- Modify: `internal/previewmgr/manager.go`, `internal/server/protection.go`, `internal/server/handlers.go`
- Test: `internal/previewmgr/manager_test.go`, `internal/server/protection_test.go`

**Interfaces:**
- Consumes: `applyProtection`, `SetDomainProtected`, `ListPreviewsForParent`.
- Produces: `func (s *Server) protectPreviews(ctx, parent core.App) error`

- [ ] **Step 1: Write the failing tests**

```go
func TestMintDomainsCopiesProtection(t *testing.T) { /* a protected parent row => a protected preview row */ }
func TestCollapsedPreviewRowIsProtectedIfAnyParentRowIs(t *testing.T) {
	// Two parent rows with different hosts, the second protected, collapse to one preview row: Protected true,
	// whichever comes first.
}
func TestTurningParentOnProtectsExistingPreviews(t *testing.T) {
	// Parent with a preview with a matching Service+Path row. Turn the parent's switch on, or add a protected row:
	// the preview's row is Protected and a preview deployment is queued.
}
func TestTurningParentOffLeavesPreviews(t *testing.T) { /* the preview stays protected; a preview made later is open */ }
```

- [ ] **Step 2: Run the tests and watch them fail**

Run: `go test ./internal/previewmgr/ ./internal/server/ -run 'TestMintDomains|TestCollapsed|TestTurningParent'`
Expected: FAIL.

- [ ] **Step 3: Implement**

In `mintDomains`, build the deduplicated rows first, OR-ing `Protected` per derived key, then add them. `protectPreviews` matches a child's row to a parent's protected row by `Service` and `Path`; the preview host derives only from those and the PR, so the match is exact.

- [ ] **Step 4: Run the tests and watch them pass**

Run: `go test ./internal/previewmgr/ ./internal/server/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/previewmgr internal/server
git commit -m "feat(previews): previews inherit protection from their parent"
```

### Task 8: Check against real Traefik, and write it down

**Files:**
- Create: `docs/PROTECTED-ROUTES.md`
- Modify: `deploy/test/integration_gate.sh`, `ARCHITECTURE.md`, `AGENTS.md`, `README.md`, `docs/SSO.md`

- [ ] **Step 1: Point the Traefik test at the real gate**

Task 1 wrote `deploy/test/integration_gate.sh` against a stub. Add a second mode, `sh deploy/test/integration_gate.sh --outhaul <address>`, that uses a running Outhaul's `/gate/check` in place of the stub, and asserts:
- a browser-style `GET` with no cookie gets a 302 to `/gate/start` on the public URL;
- a `POST`, and a `GET` with `Accept: application/json`, get a 401;
- a WebSocket handshake with no cookie gets a 401, which shows whether Traefik passes `Upgrade` to the gate or the `Accept` test is what catches it;
- `GET //evil.example/` with a valid route cookie stays on the host.

- [ ] **Step 2: Run both modes, and shellcheck it**

Run: `sh deploy/test/integration_gate.sh`, then the `--outhaul` mode against the Outhaul started in Step 3.
Expected: every assertion passes, and `docker ps -a --filter name=outhaul-gatetest` shows nothing left over.

Run: `docker run --rm -v "$PWD:/mnt" koalaman/shellcheck:v0.9.0 -s sh --severity=warning /mnt/deploy/test/integration_gate.sh`
Expected: no output.

- [ ] **Step 3: Run the whole flow once with a real provider**

Outhaul manages a container named `outhaul-traefik` on the host's Docker daemon, and the maintainer's machine may already be using one. Run `docker ps -a --filter name=outhaul-traefik` first. If it's there, stop and get the maintainer's OK before going on.

Use the Dex setup from part 2, and a real app with a TLS domain, ideally on the local CA. Confirm in a browser:
- visiting the protected route goes through SSO and reaches the app;
- the app sees `X-Outhaul-User`;
- a second protected host needs no second prompt;
- an open path under the protected host can't be used as the return address at `/gate/start`;
- `/gate/sign-out` ends access to both;
- turning the switch off redeploys and leaves the route open;
- a tunnel-mode request works, if a tunnel is available.

Record what was and wasn't checked in the task's report. Don't claim a check that wasn't run.

- [ ] **Step 4: Write `docs/PROTECTED-ROUTES.md`**

Cover: what the switch does; the `X-Outhaul-User` contract, and that it's only trustworthy through Traefik; the open-path pattern and the protected-path-under-open-host pattern; `/gate/sign-out`; the 401 for scripts; the requirements; what happens when Outhaul restarts; the warning that everyone who can sign in reaches every protected route; that the app can read the route cookie, which works only on its own host; that the name in the header is whatever the provider sends.

- [ ] **Step 5: Update the other docs**

`ARCHITECTURE.md`: add the gate to the package layout and describe the flow. `AGENTS.md` "Decisions to keep": add a **Protected routes** entry: the gate lives in labels, never `@file`, because an undefined middleware falls through to an open route; row status comes from container labels; each open row strips the identity header. `README.md`: add the feature. `docs/SSO.md`: link to the new page.

- [ ] **Step 6: Run everything**

Run: `go vet ./... && go test -race ./... && sh deploy/test/run.sh`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add deploy/test/integration_gate.sh docs ARCHITECTURE.md AGENTS.md README.md
git commit -m "docs(gate): protected routes, checked against real Traefik"
```
