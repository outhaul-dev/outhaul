# Container Resource Limits Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add opt-in default CPU, memory and process limits to every container Outhaul creates, let a system admin set one app's limits and see them beside the live figures, and cap how many builds run at once.

**Architecture:** `core.Limits` carries thousandths of a CPU, bytes and processes; `Limits.Or` applies a server default field by field. `docker.ContainerSpec` passes the three values to Docker's `HostConfig.Resources`, and the Compose override adds only the resource keys a service did not set itself. A migration stores per-app overrides, a system-admin route and panel edit them, and `docker.Stats` reads the live limits. The worker gets a second semaphore for deployments that have to build.

**Tech Stack:** Go 1.26, Docker Engine API (cgroup v2 on the staging host; cgroup v1 is not tested), Docker Compose 2.35 or newer, Traefik v3.7.6 labels.

**Spec:** `docs/superpowers/specs/2026-11-11-container-guard-rails-design.md`. Read it first; this plan implements R7 and the final “Where R6 and R7 meet” commit. R6 is `docs/superpowers/plans/2026-11-11-container-policy.md`.

**Base:** Start from a branch that contains the R6 plan (including its Task 10 header case) and the proxy-ingress plan (Task 1's `traefik.Routing` and R3's `Config.HeaderRequired()`).

## Global Constraints

- An install that sets none of the new settings sees no change: the same container specifications, the same override, the same pages, no build cap, and no Compose model read for limits. The R6 plan's “no change” tests must still pass.
- New settings, exactly: `OUTHAUL_DEFAULT_CPUS`, `OUTHAUL_DEFAULT_MEMORY`, `OUTHAUL_DEFAULT_PIDS`, `OUTHAUL_MAX_CONCURRENT_BUILDS`. Each works alone. They are read through the `parser` in `internal/config/config.go`; `Validate` names the variable in every error.
- `OUTHAUL_DEFAULT_CPUS` accepts digits with up to three decimals, `0.01` or more, stored as thousandths. `OUTHAUL_DEFAULT_MEMORY` accepts digits, an optional decimal part, and an optional `k`, `m` or `g` in either case, with or without `b`; units are powers of 1024; the minimum is 6 MiB. `OUTHAUL_DEFAULT_PIDS` is a whole number, 1 or more. `OUTHAUL_MAX_CONCURRENT_BUILDS` is 1 to 4.
- `serve` refuses to start when `OUTHAUL_DEFAULT_CPUS` is more than `runtime.NumCPU()` and `OUTHAUL_DOCKER_HOST` is unset.
- Limits are per container, not per stack. There is no swap limit, no limit for one database, no “no limit” for one app, and no limit on Traefik, cloudflared or build steps.
- A zero field in `core.Limits` means “none”. `Limits.Or` takes the receiver's non-zero field and the fallback otherwise. An app's effective limit is `app.Limits.Or(cfg.DefaultLimits)`.
- Every path that creates a container Outhaul owns sets the effective limit: single-container apps (including the health-check container), Compose services through the override, databases and preview databases, and backup/restore/data-removal helpers. Traefik and cloudflared get none.
- A changed limit applies when the container is next created. Nothing is redeployed at startup and no running container is updated.
- The Compose model holds every interpolated value. It is never logged, stored, or quoted in an error.
- The next free migration (0029 at `06150cc`; check before landing, because other plans add migrations too) adds three columns to `apps`; 0 means the server default. `CreateApp` leaves them at 0, so a preview that copies its parent carries no limits. Nothing is sealed, so `sealedQueries` does not change.
- The limits route is `POST /apps/{id}/limits` at `levelSystemAdmin`; the panel exists only when at least one `OUTHAUL_DEFAULT_*` is set, and the route answers 404 otherwise.
- The build cap counts only deployments that have to build. A rollback, or a protection-change redeploy that arrives with an image, never waits for a build slot. A build slot is released when the deployment leaves `building`, not when the whole pipeline ends.
- Test first: write the failing test, watch it fail for the stated reason, then implement. Use `docker.Fake`, `compose.Fake`, and the server test environment.
- This repository is public: `example.com` names only, in code, tests, fixtures and docs.
- Commits: `feat(scope): summary` or `fix(scope): summary`, with a body saying what someone saw go wrong and what changed.
- Docs travel with the code. Every task that adds a setting updates `README.md` in the same commit. Task 9 finishes `docs/GUARD-RAILS.md`, `ARCHITECTURE.md` and `AGENTS.md`.
- After each task: `go build ./... && go vet ./... && go test ./...`. Before calling the plan done: `go test -race -timeout 30m ./...`.

## Review Focus

The inputs most likely to bite that no requirement spells out. Each has its test in the task named.

1. An install with no defaults and no app limits: every specification and the override are byte-for-byte today's (Tasks 3 and 4).
2. An app with one field set and two inherited: `Limits.Or` fills each field independently (Task 1).
3. A Compose service that sets one resource itself and inherits the others, with or without a `deploy.resources.limits` block: only the missing keys are added, and memory is a quoted string inside `deploy` (Task 4).
4. A service that reserves more memory than the limit Outhaul would add: the deploy fails in `building`, naming both figures, rather than at `up` with Docker's message (Task 4).
5. A rollback (an image already recorded) while the build cap is set, with a newer queued build of the same app: the rollback runs at once and the newer build does not jump ahead of an older queued build (Task 7).
6. A container with no limit: the pages show exactly today's values, and the stats fields stay zero (Task 6).

---

## File map

| File | Responsibility | Tasks |
|---|---|---|
| `internal/core/limits.go` | `Limits`, `Or`, `ParseCPUs`, `ParseMemory`, `MinMemory`, `MaxPipelines` | 1 |
| `internal/config/config.go` | the four settings, defaults, validation, CPU-count check | 1, 7 |
| `internal/docker/client.go`, `real.go` | `ContainerSpec` resources, `Stats` limits and processes | 2 |
| `internal/deploy/pipeline.go`, `worker.go` | effective limits on every single-container and health-check container | 3, 5 |
| `internal/dbaas/manager.go` | the default on databases and the data-removal helper | 3 |
| `internal/backup/manager.go`, `restore.go` | the default on backup and restore helpers | 3 |
| `internal/compose/override.go`, `limits.go` (new) | `Extra`, `LimitExtras`, per-service override additions | 4 |
| `internal/deploy/pipeline_compose.go` | read the model for limits, write extras, pass `Policy.Max` | 4, 8 |
| `internal/store/migrations/<next>_app_limits.sql`, `apps.go` | per-app limit columns and `SetAppLimits` | 5 |
| `internal/server/limits.go` (new), `server.go`, `routes.go`, `handlers.go`, `templates/app.tmpl` | the limits route and panel | 5 |
| `internal/server/stats.go`, `metrics.go`, templates | live limits and processes | 6 |
| `internal/store/deployments.go`, `internal/deploy/worker.go` | `NextClaimable(canBuild)`, the build semaphore | 7 |
| `internal/server/handlers.go`, `templates/deployment.tmpl` | the “waiting its turn” note | 7 |
| `main.go` | setters for the defaults and the build cap | 3, 5, 7 |
| `README.md`, `docs/GUARD-RAILS.md`, `ARCHITECTURE.md`, `AGENTS.md` | the settings and the decisions | 1, 7, 9 |

