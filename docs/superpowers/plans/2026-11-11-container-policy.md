# Container Guard Rails (Strict Policy) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add an opt-in strict container policy that refuses a Compose deploy before anything is built when its file reaches the host or another app, and refuses images and managed-database ports that bypass it.

**Architecture:** Read `docker compose config --format json --no-env-resolution` into a normalised `compose.Model`. A pure `compose.Check` walks the model against an allow-list and returns violations naming the service and option. `runComposePipeline` reads and checks the model before the override and `compose build`, so a refusal fails in `building`. Single-container and Compose images are inspected for `traefik.*` labels, and strict mode reduces the environment `docker compose` inherits.

**Tech Stack:** Go 1.26, Docker Engine API, Docker Compose 2.35 or newer (for `--no-env-resolution`), Traefik v3.7.6 labels, POSIX `sh` for the real-Compose checks.

**Spec:** `docs/superpowers/specs/2026-11-11-container-guard-rails-design.md`. Read it first; this plan implements R6 and the shared Compose-model reader. R7 is `docs/superpowers/plans/2026-11-11-container-limits.md`.

**Base:** Start from a branch that contains the proxy-ingress plan's Task 1 (`traefik.Routing`, `Worker.routing`) and R3 (`Config.HeaderRequired()`). Both are used directly by Tasks 4 and 5. If the proxy-ingress plan is not merged yet, execute it first; do not start this plan from the docs-only branch.

## Global Constraints

- An install that sets none of the new settings sees no change: no `docker compose config` call, the same override, the same labels, the same environment, no new panel.
- The only new setting in this plan is `OUTHAUL_CONTAINER_POLICY`; its only accepted value is `strict`, in any case. Unset means off. It is read through the `parser` in `internal/config/config.go`, and `Validate` reports a bad value.
- No YAML dependency. The model is the JSON `docker compose config --format json` prints, read with `encoding/json` and `json.RawMessage`.
- The user's Compose file is never rewritten. Outhaul only adds its override file.
- The model holds every interpolated value. It is parsed in memory and never logged, stored, or quoted in an error. Only violation `Where`, `Option` and `Reason` reach a log.
- Strict mode is an allow-list: a key not named in the spec's tables is refused as unknown. Unknown keys are a feature, not a gap.
- A refusal names the service and the option. The build log lists every violation; the deployment reason is the first with a count of the rest.
- Local paths are compared as text after `filepath.Clean`, against `WorkDir` as given and as `filepath.EvalSymlinks` resolves it. Symlinks inside the checkout are not followed.
- The label rule refuses any `traefik.*` or `outhaul.*` key in a Compose file's merged `labels` map, in any case. It is on in strict mode and while a request header is required.
- Managed databases publish no host port in strict mode. Preview databases never have one.
- Test first: write the failing test, watch it fail for the stated reason, then implement. Use `compose.Fake`, `docker.Fake`, and the deploy harness. What depends on real Compose behaviour runs `docker compose` with no daemon.
- This repository is public: `example.com` names only, in code, tests, fixtures and docs.
- Commits: `feat(scope): summary` or `fix(scope): summary`, with a body saying what someone saw go wrong and what changed.
- Docs travel with the code. Every task that adds a setting or a rule updates `README.md` or `docs/GUARD-RAILS.md` in the same commit. Task 9 finishes the documents.
- After each task: `go build ./... && go vet ./... && go test ./...`. Before calling the plan done: `go test -race -timeout 30m ./...`.
- The real-Compose tests skip when `docker compose version` cannot run. They need no Docker daemon.

## Review Focus

The inputs most likely to bite that no requirement spells out. Each has its test in the task named.

1. A Compose file using a newer key the brief never named (`pre_start`, `provider`, `use_api_socket`, `post_start[].privileged`): refused as unknown, never silently accepted (Task 2).
2. A local path reached through a symlinked data directory, a `../` path, or a symlink inside the checkout: paths stay inside `WorkDir` as given or as resolved, and checkout symlinks are not followed (Task 3).
3. A Compose file whose routing labels arrive through `label_file`, or an image whose labels carry `traefik.*`: both refused, naming the service and the label (Tasks 3 and 5).
4. A Compose file Compose cannot load, or a Compose older than 2.35: the deployment fails in `building` with Compose's message or the version message, and the JSON is never logged (Tasks 1 and 4).
5. A database row that still has an external port under strict mode: the page says so, offers one button to stop publishing it, and provisioning omits the mapping without restarting the database (Task 7).

---

## File map

| File | Responsibility | Tasks |
|---|---|---|
| `internal/compose/model.go` (new) | `Model`, `Service`, `ParseModel`, `ReadModel` | 1 |
| `internal/compose/compose.go` | `Runner.Config`, `Runner.Pull`, `Docker.Config`, `Docker.run` env | 1, 5, 6 |
| `internal/compose/fake.go` | records `config` and `pull`, injects failures | 1, 5 |
| `internal/compose/policy.go` (new) | `Policy`, `Violation`, `Check`, the allow-list | 2, 3 |
| `internal/compose/env.go` (new) | the reduced environment for strict mode | 6 |
| `internal/core/limits.go` (new) | `Limits` and `Or`, shared with the R7 plan | 2 |
| `internal/config/config.go` | `OUTHAUL_CONTAINER_POLICY` | 2 |
| `internal/deploy/pipeline_compose.go` | read, check, pull, inspect, override | 4, 5, 6 |
| `internal/deploy/pipeline.go` | single-container image-label refusal | 5 |
| `internal/deploy/worker.go` | `needsModel`, the policy gathered from config | 4, 5 |
| `internal/dbaas/manager.go` | omit a database port in strict mode | 7 |
| `internal/server/databases.go`, `projects.go`, `templates/database.tmpl`, `templates/project.tmpl` | refuse and hide the port; the stop-publishing button | 7 |
| `main.go` | strict setters, reduced Compose environment, startup port notice | 6, 7 |
| `internal/catalog/policy_test.go` (new) | every template passes strict `Check` | 8 |
| `docs/GUARD-RAILS.md` (new), `README.md`, `ARCHITECTURE.md`, `AGENTS.md`, `docs/PROTECTED-ROUTES.md`, `docs/BEHIND-A-PROXY.md` | the policy, the label rule, the real-host checks | 2, 3, 5, 9 |

