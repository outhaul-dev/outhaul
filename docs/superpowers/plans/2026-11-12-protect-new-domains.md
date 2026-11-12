# Sign-In Required on New Domains (R11) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** With `OUTHAUL_PROTECT_NEW_DOMAINS=yes`, every domain row Outhaul creates — the dialog, the new-app form, a template deploy, a first `git push` and a preview's own address — requires sign-in from the moment it exists, and the only way to open one is a deliberate choice.

**Architecture:** The setting is parsed into `config.Config`, validated with the rest, and held on `server.Server` and on the bridge and preview manager as one boolean each. The gate's host mapping is forced in `infraController.reconcile`, so a protected container always finds the gate. Each creation path asks `protectionAvailability` before it writes, checks the shared-host and leftover-route rules, writes the row protected, and calls `ensureGateHost`; the two form handlers delete the app they just made if that last step fails.

**Tech Stack:** Go 1.26, SQLite (modernc), `html/template` pages, Traefik labels unchanged.

**Spec:** `docs/superpowers/specs/2026-11-11-shared-host-defaults-design.md`, sections "Settings", "R11: sign-in on new domains", "When sign-in can't work", "Every path that creates a row", "Making something public", "Tests", "Docs", "Order of work" and calls 19 to 22.

**Before you start:** the R9 plan (`2026-11-12-default-domain-suffix.md`) must have landed. Both plans change `handleCreateApp`, `handleDeployTemplate`, `coldCreate` and the two forms; R9 goes first, and this plan's snippets take the code as R9 leaves it (`s.naming`, the template deploy's one transaction). No migration: `domains.protected` exists (migration 0028).

## Global Constraints

- The only new setting is `OUTHAUL_PROTECT_NEW_DOMAINS` (yes/no, default no). With it off nothing is protected, no form grows a switch, and no refusal fires.
- `Validate` refuses it without `OUTHAUL_PUBLIC_URL`. `serve` refuses it with no HTTPS and no Cloudflare Tunnel, and refuses when the tunnel setting can't be read. `infraController.Disable` refuses to turn the tunnel off when that would leave neither.
- A new row is never left open by omission: a request without the dialog's `protected_offered` field is protected. A row that can't be protected is not created at all.
- Protected routes keep every "Decisions to keep" entry in `AGENTS.md`: the gate lives in each app's labels, a change reaches Traefik only by a deploy, previews only ever gain protection, and only a system admin brings two projects together on a protected host.
- Editing an existing row (`handleUpdateDomain`) and adding a preview's row (`addPreviewRow`) do not change.
- Nothing is stored per row and no migration is needed. (0031 and 0032 belong to other branches.)
- Pages that show a secret still withhold it; nothing here shows one.
- Test first: write the failing test, watch it fail for the stated reason, then implement.
- This repository is public: `example.com` names only, in code, tests and docs.
- After each task: `go build ./... && go vet ./... && go test ./...`. Before calling the plan done: `go test -race -timeout 30m ./...`.
- Commits: `feat(scope): summary` or `fix(scope): summary`, with a body saying what the user saw go wrong and what changed.

## Review Focus

The inputs and failures most likely to bite that no requirement spells out. Each has its test in the task named.

1. A request that never came from the dialog, so `protected_offered` is absent: the row is protected, not open. The same for the new-app form. Only an explicit "off" from a page that offered the switch opens a row (Tasks 3, 4).
2. Sign-in that can't work: `serve` refuses at startup with no HTTPS and no tunnel, an unreadable tunnel setting is named rather than guessed (stricter than the design's wording, on purpose), and the dialog and both form handlers refuse to create the row instead of creating an open one (Tasks 1, 3, 4, 5).
3. Another project already has a row on the host, or another app's running container still serves an old open route there: the new-app form, the template deploy and the first push refuse before writing and leave no app behind; the dialog keeps its existing warn-and-redeploy path (Tasks 3, 4, 5, 6).
4. `ensureGateHost` fails after the app and its rows were written: the new-app form and the template deploy delete the app they made and say why; no deployment is created (Tasks 4, 5).
5. A preview of an app whose whole-host row is open: the preview's own address still requires sign-in, while a path row it copies stays as open as its parent's (Task 7).
6. An install that leaves the setting unset: rows are open, the domain dialog's switch is off and disabled as it is today, the new-app and template forms have no switch, and a first push is open (Task 3's `TestProtectNewOffChangesNothing`, Task 4's, Task 6's and Task 7's off-cases).

## File map

| File | Responsibility | Tasks |
|---|---|---|
| `internal/config/config.go`, `config_test.go` | the setting and its validation | 1 |
| `main.go`, `main_test.go` | the startup/tunnel refusals, the gate host mapping, the wiring | 1, 6, 7 |
| `internal/server/server.go` | the field, its setter, the page-wide key | 1 |
| `internal/store/apps.go`, `apps_test.go` | `CreateAppProtected` and the seed it writes | 2 |
| `internal/server/handlers.go`, `protect_new_test.go` (new) | the dialog's rule; the new-app form | 3, 4 |
| `internal/server/templates/domainform.tmpl`, `appform.tmpl` | the switch, its default, its previews note | 3, 4 |
| `internal/server/catalog.go`, `catalog_test.go` | the template deploy's protected rows and refusals | 5 |
| `internal/githook/bridge.go`, `bridge_test.go` | the first push's protected row and refusals | 6 |
| `internal/previewmgr/manager.go`, `manager_test.go` | a preview's whole-host row | 7 |
| `README.md` | the setting's row | 1 |
| `docs/PROTECTED-ROUTES.md`, `docs/SHARED-HOST.md`, `ARCHITECTURE.md`, `AGENTS.md` | the guide and the kept decision | 8 |

---

## Task 1: the setting, the refusals, and the gate host mapping

**Files:**
- Modify: `internal/config/config.go` (the `Config` field, `Load`, `Validate`), `internal/config/config_test.go`, `main.go:227-232,580-590,758-772`, `main_test.go`, `internal/server/server.go:110-135,333-345`, `README.md`
- Test: `internal/config/config_test.go`, `main_test.go`

**Interfaces:**
- Produces:

```go
// config.Config
ProtectNewDomains bool // OUTHAUL_PROTECT_NEW_DOMAINS: a new domain row requires sign-in

// main.go
func protectNewDomainsError(cfg config.Config, tunnelOn bool, tunnelErr error) error // nil when it can be served

// server.Server
// protectNew bool, beside tlsEnabled
func (s *Server) SetProtectNewDomains(on bool) // call before Handler
```

- [ ] **Step 1: Write the failing tests**

Add to `internal/config/config_test.go`:

```go
func TestProtectNewDomainsSetting(t *testing.T) {
	for _, c := range []struct {
		name    string
		env     map[string]string
		wantErr bool
	}{
		{"unset", nil, false},
		{"off", map[string]string{"OUTHAUL_PROTECT_NEW_DOMAINS": "no"}, false},
		{"on without a public URL", map[string]string{"OUTHAUL_PROTECT_NEW_DOMAINS": "yes"}, true},
		{"on with a public URL", map[string]string{
			"OUTHAUL_PROTECT_NEW_DOMAINS": "yes", "OUTHAUL_PUBLIC_URL": "https://paas.example.com",
		}, false},
		{"malformed", map[string]string{"OUTHAUL_PROTECT_NEW_DOMAINS": "maybe"}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg := Load(func(k string) string { return c.env[k] })
			err := cfg.Validate()
			if c.wantErr != (err != nil) {
				t.Fatalf("Validate = %v, want an error: %v", err, c.wantErr)
			}
			if err != nil && !strings.Contains(err.Error(), "OUTHAUL_PROTECT_NEW_DOMAINS") {
				t.Errorf("error %q does not name OUTHAUL_PROTECT_NEW_DOMAINS", err)
			}
		})
	}
}
```

Add to `main_test.go`:

