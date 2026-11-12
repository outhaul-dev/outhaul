# Compose Restart Policy Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** With `OUTHAUL_COMPOSE_RESTART=unless-stopped`, give every Compose service that states no restart policy of its own the policy, so Docker brings the stack back after a reboot.

**Architecture:** A new `Config.ComposeRestart` setting makes the deploy pipeline read the Compose model it already parses (`compose.ReadModel`), and a pure function in `internal/compose` decides which services get the policy. The policy reaches a stack only through the generated override, in a new `Extra.Restart` field that R7's `Extra` gains and R7's per-service extras map merges with.

**Tech Stack:** Go 1.26, Docker Compose 2.35 or newer, `go test` with `compose.Fake`.

**Spec:** `docs/superpowers/specs/2026-11-11-shared-host-defaults-design.md`, R10. Read it with this plan: the section "R10: Compose services after a reboot", "Settings", "Tests", "Docs", "Order of work", "Left to the builder" and calls 8, 9, 10, 11 and 12.

**Base:** the R6 reader is already on `main` — `compose.ReadModel`, `compose.ParseModel` and `Service.HasRestart` in `internal/compose/model.go`. Use them as they are; do not change them. R7 (`docs/superpowers/plans/2026-11-11-container-limits.md`) changes `compose.Override` and defines `extra map[string]Extra` in its Task 4. **Building starts only once R7 is on `main`**: Task 2 adds a field to that `Extra`, and Task 4 merges its map with `compose.LimitExtras`. Both exist only after R7's Task 4. Until then this plan is a plan.

## Global Constraints

- The setting is exactly `OUTHAUL_COMPOSE_RESTART`, value `unless-stopped`. Unset means only what the Compose file says. It is read through the `parser` in `internal/config/config.go`; `Validate` names the variable and what it wants.
- An install that doesn't set it sees no change: no `docker compose config` call, the same override bytes, the same deploy log. This is R10's opt-in proof, and Task 4 owns it.
- A policy the file states is never changed. `restart` with any value counts, `"no"` included, and so does `deploy.restart_policy` (call 9).
- A service that another service waits on with `condition: service_completed_successfully` is left alone: restarted, it never completes and the deploy times out. Every other service without a policy gets one, run-once or not (call 10).
- This is the one way the setting changes what a stack does. The deploy log names every service given the policy and every one left alone with the reason; `docs/SHARED-HOST.md` says to give a run-once service `restart: "no"`.
- The policy reaches a stack at its next deploy. Nothing redeploys a stack that exists, Outhaul starts no app at boot, and `deploy/outhaul.service` does not change (call 12).
- The model is read only when a setting needs it (`needsModel`), and it is never logged, stored or quoted in an error. A file Compose can't load fails the deploy in `building`; R6's `ReadModel` already does that.
- R7's shapes stay as R7 wrote them: `type Extra struct { Limits core.Limits; InDeploy bool }` and `func Override(app core.App, domains []core.Domain, network string, rt traefik.Routing, extra map[string]Extra) []byte`. R10 adds `Restart string` to `Extra` and nothing else. `deploy/test/gatelabels/main.go` keeps passing `nil` extras.
- No migration, no store change, no route and no template: this is a server setting, not a per-app one.
- `example.com` names only, in code, tests, fixtures and docs. The repository is public.
- Commits: `feat(scope): summary`, `test(scope): summary` or `docs(scope): summary`; the body says what someone saw go wrong and what changed.
- Test first: write the failing test, watch it fail for the stated reason, then implement. After each task: `go build ./... && go vet ./... && go test ./...`. Before calling the plan done: `go test -race -timeout 30m ./...`, and for `deploy/**` also `sh deploy/test/run.sh` and `shellcheck -s sh --severity=warning deploy/test/test_unit.sh`.

## Review Focus

The inputs and conditions most likely to bite that no requirement spells out. Each has its test in the task named.

1. An install with the setting unset: no `docker compose config` call, no `restart:` line in the override, the same log (Task 4).
2. A service that states its own policy — `restart: "no"`, `always`, `on-failure:3`, or `deploy.restart_policy`: no `restart:` is written for it, whatever the value, and the override's service block for it does not appear (Tasks 2 and 4).
3. A run-once service another service waits on with `service_completed_successfully`: left alone, so `up --wait` still finishes; a service waited on only with `service_started` still gets the policy (Task 2).
4. A service that gets both an R7 limit and the policy: one service block carrying both, the limit fields unchanged (Task 3).
5. A Compose file Compose 2.35+ cannot load, on an install that turns the setting on: the deploy fails in `building`, the reason carries Compose's own line, nothing is built. This is new for such a file, because the setting adds a `config` call (Task 4).

