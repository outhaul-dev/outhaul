# Serving Behind a TLS-Terminating Proxy Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let Outhaul serve apps and its admin UI on a host reached only through a CDN on 443: certificates by DNS-01 with one wildcard, no port 80, a required request header on every route, and an admin port only Traefik can reach.

**Architecture:** One value, `traefik.Routing`, carries the server-wide routing settings to every place that writes a router (app labels, the Compose override, the admin route). Traefik's own flags follow the same settings. A stamp label on routed containers, with a Traefik constraint on it while a header is required, keeps containers with old labels from being served; a check at startup redeploys them.

**Tech Stack:** Go 1.26, Traefik v3.7.6 (Docker and file providers, lego v5.2.2), Docker Engine API, POSIX `sh` for the real-Traefik scripts, Pebble and `pebble-challtestsrv` for ACME.

**Spec:** `docs/superpowers/specs/2026-11-11-proxy-ingress-design.md`. Read it first; this plan argues from it.

## Global Constraints

- An install that sets none of the new settings sees no change: the same labels, the same Traefik flags and config hash, no redeploy at startup. `TestProxyUnchangedWithoutProtectedRoutes` and the golden label test from Task 1 must pass unchanged after every task.
- Settings are `OUTHAUL_*` variables read through the `parser` in `internal/config/config.go`; `Validate` names the variable in every error.
- New variables, exactly: `OUTHAUL_ACME_DNS_PROVIDER`, `OUTHAUL_ACME_DNS_ENV`, `OUTHAUL_TLS_WILDCARD`, `OUTHAUL_HTTP_PORT` (`80` or `off`), `OUTHAUL_REQUIRE_HEADER_NAME`, `OUTHAUL_REQUIRE_HEADER_VALUE`, `OUTHAUL_ADMIN_DIRECT` (`on` or `off`, default `on`).
- The header's value never appears in a log line, an error, a page or a command-line flag other than Traefik's constraint and rules. The header's name appears on no page.
- Outhaul passes Traefik only the variables named in `OUTHAUL_ACME_DNS_ENV`. It never sets `AZURE_AUTH_METHOD`.
- A rule is its address, then the traversal term, then the header term. `priority` is the length of the address alone.
- A protected row's middlewares are unchanged: the gate first, identity headers removed, then path rewrites.
- With HTTP off, no label and no file-provider router names the `web` entrypoint.
- Test first: write the failing test, watch it fail for the stated reason, then implement. Use the existing fakes (`docker.Fake`, `compose.Fake`, the fake runtime and deployer in `internal/server`).
- This repository is public: `example.com` names only, in code, tests, scripts and docs.
- Commits: `feat(scope): summary` or `fix(scope): summary`, with a body saying what someone saw go wrong and what changed. One series per requirement (R2, R1, R3, R4), so they can be opened as separate pull requests.
- Docs travel with the code. A task that adds a setting adds its row to `README.md`'s configuration table and its section to `docs/BEHIND-A-PROXY.md` (created in Task 2, in the plain style of `docs/SSO.md`). Task 13 finishes that document.
- After each task: `go build ./... && go vet ./... && go test ./...`. Before calling the plan done: `go test -race -timeout 30m ./...`, and `shellcheck -s sh --severity=warning deploy/test/*.sh` for the scripts.

## Review Focus

The inputs most likely to bite that no requirement spells out. Each has its test in the task named.

1. A header value containing a backtick, a quote, a space, `$` or a newline: refused at startup, naming the variable, before it can break a rule or the Compose override (Task 6).
2. A host against the wildcard in another case, the bare domain itself, a name two labels deep, a name that ends with the domain but without the dot before it, and a trailing dot: only a name exactly one label under the wildcard is covered (Task 4).
3. A container labelled under an earlier header value: pages still read its address, Traefik ignores it, and the startup check redeploys it (Tasks 6, 7 and 9).
4. Docker unreachable when Outhaul starts: the Traefik reconcile and the stale-container check are retried until they have run once, and with `OUTHAUL_ADMIN_DIRECT=off` the admin UI listens on loopback until the gateway address can be read (Tasks 8, 9 and 11).
5. `OUTHAUL_ACME_DNS_ENV` with a trailing comma, spaces around names, a duplicate, or a name whose variable is set but empty: refused at startup, naming the offending entry (Task 4).

---

## File map

| File | Responsibility | Tasks |
|---|---|---|
| `internal/traefik/routing.go` (new) | `Routing`, `RoutingFrom`, the rule and its terms, `WildcardCovers`, the stamp and constraint | 1, 2, 4, 6, 7 |
| `internal/traefik/labels.go` | `AppLabels`, `RouteLabels`, `RuleAddress` | 1, 2, 4, 6, 7 |
| `internal/traefik/proxy.go` | Traefik's flags, ports, env, admin route, `EnsureProxy` | 2, 4, 6, 7, 8, 11 |
| `internal/compose/override.go` | the Compose override | 1, 7 |
| `internal/config/config.go` | the settings and their validation | 2, 4, 6, 11 |
| `internal/deploy/worker.go`, `pipeline.go`, `pipeline_compose.go` | building `Routing` for a deploy, the image-label refusal, the "is live at" line | 1, 3, 7 |
| `internal/server/ingress.go` (new) | the startup redeploy, and whether Traefik serves a container | 9 |
| `internal/server/handlers.go`, `protection.go`, `domains_page.go`, templates | Restart, the Access column, links, the HTTPS box, the "not served" notice | 3, 9 |
| `internal/docker/client.go`, `real.go`, `fake.go` | reading a network's gateway | 11 |
| `main.go` | wiring, tunnel refusals, retries, the admin listeners | 2, 3, 4, 6, 8, 9, 11 |
| `deploy/test/gatelabels`, `proxyflags` (new), `dnsadapter` (new), `oidcstub` (new) | helpers the scripts run | 5, 10, 12 |
| `deploy/test/integration_ingress.sh`, `integration_proxy.sh` (new) | checks on real Traefik, Pebble and a real Outhaul | 5, 10, 12 |
| `docs/BEHIND-A-PROXY.md` (new) | what each setting does, and what to check on a real host | 2, 3, 4, 6, 7, 9, 11, 13 |

---

## Task 1: Pass the routing settings as one value

No change in output. Everything after this task adds a field to `Routing` instead of another positional argument.

**Files:**
- Create: `internal/traefik/routing.go`, `internal/traefik/golden_test.go`, `internal/traefik/testdata/route_labels.golden.json`, `internal/traefik/testdata/proxy_spec.golden.json`
- Modify: `internal/traefik/labels.go`, `internal/compose/override.go`, `internal/deploy/worker.go:96-110`, `internal/deploy/pipeline.go:524`, `internal/deploy/pipeline_compose.go:70`, `deploy/test/gatelabels/main.go`
- Test: `internal/traefik/labels_test.go`, `internal/compose/compose_test.go`, and the `internal/deploy` tests that call the changed functions

**Interfaces:**
- Produces:

```go
// internal/traefik/routing.go
type Routing struct {
	TLS          bool   // HTTPS routers are written: Let's Encrypt or the local CA is on, and no tunnel is
	CertResolver string // "le" under Let's Encrypt, "" under the local CA
	GateAddress  string // the forward-auth address protected rows call
}
func RoutingFrom(cfg config.Config, tunnelOn bool) Routing // TLS = cfg.TLSEnabled() && !tunnelOn
func (rt Routing) Rule(host, urlPath string) string        // replaces the package-level Rule

// internal/traefik/labels.go
func AppLabels(app core.App, domains []core.Domain, port int, rt Routing) map[string]string
func RouteLabels(router string, d core.Domain, port int, rt Routing) map[string]string

// internal/compose/override.go
func Override(app core.App, domains []core.Domain, network string, rt traefik.Routing) []byte

// internal/deploy/worker.go, replacing effectiveTLS
func (w *Worker) routing(ctx context.Context) traefik.Routing
```

`RouteLabels` reads `d.Host`, `d.Path`, `d.InternalPath`, `d.Protected` and `d.WantsTLS()`; a row gets its HTTPS router when `rt.TLS && d.WantsTLS()`, as its callers decided before. `Worker.routing` keeps `effectiveTLS`'s fallback: when the tunnel setting can't be read, it assumes no tunnel and logs.