---

## Task 1: `core.Limits` parsers and the default settings

**Files:**
- Modify: `internal/core/limits.go`, `internal/core/limits_test.go`, `internal/config/config.go`, `internal/config/config_test.go`, `README.md`
- Test: `internal/core/limits_test.go`, `internal/config/config_test.go`

**Interfaces:**
- Consumes: `core.Limits` and `Limits.Or` from the R6 plan.
- Produces:

```go
// internal/core/limits.go
const MinMemory int64 = 6 << 20 // Docker's minimum memory limit, 6 MiB
const MaxPipelines = 4         // today's maxConcurrent, shared by config and deploy

// ParseCPUs returns thousandths of a CPU. It accepts digits with up to three
// decimals, 0.01 or more. The error text is the "want" phrase the parser and
// the per-app form both show.
func ParseCPUs(s string) (int64, error)

// ParseMemory returns bytes. It accepts digits, an optional decimal part, and
// an optional k, m or g in either case, with or without b. Units are powers of
// 1024. The error text is the "want" phrase.
func ParseMemory(s string) (int64, error)

// internal/config/config.go
DefaultLimits core.Limits // zero fields mean no limit
func (c Config) HasDefaultLimits() bool // any of the three is non-zero
```

- [ ] **Step 1: Write the failing parser tests**

In `core/limits_test.go`, table-driven:

```go
func TestParseCPUs(t *testing.T) {
	// "1" -> 1000, "0.5" -> 500, "1.5" -> 1500, "0.01" -> 10
	// refused: "", "0", "0.001", "50%", "500m", "1,5", ".5", "1e2", "-1"
}

func TestParseMemory(t *testing.T) {
	// "512m" -> 512<<20, "1g" -> 1<<30, "1.5g" -> 3<<29,
	// "1gb" -> 1<<30, "1G" -> 1<<30, "6291456" -> 6<<20
	// refused: "0", "5m", "1t", "1 g", "1gib", "-1", "1.5.0g"
}
```

Run: `go test ./internal/core/ -run 'ParseCPUs|ParseMemory'`
Expected: FAIL; the functions are not defined.

- [ ] **Step 2: Implement the parsers**

Use `strconv.ParseInt`/`ParseFloat`; do not use `go-units` or a new dependency. CPU parsing multiplies by 1000 and rounds only after checking at most three decimal places. Memory parsing lowercases, strips one optional trailing `b`, then applies `k`/`m`/`g` as 1024, 1024² and 1024³. Reject `0` and anything below `MinMemory` with a message naming the minimum as `6m`. The returned error is the “want” phrase, for example `a number of CPUs with up to three decimals, 0.01 or more`.

- [ ] **Step 3: Write the failing config tests**

```go
func TestLoadDefaultLimits(t *testing.T) {
	// unset -> core.Limits{}
	// OUTHAUL_DEFAULT_CPUS=1 OUTHAUL_DEFAULT_MEMORY=1g OUTHAUL_DEFAULT_PIDS=512
	// -> {1000, 1<<30, 512}
}

func TestValidateDefaultLimits(t *testing.T) {
	// "50%" -> error names OUTHAUL_DEFAULT_CPUS
	// "5m" -> error names OUTHAUL_DEFAULT_MEMORY
	// "0" -> error names OUTHAUL_DEFAULT_PIDS
	// a CPU value above runtime.NumCPU() with DockerHost empty -> error
	// the same with DockerHost set -> no error
}

func TestHasDefaultLimits(t *testing.T) { /* any one field is enough */ }
```

Run: `go test ./internal/config/ -run 'DefaultLimits|HasDefaultLimits'`
Expected: FAIL; the fields and parser methods do not exist.

- [ ] **Step 4: Implement the settings**

Add `DefaultLimits core.Limits` to `Config` and import `internal/core` (its first Outhaul-package import; `core` imports none, so there is no cycle). In `Load`, call three parser methods:

```go
DefaultLimits: core.Limits{
	MilliCPUs: p.cpus("OUTHAUL_DEFAULT_CPUS"),
	Memory:    p.memory("OUTHAUL_DEFAULT_MEMORY"),
	Pids:      p.pids("OUTHAUL_DEFAULT_PIDS"),
},
```

Each method returns 0 for unset, calls `core.ParseCPUs`/`core.ParseMemory`, and on error calls `p.reject(key, v, err.Error())`. `p.pids` accepts a whole number, 1 or more. `HasDefaultLimits` reports any non-zero field.

In `Validate`, when `DockerHost == ""` and `DefaultLimits.MilliCPUs > int64(runtime.NumCPU())*1000`, append an error naming `OUTHAUL_DEFAULT_CPUS`, the value, and the server's CPU count. Import `runtime`.

Add the three README rows with defaults “unset (no limit)” and purposes that say “per container”.

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/core/ ./internal/config/ -run 'Parse|Limits|DefaultLimits'`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/core internal/config README.md
git commit -m "feat(config): add default CPU, memory and process limits"
```

---

## Task 2: Docker resources and the live limits

**Files:**
- Modify: `internal/docker/client.go`, `internal/docker/real.go`, `internal/docker/stats_test.go`
- Create: `internal/docker/create_test.go`
- Test: `internal/docker/create_test.go`, `internal/docker/stats_test.go`

**Interfaces:**
- Produces:

```go
// internal/docker/client.go
type ContainerSpec struct {
	// existing fields...
	NanoCPUs  int64 // billionths of a CPU (1 CPU = 1e9); 0 = none
	Memory    int64 // bytes; 0 = none
	PidsLimit int64 // processes; 0 = none
}

type Stats struct {
	// existing fields...
	CPULimit   float64 // HostConfig.NanoCPUs/1e9, else CpuQuota/CpuPeriod; 0 = none
	MemLimited bool    // HostConfig.Memory > 0: MemLimit is the container's own
	Pids       uint64  // pids_stats.current
	PidsLimit  uint64  // HostConfig.PidsLimit when above 0; 0 = none
}
```

`NanoCPUs` is Docker's field: one CPU is `1_000_000_000`. `core.Limits.MilliCPUs` is thousandths, so `NanoCPUs = MilliCPUs * 1_000_000`.

- [ ] **Step 1: Write the failing create test**

Use the `httptest` fake Engine API pattern from `cancel_test.go` (and the body-decoding pattern from `network_test.go`). Add `TestCreateContainerSendsResources`:

```go
// fake /containers/create decodes the request body into container.CreateRequest
// and records HostConfig.Resources
// spec: NanoCPUs: 500_000_000, Memory: 1 << 30, PidsLimit: 512
// want: NanoCPUs 500_000_000, Memory 1<<30, PidsLimit != nil && * == 512
```

Add `TestCreateContainerWithoutLimitsSendsNoResources`: a zero spec must leave `HostConfig.Resources` empty (or nil), so the request is today's.

Run: `go test ./internal/docker/ -run 'CreateContainer.*Resources'`
Expected: FAIL; the fields are not defined.

