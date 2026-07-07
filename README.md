# Outhaul

[![License: AGPL-3.0](https://img.shields.io/badge/License-AGPL--3.0-blue.svg)](LICENSE)
[![Go](https://img.shields.io/github/go-mod/go-version/outhaul-dev/outhaul?logo=go&logoColor=white)](go.mod)
[![Self-hosted PaaS](https://img.shields.io/badge/self--hosted-PaaS-0b7285.svg)](#features)

**A single-binary, self-hosted PaaS.** Point it at a fresh VPS, push a Git repo,
and Outhaul clones it, builds it (Nixpacks), and runs it behind Traefik on a
domain you choose — with automatic HTTPS, health-gated deploys, and one-click
rollback.

No Node, no Postgres, no control-plane sprawl: **one ~22 MB Go binary and a
SQLite file.** Outhaul is a deliberately minimal alternative to Dokploy and
Coolify — the same core workflow, a fraction of the moving parts.

```sh
go build -o outhaul . && ./outhaul serve
# → open the one-time setup URL it prints, create the admin account, deploy.
```

## Features

### Deploy straight from Git
- **Nixpacks builds** — push a repo and Outhaul detects the stack and builds it; no Dockerfile required.
- **Dockerfile builds** — repos that carry their own `Dockerfile` build with it, through the same pipeline.
- **Compose stacks** — a `docker-compose.yml` deploys as a multi-service stack, with any number of domains routed to its services.
- **Auto-deploy on push** — GitHub webhooks redeploy on push, scoped by a per-app branch and optional **watch paths** so only relevant changes trigger a build.
- **Private repos** — a GitHub App plus per-app SSH deploy keys.

### Deploys that don't break production
- **Health-gated blue-green cutover** — a new release only takes traffic once it passes health checks; a broken build is never promoted.
- **Automatic HTTPS** — Let's Encrypt certificates issued and renewed for every domain you assign.
- **One-click rollback** — return to any past deployment that still has its image — no rebuild, same health-gated cutover.

### Organize your apps
- **Projects** — Dokploy-style workspaces that group the apps for a product or client.
- **Shared environment variables** — project-level values apps reference as `${{project.KEY}}`.
- **Secrets encrypted at rest** — env vars and generated credentials are sealed on disk.
- **App lifecycle** — stop, restart, and delete from the UI.

### Batteries included
- **Managed databases** — one click provisions PostgreSQL, MySQL, or Redis with generated credentials and persistent storage, reachable over the internal network (and optionally on a published host port).
- **Template gallery** — deploy Uptime Kuma, Grafana, Umami, n8n, Vaultwarden, PocketBase, Ghost, or MinIO in one click, with generated credentials and zero-DNS `sslip.io` domains — fully editable afterward like any compose app.
- **Scheduled backups & restore** — ship database dumps and compose volumes to any S3-compatible bucket (AWS, MinIO, R2, B2, …) on cron schedules, with retention and run history; restore any archive from the same page.

### See what's happening
- **Live runtime logs** — tail container logs in the browser, per service for compose stacks.
- **Live metrics** — CPU, memory, network, and uptime, aggregated across a stack.
- **Automatic disk cleanup** — keeps the newest images per app for rollback (`OUTHAUL_IMAGE_KEEP`) while a nightly sweep reclaims old images, dangling layers, and stale build cache.

> See **[ARCHITECTURE.md](ARCHITECTURE.md)** for the design and what's
> intentionally out of scope for now (multiple users, metrics history/alerts,
> multi-server).

## Running locally

Requirements on the host: a reachable **Docker** daemon, **git**, and
**[nixpacks](https://nixpacks.com)** on `PATH`. Dockerfile apps additionally
need the **docker CLI** (`docker build`), and compose apps the **docker CLI
with the compose v2 plugin** (`docker compose`).

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
git clone https://github.com/outhaul-dev/outhaul && cd outhaul
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
| `OUTHAUL_SERVER_IP`     | (auto-detected)      | Public IP embedded in generated `sslip.io` template domains |

Apps are expected to listen on `$PORT` (Outhaul sets it and points Traefik at it).

## Development

```sh
go test ./...          # unit tests (fakes for Docker; no daemon needed)
go test -race ./...    # with the race detector
```

The codebase is a single Go module. `internal/core` is pure domain logic (the
deployment state machine); Docker, the builder, and git cloning sit behind
interfaces with fakes, so the test suite never touches a real daemon.

## License

Outhaul is free software licensed under the [GNU Affero General Public License
v3.0](LICENSE). You may use, modify, and self-host it freely; if you run a
modified version as a network service, you must offer your users the
corresponding source.

Copyright (C) 2026 the Outhaul authors.
