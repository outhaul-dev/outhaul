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
  socket for all runtime container operations. Builds are the exception and
  shell out (`nixpacks`, `docker build`, `docker compose`) — build tooling is
  CLI-first and streaming its output is the whole job.
- **Reverse proxy.** Traefik, configured through the **Docker provider**
  (container labels), not the file provider. Outhaul starts and manages the
  Traefik container itself.
- **Builds.** Nixpacks (shell out to the `nixpacks` binary) or the repo's own
  Dockerfile (shell out to `docker build`), abstracted behind a `Builder`
  interface so buildpack strategies can be added.
- **HTTP.** `net/http` with stdlib 1.22+ routing patterns. No web framework.
  SSE for log/build streaming, not websockets.
- **Background jobs.** In-process worker with the `deployments` table as the
  queue. No external queue. Deploys for the same app serialize; different apps
  run concurrently.
- **Auth.** Single admin user for v1. argon2id password hash, session cookie.
  Created on first boot via a printed one-time setup URL.
- **Config.** Single `/var/lib/outhaul/` data dir. Env-var overrides. No YAML.

### Milestone 1 scope (this session)

Thinnest end-to-end path: `outhaul serve` → ensure Traefik container exists →
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

**Cloudflare Tunnel (optional ingress).** When a connector token is set
(Settings → Cloudflare Tunnel, stored sealed in `settings`), Outhaul runs a
`cloudflared` container on the shared network and flips Traefik to a plain-HTTP
posture: no `:443`, no ACME, no redirect, and no published host ports. Public
traffic arrives over the outbound tunnel and Cloudflare terminates TLS at its
edge; every Cloudflare public hostname points at `http://outhaul-traefik:80`,
which Traefik routes by Host header as usual. The tunnel takes precedence over
`OUTHAUL_ACME_EMAIL` when both are set.

### Milestone 3 (done)

Private repos and auto-deploy on push — listed above as deferred — are now
implemented. Still deferred: multiple users/teams, databases-as-a-service,
metrics, multi-server.

### Projects (done)

Apps are grouped into **projects** — Dokploy-style workspaces (a product, a
client) — via a `projects` table and an `apps.project_id` column. A `default`
project is created by migration and backfilled with existing apps, so app
creation never requires a project step first. Deliberate divergences from
Dokploy, argued in `docs/superpowers/specs/2026-07-04-projects-design.md`:
no organization layer (single-admin), no environment layer yet (a seam, the
same retrofit path Dokploy took), and project deletion is guarded rather than
cascading — a project must be emptied of apps before it can be deleted, since
deleting an app tears down a live container. Deployments and webhooks never
see a project; apps remain the deployable unit. There is no DB-level FK on
`apps.project_id` (SQLite can't add a `NOT NULL` FK column without a table
rebuild); the store enforces the reference instead.

Projects also hold **shared environment variables** (Dokploy's model, design
in `docs/superpowers/specs/2026-07-04-project-env.md`): a per-project
dictionary (`project_env`, encrypted at rest like `app_env`, edited from a
Shared variables panel on the project page) that apps opt into by writing
`${{project.KEY}}` inside their own env values. The worker resolves
references at deploy time in both pipelines (`core.ResolveEnv`): nothing is
injected unreferenced, a reference to an undefined shared variable fails the
deploy (never ship a literal placeholder), and a value that pulled in a
secret shared variable is treated as secret — kept out of the nixpacks build
env. Resolution is one level deep; changes apply on each app's next deploy.

### Docker Compose apps + watch paths (done)