- [ ] **Step 2: Implement the create side**

Add the fields to `ContainerSpec`. In `real.CreateContainer`, set:

```go
if spec.NanoCPUs > 0 || spec.Memory > 0 || spec.PidsLimit > 0 {
	host.Resources = container.Resources{
		NanoCPUs: spec.NanoCPUs,
		Memory:   spec.Memory,
	}
	if spec.PidsLimit > 0 {
		pids := spec.PidsLimit
		host.Resources.PidsLimit = &pids
	}
}
```

Do not set `MemorySwap`, `CpuQuota` or `CpuPeriod`. Do not add a limit when all three are zero.

- [ ] **Step 3: Write the failing stats test**

`ContainerStats` already inspects the container for `StartedAt`. Extend the fake daemon in `stats_test.go` (or add a small inspect-only test) to return:

```json
{"State":{"StartedAt":"..."},
 "HostConfig":{"NanoCpus":500000000,"Memory":1073741824,"PidsLimit":512}}
```

and a statistics body with `pids_stats: {"current": 12}`. Assert `Stats.CPULimit == 0.5`, `MemLimited == true`, `Pids == 12`, `PidsLimit == 512`.

Add a second case with `NanoCpus: 0`, `CpuQuota: 50000`, `CpuPeriod: 100000` and `Memory: 0`, asserting `CPULimit == 0.5`, `MemLimited == false`, `PidsLimit == 0`.

Run: `go test ./internal/docker/ -run 'ContainerStats.*Limits'`
Expected: FAIL; the fields do not exist.

- [ ] **Step 4: Implement the stats side**

In `real.ContainerStats`, keep one `ContainerInspect` call. Fill `CPULimit` from `HostConfig.NanoCPUs/1e9`; if that is zero, use `float64(CpuQuota)/float64(CpuPeriod)` when both are positive. Set `MemLimited = HostConfig.Memory > 0`. Set `PidsLimit` from the `*int64` when it is above 0. Set `Pids` from `raw.PidsStats.Current`. `MemLimit` stays as today (`MemoryStats.Limit`), which is the host total when unlimited.

- [ ] **Step 5: Run the tests**

Run: `go build ./... && go vet ./... && go test ./internal/docker/`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/docker
git commit -m "feat(docker): carry resource limits and read them back"
```

---

## Task 3: Limits on single-container apps, databases and helpers

**Files:**
- Modify: `internal/deploy/pipeline.go:183-300,517-575`, `internal/deploy/worker.go:53-92`, `internal/deploy/deploy_test.go`, `internal/dbaas/manager.go:103-124,261-271`, `internal/dbaas/manager_test.go`, `internal/backup/manager.go:375-384`, `internal/backup/restore.go:341-354`, `internal/backup/manager_test.go`, `internal/backup/restore_test.go`, `internal/traefik/proxy_test.go`, `internal/tunnel/connector_test.go`, `main.go:232-245`
- Test: the same files

**Interfaces:**
- Produces:

```go
// internal/deploy/worker.go
// limits is the app's stored limits over the server defaults. R7's per-app
// store change replaces the empty stored value below; the preview branch reads
// the parent at each deploy.
func (w *Worker) limits(ctx context.Context, app core.App) core.Limits

// internal/dbaas/manager.go
func (m *Manager) SetLimits(l core.Limits)

// internal/backup/manager.go
func (m *Manager) SetLimits(l core.Limits)
```

- [ ] **Step 1: Write the failing deploy tests**

```go
func TestSingleContainerCarriesDefaultLimits(t *testing.T) {
	// cfg.DefaultLimits = {MilliCPUs: 1000, Memory: 1<<30, Pids: 512}
	// run a nixpacks deploy; both the health-check container and the canonical
	// container in h.docker.Created have the three fields
}

func TestSingleContainerWithoutLimitsIsUnchanged(t *testing.T) {
	// cfg.DefaultLimits zero; every created spec has zero fields
}

func TestStopFirstDeployCarriesDefaultLimits(t *testing.T) {
	// the stop-first cutover path creates the canonical container directly;
	// it has the default fields
}
```

Run: `go test ./internal/deploy/ -run 'SingleContainer.*Limits|StopFirst.*Limits'`
Expected: FAIL; `createContainer` sets no limit fields.

- [ ] **Step 2: Implement the deploy side**

Add `limits` to `worker.go`. For now it returns the server default; Task 5 replaces the body with the app's stored value and the preview parent lookup:

```go
func (w *Worker) limits(_ context.Context, _ core.App) core.Limits {
	return w.cfg.DefaultLimits
}
```

In `createContainer`, compute `l := w.limits(ctx, app)` once and set `NanoCPUs: l.MilliCPUs * 1_000_000, Memory: l.Memory, PidsLimit: l.Pids`. Both the health-check and canonical containers use the same `createContainer`, so both get it.

- [ ] **Step 3: Write the failing database and helper tests**

```go
func TestDatabaseCarriesDefaultLimits(t *testing.T) {
	// m.SetLimits({1000, 1<<30, 512}); provision
	// want: Created[0] has the fields; the replacement path copies them too
}

func TestDatabaseWithoutLimitsIsUnchanged(t *testing.T) { /* zero fields */ }

func TestRemoveDataHelperCarriesDefaultLimits(t *testing.T) {
	// force the helper path; h.docker.Runs[0] has the fields
}

func TestBackupHelperCarriesDefaultLimits(t *testing.T) { /* Runs[0] */ }
func TestRestoreHelperCarriesDefaultLimits(t *testing.T) { /* Runs[0] */ }

func TestTraefikAndCloudflaredCarryNoLimits(t *testing.T) {
	// proxySpec(...) and connectorSpec(...) have zero limit fields, whatever
	// the server default is (they are built from their own typed configs)
}
```

Run: `go test ./internal/dbaas/ ./internal/backup/ -run 'CarriesDefaultLimits'`
Expected: FAIL; the managers have no limit setter.

- [ ] **Step 4: Implement the managers and wire `main.go`**

Add `limits core.Limits` to both managers and `SetLimits`. In `dbaas.provision`, set the fields on the database spec and on `removeData`'s helper spec. In `backup`, set them on the backup and restore helper specs. In `main.go`, call `dbm.SetLimits(cfg.DefaultLimits)` and `backups.SetLimits(cfg.DefaultLimits)`.

- [ ] **Step 5: Run the tests**

Run: `go build ./... && go vet ./... && go test ./internal/deploy/ ./internal/dbaas/ ./internal/backup/`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/deploy internal/dbaas internal/backup internal/traefik internal/tunnel main.go
git commit -m "feat(deploy): give every Outhaul container its default limits"
```

---

## Task 4: Compose service limits through the override

**Files:**
- Create: `internal/compose/limits.go`, `internal/compose/limits_test.go`, `internal/compose/testdata/override.golden`
- Modify: `internal/compose/override.go`, `internal/compose/compose_test.go`, `internal/deploy/pipeline_compose.go`, `internal/deploy/deploy_compose_test.go`
- Test: the same files