- [ ] **Step 1: Write the golden tests against today's code**

`internal/traefik/golden_test.go`, with an `-update` flag that rewrites the files. Today's call is `RouteLabels("r", d.Host, 8080, d.Path, d.InternalPath, d.WantsTLS() && m.tls, m.resolver, d.Protected, gate)`. Rows, each with router `r`, port `8080` and gate `http://host.docker.internal:8080/gate/check`:

```go
var goldenRows = map[string]core.Domain{
	"protected-host": {Host: "wiki.example.com", TLS: true, Protected: true},
	"open-path":      {Host: "wiki.example.com", Path: "/webhooks", TLS: true},
	"protected-path": {Host: "shop.example.com", Path: "/admin", InternalPath: "/", Protected: true},
	"open-host":      {Host: "plain.example.com"},
}
var goldenModes = map[string]struct{ tls bool; resolver string }{
	"no-tls": {false, ""}, "acme": {true, "le"}, "local-ca": {true, ""},
}
// TestRouteLabelsGolden: for every mode and row, RouteLabels' whole map equals
// testdata/route_labels.golden.json["<mode>/<row>"].
//
// TestProxySpecGolden: the four goldenProxyConfigs(), and the acme, local-CA and tunnel ones again with
// AdminHost "outhaul.example.com", each with ProtectedRoutes off and on (14 entries). For each, proxySpec's
// Cmd, Ports, Env, ExtraHosts and outhaul.config-hash equal testdata/proxy_spec.golden.json[name]; for the
// six with an admin host, so does the YAML writeAdminDynamicConfig writes (into a copy of the config whose
// DynamicDir is t.TempDir(); the spec is taken with the fixed path, so the hash is stable).
```

`TestProxyUnchangedWithoutProtectedRoutes` pins four hashes already. This pins the modes it leaves out, before `proxySpec` changes.

- [ ] **Step 2: Generate and commit the golden files**

Run: `go test ./internal/traefik/ -run Golden -update && go test ./internal/traefik/ -run Golden`
Expected: PASS; `route_labels.golden.json` holds 12 entries and `proxy_spec.golden.json` 14.

```bash
git add internal/traefik/golden_test.go internal/traefik/testdata
git commit -m "test(traefik): pin today's labels and Traefik flags for every mode"
```

- [ ] **Step 3: Change the signatures**

Add `routing.go`; change `AppLabels`, `RouteLabels`, `Override` and `Worker.routing` to the signatures above; move `Rule` onto `Routing`. Update `gatelabels` to build a `Routing` from its flags. In the tests, change call sites only: no assertion changes. The golden test's call becomes `RouteLabels("r", d, 8080, Routing{TLS: m.tls, CertResolver: m.resolver, GateAddress: gate})`.

- [ ] **Step 4: Run everything**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: PASS, with no change to either golden file (`git status` shows `testdata` unmodified). "Golden files unmodified" in later tasks means the same check.

- [ ] **Step 5: Commit**

```bash
git commit -am "refactor(traefik): pass the server-wide routing settings as one value"
```

---

## Task 2 (R2): HTTPS only: the setting, Traefik and the routers

**Files:**
- Create: `docs/BEHIND-A-PROXY.md`
- Modify: `internal/config/config.go`, `internal/traefik/routing.go`, `internal/traefik/labels.go`, `internal/traefik/proxy.go`, `main.go` (the local-CA tunnel check in `serve`, `infraController.Enable`, `proxyConfig`), `README.md` (configuration table)
- Test: `internal/config/config_test.go`, `internal/traefik/labels_test.go`, `internal/traefik/proxy_test.go`, `main_test.go`

**Interfaces:**
- Consumes: `Routing`, `RoutingFrom`, `RouteLabels` from Task 1.
- Produces:

```go
// config.Config
HTTPOff bool // OUTHAUL_HTTP_PORT=off

// traefik.Routing gains
HTTPOff bool // no web entrypoint: every row has one router, on websecure

// traefik.ProxyConfig gains
Routing Routing // the settings routers are written from, with no tunnel
```

`RoutingFrom` sets `HTTPOff` from the config. `infraController.proxyConfig` sets `pc.Routing = traefik.RoutingFrom(ic.cfg, false)`. `proxySpec` and `writeAdminDynamicConfig` read from `pc.Routing` only `HTTPOff` and, in later tasks, `Wildcard` and the header; `ACME`, `LocalCA` and `TunnelMode` stay `ProxyConfig`'s own fields.

- [ ] **Step 1: Write the failing config tests**

```go
// TestHTTPPortSetting
//   unset, "80"        -> HTTPOff false, Validate nil
//   "off", "OFF"       -> HTTPOff true (with OUTHAUL_LOCAL_CA=true), Validate nil
//   "8080", "on", "no" -> Validate error containing "OUTHAUL_HTTP_PORT" and "80 or off"
// TestHTTPOffNeedsHTTPS
//   OUTHAUL_HTTP_PORT=off alone -> error containing "OUTHAUL_HTTP_PORT" and "HTTPS"
//   with OUTHAUL_ACME_EMAIL only -> error containing "OUTHAUL_HTTP_PORT" and "port 80"
//   (Let's Encrypt's HTTP challenge needs it; Task 4 lifts this when a DNS provider is set)
```

- [ ] **Step 2: Write the failing Traefik tests**

```go
// TestHTTPOffPublishesOnly443 (local CA and HTTPOff):
//   spec.Ports == []docker.PortMapping{{HostPort: "443", ContainerPort: "443", Proto: "tcp"}}
//   no flag contains "entrypoints.web." ; no flag contains "redirections"
// TestHTTPOffRowHasOneRouterOnWebsecure, for each golden row with Routing{TLS: true, HTTPOff: true}:
//   labels["traefik.http.routers.r.entrypoints"] == "websecure"
//   labels["traefik.http.routers.r.tls"] == "true"
//   no key has the prefix "traefik.http.routers.r-tls."
//   a row with TLS false and Protected false is served the same way
// TestHTTPOffNoLabelNamesWeb: no label value is "web"
// TestHTTPOffKeepsTheGate: RowHasGate(labels, "r") for the protected rows; middlewares equal Task 1's golden ones
// TestHTTPOffWithResolver: Routing{TLS: true, HTTPOff: true, CertResolver: "le"} adds routers.r.tls.certresolver == "le"
// TestHTTPOffAdminRouteIsOnWebsecure: the admin YAML (local CA, HTTPOff) has entryPoints ["websecure"] only
```

- [ ] **Step 3: Write the failing tunnel tests** (`main_test.go`)

```go
// TestEnableTunnelRefusedWithHTTPOff: infraController{cfg: HTTPOff}.Enable(ctx, "tok") returns an error
//   containing "OUTHAUL_HTTP_PORT"; no token is stored (st.TunnelEnabled is false)
// TestHTTPOffRefusesAnEnabledTunnel: httpOffTunnelError(cfg, true) != nil and names "OUTHAUL_HTTP_PORT";
//   httpOffTunnelError(cfg, false) == nil
```

- [ ] **Step 4: Run them to see them fail**

Run: `go test ./internal/config/ ./internal/traefik/ . -run 'HTTPOff|HTTPPort|Tunnel'`
Expected: FAIL (unknown field `HTTPOff`, undefined `httpOffTunnelError`).

- [ ] **Step 5: Implement**

