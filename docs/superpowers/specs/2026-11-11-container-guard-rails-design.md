# Container guard rails and resource limits

A strict container policy, default limits on every container, and a cap on
concurrent builds.

This covers requirements R6 and R7 of the brief "outhaul enhancements: running
behind Azure Front Door on a shared host" (10 November 2026). It builds on
`2026-11-11-proxy-ingress-design.md` (R1 to R4) and treats its settings as
decided. Code references are to `06150cc`.

R6 and R7 are separate pull requests. They meet in one place:
[Where R6 and R7 meet](#where-r6-and-r7-meet). Both read the normalised
Compose model, and so will R10 (a default `restart:`), so the reader is
described first, on its own.

## Why

Many trusted people share one server. Deploying is running code, so roles
don't keep one person's mistake away from everyone else:

- A Compose file is used as written. `privileged: true`, the Docker socket, a
  host path or `network_mode: host` hands over the server.
- A managed database can publish a host port, and `portConfig`
  (`internal/docker/real.go`) binds published ports on `0.0.0.0`.
- No container has a CPU, memory or process limit (`CreateContainer`,
  `internal/docker/real.go`). One runaway app stalls every other app.
- Up to four deployments run at once (`maxConcurrent`,
  `internal/deploy/worker.go`), and each may be building. On two CPUs, two
  builds leave nothing for the apps.

The aim is to contain mistakes and casually chosen third-party images. A
determined person escaping is accepted.

## Goals

1. With `OUTHAUL_CONTAINER_POLICY=strict`, a Compose deploy is refused before
   anything is built when its file asks for something that reaches the host or
   another app. The message names the service and the option.
2. In strict mode a managed database publishes no host port.
3. Every one-click template passes the same check, and a test keeps it so.
4. Default CPU, memory and process limits on every container Outhaul creates
   for an app or a database, and on every Compose service that sets none.
5. A system admin can set one app's limits. The app page shows the limit next
   to the live figures.
6. A cap on how many builds run at once.
7. One function reads the normalised Compose model, for the policy, the limits
   and R10.
8. Each part is off unless its setting is set. An install that sets none sees
   no change.

## Non-goals

- Containment. An image still runs any code as root in its container, on the
  network every app shares.
- Separating apps on the Docker network. Of the earlier non-goal "Restricting
  Compose files or separating apps on the Docker network"
  (`2026-11-02-sso-roles-protected-routes-design.md`), only the first half
  changes, and only in strict mode.
- A limit on a whole stack, or on how many containers a stack starts (`scale`,
  `deploy.replicas`). Limits are per container.
- Limits on build steps, on Traefik and on cloudflared.
- A limit for one database, and "no limit" for one app.
- Swap. Outhaul sets the memory limit only; where a server has swap, Docker
  lets a container use as much swap again.
- Changing a running container. A changed limit applies when the container is
  next created. Turning strict mode on checks nothing that already runs.

## Anti-goals

The work has failed, even if it runs, if any of these happen:

- An install that sets none of the new settings notices anything: another
  `docker compose` call, a changed override, a new panel, a limit.
- In strict mode a Compose file gets a host path, a published port or extra
  privileges through another spelling, or a newer key the check didn't know.
- A refusal doesn't say which service and which option.
- The normalised model, which holds every variable's value, reaches a log.
- A limit Outhaul adds makes Compose refuse a file it accepted before.
- A deployment that builds nothing waits behind someone else's build.

## Constraints

- Settings are `OUTHAUL_*` variables read through the `parser`
  (`internal/config/config.go`); `Validate` refuses bad values.
- No YAML dependency (`ARCHITECTURE.md`, "Config"). The model is the JSON
  that `docker compose config` prints, read with `encoding/json`.
- The user's Compose file is never rewritten. Outhaul layers its override
  over it (`internal/compose/override.go`).
- New routes state an access level in `routes()`. Migrations are new files.
- Fork previews stay Nixpacks-only (`AGENTS.md`, "Decisions to keep").
- Test first, with the existing fakes. What depends on how Docker or Compose
  behaves is checked on the real thing.

## Settings

| Variable | Values | Default | Needs |
|---|---|---|---|
| `OUTHAUL_CONTAINER_POLICY` | `strict` | unset: as today | Compose 2.35 or newer, for Compose apps |
| `OUTHAUL_DEFAULT_CPUS` | CPUs: `1`, `0.5`, `1.5` | unset: no CPU limit | no more than the server's CPUs; Compose 2.35 |
| `OUTHAUL_DEFAULT_MEMORY` | `512m`, `1g`, `1.5g`, or bytes | unset: no memory limit | at least `6m`; Compose 2.35 |
| `OUTHAUL_DEFAULT_PIDS` | a whole number, 1 or more | unset: no process limit | Compose 2.35 |
| `OUTHAUL_MAX_CONCURRENT_BUILDS` | a whole number, 1 to 4 | unset: up to four, as today | |

Each works without the others. The first deployment sets `strict`, `1`, `1g`,
`512` and `1`.

New `parser` methods refuse, naming the variable and what it wants:

- Policy: anything but `strict`, in any case.
- CPUs: anything but digits with up to three decimals, 0.01 or more (Docker's
  minimum). So `50%`, `500m`, `1,5`, `.5` and `1e2` are refused.
- Memory: anything but digits, an optional decimal part, and an optional `k`,
  `m` or `g` in either case, with or without a `b`. Units are powers of 1024.
  Refused: `0`, less than 6 MiB (Docker's minimum), `1t`, `1 g`, `1gib`.
- Processes and builds: anything but a whole number in range. 4 is today's
  `maxConcurrent`, which stays the number of pipelines.

`serve` refuses to start when `OUTHAUL_DEFAULT_CPUS` is more than
`runtime.NumCPU()` and `OUTHAUL_DOCKER_HOST` is unset: Docker refuses a
container with more CPUs than the server has, so every deploy would fail.

The value rules are `core.ParseCPUs` and `core.ParseMemory`, so the per-app
form uses them too. `config` imports `core` for this. It is its first import
of another Outhaul package; `core` imports none.

## Reading the Compose model

Today `runComposePipeline` (`internal/deploy/pipeline_compose.go`) writes
`.env`, writes the override when the app has domains, and runs `compose build`
then `compose up`. Outhaul never looks inside the file.

`compose.Runner` gains `Config`, and `internal/compose/model.go` (new) reads
what it returns:

```go
// Config runs `docker compose -p <project> -f <files...> config --format json
// --no-env-resolution` in dir and returns what it prints. Compose's warnings
// go to warn (the deploy log).
Config(ctx context.Context, dir string, files []string, project string, warn io.Writer) ([]byte, error)

func ReadModel(ctx context.Context, r Runner, dir string, files []string, project string, warn io.Writer) (Model, error)
func ParseModel(b []byte) (Model, error) // pure; accepts keys it doesn't know

type Model struct {
	Project  string
	Services []Service // the services `up` would start, sorted by name
	// and the top-level sections, each entry as raw JSON
}

type Service struct {
	Name                string
	Image               string // its `image`; <project>-<name> when it only builds
	Restart             string // its own `restart`; "" when it has none
	DeployRestartPolicy bool   // it sets deploy.restart_policy
	Labels              map[string]string
	Raw                 map[string]json.RawMessage // every key, as printed
}

// HasRestart reports whether the service says how it restarts. Compose
// applies deploy.restart_policy outside Swarm, and it wins over `restart`.
func (s Service) HasRestart() bool
```

R10 takes the `Model` the pipeline read and gives its restart value to each
service where `HasRestart()` is false. `compose.Fake` gains `ConfigJSON` and
`FailConfig`, and records the call.

`runComposePipeline` becomes:

1. As today: the environment, the domains, the work directory, `.env`.
2. `ReadModel` on the app's own Compose file, without Outhaul's override.
3. `Check` (R6). A violation fails the deployment in `building`.
4. The override, from the domains and the model (R7's limits, R10's restart).
5. `compose build`.
6. The image check (R6), still in `building`.
7. `compose up`, as today.

- **Only when needed.** Step 2 runs when the policy is strict, a request
  header is required, the app has a limit, or R10's setting is on. Otherwise
  `config` is never called. Steps 3 and 6 say below when they apply.
- **The same file `up` loads**: the same directory, `.env` and project name.
- **`--no-env-resolution`** keeps each `env_file` path in the model and those
  files' values out of it. Compose's 2.35.0 release notes list the flag. When
  Compose answers "unknown flag", the deploy fails saying that reading a
  Compose file needs Docker Compose 2.35 or newer.
- **Secrets.** The JSON holds every interpolated value. It is parsed in
  memory, and never logged, stored or quoted in an error.
- **A file Compose can't load** fails the deploy with Compose's message, one
  step earlier than today.

What normalising does, so `Check` sees one shape (Compose v5.6.0):

| Written | In the model |
|---|---|
| `ports: ["80"]`, `"8080:80"` | objects with `target` and `protocol`, and `published` and `host_ip` when given |
| `./data:/data`, `/etc:/x:ro` | `{"type":"bind","source":"<absolute path>",…}` |
| `named:/data`, `/anon` | `{"type":"volume","source":"named",…}`; no `source` when anonymous |
| labels as a list, and `label_file` | one `labels` map; `label_file` keeps its absolute paths |
| `mem_limit: 512m`, `cpus: '0.5'` | `"536870912"` (a string of bytes), `0.5` |
| `mem_limit: 0`, `cap_add: []`, `privileged: false` | left out |
| `extends`, `include`, `${VAR}` | resolved; what they bring in is in the model and is checked |
| a service in a profile that isn't active | left out, as `up` leaves it out |
| a volume or network with no `name` | `name` is `<project>_<key>` |

## R6: the strict container policy

### What a Compose file may contain

`internal/compose/policy.go` (new), pure:

```go
type Policy struct {
	Strict        bool        // the allow-list below
	RoutingLabels bool        // refuse traefik.* and outhaul.* labels
	WorkDir       string      // local paths must stay inside it
	Network       string      // the one external network a stack may join
	Max           core.Limits // R7's maximum; a zero field isn't compared
}
type Violation struct{ Where, Option, Reason string } // `service "web"`, `ports`, why

func Check(m Model, p Policy) []Violation
```

Strict mode works from a list of what is allowed: **a key that isn't listed is
refused.** The Compose specification grows. Its schema already has keys the
brief's list doesn't know: a `pre_start` hook that takes a whole container
specification, `provider`, `use_api_socket`, `privileged` on `post_start`.

Service keys allowed as written: `attach`, `blkio_config`, `cap_drop`,
`command`, `cpu_period`, `cpu_quota`, `cpu_shares`, `cpus`, `cpuset`,
`depends_on`, `develop`, `dns`, `dns_opt`, `dns_search`, `domainname`,
`entrypoint`, `environment`, `expose`, `extra_hosts`, `group_add`,
`healthcheck`, `hostname`, `image`, `init`, `links`, `logging`, `mem_limit`,
`mem_reservation`, `mem_swappiness`, `memswap_limit`, `pids_limit`,
`platform`, `profiles`, `pull_policy`, `read_only`, `restart`, `scale`,
`shm_size`, `stdin_open`, `stop_grace_period`, `stop_signal`, `storage_opt`,
`sysctls`, `tmpfs`, `tty`, `ulimits`, `user`, `working_dir`.

Service keys allowed with a check:

| Key | Allowed | Refused |
|---|---|---|
| `volumes` | `type: volume` (named or anonymous), `type: tmpfs` | `bind` (any host path, the Docker socket included), `image`, `npipe`, `cluster` |
| `volumes_from` | a service of the same stack | `container:…` |
| `network_mode` | `none`, `service:<name>` | `host`, `container:…`, any other value |
| `pid` | `service:<name>` | `host`, `container:…` |
| `ipc` | `private`, `shareable`, `none`, `service:<name>` | `host`, `container:…` |
| `cgroup` | `private` | `host` |
| `security_opt` | `no-new-privileges`, alone or set to `true` | everything else: `seccomp`, `apparmor`, `label`, `systempaths` |
| `networks` | a network allowed below, with `aliases` | `ipv4_address`, `ipv6_address`, `mac_address`, every other per-network key |
| `secrets`, `configs` | ones defined with `environment` or `content` | ones defined with `file` (a bind mount of that host path) or `external` |
| `post_start`, `pre_stop` | `command`, `user`, `environment`, `working_dir` | `privileged` |
| `deploy` | everything else | `resources.reservations.devices` and `.generic_resources` |
| `oom_score_adj` | 0 or more | a negative value |
| `labels`, `label_file`, `env_file`, `build` | see [Labels](#labels), [Paths](#paths) and below | |

Service keys refused by name. From the brief: `privileged`, `cap_add`,
`devices`, `cgroup_parent`, `ports`, `uts`, `userns_mode`. Beyond it:
`device_cgroup_rules`, `gpus`, `runtime`, `use_api_socket`, `provider`,
`pre_start`, `container_name`, `oom_kill_disable`, `mac_address`,
`external_links`, `annotations`, `cpu_rt_runtime`, `cpu_rt_period`,
`cpu_count`, `cpu_percent`, `credential_spec`, `isolation`, `models`. Any
other key is refused as unknown.

`build`:

| Key | Allowed | Refused |
|---|---|---|
| `context`, `additional_contexts` | a path inside the checkout, a Git or HTTP URL, `docker-image://`, a named target or service | a path outside the checkout, `oci-layout://` |
| `dockerfile` | a path that stays inside the checkout | one that leaves it |
| `network` | `default`, `none` | `host`, anything else |
| `secrets` | ones defined with `environment` | ones defined with `file` |
| `args`, `dockerfile_inline`, `extra_hosts`, `labels`, `no_cache`, `platforms`, `pull`, `shm_size`, `target`, `ulimits` | as written | |
| `tags`, and `image` on a service that builds | any name | a name under `outhaul/`, where every app's rollback images are |
| any other key | | `privileged`, `entitlements`, `ssh`, `cache_from`, `cache_to`, `isolation`, unknown keys |

Top-level sections. Anything but `name`, `services`, `volumes`, `networks`,
`secrets` and `configs` is refused.

| Section | Allowed | Refused |
|---|---|---|
| `volumes.<v>` | the default `name`, `labels`, `driver: local` | `driver_opts` (how a named volume becomes a host path), another `driver`, `external`, another `name` |
| `networks.<n>` | the default `name`, `labels`, `internal`, `attachable`, `enable_ipv4`, `enable_ipv6`, `driver: bridge`, an empty `ipam` | `driver_opts`, another `driver`, `ipam` with a subnet or a driver, another `name` |
| `networks.<n>` with `external: true` | `name` equal to `Policy.Network` (`OUTHAUL_NETWORK`) | any other, `host` among them |
| `secrets.<s>`, `configs.<c>` | `environment`, `content`, `labels`, the default `name` | `file`, `external`, `driver`, `driver_opts`, `template_driver` |

A stack may name Outhaul's shared network: a service Outhaul doesn't route may
still need a managed database. Routed services are on it already.

Refusing bind mounts takes little away. Compose resolves `./data` against the
deploy's work directory, and `runComposePipeline` removes that directory when
the deploy ends.

### Paths

Compose prints `build.context`, `env_file`, `label_file` and local
`additional_contexts` as absolute paths. Each must be inside
`Policy.WorkDir`, compared as text after `filepath.Clean`. `build.dockerfile`
stays relative to its context and must not climb out of the checkout.

`WorkDir` is what `composeWorkDir` returns: the clone's root, or a template's
directory. The data directory may itself be reached through a symlink, and
Compose may then print the resolved path. So a path passes when it is inside
`WorkDir` as given or as `filepath.EvalSymlinks` resolves it.

Symlinks in the checkout aren't followed. A link that leads out is deliberate,
not a mistake. `symlinkEscape` (`internal/deploy/forkcheck.go`) could be
reused for this later.

### Labels

A Compose file can write `traefik.*` labels on its own services. They sit
beside Outhaul's and add a router Outhaul didn't write: for any host, without
the gate, without the required header. The R1 to R4 design left this to R6.

`Policy.RoutingLabels` refuses a label whose key starts with `traefik.` or
`outhaul.`, in any case, on any service. It is on in two cases, each on its
own:

| Case | Why |
|---|---|
| The policy is strict | such a router can take another app's host |
| A request header is required (`Config.HeaderRequired`, R3) | no route may exist without the header |

The check reads the merged `labels` map, which includes what a `label_file`
adds. `deploy.labels` reach no container outside Swarm and aren't read.
`outhaul.*` is refused because Outhaul finds an app's containers, and R3's
stamp, by those labels.

### Images that carry routing labels

Docker copies an image's labels onto its containers. `refuseRoutingLabels`
(`internal/deploy/pipeline.go`) refuses a fork preview's image for that, and
R3 runs it for every single-container deploy while a header is required.

- **Single-container apps.** Strict mode is a third case in which
  `refuseRoutingLabels` runs for every deploy, a reused image included.
- **Compose services.** Nothing checks their images today. When the policy is
  strict or a header is required, step 6 above:
  1. `Runner.Pull` (new) runs `docker compose … pull --policy missing
     --ignore-buildable`, so every image `up` will use is on the server.
  2. For each service, `docker.ImageLabels(service.Image)`. A `traefik.*` key
     fails the deploy, naming the service, the image and the labels. So does
     an image that still isn't there.

  A service with `pull_policy: always` pulls again at `up`; a tag that changes
  in between isn't covered.

Both look for `traefik.*` only. Every image Outhaul builds carries
`outhaul.image` (`docker.ImageLabel`), and a label Outhaul writes on a
container replaces an image's label of the same name.

### Outhaul's own environment

`compose.Docker.run` starts `docker compose` with Outhaul's whole environment,
so a Compose file's `${NAME}` can read any variable Outhaul was started with,
`OUTHAUL_*` settings and what `OUTHAUL_ACME_DNS_ENV` names among them.

In strict mode every `docker compose` call gets only: `PATH`, `HOME`, `USER`,
`LOGNAME`, `LANG`, `LC_*`, `TMPDIR`, `XDG_*`, `DOCKER_*`, `BUILDKIT_*`,
`BUILDX_*`, `COMPOSE_*`, `SSL_CERT_FILE`, `SSL_CERT_DIR`, and `HTTP_PROXY`,
`HTTPS_PROXY`, `NO_PROXY` in either case, plus `PWD` set to the directory it
runs in (Go adds `PWD` only when it passes the whole environment on). `.env`
in the stack directory still supplies the app's variables. `compose.Docker`
gains an `Env []string` field (nil = inherit, as today); `main.go` sets it on
all three runners.

### Single-container apps

Outhaul writes these containers' specification itself (`createContainer`,
`internal/deploy/pipeline.go`): no privileges, no host path, no published
port, the app's named volumes only. A repository can't ask for more, and
`docker build` grants a Dockerfile host networking or insecure mode only with
`--allow`, which Outhaul never passes.

So strict mode changes one thing for Nixpacks and Dockerfile apps: the image
check above.

### Managed databases

Today `provision` (`internal/dbaas/manager.go`) publishes `ExtPort` on every
interface. In strict mode a database publishes no host port:

- `handleCreateDatabase` and `handleDatabaseSettings`
  (`internal/server/databases.go`) refuse a port, saying this server's
  container policy doesn't allow published ports. The forms don't show the
  field (`project.tmpl`, `database.tmpl`).
- `provision` leaves the mapping out, whatever the row says.
- A database whose row still has a port keeps publishing it until its
  container is next replaced. Its page says so and has one button, "Stop
  publishing port N", which posts an empty port to the existing settings
  route. At startup Outhaul logs one line per such database, and restarts
  none.

Preview databases never have a port (`previewmgr.provisionDatabases`).

### One-click templates

A template app is a Compose app, so it goes through the same check. All nine
templates were normalised with Compose v5.6.0 and compared with the lists
above. **None would fail.** Between them they use only `image`, `command`,
`entrypoint`, `environment`, `restart`, `depends_on`, `healthcheck`, named
volumes and the default network. Nothing needs fixing.

A test keeps it so: every template from `catalog.All()` is normalised by the
real `docker compose` and must pass strict `Check`.

### What a refusal looks like

The deployment fails in `building`, before anything is built. The build log
lists every violation. The deployment's reason is the first, with a count of
the rest. A `git push` deploy prints the same lines in the pusher's terminal.

```
This server's container policy refuses this Compose file (OUTHAUL_CONTAINER_POLICY=strict):
  service "web": ports: publishing a host port isn't allowed. Give the service a domain; services reach each other on the stack's network.
  service "db": volumes: /srv/data is a host path. Use a named volume.
  volume "cache": driver_opts isn't allowed.
```

## R7: limits

### The values

`internal/core/limits.go` (new):

```go
type Limits struct {
	MilliCPUs int64 // thousandths of a CPU; 0 = none
	Memory    int64 // bytes; 0 = none
	Pids      int64 // processes; 0 = none
}
func (l Limits) Or(fallback Limits) Limits // each zero field takes fallback's
```

`config.Config` gains `DefaultLimits core.Limits`. `docker.ContainerSpec`
gains `NanoCPUs`, `Memory` and `PidsLimit` (0 = none), which
`real.CreateContainer` puts in `HostConfig.Resources`. `docker.Fake` already
records every specification in `Created` and `Runs`.

An app's limit is `app.Limits.Or(cfg.DefaultLimits)`: its stored value where
it has one, the server default otherwise, field by field. A preview uses its
parent's stored values, read at each deploy. `Worker.limits(ctx, app)` returns
it.

### Which limit a container gets

| Container | Created in | Limit |
|---|---|---|
| A Nixpacks or Dockerfile app, and its health-check container `outhaul-deploy-<id>` | `createContainer` | the app's |
| A Compose service | the override | the app's, for each resource the service doesn't set |
| A preview | the same paths | its parent's |
| A managed database, a preview's included | `dbaas.provision` | the server default |
| A backup, restore or data-removal helper | `backup/manager.go`, `backup/restore.go`, `dbaas.removeData` | the server default |
| Traefik, cloudflared | `proxySpec`, `connectorSpec` | none |
| A build step | BuildKit, inside the Docker daemon | none; see [Build concurrency](#build-concurrency) |

During a zero-downtime cutover two containers of one app run for a moment,
each with the limit. With no default and no stored value every field is zero,
and each specification is exactly today's.

### Compose services

The override gains per-service additions built from the model:

```go
// internal/compose/override.go; rt is from the R1 to R4 plan's Task 1
func Override(app core.App, domains []core.Domain, network string, rt traefik.Routing, extra map[string]Extra) []byte

type Extra struct {
	Limits   core.Limits // only the fields to write
	InDeploy bool        // write them under deploy.resources.limits
	// R10 adds its restart value here
}

// The error is the memory reservation case below.
func LimitExtras(m Model, limit core.Limits) (map[string]Extra, error)
```

Today the override is written only when the app has domains, and names only
routed services. Now it is written when the app has domains or any service
has an addition. A service with no domain gets its additions only: no network,
no labels. With a nil map the output is today's.

A service has set a resource itself when its model has any of:

| Resource | Keys |
|---|---|
| CPU | `cpus`, `deploy.resources.limits.cpus`, `cpu_quota`, `cpu_period` |
| Memory | `mem_limit`, `deploy.resources.limits.memory` |
| Processes | `pids_limit` (`-1` too), `deploy.resources.limits.pids` |

Compose leaves a zero out of the model, so `mem_limit: 0` counts as not set.
The override writes each resource the service hasn't set and the app has a
limit for. Where it writes depends on the service: Compose refuses a service
whose own key and `deploy.resources.limits` disagree, and counts a key missing
from an existing `limits` block as disagreeing.

| The service has | The override writes |
|---|---|
| no `deploy.resources.limits` block | `cpus`, `mem_limit` (bytes), `pids_limit` |
| a `deploy.resources.limits` block | the missing keys inside that block, `memory` as a quoted string of bytes |

- `cpu_quota` and `cpu_period` count as CPU set because Docker refuses them
  together with `cpus`.
- A service that reserves more memory (`mem_reservation`,
  `deploy.resources.reservations.memory`) than the limit Outhaul would add
  fails the deploy in `building`, naming both figures. Docker would refuse the
  container at `up` with a less clear message.

### Where R6 and R7 meet

In strict mode a Compose file can't exceed the app's limits. The pipeline
passes them as `Policy.Max`. For each resource the app has a limit for:

- a `cpus`, `mem_limit` or `pids_limit` above it, in either spelling, is
  refused, naming both figures and saying a system admin can raise the app's
  limit;
- `pids_limit: -1` (unlimited) is refused;
- `cpu_quota` and `cpu_period` are refused with "set `cpus` instead", so there
  is one figure to compare.

The maximum is the app's limit. There are no separate maximum settings.
Without strict mode a service's own limits stand, whatever they are.
Whichever of R6 and R7 is built second adds this as its last commit.

### A limit for one app

- **Migration** (the next free number; 0029 at `06150cc`): `apps` gains
  `limit_cpus_milli`, `limit_memory_bytes` and `limit_pids`, each `INTEGER NOT
  NULL DEFAULT 0`. 0 means the server default. Nothing is sealed.
- **Store**: `core.App` gains `Limits`; `appCols` and `scanApp` read it;
  `SetAppLimits(ctx, id, core.Limits)` writes it. Previews store none.
- **Route**: `{"POST /apps/{id}/limits", levelSystemAdmin, nil,
  s.handleAppLimits}` in `routes()`, declared as `POST /volumes/reclaim` is.
  The handler is in `internal/server/limits.go` (new).
- **Panel**: "Limits", on the app page after Metrics. Everyone who can see the
  app sees the three values in force, and for each whether it is the server
  default or set for this app. A system admin (`IsSystemAdmin`, already in
  every page's data) also gets three fields in the settings' formats. Empty
  means the server default. The handler refuses a bad value in the parser's
  words, and more CPUs than the server has.
- A value may be above or below the default. One app can't be given "no
  limit"; a large value does that.
- A preview's page shows its parent's values and no form.
- A change applies on the app's next deploy. The panel says so.
- The panel is shown, and the route answers, only when at least one
  `OUTHAUL_DEFAULT_*` is set. Otherwise the route answers 404.

Changing a server default changes nothing that runs. An app takes it on its
next deploy, a database when its container is next replaced. Outhaul redeploys
nothing at startup for this.

### What the pages show

Today the app page's Metrics panel polls `GET /apps/{id}/stats`, and
`appStats` (`internal/server/stats.go`) shows memory "of" `MemLimit`, which is
the host's total when the container has no limit.

`real.ContainerStats` already inspects the container for its start time.
`docker.Stats` gains, from that answer and the statistics:

```go
CPULimit   float64 // HostConfig.NanoCpus/1e9, else CpuQuota/CpuPeriod; 0 = none
MemLimited bool    // HostConfig.Memory > 0: MemLimit is the container's own
Pids       uint64  // pids_stats.current
PidsLimit  uint64  // HostConfig.PidsLimit when above 0; 0 = none
```

- **App page.** CPU gains a sub-line, "limit 1 CPU". Memory reads "of 1.0 GiB"
  only when `MemLimited`. A new Processes figure reads "12 of 512". For a
  stack: the limit when every running container has the same one, "limits
  differ" when they don't, nothing when none has one. These are the live
  containers' limits, so a value not yet applied differs from the Limits
  panel.
- **Metrics page** (system admins): each row of the container table shows its
  limit beside CPU and memory, and gains Processes (`containerStats`,
  `internal/server/metrics.go`).
- A container with no limit shows what it shows today.

### Build concurrency

Today `dispatch` (`internal/deploy/worker.go`) runs up to four pipelines. Each
holds its slot from clone to health check. A deployment stays `queued` until a
slot is free and its app has nothing active (`store.NextClaimable`).

`OUTHAUL_MAX_CONCURRENT_BUILDS` caps how many deployments **that have to
build** are in `building`. A deployment has to build when it arrives with no
image: every deploy of new code, and every Compose deployment (none carries an
image, and Outhaul can't know whether `compose build` has work). A rollback,
and a protection change or R3's startup redeploy of a single-container app,
arrive with an image and never wait for a build slot.

- The worker gets a second semaphore, `buildSlots`, nil when the setting is
  unset. Then nothing below runs.
- `dispatch` takes a pipeline slot as today, tries to take a build slot
  without waiting, and calls `NextClaimable(ctx, canBuild)`.
- With `canBuild` false the query returns only a deployment that has an image
  and no older queued deployment of its own app. Nothing jumps ahead within
  one app.
- A claimed deployment that has an image gives the build slot straight back.
- The pipeline gives its build slot back when the deployment leaves
  `building`: built, failed or cancelled. It then calls `Notify`, so the next
  build is claimed at once.

A build that waits stays `queued`, in the order it was made, with Cancel as
today, and survives a restart as queued rows do. While the cap is set, the
page of a queued deployment with no image says: "Waiting its turn. This
server runs N builds at a time."

Build steps get no CPU or memory limit: they run in the Docker daemon, in no
container Outhaul creates. The cap bounds them. See call 15.

## Checked against real Docker and Compose so far

Docker 29.9.1, Compose v5.6.0, buildx v0.38.0, cgroup v2 with the systemd
driver, 11 November 2026, with throwaway files and containers:

- `docker compose config --format json` needs no Docker daemon and prints the
  shapes in the normalising table. `use_api_socket`, `provider`,
  `post_start[].privileged`, a `type: image` volume and a volume's
  `driver_opts` stay as written.
- `--no-env-resolution` keeps `env_file` as absolute paths and those files'
  values out. `${NAME}` was filled from the calling process's environment, and
  was empty when Compose was started with `PATH` and `HOME` only; `create`
  still reached the daemon.
- Run in a directory reached through a symlink, Compose printed paths under
  the symlink when `PWD` named it, and under the resolved directory when `PWD`
  was missing.
- A `file` secret or config became a bind mount of the host path; one with
  `content` or `environment` didn't. `deploy.labels` reached no container.
- `deploy.restart_policy` was applied, and won over an override's `restart`.
- `pull --policy missing --ignore-buildable` skipped a service that builds.
- `--cpus 0.5 --memory 64m --pids-limit 32` gave `cpu.max` `50000 100000`,
  `memory.max` 67108864 and `pids.max` 32; `memory.swap.max` was the same
  again. Past the memory limit the process was killed (137). Past the process
  limit a shell got "can't fork".
- The statistics answer has `memory_stats.limit` and `pids_stats`. The CPU
  limit is only in the inspect answer.
- Docker refused `--cpus 64` on 16 CPUs, a memory limit under 6 MiB, and
  `NanoCPUs` with a CPU quota or period.
- Compose refused `mem_limit` in one file with a different
  `deploy.resources.limits.memory` in the other, in either order; a base with
  only `deploy.resources.limits.cpus` and an override with `mem_limit`; and a
  bare number for `deploy.resources.limits.memory`. Keys added inside the
  base's `limits` block were accepted. Both spellings reached the containers
  as `NanoCpus`, `Memory` and `PidsLimit`.
- `docker build` refused `RUN --security=insecure` and `RUN --network=host`
  without `--allow`. `docker build --memory 64m --cpu-quota 50000` did limit a
  `RUN` step on this buildx.

Not checked: Debian 13's versions; cgroup v1; a Compose older than 2.35; a
memory reservation above an added limit; a server with swap; whether each
template and database engine runs well in 1 CPU and 1 GiB; Nixpacks' options
(it isn't installed here).

## Tests

- `internal/config`, `internal/core`: each setting's good and refused values;
  `ParseCPUs`, `ParseMemory`, `Limits.Or`; nothing set gives a zero `Limits`.
- `internal/compose`:
  - `ParseModel` on recorded JSON: a service that only builds, both restart
    spellings, an unknown key kept.
  - `Check`, table-driven: one Compose file per refused option under
    `testdata/policy/`, its normalised JSON beside it, and the violation
    expected; files that must pass, the shared network among them; the label
    rule alone; `Max` over, equal, under, `-1` and `cpu_quota`.
  - The same table through the real `docker compose config`, skipped when the
    command is missing. It needs no daemon, and CI's runner has Compose. This
    pins the normalising table to the installed Compose.
  - `LimitExtras` and `Override` for each row of the tables under
    [Compose services](#compose-services); a nil map gives the R1 to R4 plan's
    golden output.
- `internal/catalog`: every template passes strict `Check`, through the real
  command, skipped as above.
- `internal/deploy`, with `compose.Fake` and `docker.Fake`:
  - no `config` call with nothing set;
  - a refused model fails in `building` with the lines above and no `build`
    call; the label rule when strict, with a header, and with neither;
  - an image with `traefik.*` labels: single-container when strict; a Compose
    service after `pull`;
  - both cutover styles, a Compose app and a preview carry the limits;
  - the model's JSON appears in no log line;
  - with a cap of 1, two apps' builds run one after the other while a third
    app's rollback runs; a rollback doesn't pass an older queued build of its
    own app.
- `internal/docker`: `CreateContainer` sends the three limits and
  `ContainerStats` reads them, against a faked Engine API as `cancel_test.go`
  does.
- `internal/dbaas`, `internal/backup`: specifications carry the default; in
  strict mode a database's has no port.
- `internal/store`: the migration, `SetAppLimits`, `NextClaimable` both ways.
- `internal/server`: the limits route refuses everyone but a system admin and
  answers 404 with no default; the panel per role; a database port is refused
  when strict; the statistics answer for one container and for a stack; the
  queued note.
- `main`: the CPU count check; the reduced Compose environment when strict.

## Docs

- `README.md`: five rows in the configuration table.
- `docs/GUARD-RAILS.md` (new): what strict mode refuses and what to write
  instead; the label rule; that it isn't containment; which container gets
  which limit and when a change applies; the build cap; the list under
  [What only the real host can show](#what-only-the-real-host-can-show).
- `ARCHITECTURE.md`:
  - "Container guard rails (opt-in)": the model reader, the allow-list, labels
    and images, and that the earlier non-goal "Restricting Compose files" no
    longer holds in strict mode;
  - "Resource limits (opt-in)": the table of which container gets which;
  - "Queue & concurrency semantics": the build cap, with a pointer from the
    locked "Background jobs" line, which stays as written;
  - the package layout: `compose/model.go`, `compose/policy.go`,
    `core/limits.go`, `server/limits.go`.
- `AGENTS.md`, "Decisions to keep": strict mode is an allow-list, so a new
  Compose key is refused until someone lists it; the model is never logged;
  every path that creates a container sets its limits; a deployment that
  builds nothing never waits for a build slot.
- `docs/PROTECTED-ROUTES.md` and `docs/BEHIND-A-PROXY.md`: a line each on the
  label rule. The fork-preview hint stays: fork builds still have no limits.

## Order of work

On `enhancements`, each commit with its tests and docs.

**Shared, first:** `Runner.Config`, `ParseModel`, `ReadModel`. Nothing calls
them yet. R10 can be built on this commit alone.

**R6:** (1) the setting; (2) `Check` and its test data; (3) the Compose
pipeline reads and checks the model; (4) the label rule; (5) images with
routing labels, with `Runner.Pull`; (6) the reduced environment; (7)
databases publish no port; (8) the template test.

**R7:** (1) `core.Limits`, the three defaults, the CPU count check; (2)
`docker.ContainerSpec` and `real.CreateContainer`; (3) single-container apps,
databases and helpers; (4) Compose services through the override; (5) the
migration, store, route and panel; (6) statistics and the two pages; (7)
`OUTHAUL_MAX_CONCURRENT_BUILDS`.

**Last, in whichever series comes second:** where R6 and R7 meet.

Against R1 to R4: R6 needs `Config.HeaderRequired` only for the header case in
steps 4 and 5; the rest can come first. R7's step 4 changes `Override`'s
signature, which that plan's Task 1 changes too, so it follows Task 1. R7's
other steps don't depend on it.

## What only the real host can show

For the infrastructure side to run on the staging host.

1. A Compose app with `privileged: true`, with a host path, and with `ports:`
   is each refused, and the message names the service and the option.
2. A Compose file with a `traefik.http.routers…` label is refused.
3. Each of the nine templates deploys in strict mode with the default limits
   and stays up for a day. So does each database engine.
4. `docker inspect` on a Nixpacks app, a Compose service, a database and a
   preview shows `NanoCpus`, `Memory` and `PidsLimit`.
5. A container that allocates past its memory limit is killed, and the other
   apps keep answering.
6. With a cap of 1, two apps deployed together build one after the other, and
   a rollback of a third starts at once.
7. A database can't be given an external port, and `ss -ltn` shows none.
8. A Compose file that reads `${OUTHAUL_REQUIRE_HEADER_VALUE}` gets nothing.
9. `docker compose version` on the host is 2.35 or newer.

## Left to the builder

- How `Check` walks the model, and how the top-level sections are held.
- The exact wording of each violation, notice and log line.
- How the recorded JSON under `testdata/policy/` is made and refreshed.
- How `dbaas.Manager`, `backup.Manager` and the server are told the defaults
  and the policy: setters, as `SetHTTPSPort` is, or constructor arguments.
- Where the number 4 lives so `config` and `deploy` agree.

## Calls made in this design

These go beyond the brief, or choose between readings of it. The maintainer
approved this design on 11 November 2026. Those marked **(agreed)** were put
to the maintainer as questions. The rest were this design's calls, approved
with it.

1. **(agreed)** The build cap touches the locked "Background jobs" line
   ("different apps run concurrently"). That line stays as written, with a
   pointer to the cap. The cap is opt-in and counts only deployments that
   build. The other reading, making the four pipeline slots configurable, is
   smaller, but a rollback then waits behind a build.
2. Strict mode changes the earlier non-goal "Restricting Compose files", as
   the brief asks, and only when set.
3. **(agreed)** Strict mode lists what is allowed and refuses any key it
   doesn't know. The brief lists what to refuse. A refuse list needs fewer
   edits to third-party files and misses each new Compose key.
4. "Configured maximums" are the app's own limits. There are no
   `OUTHAUL_MAX_*` settings.
5. In strict mode a database's host port is refused, not bound to
   `127.0.0.1`. One already published stays until its container is next
   replaced.
6. The reduced environment for `docker compose` is part of strict mode. It
   could be on always.
7. A changed limit applies on the next deploy. Nothing is redeployed and no
   running container is updated.
8. Refused beyond the brief's list: the keys named "beyond it", the `build`
   and top-level tables, `file` secrets and configs, `volumes_from` with
   `container:`, and `security_opt` other than `no-new-privileges`.
9. Allowed: `sysctls`, `extra_hosts`, `group_add`, `logging`, `ulimits`,
   `tmpfs`, `user: root`, `memswap_limit`, and joining Outhaul's shared
   network (no other external network).
10. `traefik.*` and `outhaul.*` labels in a Compose file are refused both in
    strict mode and while a header is required.
11. Compose service images are pulled and their labels checked before `up`,
    which adds `Runner.Pull`. The brief names only labels a file writes.
12. Paths are compared as Compose prints them. Symlinks in a checkout aren't
    followed.
13. Helper containers get the server default; Traefik and cloudflared get no
    limit; a preview uses its parent's; no limit for one database; no swap
    limit.
14. `OUTHAUL_MAX_CONCURRENT_BUILDS` takes 1 to 4; a CPU default above the
    server's CPU count stops startup; `OUTHAUL_CONTAINER_POLICY` takes only
    `strict`.
15. Build steps get no limit. `docker build` can limit them, but Nixpacks,
    `docker build` and `compose build` would each need it, and it varies by
    version.
16. The Limits panel and its route exist only when a default is set, and one
    app can't be given "no limit".
17. A Compose deployment always counts as a build. A waiting build stays
    `queued` with a note on its page; nothing is written to its log.
18. Some tests run the `docker compose` command (no daemon) and are skipped
    where it is missing.
19. Reading the model needs Compose 2.35 or newer, for `--no-env-resolution`.
20. `config` imports `core`.
