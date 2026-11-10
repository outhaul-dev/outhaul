# Accounts and Roles Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let several people use one Outhaul server, with roles that stop accidental changes through the UI. This is part 1 of 3; it ships alone, with password accounts only.

**Architecture:** Every HTTP route moves into a table that states its access level and how to find its project. One guard enforces that table, and a test fails if a route has no level. A person has one role, read from the database on every request, plus a list of projects shared with them.

**Tech Stack:** Go 1.26, `net/http`, `html/template`, SQLite (`modernc.org/sqlite`), `golang.org/x/crypto/ssh`.

**Spec:** `docs/superpowers/specs/2026-11-02-sso-roles-protected-routes-design.md`

## Global Constraints

- Test first: write the failing test, watch it fail for the right reason, then fix.
- Migrations are numbered, embedded and never edited after shipping. This plan adds `0027`.
- No JS build step. No web framework. CGO stays off.
- Role labels are exactly: `System admin`, `Project admin`, `Project member`, `Project viewer`, `No role`.
- Commits look like `feat(scope): summary`. The body says what the user saw go wrong, and what changed. End every commit message with `Co-Authored-By: Claude Fable 5.2 <noreply@anthropic.com>`.
- `go vet ./...` and `go test -race ./...` pass before a task is called done.
- The repository is public: don't describe unfixed security issues in `AGENTS.md` or `ARCHITECTURE.md`.
- Every new route goes in `routes()` with a level. A new public route is also added to `TestPublicRoutesAreExactlyThese`, and a new path wildcard to the value map in `TestEveryRouteRefusesTheWrongPeople` (both in `internal/server/access_test.go`).

## Review Focus

- A person with a role but no shared projects opens every list page and the create-app form: pages render empty, and `POST /apps` with an empty `project_id` is refused instead of landing in the default project. Pinned in Task 4.
- A system admin removes or demotes the last password system admin, including themselves: refused with the reason on the page, and nothing changes. Pinned in Tasks 1 and 6.
- A viewer opens an app, project or database page: no secret value appears anywhere in the HTML, not just hidden with CSS. Pinned in Task 5.
- A setup link is reused, expired, or replaced by a newer reset: the page says it no longer works, and the password is unchanged. Pinned in Tasks 2 and 6.
- Someone is demoted while signed in, or their push key's owner is demoted: the very next request or push is refused. Pinned in Tasks 3 and 8.

---

### Task 1: Roles and accounts in the store

**Files:**
- Create: `internal/store/migrations/0027_accounts.sql`, `internal/core/role.go`, `internal/store/users.go`
- Modify: `internal/core/user.go`, `internal/core/pushkey.go`, `internal/store/auth.go`, `internal/store/pushkeys.go`
- Test: `internal/core/role_test.go`, `internal/store/users_test.go`

**Interfaces:**
- Produces:
  - `core.Role` (`string`) with constants `RoleNone = "none"`, `RoleProjectViewer = "project_viewer"`, `RoleProjectMember = "project_member"`, `RoleProjectAdmin = "project_admin"`, `RoleSystemAdmin = "system_admin"`
  - `func (r core.Role) AtLeast(min core.Role) bool`, `func (r core.Role) Label() string`, `func core.ParseRole(s string) (core.Role, bool)`, `var core.AssignableRoles []core.Role` (highest last)
  - `core.User` gains `Role core.Role`, `SignIn string` (`core.SignInPassword = "password"`, `core.SignInSSO = "sso"`), `SSOIssuer`, `SSOSubject`, `Email string`, `LastSignInAt *time.Time`, and `func (u core.User) Display() string` (email for SSO accounts, otherwise username)
  - `core.UserRow{core.User; ProjectCount int}`; `core.PushKey` gains `UserID int64` (0 means no owner; the column is nullable, so scan it with `sql.NullInt64`)
  - `store.ErrLastPasswordAdmin`
  - `(*Store) CreatePasswordAccount(ctx, username string, role core.Role) (core.User, error)`
  - `(*Store) ListUsers(ctx) ([]core.UserRow, error)`: SSO accounts with no role first, then by display name
  - `(*Store) SetUserRole(ctx, id int64, role core.Role) error`
  - `(*Store) DeleteUser(ctx, id int64) error`
  - `(*Store) EndUserSessions(ctx, id int64) error`
  - `(*Store) TouchUserSignIn(ctx, id int64) error`
  - `(*Store) ListPushKeysForUser(ctx, userID int64) ([]core.PushKey, error)`, `(*Store) DeletePushKeyForUser(ctx, userID, id int64) error`

- [ ] **Step 1: Write the failing tests**

