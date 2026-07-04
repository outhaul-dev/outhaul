# Outhaul — Architecture

Outhaul is an open-source, self-hosted PaaS: a single Go binary plus SQLite that
turns a fresh VPS into a git-push-to-deploy platform by orchestrating Docker and
Traefik. It is positioned as a minimal alternative to Dokploy/Coolify — one
~22 MB binary, no Node, no Postgres, installed with `curl | sh`.

This document is the source of truth for the **locked architecture**, the
**package layout**, and the **deployment state machine**. Get the state machine
right here before writing the worker.

---

## Locked decisions (do not revisit)

- **Language/binary.** Go 1.24+, single module, single static binary. Web UI
  embedded via `embed.FS`. No JS build step, ever.
- **Storage.** SQLite via `modernc.org/sqlite` (pure Go, CGO stays off).
  Migrations embedded, run on startup. WAL mode.
- **Docker.** Official `github.com/docker/docker/client` SDK against the local
  socket. Never shell out to the `docker` CLI.
- **Reverse proxy.** Traefik, configured through the **Docker provider**
  (container labels), not the file provider. Outhaul starts and manages the
  Traefik container itself.
- **Builds.** Nixpacks initially (shell out to the `nixpacks` binary), abstracted
  behind a `Builder` interface so Dockerfile/buildpack strategies can be added.
- **HTTP.** `net/http` with stdlib 1.22+ routing patterns. No web framework.
  SSE for log/build streaming, not websockets.
- **Background jobs.** In-process worker with the `deployments` table as the
  queue. No external queue. Deploys for the same app serialize; different apps
  run concurrently.
- **Auth.** Single admin user for v1. argon2id password hash, session cookie.
  Created on first boot via a printed one-time setup URL.
- **Config.** Single `/var/lib/slipway/` data dir. Env-var overrides. No YAML.

### Milestone 1 scope (this session)

Thinnest end-to-end path: `slipway serve` → ensure Traefik container exists →
admin logs in → create an app (public Git URL + domain) → **Deploy** clones,
builds with Nixpacks, starts the container with correct Traefik labels, streams
build logs live to the browser via SSE → app reachable on its domain. Plus
`GET /healthz` and graceful shutdown that does not orphan a mid-flight deploy
(mark it failed on restart).

**Out of M1 (leave seams, don't build):** TLS/ACME, webhooks, GitHub App,
private repos, env-var management, multiple users/teams, databases-as-a-service,
metrics, multi-server.

### Milestone 2 (done)

TLS/ACME, env-var management, and app lifecycle (stop/restart/delete) — listed
above as out-of-scope for M1 — are now implemented. Still deferred: webhooks,
GitHub App, private repos, multiple users/teams, databases-as-a-service,
metrics, multi-server.

Design decisions from M2: env values are encrypted at rest with NaCl
`secretbox` and a local `secret.key`, and secrets are injected into the
runtime container only, never into the build. Deploys are health-gated — the
new container is polled before cutover, so a failed build never disrupts the
running app. App runtime state is derived from Docker rather than stored as a
desired-state column; the `unless-stopped` restart policy handles reboot.
HTTPS is Traefik + Let's Encrypt HTTP-01, enabled by setting
`OUTHAUL_ACME_EMAIL`, with a config-hash drift check that recreates the
Traefik container when its desired config changes.

### Milestone 3 (done)

Private repos and auto-deploy on push — listed above as deferred — are now
implemented. Still deferred: multiple users/teams, databases-as-a-service,
metrics, multi-server.

Design decisions from M3: private-repo access goes through a **GitHub App**,
set up via GitHub's manifest flow (the operator submits a pre-filled manifest,
GitHub redirects back with a temporary code that is exchanged for the App's
credentials — no manual "create an app" form-filling). Clones authenticate
with a short-lived **installation access token** minted from the App's
private key, so no long-lived user PAT is ever stored. As a fallback/general
path, each app also gets a per-app **SSH deploy key** (Ed25519, generated with
`sshkey`, private half encrypted at rest the same way as other secrets) that
can be added to a repo directly. Push notifications arrive via a **generic
per-app webhook** (`internal/webhook`): a per-app HMAC secret authenticates
the payload with a constant-time comparison, and `webhook.ParsePush` extracts
the pushed branch independent of the Git host's payload shape. Auto-deploy is
gated by two per-app settings — a target branch and an on/off toggle — so a
push only enqueues a deploy when both match; tag pushes and pushes to other
branches no-op. `OUTHAUL_PUBLIC_URL` supplies the externally reachable base
URL the GitHub App manifest and webhook URLs are built from; GitHub App setup
is unavailable until it is configured.

---

## Package layout

