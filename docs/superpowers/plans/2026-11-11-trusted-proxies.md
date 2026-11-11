# Trusting a Proxy's Forwarded Headers (R12) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let an operator name the proxies in front of Traefik so apps see the visitor's address, while the protected-routes gate and the four headers a proxy has no reason to send stay under Outhaul's control.

**Architecture:** `OUTHAUL_TRUSTED_PROXIES` is parsed into `config.Config` and copied into `traefik.ProxyConfig` (never into `traefik.Routing`), so no app label and no ingress stamp changes. `proxySpec` adds Traefik's per-entrypoint `forwardedheaders` flags plus one file-provider middleware that empties four spoofable headers; `EnsureProxy` writes or removes that middleware file. The config hash includes the new flags and mount, so a change recreates Traefik once.

**Tech Stack:** Go 1.26, Traefik v3.7.6 (Docker and file providers), Docker Engine API, POSIX `sh` for the real-Traefik script.

**Spec:** `docs/superpowers/specs/2026-11-11-client-ips-streams-sign-in-design.md`, sections "Settings" and "R12: Trusting a proxy's forwarded headers".

**Before you start:** `plan-proxy-ingress` now has eight commits, through its Task 7 of 13. `traefik.Routing.HTTPOff` and `config.Config.HeaderRequired()` are present on that branch, so the parts of this plan that read them are ready. Still wait for its Task 8: it changes `EnsureProxy`, which Task 2 here also edits (the file removal on a failed pull). Start after Task 8 is in the tree, or after the whole R1–R4 plan lands.

## Global Constraints

- The only new setting is `OUTHAUL_TRUSTED_PROXIES`: IP addresses and CIDRs, comma-separated, or `all`. Unset means trust nothing, exactly as today.
- `Load` does no I/O and keeps a default on a malformed value; `Validate` names the variable and what it wants. `serve` refuses to start on a `Validate` error.
- Trust is all or nothing per address. `all` without a required header is a warning at startup, not a refusal.
- Nothing in `traefik.Routing` changes: no label changes, no app is redeployed, the R3 `outhaul.ingress` stamp is untouched.
- With the setting unset, `proxySpec` returns today's spec and hash; the golden tests from the R1–R4 plan pass unchanged.
- The gate's labels do not change. Only Traefik's entrypoints trust a proxy; the forward-auth middleware keeps `trustforwardheader=false`.
- While a proxy is trusted, Traefik removes `X-Real-Ip`, `X-Forwarded-Uri`, `-Method` and `-Prefix` through one file-provider middleware. If that file is missing, routers on the entrypoint are disabled (404); they never open.
- Test first: write the failing test, watch it fail for the stated reason, then implement. Real-Traefik behaviour is checked by `deploy/test/integration_gate.sh`, not only by unit tests.
- This repository is public: `example.com` names only, in code, tests, scripts and docs.
- After each task: `go build ./... && go vet ./... && go test ./...`. Before calling the plan done: `go test -race -timeout 30m ./...` and `shellcheck -s sh --severity=warning deploy/test/integration_gate.sh`.
- Commits: `feat(scope): summary` or `fix(scope): summary`, with a body saying what the user saw go wrong and what changed.

## Review Focus

The inputs and failures most likely to bite that no requirement spells out. Each has its test in the task named.

1. A list with a trailing comma, a doubled comma, an empty entry, a bad address, a bad CIDR, or `all` mixed with addresses: refused at startup, naming `OUTHAUL_TRUSTED_PROXIES`, and trust falls back to none (Task 1).
2. `OUTHAUL_TRUSTED_PROXIES=all` with no required header: Outhaul starts and logs a warning naming both settings; the warning never contains a header value (Task 1).
3. Trust turned on, off, or changed: the file-provider file is written or removed, the config hash changes exactly once, and the flags land on exactly the entrypoints that exist in ACME, local-CA, tunnel, plain-HTTP and `OUTHAUL_HTTP_PORT=off` modes. A failed pull while turning trust off leaves the old Traefik's file in place (Task 2).
4. Trust on with no `DynamicDir`: `EnsureProxy` fails before creating a network or container, naming the setting (Task 2).
5. The forwarded-headers file disappears while Traefik runs: its routers answer 404, and restoring the file brings them back (Task 3).