```go
// internal/core/role_test.go
func TestRoleAtLeast(t *testing.T) {
	if !core.RoleSystemAdmin.AtLeast(core.RoleProjectViewer) { t.Fatal("admin >= viewer") }
	if core.RoleProjectViewer.AtLeast(core.RoleProjectMember) { t.Fatal("viewer < member") }
	if core.RoleNone.AtLeast(core.RoleProjectViewer) { t.Fatal("none < viewer") }
	if core.Role("bogus").AtLeast(core.RoleNone) { t.Fatal("unknown role has no rank") }
}

// internal/store/users_test.go
func TestAccountsMigrationKeepsExistingAdmin(t *testing.T) {
	db := openMigratedBefore(t, "0027")
	// seed: users(id 3,'admin','h',now) and one push_keys row
	// after migrate(db):
	//   users.role == "system_admin", sign_in == "password"
	//   push_keys.user_id == 3
}

func TestLastPasswordAdminCannotBeRemovedOrDemoted(t *testing.T) {
	// CreateUser("admin") is a system admin.
	// SetUserRole(admin, RoleProjectViewer) and DeleteUser(admin) => errors.Is(err, ErrLastPasswordAdmin); unchanged.
	// Add an SSO system admin by direct insert: the same two calls are still refused.
	// CreatePasswordAccount("second", RoleSystemAdmin): now both calls succeed on admin.
}

func TestDeleteUserRemovesSessionsAndKeys(t *testing.T) {
	// Account with one session and one push key. After DeleteUser: GetSession => sql.ErrNoRows,
	// ListPushKeysForUser is empty, GetUser => sql.ErrNoRows.
}

func TestPasswordAccountCannotSignInBeforeSetup(t *testing.T) {
	// CreatePasswordAccount("reception", RoleNone): PasswordHash == "" and Role == RoleNone.
}

func TestGetUserByNameFindsOnlyPasswordAccounts(t *testing.T) {
	// A directly inserted SSO row with username "sso:x": GetUserByName("sso:x") => sql.ErrNoRows.
}
```

- [ ] **Step 2: Run the tests and watch them fail**

Run: `go test ./internal/core/ ./internal/store/ -run 'TestRole|TestAccounts|TestLastPassword|TestDeleteUser|TestPasswordAccount|TestGetUserByName'`
Expected: compile errors for the missing names.

- [ ] **Step 3: Write `0027_accounts.sql`**

Add to `users`: `role TEXT NOT NULL DEFAULT 'none'`, `sign_in TEXT NOT NULL DEFAULT 'password'`, `sso_issuer TEXT NOT NULL DEFAULT ''`, `sso_subject TEXT NOT NULL DEFAULT ''`, `email TEXT NOT NULL DEFAULT ''`, `last_sign_in_at TEXT`. Then `UPDATE users SET role = 'system_admin'`, and a unique index on `(sso_issuer, sso_subject) WHERE sign_in = 'sso'`.

Create `project_members (project_id REFERENCES projects(id) ON DELETE CASCADE, user_id REFERENCES users(id) ON DELETE CASCADE, created_at TEXT NOT NULL, PRIMARY KEY (project_id, user_id))`.

Create `setup_links (token_hash TEXT PRIMARY KEY, user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE, expires_at TEXT NOT NULL, created_at TEXT NOT NULL)`.

Add `push_keys.user_id INTEGER REFERENCES users(id) ON DELETE CASCADE`, then `UPDATE push_keys SET user_id = (SELECT MIN(id) FROM users)`.

`sessions.user_id` has no cascade, so `DeleteUser` deletes sessions first, in one transaction.

- [ ] **Step 4: Implement the store and core changes**

`CreateUser` keeps its signature and now inserts a **system admin**: it is the first-boot path. `GetUserByName` adds `AND sign_in = 'password'`. Put one `userCols` constant and a `scanUser` helper in `users.go` and use them everywhere. The guard in `SetUserRole` and `DeleteUser` runs in the same transaction as the change: refuse when the target is a password system admin, the change removes that, and no other password system admin exists. `AddPushKey` writes `UserID`. Remove `ListPushKeys` once Task 7 stops using it.

- [ ] **Step 5: Run the tests and watch them pass**

