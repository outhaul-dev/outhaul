# Outhaul

A single-binary, self-hosted PaaS. Point it at a fresh VPS, push a public Git
repo, and it clones, builds (Nixpacks), and runs your app behind Traefik on a
domain you choose — no Node, no Postgres, one ~22 MB Go binary and a SQLite file.

Outhaul is a deliberately minimal alternative to Dokploy/Coolify.

> **Status: Milestone 3 (git automation).** The thinnest end-to-end path
> works: log in, create an app, click Deploy, watch the build stream live, and
> reach the app on its domain. M2 adds automatic HTTPS (Let's Encrypt), env
> vars & secrets (encrypted at rest), health-gated deploys that never promote a
> broken build, and app lifecycle (stop/restart/delete). M3 adds private repos
> (via a GitHub App + per-app SSH deploy keys) and auto-deploy on push (via
> webhooks), with a per-app branch and auto-deploy toggle. Apps are grouped
> into Dokploy-style **projects** (workspaces for a product or client), which
> also hold **shared environment variables** that apps reference as
> `${{project.KEY}}`. Repos
> with a `docker-compose.yml` deploy as **compose stacks** (multi-service,
> with any number of domains routed to the stack's services), and **watch
> paths** scope auto-deploy to pushes that change matching files. The app page
> live-tails **runtime container logs** (per service for compose stacks) and
> shows **live metrics** (CPU, memory, network, and uptime, aggregated across
> a compose stack). Any past deployment with a built image can be **rolled
> back** to in one click — no rebuild, same health-gated cutover. Projects can
> hold **managed databases** (PostgreSQL, MySQL, Redis): one click provisions
> a container with generated credentials and persistent data, reachable by
> apps over the internal network (and optionally on a published host port).
> **Scheduled backups** ship database dumps and compose stacks' named volumes
> to any S3-compatible bucket (AWS, MinIO, R2, B2, …) on cron schedules with
> retention, run history, and one-click manual runs. See
> [ARCHITECTURE.md](ARCHITECTURE.md) for the design and what is intentionally
> not built yet (multiple users, backup restore UI, metrics history/alerts,
> multi-server).

## Running

Requirements on the host: a reachable **Docker** daemon, **git**, and
**[nixpacks](https://nixpacks.com)** on `PATH`. Compose apps additionally
need the **docker CLI with the compose v2 plugin** (`docker compose`).

```sh
go build -o slipway .
./slipway serve
```

On first boot Outhaul prints a one-time setup URL — open it to create the admin
account. The admin UI listens on `:8080` by default; Traefik is started
automatically and serves app traffic on `:80`.

## Configuration

No config files — defaults with `OUTHAUL_*` environment overrides:

| Variable                | Default              | Purpose                                   |
|-------------------------|----------------------|-------------------------------------------|
| `OUTHAUL_DATA_DIR`      | `/var/lib/slipway`   | SQLite DB and build work dirs             |
| `OUTHAUL_LISTEN_ADDR`   | `:8080`              | Admin UI / API listen address             |
| `OUTHAUL_DOCKER_HOST`   | (SDK env default)    | Docker endpoint                           |
| `OUTHAUL_TRAEFIK_IMAGE` | `traefik:v3.3`       | Managed Traefik image                     |
| `OUTHAUL_NETWORK`       | `slipway`            | Shared Docker network for app containers  |
| `OUTHAUL_ACME_EMAIL`    | (empty)              | Let's Encrypt email; set to enable HTTPS   |
| `OUTHAUL_ACME_STAGING`  | `false`              | Use the LE staging CA (testing)            |
| `OUTHAUL_HTTPS_PORT`    | `443`                | Host port for HTTPS                        |
| `OUTHAUL_HEALTH_TIMEOUT`| `60s`                | Deploy health-check deadline               |
| `OUTHAUL_PUBLIC_URL`    | (empty)              | Public base URL of the admin UI; enables GitHub App + webhook URLs |

Apps are expected to listen on `$PORT` (Outhaul sets it and points Traefik at it).

## Development

```sh
go test ./...          # unit tests (fakes for Docker; no daemon needed)
go test -race ./...    # with the race detector
```

The codebase is a single Go module. `internal/core` is pure domain logic (the
deployment state machine); Docker, the builder, and git cloning sit behind
interfaces with fakes, so the test suite never touches a real daemon.
