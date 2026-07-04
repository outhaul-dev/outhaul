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
> retention, run history, and one-click manual runs. **Disk cleanup** keeps a
> long-running host healthy: the newest builds per app stay on disk for
> rollback (`OUTHAUL_IMAGE_KEEP`, default 5) and a nightly sweep reclaims
> older images, dangling layers, and stale build cache. See
> [ARCHITECTURE.md](ARCHITECTURE.md) for the design and what is intentionally
> not built yet (multiple users, backup restore UI, metrics history/alerts,
> multi-server).

## Running

Requirements on the host: a reachable **Docker** daemon, **git**, and
**[nixpacks](https://nixpacks.com)** on `PATH`. Compose apps additionally
need the **docker CLI with the compose v2 plugin** (`docker compose`).

```sh
go build -o outhaul .
./outhaul serve
```

On first boot Outhaul prints a one-time setup URL — open it to create the admin
account. The admin UI listens on `:8080` by default; Traefik is started
automatically and serves app traffic on `:80`.

That's fine for kicking the tires; for a server install use the installer
below, which sets up a dedicated user and a systemd unit.

## Deploying to a server

Don't run Outhaul as root. From a checkout on the server:

```sh
git clone https://github.com/James-Smart/outhaul && cd outhaul
sudo deploy/install.sh
```

The installer needs root once (systemd required; Ubuntu/Debian/Fedora/RHEL/
openSUSE). Each step is skipped when already done, so it is safe to re-run —
and a re-run after `git pull` upgrades the binary and restarts the service.
It:

- installs **Docker** (via get.docker.com), **git**, and **nixpacks** if missing;
- creates an `outhaul` **system user** (no login shell, home `/var/lib/outhaul`)
  in the `docker` group;
- builds the binary if needed and installs it to `/usr/local/bin/outhaul`;
- writes `/etc/outhaul.env` (put `OUTHAUL_*` overrides there — ACME email,
  public URL) and installs and starts `outhaul.service`
  ([deploy/outhaul.service](deploy/outhaul.service)).

The one-time setup URL lands in the journal:

```sh
journalctl -u outhaul | grep -i setup
```

**Why a dedicated user and not root?** The process needs exactly the Docker
socket, the `git`/`nixpacks`/`docker` CLIs, and its data dir. It never binds
a privileged port itself — Traefik's *container* publishes 80/443, and the
(root) Docker daemon does that binding. Note that `docker` group membership
is still root-equivalent (socket access can mount the host filesystem into a
container), so the dedicated user is least-privilege hygiene rather than a
hard security boundary; the unit layers systemd sandboxing on top
(`NoNewPrivileges`, `PrivateTmp`, `ProtectSystem=full`, `ProtectHome`).

## Configuration

No config files — defaults with `OUTHAUL_*` environment overrides:

| Variable                | Default              | Purpose                                   |
|-------------------------|----------------------|-------------------------------------------|
| `OUTHAUL_DATA_DIR`      | `/var/lib/outhaul`   | SQLite DB and build work dirs             |
| `OUTHAUL_LISTEN_ADDR`   | `:8080`              | Admin UI / API listen address             |
| `OUTHAUL_DOCKER_HOST`   | (SDK env default)    | Docker endpoint                           |
| `OUTHAUL_TRAEFIK_IMAGE` | `traefik:v3.3`       | Managed Traefik image                     |
| `OUTHAUL_NETWORK`       | `outhaul`            | Shared Docker network for app containers  |
| `OUTHAUL_ACME_EMAIL`    | (empty)              | Let's Encrypt email; set to enable HTTPS   |
| `OUTHAUL_ACME_STAGING`  | `false`              | Use the LE staging CA (testing)            |
| `OUTHAUL_HTTPS_PORT`    | `443`                | Host port for HTTPS                        |
| `OUTHAUL_HEALTH_TIMEOUT`| `60s`                | Deploy health-check deadline               |
| `OUTHAUL_IMAGE_KEEP`    | `5`                  | Built images kept per app for rollback (`0` keeps all) |
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