Run: `go test ./internal/core/ ./internal/store/`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/core internal/store
git commit -m "feat(store): accounts with roles, keeping the existing admin"
```

### Task 2: Project membership and setup links in the store

**Files:**
- Modify: `internal/store/users.go`, `internal/store/projects.go`, `internal/store/deployments.go`
- Test: `internal/store/users_test.go`

**Interfaces:**
- Consumes: Task 1's types.
- Produces:
  - `(*Store) AddProjectMember(ctx, projectID, userID int64) error` (idempotent), `(*Store) RemoveProjectMember(ctx, projectID, userID int64) error`
  - `(*Store) IsProjectMember(ctx, projectID, userID int64) (bool, error)`
  - `(*Store) ListProjectMembers(ctx, projectID int64) ([]core.User, error)`: leaves out system admins, even if they were members before being promoted
  - `(*Store) ListShareableUsers(ctx, projectID int64) ([]core.User, error)`: accounts with a project role who aren't on the project
  - `(*Store) ProjectIDsForUser(ctx, userID int64) ([]int64, error)`
  - `(*Store) CreateProjectWithMember(ctx, p core.Project, userID int64) (core.Project, error)`
  - `(*Store) CreateSetupLink(ctx, userID int64, rawToken string, expiresAt time.Time) error`: replaces that user's earlier links; stores SHA-256 hex only
  - `(*Store) SetupLinkUser(ctx, rawToken string) (core.User, error)`: `sql.ErrNoRows` if unknown or expired
  - `(*Store) CompleteSetupLink(ctx, rawToken, passwordHash string) (core.User, error)`: one transaction that checks the link, sets the hash, deletes the link and ends the user's sessions
  - `(*Store) ResetUserPassword(ctx, id int64, rawToken string, expiresAt time.Time) error`: one transaction that applies the last-admin guard, blanks the password hash, ends the user's sessions and replaces their link
  - `(*Store) ListRecentDeploymentsInProjects(ctx, projectIDs []int64, limit int) ([]core.Deployment, error)`, `(*Store) CountDeploymentsInProjects(ctx, projectIDs []int64) (int, error)`

- [ ] **Step 1: Write the failing tests**

```go
func TestProjectMembersCascadeWithProject(t *testing.T) {
	// Add member, DeleteProject (empty): IsProjectMember => false, ProjectIDsForUser empty.
}

func TestListShareableUsersOnlyHasProjectRoles(t *testing.T) {
	// Accounts: admin (system), m (member, already on project), v (viewer), n (no role).
	// ListShareableUsers(project) names == ["v"].
}

func TestSystemAdminsArentListedOnProjects(t *testing.T) {
	// A member promoted to system admin no longer appears in ListProjectMembers.
}

func TestResetStopsTheOldPasswordAtOnce(t *testing.T) {
	// ResetUserPassword: hash is "", sessions are gone, an earlier link no longer works.
	// On the only password system admin => ErrLastPasswordAdmin; hash and sessions unchanged.
}

func TestSetupLinkWorksOnce(t *testing.T) {
	// CreateSetupLink(user, "raw", now+1h). SetupLinkUser("raw") finds user.
	// CompleteSetupLink("raw","h2"): user hash == "h2", their sessions are gone.
	// Second CompleteSetupLink("raw","h3") => sql.ErrNoRows, hash still "h2".
}

func TestSetupLinkExpiresAndIsReplaced(t *testing.T) {
	// expiresAt in the past: SetupLinkUser => sql.ErrNoRows.
	// CreateSetupLink("a") then CreateSetupLink("b"): "a" => sql.ErrNoRows, "b" works.
}

func TestSetupLinkTokenIsNotStoredInPlain(t *testing.T) {
	// SELECT token_hash FROM setup_links != "raw".
}