**Interfaces:**
- Consumes: `Model`, `Service` from the R6 plan; `core.Limits`; `Worker.limits` from Task 3; `traefik.Routing` and `Worker.routing` from the proxy-ingress plan.
- Produces:

```go
// internal/compose/override.go
func Override(app core.App, domains []core.Domain, network string, rt traefik.Routing, extra map[string]Extra) []byte

type Extra struct {
	Limits   core.Limits // only the fields to write
	InDeploy bool        // write them under deploy.resources.limits
	// R10 adds its restart value here.
}

// internal/compose/limits.go
// The error is the memory reservation case: a service reserves more than the
// limit Outhaul would add.
func LimitExtras(m Model, limit core.Limits) (map[string]Extra, error)
```

- [ ] **Step 1: Record today's override bytes**

The proxy-ingress plan pins route labels and Traefik's spec, not `compose.Override`. Before changing its signature, add `TestOverrideGolden` in `internal/compose`:

```go
func TestOverrideGolden(t *testing.T) {
	// app "shop"; one protected host, one open path, one protected path;
	// network "outhaul"; Routing{TLS: true, CertResolver: "le",
	// GateAddress: "http://host.docker.internal:8080/gate/check"}
	got := Override(app, domains, network, rt)
	// compare with testdata/override.golden; -update rewrites it
}
```

Run: `go test ./internal/compose/ -run OverrideGolden -update && go test ./internal/compose/ -run OverrideGolden`
Expected: PASS; `internal/compose/testdata/override.golden` exists.

```bash
git add internal/compose/compose_test.go internal/compose/testdata/override.golden
git commit -m "test(compose): pin today's override output"
```

- [ ] **Step 2: Write the failing `LimitExtras` tests**

Table-driven, one row per resource and placement:

```go
func TestLimitExtras(t *testing.T) {
	// no service keys -> all three fields in Extra, InDeploy false
	// cpus set by the service -> only memory and pids
	// mem_limit set -> only cpus and pids
	// pids_limit: -1 -> treated as set, so pids is not added
	// deploy.resources.limits.cpus set -> a deploy block exists:
	//   InDeploy true, only the missing fields
	// a service with all three set -> no extra for it
	// a service with no limits in a file that also has one that needs them ->
	//   only the latter appears
	// mem_limit above the limit is not an error here; Check owns the strict case
	// a service with no memory limit of its own and mem_reservation above the
	//   limit -> error names both figures
	// a service that sets mem_limit itself and reserves more -> no error; Outhaul
	//   adds no memory limit, so there is nothing to compare
	// deploy.resources.reservations.memory above the limit -> same rule
}

func TestLimitExtrasZeroLimitReturnsNothing(t *testing.T) { /* nil map */ }
```

Run: `go test ./internal/compose/ -run LimitExtras`
Expected: FAIL; `Extra` and `LimitExtras` are not defined.

- [ ] **Step 3: Write the failing `Override` tests**

```go
func TestOverrideWithNoExtrasIsUnchanged(t *testing.T) {
	// call with nil extra; compare with testdata/override.golden byte for byte
}

func TestOverrideWritesTopLevelLimits(t *testing.T) {
	// extra: {"web": {Limits: {MilliCPUs: 500, Memory: 1<<30, Pids: 512}}}
	// want under services.web: cpus: 0.5, mem_limit: 1073741824,
	// pids_limit: 512, no deploy block
}

func TestOverrideWritesInsideAnExistingDeployBlock(t *testing.T) {
	// extra: {"web": {InDeploy: true, Limits: {Memory: 1<<30}}}
	// want:
	//   deploy:
	//     resources:
	//       limits:
	//         memory: "1073741824"
}

func TestOverrideAddsAServiceWithNoDomain(t *testing.T) {
	// extra for "worker", domains empty
	// want: services.worker with its limits, no networks or labels, and no
	// top-level networks: block (nothing joins the shared network)
}

func TestOverrideKeepsRoutedServices(t *testing.T) {
	// domains for web plus extra for worker; both appear, web keeps its labels
}
```

Run: `go test ./internal/compose/ -run Override`
Expected: FAIL; `Override` still has the old signature and no extras.

- [ ] **Step 4: Implement `LimitExtras` and the override**

`LimitExtras` reads `Service.Raw` to decide what the service set:

- CPU set when `cpus`, `deploy.resources.limits.cpus`, `cpu_quota` or `cpu_period` is present;
- memory set when `mem_limit` or `deploy.resources.limits.memory` is present;
- pids set when `pids_limit` (including `-1`) or `deploy.resources.limits.pids` is present.

Compose leaves a zero out of the model, so a fixture with `mem_limit: 0` has no `mem_limit` key and counts as not set.

`InDeploy` is true when the service already has a `deploy.resources.limits` object. The extra's `Limits` holds only the non-zero fields the service did not set. If Outhaul would add a memory limit (`!memSet && limit.Memory > 0`) and the service reserves more than that through `mem_reservation` or `deploy.resources.reservations.memory`, return an error naming both figures.

`Override` unions the domain services and the extra keys, sorted. It writes the existing network and labels for services with domains. For each extra, if `InDeploy` it writes `deploy.resources.limits` with `cpus` as a number, `memory` as a quoted string of bytes, and `pids` as a number; otherwise it writes `cpus`, `mem_limit` (bytes, unquoted) and `pids_limit` at the service level. Use `quoteYAML` for the quoted memory and for unsigned values where needed.

The top-level `networks:` block is written only when at least one service has a domain. When the override exists only for limits, no service joins the shared network, so the block is omitted. With a nil map the output must equal `testdata/override.golden` byte for byte.

- [ ] **Step 5: Integrate the pipeline**

Extend `Worker.needsModel` (from the R6 plan) to `w.cfg.StrictContainerPolicy() || w.cfg.HeaderRequired() || w.limits(ctx, app) != (core.Limits{})`. Because `limits` may read a preview's parent, change the signature to `needsModel(ctx context.Context, app core.App) bool` and update the R6 call site.

In `runComposePipeline`, after the model is read and checked, compute:

```go
limit := w.limits(ctx, app)
var extra map[string]compose.Extra
if w.needsModel(ctx, app) {
	extra, err = compose.LimitExtras(model, limit)
	if err != nil {
		w.fail(dep, core.StatusBuilding, err.Error(), out)
		return
	}
}
if len(domains) > 0 || len(extra) > 0 {
	ov := compose.Override(app, domains, w.cfg.Network, w.routing(ctx), extra)
	// write and append the override as today
}
```

When no limit is set, `LimitExtras` returns nil and the override is written only for domains, exactly as before. Update every other `Override` call site and test to the new signature, passing `nil` extras.

- [ ] **Step 6: Write the pipeline tests**

```go
func TestComposePipelineAddsLimitsToEveryService(t *testing.T) {
	// cfg.DefaultLimits {1000, 1<<30, 512}
	// model has web (sets nothing) and worker (sets cpus: 2)
	// want: the override file has web's three keys and worker's memory/pids only
}

func TestComposePipelineReservationAboveTheLimitFails(t *testing.T) {
	// service mem_reservation 2g, default 1g
	// want: failed building, reason names both, no build call
}

func TestComposePipelineWithoutLimitsWritesOnlyDomains(t *testing.T) {
	// no defaults, no app limits; override has no cpus/mem_limit/pids_limit
}
```