Apps now carry a **kind** — `nixpacks` (the default; everything above) or
`compose` — chosen at creation. A compose app's repo ships a
`docker-compose.yml` (path configurable) and deploys as a whole stack by
shelling out to `docker compose` (v2 plugin, a new host requirement for
compose apps only) behind a `compose.Runner` interface with a fake, mirroring
the Nixpacks builder. The pipeline maps onto the same state machine:
`building` = clone + write `.env` (all env vars, for `${VAR}` interpolation,
Dokploy's layout) + `compose build`; `deploying` = `compose up -d --wait`
with the health timeout as the gate. **No blue-green for stacks** — compose
recreates containers in place (Dokploy behaves the same); the single-app
cutover is unchanged. Exposing a stack is opt-in and multi-domain (Dokploy's
model): every app, whatever its kind, has any number of rows in a single
`domains` table (`core.Domain`: host, optional compose service, container
port, optional path + internal-path rewrite, per-row TLS toggle), managed
from a Domains panel on the app page; `apps.domain` is an auto-maintained
"primary" mirror of the first row (by host, path) that list views read. For
compose apps each row routes one host (optionally scoped to a path) to one
service's container port. The pipeline layers a generated `outhaul.override.yml` over the
user's file (never rewriting it), attaching each published service to the
shared network and giving it one Traefik router per domain (named
`outhaul-<app>-d<domainID>`, unique and stable) plus `traefik.docker.network`.
Domain edits apply on the next deploy, when the override is regenerated.
Lifecycle is label-based (`docker compose -p outhaul-<name> stop|restart|down`)
so stop/restart/delete need no retained checkout; deletion keeps named
volumes. Raw pasted-YAML compose and Swarm mode are deferred (seams in
`docs/superpowers/specs/2026-07-04-compose-design.md`; multi-domain design in
`2026-07-04-compose-multidomain.md`).

**Watch paths** control *when* a push redeploys, for both kinds: per-app glob
patterns (`*`/`?` within a segment, `**` across, `[seq]`; a small built-in
matcher, no dependency) tested against the changed files a push webhook
reports. No patterns = every push to the branch deploys (unchanged default);
with patterns, a push that touches no matching file is skipped. A payload
carrying no file info fails open and deploys — not knowing what changed must
never silently drop a release (Dokploy crashes on that case; issue #4081).

### Runtime container logs (done)

The app page live-tails the *running* containers' stdout+stderr — Dokploy's
log viewer, adapted to the house SSE mechanism (design in
`docs/superpowers/specs/2026-07-04-runtime-logs.md`). Unlike build logs there
is no broker or history: the Docker daemon is the log store, so
`GET /apps/{id}/logs` opens its own follow stream per request
(`docker.Client.ContainerLogs`, which strips Docker's multiplexing framing),
replays a whitelisted tail (100/500/1000/5000 lines) and follows until the
container stops or the browser disconnects. Logs are per-container, as in
Dokploy: nixpacks apps tail their single container, compose apps pick one
service from a selector (resolved via compose labels). A stopped container
still serves its logs — post-mortem debugging is the point. Failures
("deploy first", unknown service) travel as in-stream `err` events because
EventSource clients can't read non-200 bodies. Search, time-range filters,
log-level parsing, ANSI rendering, and download are deliberately deferred.

### Live app metrics (done)

The app page's Metrics panel shows live CPU, memory, network I/O, and uptime
for the app's running containers — Dokploy's container monitoring, minus its
metrics store (design in `docs/superpowers/specs/2026-07-04-live-metrics.md`).
Dokploy polls the Docker stats API on a refresh interval and persists samples
for graphs and alerts; Outhaul keeps the data source and the poll model but
skips persistence entirely — the browser polls `GET /apps/{id}/stats` every
5s while the page is open, and each poll takes one one-shot
`docker.Client.ContainerStats` sample per container (the daemon primes CPU%
internally, so no state is kept between polls). Values match `docker stats`
semantics: CPU% where 100 = one core, memory usage minus reclaimable page
cache, cumulative network totals. Compose stacks aggregate across their
running containers — CPU/memory/network sum, memory limit is the max (an
unlimited container reports the host total; summing would double-count it),
uptime is the longest-running container's. The endpoint returns pre-formatted
display strings so formatting stays in testable Go. History, graphs,
threshold alerts, and host-level metrics are deliberately deferred.

### Rollback (done)

Every deployment row on the app page (and the deployment detail page) offers
**Rollback** once it has a built image — Dokploy's registry-based rollbacks
without the registry (design in `docs/superpowers/specs/2026-07-04-rollback.md`).
Dokploy tags and pushes each deploy's image to a configured registry and links
the deployment record to the tag; Outhaul already tags every nixpacks build
`outhaul/<app>:<depID>` and records it on the row, so the rollback material is
on the host — single-server means a registry buys nothing. Image retention
(below) bounds how far back that material goes: the newest `OUTHAUL_IMAGE_KEEP`
images per app stay rollback-able, older rows show "image pruned" instead of
the button. A rollback is an ordinary deployment enqueued with the source's
image and `rollback_of` pre-set (`POST /deployments/{id}/rollback`); the
pipeline sees the pre-set image and skips clone+build, then shares everything
downstream — health-gated blue-green cutover, cancel, crash recovery, per-app
serialization. Only the image is rolled back: env vars, domain, and routing
are the app's *current* settings (Dokploy snapshots config per deploy; one
state model is worth the divergence, and the deploy log says which image was
reused). Compose stacks can't roll back — `compose build` leaves no
per-deployment image handle — matching Dokploy's own limitation, and the
existing Deploy button is the "redeploy" (rebuild the branch head,
health-gated). Dokploy's Swarm-based auto-rollback has no equivalent because
it isn't needed: a failed deploy never touches the live container. Per-deploy
config snapshots are a deliberate seam.

### Databases as a service (done)

Projects can hold managed **databases** — PostgreSQL, MySQL, or Redis — next
to their apps (design in `docs/superpowers/specs/2026-07-04-databases.md`).
This is Dokploy's databases feature trimmed to Outhaul's shape: one form field
set (name + engine + optional image + optional external port) instead of five
credential fields, with the user/database name defaulting to the database's
name and the password always generated server-side (stored encrypted, same
secretbox scheme as env values). Each database is a plain container named
`outhaul-db-<name>` on the shared network, so apps connect internally by
hostname (`postgres://user:pass@outhaul-db-shop:5432/shop`); the database page
shows the ready-to-paste URL and nothing is auto-injected — wiring it into
apps is a copy-paste into project shared env (`${{project.KEY}}`), which is
also Dokploy's model. An optional **external port** publishes the engine's
port on the host for outside access — raw TCP publish rather than Traefik,
which is HTTP-only (Dokploy bypasses its proxy here too).

Provisioning (pull → create → start) runs in a background goroutine owned by
`internal/dbaas.Manager` — the databases counterpart of the deploy worker; the
server calls it through a small interface and never creates containers
itself. The row's stored status (`creating → running/failed`) exists only
because "being created" and "failed to create" aren't observable from Docker;
everything else derives from the container, which carries
`restart: unless-stopped` so Docker owns reboots (no Swarm, no HA — matching
the rest of Outhaul). Data lives in a bind mount under
`<data-dir>/databases/<name>` rather than a named volume, keeping all state
rsync-able in one directory; because of that, reprovisioning (retry after a
failed pull, or applying a changed external port) can simply remove and
recreate the container. Rows stuck in `creating` after a binary restart are
recovered as failed on boot, like interrupted deployments. Deleting a
database removes container, data directory, and row — the confirm dialog says
so. Scheduled S3 backups and restore are done (sections below). Deliberate
seams: more engines (a table row each), image upgrades, and a database
metrics panel.

### Scheduled backups to S3-compatible storage (done)

Operators register **destinations** — S3-compatible buckets (AWS, MinIO, R2,
B2, Wasabi, …) — once on the settings page, then attach **backup schedules**
to databases and compose apps: a 5-field cron expression, a key prefix, and a
retention count (design in `docs/superpowers/specs/2026-07-04-backups.md`).
This is Dokploy's backups model with two deliberate substitutions. First, no
rclone: Dokploy shells out to it for transfers, but Outhaul's S3 surface is
three calls (put, list, delete), so `internal/blobstore` implements them
directly with stdlib SigV4 signing — verified against AWS's published test
vector — the same house call as the hand-rolled GitHub App JWT. Path-style
addressing always; single-PUT uploads cap one archive at 5 GB (multipart is a
seam). Second, no cron library: `internal/cron` is a ~100-line 5-field parser
(vixie dom/dow OR-semantics) and the `internal/backup` manager just ticks
once a minute and runs whatever matches, in goroutines with a per-backup
in-flight guard.

Database backups run the engine's own tool **inside the database container**
via docker exec (`pg_dump -Fc --no-acl --no-owner`, `mysqldump
--single-transaction` — Dokploy's commands; the tools ship in the official
images so the host needs nothing), gzipped in Go, staged under the work dir,
then uploaded to `<prefix>/<name>/<timestamp>`. Redis is excluded, as in
Dokploy — no dump tooling; it's cache-shaped. App backups are Dokploy's
volume-backups feature: each **named volume** of a compose stack (found by
its compose-project label) is tarred by a transient `busybox` container and
uploaded as its own object; bind mounts aren't covered, and nixpacks apps are
excluded honestly — Outhaul gives them no volumes, so there is nothing to
back up. After a successful run, retention prunes each directory to the
newest N objects (timestamps sort lexicographically). Every run lands in a
history table (capped at 20 rows per schedule) shown on the target's page
next to Run-now/pause/remove; destinations have a Test button that writes and
deletes a probe object. Deliberate seams: multipart uploads,
stop-during-tar consistency locks, non-S3 destinations.

### Backup restore (done)

Each backup schedule has a **Restore** page (`/backups/{id}/restore`, design
in `docs/superpowers/specs/2026-07-05-backup-restore.md`) listing the
archives under the schedule's own bucket directory, newest first — restore is
deliberately scoped to *what this schedule backed up*, where Dokploy offers
free-text bucket paths and target database names to mistype. The transfer is
Dokploy's restore pipeline without the rclone: the archive is staged to the
work dir first (a network blip must not leave a half-restored database), then
gunzipped in Go and streamed into the engine's own tool inside the running
database container over docker exec stdin — `pg_restore -O --clean
--if-exists` for postgres (our dumps are `pg_dump -Fc`, exactly what it
expects), `mysql` for mysql (our dumps carry no `USE` statement, so they
always land in the named database — Dokploy's issue #3436 can't happen with
our own archives). Compose volume archives name their volume in the key;
restoring one stops the stack's running containers, empties the volume, untars
the archive into it with the same busybox helper backups use (volume mounted
rw, staged archive bind-mounted read-only), and restarts what was running —
best-effort even when the untar failed, because a broken volume with the
stack down is strictly worse. The volume must already exist; restore never
creates one. Restores share the schedule's in-flight guard (never overlapping
its backups) and land in the same run history with a `restore` badge.
Deliberate seams: cross-target restore (different database/volume/server),
upload-a-local-dump, PITR/incremental, automatic pre-restore safety backup
(the confirm dialog tells the operator to Run now first).

### Dockerfile builds (done)

A third app kind, `dockerfile`, deploys repos that carry their own Dockerfile
— because Nixpacks guesses wrong or the image is already dialed in (design in
`docs/superpowers/specs/2026-07-05-dockerfile-builds.md`, grounded in
Dokploy's `dockerfile` build type). It is *only* a build-strategy swap: the
app runs the exact same single-container pipeline as nixpacks apps, so
blue-green cutover, `$PORT` routing, health gating, rollback, image
retention, logs, metrics, and lifecycle all come for free with no new
branches. The worker holds one `builder.Builder` per single-container kind
(`deploy.Builders`) and the pipeline picks by `app.Kind`; the Dockerfile
strategy shells out to `docker build <ctx> --file <ctx>/<path> --tag
outhaul/<app>:<depID>` exactly like the Nixpacks strategy shells out to
`nixpacks`, with a pre-flight check that names the missing Dockerfile before
Docker prints its own noise. The per-app `dockerfile_path` (default
`Dockerfile`, validated relative like the compose path) is set on the create
form and editable in deploy settings. The build context is always the repo
root — with `--file` pointing anywhere, `COPY` paths keep working and a
monorepo Dockerfile can reference any file; a narrower context is a
performance knob, not a correctness one. Build-time env (the same non-secret
vars + `PORT` Nixpacks gets) is passed as `--build-arg`s; consuming one is
opt-in via `ARG`. Deliberate seams: context override, `--target` build
stage, `--no-cache`, BuildKit `--secret` mounts, buildpacks/static builds,
and Dockerfile autodetection at create time.

### One-click templates (done)

`internal/catalog` is a built-in gallery of popular open-source apps (Uptime
Kuma, Grafana, Umami, n8n, Vaultwarden, PocketBase, Ghost, MinIO) that deploy
as compose stacks with one click (design in
`docs/superpowers/specs/2026-07-05-one-click-templates.md`, grounded in
Dokploy's `Dokploy/templates` blueprints and its template processors). Where
Dokploy fetches blueprints from GitHub at deploy time, Outhaul embeds the
curated catalog in the binary (`go:embed`): no network dependency, and every
release ships a catalog its own tests have rendered. Each template is a
compose file plus a `template.json` manifest — JSON, not Dokploy's TOML, so
no new dependency — declaring variables (Dokploy's helper syntax:
`${domain}`, `${password:24}`, `${base64:64}`, `${hash}`, `${uuid}`,
`${email}`, `${username}`), the domains to route (service + port, mapped to
`ComposeDomain` rows), and env vars with an explicit secret flag.

Deploying a template creates a normal compose app with no repo (`Source =
"template"`): the compose text is snapshotted into `apps.compose_raw` —
Dokploy's raw-compose source — and `runComposePipeline` writes the snapshot
into the work dir instead of cloning. Everything downstream (.env, the
domain override, `up --wait` health gating, runtime logs, metrics,
stop/restart/delete, volume backups) is the existing compose machinery,
unbranched. Generated domains use Dokploy's zero-DNS shape
`<app>-<hash>-<ip-dashed>.sslip.io` (server IP from `OUTHAUL_SERVER_IP` or
auto-detected via the outbound-UDP local-address trick); generated
credentials land in the app's ordinary env vars, encrypted at rest and
editable on the app page. The catalog test cross-checks each entry both
ways: every manifest env key must be consumed by its compose file, and every
`${VAR}` the compose file reads must be fed by the manifest. Deliberate
seams: file mounts, template upgrades, user-pasted compose, a remote
catalog URL, and the jwt/timestamp/randomPort helpers.

### Disk cleanup: image retention, dangling images, build cache (done)

Left alone, a PaaS host fills its disk: every nixpacks deploy keeps its
`outhaul/<app>:<depID>` tag forever (that is what makes registry-less rollback
work), compose rebuilds leave the old images dangling, BuildKit cache grows
without bound, and deleting an app never deleted its images. Dokploy's answer
is an opt-in daily `docker image prune -a --force` — indiscriminate, and
survivable for them only because their rollbacks pull from a registry.
Outhaul's rollbacks are local images, so `internal/prune` replaces the blanket
prune with **deterministic per-app retention** driven by the deployments
table (design in `docs/superpowers/specs/2026-07-04-image-cleanup.md`): keep
the newest `OUTHAUL_IMAGE_KEEP` (default 5, 0 disables) **distinct** tags per
app — distinct because rollback rows repeat older tags — plus anything an
in-flight deployment references and the live image. Removed tags flag
`image_pruned` on every row bearing them, which hides the Rollback button
(a muted "image pruned" explains why) and makes the rollback handler reject
stale requests; the `image` column keeps its value so history stays truthful.
Removal never uses `--force`: an image Docker refuses to delete (in use)
stays rollback-able and is retried by the next sweep.

Retention runs right after each successful nixpacks deploy (logged into the
deploy log) and in a daily 03:30 sweep (minute ticker + the in-house cron
parser, like backups). The sweep also **reconciles** the `outhaul/*`
namespace — any tag no unpruned row references is removed, which converges
after partial failures and cleans pre-retention installs; tags of in-flight
deployments are skipped to dodge the SetImage race — then prunes **dangling
images only** (the safe subset of Dokploy's cleanup: never touches tagged
images, so pulled `postgres:*`/`traefik:*` and compose job images survive),
prunes **build cache** unused for 7 days (recent cache keeps rebuilds fast),
and removes crash-leftover `work/dep-*` checkouts and day-old backup staging
temps. Deleting an app now also best-effort removes its recorded images
(nixpacks) or runs `compose down --rmi local` (compose); anything missed is
the next sweep's reconciliation. The deploy worker calls the pruner through a
one-method hook interface so `internal/prune` stays out of the pipeline's
dependency graph. Deliberate seams: per-app retention overrides, a disk-usage
gauge, a "prune now" button.

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

### Push-to-deploy (done)

A fourth app source, alongside public/SSH/GitHub: Outhaul hosts the repo
itself. `internal/gitrepo` owns a bare repo per push app plus its
`post-receive` hook; `internal/gitssh` embeds an SSH server (default
`OUTHAUL_SSH_ADDR`, `:2222`, overridable live from Settings and persisted to
the `ssh_addr` setting) that authenticates `git push` against registered
**push keys** — SHA256-fingerprinted SSH public keys managed from
Settings — rather than per-app deploy keys, since the repo is local. A push
runs the hook, which relays over a unix socket (`internal/githook`) to the
deploy worker so the build streams back into the pusher's terminal exactly
like a browser-triggered deploy; a cold push to a name with no existing app
auto-creates one first, detecting the build kind (nixpacks/Dockerfile/compose)
from the pushed tree and minting a `sslip.io` domain when the server IP is
known. An app's page
shows its `ssh://git@<server>:<port>/<app>` remote and the `git remote add` /
`git push` incantation once it exists.

---

## Package layout

Single module `github.com/outhaul-dev/outhaul`.
`main.go` at the root; everything else under `internal/` so nothing is importable
by third parties. Dependencies point inward: `core` depends on nothing; `server`
and `deploy` wire the rest together.

```
outhaul/
  main.go                     # entrypoint: parse `serve`, wire deps, run, graceful shutdown
  ARCHITECTURE.md

  internal/
    config/                   # data dir resolution, env-var overrides, defaults
        config.go

    core/                     # PURE domain: no I/O, no deps. The testable heart.
        app.go                # App model
        database.go           # managed Database model (engine, credentials, lifecycle status)
        env.go                # EnvVar model + ResolveEnv (${{project.KEY}} references)
        project.go            # Project model (workspace grouping apps)
        deployment.go         # Deployment model, DeployStatus enum
        statemachine.go       # legal transitions, terminal/active predicates
        statemachine_test.go  # table-driven

    store/                    # SQLite persistence + the queue
        store.go              # open DB, WAL, pragmas, single-writer serialization
        migrate.go            # embedded migrations, run on startup
        migrations/*.sql
        apps.go               # App CRUD
        projects.go           # Project CRUD (guarded delete, app counts)
        project_env.go        # project-level shared env vars (encrypted at rest)
        databases.go          # managed-database CRUD (password encrypted at rest, recover-on-boot)
        backups.go            # S3 destinations (secret encrypted), backup schedules, run history
        deployments.go        # Deployment CRUD + queue ops (claim, recover-on-boot, rollbacks)

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
        dockerfile.go         # shells out to `docker build` for the repo's own Dockerfile

    compose/                  # docker compose stacks behind a Runner interface (fake for tests)
        compose.go            # Runner: Build/Up (files) + Stop/Restart/Down (label-based, -p only)
        override.go           # Override: generated outhaul.override.yml publishing services on their domains
        fake.go               # in-memory fake for unit tests

    catalog/                  # built-in one-click template gallery (go:embed)
        catalog.go            # load + validate embedded templates (compose + template.json manifest)
        expand.go             # variable engine: ${domain}/${password:n}/... helpers + references
        ip.go                 # outbound-IP detection for generated sslip.io domains
        templates/*/          # one dir per template: docker-compose.yml + template.json

    logstream/               # in-memory pub/sub broker: build/deploy log lines -> SSE subscribers
        broker.go

    sshkey/                   # per-app Ed25519 deploy keypair generation
        keygen.go             # Generate: private key encrypted at rest, public key for the repo host

    webhook/                  # generic push-webhook parsing + HMAC signature verification
        parse.go              # ParsePush: extract repo + branch + changed files from a push payload
        match.go              # watch-path glob matching (*, **, ?, [seq]) against changed files
        verify.go             # constant-time signature check against the per-app secret

    github/                   # GitHub App: manifest flow, JWT, installation-token client
        manifest.go           # BuildManifest: pre-filled App-creation manifest JSON
        jwt.go                # AppJWT: RS256 App JWT (stdlib crypto, no external deps)
        client.go             # Client interface: exchange manifest code, mint installation tokens
        real.go               # HTTP-backed implementation
        fake.go               # in-memory fake for unit tests

    dbaas/                    # managed database containers (databases-as-a-service)
        engines.go            # engine table: images, ports, env/credentials, connection URLs
        manager.go            # provision/start/stop/remove; the server's Databases interface

    cron/                     # 5-field cron expression parsing + minute matching (no deps)
        cron.go

    blobstore/                # S3-compatible object storage via stdlib SigV4 (no SDK)
        blobstore.go          # Client interface (Put/List/Delete), path-style requests, Probe
        sigv4.go              # AWS Signature V4 signing, tested against the AWS test vector

    backup/                   # the backup scheduler/executor
        manager.go            # minute ticker -> cron match -> dump/tar -> upload -> prune

    prune/                    # disk cleanup: per-app image retention + daily sweep
        pruner.go             # retention window, orphan reconciliation, dangling/build-cache prunes

    deploy/                   # the worker/orchestrator — drives the state machine
        worker.go             # dispatcher loop: claim queued work, per-app serialization, concurrency across apps
        pipeline.go           # one single-container deployment (nixpacks/dockerfile): clone -> build -> start -> health -> running
        pipeline_compose.go   # one compose deployment: clone (or template snapshot) -> .env/override -> compose build -> up --wait
        git.go                # clone a repo (public, SSH deploy key, or GitHub App installation token)

    server/
        server.go             # http.ServeMux, route table, middleware, graceful Shutdown
        auth.go               # argon2id, session cookies, first-boot setup token
        handlers.go           # apps list/create, deploy trigger, deployment detail
        catalog.go            # template gallery + one-click deploy-from-template
        databases.go          # managed-database pages: cross-project list, create, detail, lifecycle, settings
        backups.go            # backup schedules + destinations (create/test/delete, run-now)
        sse.go                # SSE handlers: build logs (logstream broker) + runtime container logs (docker follow)
        stats.go              # live app metrics: aggregated docker-stats snapshot, polled by the app page
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
- **`Builder` is an interface.** Nixpacks and the Dockerfile strategy are the
  two implementations; the pipeline picks by app kind and depends only on the
  interface, so buildpack strategies slot in without touching the worker.
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
| `superseded` | terminal | Was `running`, then a newer deploy took its place (container already removed at cutover). |

"active" = a worker may be operating on it. "terminal" = settled; the one
exception is `running --> superseded`, when a newer deploy retires it.

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

    running    --> superseded : a newer deploy takes over
    running    --> [*]
    superseded --> [*]
    failed     --> [*]
    cancelled  --> [*]
```

ASCII fallback (same truth as the diagram):

```
                 +------------ cancel -----------+------------ cancel ----------+
                 v                                |                             |
   Deploy --> queued --claim--> building --built--> deploying --healthy--> running --newer deploy--> superseded
                 |                  |                    |                  (terminal)              (terminal)
              cancel             err|                 err|
                 |                  v                    v
                 +--------------> failed <---------------+ (terminal)
                 |
                 +--------------> cancelled (terminal)
```

### Legal-transition table (drives `core.statemachine_test.go`)

| From \ To     | queued | building | deploying | running | failed | cancelled | superseded |
|---------------|:------:|:--------:|:---------:|:-------:|:------:|:---------:|:----------:|
| **queued**    |   –    |    ✓     |     ✗     |    ✗    |   ✗    |     ✓     |     ✗      |
| **building**  |   ✗    |    –     |     ✓     |    ✗    |   ✓    |     ✓     |     ✗      |
| **deploying** |   ✗    |    ✗     |     –     |    ✓    |   ✓    |     ✗     |     ✗      |
| **running**   |   ✗    |    ✗     |     ✗     |    –    |   ✗    |     ✗     |     ✓      |
| **failed**    |   ✗    |    ✗     |     ✗     |    ✗    |   –    |     ✗     |     ✗      |
| **cancelled** |   ✗    |    ✗     |     ✗     |    ✗    |   ✗    |     –     |     ✗      |
| **superseded**|   ✗    |    ✗     |     ✗     |    ✗    |   ✗    |     ✗     |     –      |

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
- `nixpacks` on `PATH` (the default build strategy); the `docker` CLI for
  Dockerfile apps (`docker build`) and compose stacks. Absence is surfaced as
  a clear deploy failure, not a crash.
- Traefik image pullable; Outhaul creates the proxy container and a shared
  `outhaul` Docker network that app containers join.