Single module `github.com/slipwaydev/slipway`.
`main.go` at the root; everything else under `internal/` so nothing is importable
by third parties. Dependencies point inward: `core` depends on nothing; `server`
and `deploy` wire the rest together.

```
slipway/
  main.go                     # entrypoint: parse `serve`, wire deps, run, graceful shutdown
  ARCHITECTURE.md

  internal/
    config/                   # data dir resolution, env-var overrides, defaults
        config.go

    core/                     # PURE domain: no I/O, no deps. The testable heart.
        app.go                # App model
        deployment.go         # Deployment model, DeployStatus enum
        statemachine.go       # legal transitions, terminal/active predicates
        statemachine_test.go  # table-driven

    store/                    # SQLite persistence + the queue
        store.go              # open DB, WAL, pragmas, single-writer serialization
        migrate.go            # embedded migrations, run on startup
        migrations/*.sql
        apps.go               # App CRUD
        deployments.go        # Deployment CRUD + queue ops (claim, recover-on-boot)

    docker/                   # Docker behind an interface (fake for tests)
        client.go             # Client interface: PullImage, Create/Start/Stop/Remove, Inspect, EnsureNetwork
        real.go               # SDK-backed implementation
        fake.go               # in-memory fake for unit tests

    traefik/
        labels.go             # Labels(app) -> map[string]string   (PURE, table-driven tested)
        labels_test.go
        proxy.go              # EnsureProxy(ctx, docker): create/adopt the Traefik container + network

    builder/
        builder.go            # Builder interface: Build(ctx, BuildRequest) streaming logs -> image tag
        nixpacks.go           # shells out to `nixpacks build`; requires nixpacks on PATH at runtime

    logstream/               # in-memory pub/sub broker: build/deploy log lines -> SSE subscribers
        broker.go

    sshkey/                   # per-app Ed25519 deploy keypair generation
        keygen.go             # Generate: private key encrypted at rest, public key for the repo host

    webhook/                  # generic push-webhook parsing + HMAC signature verification
        parse.go              # ParsePush: extract repo + branch from a push payload
        verify.go             # constant-time signature check against the per-app secret

    github/                   # GitHub App: manifest flow, JWT, installation-token client
        manifest.go           # BuildManifest: pre-filled App-creation manifest JSON
        jwt.go                # AppJWT: RS256 App JWT (stdlib crypto, no external deps)
        client.go             # Client interface: exchange manifest code, mint installation tokens
        real.go               # HTTP-backed implementation
        fake.go               # in-memory fake for unit tests

    deploy/                   # the worker/orchestrator — drives the state machine
        worker.go             # dispatcher loop: claim queued work, per-app serialization, concurrency across apps
        pipeline.go           # one deployment: clone -> build -> start container -> health -> running
        git.go                # clone a repo (public, SSH deploy key, or GitHub App installation token)

    server/
        server.go             # http.ServeMux, route table, middleware, graceful Shutdown
        auth.go               # argon2id, session cookies, first-boot setup token
        handlers.go           # apps list/create, deploy trigger, deployment detail
        sse.go                # SSE handler bridging logstream -> browser
        templates/*.tmpl      # html/template, embedded
        static/*              # CSS (from the design system), embedded
        embed.go              # embed.FS for templates + static
```

### Boundaries and testing seams

- **`core` is pure and has no imports** from other Outhaul packages or I/O. The
  state machine and its tests live here. This is what "get the state machine
  right first" means concretely.
- **Docker is an interface** (`docker.Client`) with a real SDK impl and an
  in-memory `fake`. Unit tests use the fake; no test touches a real Docker
  daemon. An integration build tag can exercise the real client later.
- **Traefik label generation is a pure function** (`traefik.Labels`) tested
  table-driven, independent of any running container.
- **`Builder` is an interface.** Nixpacks is one implementation. The pipeline
  depends on the interface, so a `Dockerfile` builder slots in without touching
  the worker.
- **`logstream.Broker`** decouples the pipeline (producer) from SSE handlers
  (consumers). The pipeline never knows about HTTP; the server never knows about
  the build.

---

## Deployment state machine

A `Deployment` is one deploy attempt for an app. The `deployments` table is also
the job queue. Status is the single source of truth for where an attempt is.

### States

| Status       | Kind     | Meaning                                                        |
|--------------|----------|----------------------------------------------------------------|
| `queued`     | active   | Accepted, waiting for a worker slot for its app.               |
| `building`   | active   | Worker owns it: cloning the repo and building the image.       |
| `deploying`  | active   | Image built; creating/starting the container with Traefik labels. |
| `running`    | terminal | Container started and healthy. This attempt is the live one.   |
| `failed`     | terminal | Clone/build/start error, or crash-recovery on restart.         |
| `cancelled`  | terminal | Operator cancelled before the container went live.             |

"active" = a worker may be operating on it. "terminal" = no further transitions.