---

## Task 1: Read the normalised Compose model

This is the shared first commit. Nothing calls the reader yet, so it changes no output. R10 and the R7 plan are built on this commit.

**Files:**
- Create: `internal/compose/model.go`, `internal/compose/model_test.go`, `internal/compose/testdata/model/*.json`
- Modify: `internal/compose/compose.go`, `internal/compose/fake.go`
- Test: `internal/compose/model_test.go`, `internal/compose/compose_test.go`

**Interfaces:**
- Produces:

```go
// internal/compose/compose.go
type Runner interface {
	// Config runs `docker compose -p <project> -f <files...> config
	// --format json --no-env-resolution` in dir and returns stdout. Compose's
	// warnings go to warn (the deploy log).
	Config(ctx context.Context, dir string, files []string, project string, warn io.Writer) ([]byte, error)
	// Build, Up, Stop, Restart and Down as today. Task 5 adds Pull.
}

// internal/compose/model.go
type Model struct {
	Project  string
	Services []Service                     // sorted by Name
	Raw      map[string]json.RawMessage    // every top-level section and its value
}

type Service struct {
	Name                string
	Image               string // its `image`; "<project>-<name>" when it only builds
	Restart             string // its own `restart`; "" when it has none
	DeployRestartPolicy bool   // it sets deploy.restart_policy
	Labels              map[string]string
	Raw                 map[string]json.RawMessage // every service key, as printed
}

func (s Service) HasRestart() bool
func ParseModel(b []byte) (Model, error) // pure; accepts keys it does not know
func ReadModel(ctx context.Context, r Runner, dir string, files []string, project string, warn io.Writer) (Model, error)

// internal/compose/fake.go
type Fake struct {
	// existing fields...
	ConfigJSON []byte
	FailConfig error
	// Config appends Call{Verb: "config"} to Calls, so CallsFor("config")
	// works like every other verb. Task 5 adds FailPull and "pull" calls.
}
```

- [ ] **Step 1: Write the failing `ParseModel` tests**

Create `internal/compose/testdata/model/normalised.json` by running the command against a fixture with a web service that builds and a db service that sets `restart: always` and `deploy.restart_policy: on-failure`. Add a second fixture with an unknown service key (`x-example: kept`), `labels` as a map (Compose has already merged `label_file`), and `mem_limit: 0` (Compose leaves it out).

In `internal/compose/model_test.go`:

```go
func TestParseModel(t *testing.T) {
	// web builds only -> Image is "demo-web"
	// db sets restart -> Restart "always", DeployRestartPolicy true
	// x-example survives in Raw, and services are sorted by name
}

func TestParseModelKeepsUnknownTopLevelKeys(t *testing.T) {
	// a top-level "x-example" key is in Model.Raw
}