func TestRecentDeploymentsInProjects(t *testing.T) {
	// Two apps in two projects, one deployment each: querying project A returns A's only;
	// an empty projectIDs slice returns none and count 0, without a SQL error.
}
```

- [ ] **Step 2: Run the tests and watch them fail**

Run: `go test ./internal/store/ -run 'TestProjectMembers|TestListShareable|TestSystemAdminsArent|TestResetStops|TestSetupLink|TestRecentDeployments'`
Expected: compile errors.

- [ ] **Step 3: Implement the methods listed under Interfaces**

- [ ] **Step 4: Run the tests and watch them pass**

Run: `go test ./internal/store/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/store
git commit -m "feat(store): project membership and one-time setup links"
```

### Task 3: One guard and a route table

**Files:**
- Create: `internal/server/access.go`, `internal/server/routes.go`, `internal/server/templates/noaccess.tmpl`
- Modify: `internal/server/server.go`, `internal/server/auth.go`, every `s.render(` caller, `internal/server/render_test.go`
- Test: `internal/server/access_test.go`

**Interfaces:**
- Consumes: Tasks 1 and 2.
- Produces:
  - `type accessLevel int` with `levelUnset`, `levelPublic`, `levelAccount`, `levelAnyRole`, `levelMemberRole`, `levelProjectAdminRole`, `levelSystemAdmin`, `levelViewer`, `levelMember`, `levelProjectAdmin`, and `func (l accessLevel) projectScoped() bool` (true for the last three)
  - `type projectResolver func(r *http.Request) (projectID int64, found bool, err error)`
  - `type route struct { pattern string; level accessLevel; project projectResolver; handler http.HandlerFunc }`
  - `func (s *Server) routes() []route`
  - `func userFrom(r *http.Request) (core.User, bool)`: the signed-in account, from the request context
  - `func (s *Server) canChange(r *http.Request) bool`: role is project member or above
  - `func (s *Server) render(w http.ResponseWriter, r *http.Request, status int, page string, data map[string]any)`: also sets `Me`, `IsSystemAdmin`, `CanManage` (role is project admin or above), `CanChange` (member or above) and `HasRole` (viewer or above) on `data`
  - Test helpers `func (e *testEnv) signInAs(t *testing.T, name string, role core.Role, projects ...int64) (sessionToken string)`, which creates the account and session directly through the store so no password is hashed, plus `getAs(t, token, path)` and `postAs(t, token, path, form)`

- [ ] **Step 1: Write the failing tests**

```go
func TestEveryRouteStatesAnAccessLevel(t *testing.T) {
	for _, rt := range newTestEnv(t).srv.routes() {
		if rt.level == levelUnset {
			t.Errorf("%s has no access level", rt.pattern)
		}
		if rt.level.projectScoped() != (rt.project != nil) {
			t.Errorf("%s: project resolver doesn't match its level", rt.pattern)
		}
	}
}

// Walks every route in the table, substituting ids of seeded objects: a project with an
// app, a database, a deployment, a domain, a volume, an attachment and a backup.
// It only asserts refusals, so no handler with side effects runs.
func TestEveryRouteRefusesTheWrongPeople(t *testing.T) {
	// no cookie, any non-public route   => 303 to /login
	// RoleNone, above levelAccount      => 403 and the no-access page
	// outsider (member of no project), project-scoped route => 404
	// viewer on the project, levelMember or levelProjectAdmin => 403
	// member on the project, levelProjectAdmin                => 403
	// project admin, levelSystemAdmin                         => 403
}

func TestRoleChangeAppliesToTheNextRequest(t *testing.T) {
	// Member gets 200 on /apps/{id}; SetUserRole(RoleNone); the same session gets 403.
}

func TestChildObjectsMustBelongToTheApp(t *testing.T) {
	// A member of project A posts to /apps/{A's app}/... with an id belonging to project B's app:
	// domains/{id}/delete, volumes/{id}/delete, attachments/{id}/delete, previews/{id}/destroy.
	// B's objects are unchanged. The store already scopes these by app ID, and this keeps it that way.
}

func TestNoRolePersonSeesNoAccessPage(t *testing.T) {
	// GET / => 403, body contains "You don't have access to Outhaul yet" and has a Sign out form,
	// and no project or app name.
}

func TestSystemAdminKeepsEveryRoute(t *testing.T) {
	// env.login(t) (first-boot admin) GET /settings, /containers and /apps/{id} => 200.
}
```

- [ ] **Step 2: Run the tests and watch them fail**

Run: `go test ./internal/server/ -run 'TestEveryRoute|TestRoleChange|TestChildObjects|TestNoRolePerson|TestSystemAdminKeeps'`
Expected: compile errors.

- [ ] **Step 3: Move routes into `routes()` with these levels**

`Handler()` loops over `routes()` and registers `s.guard(rt)`. It panics on `levelUnset`, so a forgotten level can't ship. `requireAuth` is deleted. `GET /static/` becomes a route whose handler is the existing `StripPrefix` file server's `ServeHTTP`.

| Level | Resolver | Patterns |
|---|---|---|
| `levelPublic` | | `GET /healthz`, `GET /ca.pem`, `GET /static/`, `GET`/`POST /setup`, `GET`/`POST /login`, `POST /logout`, `GET /deployments`, `GET /github/callback`, `GET /github/setup`, `POST /webhooks/github`, `POST /webhooks/app/{token}` |
| `levelAnyRole` | | `GET /{$}`, `GET /projects`, `GET /apps`, `GET /databases`, `GET /domains` |
| `levelMemberRole` | | `GET /templates` |
| `levelProjectAdminRole` | | `POST /projects` |
| `levelSystemAdmin` | | `GET /containers`, `GET /metrics`, `GET /metrics/sample`, `GET /volumes`, `POST /volumes/reclaim`, `GET /settings`, every other `/settings/...` route, `GET /github/connect`, each placeholder page |
| `levelViewer` | `projectOfPath` | `GET /projects/{id}` |
| `levelViewer` | `projectOfApp` | `GET /apps/{id}`, `GET /apps/{id}/logs`, `GET /apps/{id}/stats` |
| `levelViewer` | `projectOfDatabase` | `GET /databases/{id}`, `GET /databases/{id}/logs` |
| `levelViewer` | `projectOfDeployment` | `GET /deployments/{id}`, `GET /deployments/{id}/logs` |
| `levelMember` | `projectOfPath` | `POST /projects/{id}/settings`, `/env`, `/env/delete`, `/databases` |
| `levelMember` | `projectOfApp` | every `POST /apps/{id}/...` route: deploy, settings, kind, source, domains (3), volumes (3), env (2), attachments (2), webhook/secret, previews (2), stop, restart, delete |
| `levelMember` | `projectOfDatabase` | `POST /databases/{id}/start`, `/stop`, `/settings`, `/delete` |
| `levelMember` | `projectOfDeployment` | `POST /deployments/{id}/cancel`, `/rollback` |
| `levelMember` | `projectOfBackup` | `POST /backups/{id}/run`, `/toggle`, `/delete`, `/restore`, `GET /backups/{id}/restore` |
| `levelMember` | `projectOfForm` | `POST /apps`, `POST /templates/{id}` |
| `levelMember` | `projectOfBackupForm` | `POST /backups` |
| `levelProjectAdmin` | `projectOfPath` | `POST /projects/{id}/delete` |

`projectOfForm` must read the form the way `handleCreateApp` and `handleDeployTemplate` do, so the project that's checked is the project that's used: `strings.TrimSpace(r.FormValue("project_id"))`, where empty means `core.DefaultProjectID` and anything non-numeric returns `found == false`. `projectOfBackupForm` reads `target_kind` and `target_id`. `projectOfBackup` goes through `b.TargetKind` and `b.TargetID`. A backup target that can't be found returns `found == false`.

- [ ] **Step 4: Implement `guard`**

In order: public routes run straight through. No valid session, or a session whose user can't be loaded: 303 to `/login`, as today. Put the user in the request context. Role is `RoleNone` above `levelAccount`: render `noaccess` with 403. A system admin passes every level without running the resolver, so handlers keep answering them exactly as they do today (`internal/server/projects_test.go:179` expects a 400 for an unknown project, not a 404). For anyone else on a project-scoped route: resolve the project. Not found, or `IsProjectMember` is false, returns 404. Role too low returns 403 with `http.Error`. A store error returns 500.

`noaccess.tmpl` uses `HideChrome` and the copy from the mockup: "You don't have access to Outhaul yet. Ask a system admin to give you a role." and "You can still use apps on protected routes." is omitted until part 3. It has a Sign out form, and a link to `/account` for password accounts.

- [ ] **Step 5: Change `render` to take `r`**

Add `r` to all 29 call sites; each already has `r` in scope. Add `noaccess` to the `pages` list in `parseTemplates`. Public pages have no user, so the new keys are their zero values.

- [ ] **Step 6: Run the tests and watch them pass**

Run: `go test ./internal/server/`
Expected: PASS, including every existing test, because `completeSetup` creates a system admin.

- [ ] **Step 7: Commit**

```bash
git add internal/server
git commit -m "feat(server): every route states who may use it"
```

### Task 4: Show people only their projects

**Files:**
- Modify: `internal/server/access.go`, `internal/server/overview.go`, `internal/server/projects.go`, `internal/server/handlers.go`, `internal/server/databases.go`, `internal/server/domains_page.go`, `internal/server/catalog.go`
- Test: `internal/server/access_lists_test.go`

**Interfaces:**
- Consumes: `userFrom`, Task 2's store methods.
- Produces:
  - `type projectScope struct { all bool; ids map[int64]bool }`, `func (p projectScope) allows(id int64) bool`, `func (p projectScope) idList() []int64`
  - `func (s *Server) scopeFor(r *http.Request) (projectScope, error)`
  - `func (s *Server) projectsFor(r *http.Request) ([]core.Project, error)`: replaces `s.store.ListProjects` wherever a page lists or offers projects

- [ ] **Step 1: Write the failing tests**

```go
func TestListPagesShowOnlySharedProjects(t *testing.T) {
	// Projects A (shared with the member) and B (not), each with an app, database and domain.
	// For /, /projects, /apps, /databases, /domains: body contains A's name, never B's.
	// / shows AppCount 1 and only A's recent deployments.
}

func TestPersonWithNoSharedProjectsSeesEmptyPages(t *testing.T) {
	// Member with no projects: all five pages => 200 with no project or app names.
	// POST /apps with empty project_id => 404; ListApps stays empty.
}

func TestCreateAppFormOffersOnlyTheirProjects(t *testing.T) {
	// /apps for the member has <option value=A> and no option for B or the default project.
}

func TestCreatingAProjectSharesItWithTheCreator(t *testing.T) {
	// Project admin POST /projects: IsProjectMember true. System admin: not added.
}

func TestDefaultProjectIsNotImplied(t *testing.T) {
	// POST /templates/{id} with empty project_id by a member not on Default => 404.
	// The same member added to Default => succeeds.
}
```

- [ ] **Step 2: Run the tests and watch them fail**

Run: `go test ./internal/server/ -run 'TestListPages|TestPersonWithNo|TestCreateAppForm|TestCreatingAProject|TestDefaultProject'`
Expected: FAIL: project B's name appears.

- [ ] **Step 3: Filter the lists**

System admins keep the existing unfiltered store calls. For everyone else: `projectRows`, `handleOverview`, `handleAppsList`, `renderAppsWithError`, `handleDatabasesList`, `handleDomainsList`, `renderTemplates` and `renderProject`'s `Projects` go through the scope. The Overview's four figures all count things inside projects, so filtering them is enough to meet "hides host-wide figures". Filter `ListAllDomains` rows by a map of app ID to project, built from `ListApps`. `handleCreateProject` calls `CreateProjectWithMember` unless the creator is a system admin. A create-app dialog with no projects shows no "+ New app" button.

- [ ] **Step 4: Run the tests and watch them pass**

Run: `go test ./internal/server/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/server
git commit -m "feat(server): list pages show only the projects shared with you"
```

### Task 5: Hide controls and secrets

**Files:**
- Modify: `internal/server/templates/base.tmpl`, `app.tmpl`, `project.tmpl`, `projects.tmpl`, `apps.tmpl`, `database.tmpl`, `deployment.tmpl`, `backups.tmpl`, `restore.tmpl`, `internal/server/handlers.go`, `internal/server/projects.go`, `internal/server/databases.go`
- Test: `internal/server/access_viewer_test.go`

**Interfaces:**
- Consumes: `canChange`, the keys `render` sets.
- Produces: `func viewerEnv(rows []envRow) []envRow`: every value blanked and `IsSecret` true, so templates show the mask.

- [ ] **Step 1: Write the failing tests**

```go
func TestViewerSeesNoSecrets(t *testing.T) {
	// Seed with sentinels: app env "PLAIN"="plain-sentinel" (not secret), project env the same,
	// app.WebhookSecret "whsec-sentinel", WebhookToken "whtok-sentinel", and a database attached to the app
	// with password "dbpw-sentinel".
	// As a viewer, GET /apps/{id}, /projects/{id}, /databases/{id}:
	// no body contains any sentinel. The keys ("PLAIN") and status are still shown.
	// As a member, the plain value and webhook URL are shown.
}

func TestMemberSeesNoSourceOrDestinationCredentials(t *testing.T) {
	// A destination with AccessKey "ak-sentinel" and SecretKey "sk-sentinel", and a git source with
	// PrivateKey "pk-sentinel" and WebhookSecret "gs-sentinel".
	// As a member, GET /apps, /apps/{id}, /databases/{id}: destination and source names can appear,
	// and no body contains a sentinel.
}

func TestViewerGetsNoChangeControls(t *testing.T) {
	// As a viewer, app/project/database/deployment pages contain no `method="post"` form
	// other than the sidebar's /logout.
	// As a member, the app page has a form with action "/apps/{id}/deploy".
}

func TestSidebarMatchesRole(t *testing.T) {
	// Member: no href="/settings", "/containers", "/metrics", "/volumes", "/registry", "/users";
	// has href="/account". System admin: has all of them.
}
```

- [ ] **Step 2: Run the tests and watch them fail**

Run: `go test ./internal/server/ -run 'TestViewer|TestMemberSeesNo|TestSidebar'`
Expected: FAIL on the sentinels.

- [ ] **Step 3: Remove the data in the handlers, not only the templates**

When `!s.canChange(r)`: `handleAppDetail` passes `viewerEnv(rows)`, omits `WebhookURL`, `WebhookEnable` and `PushRemote`, and blanks `app.WebhookSecret` and `app.WebhookToken` on its copy before it goes into `data`. `renderProject` passes `viewerEnv`. `renderDatabase` omits `InternalURL`, `ExternalURL` and `EnvExample`.

- [ ] **Step 4: Guard the templates**

Wrap each change form and button listed by `grep -n '<form \|onclick="open' internal/server/templates/*.tmpl` in `{{if .CanChange}}` (`$.CanChange` inside `range`). Hide the Connection panel in `database.tmpl` the same way. `+ New project` and project delete need `.CanManage`. In `base.tmpl`, `Containers`, `Volumes`, `Registry`, `Metrics` and `Settings` need `.IsSystemAdmin`, `Templates` needs `.CanChange`, and `Overview`, `Projects`, `Apps`, `Databases` and `Domains` need `.HasRole`. Add an `Account` link to `.sidebar-foot`. In `render`, compute `UpdateAvailable` only for system admins.

- [ ] **Step 5: Run the tests and watch them pass**

Run: `go test ./internal/server/`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/server
git commit -m "feat(ui): viewers see no secrets and no change controls"
```

### Task 6: Users page and setup links

**Files:**
- Create: `internal/server/users.go`, `internal/server/templates/users.tmpl`, `internal/server/templates/account_setup.tmpl`
- Modify: `internal/server/routes.go`, `internal/server/server.go` (`pages`), `internal/server/templates/base.tmpl`
- Test: `internal/server/users_test.go`

**Interfaces:**
- Consumes: Tasks 1, 2 and 3.
- Produces: routes at `levelSystemAdmin`: `GET /users`, `POST /users`, `POST /users/{id}/role`, `POST /users/{id}/reset`, `POST /users/{id}/delete`. Routes at `levelPublic`: `GET /account/setup`, `POST /account/setup`. `const setupLinkTTL = 7 * 24 * time.Hour`.

- [ ] **Step 1: Write the failing tests**

```go
func TestCreatePasswordAccountShowsLinkOnce(t *testing.T) {
	// POST /users username=reception role=none => page contains /account/setup?token=...
	// GET /users afterwards does not contain that token.
}

func TestSetupLinkSetsPasswordAndSignsIn(t *testing.T) {
	// GET the link => 200, shows "reception". POST password => 303 to / with a session cookie.
	// POST /login with those credentials works. Reusing the link shows
	// "This link has expired or has already been used."
}

func TestAccountWithoutPasswordCannotSignIn(t *testing.T) {
	// Before setup, POST /login username=reception password=anything => 401.
}

func TestResetPasswordEndsSessionsAndReplacesLink(t *testing.T) {
	// A signed-in account; POST /users/{id}/reset: its session now gets 303 to /login.
	// Its old password no longer signs in. An earlier link no longer works, the new one does.
}

func TestResetPasswordRefusesYourselfAndTheLastAdmin(t *testing.T) {
	// A system admin on their own row, even with another password admin present => 400 pointing to /account;
	// their password and session are unchanged.
	// An SSO system admin resetting the only password system admin => 400 with the last-admin reason.
}

func TestUsersPageRefusesLastPasswordAdmin(t *testing.T) {
	// The only admin POSTs a role change or delete on themselves => 400 re-render containing
	// "last system admin that signs in with a password"; GetUser unchanged.
}

func TestUsernameRules(t *testing.T) {
	// POST /users username="ab" => error. username="sso:x" => error.
	// A duplicate username => error shown, not a 500.
}

func TestWaitingBanner(t *testing.T) {
	// An SSO account with no role, by direct insert: /users has "1 person signed in and is waiting for a role."
	// and lists them first.
}
```

- [ ] **Step 2: Run the tests and watch them fail**

Run: `go test ./internal/server/ -run 'TestCreatePasswordAccount|TestSetupLink|TestAccountWithout|TestResetPassword|TestUsersPage|TestUsernameRules|TestWaitingBanner'`
Expected: FAIL with 404s.

- [ ] **Step 3: Implement the handlers and pages**

`POST /users/{id}/reset` calls `ResetUserPassword`, after refusing the caller's own account. It shows the new link once, the same way as a new account.

Copy comes from the mockup: columns Person, Signs in with, Role, Projects, Last sign-in; the note `"No role" means the person can't use Outhaul.`, extended in part 3; the "+ New password account" dialog with a username and role. Role changes post on `change` with a small inline script, and also work with a Save button when JS is off. `Remove` needs `confirm()`. `Reset password` appears only for password accounts. Build link URLs from `s.publicURL`, or `scheme://r.Host` when it is empty. `GET`/`POST /account/setup` uses `HideChrome` and the same rules as first-boot setup: at least 8 characters and `withHashSlot`. Add `Users` to the System sidebar group, for `.IsSystemAdmin`. `s.login` calls `TouchUserSignIn`.

- [ ] **Step 4: Run the tests and watch them pass**

Run: `go test ./internal/server/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/server
git commit -m "feat(users): system admins manage accounts and roles"
```

### Task 7: Sharing panel and Account page

**Files:**
- Create: `internal/server/account.go`, `internal/server/templates/account.tmpl`
- Modify: `internal/server/projects.go`, `internal/server/templates/project.tmpl`, `internal/server/routes.go`, `internal/server/settings.go`, `internal/server/templates/settings.tmpl`, `internal/server/templates/app.tmpl` (the link to `/settings`, near line 168), `internal/server/settings_test.go`, `internal/server/access_test.go`, `internal/store/pushkeys.go`
- Test: `internal/server/account_test.go`, `internal/server/projects_share_test.go`

**Interfaces:**
- Consumes: Tasks 2 and 3.
- Produces: routes `POST /projects/{id}/members` and `POST /projects/{id}/members/{userID}/delete` at `levelProjectAdmin` with `projectOfPath`. Routes at `levelAccount`: `GET /account`, `POST /account/password`, `POST /account/push-keys`, `POST /account/push-keys/{id}/delete`. `/settings/password` and `/settings/push-keys*` are removed.

- [ ] **Step 1: Write the failing tests**

```go
func TestProjectAdminSharesAndUnshares(t *testing.T) {
	// Project admin on P: POST members user_id=viewer => viewer now gets 200 on /projects/P.
	// Delete it => viewer gets 404. The People panel lists members with role labels.
}

func TestCannotShareWithNoRoleAccount(t *testing.T) {
	// POST members user_id=<no-role account> => 400; not added. They aren't offered in the dropdown.
}

func TestMemberCannotShare(t *testing.T) { /* project member POST members => 403 */ }