### Transitions

```mermaid
stateDiagram-v2
    [*] --> queued : operator clicks Deploy

    queued    --> building   : worker claims (app free)
    queued    --> cancelled  : operator cancels

    building  --> deploying  : image built OK
    building  --> failed     : clone/build error
    building  --> cancelled  : operator cancels

    deploying --> running    : container up + healthy
    deploying --> failed     : container failed to start

    running   --> [*]
    failed    --> [*]
    cancelled --> [*]
```

ASCII fallback (same truth as the diagram):

```
                 +------------ cancel -----------+------------ cancel ----------+
                 v                                |                             |
   Deploy --> queued --claim--> building --built--> deploying --healthy--> running (terminal)
                 |                  |                    |
              cancel             err|                 err|
                 |                  v                    v
                 +--------------> failed <---------------+ (terminal)
                 |
                 +--------------> cancelled (terminal)
```

### Legal-transition table (drives `core.statemachine_test.go`)

| From \ To    | queued | building | deploying | running | failed | cancelled |
|--------------|:------:|:--------:|:---------:|:-------:|:------:|:---------:|
| **queued**   |   –    |    ✓     |     ✗     |    ✗    |   ✗    |     ✓     |
| **building** |   ✗    |    –     |     ✓     |    ✗    |   ✓    |     ✓     |
| **deploying**|   ✗    |    ✗     |     –     |    ✓    |   ✓    |     ✗     |
| **running**  |   ✗    |    ✗     |     ✗     |    –    |   ✗    |     ✗     |
| **failed**   |   ✗    |    ✗     |     ✗     |    ✗    |   –    |     ✗     |
| **cancelled**|   ✗    |    ✗     |     ✗     |    ✗    |   ✗    |     –     |

### Design decisions (my latitude, not locked — push back if you disagree)

1. **Cancellation is allowed only in `queued` and `building`.** Once we are
   `deploying` (mutating live container/Traefik state), we run to `running` or
   `failed` rather than tearing down mid-flight. Simpler and avoids a partial
   proxy state. Cancelling `building` signals the pipeline's `context` and marks
   the row `cancelled`.
2. **`running` is terminal for the attempt; supersession is not modelled in M1.**
   A new deploy creates a new `Deployment` row. The App points at its current
   live deployment; older `running` rows simply become history. A `superseded`
   status is a later seam, not M1.
3. **Crash recovery.** On startup, any deployment left in `building` or
   `deploying` (a process died mid-flight) is marked `failed` with reason
   `"interrupted by restart"`. Rows in `queued` are safe and are left to be
   picked up by the worker.
4. **Graceful shutdown.** On SIGINT/SIGTERM: stop accepting new HTTP, cancel the
   worker context. The in-flight deployment's pipeline context is cancelled; the
   worker marks it `failed` ("server shutting down") before exit. Combined with
   (3), no attempt is ever orphaned in an active state.

### Queue & concurrency semantics

- **Claim is atomic:** `UPDATE deployments SET status='building', started_at=? 
  WHERE id=? AND status='queued'`. If it affects 0 rows, another worker/loop won
  the race; skip.
- **Per-app serialization:** the dispatcher tracks the set of apps with an active
  (`building`/`deploying`) deployment. It never claims a second deployment for an
  app already active. Different apps proceed concurrently up to a worker cap.
- **SQLite writes are serialized** through the store (single writer + WAL +
  `busy_timeout`) so the pure-Go driver never trips on concurrent writers.
- **Ordering:** oldest `queued` first (`created_at`), so a rapid double-click
  deploys in submission order.

---

## Request → deploy data flow (M1 happy path)

```
Browser                 server            store            deploy.worker         docker / traefik / nixpacks
  |  POST /apps/{id}/deploy |               |                    |                         |
  |------------------------>| insert Deployment(queued)          |                         |
  |  302 -> deployment page | ------------> |                    |                         |
  |                         |               |  notify -----------> claim (queued->building)|
  |  GET /deployments/{id}/logs (SSE)       |                    |  git clone (public)     |
  |<==== log lines (logstream broker) ======|<------------------ |  nixpacks build ------->| image
  |                         |               |     building->deploying                      |
  |                         |               |                    |  create+start container-> (traefik labels)
  |                         |               |     deploying->running                       |
  |<==== "deployed" event ==|               |                    |                         |
  |                         |               |                    |   app now reachable on its domain via Traefik
```

## Runtime dependencies

- Docker daemon reachable at the local socket (`/var/run/docker.sock`).
- `nixpacks` on `PATH` (build-time strategy for M1). Absence is surfaced as a
  clear deploy failure, not a crash.
- Traefik image pullable; Outhaul creates the proxy container and a shared
  `slipway` Docker network that app containers join.