## File map

| File | Responsibility | Tasks |
|---|---|---|
| `internal/config/config.go`, `config_test.go` | the setting, its parser and validation | 1 |
| `main.go`, `main_test.go` | the `all` warning, `proxyConfig` wiring | 1, 2 |
| `internal/traefik/proxy.go`, `proxy_test.go` | `ProxyConfig` fields, per-entrypoint flags, the middleware file, the hash | 2 |
| `deploy/test/forwardedconfig/main.go` (new) | prints the middleware file the script mounts | 3 |
| `deploy/test/integration_gate.sh` | the `trustedips` and `untrustedips` Traefiks, removed-header and gate assertions, fail-closed check | 3 |
| `README.md` | the setting's row | 1 |
| `docs/BEHIND-A-PROXY.md` | trusted proxies: what an app receives, `all`, the real-host checks | 4 |
| `docs/PROTECTED-ROUTES.md` | the gate is told the same under trusted proxies | 4 |
| `ARCHITECTURE.md` | a section on trusted proxies | 4 |
| `AGENTS.md` | the kept decision, and the script's new coverage | 4 |

---

## Task 1: The setting, its validation and the `all` warning

**Files:**
- Modify: `internal/config/config.go`, `internal/config/config_test.go`, `main.go`, `main_test.go`, `README.md`
- Test: `internal/config/config_test.go`, `main_test.go`

**Interfaces:**
- Produces:

```go
// config.Config
TrustedProxies  []string // parsed addresses and CIDRs, trimmed; nil when unset
TrustAllProxies bool     // OUTHAUL_TRUSTED_PROXIES=all

// main.go
func trustedAllWarning(cfg config.Config) string // "" when there is nothing to warn about
```

- [ ] **Step 1: Write the failing config tests**

Add to `internal/config/config_test.go`:

```go
func TestTrustedProxiesSetting(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
		all  bool
		bad  bool
	}{
		{"unset", "", nil, false, false},
		{"address", "192.0.2.1", []string{"192.0.2.1"}, false, false},
		{"cidr and ipv6, spaces", " 192.0.2.0/24 , 2001:db8::1 ", []string{"192.0.2.0/24", "2001:db8::1"}, false, false},
		{"all", "all", nil, true, false},
		{"all in another case", "ALL", nil, true, false},
		{"all mixed", "all,192.0.2.1", nil, false, true},
		{"empty first", ",192.0.2.1", nil, false, true},
		{"empty last", "192.0.2.1,", nil, false, true},
		{"double comma", "192.0.2.1,,192.0.2.2", nil, false, true},
		{"bad address", "192.0.2.999", nil, false, true},
		{"bad cidr", "10.0.0.0/33", nil, false, true},
		{"hostname", "proxy.example.com", nil, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := map[string]string{}
			if tc.in != "" {
				env["OUTHAUL_TRUSTED_PROXIES"] = tc.in
			}
			c := Load(func(k string) string { return env[k] })
			err := c.Validate()
			if tc.bad {
				if err == nil || !strings.Contains(err.Error(), "OUTHAUL_TRUSTED_PROXIES") {
					t.Fatalf("Validate error = %v, want one naming OUTHAUL_TRUSTED_PROXIES", err)
				}
			} else if err != nil {
				t.Fatalf("Validate: %v", err)
			}
			if !reflect.DeepEqual(c.TrustedProxies, tc.want) {
				t.Errorf("TrustedProxies = %#v, want %#v", c.TrustedProxies, tc.want)
			}
			if c.TrustAllProxies != tc.all {
				t.Errorf("TrustAllProxies = %v, want %v", c.TrustAllProxies, tc.all)
			}
		})
	}
}
```

Add the `reflect` import if the file does not have it.