---

## File map

| File | Responsibility | Tasks |
|---|---|---|
| `internal/config/config.go`, `internal/config/config_test.go` | `Config.ComposeRestart`, `OUTHAUL_COMPOSE_RESTART` | 1 |
| `README.md` | the setting's row in the configuration table | 1 |
| `internal/compose/override.go` | `Extra.Restart`, the per-service `restart:` line | 2, 3 |
| `internal/compose/restart.go` (new), `internal/compose/restart_test.go` (new) | `RestartExtras`, `LeftAlone`, its reasons, `MergeExtras` | 2 |
| `internal/compose/testdata/model/run-once.json` (new) | the recorded model the selection is tested on | 2 |
| `internal/compose/compose_test.go` | the override's `restart:` tests | 3 |
| `internal/deploy/worker.go` | `needsModel` reads the setting | 4 |
| `internal/deploy/pipeline_compose.go` | the selection, the merge with R7's extras, the log lines | 4 |
| `internal/deploy/deploy_compose_test.go` | the pipeline, setting on and off, and a file Compose can't read | 4 |
| `deploy/test/test_unit.sh` (new) | the unit file still starts after Docker | 5 |
| `docs/SHARED-HOST.md`, `ARCHITECTURE.md`, `AGENTS.md` | the setting, the exceptions and the decisions | 5 |

---

## Task 1: The setting