func TestSharingRefusesPeopleWhoCantBeListed(t *testing.T) {
	// POST members with a system admin's id, an unknown id, or no id => 400; no row is added.
	// Adding someone twice => 303 and one row.
}

func TestProjectAdminCanLeaveAProject(t *testing.T) {
	// A project admin removes themselves => 303 to /projects, not to the project page they can no longer open.
}

func TestProjectAdminOnAnotherProjectCannotShareThisOne(t *testing.T) { /* => 404 */ }

func TestAccountPageWorksWithNoRole(t *testing.T) {
	// A password account with RoleNone: GET /account => 200 with the Change password panel.
	// The sidebar has no href="/apps" or "/projects".
}

func TestPushKeysBelongToTheirOwner(t *testing.T) {
	// A adds a key; it appears on A's /account, not B's.
	// B POST /account/push-keys/{A's id}/delete => key still there.
}

func TestSettingsNoLongerHasPasswordOrKeys(t *testing.T) {
	// /settings has no "/settings/password"; POST /settings/password => 404 or 405.
}
```

Move the existing `TestChangePassword*` tests to `/account/password`, redirecting to `/account?ok=1`, and `TestSettingsShowsPushKeysPanel` to `/account`.

- [ ] **Step 2: Run the tests and watch them fail**

Run: `go test ./internal/server/ -run 'TestProjectAdminShares|TestCannotShare|TestMemberCannotShare|TestSharingRefuses|TestProjectAdminCan|TestProjectAdminOnAnother|TestAccountPage|TestPushKeysBelong|TestSettingsNoLonger|TestChangePassword'`
Expected: FAIL.

- [ ] **Step 3: Implement**

Move `handleChangePassword`, `handleAddPushKey` and `handleDeletePushKey` to `account.go`, unchanged apart from their paths and owner. A new key's `UserID` is the signed-in user, and deletes use `DeletePushKeyForUser`. Delete `ListPushKeys`. Cut both panels from `settings.tmpl` into `account.tmpl`, and fix the `app.tmpl` link to `/settings`. Add the `{userID}` wildcard to `TestEveryRouteRefusesTheWrongPeople`. The sidebar and `noaccess.tmpl` already link to `/account`, and the Users page's self-reset message points there, so check each now leads somewhere. People panel copy comes from the mockup, including "System admins can see every project and aren't listed here." Controls need `.CanManage`.

- [ ] **Step 4: Run the tests and watch them pass**

Run: `go test ./internal/server/ ./internal/store/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/server internal/store
git commit -m "feat(account): share projects, and manage your own password and keys"
```

### Task 8: Pushes follow roles, and the decisions are recorded

**Files:**
- Modify: `internal/gitssh/server.go`, `main.go` (only if the keyring wiring needs it), `ARCHITECTURE.md`, `AGENTS.md`
- Test: `internal/gitssh/authorize_test.go`, `internal/gitssh/server_test.go`

**Interfaces:**
- Consumes: `core.PushKey.UserID`, `Store.GetUser`, `Store.IsProjectMember`.
- Produces: `Keyring` gains `GetUser(ctx, id int64) (core.User, error)` and `IsProjectMember(ctx, projectID, userID int64) (bool, error)`. `func (s *Server) authorize(ctx context.Context, fp string, projectID int64) error` returns `errPushDenied` when the owner may not use that project.

- [ ] **Step 1: Write the failing test**

```go
func TestAuthorize(t *testing.T) {
	// Table using testServer's real store, key owner's role and membership in a project P:
	// system admin, not a member             => nil
	// project member, member of P            => nil
	// project member, not a member           => errPushDenied
	// viewer, member of P                    => errPushDenied
	// no role                                => errPushDenied
	// key with UserID 0 (no owner)           => errPushDenied
}