- [ ] **Step 2: Write the failing warning test**

Add to `main_test.go`:

```go
func TestTrustedAllWarning(t *testing.T) {
	w := trustedAllWarning(config.Config{TrustAllProxies: true})
	for _, want := range []string{"OUTHAUL_TRUSTED_PROXIES", "OUTHAUL_REQUIRE_HEADER_NAME", "OUTHAUL_REQUIRE_HEADER_VALUE"} {
		if !strings.Contains(w, want) {
			t.Errorf("warning %q does not name %s", w, want)
		}
	}
	if got := trustedAllWarning(config.Config{
		TrustAllProxies:    true,
		RequireHeaderName:  "X-Probe-Id",
		RequireHeaderValue: "abc-123",
	}); got != "" {
		t.Errorf("warning = %q with a required header, want empty", got)
	}
	if got := trustedAllWarning(config.Config{}); got != "" {
		t.Errorf("warning = %q without trust-all, want empty", got)
	}
}
```

- [ ] **Step 3: Run the tests to see them fail**

Run: `go test ./internal/config/ . -run 'TrustedProxies|TrustedAll'`

Expected: FAIL — `TrustedProxies`, `TrustAllProxies` and `trustedAllWarning` are undefined.

- [ ] **Step 4: Implement**

In `internal/config/config.go`:

- Add the two fields to `Config` beside `PublicURL`/`ServerIP`.
- In `Load`, add:

```go
c.TrustedProxies, c.TrustAllProxies = p.trustedProxies("OUTHAUL_TRUSTED_PROXIES")
```

- Add the parser method. It splits on `,`, trims each entry, refuses an empty entry, accepts `all` in any case, and otherwise requires `net.ParseIP` to succeed or `net.ParseCIDR` to parse. On any bad entry, or `all` mixed with addresses, call `p.reject` with the variable and what it wants, and return `nil, false`. `net` is already imported.
- The `reject` message wants: `IP addresses or CIDRs, comma-separated, or all`.

In `main.go`:

```go
func trustedAllWarning(cfg config.Config) string {
	if !cfg.TrustAllProxies || cfg.HeaderRequired() {
		return ""
	}
	return "WARNING: OUTHAUL_TRUSTED_PROXIES=all trusts any peer that can connect; set OUTHAUL_REQUIRE_HEADER_NAME and OUTHAUL_REQUIRE_HEADER_VALUE so a request without the header matches no router."
}
```

In `serve`, immediately after `cfg.Validate()` succeeds:

```go
if w := trustedAllWarning(cfg); w != "" {
	log.Printf("%s", w)
}
```

In `README.md`, add the row to the configuration table:

```
| `OUTHAUL_TRUSTED_PROXIES` | (empty) | IPs/CIDRs Traefik trusts to supply forwarded headers, comma-separated, or `all`; pair `all` with a required header |
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/config/ . -run 'TrustedProxies|TrustedAll'`

Expected: PASS, including the `all` cases that must not be refused.

- [ ] **Step 6: Commit**

```bash
git add internal/config main.go main_test.go README.md
git commit -m "feat(config): OUTHAUL_TRUSTED_PROXIES, refused when malformed and warned about with all" -m "A malformed list silently meant no trust, and all without a header was not called out. Parse and validate the list, and warn at startup."
```

---

## Task 2: Traefik's flags and the forwarded-headers middleware file

**Files:**
- Modify: `internal/traefik/proxy.go`, `internal/traefik/proxy_test.go`, `main.go`, `main_test.go`
- Test: `internal/traefik/proxy_test.go`, `main_test.go`

**Interfaces:**
- Consumes: `config.Config.TrustedProxies` and `TrustAllProxies` from Task 1.
- Produces:

```go
// traefik.ProxyConfig gains
TrustedProxies  []string
TrustAllProxies bool

// proxy.go
const forwardedDynamicFile = "outhaul-forwarded.yml"
func (pc ProxyConfig) trustsProxy() bool          // TrustAllProxies || len(TrustedProxies) > 0
func ForwardedDynamicConfig() string              // the exact YAML EnsureProxy writes
func writeForwardedDynamicConfig(pc ProxyConfig) error  // writes when trusted; nil otherwise
func removeForwardedDynamicConfig(pc ProxyConfig) error // removes, ignoring absence
```