Run: `go test ./internal/deploy/ -run 'ComposePipeline.*Limits|Reservation'`
Expected: FAIL until the integration is complete.

- [ ] **Step 7: Run everything**

Run: `go build ./... && go vet ./... && go test ./internal/compose/ ./internal/deploy/`
Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add internal/compose internal/deploy
git commit -m "feat(compose): add per-service limits through the override"
```

---

## Task 5: A limit for one app: migration, store, route and panel

**Files:**
- Create: `internal/store/migrations/<next>_app_limits.sql`, `internal/server/limits.go`, `internal/server/limits_test.go`
- Modify: `internal/core/app.go`, `internal/store/apps.go`, `internal/store/apps_test.go` (or a new `apps_limits_test.go`), `internal/previewmgr/manager_test.go`, `internal/server/server.go`, `internal/server/routes.go`, `internal/server/handlers.go`, `internal/server/templates/app.tmpl`, `internal/deploy/worker.go`, `main.go`
- Test: the store and server tests

**Interfaces:**
- Consumes: `core.Limits`, `ParseCPUs`, `ParseMemory`, `Config.DefaultLimits`, `Server.SetDefaultLimits`.
- Produces:

```go
// internal/core/app.go
type App struct {
	// existing fields...
	Limits Limits // zero fields use the server default
}

// internal/store/apps.go
func (s *Store) SetAppLimits(ctx context.Context, id int64, l core.Limits) error

// internal/server/server.go
func (s *Server) SetDefaultLimits(l core.Limits)
func (s *Server) hasDefaultLimits() bool

// internal/server/limits.go
func (s *Server) handleAppLimits(w http.ResponseWriter, r *http.Request)

// internal/deploy/worker.go
// limits now reads app.Limits and the parent for a preview.
func (w *Worker) limits(ctx context.Context, app core.App) core.Limits
```

- [ ] **Step 1: Write the failing migration and store tests**

Use `openMigratedBefore(t, "<next>")` (the migration number chosen in Step 2) to seed an old-schema app, then run `migrate` and assert its limits are zero. Add:

```go
func TestAppLimitsRoundTrip(t *testing.T) {
	// SetAppLimits(id, {500, 1<<30, 512}); GetApp returns the same
}

func TestNewAppsStartWithoutLimits(t *testing.T) {
	// CreateApp with a caller struct whose Limits copy a parent's values
	// (the preview path does `child := parent`); both the returned app and a
	// fresh GetApp read zero
}
```

Run: `go test ./internal/store/ -run 'AppLimits|Migrate.*Limits'`
Expected: FAIL; the columns and field do not exist.

- [ ] **Step 2: Implement the migration and store**

Create `<next>_app_limits.sql`, checking the next free number first (0029 at `06150cc`):

```sql
ALTER TABLE apps ADD COLUMN limit_cpus_milli INTEGER NOT NULL DEFAULT 0;
ALTER TABLE apps ADD COLUMN limit_memory_bytes INTEGER NOT NULL DEFAULT 0;
ALTER TABLE apps ADD COLUMN limit_pids INTEGER NOT NULL DEFAULT 0;
```

Append the three columns to `appCols` and `scanApp`; add `SetAppLimits`. Do **not** add them to `CreateAppWithDomains`'s `INSERT`, so every new row starts at 0. That matters for previews: `internal/previewmgr/manager.go` makes a child with `child := parent`, and an INSERT that copied the copied fields would store the parent's limits on the preview. Also clear `app.Limits` before `CreateAppWithDomains` returns: it returns the caller's struct, not a re-read, so the copied parent values would otherwise survive in memory even though the row is zero. Do not add the columns to `sealedQueries`; they are not secret.

- [ ] **Step 3: Write the failing route tests**

```go
func TestAppLimitsRouteNeedsADefault(t *testing.T) {
	// no defaults; POST /apps/{id}/limits -> 404
}

func TestAppLimitsRouteIsSystemAdminOnly(t *testing.T) {
	// default set; project member -> 403, system admin -> 303
}

func TestAppLimitsRouteValidates(t *testing.T) {
	// default set; "50%" -> 400, body names the CPU form
	// "5m" -> 400; "0" pids -> 400; CPUs above runtime.NumCPU() -> 400
}

func TestAppLimitsRouteStoresAndClears(t *testing.T) {
	// valid values -> stored; all empty -> all zero (server defaults)
}

func TestAppLimitsMayBeAboveOrBelowTheDefault(t *testing.T) {
	// default 1 CPU / 1 GiB / 512; store 0.5 CPU / 2 GiB / 1024
	// want: exactly those values, not clamped to the default
}

func TestPreviewLimitsCannotBeChanged(t *testing.T) {
	// a preview app -> 400 with "a preview uses its parent's limits", and its
	// row keeps zero limits
}
```

Run: `go test ./internal/server/ -run AppLimits`
Expected: FAIL; the route does not exist.

- [ ] **Step 4: Implement the route**

Add the route to `routes()` exactly as:

```go
{"POST /apps/{id}/limits", levelSystemAdmin, nil, s.handleAppLimits},
```

The handler:

1. parses `{id}`; `GetApp`; 404 if missing;
2. if `app.Ephemeral`, 400 with “a preview uses its parent's limits”;
3. if `!s.hasDefaultLimits()`, 404;
4. parses `limit_cpus`, `limit_memory`, `limit_pids`; empty means 0;
5. uses `core.ParseCPUs`, `core.ParseMemory` and a whole-number check for pids; on error re-renders the app page with 400 and the parser's message;
6. rejects CPUs above `runtime.NumCPU()`;
7. `SetAppLimits`; redirects 303 to `/apps/{id}`.

Add `defaultLimits core.Limits` to `Server`, `SetDefaultLimits`, and `hasDefaultLimits` (any field non-zero). In `main.go`, call `srv.SetDefaultLimits(cfg.DefaultLimits)` after `server.New`.

- [ ] **Step 5: Write the failing panel tests**

```go
func TestLimitsPanelOnlyWithADefault(t *testing.T) {
	// no default -> no "Limits" panel, even for a system admin
}

func TestLimitsPanelShowsTheValueInForce(t *testing.T) {
	// default 1 CPU / 1 GiB / 512; app memory 2 GiB
	// want: 1 CPU "server default", 2 GiB "set for this app", 512 "server default"
}

func TestLimitsPanelFormIsSystemAdminOnly(t *testing.T) {
	// viewer sees the values but no form and no name="limit_cpus"
}

