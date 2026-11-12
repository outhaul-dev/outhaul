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
- The setting changes what a stack does in two ways, and both are documented. A run-once service that nothing waits on restarts every time it exits: the deploy log names every service given the policy and every one left alone with the reason, and `docs/SHARED-HOST.md` says to give such a service `restart: "no"`. And a service with no healthcheck that exits straight away no longer fails the deploy: checked on Compose v5.6.0 with Outhaul's `up -d --wait` flags, it fails `up` today (`container … exited (1)`, and exit 0 too), while with `unless-stopped` from the override `up` exits 0 and the service restarts in a loop. The same service with a healthcheck still fails `up --wait` (`is unhealthy`). The maintainer accepted this; the docs say it, and no new check runs after `up`.
- The policy reaches a stack at its next deploy. Nothing redeploys a stack that exists, Outhaul starts no app at boot, and `deploy/outhaul.service` does not change (call 12).
- The model is read only when a setting needs it (`needsModel`), and it is never logged, stored or quoted in an error. A file Compose can't load fails the deploy in `building`; R6's `ReadModel` already does that.
- R7's shapes stay as R7 wrote them: `type Extra struct { Limits core.Limits; InDeploy bool }` and `func Override(app core.App, domains []core.Domain, network string, rt traefik.Routing, extra map[string]Extra) []byte`. R10 adds `Restart string` to `Extra` and nothing else. `deploy/test/gatelabels/main.go` keeps passing `nil` extras.
- No migration, no store change, no route and no template: this is a server setting, not a per-app one.
- `example.com` names only, in code, tests, fixtures and docs. The repository is public.
- Commits: `feat(scope): summary`, `test(scope): summary` or `docs(scope): summary`, and every one carries a body, as AGENTS.md asks: what someone saw go wrong, and what changed.
- Test first: write the failing test, watch it fail for the stated reason, then implement. After each task: `go build ./... && go vet ./... && go test ./...`. Before calling the plan done: `go test -race -timeout 30m ./...`, and for `deploy/**` also `sh deploy/test/run.sh` and `shellcheck -s sh --severity=warning deploy/test/test_unit.sh`.

## Review Focus

The inputs and conditions most likely to bite that no requirement spells out. Each has its test in the task named.