- [ ] **Step 1: Write the failing proxy tests**

Add to `internal/traefik/proxy_test.go`:

```go
// TestProxySpecTrustsAddresses
//   pc := tlsProxyConfig(); pc.DynamicDir = t.TempDir()
//   pc.TrustedProxies = []string{"192.0.2.0/24", "2001:db8::1"}
//   spec := proxySpec(pc)
//   for both "web" and "websecure":
//     slices.Contains(spec.Cmd, "--entrypoints.<ep>.forwardedheaders.trustedips=192.0.2.0/24,2001:db8::1")
//     slices.Contains(spec.Cmd, "--entrypoints.<ep>.http.middlewares=outhaul-forwarded@file")
//   no flag contains "forwardedheaders.insecure"
//   spec.Cmd contains "--providers.file.directory=/etc/traefik/dynamic" and "--providers.file.watch=true"
//   spec.Mounts contains docker.Mount{Source: pc.DynamicDir, Target: "/etc/traefik/dynamic", ReadOnly: true}
//
// TestProxySpecTrustsAll
//   pc := tlsProxyConfig(); pc.DynamicDir = t.TempDir(); pc.TrustAllProxies = true
//   for both entrypoints: "--entrypoints.<ep>.forwardedheaders.insecure=true" is present
//   no flag contains "forwardedheaders.trustedips"
//
// TestProxySpecTrustedEntrypoints, a table:
//   plain HTTP (testProxyConfig)                    -> flags on web only
//   ACME (tlsProxyConfig)                           -> flags on web and websecure
//   local CA (localCAProxyConfig(t))                -> flags on web and websecure
//   tunnel (testProxyConfig + TunnelMode)           -> flags on web only
//   HTTPOff (tlsProxyConfig + Routing.HTTPOff)      -> flags on websecure only
//   For every other entrypoint name (web/websecure), assert neither flag nor the
//   middleware flag is present. Always set DynamicDir = t.TempDir().
//
// TestProxySpecHashChangesWithTrust
//   base := proxySpec(pc).Labels["outhaul.config-hash"] with trust unset
//   pc.TrustedProxies = []string{"192.0.2.1"} -> hash differs
//   pc.TrustAllProxies = true, pc.TrustedProxies = nil -> differs from both
//
// TestEnsureProxyWritesAndRemovesForwardedConfig
//   pc := tlsProxyConfig(); pc.DynamicDir = t.TempDir(); pc.TrustedProxies = []string{"192.0.2.1"}
//   EnsureProxy -> os.ReadFile(filepath.Join(pc.DynamicDir, forwardedDynamicFile)) == ForwardedDynamicConfig()
//   pc.TrustedProxies = nil; EnsureProxy -> os.Stat is os.IsNotExist
//
// TestEnsureProxyKeepsForwardedConfigWhenThePullFailsWhileTurningItOff
//   f := docker.NewFake()
//   pc := tlsProxyConfig(); pc.DynamicDir = t.TempDir(); pc.TrustedProxies = []string{"192.0.2.1"}
//   EnsureProxy(ctx, f, pc, nil) succeeds; path := filepath.Join(pc.DynamicDir, forwardedDynamicFile)
//   before, _ := f.FindContainer(ctx, pc.ContainerName)
//   pc.TrustedProxies = nil
//   f.FailPull = func(string) error { return errors.New("registry down") }
//   EnsureProxy returns an error
//   os.Stat(path) succeeds: the old Traefik still names the middleware
//   before.Running() is true
//
// TestEnsureProxyTrustedNeedsDynamicDir
//   pc := tlsProxyConfig(); pc.TrustedProxies = []string{"192.0.2.1"}; pc.DynamicDir = ""
//   EnsureProxy returns an error containing "OUTHAUL_TRUSTED_PROXIES"
//   no network and no container was created
```