func TestPreviewPanelShowsTheParentsLimits(t *testing.T) {
	// parent memory 2 GiB; preview page shows 2 GiB and no form
}
```

Run: `go test ./internal/server/ -run LimitsPanel`
Expected: FAIL; there is no panel.

- [ ] **Step 6: Implement the panel**

In `renderAppPage`, add to the data map:

```go
"LimitsAvailable": s.hasDefaultLimits(),
"Limits":          effective,   // app.Limits.Or(s.defaultLimits), or the parent's for a preview
"LimitSources":    sources,     // "server default" or "set for this app"; for a preview, "set for the parent app"
"DefaultLimits":   s.defaultLimits,
```

For a preview, load the parent and use `parent.Limits.Or(s.defaultLimits)`; never read the preview's stored columns. Add the panel to `app.tmpl` after Metrics: three rows (CPU, memory, processes) showing the value in force and its source. When `IsSystemAdmin` and the app is not ephemeral, show the three fields in the settings formats, with `placeholder` naming the server default; empty means the default. The panel says “A change applies on the app's next deploy.”

- [ ] **Step 7: Update `Worker.limits` for per-app and preview values**

Replace the Task 3 body:

```go
stored := app.Limits
if app.ParentID != 0 {
	if parent, err := w.store.GetApp(ctx, app.ParentID); err == nil {
		stored = parent.Limits
	} else {
		log.Printf("deploy: read preview parent %d: %v", app.ParentID, err)
	}
}
return stored.Or(w.cfg.DefaultLimits)
```

Add a deploy test where a preview's parent has memory 2 GiB and the preview's own row has nothing; the created spec has 2 GiB. Add a test where the parent lookup fails and the default still applies.

Add `TestPreviewStartsWithNoLimits` to `internal/previewmgr/manager_test.go`: set the parent's limits with `SetAppLimits`, spawn a preview through the existing harness, and assert both the returned child and a fresh `GetApp` read zero limits (the parent struct was copied, but the INSERT does not carry the columns and `CreateApp` clears the copy). The preview page test in Step 5 already asserts the parent's values are shown.

- [ ] **Step 8: Run everything**

Run: `go build ./... && go vet ./... && go test ./internal/store/ ./internal/server/ ./internal/deploy/`
Expected: PASS.

- [ ] **Step 9: Commit**

```bash
git add internal/store internal/previewmgr internal/server internal/deploy internal/core main.go
git commit -m "feat(apps): store and edit one app's limits"
```

---

## Task 6: The live limits and processes on the two pages

**Files:**
- Modify: `internal/server/stats.go:16-111`, `internal/server/stats_test.go`, `internal/server/metrics.go:49-55,126-178`, `internal/server/metrics_test.go`, `internal/server/templates/app.tmpl:47-98`, `internal/server/templates/metrics.tmpl:50-92`
- Test: the same files

**Interfaces:**
- Consumes: `docker.Stats` fields from Task 2.
- Produces:

```go
// internal/server/stats.go
type appStatsResponse struct {
	// existing fields...
	CPUSub  string `json:"cpuSub,omitempty"`  // "limit 1 CPU" or "limits differ"
	Pids    string `json:"pids,omitempty"`    // e.g. "12"
	PidsSub string `json:"pidsSub,omitempty"` // "of 512" or "limits differ"
}

// internal/server/metrics.go
type containerStat struct {
	// existing fields...
	CPU  string `json:"cpu"`            // "12.3% · limit 1" when limited
	Mem  string `json:"mem"`            // "118 MiB · of 1.0 GiB" when limited
	Pids string `json:"pids,omitempty"` // "12 · limit 512"; empty when no limit
}

type metricsSample struct {
	// existing fields...
	Processes bool `json:"processes,omitempty"` // any row has a process limit
}
```

- [ ] **Step 1: Write the failing app-stats tests**

```go
func TestAppStatsOneContainerWithLimits(t *testing.T) {
	// Stats{CPUPercent: 12.3, MemUsage: 118<<20, MemLimit: 1<<30,
	//       MemLimited: true, CPULimit: 1, Pids: 12, PidsLimit: 512}
	// want: CPUSub "limit 1 CPU", MemSub "of 1.0 GiB",
	//       Pids "12", PidsSub "of 512"
}

func TestAppStatsStackWithTheSameLimit(t *testing.T) {
	// two containers, both CPULimit 0.5 and PidsLimit 512
	// want: one "limit 0.5 CPU", one "of 512", sums the pids
}

func TestAppStatsStackWithDifferentLimits(t *testing.T) {
	// one limited, one not, or two different values
	// want: CPUSub "limits differ", MemSub "limits differ" when any is
	// limited, PidsSub "limits differ"
}

func TestAppStatsWithoutLimitsShowsToday(t *testing.T) {
	// CPULimit 0, MemLimited false, MemLimit 31<<30, PidsLimit 0
	// want: CPUSub "", MemSub "of 31 GiB" (today's host-total text),
	//       Pids "", PidsSub ""
}
```

Run: `go test ./internal/server/ -run 'AppStats.*Limits|AppStats.*Stack|AppStatsWithoutLimits'`
Expected: FAIL; the response has no limit fields.

- [ ] **Step 2: Implement the aggregation**

Add a helper `commonLimit` that returns `(value, same bool, any bool)` for a slice of per-sample values. For CPU, format with `fmtCPU` (trim trailing zeros: `1`, `0.5`, `1.5`).

For memory, keep today's text when no sampled container has a limit: `MemSub = "of "+fmtBytes(agg.MemLimit)` using the maximum `MemLimit` (the host total). When every sampled container has `MemLimited` and the same `MemLimit`, use `"of "+fmtBytes(limit)`. When some have a memory limit and they differ, use `"limits differ"`. Never replace today's host-total text with “usage”.

For processes, set `Pids` and `PidsSub` only when at least one sampled container has `PidsLimit > 0`; otherwise leave both empty so the page renders no Processes figure. Sum `Pids`; `PidsSub` is `"of N"` when every sampled container has the same `PidsLimit > 0`, and `"limits differ"` when they differ. `CPUSub` is `"limit N CPU"` when all agree, `"limits differ"` when they differ, empty when none.

- [ ] **Step 3: Update the app template**

Add a Processes stat, hidden by default:

```html
<div class="stat" id="metric-pids-stat" hidden><div class="stat-label">Processes</div><div class="stat-value" id="metric-pids">—</div><div class="stat-sub" id="metric-pids-sub">processes</div></div>
```

Add `id="metric-cpu-sub"` to the CPU sub-line (its default text stays “100% = one core”). Update the poller:

- set `cpuSub` to `s.cpuSub || '100% = one core'`;
- set `memSub` to `s.memSub || 'usage'` (the server supplies today's `of 31 GiB` when there is no limit);
- set `pids`/`pidsSub` and unhide `metric-pids-stat` only when `s.pids` is non-empty; otherwise hide it.

Reset all of them in `dashes()`.

- [ ] **Step 4: Write the failing metrics tests**

```go
func TestMetricsSampleShowsLimitsAndProcesses(t *testing.T) {
	// one container with CPULimit 1, MemLimited true, MemLimit 1<<30,
	// Pids 12, PidsLimit 512
	// want: Processes true, CPU contains "limit 1", Mem contains "of 1.0 GiB",
	//       Pids "12 · limit 512"
}