```go
func TestProtectNewDomainsError(t *testing.T) {
	on := config.Config{ProtectNewDomains: true, PublicURL: "https://paas.example.com"}
	if err := protectNewDomainsError(on, false, nil); err == nil || !strings.Contains(err.Error(), "OUTHAUL_PROTECT_NEW_DOMAINS") {
		t.Fatalf("no HTTPS and no tunnel: %v, want an error naming the setting", err)
	}
	if err := protectNewDomainsError(on, true, nil); err != nil {
		t.Errorf("a tunnel serves it: %v", err)
	}
	https := on
	https.ACMEEmail = "ops@example.com"
	if err := protectNewDomainsError(https, false, nil); err != nil {
		t.Errorf("HTTPS serves it: %v", err)
	}
	if err := protectNewDomainsError(https, false, errors.New("no such table")); err != nil {
		t.Errorf("HTTPS makes an unreadable tunnel setting irrelevant: %v", err)
	}
	if err := protectNewDomainsError(on, false, errors.New("no such table")); err == nil || !strings.Contains(err.Error(), "no such table") {
		t.Errorf("an unreadable tunnel setting: %v, want the cause named", err)
	}
	off := config.Config{PublicURL: "https://paas.example.com"}
	if err := protectNewDomainsError(off, false, errors.New("no such table")); err != nil {
		t.Errorf("the setting off: %v", err)
	}
}

// Disabling the tunnel would leave a protected row with no HTTPS at all.
func TestDisableTunnelRefusedWithProtectNewDomains(t *testing.T) {
	cfg := config.Config{
		DataDir: t.TempDir(), Network: "outhaul", TraefikImage: config.DefaultTraefikImage,
		ListenAddr: ":8080", AdminDirect: true,
		PublicURL: "https://paas.example.com", ProtectNewDomains: true,
	}
	st, err := openStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.SetCloudflareToken(context.Background(), "tok"); err != nil {
		t.Fatal(err)
	}
	ic := &infraController{dc: docker.NewFake(), st: st, cfg: cfg}
	err = ic.Disable(context.Background())
	if err == nil || !strings.Contains(err.Error(), "OUTHAUL_PROTECT_NEW_DOMAINS") {
		t.Fatalf("Disable = %v, want an error naming OUTHAUL_PROTECT_NEW_DOMAINS", err)
	}
	if on, err := st.TunnelEnabled(context.Background()); err != nil || !on {
		t.Errorf("TunnelEnabled = %v, %v; the tunnel must stay enabled", on, err)
	}

	// With HTTPS configured the tunnel can go.
	cfg.ACMEEmail = "ops@example.com"
	ic.cfg = cfg
	if err := ic.Disable(context.Background()); err != nil {
		t.Errorf("Disable with HTTPS: %v", err)
	}
}

// gateMapped reports whether the Traefik the fake created maps the gate's
// host to the host gateway.
func gateMapped(t *testing.T, dc *docker.Fake) bool {
	t.Helper()
	var spec *docker.ContainerSpec
	for i := range dc.Created {
		if dc.Created[i].Name == "outhaul-traefik" {
			spec = &dc.Created[i]
		}
	}
	if spec == nil {
		t.Fatal("no Traefik container was created")
	}
	for _, h := range spec.ExtraHosts {
		if h == "host.docker.internal:host-gateway" {
			return true
		}
	}
	return false
}

// Every new row will be protected, so the gate's host is mapped from startup,
// whatever the rows say. No public URL (so no admin route) and no protected
// row: only the setting can map it.
// Model this on TestReconcileMapsTheGateHostWhileARouteIsProtected.
func TestReconcileMapsTheGateHostForNewDomains(t *testing.T) {
	ctx := context.Background()
	cfg := config.Config{
		DataDir: t.TempDir(), Network: "outhaul", TraefikImage: config.DefaultTraefikImage,
		CloudflaredImage: config.DefaultCloudflaredImage, HTTPSPort: "443", ListenAddr: ":8080",
		ACMEEmail: "ops@example.com", AdminDirect: true, ProtectNewDomains: true,
	}
	st, err := openStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	dc := docker.NewFake()
	ic := &infraController{dc: dc, st: st, cfg: cfg}
	if err := ic.reconcile(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !gateMapped(t, dc) {
		t.Error("the gate host is not mapped although every new row will be protected")
	}
}

// The same config with the setting off maps nothing, as it always did.
func TestReconcileLeavesTheGateHostUnmappedWithoutProtectNewDomains(t *testing.T) {
	ctx := context.Background()
	cfg := config.Config{
		DataDir: t.TempDir(), Network: "outhaul", TraefikImage: config.DefaultTraefikImage,
		CloudflaredImage: config.DefaultCloudflaredImage, HTTPSPort: "443", ListenAddr: ":8080",
		ACMEEmail: "ops@example.com", AdminDirect: true,
	}
	st, err := openStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	dc := docker.NewFake()
	ic := &infraController{dc: dc, st: st, cfg: cfg}
	if err := ic.reconcile(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if gateMapped(t, dc) {
		t.Error("with the setting off the gate host must stay unmapped")
	}
}
```

Check `main_test.go`'s existing helpers (`openStore`, `docker.NewFake`, `config`) before writing: `TestReconcileMapsTheGateHostWhileARouteIsProtected` uses exactly these.

- [ ] **Step 2: Run the tests to see them fail**

Run: `go test ./internal/config/ . -run 'ProtectNewDomains|DisableTunnelRefusedWithProtect|Reconcile'`

Expected: FAIL — the field, `protectNewDomainsError` and the refusals are undefined.

- [ ] **Step 3: Implement**

- `internal/config/config.go`: `ProtectNewDomains bool` beside `PublicURL`, `ProtectNewDomains: p.bool("OUTHAUL_PROTECT_NEW_DOMAINS")` in `Load`, and in `Validate`:

```go
if c.ProtectNewDomains && !c.PublicURLSet() {
	errs = append(errs, errors.New("OUTHAUL_PROTECT_NEW_DOMAINS needs OUTHAUL_PUBLIC_URL, where people are sent to sign in"))
}
```

- `main.go`, the tunnel read in `serve` (now `if on, err := st.TunnelEnabled(...); err == nil { … }`): keep the two existing checks, and keep the block skipped on a read error, then add the new check after it:

```go
	tunnelOn, tunnelErr := st.TunnelEnabled(context.Background())
	if tunnelErr == nil {
		if err := httpOffTunnelError(cfg, tunnelOn); err != nil {
			return err
		}
		if err := adminPublishError(cfg, tunnelOn); err != nil {
			return err
		}
	}
	if err := protectNewDomainsError(cfg, tunnelOn, tunnelErr); err != nil {
		return err
	}
```

  and the function beside `httpOffTunnelError`:

```go
// protectNewDomainsError reports why an install that requires sign-in on every
// new domain can't be served: people need somewhere to sign in, over HTTPS or
// through a tunnel, and that has to be known, not guessed.
func protectNewDomainsError(cfg config.Config, tunnelOn bool, tunnelErr error) error {
	if !cfg.ProtectNewDomains || cfg.TLSEnabled() || tunnelOn {
		return nil
	}
	if tunnelErr != nil {
		return fmt.Errorf("OUTHAUL_PROTECT_NEW_DOMAINS needs HTTPS or a Cloudflare Tunnel, and the tunnel setting couldn't be read: %w", tunnelErr)
	}
	return errors.New("OUTHAUL_PROTECT_NEW_DOMAINS needs HTTPS or a Cloudflare Tunnel: set OUTHAUL_ACME_EMAIL or OUTHAUL_LOCAL_CA, or enable the tunnel in Settings")
}
```

Refusing on a tunnel read error is stricter than the design's wording, and deliberately so. The local CA's tunnel rule (and the two checks above) skip when the read fails; here an unreadable setting on a server with no HTTPS means nothing can be verified, and guessing "no tunnel" would let the install start and then create rows nobody could get through. With HTTPS configured the read is irrelevant and the error is ignored. Say so in the function's comment.

- `infraController.Disable`, first line: `if ic.cfg.ProtectNewDomains && !ic.cfg.TLSEnabled() { return errors.New("cannot disable the Cloudflare Tunnel while OUTHAUL_PROTECT_NEW_DOMAINS is set: with no HTTPS, nobody could get through a protected row") }`.
- `infraController.reconcile`, where `protected` is read:

```go
	if ic.cfg.ProtectNewDomains {
		// Every row created from now on is protected, so the gate's host is
		// mapped whatever the rows say. The admin route needs it anyway, so
		// this usually recreates nothing.
		protected = true
	} else if !protected {
		protected = ic.gatedContainersRemain(ctx, tunnelOn)
	}
```

- `internal/server/server.go`: `protectNew bool // OUTHAUL_PROTECT_NEW_DOMAINS: a new row requires sign-in` in the `Server` struct, a `SetProtectNewDomains` setter beside `SetSignInBackoff`, and in `render` add `data["ProtectNewDomains"] = s.protectNew` beside `CanChange`.
- `main.go`: `srv.SetProtectNewDomains(cfg.ProtectNewDomains)` beside `srv.SetSignInBackoff(...)`.
- `README.md`: add the row after `OUTHAUL_PUBLIC_URL`:

```text
| `OUTHAUL_PROTECT_NEW_DOMAINS` | `no`           | `yes` makes every new domain require sign-in: the dialog's switch starts on, and the new-app form, a template deploy, a first `git push` and a preview's own address all get it. Needs `OUTHAUL_PUBLIC_URL` and HTTPS or a tunnel — see docs/PROTECTED-ROUTES.md |
```

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/config/ . && go build ./... && go test ./...`

Expected: PASS. Nothing else changed: `SetProtectNewDomains(false)` leaves every existing page and handler as it was.

- [ ] **Step 5: Commit**

```bash
git add internal/config internal/server/server.go main.go main_test.go README.md
git commit -m "feat(server): OUTHAUL_PROTECT_NEW_DOMAINS, refused where sign-in can't work" -m "A host could ask for sign-in on every new domain and still start with nowhere to sign in, or keep the gate's host unmapped. Refuse the setting where it can't be served and map the gate from startup."
```

---

## Task 2: a protected primary row in the store

**Files:**
- Modify: `internal/store/apps.go:19-115`
- Test: `internal/store/apps_test.go`

**Interfaces:**
- Produces:

```go
// CreateAppProtected is CreateAppWithDomains for an install that requires
// sign-in on new domains: the row seeded from app.Domain requires it too. An
// explicit row in domains keeps its own Protected.
func (s *Store) CreateAppProtected(ctx context.Context, app core.App) (core.App, error)
```

- [ ] **Step 1: Write the failing test**

Add to `internal/store/apps_test.go`:

```go
// The row seeded from app.Domain is open by default and protected when the
// caller asks for it; the app's own Domain field is unchanged either way.
func TestCreateAppProtectedSeedsAProtectedPrimaryRow(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()

	open, err := st.CreateApp(ctx, core.App{Name: "open", RepoURL: "https://example.com/o.git", Domain: "open.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := st.ListDomains(ctx, open.ID)
	if err != nil || len(rows) != 1 {
		t.Fatalf("open rows = %v (%v)", rows, err)
	}
	if rows[0].Protected {
		t.Error("CreateApp seeded a protected row")
	}

	guarded, err := st.CreateAppProtected(ctx, core.App{Name: "guarded", RepoURL: "https://example.com/g.git", Domain: "guarded.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	rows, err = st.ListDomains(ctx, guarded.ID)
	if err != nil || len(rows) != 1 {
		t.Fatalf("guarded rows = %v (%v)", rows, err)
	}
	if !rows[0].Protected || rows[0].Host != "guarded.example.com" || rows[0].Port != primaryDomainPort {
		t.Errorf("guarded row = %+v, want a protected row on guarded.example.com", rows[0])
	}
	if guarded.Domain != "guarded.example.com" {
		t.Errorf("app.Domain = %q, want the submitted host", guarded.Domain)
	}

	// A compose app seeds no row, so the protected seed changes nothing there.
	stack, err := st.CreateAppProtected(ctx, core.App{Name: "stack", RepoURL: "https://example.com/s.git", Kind: core.KindCompose})
	if err != nil {
		t.Fatal(err)
	}
	if rows, _ := st.ListDomains(ctx, stack.ID); len(rows) != 0 {
		t.Errorf("compose rows = %v, want none", rows)
	}
}
```

- [ ] **Step 2: Run the test to see it fail**

Run: `go test ./internal/store/ -run 'CreateAppProtected'`

Expected: FAIL — `CreateAppProtected` is undefined.

- [ ] **Step 3: Implement**

In `internal/store/apps.go`: rename the body of `CreateAppWithDomains` to an unexported `createApp(ctx, app, domains []core.Domain, primaryProtected bool) (core.App, error)`, make `CreateAppWithDomains` call it with `false`, add `CreateAppProtected` calling it with `true` and no extra rows, and set `Protected: primaryProtected` on the seeded row:

```go
	if app.Domain != "" && app.Kind != core.KindCompose {
		if _, err := s.addDomainTx(ctx, tx, core.Domain{
			AppID: app.ID, Host: app.Domain, Port: primaryDomainPort, TLS: true, Protected: primaryProtected,
		}); err != nil {
			return core.App{}, err
		}
	}
```

Update the doc comment on `CreateAppWithDomains` to say the primary row is open, and on `CreateAppProtected` to say it isn't.

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/store/ ./...`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/store
git commit -m "feat(store): CreateAppProtected seeds a protected primary row" -m "A single-container app's first row was always open, so a host that requires sign-in on new domains had to open it and close it again. Seed it protected when the caller says so."
```

---

## Task 3: the domain dialog

**Files:**
- Modify: `internal/server/handlers.go:1027-1090` (`handleAddDomain`), `internal/server/templates/domainform.tmpl`
- Test: `internal/server/protect_new_test.go` (new)

**Interfaces:**
- Consumes: `s.protectNew` (Task 1), `s.protectionAvailability`, `s.sharedHostRefused`, `s.ensureGateHost`, page key `ProtectNewDomains` (Task 1).

- [ ] **Step 1: Write the failing tests**

Create `internal/server/protect_new_test.go`:

```go
// A request that didn't come from the dialog (no protected_offered: a script,
// an old page) doesn't mean "open": the row is protected.
func TestNewDomainIsProtectedWithoutTheFormsField(t *testing.T) {
	e, _ := newProtectionEnv(t)
	e.srv.protectNew = true
	app, err := e.store.CreateApp(context.Background(), core.App{Name: "web", RepoURL: "https://x/y.git", Domain: "web.test"})
	if err != nil {
		t.Fatal(err)
	}

	r := e.postForm(t, "/apps/"+itoa(app.ID)+"/domains", url.Values{"host_kind": {"custom"}, "host": {"shop.test"}})
	if r.StatusCode >= 400 {
		t.Fatalf("add domain: status %d, body %s", r.StatusCode, body(t, r))
	}
	r.Body.Close()
	rows, _ := e.store.ListDomains(context.Background(), app.ID)
	var added core.Domain
	for _, d := range rows {
		if d.Host == "shop.test" {
			added = d
		}
	}
	if !added.Protected {
		t.Errorf("row = %+v, want it protected", added)
	}
}

// The dialog's switch starts on for a new row, and says how to open one.
func TestDomainDialogOffersSignInOnByDefault(t *testing.T) {
	e, _ := newProtectionEnv(t)
	e.srv.protectNew = true
	app, _ := e.store.CreateApp(context.Background(), core.App{Name: "web", RepoURL: "https://x/y.git", Domain: "web.test"})
	page := body(t, e.get(t, "/apps/"+itoa(app.ID)))
	if !strings.Contains(page, `id="domain-protected" checked`) {
		t.Error("the dialog's Require sign-in switch is not on for a new row")
	}
	if !strings.Contains(page, "turn this switch off on the host") {
		t.Error("the dialog doesn't say how to make an address public")
	}
}

// Turning the switch off in the dialog opens the row, as before.
func TestNewDomainOpenedByTheDialogsSwitch(t *testing.T) {
	e, _ := newProtectionEnv(t)
	e.srv.protectNew = true
	app, _ := e.store.CreateApp(context.Background(), core.App{Name: "web", RepoURL: "https://x/y.git", Domain: "web.test"})

	r := e.postForm(t, "/apps/"+itoa(app.ID)+"/domains", url.Values{
		"host_kind": {"custom"}, "host": {"shop.test"}, "protected_offered": {"1"},
	})
	if r.StatusCode >= 400 {
		t.Fatalf("add domain: status %d, body %s", r.StatusCode, body(t, r))
	}
	r.Body.Close()
	rows, _ := e.store.ListDomains(context.Background(), app.ID)
	for _, d := range rows {
		if d.Host == "shop.test" && d.Protected {
			t.Errorf("row = %+v, want it open", d)
		}
	}
}

// With the setting off nothing changes: a row is open, and the switch is the
// disabled, off one it was.
func TestProtectNewOffChangesNothing(t *testing.T) {
	e := newTestEnv(t)
	e.completeSetup(t)
	app, _ := e.store.CreateApp(context.Background(), core.App{Name: "web", RepoURL: "https://x/y.git", Domain: "web.test"})

	r := e.postForm(t, "/apps/"+itoa(app.ID)+"/domains", url.Values{"host_kind": {"custom"}, "host": {"shop.test"}})
	if r.StatusCode >= 400 {
		t.Fatalf("add domain: status %d, body %s", r.StatusCode, body(t, r))
	}
	r.Body.Close()
	rows, _ := e.store.ListDomains(context.Background(), app.ID)
	for _, d := range rows {
		if d.Host == "shop.test" && d.Protected {
			t.Errorf("row = %+v, want it open with the setting off", d)
		}
	}
	page := body(t, e.get(t, "/apps/"+itoa(app.ID)))
	if strings.Contains(page, `id="domain-protected" checked`) {
		t.Error("the switch is on although the setting is off")
	}
}
```

`newProtectionEnv(t)` is the existing helper in `protection_test.go`: it signs in, turns TLS on and installs the fake proxy that `ensureGateHost` needs.

- [ ] **Step 2: Run the tests to see them fail**

Run: `go test ./internal/server/ -run 'ProtectedWithout|OffersSignInOn|OpenedByTheDialogs|ProtectNewOff'`

Expected: FAIL — the first row is open and the page's switch is never checked.

- [ ] **Step 3: Implement**

`internal/server/handlers.go`, `handleAddDomain`, replace the `if r.FormValue("protected") != "" { … }` block:

```go
	var others []otherRedeploy
	// A new row requires sign-in while the server requires it on new domains,
	// unless the dialog turned it off. A request without the dialog's
	// protected_offered (a script, an old page) didn't choose, so protect it.
	protect := r.FormValue("protected") != ""
	if s.protectNew && r.FormValue("protected_offered") == "" {
		protect = true
	}
	if protect {
		if ok, reason := s.protectionAvailability(ctx, d); !ok {
			http.Error(w, reason, http.StatusBadRequest)
			return
		}
		d.Protected = true
	}
```

In `internal/server/templates/domainform.tmpl`, the switch in Step 3:

```html
          <label class="field-check">
            <input type="checkbox" class="switch" role="switch" name="protected" id="domain-protected"{{if .ProtectNewDomains}} checked{{end}}{{if and (not .ProtectNewDomains) (not .ProtectionAvailable)}} disabled{{end}}>
            <span>Require sign-in</span>
          </label>
          {{if .ProtectNewDomains}}
          <p class="muted">New addresses require sign-in. To make this one public, turn this switch off on the host's row; to leave one path open, such as a webhook, add a row for that path with the switch off.</p>
          {{end}}
```

With the setting on the switch is never rendered disabled: a new row has to be protected or deliberately opened, and a save that can't protect it is refused with the reason. The script's `applyLock` already leaves a new row's switch alone when it isn't disabled. The paragraph is the design's "Making something public", in the dialog: it names both ways to open something, so nobody has to read the docs to find them.

The previews note in the same file gains the setting's wording:

```html
            {{if .HasPreviews}}<p class="protect-also" id="domain-previews-stay">{{if .ProtectNewDomains}}Existing previews stay protected. A new preview's own address will require sign-in too.{{else}}Existing previews stay protected. New ones will be open.{{end}}</p>{{end}}
```

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/server/ -run 'ProtectedWithout|OffersSignInOn|OpenedByTheDialogs|ProtectNewOff' && go test ./...`

Expected: PASS, including every existing `protection_test.go` case.

- [ ] **Step 5: Commit**

```bash
git add internal/server
git commit -m "feat(server): a new domain requires sign-in when the host says so" -m "The dialog's switch started off, so forgetting it made an app public. Start it on for a new row, and treat a request that never saw the dialog as protected."
```

---

## Task 4: the new-app form

**Files:**
- Modify: `internal/server/handlers.go:257-400` (`handleCreateApp`), `internal/server/templates/appform.tmpl`
- Test: `internal/server/protect_new_test.go`

**Interfaces:**
- Consumes: `s.protectNew`, `store.CreateAppProtected` (Task 2), `s.protectionAvailability`, `s.sharedHostRefused`, `s.leftoverRoutes`, `s.ensureGateHost`, `store.DeleteApp`, `otherRedeploy.label()`.

- [ ] **Step 1: Write the failing tests**

Add to `internal/server/protect_new_test.go`:

```go
// The form's switch is on, and a single-container app's own row is protected.
func TestCreateAppProtectsItsRow(t *testing.T) {
	e, _ := newProtectionEnv(t)
	e.srv.protectNew = true
	form := appForm("web", "web.test")
	form.Set("protected_offered", "1")
	form.Set("protected", "on")
	r := e.postForm(t, "/apps", form)
	if r.StatusCode >= 400 {
		t.Fatalf("create app: status %d, body %s", r.StatusCode, body(t, r))
	}
	r.Body.Close()
	app, err := e.store.GetAppByName(context.Background(), "web")
	if err != nil {
		t.Fatal(err)
	}
	rows, _ := e.store.ListDomains(context.Background(), app.ID)
	if len(rows) != 1 || !rows[0].Protected {
		t.Errorf("rows = %+v, want one protected", rows)
	}
}

// A request that never saw the form is protected too; the form's own "off"
// opens the row.
func TestCreateAppWithoutTheFormsFieldIsProtected(t *testing.T) {
	e, _ := newProtectionEnv(t)
	e.srv.protectNew = true
	r := e.postForm(t, "/apps", appForm("web", "web.test")) // neither field, as a script sends it
	if r.StatusCode >= 400 {
		t.Fatalf("create app: status %d, body %s", r.StatusCode, body(t, r))
	}
	r.Body.Close()
	app, _ := e.store.GetAppByName(context.Background(), "web")
	rows, _ := e.store.ListDomains(context.Background(), app.ID)
	if len(rows) != 1 || !rows[0].Protected {
		t.Errorf("rows = %+v, want one protected", rows)
	}
}

func TestCreateAppFormSwitchOffOpensTheRow(t *testing.T) {
	e, _ := newProtectionEnv(t)
	e.srv.protectNew = true
	form := appForm("web", "web.test")
	form.Set("protected_offered", "1") // the switch off: offered, not named
	r := e.postForm(t, "/apps", form)
	if r.StatusCode >= 400 {
		t.Fatalf("create app: status %d, body %s", r.StatusCode, body(t, r))
	}
	r.Body.Close()
	app, _ := e.store.GetAppByName(context.Background(), "web")
	rows, _ := e.store.ListDomains(context.Background(), app.ID)
	if len(rows) != 1 || rows[0].Protected {
		t.Errorf("rows = %+v, want one open", rows)
	}
}

// A compose app's first row, given in the form, is protected too; an
// internal-only compose app has no row to protect and is created as before.
func TestCreateComposeAppProtectsItsFirstRow(t *testing.T) {
	e, _ := newProtectionEnv(t)
	e.srv.protectNew = true
	form := composeForm("shop")
	form.Set("protected_offered", "1")
	form.Set("protected", "on")
	r := e.postForm(t, "/apps", form)
	if r.StatusCode >= 400 {
		t.Fatalf("create app: status %d, body %s", r.StatusCode, body(t, r))
	}
	r.Body.Close()
	app, _ := e.store.GetAppByName(context.Background(), "shop")
	rows, _ := e.store.ListDomains(context.Background(), app.ID)
	if len(rows) != 1 || !rows[0].Protected {
		t.Errorf("rows = %+v, want one protected", rows)
	}

	internal := composeForm("box")
	internal.Del("domain")
	internal.Del("compose_service")
	internal.Del("compose_port")
	r = e.postForm(t, "/apps", internal)
	if r.StatusCode >= 400 {
		t.Fatalf("internal-only compose app: status %d, body %s", r.StatusCode, body(t, r))
	}
	r.Body.Close()
	if _, err := e.store.GetAppByName(context.Background(), "box"); err != nil {
		t.Errorf("internal-only compose app was not created: %v", err)
	}
}

// When the proxy can't be reconciled the app and its row are removed again,
// so nothing is left that claims sign-in it doesn't have.
func TestCreateAppRefusesWhenTheGateHostFails(t *testing.T) {
	e, px := newProtectionEnv(t)
	e.srv.protectNew = true
	px.err = errors.New("docker unreachable")

	r := e.postForm(t, "/apps", appForm("web", "web.test"))
	if r.StatusCode < 400 || !strings.Contains(body(t, r), "docker unreachable") {
		t.Fatalf("create app: status %d, want a refusal naming the cause", r.StatusCode)
	}
	if _, err := e.store.GetAppByName(context.Background(), "web"); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("app survived a failed gate host: %v", err)
	}
}

// An app that still serves an old open route on the host is refused, not
// redeployed from here: the new row would be reachable openly until it does.
func TestCreateAppRefusesAnotherAppsOldRoute(t *testing.T) {
	e, _ := newProtectionEnv(t)
	e.srv.protectNew = true
	leftoverHooks(t, e, []core.Domain{{Host: "shop.test", Port: deployAppPort}}, nil)

	r := e.postForm(t, "/apps", appForm("web", "shop.test"))
	if r.StatusCode < 400 || !strings.Contains(body(t, r), "hooks") {
		t.Fatalf("create app: status %d, want a refusal naming the app that must redeploy", r.StatusCode)
	}
	if _, err := e.store.GetAppByName(context.Background(), "web"); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("app survived a refused host: %v", err)
	}
}

// With the setting off the form offers no switch and the row is open.
func TestCreateAppWithoutTheSettingLeavesTheRowOpen(t *testing.T) {
	e := newTestEnv(t)
	e.completeSetup(t)
	r := e.postForm(t, "/apps", appForm("web", "web.test"))
	if r.StatusCode >= 400 {
		t.Fatalf("create app: status %d, body %s", r.StatusCode, body(t, r))
	}
	r.Body.Close()
	app, _ := e.store.GetAppByName(context.Background(), "web")
	rows, _ := e.store.ListDomains(context.Background(), app.ID)
	if len(rows) != 1 || rows[0].Protected {
		t.Errorf("rows = %+v, want one open", rows)
	}
	page := body(t, e.get(t, "/apps"))
	if strings.Contains(page, `name="protected"`) {
		t.Error("the new-app form offers a sign-in switch with the setting off")
	}
}
```

`appForm` (in `server_test.go`) stays as it is: it sends neither field, which is exactly what a script or an old page sends, and the tests that want the switch set say so with `form.Set`. The file's imports are `context`, `database/sql`, `errors`, `net/url`, `strings`, `testing` and `internal/core`.

- [ ] **Step 2: Run the tests to see them fail**

Run: `go test ./internal/server/ -run 'CreateAppProtects|WithoutTheFormsField|FormSwitchOff|ComposeAppProtects|GateHostFails|AnotherAppsOldRoute|WithoutTheSettingLeaves'`

Expected: FAIL — the rows are open, the form has no switch, and a failed reconcile leaves the app behind.

- [ ] **Step 3: Implement the handler**

In `handleCreateApp`, replace the `if host := app.Domain; … sharedHostRefused(…)` block with:

```go
	host := app.Domain
	if firstDomain != nil {
		host = firstDomain.Host
	}
	// A row created while the server requires sign-in on new domains is
	// protected, unless the form turned it off. A request without the form's
	// protected_offered (a script, an old page) didn't choose: protect it. A
	// compose app with no domain has no row to protect.
	protect := s.protectNew && host != "" &&
		(r.FormValue("protected_offered") == "" || r.FormValue("protected") != "")
	if protect {
		if ok, reason := s.protectionAvailability(r.Context(), core.Domain{Host: host}); !ok {
			s.renderAppsWithError(w, r, reason)
			return
		}
		if firstDomain != nil {
			firstDomain.Protected = true
		}
	}
	if host != "" {
		if refused, err := s.sharedHostRefused(r, projectID, host, true, protect); err != nil {
			s.renderAppsWithError(w, r, "Could not create app: "+err.Error())
			return
		} else if refused {
			s.renderAppsWithError(w, r, sharedHostRefusal)
			return
		}
	}
	if protect {
		// The row requires sign-in, so an app whose running container still
		// serves an old open route on the host would stay reachable openly
		// until it redeploys. Refuse and say which app: this form doesn't
		// redeploy another app.
		scope, err := s.scopeFor(r)
		if err != nil {
			log.Printf("create app: read the person's projects: %v", err)
			scope = projectScope{}
		}
		others, err := s.leftoverRoutes(r.Context(), core.App{Name: name, ProjectID: projectID},
			map[string]bool{strings.ToLower(host): true}, scope)
		if err != nil {
			s.renderAppsWithError(w, r, fmt.Sprintf("The app wasn't created: Outhaul couldn't tell which other apps still serve old routes on %s (%v).", host, err))
			return
		}
		if len(others) > 0 {
			s.renderAppsWithError(w, r, fmt.Sprintf(
				"The app wasn't created: %s still serves an old route on %s with no sign-in. Deploy it from its page, then try again.",
				others[0].label(), host))
			return
		}
	}
```

`log`, `fmt` and `strings` are already imported. Then the store write, replacing the existing `CreateAppWithDomains` call and its two error branches:

```go
	var created core.App
	var err error
	if protect && firstDomain == nil {
		created, err = s.store.CreateAppProtected(r.Context(), app)
	} else {
		created, err = s.store.CreateAppWithDomains(r.Context(), app, extra)
	}
	if err != nil {
		if errors.Is(err, store.ErrHostReserved) {
			s.renderAppsWithError(w, r, ownHostRefusal)
			return
		}
		s.renderAppsWithError(w, r, "Could not create app: "+s.takenText(r, err))
		return
	}
```

and after it:

```go
	if protect {
		ctx := context.WithoutCancel(r.Context())
		if err := s.ensureGateHost(ctx); err != nil {
			if undo := s.store.DeleteApp(ctx, created.ID); undo != nil {
				log.Printf("create app: remove %q after the proxy failed: %v", app.Name, undo)
			}
			s.renderAppsWithError(w, r, fmt.Sprintf(
				"The app wasn't created: Outhaul couldn't map the sign-in gate before it deploys (%v).", err))
			return
		}
	}
```

- [ ] **Step 4: Implement the form**

`internal/server/templates/appform.tmpl`, in Step 3 (Expose), after the domain field:

```html
        {{if .ProtectNewDomains}}
        <input type="hidden" name="protected_offered" value="1">
        <label class="field-check">
          <input type="checkbox" class="switch" role="switch" name="protected" checked>
          <span>Require sign-in {{template "hint" "People must sign in to Outhaul before they reach this app. Everyone who can sign in gets through, and the app receives their name in the X-Outhaul-User header."}}</span>
        </label>
        {{end}}
```

- [ ] **Step 5: Run the tests**

Run: `go test ./internal/server/ && go test ./...`

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/server
git commit -m "feat(server): a new app requires sign-in when the host says so" -m "The new-app form had no switch and its row was always open. Protect it, refuse the hosts where that would leave someone reachable openly, and remove the app if the gate can't be mapped."
```

---

## Task 5: the template deploy

**Files:**
- Modify: `internal/server/catalog.go:38-130`
- Test: `internal/server/catalog_test.go`

**Interfaces:**
- Consumes: everything Task 4 uses, plus `renderTemplates`.

- [ ] **Step 1: Write the failing tests**

Add to `internal/server/catalog_test.go`:

```go
// With the setting on, every row a template deploy writes requires sign-in.
func TestDeployTemplateProtectsItsRows(t *testing.T) {
	e, _ := newProtectionEnv(t)
	e.srv.protectNew = true
	r := e.postForm(t, "/templates/wordpress", url.Values{"name": {"blog"}, "project_id": {itoa(core.DefaultProjectID)}})
	if r.StatusCode >= 400 {
		t.Fatalf("deploy template: status %d, body %s", r.StatusCode, body(t, r))
	}
	r.Body.Close()
	app, err := e.store.GetAppByName(context.Background(), "blog")
	if err != nil {
		t.Fatal(err)
	}
	rows, _ := e.store.ListDomains(context.Background(), app.ID)
	if len(rows) == 0 {
		t.Fatal("no rows were written")
	}
	for _, d := range rows {
		if !d.Protected {
			t.Errorf("row %s%s is open", d.Host, d.Path)
		}
	}
	if e.deployer.notified == 0 {
		t.Error("the first deploy was not queued")
	}
}

// The template form offers no switch: a request that doesn't say is protected.
func TestDeployTemplateWithoutTheFormsFieldIsProtected(t *testing.T) {
	e, _ := newProtectionEnv(t)
	e.srv.protectNew = true
	r := e.postForm(t, "/templates/wordpress", url.Values{"name": {"blog"}})
	if r.StatusCode >= 400 {
		t.Fatalf("deploy template: status %d, body %s", r.StatusCode, body(t, r))
	}
	r.Body.Close()
	app, _ := e.store.GetAppByName(context.Background(), "blog")
	rows, _ := e.store.ListDomains(context.Background(), app.ID)
	for _, d := range rows {
		if !d.Protected {
			t.Errorf("row %s%s is open", d.Host, d.Path)
		}
	}
}

// A failed gate-host reconcile removes the app and queues no deployment.
func TestDeployTemplateRefusesWhenTheGateHostFails(t *testing.T) {
	e, px := newProtectionEnv(t)
	e.srv.protectNew = true
	px.err = errors.New("docker unreachable")
	r := e.postForm(t, "/templates/wordpress", url.Values{"name": {"blog"}})
	if r.StatusCode < 400 || !strings.Contains(body(t, r), "docker unreachable") {
		t.Fatalf("deploy template: status %d, want a refusal naming the cause", r.StatusCode)
	}
	if _, err := e.store.GetAppByName(context.Background(), "blog"); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("app survived a failed gate host: %v", err)
	}
}

// Another app's old open route on the host is refused, and nothing is written.
func TestDeployTemplateRefusesAnotherAppsOldRoute(t *testing.T) {
	e, _ := newProtectionEnv(t)
	e.srv.protectNew = true
	e.srv.naming = core.Naming{Suffix: "shared.demo.example.com"}
	leftoverHooks(t, e, []core.Domain{{Host: "blog.shared.demo.example.com", Port: 80}}, nil)

	r := e.postForm(t, "/templates/wordpress", url.Values{"name": {"blog"}})
	if r.StatusCode < 400 || !strings.Contains(body(t, r), "hooks") {
		t.Fatalf("deploy template: status %d, want a refusal naming the app that must redeploy", r.StatusCode)
	}
	if _, err := e.store.GetAppByName(context.Background(), "blog"); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("app survived a refused host: %v", err)
	}
}

// With the setting off the rows are open, as they were.
func TestDeployTemplateWithoutTheSettingLeavesRowsOpen(t *testing.T) {
	e := newTestEnv(t)
	e.completeSetup(t)
	r := e.postForm(t, "/templates/wordpress", url.Values{"name": {"blog"}})
	if r.StatusCode >= 400 {
		t.Fatalf("deploy template: status %d, body %s", r.StatusCode, body(t, r))
	}
	r.Body.Close()
	app, _ := e.store.GetAppByName(context.Background(), "blog")
	rows, _ := e.store.ListDomains(context.Background(), app.ID)
	for _, d := range rows {
		if d.Protected {
			t.Errorf("row %s%s is protected with the setting off", d.Host, d.Path)
		}
	}
}
```

`e.deployer` is the `fakeDeployer` the test env holds; `notified` counts `Notify` calls.

- [ ] **Step 2: Run the tests to see them fail**

Run: `go test ./internal/server/ -run 'DeployTemplateProtects|DeployTemplateWithout|DeployTemplateRefuses'`

Expected: FAIL — the rows are open, and a failed reconcile leaves the app behind.

- [ ] **Step 3: Implement**

In `handleDeployTemplate`, after the project check and `catalog.Render`:

```go
	// The form has no sign-in switch: a deploy that doesn't say is protected,
	// as call 19 asks. An explicit protected_offered without protected is the
	// only way to open a template's rows, and no page sends it.
	protect := s.protectNew && (r.FormValue("protected_offered") == "" || r.FormValue("protected") != "")

	hosts := map[string]bool{}
	for _, d := range rendered.Domains {
		host := strings.ToLower(d.Host)
		hosts[host] = true
		if s.isOwnHost(host) {
			s.renderTemplates(w, r, http.StatusBadRequest, ownHostRefusal)
			return
		}
		if protect {
			if ok, reason := s.protectionAvailability(r.Context(), core.Domain{Host: host}); !ok {
				s.renderTemplates(w, r, http.StatusBadRequest, reason)
				return
			}
		}
		refused, err := s.sharedHostRefused(r, projectID, host, true, protect)
		if err != nil {
			s.renderTemplates(w, r, http.StatusInternalServerError, "Could not create app: "+err.Error())
			return
		}
		if refused {
			s.renderTemplates(w, r, http.StatusBadRequest, sharedHostRefusal)
			return
		}
	}
	if protect {
		scope, err := s.scopeFor(r)
		if err != nil {
			log.Printf("deploy template: read the person's projects: %v", err)
			scope = projectScope{}
		}
		others, err := s.leftoverRoutes(r.Context(), core.App{Name: name, ProjectID: projectID}, hosts, scope)
		if err != nil {
			s.renderTemplates(w, r, http.StatusServiceUnavailable,
				fmt.Sprintf("The app wasn't created: Outhaul couldn't tell which other apps still serve old routes on %s (%v).", rendered.Domains[0].Host, err))
			return
		}
		if len(others) > 0 {
			s.renderTemplates(w, r, http.StatusBadRequest, fmt.Sprintf(
				"The app wasn't created: %s still serves an old route on %s with no sign-in. Deploy it from its page, then try again.",
				others[0].label(), others[0].Host))
			return
		}
	}
```

Then the `rows` that were built for `CreateAppWithDomains` carry `Protected: protect`, and after the successful create:

```go
	app, err := s.store.CreateAppWithDomains(...)
	if err != nil { … unchanged … }
	if protect {
		ctx := context.WithoutCancel(r.Context())
		if err := s.ensureGateHost(ctx); err != nil {
			if undo := s.store.DeleteApp(ctx, app.ID); undo != nil {
				log.Printf("deploy template: remove %q after the proxy failed: %v", app.Name, undo)
			}
			s.renderTemplates(w, r, http.StatusServiceUnavailable, fmt.Sprintf(
				"The app wasn't created: Outhaul couldn't map the sign-in gate before it deploys (%v).", err))
			return
		}
	}
```

This must come before the env loop and before `CreateDeployment`, so a refusal leaves neither an app nor a queued deploy. `catalog.go` gains the `context`, `fmt` and `log` imports (`errors`, `store` and `core` arrive with the R9 plan).

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/server/ && go test ./...`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/server
git commit -m "feat(catalog): a template deploy requires sign-in when the host says so" -m "A one-click template landed with open rows. Protect them, refuse a host where someone would stay reachable openly, and remove the app if the gate can't be mapped."
```

---

## Task 6: the first `git push`

**Files:**
- Modify: `internal/githook/bridge.go:24-50,168-220`, `internal/githook/bridge_test.go` (including `bridgeEnv`'s `NewBridge` call), `main.go:995-1015`
- Test: `internal/githook/bridge_test.go`

**Interfaces:**
- Consumes: `store.CreateAppProtected` (Task 2), `store.Store.OtherProjectsOnHost`.
- Produces:

```go
// githook.Store gains
CreateAppProtected(ctx context.Context, app core.App) (core.App, error)
OtherProjectsOnHost(ctx context.Context, host string, projectID int64) (anyRow, protected bool, err error)

// NewBridge gains protectNew, after naming.
func NewBridge(st Store, br *logstream.Broker, n Notifier, repos *gitrepo.Manager, naming core.Naming, protectNew bool, timeout time.Duration) *Bridge
```

- [ ] **Step 1: Write the failing tests**

Add to `internal/githook/bridge_test.go`:

```go
// A cold push under an install that requires sign-in on new domains creates
// the app's own row protected.
func TestBridgeColdPushProtectsTheNewRow(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	b, st, br := bridgeEnv(t)
	b.naming = core.Naming{Suffix: "shared.demo.example.com"}
	b.protectNew = true

	done := make(chan struct{})
	go func() {
		defer close(done)
		app := waitForApp(t, st, "web")
		if app.ID == 0 {
			return
		}
		dep := waitForDeployment(t, st, app.ID)
		markTerminal(t, st, dep, core.StatusRunning)
		br.Close(dep.ID)
	}()
	out := runHandle(t, b, request{App: "web", Refs: []RefUpdate{{"0", pushedCommit(t, b, "web"), "refs/heads/main"}}})
	<-done
	if !strings.Contains(out, "exit 0") {
		t.Fatalf("cold push:\n%s", out)
	}
	app, err := st.GetAppByName(context.Background(), "web")
	if err != nil {
		t.Fatal(err)
	}
	rows, _ := st.ListDomains(context.Background(), app.ID)
	if len(rows) != 1 || !rows[0].Protected {
		t.Errorf("rows = %+v, want one protected", rows)
	}
}

// Another project's row on the host refuses the push, whoever pushes, and
// creates nothing.
func TestBridgeColdPushRefusesAnotherProjectsHost(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	b, st, br := bridgeEnv(t)
	b.naming = core.Naming{Suffix: "shared.demo.example.com"}
	b.protectNew = true
	other, err := st.CreateProject(context.Background(), core.Project{Name: "Other"})
	if err != nil {
		t.Fatal(err)
	}
	theirs, err := st.CreateApp(context.Background(), core.App{Name: "theirs", ProjectID: other.ID, RepoURL: "https://x/y.git"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddDomain(context.Background(), core.Domain{AppID: theirs.ID, Host: "web.shared.demo.example.com", Port: 8080}); err != nil {
		t.Fatal(err)
	}

	out := runHandle(t, b, request{App: "web", Refs: []RefUpdate{{"0", pushedCommit(t, b, "web"), "refs/heads/main"}}})
	if !strings.Contains(out, "another project") || !strings.Contains(out, "push under another name") || !strings.Contains(out, "exit 1") {
		t.Fatalf("want a refusal naming the other project, exit 1:\n%s", out)
	}
	if _, err := st.GetAppByName(context.Background(), "web"); err == nil {
		t.Error("an app was created although the host belongs to another project")
	}
}

// With the setting off the row is open, as it was.
func TestBridgeColdPushWithoutTheSettingLeavesTheRowOpen(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	b, st, br := bridgeEnv(t)
	b.naming = core.Naming{Suffix: "shared.demo.example.com"}

	done := make(chan struct{})
	go func() {
		defer close(done)
		app := waitForApp(t, st, "web")
		if app.ID == 0 {
			return
		}
		dep := waitForDeployment(t, st, app.ID)
		markTerminal(t, st, dep, core.StatusRunning)
		br.Close(dep.ID)
	}()
	runHandle(t, b, request{App: "web", Refs: []RefUpdate{{"0", pushedCommit(t, b, "web"), "refs/heads/main"}}})
	<-done
	app, _ := st.GetAppByName(context.Background(), "web")
	rows, _ := st.ListDomains(context.Background(), app.ID)
	for _, d := range rows {
		if d.Protected {
			t.Errorf("row %s is protected with the setting off", d.Host)
		}
	}
}
```

`bridgeEnv` builds the bridge with `core.Naming{ServerIP: "203.0.113.9"}` and no protection; the tests set the fields directly. `CreateProject` here is the store's, whose `Project` value has `Name`.

- [ ] **Step 2: Run the tests to see them fail**

Run: `go test ./internal/githook/ -run 'ColdPushProtects|RefusesAnotherProjects|WithoutTheSettingLeaves'`

Expected: FAIL — `protectNew` and the store calls are undefined, and the row is open.

- [ ] **Step 3: Implement**

- `internal/githook/bridge.go`: add `protectNew bool` to `Bridge` and the `NewBridge` parameter (comment: `// OUTHAUL_PROTECT_NEW_DOMAINS; see coldCreate`), and add the two methods to the `Store` interface.
- `coldCreate`, after `kind, composePath, err := gitrepo.DetectKind(...)` and before the app value:

```go
	host, hasHost := b.naming.AppHost(req.App)
	if kind == core.KindCompose {
		host, hasHost = "", false
	}
	if b.protectNew && hasHost {
		// A sign-in holds for a whole host, so only a system admin may bring
		// two projects together there. A push has no request to ask with, so
		// ask the store: a cold push is always on the default project.
		anyRow, _, err := b.store.OtherProjectsOnHost(ctx, host, core.DefaultProjectID)
		if err != nil {
			return core.App{}, fmt.Errorf("check who uses %s: %w", host, err)
		}
		if anyRow {
			return core.App{}, fmt.Errorf("an app named '%s' would get %s, which another project already uses; ask a system admin to set it up on the app page, or push under another name",
				req.App, host)
		}
	}
```

  Then `app.Domain = host` when `hasHost` (replacing the `AppHost` line R9 added), and the create call:

```go
	var created core.App
	var err error
	if b.protectNew && hasHost {
		created, err = b.store.CreateAppProtected(ctx, app)
	} else {
		created, err = b.store.CreateApp(ctx, app)
	}
```

The first push checks rows only, not what a container still serves: `leftoverRoutes` reads containers, which the bridge has no reason to list, and the preview manager follows the same rule.

  Keep the reserved-host and taken-host branches below, unchanged.
- `internal/githook/bridge_test.go`: `bridgeEnv` passes `false` for the new `NewBridge` argument.
- `main.go`, `startGitPush`: pass `cfg.ProtectNewDomains` to `githook.NewBridge`, after `naming`.

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/githook/ && go test ./...`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/githook main.go
git commit -m "feat(githook): a first git push requires sign-in when the host says so" -m "A cold push created an open app, so the one path with no page in front of it stayed public. Protect its row and refuse a host another project already uses."
```

---

## Task 7: previews

**Files:**
- Modify: `internal/previewmgr/manager.go:70-110,429-470`, `internal/previewmgr/manager_test.go`, `main.go:351-365`
- Test: `internal/previewmgr/manager_test.go`

**Interfaces:**
- Produces:

```go
// previewmgr.Manager
func (m *Manager) SetProtectNewDomains(on bool) // call before Run
```

- [ ] **Step 1: Write the failing tests**

Add to `internal/previewmgr/manager_test.go`:

```go
// A preview's whole-host row requires sign-in while the server requires it on
// new domains, whatever its parent's row says; a path row copies its parent.
func TestPreviewRowsFollowProtectNewDomains(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.mgr.SetProtectNewDomains(true)
	parent := h.seedGithubApp(t, "web", "acme/site", nil) // one whole-host row, open
	if _, err := h.st.AddDomain(ctx, core.Domain{AppID: parent.ID, Host: "web.example", Path: "/hooks", Port: 8080}); err != nil {
		t.Fatal(err)
	}
	cfg, err := h.st.GetPreviewConfig(ctx, parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	child, err := h.st.CreateApp(ctx, core.App{
		Name: core.PreviewAppName(parent.Name, 1), ParentID: parent.ID, PRNumber: 1,
		Ephemeral: true, Source: core.SourceGithub, GithubRepo: "acme/site", GitSourceID: h.sourceID,
	})
	if err != nil {
		t.Fatal(err)
	}

	on, err := h.mgr.mintDomains(ctx, parent, child, cfg, 1)
	if err != nil {
		t.Fatalf("mintDomains: %v", err)
	}
	if !on {
		t.Error("mintDomains reported no protected row")
	}
	rows, err := h.st.ListDomains(ctx, child.ID)
	if err != nil || len(rows) != 2 {
		t.Fatalf("preview rows = %v (%v), want two", rows, err)
	}
	for _, d := range rows {
		switch d.Path {
		case "":
			if !d.Protected {
				t.Error("the preview's whole-host row is open")
			}
		case "/hooks":
			if d.Protected {
				t.Error("a copied path row should stay as open as its parent's")
			}
		default:
			t.Errorf("unexpected row %+v", d)
		}
	}
}

// With the setting off a preview copies its parent's rows, open ones included.
func TestPreviewRowsCopyWithProtectNewOff(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	parent := h.seedGithubApp(t, "web", "acme/site", nil)
	cfg, _ := h.st.GetPreviewConfig(ctx, parent.ID)
	child, err := h.st.CreateApp(ctx, core.App{
		Name: core.PreviewAppName(parent.Name, 1), ParentID: parent.ID, PRNumber: 1,
		Ephemeral: true, Source: core.SourceGithub, GithubRepo: "acme/site", GitSourceID: h.sourceID,
	})
	if err != nil {
		t.Fatal(err)
	}
	on, err := h.mgr.mintDomains(ctx, parent, child, cfg, 1)
	if err != nil {
		t.Fatal(err)
	}
	if on {
		t.Error("mintDomains reported a protected row with the setting off")
	}
	rows, _ := h.st.ListDomains(ctx, child.ID)
	for _, d := range rows {
		if d.Protected {
			t.Errorf("row %s%s is protected with the setting off", d.Host, d.Path)
		}
	}
}
```

- [ ] **Step 2: Run the tests to see them fail**

Run: `go test ./internal/previewmgr/ -run 'ProtectNewDomains|ProtectNewOff'`

Expected: FAIL — `SetProtectNewDomains` is undefined.

- [ ] **Step 3: Implement**

- `internal/previewmgr/manager.go`: add `protectNew bool // OUTHAUL_PROTECT_NEW_DOMAINS: a preview's whole-host row requires sign-in` to `Manager`, and:

```go
// SetProtectNewDomains makes every preview's whole-host row require sign-in,
// whatever its parent's does. A path row copies its parent's, so a webhook
// path left open stays open. Call it before Run.
func (m *Manager) SetProtectNewDomains(on bool) { m.protectNew = on }
```

- In `mintDomains`:

```go
	for _, d := range parentDomains {
		host := m.naming.PreviewHost(cfg, parent.Name, pr, d.Service)
		rowProtected := d.Protected
		if m.protectNew && d.Path == "" {
			// A whole host is the address people reach; requiring sign-in
			// there is the point of the setting, whatever the parent's rows
			// say. A path lets one route out of a protected host stay open.
			rowProtected = true
		}
		key := host + "\x00" + d.Path
		if i, ok := at[key]; ok {
			rows[i].Protected = rows[i].Protected || rowProtected
			continue
		}
		at[key] = len(rows)
		rows = append(rows, core.Domain{
			AppID: child.ID, Host: host, Service: d.Service, Port: d.Port,
			Path: d.Path, InternalPath: d.InternalPath, TLS: d.TLS, Protected: rowProtected,
		})
	}
```

- `main.go`: `previews.SetProtectNewDomains(cfg.ProtectNewDomains)` beside `previews.SetHTTPSOnly(...)`.

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/previewmgr/ ./internal/server/ && go test ./...`

Expected: PASS, including the existing preview protection tests with the setting off.

- [ ] **Step 5: Commit**

```bash
git add internal/previewmgr main.go
git commit -m "feat(previews): a preview's own address requires sign-in when the host says so" -m "A preview of an open app was open too, so a host that protects new domains still published every pull request. Protect the whole-host row and keep copied path rows as they are."
```

---

## Task 8: Docs

**Files:**
- Modify: `docs/PROTECTED-ROUTES.md`, `docs/SHARED-HOST.md` (created by the R9 plan), `ARCHITECTURE.md`, `AGENTS.md`

- [ ] **Step 1: Write the guide**

In `docs/PROTECTED-ROUTES.md`, add a section "Requiring sign-in on every new domain" after "Requiring sign-in on a domain", covering:

- `OUTHAUL_PROTECT_NEW_DOMAINS=yes`, and that it is off unless set.
- What starts protected: a row added in the dialog (the switch starts on), the new-app form (it shows the switch), a template deploy (no switch; every row is protected), a first `git push`, and a preview's whole-host row.
- A page that offers no switch can't open a row, and a request that never saw the page is protected: nothing is left open because nobody chose.
- Only an explicit "off" on a page that offered the switch opens a row.
- What it needs: `OUTHAUL_PUBLIC_URL`, and HTTPS or a tunnel; `serve` refuses without them, and the tunnel can't be turned off while it is set and nothing else serves HTTPS.
- How to make something public: turn the switch off on the host's row, or add a path row with the switch off under the protected host — pointing at "Leaving a path open" in the same file. Either save redeploys a running app. The same applies to a preview's row.
- Previews: a preview's own address requires sign-in; a path it copies stays as open as its parent's; the parent's dialog says so.
- What a new row costs: another project's host, or an app still serving an old open route, refuses the new-app form and the template deploy; the first push refuses a host another project uses.
- At the end of the section, a `### What only the real host can show` subsection holding the design's item 6, the one this series adds:

```text
6. Each of them — a new app, a template deploy, a preview — and a first git push
   starts with sign-in required. A path row opened for a webhook is reached
   without signing in.
```

  It carries the design's numbering, so the item stays recognisable as item 6; `docs/SHARED-HOST.md` has the same subsection with item 5.

Update the "Previews" section's last paragraph: with the setting on, the dialog says a new preview's own address will require sign-in.

In `docs/SHARED-HOST.md`, add the section "A new row is never left open" pointing at the above, and saying which of the six settings the first shared host sets (the table is R8's/R9's; do not repeat it).

- [ ] **Step 2: Write the kept decision**

`ARCHITECTURE.md`, after the protected-routes section: a short "Sign-in on new domains (opt-in)" paragraph — the setting, that it refuses to start where sign-in can't work, that the gate's host is mapped from startup while it is on, and that the six creation paths are named in `docs/PROTECTED-ROUTES.md`.

`AGENTS.md`, in "Decisions to keep": a bullet — with `OUTHAUL_PROTECT_NEW_DOMAINS` on, a new row is never left open by omission (a request without `protected_offered` is protected), a row that can't be protected isn't created, and `serve` refuses the setting where sign-in can't work.

- [ ] **Step 3: Verify**

Run: `grep -n "OUTHAUL_PROTECT_NEW_DOMAINS" README.md docs/PROTECTED-ROUTES.md docs/SHARED-HOST.md ARCHITECTURE.md AGENTS.md`

Expected: each file names it where the steps say.

Run: `go build ./... && go vet ./... && go test ./...`

Expected: PASS.

- [ ] **Step 4: Run the whole suite once with the race detector**

Run: `go test -race -timeout 30m ./...`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add README.md docs/PROTECTED-ROUTES.md docs/SHARED-HOST.md ARCHITECTURE.md AGENTS.md
git commit -m "docs(protection): sign-in required on every new domain" -m "The setting, the five paths that now protect a row, and how to open one on purpose were only in the design. Add the guide and the kept decision."
```