- [ ] **Step 2: Run the tests to see them fail**

Run: `go test ./internal/traefik/ -run 'Trusted|Forwarded'`

Expected: FAIL — the fields and functions are undefined, and no flags are emitted.

- [ ] **Step 3: Implement**

In `internal/traefik/proxy.go`:

- Add the two fields to `ProxyConfig` with a comment that they are not part of `Routing`.
- Add `trustsProxy()`.
- In `proxySpec`, declare a local `entrypoints []string`. Append `"web"` in the same branch that writes `--entrypoints.web.address=:80` (the R1–R4 plan omits that branch under `pc.Routing.HTTPOff`), and `"websecure"` in the existing TLS branch. Do not infer them from `Routing` elsewhere; this keeps the list beside the flags.
- Change the file-provider condition to:

```go
fileProvider := pc.adminRoutingEnabled() || (pc.LocalCA && !pc.TunnelMode) || pc.trustsProxy()
if fileProvider { /* the existing two flags and mount */ }
```

- After the TLS/entrypoint flags are written, append the trust flags:

```go
if pc.trustsProxy() {
	for _, ep := range entrypoints {
		if pc.TrustAllProxies {
			cmd = append(cmd, "--entrypoints."+ep+".forwardedheaders.insecure=true")
		} else {
			cmd = append(cmd, "--entrypoints."+ep+".forwardedheaders.trustedips="+strings.Join(pc.TrustedProxies, ","))
		}
		cmd = append(cmd, "--entrypoints."+ep+".http.middlewares=outhaul-forwarded@file")
	}
}
```

- Add the YAML. `ForwardedDynamicConfig` returns exactly the block below, with a trailing newline after the last line:

```yaml
# Managed by Outhaul — removes the headers a proxy has no reason to send.
# Do not edit by hand.
http:
  middlewares:
    outhaul-forwarded:
      headers:
        customRequestHeaders:
          X-Real-Ip: ""
          X-Forwarded-Uri: ""
          X-Forwarded-Method: ""
          X-Forwarded-Prefix: ""
```

- `writeForwardedDynamicConfig`: with `DynamicDir == ""` or trust off, return nil; otherwise `os.MkdirAll(DynamicDir, 0o700)` and write the YAML with mode `0o644`.
- `removeForwardedDynamicConfig`: with `DynamicDir == ""` return nil; `os.Remove` the file and ignore `os.IsNotExist`.
- In `EnsureProxy`, before `writeAdminDynamicConfig`, return an error naming `OUTHAUL_TRUSTED_PROXIES` when `pc.trustsProxy() && pc.DynamicDir == ""`. When trust is on, write the file there, before the pull and before any container is created, so a replacement finds it.
- Remove the file only **after** a replacement has started, or when the existing container already matches the untrusted spec. Concretely:
  - existing container's hash matches: start it if stopped; when trust is off, remove the file (its labels do not name the middleware); return.
  - otherwise pull, then stop/remove/create/start. If the pull fails, return the error **without touching the file**: the old Traefik, which may still name the middleware, keeps running. After the new container starts, remove the file when trust is off.
- This ordering is the fix for the failed-pull case: removing the file before the pull would leave an old trusting Traefik naming a middleware that no longer exists, and every route on it would answer 404.
- Do not add the trust fields to `hashConfig` separately: the new flags and mount are already in its inputs.

In `main.go`, in `infraController.proxyConfig`, set both fields from `ic.cfg` before the `tunnelOn` early return, so tunnel mode carries them too.

Add to `main_test.go`:

```go
// TestProxyConfigCarriesTrustedProxies: infraController.proxyConfig returns
// the cfg's TrustedProxies and TrustAllProxies both with tunnelOn true and false.
```

- [ ] **Step 4: Run all tests**

Run: `go build ./... && go vet ./... && go test ./...`

Expected: PASS. The R1–R4 golden tests show no change with trust unset.