**Files:**
- Modify: `internal/config/config.go`, `internal/config/config_test.go`, `README.md`
- Test: `internal/config/config_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces:

```go
// internal/config/config.go
ComposeRestart string // OUTHAUL_COMPOSE_RESTART: "unless-stopped", or "" for none
```

- [ ] **Step 1: Write the failing Load test**

```go
func TestLoadComposeRestart(t *testing.T) {
	// unset -> ""
	// "unless-stopped" -> "unless-stopped"
	// "Unless-Stopped" -> "unless-stopped"
}
```

Run: `go test ./internal/config/ -run ComposeRestart`
Expected: FAIL; `Config` has no field `ComposeRestart`.

- [ ] **Step 2: Write the failing Validate test**

```go
func TestValidateComposeRestart(t *testing.T) {
	// "always" -> error naming OUTHAUL_COMPOSE_RESTART and unless-stopped
	// "on-failure:3" -> the same
	// "unless-stopped" -> nil
	// unset -> nil
}
```

Run: `go test ./internal/config/ -run ComposeRestart`
Expected: FAIL, for the same reason.

- [ ] **Step 3: Implement the setting**

Add the field to `Config` and one parser call to `Load`, beside `Network`:

```go
ComposeRestart: p.word("OUTHAUL_COMPOSE_RESTART", "", "unless-stopped"),
```

`p.word` already lower-cases the accepted value, takes empty as unset, and appends a problem naming the choices for anything else; `Validate` joins `problems`, so it needs no new case. Keep the doc comment on the field in the form above.

- [ ] **Step 4: Add the README row**

In `README.md`'s configuration table, after `OUTHAUL_CONTAINER_POLICY`:

```markdown
| `OUTHAUL_COMPOSE_RESTART` | (unset: only the Compose file's own policies) | `unless-stopped` gives every Compose service that states no restart policy of its own one, so Docker brings the stack back after a reboot — see docs/SHARED-HOST.md |
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/config/ -run 'ComposeRestart' && go test ./internal/config/ ./internal/core/`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/config README.md
git commit -m "feat(config): add OUTHAUL_COMPOSE_RESTART"
```

---

## Task 2: Which services get the policy

**Files:**
- Create: `internal/compose/restart.go`, `internal/compose/restart_test.go`, `internal/compose/testdata/model/run-once.json`
- Modify: `internal/compose/override.go` (the `Extra` field only)
- Test: `internal/compose/restart_test.go`

**Interfaces:**
- Consumes: `Model`, `Service`, `Service.HasRestart`, `readFixture` from `internal/compose/model.go` and `model_test.go`; `Extra` from R7's Task 4; `core.Limits.Or`.
- Produces:

```go
// internal/compose/override.go
type Extra struct {
	Limits   core.Limits // only the fields to write
	InDeploy bool        // write them under deploy.resources.limits
	Restart  string      // the restart policy to write; "" for none
}

// internal/compose/restart.go

// LeftAlone is one service that keeps the restart behaviour its file gives it.
type LeftAlone struct {
	Service string
	Reason  string // one of the two constants below
}

const (
	LeftAloneOwnPolicy = "it sets its own restart policy"
	LeftAloneWaitedOn  = "another service waits for it to finish"
)

// RestartExtras returns an Extra{Restart: policy} for every service that states
// no policy of its own and that no service waits on to complete, plus every
// service it left alone. Both are in Model order (service name). An empty
// policy returns nil, nil.
func RestartExtras(m Model, policy string) (map[string]Extra, []LeftAlone)

// MergeExtras unions two per-service extras maps: each field comes from the map
// that has it. Two empty maps return nil.
func MergeExtras(a, b map[string]Extra) map[string]Extra
```

- [ ] **Step 1: Record the fixture**

On a host with Docker Compose 2.35 or newer, in an empty temp directory, write `compose.yaml`:

```yaml
services:
  api:
    image: example.com/api:1
    depends_on:
      web:
        condition: service_started
  batch:
    image: example.com/batch:1
  db:
    image: postgres:16
    restart: "no"
  migrate:
    image: example.com/migrate:1
  web:
    image: nginx:1.27
    depends_on:
      migrate:
        condition: service_completed_successfully
  worker:
    image: example.com/worker:1
    deploy:
      restart_policy:
        condition: on-failure
```

Record it:

```sh
docker compose -p demo -f compose.yaml config --format json --no-env-resolution \
  > internal/compose/testdata/model/run-once.json
```

It holds every row of the spec's table: `batch` is run-once and nothing waits on it, `migrate` is waited on to complete, `db` says `restart: "no"`, `worker` sets `deploy.restart_policy`, `web` is waited on only with `service_started`, and `api` states nothing. No `build:` section, so no path in the fixture needs trimming.

- [ ] **Step 2: Write the failing selection tests**

In `restart_test.go`, with a comment above `TestRestartExtras` naming the compose file, the command and the Compose version that produced `run-once.json` — the form `model_test.go` uses for `normalised.json`:

```go
func TestRestartExtras(t *testing.T) {
	// ParseModel(readFixture(t, "run-once.json")); RestartExtras(m, "unless-stopped")
	// extras has exactly api, batch, web, each Extra{Restart: "unless-stopped"}
	// left == [{Service: "db", Reason: LeftAloneOwnPolicy},
	//          {Service: "migrate", Reason: LeftAloneWaitedOn},
	//          {Service: "worker", Reason: LeftAloneOwnPolicy}] in that order
}

func TestRestartExtrasShortDependsOn(t *testing.T) {
	// hand-written model: web depends_on ["db"], db states nothing
	// both get the policy: the short form states no condition
}

func TestRestartExtrasWithoutTheSetting(t *testing.T) {
	// RestartExtras(m, "") -> nil extras, nil left
}

func TestMergeExtras(t *testing.T) {
	// MergeExtras(nil, nil) -> nil
	// a limits-only web, a restart-only worker, merged with web's restart:
	// web carries Limits, InDeploy and Restart; worker keeps its Restart
	// a limits map merged second does not replace the restart of a shared key
}
```

Run: `go test ./internal/compose/ -run 'RestartExtras|MergeExtras'`
Expected: FAIL; `RestartExtras`, `LeftAlone` and `Extra.Restart` are not defined.

- [ ] **Step 3: Implement the selection**

Add `Restart string` to `Extra` in `override.go` — nothing else in that file yet.

In `restart.go`:

- `RestartExtras` returns `nil, nil` for an empty policy.
- It first builds the set of services another service waits on to complete: for each service, read `Service.Raw["depends_on"]` and take the names whose condition is `service_completed_successfully`. Compose prints `depends_on` as an object of service name to `{"condition": ..., "required": ...}`; an array of names means the short form, which states no condition, so it adds nothing. Those are the only two shapes Compose emits.
- Then, for each service in `m.Services`: `HasRestart()` is `LeftAlone{Service: s.Name, Reason: LeftAloneOwnPolicy}`; a name in the waited-on set is `LeftAlone{Service: s.Name, Reason: LeftAloneWaitedOn}`; everything else is `extras[s.Name] = Extra{Restart: policy}`. `nil` for an empty map and an empty slice.
- `MergeExtras` copies `a`, then for every key of `b` unions onto what may already be there: `cur.Limits = cur.Limits.Or(v.Limits)`, `cur.InDeploy = cur.InDeploy || v.InDeploy`, and `v.Restart` fills an empty `cur.Restart`. `nil` when the result is empty.

No `Service` field, no `Model` field, no parser change: `Raw` already holds `depends_on`.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/compose/ -run 'RestartExtras|MergeExtras|ParseModel|Override'`
Expected: PASS, including the existing model and override tests.

- [ ] **Step 5: Commit**

```bash
git add internal/compose
git commit -m "feat(compose): pick the services that gain the restart policy"
```

---

## Task 3: The override writes the policy

**Files:**
- Modify: `internal/compose/override.go`, `internal/compose/compose_test.go`
- Test: `internal/compose/compose_test.go`

**Interfaces:**
- Consumes: `Extra.Restart`, `MergeExtras` from Task 2; R7's `Override` signature, its union of domain services and extra keys, and its rule that the top-level `networks:` block appears only when a service has a domain.
- Produces: no new names. The rendering only.

- [ ] **Step 1: Write the failing override tests**

In `compose_test.go`, following the existing `TestOverride*` tests' style (`strings.Contains` on the rendered bytes):

```go
func TestOverrideWritesTheRestartPolicy(t *testing.T) {
	// no domains; extra {"worker": {Restart: "unless-stopped"}}
	// want services.worker with restart: "unless-stopped" and nothing else
	// no top-level networks: block
}

func TestOverrideWritesRestartBesideLabels(t *testing.T) {
	// domains for web; extra {"web": {Restart: "unless-stopped"}}
	// want: restart: comes before networks: and labels: under services.web,
	// and the router label for the domain is still there
}

func TestOverrideWritesRestartAndLimits(t *testing.T) {
	// extra from MergeExtras({web: {Limits: {MilliCPUs: 500}}},
	//                         {web: {Restart: "unless-stopped"},
	//                          worker: {Restart: "unless-stopped"}})
	// domains for web only
	// want: web has restart, cpus: 0.5 and its labels; worker has restart and
	// nothing else — no labels, no networks, no cpus
}
```

Run: `go test ./internal/compose/ -run Override`
Expected: FAIL; no `restart:` line is written.

- [ ] **Step 2: Implement the line**

In `Override`'s per-service loop — the one R7 keyed on the union of domain services and extra keys — write the policy as the service block's first key, before `networks:`:

```go
fmt.Fprintf(&b, "  %s:\n", quoteYAML(svc))
if p := extra[svc].Restart; p != "" {
	fmt.Fprintf(&b, "    restart: %s\n", quoteYAML(p))
}
```

Order it before R7's limit keys, so a service with a policy and limits reads `restart:`, then networks and labels when it has domains, then the limits. `quoteYAML` gives `"unless-stopped"`.

- [ ] **Step 3: Run the tests to verify they pass**

Run: `go test ./internal/compose/`
Expected: PASS, including R7's `TestOverrideWithNoExtrasIsUnchanged` against `testdata/override.golden`, which proves a nil extras map still renders today's bytes.

- [ ] **Step 4: Commit**

```bash
git add internal/compose
git commit -m "feat(compose): write the restart policy into the override"
```

---

## Task 4: The pipeline reads the model and logs its decision

**Files:**
- Modify: `internal/deploy/worker.go`, `internal/deploy/pipeline_compose.go`, `internal/deploy/deploy_compose_test.go`
- Test: `internal/deploy/deploy_compose_test.go`

**Interfaces:**
- Consumes: `Config.ComposeRestart` from Task 1; `RestartExtras`, `MergeExtras`, `LeftAlone`, `LeftAloneOwnPolicy`, `LeftAloneWaitedOn` from Task 2; R7's `needsModel(ctx, app)`, `extra` map and `LimitExtras` call in `runComposePipeline`.
- Produces: `func (w *Worker) needsModel(ctx context.Context, app core.App) bool` — R7's signature, one clause longer.

- [ ] **Step 1: Write the failing pipeline tests**

```go
func TestComposePipelineGivesTheRestartPolicy(t *testing.T) {
	// h.worker.cfg.ComposeRestart = "unless-stopped"
	// h.compose.ConfigJSON: an inline model with four services — web states
	//   nothing, db says restart: "no", migrate is the target of web's
	//   service_completed_successfully, worker sets deploy.restart_policy
	// app with no domain rows, so the override exists only for the extras
	// want: status running; exactly one config call; the override on disk has
	//   restart: "unless-stopped" under services.web and no services.db,
	//   services.migrate or services.worker, and no top-level networks:
	// and the log, in this order:
	//   Restart policy "unless-stopped" (OUTHAUL_COMPOSE_RESTART):
	//     db: left alone, it sets its own restart policy
	//     migrate: left alone, another service waits for it to finish
	//     web: restart: unless-stopped
	//     worker: left alone, it sets its own restart policy
}

func TestComposePipelineWithoutTheRestartSettingIsUnchanged(t *testing.T) {
	// cfg.ComposeRestart unset, ConfigJSON unset, one domain row
	// want: status running; zero config calls; the override carries the
	//   domain's router label and no "restart" text at all
}

func TestComposePipelineFailsWhenComposeCannotReadTheFile(t *testing.T) {
	// cfg.ComposeRestart = "unless-stopped"; h.compose.FailConfig =
	//   errors.New("compose go: yaml: line 3: found character that cannot start any token")
	// want: failed in building; the reason carries Compose's line; zero build
	//   and zero up calls
}
```

Read the log with `h.logOf(dep)` and the generated file through `h.compose.Hook`, as `TestComposePipelineHappyPath` does. Keep the model JSON inline, so the test doesn't depend on the fixture's path.

Run: `go test ./internal/deploy/ -run 'RestartPolicy|RestartSetting|CannotReadTheFile'`
Expected: FAIL; the setting is never read and no `restart:` appears.

- [ ] **Step 2: Let `needsModel` see the setting**

In `worker.go`, R7's function gains the clause and its doc comment gains the reason:

```go
func (w *Worker) needsModel(ctx context.Context, app core.App) bool {
	return w.cfg.StrictContainerPolicy() || w.cfg.HeaderRequired() ||
		w.limits(ctx, app) != (core.Limits{}) ||
		w.cfg.ComposeRestart != ""
}
```

The comment already reserves the place for it.

- [ ] **Step 3: Select, merge and log in `runComposePipeline`**

After R7's block that computes `extra` from `compose.LimitExtras`, and before the override is written:

```go
if w.cfg.ComposeRestart != "" {
	restart, left := compose.RestartExtras(model, w.cfg.ComposeRestart)
	extra = compose.MergeExtras(extra, restart)
	logRestartPolicy(out, w.cfg.ComposeRestart, restart, left)
}
```

The guard is safe: `needsModel` now includes the setting, so the model was read and `compose.Check` ran with neither strict mode nor a required header, which returns no violations. Leave R7's write condition (`len(domains) > 0 || len(extra) > 0`) as it is; a stack with restart extras and no domains now gets a file.

Add the unexported helper at the bottom of `pipeline_compose.go`:

```go
// logRestartPolicy names every service of the stack once: the ones that gain
// the policy, and the ones the file or another service keeps as they are.
func logRestartPolicy(out io.Writer, policy string, extra map[string]compose.Extra, left []compose.LeftAlone)
```

It writes the header `Restart policy "<policy>" (OUTHAUL_COMPOSE_RESTART):`, then one indented line per service, sorted by name across both inputs: `<name>: restart: <policy>` from `extra`, `<name>: left alone, <reason>` from `left`. Nothing else goes in the log, and no value from the model.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/deploy/ ./internal/compose/ ./internal/config/`
Expected: PASS, including R6's `TestComposePipelineWithoutPolicyNeverReadsModel`, which now also proves the unset setting adds no `config` call.

- [ ] **Step 5: Run everything and commit**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: PASS.

```bash
git add internal/deploy
git commit -m "feat(deploy): give Compose services a restart policy"
```

---

## Task 5: Docs and the unit-file test

**Files:**
- Create: `deploy/test/test_unit.sh`
- Modify: `docs/SHARED-HOST.md`, `ARCHITECTURE.md`, `AGENTS.md`
- Test: `deploy/test/test_unit.sh`

**Interfaces:**
- Consumes: the setting from Task 1, the exceptions from Task 2, the log lines from Task 4.
- Produces: nothing code calls.

- [ ] **Step 1: Write the unit-file test**

`deploy/test/test_unit.sh`, in the style of `test_goarch.sh` (POSIX `sh`, `assert.sh`, a final `[ "${TESTS_FAIL:-0}" -eq 0 ]`):

```sh
#!/usr/bin/env sh
here=$(cd "$(dirname "$0")" && pwd)
. "$here/assert.sh"

unit="$here/../outhaul.service"
after=$(sed -n 's/^After=//p' "$unit")
wants=$(sed -n 's/^Wants=//p' "$unit")

assert_contains "After: docker.service" " $after " " docker.service "
assert_contains "After: network-online.target" " $after " " network-online.target "
assert_contains "Wants: docker.service" " $wants " " docker.service "
assert_contains "Wants: network-online.target" " $wants " " network-online.target "

[ "${TESTS_FAIL:-0}" -eq 0 ]
```

The spaces inside the needles make a name a whole word, so `docker.service.d` can't pass for it. The file passes at once: it is a guard for the spec's "The unit file doesn't change", not a fix. `run.sh` picks it up because it runs every `test_*.sh`.

- [ ] **Step 2: Watch it fail for the right reason**

Temporarily delete `docker.service` from `After=` in `deploy/outhaul.service`, run `sh deploy/test/test_unit.sh`, and see `FAIL After: docker.service`. Restore the line.

- [ ] **Step 3: Run the installer tests**

Run: `sh deploy/test/test_unit.sh && dash deploy/test/test_unit.sh && bash --posix deploy/test/test_unit.sh && busybox sh deploy/test/test_unit.sh && sh deploy/test/run.sh`
Expected: PASS in each shell, and every installer test file still passes.

Run: `shellcheck -s sh --severity=warning deploy/test/test_unit.sh`
Expected: no output.

- [ ] **Step 4: Commit the test**

```bash
git add deploy/test/test_unit.sh
git commit -m "test(installer): the unit starts after Docker"
```

- [ ] **Step 5: Write the docs**

`docs/SHARED-HOST.md`: add a `## Compose services after a reboot` section, and a row to the settings table the file's other sections use:

| Variable | Values | Default | Needs |
|---|---|---|---|
| `OUTHAUL_COMPOSE_RESTART` | `unless-stopped` | unset: only what the Compose file says | nothing |

The section says, with a `sh` example `OUTHAUL_COMPOSE_RESTART=unless-stopped` and a short compose snippet carrying `restart: "no"` on a migration job:

- what the setting does, and that each service gets `unless-stopped` at the stack's next deploy, never by redeploying a stack that exists;
- the three services left alone: one that sets `restart` (any value, `"no"` included), one that sets `deploy.restart_policy`, and one another service waits on with `service_completed_successfully` — that last one is why the deploy would otherwise time out;
- the case the file can't show: a run-once service nothing waits on runs again each time it exits, so give it `restart: "no"`; the deploy log names every service either way;
- that Stop stays stopped across a reboot, as it does for a single-container app.

If `docs/SHARED-HOST.md` doesn't exist yet (R8's series creates it), create it with the title `# A shared host`, one intro sentence naming this setting, the table and the section; the second of the two to land folds the other's intro into one sentence. If it exists, extend that intro's last sentence and add the section after the existing ones.