func TestMetricsSampleWithoutLimits(t *testing.T) {
	// CPULimit 0, MemLimited false, MemLimit 31<<30, PidsLimit 0
	// want: Processes false, CPU has no "limit", Mem is today's usage text
	// with no "of", Pids ""
}
```

Run: `go test ./internal/server/ -run MetricsSample.*Limits`
Expected: FAIL; `containerStat` has no fields.

- [ ] **Step 5: Implement the metrics rows and template**

In `containerStats`, build `CPU: fmt.Sprintf("%.1f%% · limit %s", st.CPUPercent, fmtCPU(st.CPULimit))` when `CPULimit > 0`, else today's percentage. Build `Mem` with ` · of <limit>` only when `MemLimited`, else today's usage text. Build `Pids: fmt.Sprintf("%d · limit %d", st.Pids, st.PidsLimit)` only when `PidsLimit > 0`; otherwise leave it empty. Set `metricsSample.Processes` when any row has a non-empty `Pids`. Add a Processes column to `metrics.tmpl` between Memory and Net I/O, hidden by default; the poller shows it only when `sample.processes` is true.

- [ ] **Step 6: Run everything**

Run: `go build ./... && go vet ./... && go test ./internal/server/`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/server
git commit -m "feat(server): show the limits and process counts beside live figures"
```

---

## Task 7: `OUTHAUL_MAX_CONCURRENT_BUILDS`

**Files:**
- Modify: `internal/config/config.go`, `internal/config/config_test.go`, `internal/core/limits.go`, `internal/store/deployments.go:405-428`, `internal/store/store_test.go`, `internal/deploy/worker.go:30-31,77-92,155-211`, `internal/deploy/deploy_test.go`, `internal/deploy/pipeline.go:78-130`, `internal/deploy/pipeline_compose.go:80-107`, `internal/server/server.go`, `internal/server/handlers.go:1990-2025`, `internal/server/templates/deployment.tmpl:1-25`, `internal/server/server_test.go`, `main.go:298-307`, `README.md`
- Test: the same files

**Interfaces:**
- Produces:

```go
// internal/config/config.go
MaxConcurrentBuilds int // 0 = unset, as today

// internal/store/deployments.go
// canBuild false returns only a deployment that has an image and no older
// queued deployment of its own app.
func (s *Store) NextClaimable(ctx context.Context, canBuild bool) (*core.Deployment, error)

// internal/deploy/worker.go
buildSlots chan struct{} // nil when the setting is unset
buildHeld  map[int64]bool // guarded by Worker.mu; which deployment holds a build slot
claim      func(ctx context.Context, id int64) (bool, error) // default store.ClaimDeployment; a test seam
func (w *Worker) releaseBuild(id int64)
func (w *Worker) holdBuild(id int64)

// internal/server/server.go
func (s *Server) SetBuildCap(n int)
```

- [ ] **Step 1: Write the failing store test**

```go
func TestNextClaimablePrefersImagesWhenItCannotBuild(t *testing.T) {
	// queue: app A build (older), app B rollback (image), app C build
	// NextClaimable(ctx, false) -> B
	// NextClaimable(ctx, true) -> A
}

func TestNextClaimableDoesNotSkipAnOlderQueuedBuild(t *testing.T) {
	// app A: older queued build, newer queued rollback with an image
	// NextClaimable(ctx, false) -> nil (the older build is first; it cannot
	// jump ahead even though it could run without building)
}
```

Run: `go test ./internal/store/ -run NextClaimablePrefers|DoesNotSkip`
Expected: FAIL; `NextClaimable` takes no `canBuild`.

- [ ] **Step 2: Implement the query**

Keep today's query for `canBuild == true`. For `false`, add `d.image <> ''` and a second `NOT EXISTS` for an older queued row of the same app:

```sql
NOT EXISTS (
    SELECT 1 FROM deployments q
    WHERE q.app_id = d.app_id AND q.status = ?
      AND (q.created_at < d.created_at OR (q.created_at = d.created_at AND q.id < d.id))
)
```

Update the existing callers and tests to pass `true`.

- [ ] **Step 3: Write the failing worker tests**

```go
func TestBuildCapRunsBuildsOneAtATime(t *testing.T) {
	// cfg.MaxConcurrentBuilds = 1; two apps with blocking nixpacks builders
	// start the worker; one deployment is building, the other queued;
	// release the first builder; the second starts
}

func TestBuildCapLetsARollbackRunBesideABuild(t *testing.T) {
	// app A is building; app B's rollback (image pre-set) is claimed and runs
}

func TestBuildSlotIsReleasedWhenBuildingEnds(t *testing.T) {
	// compose.Fake.Up blocks; after the build finishes and the pipeline enters
	// deploying, a second build starts; the first Up is still blocked
}

func TestBuildCapDoesNotJumpAnOlderQueuedBuild(t *testing.T) {
	// app A: older queued build, newer rollback; with the cap full, the
	// rollback is not claimed before the older build
}

func TestBuildSlotIsReleasedWhenTheClaimFails(t *testing.T) {
	// cap 1; queue one build; h.worker.claim returns an error
	// call dispatch; want len(h.worker.buildSlots) == 0 afterwards
}

func TestBuildSlotIsReleasedWhenTheClaimLosesTheRace(t *testing.T) {
	// cap 1; queue one build; h.worker.claim returns (false, nil) once and
	// cancels the dispatch context, so the loop ends after the failed claim
	// want: len(h.worker.buildSlots) == 0 afterwards
}
```

Run: `go test ./internal/deploy/ -run 'BuildCap|BuildSlot'`
Expected: FAIL; the worker has no build semaphore.

- [ ] **Step 4: Implement the build semaphore**

In `NewWorker`, set `buildSlots: make(chan struct{}, cfg.MaxConcurrentBuilds)` when `cfg.MaxConcurrentBuilds > 0`; otherwise leave it nil. Initialize `buildHeld: map[int64]bool{}`. Set `claim: st.ClaimDeployment` so the tests can replace it. Replace `maxConcurrent` with `core.MaxPipelines` in the pipeline semaphore.

In `dispatch`, after taking the pipeline slot:

```go
canBuild := w.buildSlots == nil
if !canBuild {
	select {
	case w.buildSlots <- struct{}{}:
		canBuild = true
	default:
	}
}
dep, err := w.store.NextClaimable(ctx, canBuild)
```

If `NextClaimable` returns nil or an error, release the pipeline slot and the build slot (if one was taken). Before claiming, if `canBuild` is false and `dep.Image == ""`, release the pipeline slot, log, and return: the query should never produce that, and claiming it would leave a row stuck in `building`.

Claim through `w.claim(ctx, dep.ID)`. On a claim error, release the pipeline slot and the build slot, log, and return. On `!ok` (lost the race), release the pipeline slot and the build slot, then `continue` to the next claimable row. After a successful claim, if `dep.Image != ""`, release the build slot immediately; otherwise `w.holdBuild(dep.ID)`.

Leaking on either claim path would stop every build until a restart when the cap is 1; the two tests in Step 3 pin both releases.

`releaseBuild` is idempotent and guarded by the worker's mutex: if the ID is held, delete it, receive from `buildSlots`, and call `w.Notify()`. `holdBuild` records the ID. Call `releaseBuild`:

- in `w.fail` (idempotent, covers every building failure);
- immediately after the `building -> deploying` `SetStatus` succeeds, in both pipelines;
- when that transition is cancelled (`!ok`);
- in the pipeline goroutine's `defer` as a safety net.

