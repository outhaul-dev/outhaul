# Default Domains Under One Suffix (R9) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** With `OUTHAUL_DOMAIN_SUFFIX` set, every name Outhaul generates — templates, previews, a first `git push`, the new-app form and the domain dialog — is one label under that suffix, and with `OUTHAUL_DOMAIN_SUFFIX_ONLY` also set, a row outside the suffix is refused whoever writes it.

**Architecture:** One `core.Naming` value (the server IP, the suffix, and whether the suffix is mandatory) replaces the `serverIP string` that `server.New`, `githook.NewBridge`, `previewmgr.New` and `catalog.Render` each carry today, so every place that mints a name asks one value. The store learns the suffix at startup exactly as it learns the admin host (`SetReservedHost`), so the suffix rule is checked in the same transaction as the uniqueness and reserved-host checks. Nothing is stored per row and no migration is needed.

**Tech Stack:** Go 1.26, SQLite (modernc), `html/template` pages, embedded template catalog (`go:embed`), Docker/Traefik labels unchanged.

**Spec:** `docs/superpowers/specs/2026-11-11-shared-host-defaults-design.md`, sections "Settings", "R9: domains under one suffix", "Tests", "Docs", "Order of work", "Left to the builder" and "Calls made in this design" (calls 13 to 18). Code references in this plan are to `c719e7c`.

**Before you start:** nothing else has to land first. R11 (`2026-11-12-protect-new-domains.md`) lands after this plan, because both change `handleCreateApp`, `handleDeployTemplate`, `coldCreate` and the two forms.

## Global Constraints

- The only new settings are `OUTHAUL_DOMAIN_SUFFIX` (a domain, such as `shared.demo.example.com`) and `OUTHAUL_DOMAIN_SUFFIX_ONLY` (yes/no, default no, needs the suffix). Unset means `sslip.io` names and no host rule, exactly as today.
- `Load` does no I/O and keeps a default on a malformed value; `Validate` names the variable and what it wants. `outhaul serve` refuses to start on a `Validate` error.
- `OUTHAUL_DOMAIN_SUFFIX` together with `OUTHAUL_TLS_WILDCARD` must agree: the wildcard must be `*.` and the suffix.
- A generated name is one label under the suffix: a label is at most 63 characters, an app name is 2–40 characters of `a-z`, `0-9` and `-` (`core.AppNameRe`), and the suffix is capped at 189 characters.
- No migration. Nothing about a row changes; both settings are read at startup. (0031 and 0032 belong to other branches.)
- Nothing renames an existing domain. A row already outside the suffix stays, keeps working, and can still be edited in place and deleted.
- A preview base domain an app sets still wins over the suffix.
- The Compose model, the catalog and the pages are not restructured.
- Test first: write the failing test, watch it fail for the stated reason, then implement.
- This repository is public: `example.com` names only, in code, tests and docs.
- After each task: `go build ./... && go vet ./... && go test ./...`. Before calling the plan done: `go test -race -timeout 30m ./...`.
- Commits: `feat(scope): summary` or `fix(scope): summary`, with a body saying what the user saw go wrong and what changed.

## Review Focus

The inputs and failures most likely to bite that no requirement spells out. Each has its test in the task named.