`ARCHITECTURE.md`: a `### Compose restart policy (opt-in)` subsection after "Container guard rails (opt-in)", naming the setting, the selection rule and its two exceptions, `Extra.Restart` in the override, `needsModel` as the switch that makes the model read, that nothing redeploys an existing stack, and that the unit file already orders itself after `docker.service`, which `deploy/test/test_unit.sh` holds.

`AGENTS.md`: one bullet under "Decisions to keep", after "The Compose model is never logged.":

```markdown
- **Compose restart policy.** With `OUTHAUL_COMPOSE_RESTART=unless-stopped` a
  service that states no policy of its own gets one, except one another service
  waits on to finish (`condition: service_completed_successfully`). A policy the
  file states is never changed, `restart: "no"` and `deploy.restart_policy`
  included. The deploy log names every service either way, and nothing redeploys
  a stack that exists.
```

- [ ] **Step 6: Check the docs and commit**

Run: `grep -rn 'example\.com' docs/SHARED-HOST.md ARCHITECTURE.md` and check no other host name appears in the new text. Run: `go build ./... && go vet ./... && go test ./...`
Expected: PASS.

```bash
git add docs/SHARED-HOST.md ARCHITECTURE.md AGENTS.md
git commit -m "docs(shared-host): the Compose restart setting"
```

- [ ] **Step 7: Run the whole suite**

Run: `go test -race -timeout 30m ./...`
Expected: PASS.

Run: `sh deploy/test/run.sh && shellcheck -s sh --severity=warning deploy/bootstrap.sh deploy/update.sh deploy/test/*.sh`
Expected: PASS, no shellcheck output.