A deployment that builds nothing never takes a slot, so its `releaseBuild` is a no-op.

- [ ] **Step 5: Add the setting, page note and wiring**

`p.builds("OUTHAUL_MAX_CONCURRENT_BUILDS")` accepts 1–4 and rejects anything else with the parser's variable-naming error. Add the README row.

`Server` gains `buildCap int` and `SetBuildCap`; `main.go` calls `srv.SetBuildCap(cfg.MaxConcurrentBuilds)`. `handleDeploymentDetail` adds `"BuildCap": s.buildCap`. In `deployment.tmpl`, after the reason:

```html
{{if and .BuildCap (eq .Deployment.Status "queued") (not .Deployment.Image)}}
<div class="alert">Waiting its turn. This server runs {{.BuildCap}} builds at a time.</div>
{{end}}
```

Add a server test that renders a queued deployment with no image and a cap of 2; the note names 2. A queued rollback (image set) and a cap of 0 show no note.

- [ ] **Step 6: Run everything**

Run: `go build ./... && go vet ./... && go test ./internal/store/ ./internal/deploy/ ./internal/server/ ./internal/config/`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/config internal/core internal/store internal/deploy internal/server main.go README.md
git commit -m "feat(deploy): cap how many deployments build at once"
```

---

## Task 8: Where R6 and R7 meet

This is the last commit of the second series. It makes strict mode refuse a Compose service that asks for more than the app's own limit.

**Files:**
- Modify: `internal/compose/policy.go`, `internal/compose/policy_test.go`, `internal/deploy/pipeline_compose.go`, `internal/deploy/deploy_compose_test.go`
- Test: the same files

**Interfaces:**
- Consumes: `Policy.Max core.Limits` from the R6 plan; `Worker.limits` from Task 5.
- Produces: no new names. `Check` now compares the three resources against `Policy.Max`.

- [ ] **Step 1: Write the failing `Check` tests**

```go
func TestCheckRefusesAServiceAboveTheAppLimit(t *testing.T) {
	// strict, Max {1000, 1<<30, 512}
	// cpus: 2 -> violation names both 2 and 1
	// mem_limit: 2g -> violation names both figures
	// pids_limit: 1024 -> violation names both figures
}

func TestCheckAllowsEqualAndLowerValues(t *testing.T) {
	// cpus: 1, mem_limit: 1g, pids_limit: 512 -> no violations
	// cpus: 0.5 -> no violations
}

func TestCheckRefusesUnlimitedPids(t *testing.T) {
	// pids_limit: -1 -> violation says it is unlimited
}

func TestCheckRefusesCPUQuotaAndAsksForCpus(t *testing.T) {
	// cpu_quota: 50000, cpu_period: 100000 -> violation says "set cpus instead"
}

func TestCheckLeavesLimitsAloneWithoutStrictMode(t *testing.T) {
	// the same file with Strict false -> no violation
}
```

Run: `go test ./internal/compose/ -run Check.*(Limit|Pids|Quota)`
Expected: FAIL; `Check` ignores `Policy.Max`.

- [ ] **Step 2: Implement the comparison**

After the allow-list checks for a service, when `p.Strict`:

- for each resource with a non-zero `Max` field, read the service's spelling (`cpus` or `deploy.resources.limits.cpus`, `mem_limit` or `deploy.resources.limits.memory`, `pids_limit` or `deploy.resources.limits.pids`);
- `pids_limit: -1` is refused as unlimited;
- `cpu_quota`/`cpu_period` are refused with `set cpus instead` so there is one figure to compare;
- a value above the maximum is a violation naming both figures and saying `a system admin can raise the app's limit`;
- equal or lower values pass.

Do this only in strict mode. Without strict mode a service's own limits stand, whatever they are.

- [ ] **Step 3: Pass the app's limit from the pipeline**

Where the R6 plan builds `compose.Policy`, set `Max: w.limits(ctx, app)`. No new condition: `Check` only compares when `Strict` is true.

Add a deploy test: strict mode, default limit 1 CPU, a Compose file with `cpus: 2` -> failed in `building`, reason names both figures and the admin can raise the limit, no build call.

- [ ] **Step 4: Run everything**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/compose internal/deploy
git commit -m "feat(compose): strict mode refuses a service above the app's limit"
```

---

## Task 9: Documentation and the real-host check

**Files:**
- Modify: `README.md`, `docs/GUARD-RAILS.md`, `ARCHITECTURE.md`, `AGENTS.md`
- Test: `go build ./... && go vet ./... && go test ./...`, then `go test -race -timeout 30m ./...`

**Interfaces:**
- Consumes: everything above.
- Produces: the user-facing explanation of limits and the build cap.

- [ ] **Step 1: Finish `docs/GUARD-RAILS.md`**

Append, in the same plain style as the R6 sections:

- the four settings, their accepted values and defaults;
- which container gets which limit, as a table: single-container app and its health-check container, Compose service, preview, database, backup/restore/data-removal helper, Traefik/cloudflared, build step;
- that a Compose service keeps any resource it sets itself, and what Outhaul adds when it sets none;
- that a service reserving more memory than its limit fails the deploy before `up`;
- that a changed limit applies on the next deploy, and that a default change redeploys nothing;
- the build cap: only deployments that build wait, rollbacks and protection redeploys do not, and a waiting deployment stays `queued`;
- the real-host checks below, marked as not yet run.

- [ ] **Step 2: Update the other documents**

- `README.md`: the four rows added in Tasks 1 and 7; check the default text says “unset (no limit)” or “unset (up to four)”.
- `ARCHITECTURE.md`: add “Resource limits (opt-in)” with the table of which container gets which limit, and “Queue & concurrency semantics” with the build cap and a pointer from the locked “Background jobs” line, which stays as written. Add `core/limits.go` and `server/limits.go` to the package layout.
- `AGENTS.md`, “Decisions to keep”: every container-creating path sets its limits; a deployment that builds nothing never waits for a build slot; the model is never logged; a changed limit applies on the next deploy.

- [ ] **Step 3: Run the full check**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: PASS.

Run: `go test -race -timeout 30m ./...`
Expected: PASS.

- [ ] **Step 4: Leave the real-host checklist for the staging host**

These cannot be proved by the unit tests:

1. `docker inspect` on a Nixpacks app, a Compose service, a database and a preview shows `NanoCpus`, `Memory` and `PidsLimit`.
2. A container that allocates past its memory limit is killed, and the other apps keep answering.
3. Each of the nine templates deploys in strict mode with the default limits and stays up for a day. So does each database engine.
4. With a cap of 1, two apps deployed together build one after the other, and a rollback of a third starts at once.
5. `docker inspect` on a service that sets its own limit shows that value, not the server default.
6. A service that reserves more memory than its limit fails before `up`, naming both figures.

Record the results in `docs/GUARD-RAILS.md` under “Testing so far” when they have been run.

- [ ] **Step 5: Commit**

```bash
git add docs README.md ARCHITECTURE.md AGENTS.md
git commit -m "docs(guard-rails): limits, the limits panel and the build cap"
```