1. An install with the setting unset: no `docker compose config` call, no `restart:` line in the override, the same log (Task 4).
2. A service with no healthcheck that exits at start, on an install with the setting on: today `up --wait` fails the deploy; given the policy it exits 0 and the service restarts in a loop, and a healthcheck still fails the deploy. Nothing checks a container after `up`. The behaviour is documented in Task 5, and Task 3's real-Compose step shows the merged file carries `restart` on exactly the named services and nowhere else.
3. A service that states its own policy — `restart: "no"`, `always`, `on-failure:3`, or `deploy.restart_policy`: no `restart:` is written for it, whatever the value, and the override's service block for it does not appear (Tasks 2 and 4).
4. A run-once service another service waits on with `service_completed_successfully`: left alone, so `up --wait` still finishes; a service waited on only through the short `depends_on` form, which Compose records as `condition: service_started`, still gets the policy (Task 2).
5. A service that gets both an R7 limit and the policy: one service block carrying both, the limit fields unchanged (Task 3).
6. A Compose file Compose 2.35+ cannot load, on an install that turns the setting on: the deploy fails in `building`, the reason carries Compose's own line, nothing is built. This is new for such a file, because the setting adds a `config` call (Task 4).

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
| `internal/deploy/deploy_compose_test.go` | the pipeline, setting on and off, and the two refusals | 4 |
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
| `OUTHAUL_COMPOSE_RESTART` | (unset: only the Compose file's own policies) | `unless-stopped` gives every Compose service that states no restart policy of its own one, so Docker brings the stack back after a reboot; needs Docker Compose 2.35 or newer — see docs/SHARED-HOST.md |
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/config/ -run 'ComposeRestart' && go test ./internal/config/ ./internal/core/`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/config README.md
git commit -m "feat(config): add OUTHAUL_COMPOSE_RESTART" -m "A Compose service came back after a reboot only if its own file said so, while single-container apps and databases came back themselves. Setting OUTHAUL_COMPOSE_RESTART=unless-stopped gives every Compose service that states no policy of its own one; unset, nothing changes."
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
// policy returns nil, nil, nil. A service whose depends_on cannot be read fails
// the whole call: the error names the service and the option, never a value.
func RestartExtras(m Model, policy string) (map[string]Extra, []LeftAlone, error)

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
      - web
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

It holds every row of the spec's table: `batch` is run-once and nothing waits on it, `migrate` is waited on to complete, `db` says `restart: "no"`, `worker` sets `deploy.restart_policy`, `api` waits on `web` in the short form — which Compose records as `{"condition": "service_started"}` — and `api` itself states nothing. No `build:` section, so no path in the fixture needs trimming.

- [ ] **Step 2: Write the failing selection tests**

In `restart_test.go`, with a comment above `TestRestartExtras` naming the compose file, the command and the Compose version that produced `run-once.json` — the form `model_test.go` uses for `normalised.json`:

```go
func TestRestartExtras(t *testing.T) {
	// ParseModel(readFixture(t, "run-once.json")); RestartExtras(m, "unless-stopped")
	// extras has exactly api, batch, web, each Extra{Restart: "unless-stopped"}:
	//   api is the waiter in the short form, so nobody waits on it to complete
	// left == [{Service: "db", Reason: LeftAloneOwnPolicy},
	//          {Service: "migrate", Reason: LeftAloneWaitedOn},
	//          {Service: "worker", Reason: LeftAloneOwnPolicy}] in that order
}

func TestRestartExtrasRefusesAnUnreadableDependsOn(t *testing.T) {
	// hand-written model: {"services": {"web": {"image": "nginx",
	//   "depends_on": "db"}, "db": {"image": "postgres:16"}}}
	// want: an error whose text contains service "web" and depends_on, and
	//   does not contain db
}

func TestRestartExtrasWithoutTheSetting(t *testing.T) {
	// RestartExtras(m, "") -> nil extras, nil left, nil error
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

- `RestartExtras` returns `nil, nil, nil` for an empty policy.
- It first builds the set of services another service waits on to complete: for each service that has a `depends_on` key, unmarshal it into `map[string]struct{ Condition string }` and take the names whose condition is `service_completed_successfully`. Compose prints the long form as an object of service name to `{"condition": ..., "required": ...}`, and the short form (`depends_on: [web]`) as the same object with `condition: service_started`, so one shape covers both. A value that does not unmarshal is an error: `fmt.Errorf("service %q: depends_on: this option could not be read", s.Name)`. Return it as it is — do not wrap `json.Unmarshal`'s error, which can quote a character of the value — so the message names the service and the option and nothing else.
- Then, for each service in `m.Services`: `HasRestart()` is `LeftAlone{Service: s.Name, Reason: LeftAloneOwnPolicy}`; a name in the waited-on set is `LeftAlone{Service: s.Name, Reason: LeftAloneWaitedOn}`; everything else is `extras[s.Name] = Extra{Restart: policy}`. `nil` for an empty map and an empty slice, and `nil` for the error.
- `MergeExtras` copies `a`, then for every key of `b` unions onto what may already be there: `cur.Limits = cur.Limits.Or(v.Limits)`, `cur.InDeploy = cur.InDeploy || v.InDeploy`, and `v.Restart` fills an empty `cur.Restart`. `nil` when the result is empty.

No `Service` field, no `Model` field, no parser change: `Raw` already holds `depends_on`.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/compose/ -run 'RestartExtras|MergeExtras|ParseModel|Override'`
Expected: PASS, including the existing model and override tests.

- [ ] **Step 5: Commit**

```bash
git add internal/compose
git commit -m "feat(compose): pick the services that gain the restart policy" -m "The override knew only the services with domain rows, so nothing could tell which of a stack's services a restart policy should reach. compose.RestartExtras reads the recorded model, leaves alone a service that states its own policy and one another service waits on to complete, and returns the rest; MergeExtras folds its map into R7's limits map. A depends_on it cannot read is an error naming the service, not a value."
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

- [ ] **Step 4: Check the merged file with real Compose**

`compose config` reads both files and prints the merged stack. It creates no container, and it needs Docker Compose 2.35 or newer.

From the repo root, render the override from the recorded fixture with a throwaway program — do not commit it:

```sh
mkdir -p /tmp/r10check zz-r10-check && cat > zz-r10-check/main.go <<'EOF'
package main

import (
	"os"

	"github.com/outhaul-dev/outhaul/internal/compose"
	"github.com/outhaul-dev/outhaul/internal/core"
	"github.com/outhaul-dev/outhaul/internal/traefik"
)

func main() {
	b, err := os.ReadFile("internal/compose/testdata/model/run-once.json")
	if err != nil {
		panic(err)
	}
	m, err := compose.ParseModel(b)
	if err != nil {
		panic(err)
	}
	extra, _, err := compose.RestartExtras(m, "unless-stopped")
	if err != nil {
		panic(err)
	}
	os.Stdout.Write(compose.Override(core.App{Name: "demo"}, nil, "outhaul", traefik.Routing{}, extra))
}
EOF
go run ./zz-r10-check > /tmp/r10check/outhaul.override.yml
rm -rf zz-r10-check
```

Write Task 2 Step 1's `compose.yaml` to `/tmp/r10check/` as well, then:

```sh
cd /tmp/r10check
docker compose -p demo -f compose.yaml -f outhaul.override.yml config --format json > merged.json
grep -o '"restart": "unless-stopped"' merged.json | wc -l   # want 3: api, batch, web
grep -o '"restart"' merged.json | wc -l                      # want 4: those three and db's "no"
```

Read `merged.json` and confirm the three services are `api`, `batch` and `web`, and that `db`, `migrate` and `worker` gained no `restart`. Back in the repo, `git status` must show nothing: the throwaway package is gone and nothing else was written.

- [ ] **Step 5: Commit**

```bash
git add internal/compose
git commit -m "feat(compose): write the restart policy into the override" -m "A stack deployed with OUTHAUL_COMPOSE_RESTART=unless-stopped still came back after a reboot only if its own file said so. The override now writes restart: as the first key of each service the selection names, so the policy reaches Docker without changing a policy the file states."
```

---

## Task 4: The pipeline reads the model and logs its decision

**Files:**
- Modify: `internal/deploy/worker.go`, `internal/deploy/pipeline_compose.go`, `internal/deploy/deploy_compose_test.go`
- Test: `internal/deploy/deploy_compose_test.go`

**Interfaces:**
- Consumes: `Config.ComposeRestart` from Task 1; `RestartExtras` and its error, `MergeExtras`, `LeftAlone`, `LeftAloneOwnPolicy`, `LeftAloneWaitedOn` from Task 2; R7's `needsModel(ctx, app)`, `extra` map and `LimitExtras` call in `runComposePipeline`.
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

func TestComposePipelineRefusesAnUnreadableDependsOn(t *testing.T) {
	// cfg.ComposeRestart = "unless-stopped"
	// ConfigJSON: web's depends_on is the string "db"
	// want: failed in building; the reason contains service "web" and
	//   depends_on and does not contain db; zero build and zero up calls
}
```

Read the log with `h.logOf(dep)` and the generated file through `h.compose.Hook`, as `TestComposePipelineHappyPath` does. Keep the model JSON inline, so the test doesn't depend on the fixture's path.

Run: `go test ./internal/deploy/ -run 'RestartPolicy|RestartSetting|CannotReadTheFile|UnreadableDependsOn'`
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
	restart, left, err := compose.RestartExtras(model, w.cfg.ComposeRestart)
	if err != nil {
		w.fail(dep, core.StatusBuilding, err.Error(), out)
		return
	}
	extra = compose.MergeExtras(extra, restart)
	logRestartPolicy(out, w.cfg.ComposeRestart, restart, left)
}
```

The error path fails the deploy in `building` before the override is written and before the build, as R7's memory-reservation case does; its text names the service and `depends_on` and nothing from the model.

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
git commit -m "feat(deploy): give Compose services a restart policy" -m "Nothing told a Compose deploy to read the stack's model for the restart setting, and the decision was invisible to whoever read the deploy log. needsModel now counts the setting, the pipeline merges the selection into the override's extras, and the log names every service given the policy or left alone. A depends_on it cannot read fails the deploy in building, naming the service."
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
git commit -m "test(installer): the unit starts after Docker" -m "The design keeps the unit file as it is, so the ordering it carries is the whole of the restart story. Nothing failed if a later edit dropped docker.service from After=; this test now does."
```

- [ ] **Step 5: Write the docs**

`docs/SHARED-HOST.md`: add a `## Compose services after a reboot` section, and a row to the settings table the file's other sections use:

| Variable | Values | Default | Needs |
|---|---|---|---|
| `OUTHAUL_COMPOSE_RESTART` | `unless-stopped` | unset: only what the Compose file says | Docker Compose 2.35 or newer on the host: every Compose deploy then reads the file's normalised model — see [GUARD-RAILS.md](GUARD-RAILS.md#docker-compose-235-or-newer) |

The section says, with a `sh` example `OUTHAUL_COMPOSE_RESTART=unless-stopped` and a short compose snippet carrying `restart: "no"` on a migration job:

- what the setting does, and that each service gets `unless-stopped` at the stack's next deploy, never by redeploying a stack that exists;
- the three services left alone: one that sets `restart` (any value, `"no"` included), one that sets `deploy.restart_policy`, and one another service waits on with `service_completed_successfully` — that last one is why the deploy would otherwise time out;
- the case the file can't show: a run-once service nothing waits on runs again each time it exits, so give it `restart: "no"`; the deploy log names every service either way;
- what it does to a failed deploy: a service with no healthcheck that exits at start fails the deploy today (`container … exited (1)`, exit 0 included), and with the setting the deploy succeeds while the service restarts in a loop; a service with a healthcheck still fails `up --wait` (`is unhealthy`);
- the version: the setting needs Docker Compose 2.35 or newer, because from then on every Compose deploy reads the file's normalised model; on an older host a Compose deploy fails before it builds, with the message GUARD-RAILS.md already describes for that flag.

Then a `## What only the real host can show` section, or these lines under that heading if R8's part of the file already has one:

- After `sudo reboot`, and after `systemctl restart docker`, every Compose service of a stack deployed with the setting answers with nobody acting, and a stack stopped in the UI beforehand is still stopped. Not checked yet.

If `docs/SHARED-HOST.md` doesn't exist yet (R8's series creates it), create it with the title `# A shared host`, one intro sentence naming this setting, the table and the section; the second of the two to land folds the other's intro into one sentence. If it exists, extend that intro's last sentence and add the section after the existing ones.

`ARCHITECTURE.md`: a `### Compose restart policy (opt-in)` subsection after "Container guard rails (opt-in)", naming the setting, the selection rule and its two exceptions, `Extra.Restart` in the override, `needsModel` as the switch that makes the model read and the Docker Compose 2.35 it therefore needs, that a service with no healthcheck which exits at start is restarted while the deploy still succeeds (today the deploy fails) and that a healthcheck still fails it, that nothing redeploys an existing stack, and that the unit file already orders itself after `docker.service`, which `deploy/test/test_unit.sh` holds.

`AGENTS.md`: one bullet under "Decisions to keep", after "The Compose model is never logged.":

```markdown
- **Compose restart policy.** With `OUTHAUL_COMPOSE_RESTART=unless-stopped` a
  service that states no policy of its own gets one, except one another service
  waits on to finish (`condition: service_completed_successfully`). A policy the
  file states is never changed, `restart: "no"` and `deploy.restart_policy`
  included. A service with no healthcheck that exits at start is restarted and
  the deploy still succeeds, where today's deploy fails; a healthcheck still
  fails `up --wait`. The deploy log names every service either way, nothing
  redeploys a stack that exists, and the setting needs Docker Compose 2.35 or
  newer.
```

- [ ] **Step 6: Check the docs and commit**

Run: `grep -rn 'example\.com' docs/SHARED-HOST.md ARCHITECTURE.md` and check no other host name appears in the new text. Run: `go build ./... && go vet ./... && go test ./...`
Expected: PASS.

```bash
git add docs/SHARED-HOST.md ARCHITECTURE.md AGENTS.md
git commit -m "docs(shared-host): the Compose restart setting" -m "The setting was invisible to anyone who had not read the design, including what it does to a service that exits at start and the Docker Compose version it needs. docs/SHARED-HOST.md now says what it does, the services it leaves alone, both ways it changes what a stack does, the version, and what only a real host can show; ARCHITECTURE.md and AGENTS.md carry the decisions."
```

- [ ] **Step 7: Run the whole suite**

Run: `go test -race -timeout 30m ./...`
Expected: PASS.

Run: `sh deploy/test/run.sh && shellcheck -s sh --severity=warning deploy/bootstrap.sh deploy/update.sh deploy/test/*.sh`
Expected: PASS, no shellcheck output.