func TestReadModelUsesTheRunner(t *testing.T) {
	// compose.Fake{ConfigJSON: ...}; ReadModel returns the parsed model and
	// records one "config" call with the app's files only
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/compose/ -run 'ParseModel|ReadModel'`
Expected: FAIL; `Model`, `Service`, `ParseModel` and `ReadModel` are not defined.

- [ ] **Step 3: Implement `model.go`**

Decode the top-level object into `map[string]json.RawMessage`. Decode `services` as `map[string]json.RawMessage`; for each service decode `image`, `restart`, `labels`, and whether `deploy` contains `restart_policy`. If `image` is empty and `build` is present, set `Image` to `m.Project + "-" + name` (the project is the top-level `name`). Sort services by name. Keep every raw value.

`ReadModel` calls `r.Config(ctx, dir, files, project, warn)` and then `ParseModel`.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/compose/ -run 'ParseModel|ReadModel'`
Expected: PASS.

- [ ] **Step 5: Write the failing `Docker.Config` tests**

In `compose_test.go`:

```go
func TestConfigArgs(t *testing.T) {
	got := configArgs([]string{"docker-compose.yml"}, "outhaul-shop")
	want := []string{"compose", "-p", "outhaul-shop", "-f", "docker-compose.yml",
		"config", "--format", "json", "--no-env-resolution"}
	// compare
}

func TestDockerConfigNeedsCompose235(t *testing.T) {
	// d := &Docker{Bin: tempScript(t, "#!/bin/sh\necho 'unknown flag: --no-env-resolution' >&2\nexit 1\n")}
	// _, err := d.Config(...)
	// error contains "Docker Compose 2.35 or newer"
}
```

Add a passing case with a script that writes a small JSON document to stdout and a warning to stderr; assert the warning reaches `warn` and the JSON does not.

- [ ] **Step 6: Run the tests to verify they fail**

Run: `go test ./internal/compose/ -run 'ConfigArgs|DockerConfig'`
Expected: FAIL; `configArgs` and `Config` are not defined.

- [ ] **Step 7: Implement `Runner.Config`, `Docker.Config` and the fake**

Add `configArgs` beside `buildArgs`. `Docker.Config` uses a new `output` helper: `cmd.Stdout` is a `bytes.Buffer`, `cmd.Stderr` is an `io.MultiWriter(warn, &stderrBuf)` so the deploy log still sees every warning, and `cmd.Dir` is `dir`. If the command fails and the captured stderr mentions `unknown flag` or `unknown shorthand flag` for `--no-env-resolution`, return `reading a Compose file needs Docker Compose 2.35 or newer: %w`. Otherwise return the normal `docker compose ... failed` error.

Add `ConfigJSON` and `FailConfig` to `compose.Fake`; `Config` appends a `Call{Verb: "config", Dir: dir, Files: files, Project: project}` to `Calls`, honours `FailConfig`, and returns `ConfigJSON`.

- [ ] **Step 8: Run everything**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: PASS.

- [ ] **Step 9: Commit**

```bash
git add internal/compose
git commit -m "feat(compose): read the normalised Compose model"
```

---

## Task 2: The strict setting, `core.Limits`, and the service allow-list

**Files:**
- Create: `internal/core/limits.go`, `internal/core/limits_test.go`, `internal/compose/policy.go`, `internal/compose/policy_test.go`, `internal/compose/testdata/policy/*.json` and `*.yml`
- Modify: `internal/config/config.go`, `internal/config/config_test.go`, `README.md`
- Test: `internal/core/limits_test.go`, `internal/config/config_test.go`, `internal/compose/policy_test.go`

**Interfaces:**
- Consumes: `Model`, `Service` from Task 1.
- Produces:

```go
// internal/core/limits.go
type Limits struct {
	MilliCPUs int64 // thousandths of a CPU; 0 = none
	Memory    int64 // bytes; 0 = none
	Pids      int64 // processes; 0 = none
}
func (l Limits) Or(fallback Limits) Limits // each zero field takes fallback's

// internal/compose/policy.go
type Policy struct {
	Strict        bool        // the allow-list below
	RoutingLabels bool        // refuse traefik.* and outhaul.* labels
	WorkDir       string      // local paths must stay inside it
	Network       string      // the one external network a stack may join
	Max           core.Limits // R7's maximum; a zero field is not compared
}
type Violation struct{ Where, Option, Reason string } // `service "web"`, `ports`, why
func Check(m Model, p Policy) []Violation

// internal/config/config.go
ContainerPolicy string // "strict" or ""
func (c Config) StrictContainerPolicy() bool
```

- [ ] **Step 1: Write the failing `Limits.Or` test**

```go
func TestLimitsOr(t *testing.T) {
	got := core.Limits{MilliCPUs: 500}.Or(core.Limits{MilliCPUs: 1000, Memory: 1 << 30, Pids: 512})
	// got.MilliCPUs == 500, got.Memory == 1<<30, got.Pids == 512
}
```

Run: `go test ./internal/core/ -run LimitsOr`
Expected: FAIL; `Limits` is not defined.

- [ ] **Step 2: Implement `core.Limits` and `Or`**

`Or` returns each non-zero field of the receiver, falling back field by field.

- [ ] **Step 3: Write the failing config test**

```go
func TestContainerPolicySetting(t *testing.T) {
	// unset -> ""
	// "strict" and "STRICT" -> "strict"
	// "lenient" -> Load keeps "" and Validate names OUTHAUL_CONTAINER_POLICY
}
```

Run: `go test ./internal/config/ -run ContainerPolicy`
Expected: FAIL; the setting is not parsed.

- [ ] **Step 4: Implement the setting**

Add `ContainerPolicy` to `Config`, `p.policy("OUTHAUL_CONTAINER_POLICY")` to `Load`, and `StrictContainerPolicy()`. On any value but `strict` (case-insensitive), call `p.reject(key, v, "strict")` and keep the default. Add the README row: default “unset (off)”, purpose “refuse Compose files that reach the host or another app”.

- [ ] **Step 5: Write the failing service allow-list tests**

Create one fixture pair per refused option under `internal/compose/testdata/policy/`, for example `privileged.yml` and `privileged.json`, `ports.yml` and `ports.json`, `host-path.yml` and `host-path.json`. The `.json` is the recorded output of:

```sh
docker compose -p policy -f <name>.yml config --format json --no-env-resolution
```

In `policy_test.go`, a table names each JSON file, the policy and the expected violations. Cover at least:

- refused service keys: `privileged`, `cap_add`, `devices`, `cgroup_parent`, `ports`, `uts`, `userns_mode`, `device_cgroup_rules`, `gpus`, `runtime`, `use_api_socket`, `provider`, `pre_start`, `container_name`, `oom_kill_disable`, `mac_address`, `external_links`, `annotations`, `cpu_rt_runtime`, `cpu_rt_period`, `cpu_count`, `cpu_percent`, `credential_spec`, `isolation`, `models`, and an unknown `x-key`;
- allowed service keys as written: `cap_drop`, `command`, `cpus`, `depends_on`, `environment`, `expose`, `healthcheck`, `image`, `mem_limit`, `pids_limit`, `read_only`, `restart`, `user`, `working_dir`, and the rest of the spec's allow-list;
- conditional keys: `volumes` with a named or anonymous volume and with a `tmpfs` passes; a bind, `image`, `npipe` or `cluster` volume is refused. `volumes_from` a service passes; `container:…` is refused. `network_mode` `none` and `service:db` pass; `host` and `container:…` are refused. `pid`, `ipc`, `cgroup`, `security_opt`, `networks`, `secrets`, `configs`, `post_start`, `pre_stop`, `deploy` and `oom_score_adj` follow the spec's tables.

Assert `Where == "service \"web\""` and `Option` names the key. Assert every violation has a non-empty `Reason`.

Run: `go test ./internal/compose/ -run Check`
Expected: FAIL; `Policy`, `Violation` and `Check` are not defined.

- [ ] **Step 6: Implement the service half of `Check`**

`Check` runs the allow-list only when `p.Strict`; the label rule below is separate and can run with strict off. When strict, iterate `m.Services`, which is sorted. For each service, check keys in a stable order (iterate the service's `Raw` keys sorted) against three sets: allowed as written, allowed with a check, refused by name. Any other key is `Reason: "this key is not in the server's Compose allow-list."`. For conditional keys decode only the needed raw value; do not marshal the whole service.

Return violations sorted by `Where`, then the order the keys were checked. `Where` is `service "<name>"`; `Option` is the key path; `Reason` is a short sentence that says what to write instead where the spec gives one. Pin the port reason from the spec: `publishing a host port is not allowed. Give the service a domain; services reach each other on the stack's network.`

- [ ] **Step 7: Run the tests to verify they pass**

Run: `go test ./internal/core/ ./internal/config/ ./internal/compose/ -run 'LimitsOr|ContainerPolicy|Check'`
Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add internal/core/limits.go internal/core/limits_test.go internal/compose/policy.go internal/compose/policy_test.go internal/compose/testdata/policy internal/config README.md
git commit -m "feat(compose): refuse the Compose keys that reach the host"
```

---

## Task 3: Top-level sections, build, paths and labels

**Files:**
- Modify: `internal/compose/policy.go`, `internal/compose/policy_test.go`
- Create: more `internal/compose/testdata/policy/*.yml` and `*.json`
- Test: `internal/compose/policy_test.go`

**Interfaces:**
- Consumes: `Policy`, `Violation`, `Check` from Task 2.
- Produces: no new exported names. `Check` now covers the whole model.

- [ ] **Step 1: Write the failing top-level and build tests**

Add fixtures and table rows for:

- top-level sections: `name`, `services`, `volumes`, `networks`, `secrets`, `configs` pass; `x-anything` and every other section is refused;
- `volumes.<v>`: default `name`, `labels`, `driver: local` pass; `driver_opts`, another driver, `external`, another `name` are refused;
- `networks.<n>`: default `name`, `labels`, `internal`, `attachable`, `enable_ipv4`, `enable_ipv6`, `driver: bridge`, an empty `ipam` pass; `driver_opts`, another driver, `ipam` with a subnet or driver, another `name` are refused; `external: true` passes only when `name == Policy.Network`;
- `secrets.<s>` and `configs.<c>`: `environment`, `content`, `labels`, the default `name` pass; `file`, `external`, `driver`, `driver_opts`, `template_driver` are refused;
- `build`: `context` inside the checkout, a Git/HTTP URL, `docker-image://`, a named target or service pass; a path outside the checkout and `oci-layout://` are refused; `dockerfile` inside the checkout passes, one that climbs out is refused; `network: default` and `none` pass, `host` and anything else are refused; build `secrets` with `environment` pass, `file` is refused; `args`, `dockerfile_inline`, `extra_hosts`, `labels`, `no_cache`, `platforms`, `pull`, `shm_size`, `target` and `ulimits` pass as written; `tags` and a service `image` under `outhaul/` are refused; `privileged`, `entitlements`, `ssh`, `cache_from`, `cache_to`, `isolation` and unknown keys are refused;
- paths: `env_file`, `label_file`, `build.context` and local `additional_contexts` must be inside `WorkDir` after `filepath.Clean`; a path inside `WorkDir` as given passes when `WorkDir` is reached through a symlink and Compose prints the resolved path; `../` outside fails; a symlink inside the checkout pointing out is not followed, so the path stays as printed;
- labels: `traefik.http.routers.evil.rule` and `outhaul.app` are refused in any case when `RoutingLabels` is true; the same file passes when it is false; a label that arrived through `label_file` is refused (Compose merges it into `labels`, as the fixture records).

For fixtures that contain local paths, the recorded `.json` uses `/work` as the checkout root: rewrite whatever absolute path Compose printed while recording. The pure test sets `Policy.WorkDir` to `/work`. The real-Compose test below runs the `.yml` in the fixture directory and compares the violation set, not the absolute paths.

Run: `go test ./internal/compose/ -run Check`
Expected: FAIL; the new cases report no violations.

- [ ] **Step 2: Implement the rest of `Check`**

When `p.Strict`, walk `m.Raw` in sorted key order for the top-level allow-list. For each section decode its entries and check the spec's tables. For paths, use a helper:

```go
func inside(workDir, p string) bool // text-clean first; also try EvalSymlinks(workDir)
```

If `EvalSymlinks` fails, fall back to the text comparison. Paths in `build.dockerfile` stay relative to the build context and must not climb out. When `p.RoutingLabels`, labels are read from `Service.Labels`, which already contains `label_file` contents.

- [ ] **Step 3: Run the table against the real `docker compose`**

Add `TestCheckAgainstRealCompose` in `policy_test.go`:

```go
func TestCheckAgainstRealCompose(t *testing.T) {
	if !composeAvailable(t) {
		t.Skip("docker compose is not installed")
	}
	for _, tc := range policyCases {
		// run: docker compose -p policy -f testdata/policy/<name>.yml config --format json --no-env-resolution
		// ParseModel, Check with the same policy, compare violations to tc.want
	}
}
```

`composeAvailable` runs `docker compose version` and skips on error. The command runs in `testdata/policy` so relative paths resolve as they did when recorded. Do not start a daemon.

Run: `go test ./internal/compose/ -run CheckAgainstRealCompose`
Expected: PASS on a host with Compose 2.35 or newer; SKIP when it is missing. If a case fails, the allow-list and the installed Compose disagree; fix the allow-list, not the test.

- [ ] **Step 4: Run everything**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/compose
git commit -m "feat(compose): check top-level sections, build paths and routing labels"
```

---

## Task 4: The Compose pipeline reads and checks the model

**Files:**
- Modify: `internal/deploy/pipeline_compose.go:25-126`, `internal/deploy/worker.go:53-74`, `internal/deploy/deploy_compose_test.go`
- Test: `internal/deploy/deploy_compose_test.go`, `internal/deploy/deploy_test.go`

**Interfaces:**
- Consumes: `compose.ReadModel`, `compose.Check`, `compose.Policy` from Tasks 1–3; `Config.StrictContainerPolicy` and `Config.HeaderRequired` from Task 2 and the proxy plan.
- Produces:

```go
// internal/deploy/worker.go
// needsModel reports whether any setting needs the normalised Compose model.
// R7 adds the app's effective limit; R10 adds its restart setting.
func (w *Worker) needsModel(app core.App) bool

// internal/deploy/pipeline_compose.go
func policyViolationReason(vs []compose.Violation) (reason string, lines []string)
```

- [ ] **Step 1: Write the failing pipeline tests**

In `deploy_compose_test.go`:

```go
func TestComposePipelineWithoutPolicyNeverReadsModel(t *testing.T) {
	// composeApp; runPipeline; h.compose.CallsFor("config") is empty
}

func TestComposePipelineRefusesAHostPathBeforeBuilding(t *testing.T) {
	// cfg.ContainerPolicy = "strict"; ConfigJSON holds a service with
	// volumes: [{type: bind, source: /srv/data, target: /data}]
	// want: status failed, reason names service "web" and "volumes",
	// no Build and no Up calls, log has the full violation
}

func TestComposePipelineLogsEveryViolationAndKeepsTheJSONOut(t *testing.T) {
	// ConfigJSON holds environment: {TOKEN: "super-secret"} plus two violations
	// want: both lines in the log, "super-secret" absent from the log and reason
}

func TestComposePipelineChecksLabelsWhenAHeaderIsRequired(t *testing.T) {
	// cfg.RequireHeaderName/Value set; strict off; ConfigJSON has a traefik.* label
	// want: refused before Build
}

func TestComposePipelineReportsAFileComposeCannotLoad(t *testing.T) {
	// h.compose.FailConfig = errors.New("services.web.ports must be a list")
	// want: failed building with that message, no Build and no Up
}
```

Run: `go test ./internal/deploy/ -run 'ComposePipeline.*(Model|Refuses|Logs|Labels|CannotLoad)'`
Expected: FAIL; `ConfigJSON` is unused and the pipeline never calls `Config`.

- [ ] **Step 2: Implement the read and check**

In `runComposePipeline`, after `.env` and before the override:

```go
project := compose.ProjectName(app.Name)
var model compose.Model
if w.needsModel(app) {
	model, err = compose.ReadModel(ctx, w.compose, workDir, []string{app.ComposePath}, project, out)
	if err != nil {
		w.fail(dep, core.StatusBuilding, "read Compose file: "+err.Error(), out)
		return
	}
	policy := compose.Policy{
		Strict:        w.cfg.StrictContainerPolicy(),
		RoutingLabels: w.cfg.StrictContainerPolicy() || w.cfg.HeaderRequired(),
		WorkDir:       workDir,
		Network:       w.cfg.Network,
	}
	if vs := compose.Check(model, policy); len(vs) > 0 {
		reason, lines := policyViolationReason(vs)
		if w.cfg.StrictContainerPolicy() {
			logf(out, "This server's container policy refuses this Compose file (OUTHAUL_CONTAINER_POLICY=strict):")
		} else {
			logf(out, "This server refuses a Compose file that adds its own routing while a request header is required:")
		}
		for _, line := range lines {
			logf(out, "%s", line)
		}
		w.fail(dep, core.StatusBuilding, reason, out)
		return
	}
}
```

Move `project := compose.ProjectName(app.Name)` above this block; do not compute it twice. `needsModel` returns `w.cfg.StrictContainerPolicy() || w.cfg.HeaderRequired()`.

`policyViolationReason` formats each violation as `  service "web": ports: <reason>`, returns the first without the leading two spaces, and appends ` (and N more)` when there are more. Keep all text free of the model's values except the path in a violation, which is the point of the message.

- [ ] **Step 3: Run the tests to verify they pass**

Run: `go test ./internal/deploy/ -run 'ComposePipeline'`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add internal/deploy
git commit -m "feat(deploy): refuse a Compose file before building it"
```

---

## Task 5: Images that carry routing labels

**Files:**
- Modify: `internal/compose/compose.go:28-46,102-120`, `internal/compose/fake.go:23-34`, `internal/deploy/pipeline.go:78-120,315-329`, `internal/deploy/pipeline_compose.go:80-107`, `internal/deploy/fork_build_test.go`
- Test: `internal/deploy/deploy_test.go`, `internal/deploy/deploy_compose_test.go`, `internal/deploy/fork_build_test.go`

**Interfaces:**
- Consumes: `Config.StrictContainerPolicy`, `Config.HeaderRequired`, and the model from Task 4.
- Produces:

```go
// internal/compose/compose.go
// Pull runs `docker compose ... pull --policy missing --ignore-buildable`.
Pull(ctx context.Context, dir string, files []string, project string, out io.Writer) error
// Fake gains FailPull error; Pull appends Call{Verb: "pull"} to Calls.

// internal/deploy/pipeline.go
func (w *Worker) refuseRoutingLabels(ctx context.Context, image, why string) error
```

- [ ] **Step 1: Write the failing single-container tests**

```go
func TestStrictPolicyRefusesAnImageWithTraefikLabels(t *testing.T) {
	// cfg.ContainerPolicy = "strict"
	// h.docker.ImageLabelsByRef["outhaul/web:1"] = {"traefik.enable": "true"}
	// run a normal build; want: failed building, reason contains "traefik",
	// no canonical container created
}

func TestStrictPolicyRefusesARollbackImageWithTraefikLabels(t *testing.T) {
	// dep.Image is pre-set (rollback); strict on; same labels
	// want: failed before the building -> deploying transition
}

func TestForkPreviewMessageStillNamesTheFork(t *testing.T) {
	// existing fork test, updated to the neutral image-label message
}
```

Run: `go test ./internal/deploy/ -run 'StrictPolicyRefuses|ForkPreviewMessage'`
Expected: FAIL; strict mode does not inspect single-container images.

- [ ] **Step 2: Implement the single-container case**

Change `refuseRoutingLabels` to take a `why` clause and return:

```go
fmt.Errorf("refusing an image that carries traefik labels %s: %s", why, strings.Join(keys, ", "))
```

Call it when `app.IsForkPreview()` (why `"in a fork preview"`), when `w.cfg.HeaderRequired()` (why `"while a request header is required"`), or when `w.cfg.StrictContainerPolicy()` (why `"under this server's strict container policy"`). For a deployment that arrived with an image, call it once before the `building -> deploying` transition. For a built image, call it after the build. Update the fork test's expected text to `traefik labels`.

- [ ] **Step 3: Write the failing Compose image tests**

```go
func TestComposePolicyPullsAndRefusesATraefikImage(t *testing.T) {
	// strict on; ConfigJSON has services web (image nginx:1) and db
	// h.docker.ImageLabelsByRef["nginx:1"] = {"traefik.http.routers.evil.rule": "..."}
	// want: one "pull" call, then failed building naming "web" and "nginx:1",
	// no "up"
}

func TestComposePolicyRefusesAnImageThatIsNotOnTheServer(t *testing.T) {
	// ImageLabels returns an empty map for a missing ref in the fake;
	// configure FakeConfigJSON with an image whose labels map is absent and
	// use a docker fake that reports a not-found error
}

func TestComposePolicyReportsAPullFailure(t *testing.T) {
	// h.compose.FailPull = errors.New("registry unreachable")
	// want: failed building with that text; no image inspection
}
```

The fake's `ImageLabels` returns `nil, nil` for an unknown ref today; add a `FailImageLabels func(ref string) error` to `docker.Fake` so the missing-image case can return `errors.New("no such image")`. Do not infer missing from a nil map: a real image with no labels is valid.

Run: `go test ./internal/deploy/ -run 'ComposePolicy.*(Traefik|Missing|Pull)'`
Expected: FAIL; the Compose pipeline never pulls or inspects.

- [ ] **Step 4: Implement `Runner.Pull` and the Compose image check**

`Docker.Pull` runs `pull --policy missing --ignore-buildable` through `run`. `Fake.Pull` records `Call{Verb: "pull"}` and honours `FailPull`.

In `runComposePipeline`, after `compose.Build` and before the `building -> deploying` transition, when strict or a header is required:

1. `w.compose.Pull(ctx, workDir, files, project, out)`; on error fail in `building`.
2. For each service in `model.Services`, `labels, err := w.docker.ImageLabels(ctx, svc.Image)`; on error fail naming the service and image.
3. If any lowercased key starts with `traefik.`, fail naming the service, the image and the sorted label keys.

A service with `pull_policy: always` can change its tag at `up`; the message says the check is at pull time. Do not check images when neither strict nor a header is required.

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/deploy/ ./internal/compose/`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/compose internal/deploy
git commit -m "feat(deploy): refuse images that route themselves"
```

---

## Task 6: The reduced `docker compose` environment

**Files:**
- Create: `internal/compose/env.go`, `internal/compose/env_test.go`
- Modify: `internal/compose/compose.go`, `main.go`, `main_test.go`
- Test: `internal/compose/env_test.go`, `main_test.go`

**Interfaces:**
- Produces:

```go
// internal/compose/env.go
// RestrictedEnv keeps only the variables a Compose run needs, so a Compose
// file's ${NAME} cannot read Outhaul's own settings or secrets.
func RestrictedEnv(environ []string) []string

// internal/compose/compose.go
type Docker struct {
	Bin string
	Env []string // nil = inherit, as today
}
```

- [ ] **Step 1: Write the failing tests**

```go
func TestRestrictedEnv(t *testing.T) {
	// keeps PATH, HOME, USER, LOGNAME, LANG, LC_ALL, TMPDIR, XDG_DATA_HOME,
	// DOCKER_HOST, BUILDKIT_PROGRESS, BUILDX_DEBUG, COMPOSE_PROJECT_NAME,
	// SSL_CERT_FILE, SSL_CERT_DIR, HTTP_PROXY, https_proxy, NO_PROXY
	// drops OUTHAUL_REQUIRE_HEADER_VALUE, OUTHAUL_ACME_DNS_ENV and AZURE_CLIENT_SECRET
}

func TestDockerRunUsesEnvAndSetsPWD(t *testing.T) {
	// a temp "docker" script prints $PATH and $OUTHAUL_REQUIRE_HEADER_VALUE
	// and $PWD; Docker{Bin: script, Env: RestrictedEnv(os.Environ())}
	// want: PATH present, OUTHAUL_* empty, PWD == dir
}
```

Run: `go test ./internal/compose/ -run 'RestrictedEnv|DockerRunUsesEnv'`
Expected: FAIL; `RestrictedEnv` and `Docker.Env` are not defined.

- [ ] **Step 2: Implement the environment**

`RestrictedEnv` filters a `[]string` of `NAME=value`. Keep exact names and prefixes from the spec: `PATH`, `HOME`, `USER`, `LOGNAME`, `LANG`, `TMPDIR`, `SSL_CERT_FILE`, `SSL_CERT_DIR`, `HTTP_PROXY`, `HTTPS_PROXY`, `NO_PROXY`; prefixes `LC_`, `XDG_`, `DOCKER_`, `BUILDKIT_`, `BUILDX_`, `COMPOSE_`; proxy names in either case. Drop everything else.

Both `Docker.run` and the `output` helper used by `Config` set `cmd.Env = d.Env` when non-nil, and append `PWD=<dir>` because Go only adds `PWD` when it passes the whole environment on. Do not set `PWD` when `dir` is empty.

- [ ] **Step 3: Wire it in `main.go`**

Build the environment once:

```go
var composeEnv []string
if cfg.StrictContainerPolicy() {
	composeEnv = compose.RestrictedEnv(os.Environ())
}
```

Assign it to all three `compose.NewDocker()` runners: the deploy worker, the preview manager's `previewDocker`, and the HTTP server. A small helper `newComposeRunner(env []string) *compose.Docker` avoids repeating the assignment. With strict off, `composeEnv` is nil and every runner inherits as today.

- [ ] **Step 4: Add the `main` test**

`TestStrictPolicyReducesTheComposeEnvironment` calls the helper twice, with and without strict, and asserts the runner's `Env` is nil or contains `PATH` and not `OUTHAUL_*`. Keep `main_test.go` free of a real Compose run.

- [ ] **Step 5: Run everything**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/compose main.go main_test.go
git commit -m "feat(compose): hide Outhaul's environment from strict Compose files"
```

---

## Task 7: Managed databases publish no port

**Files:**
- Modify: `internal/dbaas/manager.go:92-184`, `internal/server/databases.go:63-130,233-253`, `internal/server/projects.go:108-177`, `internal/server/templates/database.tmpl`, `internal/server/templates/project.tmpl`, `main.go:232-315`
- Test: `internal/dbaas/manager_test.go`, `internal/server/databases_test.go`, `internal/server/server_test.go`, `main_test.go`

**Interfaces:**
- Produces:

```go
// internal/dbaas/manager.go
func (m *Manager) SetPolicy(strict bool)

// internal/server/server.go (setter beside SetHTTPSPort)
func (s *Server) SetContainerPolicy(strict bool)
```

- [ ] **Step 1: Write the failing database tests**

```go
func TestStrictPolicyOmitsADatabasePort(t *testing.T) {
	// m.SetPolicy(true); provision a database with ExtPort 5433
	// want: h.docker.Created[0].Ports is empty
}

func TestStrictPolicyRefusesAPortFromTheForms(t *testing.T) {
	// e.srv.SetContainerPolicy(true)
	// POST /projects/1/databases with external_port=5433
	// want: 400, body says "container policy" and "published port"
	// POST /databases/{id}/settings with external_port=5433: same
}

func TestStrictPolicyHidesThePortField(t *testing.T) {
	// renderDatabase and renderProject with CanChange true
	// want: no name="external_port" in either page
}

func TestStrictPolicyOffersToStopAnExistingPort(t *testing.T) {
	// seed a database with ExtPort 5433, strict on
	// want: page contains "Stop publishing port 5433" and a form posting
	// external_port= (empty) to /databases/{id}/settings
}
```

Run: `go test ./internal/dbaas/ ./internal/server/ -run StrictPolicy`
Expected: FAIL; there is no policy setter.

- [ ] **Step 2: Implement the manager and server policy**

Add `strict bool` to `dbaas.Manager` and `SetPolicy`. In `provision`, only add `spec.Ports` when `d.ExtPort > 0 && !m.strict`. Keep the replacement path identical: it copies the same spec.

Add `strictPolicy bool` to `Server` and `SetContainerPolicy`. In `handleCreateDatabase` and `handleDatabaseSettings`, after parsing the port, if strict and `extPort > 0`, re-render with `400` and the message `This server's container policy doesn't allow published ports. Leave the field empty.` Do not change other fields. In `renderDatabase`, add `"StrictPolicy": s.strictPolicy`; in `renderProject`, add `"StrictPolicy": s.strictPolicy` to the data map. In `database.tmpl`, hide the external-port field when `StrictPolicy`; when `Database.ExtPort > 0`, show the notice and the one-button form posting an empty `external_port`. In `project.tmpl`'s create dialog, hide the field when `StrictPolicy`.

- [ ] **Step 3: Wire `main.go` and the startup notice**

After `dbm := dbaas.NewManager(...)`, call `dbm.SetPolicy(cfg.StrictContainerPolicy())`. After `srv := server.New(...)`, call `srv.SetContainerPolicy(cfg.StrictContainerPolicy())`.

Before the worker starts, when strict, list every database and log one line per row with `ExtPort > 0`:

```go
if cfg.StrictContainerPolicy() {
	if dbs, err := st.ListDatabases(context.Background()); err == nil {
		for _, d := range dbs {
			if d.ExtPort > 0 {
				log.Printf("WARNING: database %q publishes port %d, which OUTHAUL_CONTAINER_POLICY=strict does not allow; open its page and stop publishing it", d.Name, d.ExtPort)
			}
		}
	}
}
```

Do not restart or reprovision anything at startup. Add a `main_test.go` test around a small helper `warnPublishedDatabasePorts(st, strict)` that captures the log output, or test the rendered pages and provision path only if the startup line is already covered by an existing startup test pattern.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/dbaas/ ./internal/server/ . -run 'StrictPolicy|PublishedDatabase'`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/dbaas internal/server main.go main_test.go
git commit -m "feat(dbaas): publish no database port under the strict policy"
```

---

## Task 8: Every one-click template passes strict `Check`

**Files:**
- Create: `internal/catalog/policy_test.go`

**Interfaces:**
- Consumes: `catalog.All()`; `compose.Docker.Config`, `compose.ParseModel`, `compose.Check`; `compose.ProjectName`.
- Produces: no new production names.

- [ ] **Step 1: Write the guard test**

```go
func TestEveryTemplatePassesTheStrictPolicy(t *testing.T) {
	if !composeAvailable(t) {
		t.Skip("docker compose is not installed")
	}
	templates, err := catalog.All()
	// for each: write Template.Compose to t.TempDir()/docker-compose.yml,
	// run compose.Docker{}.Config with project "check-"+id, parse the model,
	// Check with Policy{Strict: true, WorkDir: dir, Network: "outhaul"}
	// want: zero violations
}
```

The raw template text contains `${domain}` and generated secrets; `--no-env-resolution` and an unset environment leave them empty, which is fine because the policy checks keys and shapes, not values. `composeAvailable` repeats the `docker compose version` check from Task 3; keep it local to the test package.

- [ ] **Step 2: Run the test**

Run: `go test ./internal/catalog/ -run EveryTemplatePasses`
Expected: PASS, or SKIP when Compose is missing. If it fails, the allow-list is missing a key one of the nine templates uses; add the key only if the spec's table allows it, otherwise fix the template in a separate change and say why in the commit body.

- [ ] **Step 3: Commit**

```bash
git add internal/catalog/policy_test.go
git commit -m "test(catalog): every template passes the strict policy"
```

---

## Task 9: Documentation and the real-host check

**Files:**
- Create: `docs/GUARD-RAILS.md`
- Modify: `README.md`, `ARCHITECTURE.md`, `AGENTS.md`, `docs/PROTECTED-ROUTES.md`, `docs/BEHIND-A-PROXY.md`
- Test: `go build ./... && go vet ./... && go test ./...`, then `go test -race -timeout 30m ./...`

**Interfaces:**
- Consumes: everything above.
- Produces: the user-facing explanation of strict mode.

- [ ] **Step 1: Write `docs/GUARD-RAILS.md`**

Use the plain, second-person style of `docs/SSO.md`. Cover:

- what `OUTHAUL_CONTAINER_POLICY=strict` is for, and that roles are not containment;
- a table of what a Compose file may not contain, with what to write instead (`ports` -> a domain, a host path -> a named volume, `privileged`/`cap_add`/`devices` -> no substitute, `network_mode: host` -> the stack network, `file` secrets -> `environment` or `content`);
- the `traefik.*`/`outhaul.*` label rule and why an image's own labels matter;
- where strict mode applies: Compose deploys, database ports, single-container image checks; not Traefik/cloudflared, not builds;
- that reading a Compose file needs Docker Compose 2.35 or newer, and what the error says on an older host;
- the real-host checks below, marked as not yet run.

- [ ] **Step 2: Update the other documents**

- `README.md`: the `OUTHAUL_CONTAINER_POLICY` row (added in Task 2); check the wording matches the parser.
- `ARCHITECTURE.md`: add a “Container guard rails (opt-in)” section with the model reader, the allow-list, the label and image checks, and that the earlier non-goal “Restricting Compose files” no longer holds in strict mode. Add `compose/model.go` and `compose/policy.go` to the package layout.
- `AGENTS.md`, “Decisions to keep”: strict mode is an allow-list, so a new Compose key is refused until someone lists it; the model is never logged; a refusal names the service and the option; database ports are refused under strict mode.
- `docs/PROTECTED-ROUTES.md` and `docs/BEHIND-A-PROXY.md`: one line each that a Compose file's own `traefik.*` labels are refused while a header is required, and in strict mode.

- [ ] **Step 3: Run the full check**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: PASS.

Run: `go test -race -timeout 30m ./...`
Expected: PASS. `internal/server` takes longer than the default timeout under `-race`; that is why the timeout is explicit.

- [ ] **Step 4: Run the real-Compose tests on a host with Compose 2.35+**

Run: `go test ./internal/compose/ ./internal/catalog/ -run 'CheckAgainstRealCompose|EveryTemplatePasses' -v`
Expected: PASS, not SKIP, on the staging host.

- [ ] **Step 5: Leave the real-host checklist for the staging host**

The following cannot be proved by the unit tests and must be run where Docker and Compose are real:

1. A Compose app with `privileged: true`, with a host path, and with `ports:` is each refused, and the message names the service and the option.
2. A Compose file with a `traefik.http.routers…` label is refused.
3. A database can't be given an external port, and `ss -ltn` shows none.
4. `docker compose version` on the host is 2.35 or newer.
5. A Compose file that reads `${OUTHAUL_REQUIRE_HEADER_VALUE}` gets nothing when strict mode reduces the environment.

Record the results in `docs/GUARD-RAILS.md` under “Testing so far” when they have been run.

- [ ] **Step 6: Commit**

```bash
git add docs README.md ARCHITECTURE.md AGENTS.md
git commit -m "docs(guard-rails): what strict mode refuses and what to write instead"
```