1. A suffix that is uppercase or has a trailing dot, a value one character too long, a value with an underscore, a path or a `*.` in it: refused at startup naming `OUTHAUL_DOMAIN_SUFFIX`, and the old names are kept rather than half-adopted (Task 2).
2. `OUTHAUL_DOMAIN_SUFFIX` and `OUTHAUL_TLS_WILDCARD` naming different domains: refused at startup, naming both, because every generated name would then be covered by no certificate (Task 2).
3. A manifest that uses `${domain}` twice, or `${domain:<label>}`: two distinct rows, each one label under the suffix, so the template deploy never stops with "a domain could not be added" (Task 2).
4. A generated `<app>.<suffix>` another app already serves, or Outhaul's own host: the creation path refuses it and leaves no app behind — template deploy and first push (Task 4), and the new-app form and domain dialog (Task 3).
5. With `OUTHAUL_DOMAIN_SUFFIX_ONLY` on: an outside host is refused for every writer including a system admin, a row already outside is still editable in place and deletable, startup warns about each one, and a preview base domain other than the suffix is refused when saved while an empty one is left alone (Task 5).
6. An install that sets neither setting: the same names, the same dialog options, the same form fields, and no `default` host option (Task 3's `TestNoSuffixKeepsTodaysNamesAndForms`, plus the existing catalog, preview and githook tests).

## File map

| File | Responsibility | Tasks |
|---|---|---|
| `internal/core/naming.go` (new), `naming_test.go` (new) | `core.Naming` and every generated-host shape | 1, 2, 5 |
| `internal/core/preview.go`, `preview_test.go` | `PreviewHost` becomes a `Naming` method | 1 |
| `internal/catalog/expand.go`, `catalog_test.go` | `Render`/`expand`/`generate` take a `Naming`; `${domain:<label>}` | 1, 2 |
| `internal/catalog/templates/minio/template.json` | `api_domain` becomes `${domain:api}` | 2 |
| `internal/config/config.go`, `config_test.go` | both settings, their parsers and validation | 2, 5 |
| `internal/store/domains.go`, `domains_suffix_test.go` (new) | `SetDomainSuffix`, `ErrHostOutsideSuffix`, `DomainsOutsideSuffix` | 5 |
| `internal/githook/bridge.go`, `bridge_test.go` | `Naming` in place of `serverIP`; the cold-push host and refusal | 1, 4 |
| `internal/previewmgr/manager.go`, `manager_test.go` | `Naming` in place of `serverIP`; preview hosts | 1 |
| `internal/server/server.go` | `naming core.Naming` in place of `serverIP`; the page-wide suffix | 1, 3 |
| `internal/server/handlers.go`, `domains_test.go`, `domain_defaults_test.go` (new) | the dialog's default option, the app form's fallback, the outside-suffix refusal | 3, 5 |
| `internal/server/catalog.go`, `catalog_test.go` | the template deploy in one transaction, its refusals | 4 |
| `internal/server/templates/appform.tmpl`, `domainform.tmpl` | prefill, optional domain, the default host option | 3 |
| `internal/server/previews.go`, `previews_test.go` | the base domain must be the suffix | 5 |
| `internal/previewmgr/comment.go`, `manager_test.go` | the preview comment for a host outside the suffix | 5 |
| `main.go`, `main_test.go` | the `Naming` value, the startup suffix rule and its warning | 2, 5 |
| `README.md` | one row per setting | 2, 5 |
| `docs/SHARED-HOST.md` (created by the R8 subnet plan), `ARCHITECTURE.md`, `AGENTS.md` | the guide and the kept decisions | 6 |

---

## Task 1: `core.Naming` in place of every `serverIP`

No generated name changes in this task. It is one compile-enforced swap: the type exists, every caller passes it, and every test that asserted a name still asserts the same string.

**Files:**
- Create: `internal/core/naming.go`, `internal/core/naming_test.go`
- Modify: `internal/core/preview.go:60-75`, `internal/core/preview_test.go`, `internal/catalog/expand.go`, `internal/catalog/catalog_test.go`, `internal/server/server.go:110,176-190,333`, `internal/server/handlers.go:827,1498-1505`, `internal/server/pushdeploy.go:33-35`, `internal/server/protection.go:468`, `internal/server/catalog.go:67`, `internal/server/server_test.go:297`, `internal/server/domains_test.go:88`, `internal/server/protection_test.go:2102,2224,2590`, `internal/server/access_test.go:755`, `internal/githook/bridge.go:40-46,200`, `internal/githook/bridge_test.go:58,287`, `internal/previewmgr/manager.go:78,101-103,437`, `internal/previewmgr/manager_test.go:171`, `main.go:326-329,339,354,381,995,1010`
- Test: `internal/core/naming_test.go`, plus the existing suites

**Interfaces:**
- Produces:

```go
// internal/core/naming.go
type Naming struct {
	ServerIP   string // the host's public IPv4, for sslip.io names; may be empty
	Suffix     string // OUTHAUL_DOMAIN_SUFFIX, lower-cased; "" keeps sslip.io names
	SuffixOnly bool   // OUTHAUL_DOMAIN_SUFFIX_ONLY: refuse hosts outside Suffix
}

// HostUnderSuffix is label under the suffix, or "" when no suffix is set.
func (n Naming) HostUnderSuffix(label string) string

// AppHost is the host an app's first row gets, and whether one can be made.
func (n Naming) AppHost(appName string) (host string, ok bool)

// PreviewHost is core.PreviewHost grown a Naming. Same arguments, one fewer.
func (n Naming) PreviewHost(cfg PreviewConfig, appName string, pr int, service string) string

// internal/catalog/expand.go
func Render(t Template, appName string, naming core.Naming) (Rendered, error)

// internal/server/server.go
// Server.naming core.Naming replaces Server.serverIP string.
func New(..., publicURL string, naming core.Naming, tlsEnabled bool, setupToken string) (*Server, error)

// internal/githook/bridge.go
func NewBridge(st Store, br *logstream.Broker, n Notifier, repos *gitrepo.Manager, naming core.Naming, timeout time.Duration) *Bridge

// internal/previewmgr/manager.go
func New(st Store, n Notifier, db DBProvisioner, dk Docker, gh GithubAPI, ts TokenSource, naming core.Naming) *Manager
```

- [ ] **Step 1: Write the failing naming tests**

Add `internal/core/naming_test.go`, in package `core`:

```go
func TestAppHost(t *testing.T) {
	ip := Naming{ServerIP: "203.0.113.7"}
	if h, ok := ip.AppHost("web"); !ok || h != "web.203.0.113.7.sslip.io" {
		t.Errorf("AppHost with an IP = %q, %v", h, ok)
	}
	if h, ok := (Naming{}).AppHost("web"); ok || h != "" {
		t.Errorf("AppHost with neither = %q, %v; want no host", h, ok)
	}
	shared := Naming{ServerIP: "203.0.113.7", Suffix: "shared.demo.example.com"}
	if h, ok := shared.AppHost("web"); !ok || h != "web.shared.demo.example.com" {
		t.Errorf("AppHost under a suffix = %q, %v; the suffix wins over the IP", h, ok)
	}
}

func TestHostUnderSuffix(t *testing.T) {
	if got := (Naming{}).HostUnderSuffix("web"); got != "" {
		t.Errorf("no suffix = %q, want empty", got)
	}
	if got := (Naming{Suffix: "shared.demo.example.com"}).HostUnderSuffix("web-api"); got != "web-api.shared.demo.example.com" {
		t.Errorf("HostUnderSuffix = %q", got)
	}
}
```

Extend `TestPreviewHost` in `internal/core/preview_test.go` with the suffix case (the rest of that test only gains `naming := Naming{ServerIP: "1.2.3.4"}` and the method call):

```go
	// Under a suffix the label sits under it, whatever the server IP.
	cfg.BaseDomain = ""
	if got := (Naming{ServerIP: "1.2.3.4", Suffix: "shared.demo.example.com"}).PreviewHost(cfg, "web", 42, ""); got != "web-pr-42.shared.demo.example.com" {
		t.Errorf("preview host under a suffix = %q", got)
	}
```

- [ ] **Step 2: Run the tests to see them fail**

Run: `go test ./internal/core/ -run 'AppHost|HostUnderSuffix|PreviewHost'`

Expected: FAIL — `Naming` is undefined, and `PreviewHost` still takes the IP as its fifth argument.

- [ ] **Step 3: Add `core.Naming` and move `PreviewHost` onto it**

In `internal/core/naming.go`, `AppHost` tries `HostUnderSuffix(appName)` first, then `appName + "." + n.ServerIP + ".sslip.io"` when `ServerIP != ""`, then `"", false`.

In `internal/core/preview.go`, delete `func PreviewHost(cfg PreviewConfig, appName string, pr int, service, serverIP string)` and give its body to `func (n Naming) PreviewHost(cfg PreviewConfig, appName string, pr int, service string)`: the label is built and capped exactly as today, `cfg.BaseDomain` still wins, then `n.HostUnderSuffix(label)`, then `label + "." + n.ServerIP + ".sslip.io"`. Keep `maxLabel`, `dnsSafe` and `capLabel` as they are.

- [ ] **Step 4: Thread the value through catalog, the server, the bridge and previews**

- `internal/catalog/expand.go`: `Render(t Template, appName string, naming core.Naming)`, and `expand`/`generate` take `naming core.Naming` in place of `serverIP string`. `generate` still calls `sslipDomain(appName, naming.ServerIP)` and `sslipHost(appName, naming.ServerIP)`; `catalog` now imports `internal/core`.
- `internal/server/server.go`: replace field `serverIP string` with `naming core.Naming`; `New`'s parameter `serverIP string` becomes `naming core.Naming`. In `render` (line 333) add `data["DomainSuffix"] = s.naming.Suffix` beside `CanChange`, so every page gets it without each handler remembering.
- `internal/server/handlers.go`: `"ServerIP": s.naming.ServerIP`; `parseDomainForm` uses `s.naming.ServerIP`.
- `internal/server/pushdeploy.go`: `host := s.naming.ServerIP`.
- `internal/server/protection.go`: `s.naming.PreviewHost(cfg, parent.Name, preview.PRNumber, service)`.
- `internal/githook/bridge.go`: field `naming core.Naming` (comment: `// for cold-push generated hosts; a zero value disables them`), `NewBridge` parameter, and `coldCreate` uses `if host, ok := b.naming.AppHost(req.App); ok && kind != core.KindCompose { app.Domain = host }`.
- `internal/previewmgr/manager.go`: field `naming core.Naming`, `New` parameter, `m.naming.PreviewHost(cfg, parent.Name, pr, d.Service)`.
- `internal/server/catalog.go`: `catalog.Render(tmpl, name, s.naming)`.
- `main.go`: `naming := core.Naming{ServerIP: serverIP}` beside where `serverIP` is resolved, then pass `naming` to `startGitPush`, `previewmgr.New` and `server.New`; `startGitPush`'s `serverIP string` parameter becomes `naming core.Naming`, and `githook.NewBridge` gets it. `main.go` gains the `internal/core` import.

Update the tests the compiler names:

- `internal/server/server_test.go:297`: the `"203.0.113.7"` argument becomes `core.Naming{ServerIP: "203.0.113.7"}`.
- `internal/server/domains_test.go:88`: `e.srv.serverIP = ""` becomes `e.srv.naming.ServerIP = ""`.
- `internal/server/protection_test.go:2102`: `core.PreviewHost(cfg, "web", 42, "", e.srv.serverIP)` becomes `e.srv.naming.PreviewHost(cfg, "web", 42, "")`.
- `internal/server/protection_test.go:2224`: `previewmgr.New(..., e.srv.serverIP)` becomes `previewmgr.New(..., e.srv.naming)`.
- `internal/server/protection_test.go:2590`: `&Server{serverIP: "203.0.113.7"}` becomes `&Server{naming: core.Naming{ServerIP: "203.0.113.7"}}`.
- `internal/server/access_test.go:755`: `previewmgr.New(e.store, nil, nil, nil, nil, nil, "")` passes `core.Naming{}`.
- `internal/previewmgr/manager_test.go:171`: the `serverIP` argument becomes `core.Naming{ServerIP: serverIP}`; the package's `const serverIP = "1.2.3.4"` stays, because the expected hosts still embed it.
- `internal/githook/bridge_test.go:58` passes `core.Naming{ServerIP: "203.0.113.9"}`, and `:287` passes `core.Naming{}`.
- `internal/catalog/catalog_test.go`: `Render(tmpl, "demo", "203.0.113.7")` becomes `Render(tmpl, "demo", core.Naming{ServerIP: "203.0.113.7"})`, and each `expand(..., "203.0.113.7")` becomes `expand(..., core.Naming{ServerIP: "203.0.113.7"})`, with `""` becoming `core.Naming{}`.

- [ ] **Step 5: Run the tests**

Run: `go build ./... && go vet ./... && go test ./...`

Expected: PASS. Every existing name assertion still holds unchanged — that is the proof this task is behaviour-neutral.

- [ ] **Step 6: Commit**

```bash
git add internal/core internal/catalog internal/server internal/githook internal/previewmgr main.go
git commit -m "refactor(core): one Naming value in place of the server IP" -m "Every place that mints a host carried the server IP separately, so a shared suffix would have to reach five constructors. Carry one value instead; no name changes."
```

---

## Task 2: `OUTHAUL_DOMAIN_SUFFIX`, and the names it changes

**Files:**
- Modify: `internal/config/config.go` (the `Config` fields, `Load`, `Validate`, a new `domainSuffix` parser), `internal/config/config_test.go`, `main.go:326-329`, `main_test.go`, `internal/core/naming.go`, `internal/catalog/expand.go`, `internal/catalog/templates/minio/template.json`, `internal/catalog/catalog_test.go`, `internal/githook/bridge.go:200`, `internal/githook/bridge_test.go`, `internal/previewmgr/manager_test.go`, `README.md:275`
- Test: `internal/config/config_test.go`, `internal/core/naming_test.go`, `internal/catalog/catalog_test.go`, `internal/githook/bridge_test.go`, `internal/previewmgr/manager_test.go`

**Interfaces:**
- Consumes: `core.Naming` and its methods from Task 1; `Naming.Suffix` is set for the first time here.
- Produces:

```go
// config.Config
DomainSuffix string // OUTHAUL_DOMAIN_SUFFIX, lower-cased, one trailing dot dropped; "" keeps sslip.io names

// config.go
const maxSuffixLen = 253 - 63 - 1 // room for a 63-character label under a 253-character name
var domainNameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)*$`)
func (p *parser) domainSuffix(key string) string
```

- [ ] **Step 1: Write the failing config tests**

Add to `internal/config/config_test.go`:

```go
func TestDomainSuffixSetting(t *testing.T) {
	long := strings.Repeat("a", 63) + "." + strings.Repeat("b", 63) + "." + strings.Repeat("c", 61) // 189
	tooLong := strings.Repeat("a", 63) + "." + strings.Repeat("b", 63) + "." + strings.Repeat("c", 62)
	cases := []struct{ name, in, want string; bad bool }{
		{"unset", "", "", false},
		{"suffix", "shared.demo.example.com", "shared.demo.example.com", false},
		{"case and a trailing dot", "Shared.Demo.Example.COM.", "shared.demo.example.com", false},
		{"two labels", "example.com", "example.com", false},
		{"189 characters", long, long, false},
		{"190 characters", tooLong, "", true},
		{"one dot", ".", "", true},
		{"underscore", "shared_host.example.com", "", true},
		{"a path", "shared.demo.example.com/app", "", true},
		{"a wildcard", "*.shared.demo.example.com", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			env := map[string]string{}
			if c.in != "" {
				env["OUTHAUL_DOMAIN_SUFFIX"] = c.in
			}
			cfg := Load(func(k string) string { return env[k] })
			err := cfg.Validate()
			if c.bad {
				if err == nil || !strings.Contains(err.Error(), "OUTHAUL_DOMAIN_SUFFIX") {
					t.Fatalf("Validate = %v, want one naming OUTHAUL_DOMAIN_SUFFIX", err)
				}
			} else if err != nil {
				t.Fatalf("Validate: %v", err)
			}
			if cfg.DomainSuffix != c.want {
				t.Errorf("DomainSuffix = %q, want %q", cfg.DomainSuffix, c.want)
			}
		})
	}
}