func TestColdPushCountsAsTheDefaultProject(t *testing.T) {
	// Over SSH: a member who isn't on the default project pushes to a new app name: refused,
	// with the permission message on stderr, and no bare repo or app is created.
	// Once they're added to the default project, the same push is accepted.
}
```

The existing helper at `internal/gitssh/server_test.go:91` adds a key with no owner, which is now refused. Give it a system-admin owner.

- [ ] **Step 2: Run the test and watch it fail**

Run: `go test ./internal/gitssh/ -run 'TestAuthorize|TestColdPush'`
Expected: compile error.

- [ ] **Step 3: Implement**

In `runGit`, after the app lookup: an existing app uses `app.ProjectID`; a cold push (`sql.ErrNoRows`) uses `core.DefaultProjectID`, because `githook` creates its app there. On denial, write `outhaul: you don't have permission to use '<repo>'` to stderr and return 1, before `repos.Init`. This applies to both `receive-pack` and `upload-pack`.

- [ ] **Step 4: Update the docs**

In `ARCHITECTURE.md`, replace the "Auth." locked decision with: several accounts, a role per person managed in Outhaul, argon2id passwords, session cookie, first account created on first boot. Add `access.go`, `routes.go`, `users.go` and `account.go` to the package layout. In `AGENTS.md` "Decisions to keep", add a **Roles** entry: roles are guardrails for trusted people, not containment; new routes go in `routes()` with a level; the last password system admin can't be removed because there's no recovery command. Add a convention: new pages that show secrets must withhold them when `!canChange`.

- [ ] **Step 5: Run everything**

Run: `go vet ./... && go test -race ./...`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/gitssh main.go ARCHITECTURE.md AGENTS.md
git commit -m "feat(gitssh): a push needs member or above on the project"
```