- `config`: a `parser.httpPort(key string) bool` that accepts `""`, `80` and `off` in any case; the two `Validate` rules from Step 1.
- `proxySpec`: with `pc.Routing.HTTPOff` and no tunnel, leave out `--entrypoints.web.address=:80`, the port 80 mapping and the two redirect flags.
- `RouteLabels`: with `rt.HTTPOff && rt.TLS`, write one router named `router` on `websecure` with `tls=true` (and `tls.certresolver` when the resolver isn't empty), for every row whatever `d.TLS` says, and no `-tls` twin.
- `main.go`: `func httpOffTunnelError(cfg config.Config, tunnelOn bool) error`, called in `serve` beside the local-CA check; `Enable` refuses with `cannot enable a Cloudflare Tunnel while OUTHAUL_HTTP_PORT=off; unset it and restart first`.
- `README.md`: add the `OUTHAUL_HTTP_PORT` row. `docs/BEHIND-A-PROXY.md`: one paragraph on what the document is for, and "HTTPS only" (what the setting does, what it needs, that it can't be combined with a tunnel).

- [ ] **Step 6: Run everything**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: PASS, golden files unmodified.

- [ ] **Step 7: Commit**

```bash
git commit -am "feat(traefik): an HTTPS-only mode with no port 80 (OUTHAUL_HTTP_PORT=off)"
```

---

## Task 3 (R2): HTTPS only: pages, links and the deploy log

**Files:**
- Modify: `internal/server/server.go` (a `routing` field), `internal/server/handlers.go:795-797,826,1116` and the add-domain handler, `internal/server/templates/domainform.tmpl:66-76`, `templates/app.tmpl:247`, `templates/domains.tmpl:17,23`, `internal/server/domains_page.go`, `internal/previewmgr/manager.go:355-363`, `internal/deploy/pipeline.go:176`, `internal/deploy/pipeline_compose.go` (the "is live at" line), `main.go`
- Test: `internal/server` (a new `httpsonly_test.go`), `internal/previewmgr/manager_test.go`, `internal/deploy/deploy_test.go`

**Interfaces:**
- Consumes: `traefik.Routing.HTTPOff` from Task 2.
- Produces:

```go
func (s *Server) SetRouting(rt traefik.Routing)  // call before Handler; main passes RoutingFrom(cfg, false)
func (m *previewmgr.Manager) SetHTTPSOnly(on bool) // previews link over https whatever the row says
```

Pages that render domain rows get `HTTPSOnly bool` in their data.

- [ ] **Step 1: Write the failing tests**

```go
// internal/server/httpsonly_test.go, each on a testEnv with srv.SetRouting(traefik.Routing{TLS: true, HTTPOff: true})
// and tlsEnabled true:
// TestHTTPSOnlyLocksTheHTTPSBox: the app page's domain form has the tls checkbox with both "checked" and "disabled",
//   and the text "This server serves HTTPS only."
// TestHTTPSOnlyAddDomainStoresTLS: POST a new domain without a tls field -> the stored row has TLS true
// TestHTTPSOnlyEditKeepsTheStoredFlag: edit a row stored with TLS false -> still false (the box submits nothing)
// TestHTTPSOnlyLinksAreHTTPS: a row stored with TLS false and Protected false is linked as
//   href="https://plain.example.com" on the app page and on /domains
// internal/previewmgr: TestLinkSchemeHTTPSOnly: after SetHTTPSOnly(true), linkScheme of an open, TLS-off row is "https://"
// internal/deploy: TestDoneLineIsHTTPSWhenHTTPOff: with cfg.HTTPOff and the local CA, the build log's last line
//   contains "is live at https://"; with nothing set it still contains "is live at http://"
```

- [ ] **Step 2: Run them to see them fail**

Run: `go test ./internal/server/ -run HTTPSOnly; go test ./internal/previewmgr/ -run HTTPSOnly; go test ./internal/deploy/ -run DoneLine`
Expected: FAIL (undefined `SetRouting`, `SetHTTPSOnly`; `http://` in the log line).

- [ ] **Step 3: Implement**

- Templates: `http{{if or $.HTTPSOnly .TLS (and .Protected $.ProtectedHTTPS)}}s{{end}}` in the three link sites; the box gets `checked disabled` and the note when `.HTTPSOnly`.
- Handlers: on add with HTTPS only, `d.TLS = true`; on edit, keep `existing.TLS`, as the `!s.tlsEnabled` branch does. The preview-row link at `handlers.go:795` follows the same rule as the templates.
- `main.go`: `srv.SetRouting(traefik.RoutingFrom(cfg, false))` and `previews.SetHTTPSOnly(cfg.HTTPOff)`.
- Deploy: both "is live at" lines take their scheme from `w.routing(ctx)`: `https` when `rt.TLS` and `rt.HTTPOff`, otherwise `http` as today.
- `docs/BEHIND-A-PROXY.md`, "HTTPS only": what the pages and links show.

- [ ] **Step 4: Run everything**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git commit -am "feat(server): with HTTP off, every row is shown and linked as HTTPS"
```

---

## Task 4 (R1): DNS-01 and the wildcard

**Files:**
- Modify: `internal/config/config.go`, `internal/traefik/routing.go`, `internal/traefik/labels.go`, `internal/traefik/proxy.go` (`proxySpec`, `writeAdminDynamicConfig`), `main.go` (`proxyConfig`), `README.md`
- Test: `internal/config/config_test.go`, `internal/traefik/routing_test.go` (new), `labels_test.go`, `proxy_test.go`

**Interfaces:**
- Produces:

```go
// config.Config
ACMEDNSProvider string   // OUTHAUL_ACME_DNS_PROVIDER; "" = HTTP-01
ACMEDNSEnv      []string // "NAME=value" for each name in OUTHAUL_ACME_DNS_ENV, in the order given
TLSWildcard     string   // OUTHAUL_TLS_WILDCARD, lower-cased; "" = none

// traefik.Routing gains
Wildcard string

// traefik.ProxyConfig gains
DNSProvider string
DNSEnv      []string

func WildcardCovers(wildcard, host string) bool // internal/traefik/routing.go
```

`Config.ACMEDNSEnv` holds values, so `Config` must never be printed whole; nothing does today.

- [ ] **Step 1: Write the failing config tests**

```go
// TestDNSProviderSetting: "azuredns" with OUTHAUL_ACME_EMAIL -> ok; without the email -> error naming
//   OUTHAUL_ACME_DNS_PROVIDER and OUTHAUL_ACME_EMAIL; "Azure DNS", "azure_dns", "azure;rm" -> error naming the variable
// TestDNSEnvSetting (provider and email set; env has A=1, B=2, EMPTY=""):
//   "A,B" -> ACMEDNSEnv == []string{"A=1", "B=2"}
//   " A , B " -> same (names are trimmed)
//   "A," ; "A,,B" ; "A,A" ; "A B" ; "1A" ; "A,MISSING" ; "A,EMPTY"
//        -> error containing "OUTHAUL_ACME_DNS_ENV" and the offending entry ("MISSING", "EMPTY", "A" for the duplicate)
//   set without a provider -> error naming OUTHAUL_ACME_DNS_PROVIDER
//   no error text contains a variable's value ("1", "2")
// TestTLSWildcardSetting: "*.shared.demo.example.com" -> ok; "*.Shared.Demo.Example.com" -> stored lower-cased;
//   "shared.demo.example.com", "*.com", "*.*.example.com", "*.exa mple.com", "*.example.com." -> error naming the variable;
//   set without a provider -> error naming OUTHAUL_ACME_DNS_PROVIDER
// TestHTTPOffWithDNSChallenge: OUTHAUL_HTTP_PORT=off, an email and a provider -> Validate nil (lifts Task 2's rule)
```

- [ ] **Step 2: Write the failing Traefik tests**

```go
// TestWildcardCovers, wildcard "*.shared.demo.example.com":
//   "app.shared.demo.example.com" true ; "APP.Shared.Demo.Example.COM" true
//   "shared.demo.example.com" false ; "a.b.shared.demo.example.com" false
//   "xshared.demo.example.com" false ; "app.shared.demo.example.com." false
//   "" false ; and with wildcard "" every host is false
// TestDNSChallengeFlags (ACME, DNSProvider "azuredns", DNSEnv {"AZURE_CLIENT_ID=abc"}, staging on):
//   cmd contains "--certificatesresolvers.le.acme.dnschallenge=true" and
//   "--certificatesresolvers.le.acme.dnschallenge.provider=azuredns"; no flag contains "httpchallenge";
//   the email, storage and staging caserver flags are as in TestEnsureProxyTLSFlagsAndMount;
//   spec.Env contains "AZURE_CLIENT_ID=abc"; no flag contains "abc"
// TestDNSEnvChangesTheHash: the same config with "AZURE_CLIENT_ID=xyz" has another outhaul.config-hash
// TestWildcardEntrypointFlags: cmd contains exactly
//   "--entrypoints.websecure.http.tls.certresolver=le" and
//   "--entrypoints.websecure.http.tls.domains[0].main=*.shared.demo.example.com"; absent with no wildcard, and in tunnel mode
// TestCoveredHostInheritsTheWildcard, Routing{TLS: true, CertResolver: "le", Wildcard: "*.shared.demo.example.com"}:
//   HTTP on, host app.shared.demo.example.com: r-tls has entrypoints "websecure" and no key under "traefik.http.routers.r-tls.tls"
//   HTTP off: router r has entrypoints "websecure" and no key under "traefik.http.routers.r.tls"
//   host "other.example.org": tls == "true" and tls.certresolver == "le", as today
// TestAdminRouteUnderTheWildcard: admin host covered -> the YAML has no "tls:" line; not covered -> "certResolver: le"
```

- [ ] **Step 3: Run them to see them fail**

Run: `go test ./internal/config/ ./internal/traefik/ -run 'DNS|Wildcard|Covered|AdminRouteUnder'`
Expected: FAIL (unknown fields, undefined `WildcardCovers`).

- [ ] **Step 4: Implement**

- `config`: provider matches `^[a-z0-9]+$`; an env name matches `^[A-Za-z_][A-Za-z0-9_]*$`; the wildcard is `*.` and at least two labels of `[a-z0-9-]`, none starting or ending with `-`, no trailing dot. Replace Task 2's "port 80" rule with: `off` under Let's Encrypt needs `OUTHAUL_ACME_DNS_PROVIDER`.
- `proxySpec`: in the ACME block, the two `dnschallenge` flags in place of the two `httpchallenge` flags when a provider is set; append `DNSEnv` to the container's env; the two entrypoint flags right after `--entrypoints.websecure.address=:443` when ACME and a wildcard are set.
- `RouteLabels`: a `websecure` router whose host the wildcard covers, when `rt.CertResolver != ""`, gets no `tls` label at all.
- `writeAdminDynamicConfig`: no `tls` block when the wildcard covers the admin host under Let's Encrypt.
- `proxyConfig` in `main.go` fills `DNSProvider` and `DNSEnv`; `RoutingFrom` fills `Wildcard`.
- `README.md`: the three rows. `docs/BEHIND-A-PROXY.md`, "Certificates": DNS-01, the variables Traefik is handed by name, that Traefik needs outbound DNS on port 53, the wildcard and which hosts it covers, what a host outside it gets.

- [ ] **Step 5: Run everything**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: PASS, golden files unmodified, `TestProxyUnchangedWithoutProtectedRoutes` passing.

- [ ] **Step 6: Commit**

```bash
git commit -am "feat(traefik): certificates by DNS-01, and one wildcard for every host under it"
```

---

## Task 5 (R1): Check certificates and ports on a real Traefik

Not run by CI. This task has discovery in it: Pebble's and lego's exact options are found by running them.

**Files:**
- Create: `deploy/test/integration_ingress.sh`, `deploy/test/proxyflags/main.go`, `deploy/test/dnsadapter/main.go`
- Modify: `internal/traefik/proxy.go` (export the flags), `deploy/test/gatelabels/main.go`, `AGENTS.md` ("Build, run, test")

**Interfaces:**
- Produces:

```go
func ProxyFlags(pc ProxyConfig) (cmd, env []string) // internal/traefik: proxySpec's Cmd and Env
```

```
proxyflags [-http-off] [-wildcard W] [-dns-provider P] [-dns-env NAME=value]... [-header NAME=VALUE] cmd|env
    prints Traefik's flags, or its environment, one per line, for an ACME install with those settings
gatelabels gains: -http-off, -wildcard W, -resolver NAME, -header NAME=VALUE   (they fill traefik.Routing)
dnsadapter -listen :8080 -challtestsrv http://HOST:8055
    answers lego's httpreq provider: POST /present and /cleanup with {"fqdn","value"},
    forwarded to challtestsrv's /set-txt and /clear-txt as {"host","value"}
```

- [ ] **Step 1: Add `ProxyFlags` and the two helpers, with a unit test**

```go
// TestProxyFlagsAreTheSpecs: ProxyFlags(pc) returns proxySpec(pc).Cmd and .Env for tlsProxyConfig()
```

Run: `go test ./internal/traefik/ -run ProxyFlags && go build ./deploy/test/...`
Expected: PASS.

- [ ] **Step 2: Get one wildcard issued by hand, inside the script's skeleton**

Follow `integration_gate.sh`'s conventions: POSIX `sh`, containers and one network named `outhaul-ingtest-*`, loopback ports only, removed on exit (failure included), no `traefik.enable` label on any test container (the test Traefik exposes by default and is constrained to `outhaul.ingtest=true`), `PASS`/`FAIL`/`INFO` lines, non-zero exit on any failure. Start:

- `pebble` with `PEBBLE_VA_NOSLEEP=1` and `-dnsserver` pointing at `pebble-challtestsrv`;
- `pebble-challtestsrv`;
- `dnsadapter`;
- Traefik (`INGTEST_TRAEFIK_IMAGE`, default `traefik:v3.7.6`) with the flags `proxyflags ... cmd` prints, the environment `proxyflags ... env` prints, and the script's own additions: the ACME directory (`--certificatesresolvers.le.acme.caserver`), Pebble's root as a trusted CA, and whatever lego's propagation check needs to work against `pebble-challtestsrv` or be turned off. Pass `HTTPREQ_ENDPOINT` through `-dns-env`, so `OUTHAUL_ACME_DNS_ENV`'s path is the one exercised.

Record in the script's header which additions were needed and why. Outhaul itself gets no setting for them.

- [ ] **Step 3: Write the assertions**

1. `acme.json` holds one certificate, for `*.shared.demo.example.com`, and Pebble's log shows one order.
2. Starting three backends on hosts under the wildcard (labels from `gatelabels -wildcard ... -resolver le`) causes no further order.
3. A backend on `other.example.org` causes one more order, for that name.
4. With `-http-off`: `docker port` shows only 443, and Traefik's log has no `EntryPoint doesn't exist` line.
5. A request to an app under the wildcard is answered with a certificate whose subject alternative name is the wildcard (`openssl s_client`, or `curl --cacert` with Pebble's root).
6. Not an assertion, an `INFO` line: start once with no wildcard so a covered host gets its own certificate, then restart Traefik with the wildcard on the same `acme.json`, and report which certificate the host is served with. The spec leaves this unchecked; put the answer in `docs/BEHIND-A-PROXY.md`, "Certificates".

- [ ] **Step 4: Run it**

Run: `sh deploy/test/integration_ingress.sh && shellcheck -s sh --severity=warning deploy/test/integration_ingress.sh`
Expected: every line `PASS`, exit 0, no shellcheck warnings, and `docker ps -a | grep -c outhaul-ingtest` prints 0 afterwards.

- [ ] **Step 5: Commit**

```bash
git add deploy/test internal/traefik AGENTS.md
git commit -m "test(traefik): the wildcard is issued once by DNS-01, checked on real Traefik and Pebble"
```

---

## Task 6 (R3): The required header in every rule

**Files:**
- Modify: `internal/config/config.go`, `internal/traefik/routing.go`, `internal/traefik/labels.go` (`RuleAddress`), `internal/traefik/proxy.go` (`adminDynamicConfig`), `README.md`
- Test: `internal/config/config_test.go`, `internal/traefik/routing_test.go`, `labels_test.go`, `proxy_test.go`, `internal/compose/compose_test.go`

**Interfaces:**
- Produces:

```go
// config.Config
RequireHeaderName, RequireHeaderValue string
func (c Config) HeaderRequired() bool

// traefik.Routing gains
HeaderName, HeaderValue string
func (rt Routing) HeaderRequired() bool
```

- [ ] **Step 1: Write the failing config tests**

```go
// TestRequireHeaderSetting:
//   name "X-Azure-FDID", value "8f7c1a2e-0000-4000-8000-000000000000" -> ok
//   name without value, value without name -> error naming the missing variable
//   names "X Azure", "X_Azure", "X:Azure", "" with a value -> error naming OUTHAUL_REQUIRE_HEADER_NAME
//   values "a`b", `a"b`, "a b", "a$b", "a\nb", "a\\b", and a 257-character value
//        -> error naming OUTHAUL_REQUIRE_HEADER_VALUE, whose text does not contain the value
//   value "Ab0-._~+/=:" -> ok
```

- [ ] **Step 2: Write the failing rule tests**

```go
// rt := Routing{HeaderName: "X-Azure-FDID", HeaderValue: "abc-123"}
// TestRuleCarriesTheHeaderTerm:
//   rt.Rule("wiki.example.com", "/api") ==
//     "Host(`wiki.example.com`) && PathPrefix(`/api`)" + traversalTerm + " && Header(`X-Azure-FDID`, `abc-123`)"
//   Routing{}.Rule(...) is unchanged from Task 1
// TestPriorityIgnoresTheHeaderTerm: routers.r.priority == strconv.Itoa(len("Host(`wiki.example.com`) && PathPrefix(`/api`)"))
// TestRuleAddress, added cases:
//   rt.Rule(h, p) for both rows -> h, p, true
//   the same rule written under value "old-999" and under name "X-Other" -> h, p, true
//   refused: an address with a header term but no traversal term; two header terms; a header term then " && Method(`GET`)";
//            "Header(`X`, ``)" ; "Header(``, `v`)"
//   SameAddress(rule written under "old-999", h, p) is true
// TestEveryRouterCarriesTheHeader: every "….rule" label of AppLabels (no TLS, HTTP on with TLS, HTTP off), every rule
//   in compose.Override, and the admin YAML's rule (HTTPS, and tunnel mode) end with " && Header(`X-Azure-FDID`, `abc-123`)"
// TestComposeOverrideKeepsTheHeaderValue: with value "Ab0-._~+/=:" the override contains that value unchanged
// TestHeaderKeepsTheGate: with rt, the protected golden rows' middlewares equal the golden ones and RowHasGate is true
```

- [ ] **Step 3: Run them to see them fail**

Run: `go test ./internal/config/ ./internal/traefik/ ./internal/compose/ -run 'Header|RuleAddress'`
Expected: FAIL.

- [ ] **Step 4: Implement**

- `config`: name `^[A-Za-z0-9-]+$`; value `^[A-Za-z0-9._~+/=:-]{1,256}$`; both or neither. Errors name the variable and what it wants, never the value.
- `Routing.Rule`: append ``" && Header(`" + name + "`, `" + value + "`)"`` when a header is required.
- `RuleAddress`: after the address, accept nothing, the traversal term, or the traversal term followed by one header term whose name and value are non-empty and hold no backtick.
- `adminDynamicConfig`: the admin rule is ``Host(`h`)`` plus the header term. `writeAdminDynamicConfig` takes it from `pc.Routing`.
- `RoutingFrom` fills the two fields. `README.md`: the two rows. `docs/BEHIND-A-PROXY.md`, "The required header": that Traefik answers 404 without it; that port 80's redirect, when HTTP is on, answers before it is checked; that it is only as good as the network rule in front of the host; that the value can be read in container labels.

- [ ] **Step 5: Run everything**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: PASS, golden files unmodified.

- [ ] **Step 6: Commit**

```bash
git commit -am "feat(traefik): a required request header in every router's rule"
```

---

## Task 7 (R3): The stamp, the constraint, and images that route themselves

**Files:**
- Modify: `internal/traefik/routing.go`, `internal/traefik/labels.go`, `internal/traefik/proxy.go`, `internal/compose/override.go`, `internal/deploy/pipeline.go:78-118` and the image-reuse path
- Test: `internal/traefik/routing_test.go`, `labels_test.go`, `proxy_test.go`, `internal/compose/compose_test.go`, `internal/deploy/fork_build_test.go` (or a new `routing_labels_test.go` beside it)

**Interfaces:**
- Produces:

```go
const IngressLabel = "outhaul.ingress"

// Stamp is "" when none of the header, HTTPOff and Wildcard is set; otherwise 12 hex characters of a
// SHA-256 over those settings (header name lower-cased).
func (rt Routing) Stamp() string

// Constraint is "" unless a header is required; then "Label(`outhaul.ingress`,`<stamp>`)".
func (rt Routing) Constraint() string

// Stale reports whether a container Traefik is asked to route (traefik.enable on) was labelled under
// other settings: its outhaul.ingress label, "" when absent, isn't rt's stamp.
func (rt Routing) Stale(labels map[string]string) bool

// Ignores reports whether Traefik, running with rt's constraint, ignores a container with labels:
// a header is required and the container is stale.
func (rt Routing) Ignores(labels map[string]string) bool
```

- [ ] **Step 1: Write the failing tests**

```go
// TestStamp: Routing{} and Routing{TLS: true, CertResolver: "le", GateAddress: "x"} -> ""
//   each of HTTPOff, Wildcard, HeaderName+HeaderValue alone -> 12 lower-case hex characters, all different
//   changing HeaderValue changes it; "x-azure-fdid" and "X-Azure-FDID" give the same stamp
// TestRoutedContainersCarryTheStamp: AppLabels with domains and compose.Override's services carry
//   outhaul.ingress == rt.Stamp(); AppLabels with no domains (traefik.enable=false) does not
// TestNoStampWithNothingSet: with Routing{TLS: true}, no label key is "outhaul.ingress" (the golden test also covers this)
// TestConstraintFlag: with a header, cmd contains exactly
//   "--providers.docker.constraints=Label(`outhaul.ingress`,`" + rt.Stamp() + "`)" ; with HTTPOff alone, no flag contains "constraints"
// TestStaleAndIgnores, rt with a header:
//   {"traefik.enable":"true"}                                  -> Stale true,  Ignores true
//   {"traefik.enable":"true","outhaul.ingress":"0123456789ab"} -> Stale true,  Ignores true
//   {"traefik.enable":"true","outhaul.ingress":rt.Stamp()}     -> Stale false, Ignores false
//   {"traefik.enable":"false"} and {}                          -> Stale false, Ignores false
//   Routing{HTTPOff: true}: a container with no stamp          -> Stale true,  Ignores false
//   Routing{}: a container with "outhaul.ingress":"0123456789ab" -> Stale true, Ignores false
// internal/deploy:
// TestHeaderRequiredRefusesAnImageThatRoutesItself: cfg with a header; fake.ImageLabelsByRef for the built image holds
//   "traefik.http.routers.x.rule"; the deployment fails with a reason containing "traefik.http.routers.x.rule",
//   and no container was created with traefik.enable=true. The same for a deployment that reuses an image (RollbackOf set).
// TestNoHeaderAllowsAnImageWithTraefikLabels: the same image deploys when no header is required and the app isn't a fork
```

- [ ] **Step 2: Run them to see them fail**

Run: `go test ./internal/traefik/ ./internal/compose/ ./internal/deploy/ -run 'Stamp|Constraint|Stale|RoutesItself|TraefikLabels'`
Expected: FAIL.

- [ ] **Step 3: Implement**

- `AppLabels` (when it has domains) and `Override` add `IngressLabel` when `rt.Stamp() != ""`.
- `proxySpec` adds `--providers.docker.constraints=<Constraint>` after the `exposedbydefault` flag when it isn't empty.
- `pipeline.go`: `refuseRoutingLabels` runs when the app is a fork preview, as now, or when `w.cfg.HeaderRequired()`; in the second case also for a deployment that arrived with an image, before `building -> deploying`. Its message for the non-fork case: `refusing an image that carries traefik labels while a request header is required: <labels>`.
- `docs/BEHIND-A-PROXY.md`, "Images and Compose files with their own Traefik labels": what is refused, and that labels a Compose file writes on a service Outhaul routes aren't checked yet.

- [ ] **Step 4: Run everything**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: PASS, golden files unmodified, `TestProxyUnchangedWithoutProtectedRoutes` passing.

- [ ] **Step 5: Commit**

```bash
git commit -am "feat(traefik): Traefik ignores containers labelled before the required header"
```

---

## Task 8 (R3): Fail closed when Traefik can't be replaced

**Files:**
- Modify: `internal/traefik/proxy.go` (`EnsureProxy`), `main.go` (`ensureInfra`)
- Test: `internal/traefik/proxy_test.go`, `main_test.go`

**Interfaces:**
- Consumes: `Routing.Constraint` from Task 7; `docker.Client.ListImages` to tell whether the image is on the server.
- Produces:

```go
// retryReconcile calls ic.reconcile every interval until it succeeds once or ctx ends, logging each failure.
func (ic *infraController) retryReconcile(ctx context.Context, interval time.Duration)
```

- [ ] **Step 1: Write the failing tests**

```go
// internal/traefik (each starts from a running Traefik made without a header, then asks for one with a header):
// TestHeaderRequiredUsesTheLocalImageWhenThePullFails: f.Images["traefik:v3.7.6"] = true; FailPull set
//   -> EnsureProxy returns nil; the container is a new one whose Cmd has the constraint flag
// TestHeaderRequiredStopsTraefikItCannotReplace: f.Images empty; FailPull set
//   -> EnsureProxy returns an error; the old container is not Running()
// TestEnsureProxyKeepsOldContainerWhenPullFails: unchanged, still passes (no header: the old one keeps running)
// main_test.go:
// TestReconcileIsRetriedWhileAHeaderIsRequired: a docker fake whose first two PullImage calls fail and with no local image;
//   retryReconcile(ctx, time.Millisecond) returns after the third try with a running, constrained Traefik
// TestStartupDoesNotRetryWithoutAHeader: ensureInfra with a failing reconcile and no header starts no retry
//   (the fake sees one reconcile attempt after 50ms)
```

- [ ] **Step 2: Run them to see them fail**

Run: `go test ./internal/traefik/ -run 'HeaderRequired(Uses|Stops)'; go test . -run 'Retr'`
Expected: FAIL.

- [ ] **Step 3: Implement**

- `EnsureProxy`: when the pull fails and `pc.Routing.Constraint() != ""`: if `dc.ListImages(ctx, pc.Image)` returns the image, carry on with it; otherwise stop the existing container (when there is one and it runs) and return the pull error. The pull is only reached when the running Traefik's hash differs, so this is the spec's simpler reading: a Traefik whose configuration differs is stopped, whatever the difference. One whose hash matches is adopted before any pull, as today.
- `ensureInfra`: when the Docker ping or the first reconcile fails and `cfg.HeaderRequired()`, start `go ic.retryReconcile(ctx, 30*time.Second)`. `ensureInfra` takes the worker context for this; pass it from `serve`.

- [ ] **Step 4: Run everything**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git commit -am "fix(traefik): with a header required, a Traefik that can't be replaced is stopped, not left serving"
```

---

## Task 9 (R3): Redeploy at startup, Restart, and what the pages say

**Files:**
- Create: `internal/server/ingress.go`, `internal/server/ingress_test.go`
- Modify: `internal/server/handlers.go:960` (app page) and `restartDeploysInstead` (`:1723-1764`), `internal/server/protection.go` (`leftoverRoutes` `:343`, `strayRoute` `:1036`, `routed` `:1076`), `internal/server/domains_page.go:58`, `internal/server/templates/app.tmpl:17`, `main.go`
- Test: `internal/server/ingress_test.go`

**Interfaces:**
- Consumes: `Server.routing` (Task 3), `Routing.Stale` and `Routing.Ignores` (Task 7), `Server.applyProtection` and `queueProtectionDeploy` (`protection.go:817-880`), `store.GetAppByName`.
- Produces:

```go
// RedeployStale queues a deploy for every running app that has a routed container labelled under other
// routing settings, as a protection change would (applyProtection), and returns the apps it queued for.
// It returns an error, having queued nothing, when the containers can't be listed.
func (s *Server) RedeployStale(ctx context.Context) (apps []string, err error)

// served returns the containers Traefik isn't ignoring under the current constraint.
func (s *Server) served(cs []docker.Container) []docker.Container
```

- [ ] **Step 1: Write the failing tests**

All on a `testEnv` with `srv.SetRouting(rt)`, `rt` having a header unless said otherwise. "Stale" containers carry `traefik.enable=true`, `outhaul.managed=true`, `outhaul.app=<name>` and no stamp.

```go
// TestRedeployStaleReusesTheRunningImage: a Nixpacks app with a running deployment (image outhaul/a:1, not pruned)
//   and a running stale canonical container on that image -> apps == ["a"]; one new queued deployment with
//   Image "outhaul/a:1" and RollbackOf 1; the fake deployer was notified
// TestRedeployStaleBuildsACompose App: a running stale Compose stack -> a new queued deployment with Image ""
// TestRedeployStaleSkips: a stopped app with a stale container; an app whose container has rt.Stamp();
//   an app with a deployment already queued -> nothing new queued for any of them
// TestRedeployStaleDoesNothingWithNothingSet: SetRouting(traefik.Routing{TLS: true}) and unstamped containers
//   -> apps is empty and no deployment row is added
// TestRedeployStaleAfterTheSettingIsRemoved: SetRouting(traefik.Routing{}) and a running container stamped
//   "0123456789ab" -> its app is queued (its rules still ask for the old header)
// TestRedeployStaleNamesAContainerWithNoApp: a stale container whose outhaul.app matches no app -> no error, nothing
//   queued, and the log has one line containing the container's name
// TestRedeployStaleReportsAListError: the runtime's ListContainers fails -> err != nil, nothing queued
// TestRestartDeploysOnAStaleStamp: POST /apps/{id}/restart for an app with a stale container redirects to a new
//   deployment's page, and StartContainer was not called
// TestAppPageSaysNotServed: the app page contains "This app isn't being served" for a running stale container
//   while a header is required; not when the stamp is current; not with HTTPOff alone
// TestAccessOfAnIgnoredContainer: for a protected row whose only running container is stale, the row's Access
//   state equals accessStateFor(d, app.Name, nil, latest): what a stopped app's row shows
// TestIgnoredContainerIsNoLeftoverRoute: another app's stale container with an open route on a host isn't
//   returned by leftoverRoutes for that host
// TestNoPageShowsTheHeader: with header "X-Probe-Id" / "abc-123", the bodies of the app page, /domains and
//   /settings, fetched as a system admin, contain neither string
```

- [ ] **Step 2: Run them to see them fail**

Run: `go test ./internal/server/ -run 'RedeployStale|StaleStamp|NotServed|Ignored'`
Expected: FAIL (undefined `RedeployStale`).

- [ ] **Step 3: Implement**

- `RedeployStale`: list `outhaul.managed=true`; keep containers where `s.routing.Stale(c.Labels)`; group by `outhaul.app`; `GetAppByName`, and for an unknown app log `ingress: container <name> has routes from earlier settings and no app; remove it by hand`; `applyProtection` for the rest. Log per app `ingress: the routing settings changed; redeploying <app> (deployment #<id>, <reusing image X | building the latest code>)`, and one summary line with the count.
- `restartDeploysInstead` and the app page's `RestartDeploys`: true also when any of the app's containers is stale.
- App page: `NotServed` when a container that `answers` is `Ignores`d. Alert text: `This app isn't being served. Its containers were deployed before the server's routing settings changed. Deploy it to bring it back.`
- `served` filters what is handed to `accessStates`, `strayAccess`, `leftoverRoutes` and the Domains page. `liveState` and the mismatch checks keep seeing every container.
- `main.go`: after `ensureInfra` has returned and the worker has started, a goroutine calls `srv.RedeployStale` and tries again every minute until it returns no error.
- `docs/BEHIND-A-PROXY.md`, "When a setting changes": Traefik is recreated; apps are redeployed at startup, and which image or code each kind of app gets; what "This app isn't being served" means; that a Traefik that can't be replaced is stopped while a header is required (Task 8).

- [ ] **Step 4: Run everything**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git commit -am "feat(server): redeploy apps whose containers predate the routing settings, at startup"
```

---

## Task 10 (R3): Check the header and the constraint on a real Traefik

**Files:**
- Modify: `deploy/test/integration_ingress.sh`, `deploy/test/integration_gate.sh`

**Interfaces:**
- Consumes: `gatelabels -header -http-off`, `proxyflags -header` from Task 5.
- Produces: two knobs on `integration_gate.sh`, both off by default so its present run is unchanged:

```
GATETEST_HEADER='Name=value'   labels come from gatelabels -header; every request the script makes carries it
GATETEST_HTTP_OFF=1            labels come from gatelabels -http-off; the Traefiks have no web entrypoint;
                               assertions about plain HTTP are skipped, each with an INFO line saying so
```

- [ ] **Step 1: Add the knobs to `integration_gate.sh`**

Find the one function its requests go through and add the header there; skip nothing else. Run it three ways.

Run: `sh deploy/test/integration_gate.sh; GATETEST_HEADER='X-Probe-Id=abc-123' sh deploy/test/integration_gate.sh; GATETEST_HEADER='X-Probe-Id=abc-123' GATETEST_HTTP_OFF=1 sh deploy/test/integration_gate.sh`
Expected: each exits 0. The first prints the same `PASS` lines as before the change (compare with a run on the previous commit).

- [ ] **Step 2: Add the header and constraint assertions to `integration_ingress.sh`**

With Traefik started from `proxyflags -header X-Probe-Id=abc-123` and backends labelled by `gatelabels -header X-Probe-Id=abc-123`:

1. On 443, with HTTP on and with `-http-off`: the right value gives 200; a wrong value, a missing header, and the right value under another header name each give 404; the name in another case gives 200.
2. A backend labelled without `-header` (no stamp) and one labelled under `-header X-Probe-Id=old-999` (another stamp) have no router: 404 with any header, and Traefik's API (enabled on a loopback port for the test) lists no router for them.
3. A request with the right header and a `..` path, in the spellings `integration_gate.sh` uses, still gets 404.
4. It then runs `integration_gate.sh` in the two header modes from Step 1 and fails if either does.

- [ ] **Step 3: Run it**

Run: `sh deploy/test/integration_ingress.sh && shellcheck -s sh --severity=warning deploy/test/integration_ingress.sh deploy/test/integration_gate.sh`
Expected: every line `PASS`, exit 0, no warnings, no `outhaul-ingtest` or `outhaul-gatetest` container left.

- [ ] **Step 4: Commit**

```bash
git commit -am "test(traefik): the required header and the constraint, checked on a real Traefik"
```

---

## Task 11 (R4): The admin port answers only Traefik and the host

**Files:**
- Modify: `internal/config/config.go`, `internal/docker/client.go`, `real.go`, `fake.go`, `internal/traefik/proxy.go` (`proxySpec`, `ProxyMapsGateHost`), `main.go` (`serve`, `infraController`), `README.md`
- Test: `internal/config/config_test.go`, `internal/docker/network_test.go`, `internal/traefik/proxy_test.go`, `main_test.go`

**Interfaces:**
- Produces:

```go
// config.Config
AdminDirect bool // false with OUTHAUL_ADMIN_DIRECT=off; true by default

// docker.Client
// NetworkGateway returns the IPv4 gateway of the named network: the host's address on its bridge.
NetworkGateway(ctx context.Context, name string) (string, error)
// docker.Fake: Gateways map[string]string; a network with no entry answers "172.30.0.1"; a missing network is an error

// traefik.ProxyConfig gains
GateHost string // the address host.docker.internal maps to; "" = Docker's host-gateway

// main.go
func adminAddrs(cfg config.Config, gateway string) []string
```

- [ ] **Step 1: Write the failing tests**

```go
// config: TestAdminDirectSetting
//   unset, "on", "true" -> AdminDirect true ; "off", "OFF", "false" -> false ; "proxy" -> error naming OUTHAUL_ADMIN_DIRECT
//   off without OUTHAUL_PUBLIC_URL -> error naming both variables
//   off with OUTHAUL_LISTEN_ADDR=127.0.0.1:8080 -> error naming OUTHAUL_LISTEN_ADDR ; with ":9090" -> ok
// docker: TestNetworkGateway, against a faked Engine API as cancel_test.go does: GET /networks/outhaul answering
//   IPAM.Config[0].Gateway "172.30.0.1" -> "172.30.0.1"; a 404 -> an error; no IPv4 config -> an error
// traefik:
// TestGateHostMapping: adminProxyConfig with GateHost "172.30.0.1" -> ExtraHosts == ["host.docker.internal:172.30.0.1"],
//   and its hash differs from the same config without GateHost; with GateHost and neither an admin route nor
//   ProtectedRoutes -> no extra hosts
// TestProxyMapsGateHostWithAGateway: ProxyMapsGateHost is (true, true) for a running Traefik made with GateHost
// main:
// TestAdminAddrs: AdminDirect on, ListenAddr ":8080"   -> [":8080"] whatever the gateway
//                 off, gateway "172.30.0.1"             -> ["127.0.0.1:8080", "172.30.0.1:8080"]
//                 off, gateway "", ListenAddr ":9090"   -> ["127.0.0.1:9090"]
// TestAdminListensOnLoopbackUntilDockerAnswers: with AdminDirect off and a gateway lookup that fails twice then answers
//   "127.0.0.2", the handler is reachable on 127.0.0.1 at once, and on 127.0.0.2 once the lookup succeeds
// TestProxyConfigUsesTheGateway: infraController.proxyConfig with AdminDirect off has GateHost == the fake's gateway
// TestAdminDirectOffNeedsSomethingPublishingIt: adminPublishError(cfg, tunnelOn) is an error naming
//   OUTHAUL_ADMIN_DIRECT when cfg has no HTTPS and the tunnel is off; nil with HTTPS, and with the tunnel on
// TestDisableTunnelRefusedWhenOnlyItPublishesTheAdminUI: with AdminDirect off and no HTTPS, infraController.Disable
//   returns an error naming OUTHAUL_ADMIN_DIRECT and the tunnel stays enabled; with HTTPS it disables as today
// TestWarnsWhenTheHeaderDoesNotCoverTheAdminPort: adminPortWarning(cfg) != "" with a header and AdminDirect on, and
//   contains neither the header's name nor its value; "" with AdminDirect off, and with no header
```

- [ ] **Step 2: Run them to see them fail**

Run: `go test ./internal/config/ ./internal/docker/ ./internal/traefik/ . -run 'AdminDirect|NetworkGateway|GateHost|AdminAddrs|AdminListens|Gateway|AdminPort'`
Expected: FAIL.

- [ ] **Step 3: Implement**

- `config`: `p.boolDefault("OUTHAUL_ADMIN_DIRECT", true)`, and the two `Validate` rules.
- `docker`: `NetworkGateway` through the SDK's network inspect; the first IPv4 gateway in its IPAM config.
- `proxySpec`: where it adds `host.docker.internal:host-gateway`, use `host.docker.internal:<GateHost>` when `GateHost` is set. `ProxyMapsGateHost` treats either mapping as mapped.
- `infraController.reconcile`: with `AdminDirect` off, call `EnsureNetwork` then `NetworkGateway` before building the proxy config, and keep the gateway for `adminAddrs`.
- `serve`: replace `ListenAndServe` with one `net.Listen` and `httpServer.Serve` per address from `adminAddrs`. With `AdminDirect` off and no gateway yet, listen on loopback, then look the gateway up every 5 seconds and add its listener once. A listen error on loopback is fatal, as today's is.
- `serve` refuses to start with `adminPublishError`'s message when it returns one, and logs `adminPortWarning`'s text when it isn't empty: `WARNING: a request header is required on every route, but the admin port listens on every interface, where Traefik doesn't check it. Set OUTHAUL_ADMIN_DIRECT=off, or keep other machines from that port.`
- `infraController.Disable` refuses when `adminPublishError(ic.cfg, false)` returns an error: turning the tunnel off would leave the admin UI on loopback only. The spec doesn't name this case; it follows from the setting's "Needs".
- `README.md`: the row. `docs/BEHIND-A-PROXY.md`, "The admin port": where Outhaul listens with the setting off, first-boot setup through the public URL, that app containers on Outhaul's network can still reach the port, and that the required header covers the admin UI only with this setting off.

- [ ] **Step 4: Run everything**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: PASS, `TestProxyUnchangedWithoutProtectedRoutes` and `TestProtectedRoutesMapTheGateHost` passing unchanged.

- [ ] **Step 5: Commit**

```bash
git commit -am "feat(server): with OUTHAUL_ADMIN_DIRECT=off the admin port answers only Traefik and the host"
```

---

## Task 12 (R4): The whole thing through a stand-in proxy

Not run by CI. Nothing has run a real Outhaul behind a real Traefik end to end before, so expect this task to find things. Each thing it finds is fixed test-first in the package it belongs to, in its own commit.

**Files:**
- Create: `deploy/test/integration_proxy.sh`, `deploy/test/oidcstub/main.go`, `deploy/test/Dockerfile.proxytest`
- Modify: `docs/SSO.md` and `docs/PROTECTED-ROUTES.md` ("Testing so far"), `AGENTS.md`

**Interfaces:**
- Produces:

```
oidcstub -listen :9000 -issuer https://idp.shared.demo.example.com -client-id ID -client-secret SECRET -email dana@example.com
    a stand-in OIDC provider: discovery, an authorization endpoint that redirects straight back with a code,
    a token endpoint, and a JWKS, modelled on the idp type in internal/sso/sso_test.go
```

- [ ] **Step 1: Build the environment**

`Dockerfile.proxytest`: Debian 13 with systemd, Docker and the compose plugin, as `Dockerfile.debian` is built and run (`--privileged`, its own Docker daemon), plus the `outhaul` binary built from the checkout. Outhaul needs its own daemon because its Traefik container's name is fixed. The script, in `integration_gate.sh`'s conventions, starts that container as `outhaul-proxytest-*` and removes it on exit.

Inside it, Outhaul runs with:

```
OUTHAUL_PUBLIC_URL=https://outhaul.shared.demo.example.com
OUTHAUL_LOCAL_CA=true
OUTHAUL_HTTP_PORT=off
OUTHAUL_REQUIRE_HEADER_NAME=X-Probe-Id
OUTHAUL_REQUIRE_HEADER_VALUE=abc-123
OUTHAUL_ADMIN_DIRECT=off
```

The stand-in CDN is nginx in the inner daemon: it terminates TLS with its own certificate, forwards over HTTPS with the request's Host as both Host and SNI (`proxy_ssl_server_name on; proxy_ssl_name $host;`), verifies the origin against the local CA's root (`outhaul ca root`), and sets `X-Probe-Id` itself, overwriting any the client sent. All requests go through it with `curl --resolve`.

- [ ] **Step 2: Write the assertions**

1. The setup URL from the log, opened through the stand-in, creates the first system admin; password sign-in then works and the session cookie is `Secure`.
2. With `oidcstub` connected on Settings, "Sign in with SSO" ends signed in as `dana@example.com`, and the callback URL the stub was sent is under the public URL.
3. A template app deployed to `app.shared.demo.example.com` with sign-in required: an anonymous page load is redirected to the public URL to sign in; after signing in, the app receives `X-Outhaul-User`; the `__Host-outhaul_route` cookie is set on the app's host; an open path row under it answers without sign-in.
4. A push delivery signed with the app's webhook secret, posted through the stand-in to its webhook URL, queues a deployment.
5. Straight to Traefik's 443, past the stand-in: no header gives 404, a wrong value gives 404, for the app and for the admin host.
6. The admin port: `curl http://127.0.0.1:8080/healthz` inside the container answers; a connection to the container's main address on 8080 is refused; `ss -ltn` lists 8080 on two addresses only.
7. After restarting Outhaul with `OUTHAUL_REQUIRE_HEADER_VALUE=def-456` (and the stand-in sending it): the old value gives 404 at once; the app answers again without anyone deploying it; the log has the `ingress:` line naming it.

- [ ] **Step 3: Run it, and fix what it finds**

Run: `sh deploy/test/integration_proxy.sh && shellcheck -s sh --severity=warning deploy/test/integration_proxy.sh`
Expected: every line `PASS`, exit 0, no warnings, no `outhaul-proxytest` container left.

For each failure that is a defect in Outhaul: write the failing unit test in the owning package, fix, re-run `go test ./...` and the script, commit as `fix(scope): …` with what the script showed.

- [ ] **Step 4: Record what was run**

Update "Testing so far" in `docs/SSO.md` and `docs/PROTECTED-ROUTES.md`: what this script covers (a real Outhaul and Traefik, a stand-in proxy and a stand-in provider), and what still hasn't been run (a real provider, a real CDN, a real browser). Add the script to `AGENTS.md`'s "Build, run, test".

- [ ] **Step 5: Commit**

```bash
git add deploy/test docs AGENTS.md
git commit -m "test(server): sign-in, protected routes and webhooks through a stand-in proxy, end to end"
```

---

## Task 13: Docs

**Files:**
- Modify: `docs/BEHIND-A-PROXY.md`, `README.md` (a link from the configuration section), `docs/PROTECTED-ROUTES.md` ("Before you start", "A CDN in front"), `ARCHITECTURE.md`, `AGENTS.md` ("Decisions to keep")

- [ ] **Step 1: Finish `docs/BEHIND-A-PROXY.md`**

Tasks 2 to 11 wrote a section each. Add what belongs to no one task, and read the whole as one document:

1. At the top: the request path through a CDN and why each setting exists (spec: "Why"), then the settings table and the example block from the spec's "Settings", with its `example.com` values.
2. Order the sections: Certificates, HTTPS only, The required header, When a setting changes, The admin port, Images and Compose files with their own Traefik labels.
3. "Checking a real host": the seven-item list from the spec's "What only the real host can show".
4. "Testing so far": the three scripts, what each covers, and what none of them can show (a real CDN, a real DNS provider, a real identity provider, a real browser).

- [ ] **Step 2: Update the other docs**

- `docs/PROTECTED-ROUTES.md`: "Before you start" mentions HTTPS-only servers; "A CDN in front" says the two `no-store` headers follow a convention not every CDN honours, and that behind one that doesn't, caching must stay off for routes to this host.
- `ARCHITECTURE.md`: a section "Behind a TLS-terminating proxy (opt-in)" after "Protected routes": the rule's three parts, the stamp and constraint, the admin listeners. Add `internal/traefik/routing.go` and `internal/server/ingress.go` to the package layout.
- `AGENTS.md`, "Decisions to keep", one entry: the rule's shape and what `RuleAddress` accepts; routers are written from `traefik.Routing`; the stamp and the constraint; no label names `web` with HTTP off; the header's value is never logged; the two new scripts in "Build, run, test".

- [ ] **Step 3: Check the docs against the code**

Run: `for v in OUTHAUL_ACME_DNS_PROVIDER OUTHAUL_ACME_DNS_ENV OUTHAUL_TLS_WILDCARD OUTHAUL_HTTP_PORT OUTHAUL_REQUIRE_HEADER_NAME OUTHAUL_REQUIRE_HEADER_VALUE OUTHAUL_ADMIN_DIRECT; do grep -q "$v" internal/config/config.go && grep -q "$v" README.md && grep -q "$v" docs/BEHIND-A-PROXY.md || echo "MISSING $v"; done; grep -noE '[a-z0-9.-]+\.(com|org|net|io|dev|cloud)\b' docs/BEHIND-A-PROXY.md | grep -v 'example\.\(com\|org\)'`
Expected: no output: every setting is in the code and both documents, and the document names no real host.

- [ ] **Step 4: Run the full checks**

Run: `go build ./... && go vet ./... && go test -race -timeout 30m ./... && sh deploy/test/run.sh && shellcheck -s sh --severity=warning deploy/bootstrap.sh deploy/update.sh deploy/test/*.sh`
Expected: all pass.

- [ ] **Step 5: Commit**

```bash
git add docs README.md ARCHITECTURE.md AGENTS.md
git commit -m "docs: running Outhaul behind a TLS-terminating proxy"
```