func TestDomainSuffixMustMatchTheWildcard(t *testing.T) {
	both := map[string]string{
		"OUTHAUL_DOMAIN_SUFFIX": "shared.demo.example.com",
		"OUTHAUL_TLS_WILDCARD":  "*.shared.demo.example.com",
		"OUTHAUL_ACME_EMAIL":    "ops@example.com",
		"OUTHAUL_ACME_DNS_PROVIDER": "azuredns",
	}
	if err := Load(func(k string) string { return both[k] }).Validate(); err != nil {
		t.Fatalf("agreeing wildcard and suffix: %v", err)
	}
	both["OUTHAUL_TLS_WILDCARD"] = "*.other.example.com"
	err := Load(func(k string) string { return both[k] }).Validate()
	if err == nil || !strings.Contains(err.Error(), "OUTHAUL_TLS_WILDCARD") || !strings.Contains(err.Error(), "OUTHAUL_DOMAIN_SUFFIX") {
		t.Fatalf("disagreeing wildcard and suffix: %v, want one naming both", err)
	}
}
```

- [ ] **Step 2: Write the failing naming and template tests**

Add to `internal/core/naming_test.go`:

```go
func TestNamesUnderASuffix(t *testing.T) {
	n := Naming{ServerIP: "203.0.113.7", Suffix: "shared.demo.example.com"}
	if got := n.HostUnderSuffix("web-api"); got != "web-api.shared.demo.example.com" {
		t.Errorf("HostUnderSuffix = %q", got)
	}
	if got := n.PreviewHost(PreviewConfig{}, "web", 42, ""); got != "web-pr-42.shared.demo.example.com" {
		t.Errorf("preview host = %q", got)
	}
}
```

Add to `internal/catalog/catalog_test.go`:

```go
// Under a suffix every template's rows are one label under it and distinct, so
// a manifest that uses ${domain} twice can't have its second row refused.
func TestTemplateRowsUnderASuffix(t *testing.T) {
	all, err := All()
	if err != nil {
		t.Fatal(err)
	}
	naming := core.Naming{Suffix: "shared.demo.example.com"}
	for _, tmpl := range all {
		r, err := Render(tmpl, "demo", naming)
		if err != nil {
			t.Errorf("%s: Render: %v", tmpl.ID, err)
			continue
		}
		seen := map[string]bool{}
		for _, d := range r.Domains {
			if !core.OneLabelUnder(d.Host, naming.Suffix) {
				t.Errorf("%s: host %q is not one label under the suffix", tmpl.ID, d.Host)
			}
			if seen[d.Host] {
				t.Errorf("%s: two rows on %q", tmpl.ID, d.Host)
			}
			seen[d.Host] = true
		}
	}
}