- [ ] **Step 5: Commit**

```bash
git add internal/traefik main.go main_test.go
git commit -m "feat(traefik): trust named proxies and remove headers a proxy has no reason to send" -m "Apps behind a CDN saw its address, and four spoofable headers passed through. Add the per-entrypoint trust flags and the middleware file, and keep it until a replacement runs."
```

---

## Task 3: Check trust, the removed headers and fail-closed on a real Traefik

Not run by CI. This task changes only the stub mode of `integration_gate.sh`; its `--outhaul` mode exits before these checks.

**Files:**
- Create: `deploy/test/forwardedconfig/main.go`
- Modify: `deploy/test/integration_gate.sh`

**Interfaces:**
- Consumes: `traefik.ForwardedDynamicConfig()` from Task 2.
- Produces:

```
forwardedconfig   prints the file EnsureProxy writes, and nothing else
```

- [ ] **Step 1: Add the helper**

`deploy/test/forwardedconfig/main.go`:

```go
// Command forwardedconfig prints the file-provider YAML Outhaul mounts while
// it trusts a proxy, so integration_gate.sh checks the real content.
package main

import (
	"fmt"

	"github.com/outhaul-dev/outhaul/internal/traefik"
)

func main() { fmt.Print(traefik.ForwardedDynamicConfig()) }
```

Run: `go build ./deploy/test/forwardedconfig`

Expected: builds with no output.

- [ ] **Step 2: Extend the script's setup and cleanup**

In `deploy/test/integration_gate.sh`:

- Add `trustedips_c=outhaul-gatetest-traefik-trustedips` and `untrustedips_c=outhaul-gatetest-traefik-untrustedips` beside the other container names, and add both to the `docker rm -f` cleanup list and to the "labels as Docker and Traefik keep them" loop.
- In setup, after `gatelabels` is built:

```sh
mkdir -p "$work/dynamic"
(cd "$root" && go build -o "$work/forwardedconfig" ./deploy/test/forwardedconfig) || die "go build forwardedconfig"
"$work/forwardedconfig" >"$work/dynamic/outhaul-forwarded.yml" || die "write the forwarded-headers config"
```

- Add a `start_trustedips_traefik NAME CIDR` function next to `start_traefik`: the same `docker run` options, plus `-v "$work/dynamic:/etc/traefik/dynamic:ro"`, plus these Traefik arguments:

```
--providers.file.directory=/etc/traefik/dynamic
--providers.file.watch=true
--entrypoints.web.forwardedheaders.trustedips=<CIDR>
--entrypoints.websecure.forwardedheaders.trustedips=<CIDR>
--entrypoints.web.http.middlewares=outhaul-forwarded@file
--entrypoints.websecure.http.middlewares=outhaul-forwarded@file
```

Read the network's subnet with `docker network inspect "$net" -f '{{range .IPAM.Config}}{{.Subnet}}{{end}}'`; `die` if it is empty. Start `trustedips_c` with that subnet: the host's requests to Traefik's published ports reach it from inside the subnet, so the client is trusted, as it would be from a CDN in the subnet.

- Start `untrustedips_c` with `203.0.113.0/24`, a TEST-NET range no peer can match. This is the configured-but-untrusted control: the same flags and middleware, but the host's address is outside the list, so Traefik must ignore the client's forwarded headers.

- Start both after the existing four. Add both HTTP and HTTPS loopback ports to the variables and to the "find Traefik's loopback ports" checks, and record both API ports (`8080`). Add `trustedips` and `untrustedips` to the `vias` list and to `req`'s port `case`. Add `trustedips http` and `untrustedips http` to the "which host.docker.internal answers for the gate" target list.
- Update the script's header list from four Traefiks to six, and describe `trustedips` as "main, but its entrypoints trust the client's forwarded headers and remove the four a proxy has no reason to send", and `untrustedips` as the same with a list that matches no peer.

- [ ] **Step 3: Write the failing assertions**

Add a section after the gate's forged-header checks:

```sh
echo "== what an app receives when Traefik trusts the client's proxy"
```

For each of `main`, `trusting`, `trustedips` and `untrustedips`, on `shop.test` (an open row), send one request with every forged header:

```
-H 'X-Forwarded-For: 203.0.113.9'
-H 'X-Forwarded-Host: evil.test'
-H 'X-Forwarded-Proto: https'
-H 'X-Forwarded-Uri: /evil'
-H 'X-Forwarded-Method: POST'
-H 'X-Forwarded-Prefix: /evil'
-H 'X-Real-Ip: 6.6.6.6'
-H 'X-Outhaul-User: mallory'
```

Assert on the echo backend's body (`h <name>: <value>` lines):

- via `trustedips`: 200; contains `h x-forwarded-for: 203.0.113.9, `; contains `h x-forwarded-host: evil.test`; contains `h x-forwarded-proto: https`; has no `h x-real-ip:`; has no `h x-forwarded-uri:`, `h x-forwarded-method:` or `h x-forwarded-prefix:`; has no `h x-outhaul-user:` and no `mallory`.
- via `untrustedips` (configured but no peer matches): 200; no `203.0.113.9`; contains `h x-forwarded-host: shop.test`; has no `h x-real-ip:` (the middleware still runs); no `h x-forwarded-uri:`, `h x-forwarded-method:` or `h x-forwarded-prefix:`; no `h x-outhaul-user:` and no `mallory`.
- via `trusting` (the control): contains `h x-real-ip: 6.6.6.6`, `h x-forwarded-uri: /evil`, `h x-forwarded-method: POST`, `h x-forwarded-prefix: /evil`, `h x-forwarded-host: evil.test`, and `h x-forwarded-for: 203.0.113.9, `; no `h x-outhaul-user:` and no `mallory`.
- via `main`: no `203.0.113.9`; contains `h x-forwarded-host: shop.test`; contains an `h x-real-ip:` line whose value is not `6.6.6.6`; no `h x-forwarded-uri:`, `h x-forwarded-method:` or `h x-forwarded-prefix:`; no `h x-outhaul-user:` and no `mallory`.

Then add `trustedips` and `untrustedips` to the existing `for via in main trusting` gate loop. The existing assertions must hold: the gate receives Traefik's `X-Forwarded-Host`, `-Uri`, `-Method` and `-Proto`, and never the client's `X-Real-Ip` or `-Prefix`.

- [ ] **Step 4: Write the fail-closed assertion**

At the end of the stub-mode checks:

1. Remove `$work/dynamic/outhaul-forwarded.yml`.
2. With `via=trustedips`, poll for up to 15 seconds until `shop.test` (open) answers 404 on both `http` and `https`. A missing middleware disables every router on the entrypoint.
3. In the same window, poll until `wiki.test/page` (protected) also answers 404 on both entrypoints. It must never reach the gate or the app while the middleware is missing.
4. Write the file back with `"$work/forwardedconfig" >"$work/dynamic/outhaul-forwarded.yml"`.
5. Poll for up to 15 seconds until `shop.test` answers 200 again on both entrypoints, and `wiki.test/page` is no longer 404 (send `Accept: text/html`; its protected answer is 302 without a cookie).
6. Print one PASS line; on failure, print each last status and the trustedips Traefik's `/api/http/routers` output from its loopback API port, so the missing middleware is visible.

- [ ] **Step 5: Run the script and shellcheck**

Run: `sh deploy/test/integration_gate.sh`

Expected: every line `PASS`, exit 0, and no `outhaul-gatetest-*` container left.

Run: `shellcheck -s sh --severity=warning deploy/test/integration_gate.sh`

Expected: no warnings.

- [ ] **Step 6: Commit**

```bash
git add deploy/test/integration_gate.sh deploy/test/forwardedconfig
git commit -m "test(traefik): a trusted proxy's forwarded headers and the four removed, on real Traefik" -m "Trust was only unit-tested. Check the flags, the four removed headers, the gate, an unmatched trust list and fail-closed on a real Traefik."
```