func TestMinioAPIDomainUnderASuffix(t *testing.T) {
	tmpl, err := Get("minio")
	if err != nil {
		t.Fatal(err)
	}
	r, err := Render(tmpl, "minio", core.Naming{Suffix: "shared.demo.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	var hosts []string
	for _, d := range r.Domains {
		hosts = append(hosts, d.Host)
	}
	if len(hosts) != 2 || hosts[0] != "minio.shared.demo.example.com" || hosts[1] != "minio-api.shared.demo.example.com" {
		t.Errorf("MinIO hosts = %q, want minio.… (console) and minio-api.… (api)", hosts)
	}
}
```

`TestTemplateRowsUnderASuffix` names `core.OneLabelUnder`, which this task adds:

```go
// OneLabelUnder reports whether host is exactly one non-empty label under
// suffix, the name a certificate for *.<suffix> covers. Case and one trailing
// dot on either are ignored; the bare suffix and a deeper name are not.
func OneLabelUnder(host, suffix string) bool
```

with its test in `internal/core/naming_test.go`:

```go
func TestOneLabelUnder(t *testing.T) {
	const suffix = "shared.demo.example.com"
	cases := []struct {
		host string
		want bool
	}{
		{"web.shared.demo.example.com", true},
		{"WEB.Shared.Demo.Example.COM", true},
		{"web.shared.demo.example.com.", true},
		{"shared.demo.example.com", false},
		{"a.b.shared.demo.example.com", false},
		{"xshared.demo.example.com", false},
		{"", false},
	}
	for _, c := range cases {
		if got := OneLabelUnder(c.host, suffix); got != c.want {
			t.Errorf("OneLabelUnder(%q) = %v, want %v", c.host, got, c.want)
		}
	}
	if OneLabelUnder("web.example.com", "") {
		t.Error("an empty suffix covers nothing")
	}
}
```

`traefik.WildcardCovers` is deliberately left alone: it already answers a slightly different question, and `TestWildcardCovers` pins a trailing dot as *not* covered, while `OneLabelUnder` normalizes one as the store's row checks do. Do not fold one into the other.

Add to `internal/githook/bridge_test.go`, next to `TestBridgeColdPushRefusesOuthaulsOwnAddress`:

```go
// A cold push under a suffix gives <app>.<suffix>, not an sslip.io name.
func TestBridgeColdPushUsesTheSuffix(t *testing.T) {
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
	out := runHandle(t, b, request{App: "web", Refs: []RefUpdate{{"0", pushedCommit(t, b, "web"), "refs/heads/main"}}})
	<-done
	if !strings.Contains(out, "✓ deployed web.shared.demo.example.com") {
		t.Fatalf("a cold push under a suffix:\n%s", out)
	}
}
```

- [ ] **Step 3: Run the tests to see them fail**

Run: `go test ./internal/config/ ./internal/core/ ./internal/catalog/ ./internal/githook/`

Expected: FAIL — `DomainSuffix`, `OneLabelUnder`, and the MinIO/suffix names are missing.

- [ ] **Step 4: Implement**

- `internal/config/config.go`: add `DomainSuffix string` beside `ServerIP`, `DomainSuffix: p.domainSuffix("OUTHAUL_DOMAIN_SUFFIX")` in `Load`, the parser above (trim, lower-case, drop one trailing dot, then `maxSuffixLen` and `domainNameRe`; on failure `p.reject(key, raw, "a domain such as shared.demo.example.com, at most 189 characters")`), and the wildcard agreement check in `Validate`:

```go
if c.TLSWildcard != "" && c.DomainSuffix != "" && !strings.EqualFold(c.TLSWildcard, "*."+c.DomainSuffix) {
	errs = append(errs, fmt.Errorf("OUTHAUL_DOMAIN_SUFFIX=%q and OUTHAUL_TLS_WILDCARD=%q must name the same domain: the wildcard has to be *.%s", c.DomainSuffix, c.TLSWildcard, c.DomainSuffix))
}
```

- `main.go`: `naming := core.Naming{ServerIP: serverIP, Suffix: cfg.DomainSuffix}`.
- `internal/catalog/expand.go`: with a suffix set, a bare `${domain}` is `naming.HostUnderSuffix(appName)` — `<app>.<suffix>`, the same each time it appears — and `${domain:<label>}` is `naming.HostUnderSuffix(appName + "-" + arg)`, `<app>-<label>.<suffix>`. With no suffix, `${domain}` and `${domain:<label>}` both yield `sslipDomain(appName, naming.ServerIP)`, a fresh random name each time, as today. `${email}` uses `rand + "@" + naming.HostUnderSuffix(appName)` when a suffix is set, else today's `sslipHost`.
- `internal/catalog/templates/minio/template.json`: `"api_domain": "${domain:api}"`. Nothing else in the manifest changes: `console_domain` stays `${domain}`, and both env values follow their variables.
- `internal/githook/bridge.go`: `coldCreate` already reads `b.naming.AppHost` from Task 1, so it gains the suffix here with no further edit. Confirm the existing refusal for Outhaul's own address still names `app.Domain`.
- `README.md`: add the row after `OUTHAUL_SERVER_IP`:

```text
| `OUTHAUL_DOMAIN_SUFFIX` | (empty)             | A domain such as `shared.demo.example.com`; every generated name becomes one label under it — see docs/SHARED-HOST.md |
```

- [ ] **Step 5: Run the tests**

Run: `go build ./... && go vet ./... && go test ./...`

Expected: PASS, including every existing sslip test — they all pass `Naming{ServerIP: …}` with no suffix, which is the no-change proof for generated names.

- [ ] **Step 6: Commit**

```bash
git add internal/config internal/core internal/catalog internal/githook main.go README.md
git commit -m "feat(catalog): OUTHAUL_DOMAIN_SUFFIX, and one label under it for generated names" -m "Templates, previews and a first git push minted sslip.io names that a shared host doesn't route. Add the setting and generate names under it, including a second name per manifest."
```

---

## Task 3: the two forms

**Files:**
- Modify: `internal/server/handlers.go:257-380` (`handleCreateApp`), `:1498-1545` (`parseDomainForm`), the app page's data map (`handlers.go:815-835`), `internal/server/templates/appform.tmpl`, `internal/server/templates/domainform.tmpl`
- Test: `internal/server/domain_defaults_test.go` (new), `internal/server/domains_test.go`

**Interfaces:**
- Consumes: `s.naming` (Task 1), `data["DomainSuffix"]` from `render` (Task 1).
- Produces: page key `DefaultDomain string` on the app page, `app.Name + "." + s.naming.Suffix`, or `""` with no suffix.

- [ ] **Step 1: Write the failing handler tests**

Add `internal/server/domain_defaults_test.go`:

```go
// The dialog's default host is the app's name under the suffix.
func TestAddDomainDefaultKind(t *testing.T) {
	e := newTestEnv(t)
	e.srv.naming = core.Naming{ServerIP: "203.0.113.7", Suffix: "shared.demo.example.com"}
	e.completeSetup(t)
	app, _ := e.store.CreateApp(context.Background(), core.App{Name: "web", RepoURL: "https://x/y.git", Domain: "web.test"})

	r := e.postForm(t, "/apps/"+itoa(app.ID)+"/domains", url.Values{"host_kind": {"default"}})
	if r.StatusCode >= 400 {
		t.Fatalf("default host: status %d, body %s", r.StatusCode, body(t, r))
	}
	r.Body.Close()
	got, _ := e.store.ListDomains(context.Background(), app.ID)
	var hosts []string
	for _, d := range got {
		hosts = append(hosts, d.Host)
	}
	if !slices.Contains(hosts, "web.shared.demo.example.com") {
		t.Errorf("hosts = %q, want one on web.shared.demo.example.com", hosts)
	}

	page := body(t, e.get(t, "/apps/"+itoa(app.ID)))
	if !strings.Contains(page, `value="default"`) {
		t.Error("the dialog does not offer the default host")
	}
	if strings.Contains(page, `value="sslip"`) {
		t.Error("the dialog still offers a free sslip.io domain under a suffix")
	}
}

// Without a suffix the default kind has nothing to offer and says so.
func TestAddDomainDefaultKindNeedsASuffix(t *testing.T) {
	e := newTestEnv(t)
	e.completeSetup(t)
	app, _ := e.store.CreateApp(context.Background(), core.App{Name: "web", RepoURL: "https://x/y.git", Domain: "web.test"})
	r := e.postForm(t, "/apps/"+itoa(app.ID)+"/domains", url.Values{"host_kind": {"default"}})
	if r.StatusCode < 400 || !strings.Contains(body(t, r), "suffix") {
		t.Errorf("default host with no suffix: status %d, want a refusal naming the suffix", r.StatusCode)
	}
}

// A single-container app with no domain takes its name under the suffix.
func TestCreateAppFillsTheDomainUnderASuffix(t *testing.T) {
	e := newTestEnv(t)
	e.srv.naming = core.Naming{ServerIP: "203.0.113.7", Suffix: "shared.demo.example.com"}
	e.completeSetup(t)
	r := e.postForm(t, "/apps", appForm("web", "")) // no domain: the form leaves it optional
	if r.StatusCode >= 400 {
		t.Fatalf("create app: status %d, body %s", r.StatusCode, body(t, r))
	}
	r.Body.Close()
	app, err := e.store.GetAppByName(context.Background(), "web")
	if err != nil {
		t.Fatal(err)
	}
	if app.Domain != "web.shared.demo.example.com" {
		t.Errorf("app domain = %q, want web.shared.demo.example.com", app.Domain)
	}
	rows, _ := e.store.ListDomains(context.Background(), app.ID)
	if len(rows) != 1 || rows[0].Host != "web.shared.demo.example.com" {
		t.Errorf("rows = %+v, want one on web.shared.demo.example.com", rows)
	}
}

// With neither setting an install sees no change: the sslip option, a required
// domain, and no default option.
func TestNoSuffixKeepsTodaysNamesAndForms(t *testing.T) {
	e := newTestEnv(t)
	e.completeSetup(t)
	app, _ := e.store.CreateApp(context.Background(), core.App{Name: "web", RepoURL: "https://x/y.git", Domain: "web.test"})

	page := body(t, e.get(t, "/apps/"+itoa(app.ID)))
	if !strings.Contains(page, `value="sslip"`) {
		t.Error("the dialog no longer offers the free sslip.io domain")
	}
	if strings.Contains(page, `value="default"`) {
		t.Error("the dialog offers a default host without a suffix")
	}
	apps := body(t, e.get(t, "/apps"))
	if strings.Contains(apps, "data-default-domain") {
		t.Error("the app form mentions a default domain without a suffix")
	}
}
```

Add `slices` and `strings` to that file's imports as needed.

- [ ] **Step 2: Run the tests to see them fail**

Run: `go test ./internal/server/ -run 'DefaultKind|FillsTheDomain|NoSuffixKeeps'`

Expected: FAIL — `host_kind=default` is refused by `domainHostRe`, the single-container app is refused for an empty domain, and the pages contain no default option.

- [ ] **Step 3: Implement the dialog**

In `internal/server/handlers.go`:

- `parseDomainForm`'s host_kind switch:

```go
	switch r.FormValue("host_kind") {
	case "default":
		host = s.naming.HostUnderSuffix(app.Name)
		if host == "" {
			return core.Domain{}, "This server has no domain suffix configured, so a default domain can't be generated. Enter a custom domain."
		}
	case "sslip":
		if s.naming.ServerIP == "" {
			return core.Domain{}, "This server has no public IP configured, so a sslip.io domain can't be generated. Enter a custom domain."
		}
		host = app.Name + "." + s.naming.ServerIP + ".sslip.io"
	}
```

  Both kinds then fall through the existing lower-case, `domainHostRe` and `isOwnHost` checks. Keep `sslip` accepted even under a suffix: with the suffix rule on, the store refuses it with the suffix's own message, and an old page keeps working.

- The app page's data map gains:

```go
	if host := s.naming.HostUnderSuffix(app.Name); host != "" {
		data["DefaultDomain"] = host
	}
```

In `internal/server/templates/domainform.tmpl`, put the default option first in Step 1, before the custom one, and leave `custom` checked as it is today:

```html
        {{if .DefaultDomain}}
        <label class="field-check">
          <input type="radio" name="host_kind" value="default">
          <span>Default domain {{template "hint" "This app's name under the server's domain suffix. It resolves here already, so no DNS record is needed."}}</span>
        </label>
        <label class="field" data-host="default" hidden>
          <span>Generated host</span>
          <input type="text" value="{{.DefaultDomain}}" disabled>
        </label>
        {{end}}
```

`applyHostKind` already hides `[data-host]` blocks by kind and requires a host only for `custom`; no script change is needed.

Finally, make the two generated options exclusive: a suffix replaces the free `sslip.io` one (call 17). Change the sslip block's opening condition from `{{if .ServerIP}}` to `{{if and .ServerIP (not .DefaultDomain)}}`. The `sslip` kind stays accepted by the handler, so an old page still works.

- [ ] **Step 4: Implement the app form**

`internal/server/handlers.go`, in `handleCreateApp` after the `core.App` value is built and before `validateApp`:

```go
	// The form offers <name>.<suffix> and leaves the field optional: a
	// single-container app with no domain takes that name.
	if kind != core.KindCompose && app.Domain == "" && s.naming.Suffix != "" {
		app.Domain = name + "." + s.naming.Suffix
	}
```

In `internal/server/templates/appform.tmpl`:

```html
  <form method="post" action="/apps"{{if .DomainSuffix}} data-default-domain="{{.DomainSuffix}}"{{end}}>
```

and the domain field:

```html
        <label class="field">
          <span>Domain {{template "hint" "The hostname Traefik routes to this app, e.g. web.example.com. Leave it blank to use this app's name under the server's domain suffix. For a Compose stack you can leave this blank to keep the stack internal-only."}}</span>
          <input type="text" name="domain" id="domain-input" placeholder="web.example.com" value="{{with .Form}}{{.Domain}}{{end}}"{{if not .DomainSuffix}} required{{end}}>
        </label>
```

and in the script: declare these three above `function applyKind`, because `applyKind` reads `defaultDomain`:

```js
    var defaultDomain = dialog.querySelector('form') ? dialog.querySelector('form').getAttribute('data-default-domain') : '';
    var domainField = document.getElementById('domain-input');
    var domainEdited = !!(domainField && domainField.value);
    var nameField = dialog.querySelector('input[name="name"]');
    // The domain follows the name until it is edited by hand, so a generated
    // name is offered but never overwrites one that was typed.
    if (nameField) nameField.addEventListener('input', function () {
      if (!defaultDomain || domainEdited || !domainField) return;
      if (nameField.value) domainField.value = nameField.value + '.' + defaultDomain;
    });
    if (domainField) domainField.addEventListener('input', function () { domainEdited = true; });
```

and in `applyKind`, replace `if (domain) domain.required = kind.value !== 'compose';` with `if (domain) domain.required = kind.value !== 'compose' && !defaultDomain;`.

- [ ] **Step 5: Run the tests**

Run: `go test ./internal/server/ -run 'DefaultKind|FillsTheDomain|NoSuffixKeeps'`

Expected: PASS. Then `go test ./...`, so the existing domain tests (`TestAddDomainSslipGeneratesHost`, `TestAddDomainSslipRequiresServerIP`) still pass.

- [ ] **Step 6: Commit**

```bash
git add internal/server
git commit -m "feat(server): a default host under the domain suffix, in both forms" -m "The new-app form and the domain dialog only offered a typed host and a dead sslip.io name. Offer the app's name under the suffix, and let a single-container app take it when the field is left blank."
```

---

## Task 4: the template deploy in one transaction, and the collision messages

**Files:**
- Modify: `internal/server/catalog.go:38-110`, `internal/githook/bridge.go:168-215`
- Test: `internal/server/catalog_test.go`, `internal/githook/bridge_test.go`

**Interfaces:**
- Consumes: `store.CreateAppWithDomains(ctx, app, domains)`, `s.takenText`, `s.sharedHostRefused`, `ownHostRefusal`, `store.ErrHostReserved`, `store.DomainTakenError`.

- [ ] **Step 1: Write the failing tests**

Add to `internal/server/catalog_test.go`:

```go
// A template deploy whose generated host another app already serves leaves no
// app behind: the app and its rows are one transaction.
func TestDeployTemplateLeavesNothingOnATakenHost(t *testing.T) {
	e := newTestEnv(t)
	e.completeSetup(t)
	// The template "wordpress" under no suffix gives a random sslip name, so
	// the collision is forced with the app name and a suffix instead.
	e.srv.naming = core.Naming{Suffix: "shared.demo.example.com"}
	squat, _ := e.store.CreateApp(context.Background(), core.App{
		Name: "squat", RepoURL: "https://x/y.git", Kind: core.KindCompose, ComposePath: "docker-compose.yml",
	})
	if _, err := e.store.AddDomain(context.Background(), core.Domain{
		AppID: squat.ID, Host: "blog.shared.demo.example.com", Service: "wordpress", Port: 80,
	}); err != nil {
		t.Fatal(err)
	}

	r := e.postForm(t, "/templates/wordpress", url.Values{"name": {"blog"}})
	if r.StatusCode < 400 {
		t.Fatalf("a taken host was accepted: %d", r.StatusCode)
	}
	r.Body.Close()
	if _, err := e.store.GetAppByName(context.Background(), "blog"); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("app survived a refused route: %v", err)
	}
}

// A template deploy on a host where another project's app requires sign-in is
// refused for anyone but a system admin, before anything is written.
func TestDeployTemplateRefusesAnotherProjectsHost(t *testing.T) {
	e := newTestEnv(t)
	e.completeSetup(t)
	e.srv.naming = core.Naming{Suffix: "shared.demo.example.com"}

	other, err := e.store.CreateProject(context.Background(), core.Project{Name: "Other"})
	if err != nil {
		t.Fatal(err)
	}
	theirs, err := e.store.CreateApp(context.Background(), core.App{
		Name: "theirs", ProjectID: other.ID, RepoURL: "https://x/y.git", Domain: "blog.shared.demo.example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := e.store.ListDomains(context.Background(), theirs.ID)
	if err != nil || len(rows) != 1 {
		t.Fatalf("theirs' rows = %v (%v)", rows, err)
	}
	if err := e.store.SetDomainProtected(context.Background(), theirs.ID, rows[0].ID, true); err != nil {
		t.Fatal(err)
	}

	// A project member on the default project, not a system admin: only an
	// admin may put a route where another project's app requires sign-in.
	tok := e.signInAs(t, "member", core.RoleProjectMember, core.DefaultProjectID)
	r := e.postAs(t, tok, "/templates/wordpress", url.Values{
		"name": {"blog"}, "project_id": {itoa(core.DefaultProjectID)},
	})
	if r.StatusCode < 400 || !strings.Contains(body(t, r), "Another project") {
		t.Fatalf("another project's protected host: status %d, want the shared-host refusal", r.StatusCode)
	}
	if _, err := e.store.GetAppByName(context.Background(), "blog"); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("app survived a refused host: %v", err)
	}
}
```

Add to `internal/githook/bridge_test.go`:

```go
// A cold push whose generated host another app serves is refused, naming the
// host, and creates nothing.
func TestBridgeColdPushRefusesATakenName(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	b, st, br := bridgeEnv(t)
	b.naming = core.Naming{Suffix: "shared.demo.example.com"}
	other, err := st.CreateApp(context.Background(), core.App{Name: "other", RepoURL: "https://x/y.git"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddDomain(context.Background(), core.Domain{
		AppID: other.ID, Host: "web.shared.demo.example.com", Port: 8080,
	}); err != nil {
		t.Fatal(err)
	}

	out := runHandle(t, b, request{App: "web", Refs: []RefUpdate{{"0", pushedCommit(t, b, "web"), "refs/heads/main"}}})
	if !strings.Contains(out, "web.shared.demo.example.com") || !strings.Contains(out, "push under another name") || !strings.Contains(out, "exit 1") {
		t.Fatalf("want a refusal naming the host, exit 1:\n%s", out)
	}
	if _, err := st.GetAppByName(context.Background(), "web"); err == nil {
		t.Error("an app was created although its route was refused")
	}
}
```

- [ ] **Step 2: Run the tests to see them fail**

Run: `go test ./internal/server/ -run 'DeployTemplate'` and `go test ./internal/githook/ -run 'TakenName'`

Expected: FAIL — the template deploy stops after `CreateApp` with "App created, but a domain could not be added", so the app survives; `coldCreate` reports the store's error text without the push advice.

- [ ] **Step 3: Implement**

In `internal/server/catalog.go`'s `handleDeployTemplate`, after the project check and `catalog.Render`:

```go
	// Every host is checked before the app exists, as the new-app form does.
	hosts := map[string]bool{}
	for _, d := range rendered.Domains {
		hosts[strings.ToLower(d.Host)] = true
	}
	for host := range hosts {
		if s.isOwnHost(host) {
			s.renderTemplates(w, r, http.StatusBadRequest, ownHostRefusal)
			return
		}
		refused, err := s.sharedHostRefused(r, projectID, host, true, false)
		if err != nil {
			s.renderTemplates(w, r, http.StatusInternalServerError, "Could not create app: "+err.Error())
			return
		}
		if refused {
			s.renderTemplates(w, r, http.StatusBadRequest, sharedHostRefusal)
			return
		}
	}
```

The `protecting` argument becomes true in R11, when this handler also protects a row.

Then replace the `CreateApp` call and the row loop with one call:

```go
	rows := make([]core.Domain, 0, len(rendered.Domains))
	for _, d := range rendered.Domains {
		rows = append(rows, core.Domain{Host: d.Host, Service: d.Service, Port: d.Port, TLS: true})
	}
	app, err := s.store.CreateAppWithDomains(r.Context(), core.App{
		Name: name, ProjectID: projectID,
		Source: core.SourceTemplate, Kind: core.KindCompose,
		ComposePath: "docker-compose.yml",
		TemplateID:  tmpl.ID, ComposeRaw: tmpl.Compose,
		WebhookToken:  newSecret(),
		WebhookSecret: newSecret(),
	}, rows)
	if err != nil {
		if errors.Is(err, store.ErrHostReserved) {
			s.renderTemplates(w, r, http.StatusBadRequest, ownHostRefusal)
			return
		}
		s.renderTemplates(w, r, http.StatusBadRequest, "Could not create app: "+s.takenText(r, err))
		return
	}
```

`catalog.go` gains the `errors`, `store` and (already imported) `core` imports. The env loop stays as it is: it has nothing to do with rows.

In `internal/githook/bridge.go`'s `coldCreate`, distinguish a taken host:

```go
	created, err := b.store.CreateApp(ctx, app)
	if errors.Is(err, store.ErrHostReserved) {
		return core.App{}, fmt.Errorf("an app named '%s' would get %s, which is Outhaul's own address; push under another name",
			req.App, app.Domain)
	}
	var taken *store.DomainTakenError
	if errors.As(err, &taken) {
		return core.App{}, fmt.Errorf("an app named '%s' would get %s, which another app serves; push under another name",
			req.App, app.Domain)
	}
```

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/server/ ./internal/githook/`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/server/catalog.go internal/server/catalog_test.go internal/githook
git commit -m "fix(catalog): the template deploy creates its app and rows together" -m "A generated name under a suffix can collide, and the deploy then left an app with no domain behind. Create both in one transaction and say which host is taken; the first push says so too."
```

---

## Task 5: `OUTHAUL_DOMAIN_SUFFIX_ONLY`

**Files:**
- Modify: `internal/config/config.go` (field, `Load`, `Validate`), `internal/config/config_test.go`, `internal/core/naming.go`, `internal/core/naming_test.go`, `internal/store/domains.go:41-140`, `internal/store/store.go:26-32`, `main.go:182,807-825`, `main_test.go`, `internal/server/handlers.go:257-380,1498-1545`, `internal/server/domains_test.go`, `internal/server/previews.go:17-40`, `internal/previewmgr/comment.go:44-51`, `README.md`
- Test: `internal/store/domains_suffix_test.go` (new), `internal/config/config_test.go`, `internal/server/previews_test.go`, `internal/previewmgr/manager_test.go`, `main_test.go`

**Interfaces:**
- Consumes: `core.OneLabelUnder` (added in Task 2).
- Produces:

```go
// config.Config
DomainSuffixOnly bool // OUTHAUL_DOMAIN_SUFFIX_ONLY; needs OUTHAUL_DOMAIN_SUFFIX

// store
var ErrHostOutsideSuffix = errors.New("domain is outside the server's domain suffix")
func (s *Store) SetDomainSuffix(suffix string)              // "" allows every host
func (s *Store) DomainsOutsideSuffix(ctx context.Context) ([]core.DomainListing, error)

// core
func (n Naming) SuffixRefusal(host string) string // "" when host is fine

// main.go
func constrainDomains(ctx context.Context, st *store.Store, n core.Naming, logf func(string, ...any))
```

- [ ] **Step 1: Write the failing tests**

Create `internal/store/domains_suffix_test.go`:

```go
func TestSuffixOnlyRefusesEveryWriter(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	st.SetDomainSuffix("shared.demo.example.com")
	shop := testApp(t, st, "shop", core.KindCompose, "")

	for _, host := range []string{"shop.example.com", "a.b.shared.demo.example.com", "shared.demo.example.com"} {
		if _, err := st.AddDomain(ctx, core.Domain{AppID: shop.ID, Host: host, Service: "web", Port: 80}); !errors.Is(err, ErrHostOutsideSuffix) {
			t.Errorf("AddDomain %q err = %v, want ErrHostOutsideSuffix", host, err)
		}
	}
	for _, host := range []string{"shop.shared.demo.example.com", "SHOP.Shared.Demo.Example.COM", "shop.shared.demo.example.com."} {
		if _, err := st.AddDomain(ctx, core.Domain{AppID: shop.ID, Host: host, Service: "web", Port: 80, Path: "/" + host}); err != nil {
			t.Errorf("AddDomain %q: %v", host, err)
		}
	}

	// The primary row a CreateApp seeds is checked too.
	if _, err := st.CreateApp(ctx, core.App{Name: "blog", RepoURL: "https://example.com/b.git", Domain: "blog.example.com"}); !errors.Is(err, ErrHostOutsideSuffix) {
		t.Errorf("CreateApp err = %v, want ErrHostOutsideSuffix", err)
	}
	_, err := st.CreateAppWithDomains(ctx,
		core.App{Name: "stack", RepoURL: "https://example.com/s.git", Kind: core.KindCompose},
		[]core.Domain{{Host: "outside.example.com", Service: "web", Port: 80}})
	if !errors.Is(err, ErrHostOutsideSuffix) {
		t.Errorf("CreateAppWithDomains err = %v, want ErrHostOutsideSuffix", err)
	}
}

func TestSuffixOnlyChecksOnlyAMove(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	old := testApp(t, st, "old", core.KindNixpacks, "old.example.com")
	st.SetDomainSuffix("shared.demo.example.com")
	doms, _ := st.ListDomains(ctx, old.ID)
	d := doms[0]

	d.TLS = !d.TLS
	if err := st.UpdateDomain(ctx, d); err != nil {
		t.Errorf("an edit that doesn't move an outside row: %v", err)
	}
	d.Path = "/other"
	if err := st.UpdateDomain(ctx, d); !errors.Is(err, ErrHostOutsideSuffix) {
		t.Errorf("moving an outside row: err = %v, want ErrHostOutsideSuffix", err)
	}

	got, err := st.DomainsOutsideSuffix(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].AppName != "old" {
		t.Errorf("DomainsOutsideSuffix = %+v, want the old app's row", got)
	}
	if err := st.DeleteDomain(ctx, old.ID, got[0].ID); err != nil {
		t.Fatalf("DeleteDomain: %v", err)
	}
}

func TestNoSuffixAllowsEveryHost(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	testApp(t, st, "first", core.KindNixpacks, "first.example.com")
	if got, err := st.DomainsOutsideSuffix(ctx); err != nil || len(got) != 0 {
		t.Errorf("DomainsOutsideSuffix with nothing set = %+v, %v", got, err)
	}
	st.SetDomainSuffix("shared.demo.example.com")
	st.SetDomainSuffix("")
	if _, err := st.AddDomain(ctx, core.Domain{AppID: 1, Host: "anything.example.com", Port: 8080}); err != nil {
		t.Errorf("AddDomain after clearing the suffix: %v", err)
	}
}
```

Add `TestOneLabelUnder` to `internal/core/naming_test.go` — already added in Task 2; do not duplicate it here.

Add the startup-warning test to `main_test.go`, modelled on `TestStartupReservesTheAdminHost`:

```go
func TestStartupWarnsAboutRowsOutsideTheSuffix(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	old, err := st.CreateApp(ctx, core.App{Name: "old", RepoURL: "https://x/y.git", Domain: "old.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddDomain(ctx, core.Domain{AppID: old.ID, Host: "OLD.example.com", Path: "/api", Port: 8080}); err != nil {
		t.Fatal(err)
	}

	var warnings []string
	logf := func(format string, args ...any) { warnings = append(warnings, fmt.Sprintf(format, args...)) }
	constrainDomains(ctx, st, core.Naming{Suffix: "shared.demo.example.com", SuffixOnly: true}, logf)
	if len(warnings) != 2 {
		t.Fatalf("warnings = %q, want one for each of the old app's two rows", warnings)
	}
	for _, w := range warnings {
		if !strings.HasPrefix(w, "WARNING:") || !strings.Contains(w, `"old"`) || !strings.Contains(strings.ToLower(w), "suffix") {
			t.Errorf("warning %q should name the app and the suffix", w)
		}
	}
	if rows, _ := st.ListDomains(ctx, old.ID); len(rows) != 2 {
		t.Errorf("rows = %+v, want both kept", rows)
	}

	warnings = nil
	constrainDomains(ctx, st, core.Naming{Suffix: "shared.demo.example.com"}, logf) // suffix only off
	if len(warnings) != 0 {
		t.Errorf("warnings with the rule off = %q", warnings)
	}
}
```

Add the server tests to `internal/server/domains_test.go`:

```go
// Under the suffix rule the dialog says why a host wouldn't be routed.
func TestAddDomainOutsideTheSuffixIsRefused(t *testing.T) {
	e := newTestEnv(t)
	e.srv.naming = core.Naming{ServerIP: "203.0.113.7", Suffix: "shared.demo.example.com", SuffixOnly: true}
	e.store.SetDomainSuffix("shared.demo.example.com")
	e.completeSetup(t)
	app, _ := e.store.CreateApp(context.Background(), core.App{Name: "web", RepoURL: "https://x/y.git", Domain: "web.shared.demo.example.com"})

	r := e.postForm(t, "/apps/"+itoa(app.ID)+"/domains", url.Values{"host_kind": {"custom"}, "host": {"web.example.com"}})
	if r.StatusCode < 400 || !strings.Contains(body(t, r), "shared.demo.example.com") {
		t.Errorf("an outside host: status %d, want a refusal naming the suffix", r.StatusCode)
	}
}

// A row already outside the suffix is still editable in place: only a move is
// checked, so sign-in can be turned on for it (or it can be deleted).
func TestOutsideSuffixRowStaysEditableInPlace(t *testing.T) {
	e, _ := newProtectionEnv(t)
	app, err := e.store.CreateApp(context.Background(), core.App{Name: "old", RepoURL: "https://x/y.git", Domain: "old.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := e.store.ListDomains(context.Background(), app.ID)
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows = %v (%v), want one", rows, err)
	}
	row := rows[0]

	// The rule starts after the row exists: this is a row from before it.
	e.store.SetDomainSuffix("shared.demo.example.com")
	e.srv.naming = core.Naming{Suffix: "shared.demo.example.com", SuffixOnly: true}

	r := e.saveDomain(t, app.ID, row, domainForm(row, true))
	if r.StatusCode >= 400 {
		t.Fatalf("editing an outside row in place: status %d, body %s", r.StatusCode, body(t, r))
	}
	r.Body.Close()
	got, err := e.store.GetDomain(context.Background(), app.ID, row.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Protected {
		t.Errorf("row = %+v, want sign-in on", got)
	}

	// Moving it is refused, because the new route would be outside too.
	moved := row
	moved.Host = "elsewhere.example.com"
	moved.Protected = true // as the row is now, so the stale-page check passes
	r = e.saveDomain(t, app.ID, row, domainForm(moved, true))
	if r.StatusCode < 400 || !strings.Contains(body(t, r), "shared.demo.example.com") {
		t.Errorf("moving an outside row: status %d, want a refusal naming the suffix", r.StatusCode)
	}
}
```

`newProtectionEnv`, `domainForm` and `saveDomain` are `protection_test.go`'s: the first signs in, turns TLS on and installs the fake proxy the gate needs, and the other two build the dialog's fields and post the save.

Add the preview-config test to `internal/server/previews_test.go`:
```go
// Under the suffix rule a preview base domain other than the suffix is refused
// when it is saved: the preview's name would be outside it.
func TestPreviewBaseDomainMustBeTheSuffix(t *testing.T) {
	e := newTestEnv(t)
	e.srv.naming = core.Naming{Suffix: "shared.demo.example.com", SuffixOnly: true}
	e.completeSetup(t)
	app, err := e.store.CreateApp(context.Background(), core.App{
		Name: "web", Source: core.SourceGithub, GithubRepo: "me/app", Kind: core.KindNixpacks, Branch: "main",
	})
	if err != nil {
		t.Fatalf("CreateApp: %v", err)
	}

	outside := e.postForm(t, "/apps/"+itoa(app.ID)+"/previews", url.Values{
		"enabled": {"on"}, "base_domain": {"preview.example.com"},
	})
	if outside.StatusCode < 400 {
		t.Errorf("a base domain outside the suffix was accepted: %d", outside.StatusCode)
	}
	outside.Body.Close()
	if cfg, err := e.store.GetPreviewConfig(context.Background(), app.ID); err == nil && cfg.BaseDomain == "preview.example.com" {
		t.Error("the outside base domain was stored")
	}

	inside := e.postForm(t, "/apps/"+itoa(app.ID)+"/previews", url.Values{
		"enabled": {"on"}, "base_domain": {"shared.demo.example.com"},
	})
	if inside.StatusCode >= 400 {
		t.Errorf("the suffix itself was refused: %d", inside.StatusCode)
	}
	inside.Body.Close()
}

// An empty base domain is the default (sslip.io names), so the suffix rule
// leaves it alone: the save works. Such a preview's host is outside the
// suffix, so creating the preview is refused and the PR comment says why.
func TestPreviewSettingsSaveWithoutABaseDomain(t *testing.T) {
	e := newTestEnv(t)
	e.srv.naming = core.Naming{Suffix: "shared.demo.example.com", SuffixOnly: true}
	e.store.SetDomainSuffix("shared.demo.example.com")
	e.completeSetup(t)
	app, err := e.store.CreateApp(context.Background(), core.App{
		Name: "web", Source: core.SourceGithub, GithubRepo: "me/app", Kind: core.KindNixpacks, Branch: "main",
	})
	if err != nil {
		t.Fatalf("CreateApp: %v", err)
	}

	r := e.postForm(t, "/apps/"+itoa(app.ID)+"/previews", url.Values{"enabled": {"on"}})
	if r.StatusCode >= 400 {
		t.Fatalf("saving preview settings with no base domain: status %d, body %s", r.StatusCode, body(t, r))
	}
	r.Body.Close()
	cfg, err := e.store.GetPreviewConfig(context.Background(), app.ID)
	if err != nil || !cfg.Enabled || cfg.BaseDomain != "" {
		t.Errorf("config = %+v (%v), want enabled with no base domain", cfg, err)
	}
}
```

Add the comment test to `internal/previewmgr/manager_test.go`:

```go
func TestSpawnFailedCommentNamesTheSuffixRule(t *testing.T) {
	got := spawnFailedComment(store.ErrHostOutsideSuffix)
	if !strings.Contains(got, "domain suffix") {
		t.Errorf("comment = %q, want it to name the suffix", got)
	}
}
```

- [ ] **Step 2: Run the tests to see them fail**

Run: `go test ./internal/store/ ./internal/core/ . ./internal/server/ ./internal/previewmgr/ -run 'Suffix|OneLabelUnder|OutsideTheSuffix|Preview'`

Expected: FAIL — `DomainSuffixOnly`, `SetDomainSuffix`, `ErrHostOutsideSuffix`, `DomainsOutsideSuffix`, `constrainDomains` and `Naming.SuffixRefusal` are undefined.

- [ ] **Step 3: Implement the setting and the rule**

- `internal/config/config.go`: `DomainSuffixOnly: p.bool("OUTHAUL_DOMAIN_SUFFIX_ONLY")`, and in `Validate`:

```go
if c.DomainSuffixOnly && c.DomainSuffix == "" {
	errs = append(errs, errors.New("OUTHAUL_DOMAIN_SUFFIX_ONLY needs OUTHAUL_DOMAIN_SUFFIX: it refuses every name that isn't one label under one"))
}
```

- `main.go`: `naming := core.Naming{ServerIP: serverIP, Suffix: cfg.DomainSuffix, SuffixOnly: cfg.DomainSuffixOnly}`, and after `reserveAdminHost(...)` add `constrainDomains(context.Background(), st, core.Naming{Suffix: cfg.DomainSuffix, SuffixOnly: cfg.DomainSuffixOnly}, log.Printf)` (the full value is built later, once the IP has been detected). `constrainDomains` calls `st.SetDomainSuffix(n.Suffix)` when `n.SuffixOnly`, then logs one `WARNING:` per `DomainsOutsideSuffix` row (app name, host, path, and that Outhaul leaves it in place and it can still be edited or deleted), and returns at once when `!n.SuffixOnly`.
- `internal/store/store.go`: add `suffix atomic.Value // string: the one suffix new rows must sit under; see SetDomainSuffix`.
- `internal/store/domains.go`: `SetDomainSuffix` stores `normalizeHost(suffix)`; `suffixOutside(host)` mirrors `isReservedHost`; `checkRouteFreeTx` returns `fmt.Errorf("%w: %s", ErrHostOutsideSuffix, d.Host)` after the reserved-host check and before the uniqueness query; `DomainsOutsideSuffix` mirrors `ReservedHostDomains` over `ListAllDomains`.
- `internal/core/naming.go`: `SuffixRefusal(host)` returns `""` unless `n.SuffixOnly && !OneLabelUnder(host, n.Suffix)`, and then:

```go
fmt.Sprintf("Only names one label under %s reach this server, such as demo.%s. %s wouldn't be routed.", n.Suffix, n.Suffix, host)
```

- `internal/server/handlers.go`: leave `parseDomainForm` alone. Its two callers check instead — the edit only when the row moves — so a row already outside the suffix stays editable in place.
  - `handleAddDomain`, after `d, verr := s.parseDomainForm(r, app)` and its error check: `if msg := s.naming.SuffixRefusal(d.Host); msg != "" { http.Error(w, msg, http.StatusBadRequest); return }`.
  - `handleUpdateDomain`: right after `moved := !strings.EqualFold(existing.Host, d.Host) || existing.Path != d.Path`, add `if moved { if msg := s.naming.SuffixRefusal(d.Host); msg != "" { http.Error(w, msg, http.StatusBadRequest); return } }`. An edit that leaves the host and path alone is never checked: that is what keeps a row already outside the suffix editable, which the store's move-only check already promises.
  - `handleCreateApp`, after the primary host and any compose first row are known and before `sharedHostRefused`: check `s.naming.SuffixRefusal` for each of `app.Domain` and `firstDomain.Host` and render the apps page with the message.
- `internal/server/previews.go`: in `handleSavePreviewConfig`, after the bare-hostname check. An empty base domain means `sslip.io` names, which is the default, so it is not refused here; that preview's host is outside the suffix, so creating the preview is refused by the store and the PR comment says why (below):

```go
	if s.naming.SuffixOnly && cfg.BaseDomain != "" && !strings.EqualFold(cfg.BaseDomain, s.naming.Suffix) {
		http.Error(w, "Preview base domain must be "+s.naming.Suffix+": under OUTHAUL_DOMAIN_SUFFIX_ONLY every name has to be one label under it.", http.StatusBadRequest)
		return
	}
```

- `internal/previewmgr/comment.go`: `spawnFailedComment` gains `errors.Is(err, store.ErrHostOutsideSuffix)` in its first branch, with its own sentence: `"✗ Preview could not be created: its name is outside this server's domain suffix, so nothing would route it."`
- `README.md`: add the row after `OUTHAUL_DOMAIN_SUFFIX`:

```text
| `OUTHAUL_DOMAIN_SUFFIX_ONLY` | `no`            | `yes` refuses any domain that isn't exactly one label under `OUTHAUL_DOMAIN_SUFFIX`, whoever writes it — see docs/SHARED-HOST.md |
```

`coldCreate` is deliberately not given its own check: the only host it mints comes from `naming.AppHost`, which is under the suffix by construction, and the store refuses it if the suffix is ever wrong. Say so in one comment there.

- [ ] **Step 4: Run the tests**

Run: `go test ./...`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/config internal/core internal/store internal/server internal/previewmgr main.go main_test.go README.md
git commit -m "feat(store): OUTHAUL_DOMAIN_SUFFIX_ONLY refuses hosts outside the suffix" -m "A typed or generated name outside the suffix is routed nowhere and nothing said so. Refuse it in the writing transaction for every writer, say why in the forms, and warn at startup about rows already outside."
```

---

## Task 6: Docs

**Files:**
- Modify: `docs/SHARED-HOST.md` (created by the R8 subnet plan), `ARCHITECTURE.md` (the template catalog paragraph at `:380-405`, and a section after "Behind a TLS-terminating proxy"), `AGENTS.md` ("Decisions to keep")

- [ ] **Step 1: Write the guide**

`docs/SHARED-HOST.md` already exists: the R8 subnet plan creates it, titled `# A shared host`, with an intro saying the settings are off unless set, and one section, "A fixed subnet and Traefik address". Add a `## Default domains under one suffix` section after that one, and leave the rest of the file alone. It covers:

- Why: only a name one label under the host's wildcard reaches the host, so `sslip.io` names point at the server and are routed nowhere; the free-domain option generated exactly those.
- `OUTHAUL_DOMAIN_SUFFIX=shared.demo.example.com` and what each place becomes: a template lands on `<name>.<suffix>` (`${domain}`), a second name on `<name>-<label>.<suffix>` (`${domain:<label>}`), a preview on `[<service>-]<app>-pr-<n>.<suffix>`, a first `git push` on `<app>.<suffix>`, and the new-app form offers and pre-fills `<name>.<suffix>`.
- The domain dialog's first option becomes "Default domain", and `sslip` stops being offered (it is still accepted from an old page).
- A preview base domain an app sets still wins over the suffix.
- `OUTHAUL_DOMAIN_SUFFIX_ONLY=yes`: every writer is refused an outside host, including a system admin; the message says why; rows that already exist keep working and can still be edited in place and deleted; startup logs one warning per such row; a saved preview base domain other than the suffix is refused, while leaving it empty is left alone — it means sslip.io names, so it is creating such a preview that fails, with a comment saying why.
- Set it with `OUTHAUL_TLS_WILDCARD=*.<suffix>`; Outhaul refuses to start if the two disagree.
- One warning: an app name is 2–40 characters, so `<app>-<label>` must fit in 63; that is why the suffix is capped at 189 characters.
- At the end of the section, a `### What only the real host can show` subsection holding the design's item 5, the one this series adds:

```text
5. A new app is offered <app>.<suffix>, a template lands on <name>.<suffix> and a
   preview on <app>-pr-<n>.<suffix>, all served through the CDN with no new
   certificate. a.b.<suffix> is refused.
```

  The R8 section above keeps its own "Checking the real host" list; this subsection carries the design's numbering, so the item stays recognisable as item 5.

- [ ] **Step 2: Write the locked and kept decisions**

`ARCHITECTURE.md`: name the suffix as the exception in the template catalog paragraph ("The zero-DNS `sslip.io` shape stays the default; `OUTHAUL_DOMAIN_SUFFIX` replaces it, one label under the suffix, and `${domain:<label>}` is the second name a manifest needs"), and add a short section "Default domains under one suffix (opt-in)" saying: it is opt-in; the shape lives in `core.Naming`, which every minter takes; the rule is checked in the store's writing transaction, like the reserved admin host; nothing renames a row.

`AGENTS.md`, in "Decisions to keep": a bullet saying generated names come from `core.Naming`, never from a bare server IP; the suffix rule is checked once in `checkRouteFreeTx` for every writer; a row already outside is kept and stays editable in place; a manifest that wants two names uses `${domain:<label>}`.

- [ ] **Step 3: Verify**

Run: `grep -n "OUTHAUL_DOMAIN_SUFFIX" README.md docs/SHARED-HOST.md ARCHITECTURE.md AGENTS.md`

Expected: each file names it where the steps say.

Run: `go build ./... && go vet ./... && go test ./...`

Expected: PASS.

- [ ] **Step 4: Run the whole suite once with the race detector**

Run: `go test -race -timeout 30m ./...`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add README.md docs/SHARED-HOST.md ARCHITECTURE.md AGENTS.md
git commit -m "docs(domains): default domains under one suffix" -m "The two settings, what each generated name becomes, and what the suffix-only rule refuses were only in the design. Add the guide and the kept decisions."
```