---

## Task 4: Docs

**Files:**
- Modify: `docs/BEHIND-A-PROXY.md`, `docs/PROTECTED-ROUTES.md`, `ARCHITECTURE.md`, `AGENTS.md`, `README.md` (only if the row is missing)

- [ ] **Step 1: Write the guide**

In `docs/BEHIND-A-PROXY.md`, add a "Trusted proxies" section:

- `OUTHAUL_TRUSTED_PROXIES` takes IPs and CIDRs, or `all`; it applies to every entrypoint Traefik declares, including tunnel mode and `OUTHAUL_HTTP_PORT=off`.
- The addresses are the ones Traefik sees on the connection, not the visitor's: a connection from the host itself can arrive from the Docker bridge gateway. An entry that matches no peer trusts nobody, which is today's behaviour.
- `all` is safe only when nothing but the proxy can reach Traefik's ports; it is meant to be paired with the required header, and `serve` warns when it is not.
- What an app receives: `X-Forwarded-For` is the visitor's address followed by the proxy's; the entry before the last is the visitor's. Tell the app's framework there are two proxies.
- `X-Real-Ip` is never the visitor's address unless the proxy sets it; a trusted proxy that passes a client's value lets the client choose it.
- While trust is on, the file-provider middleware removes `X-Real-Ip`, `X-Forwarded-Uri`, `-Method` and `-Prefix`. If the file is missing, routers on the entrypoint are disabled and answer 404, so it fails closed. It is not the gate: the gate remains in each app's own labels.
- Turning the setting off removes the file only after the replacement Traefik starts. If the image pull fails, the old Traefik keeps serving and the file stays, so a failed pull cannot take every route down.
- The gate is unchanged: its forward-auth keeps `trustforwardheader=false`, so a client cannot choose what it is told.
- Add the real-host checks: an app reached through the CDN sees the visitor before the CDN's address; made-up `X-Forwarded-Host`, `-Proto`, `-Port` and `X-Real-Ip` are overwritten or absent; a protected app's sign-in still works.

In `docs/PROTECTED-ROUTES.md`, add one paragraph: with trusted proxies, the gate receives the same Traefik-written `X-Forwarded-*` headers and never the client's `X-Real-Ip` or `X-Forwarded-Prefix`; nothing in the labels or the gate changes.

- [ ] **Step 2: Write the locked and kept decisions**

In `ARCHITECTURE.md`, add a "Trusted proxies" section near the other ingress sections: opt-in, per-entrypoint, all-or-nothing per address; it changes Traefik's flags and one file-provider middleware only, never a router label or the ingress stamp; the gate keeps `trustforwardheader=false`.

In `AGENTS.md`:

- Add a "Decisions to keep" bullet: trusted proxies are opt-in; `all` is paired with the required header; the four removed headers; the file provider middleware fails closed. Say it is the only middleware in the file provider, and why that does not contradict the gate's rule: an entrypoint middleware that is missing closes the entrypoint, while a router that names a missing file-provider middleware falls to an open router. The gate therefore stays in each app's own labels.
- Update "Build, run, test" so the `integration_gate.sh` sentence also says it checks the forwarded headers and the removed four.

- [ ] **Step 3: Verify**

Run: `grep -n "OUTHAUL_TRUSTED_PROXIES" README.md docs/BEHIND-A-PROXY.md docs/PROTECTED-ROUTES.md ARCHITECTURE.md AGENTS.md`

Expected: each file names the setting where the task above says it should.

- [ ] **Step 4: Run the whole suite**

Run: `go build ./... && go vet ./... && go test ./...`

Expected: PASS.

Run: `go test -race -timeout 30m ./...`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add README.md docs/BEHIND-A-PROXY.md docs/PROTECTED-ROUTES.md ARCHITECTURE.md AGENTS.md
git commit -m "docs(proxy): trusted proxies, what an app receives, and why the gate is unchanged" -m "The setting, its all pairing and the removed headers were not documented. Add the guide and the kept decision."
```
