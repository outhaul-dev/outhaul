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

> [!WARNING]
> **Outhaul is alpha, pre-release software — not ready for production.**
> Expect breaking changes, missing features, and rough edges, with no upgrade or
> data-migration guarantees between versions. Only run it on a disposable
> **test server you can afford to wipe** — never a production host or a machine
> holding anything you care about. Outhaul also needs Docker-socket access
> (which is root-equivalent), so treat any box you install it on as fully
> entrusted to it.

```sh
go build -o outhaul . && ./outhaul serve
# → open the one-time setup URL it prints, create the admin account, deploy.
```

## Install on a fresh Debian or Ubuntu VPS

    curl -fsSL https://outhaul.sh/install | sh

The installer walks you through ingress (Let's Encrypt or Cloudflare Tunnel),
opens the right firewall ports (keeping SSH), builds from source, and prints a
one-time setup URL. Developers with a local checkout can run
`sudo sh deploy/bootstrap.sh --from-checkout .`.

## Features

### Deploy straight from Git
- **Nixpacks builds** — push a repo and Outhaul detects the stack and builds it; no Dockerfile required.
- **Dockerfile builds** — repos that carry their own `Dockerfile` build with it, through the same pipeline.
- **Compose stacks** — a `docker-compose.yml` deploys as a multi-service stack, with any number of domains routed to its services.
- **Auto-deploy on push** — GitHub webhooks redeploy on push, scoped by a per-app branch and optional **watch paths** so only relevant changes trigger a build.
- **Private repos** — connect one or more GitHub accounts or organizations (each its own GitHub App), plus per-app SSH deploy keys.

### Deploys that don't break production
- **Health-gated blue-green cutover** — a new release only takes traffic once it passes health checks; a broken build is never promoted.
- **Automatic HTTPS** — Let's Encrypt certificates issued and renewed for every domain you assign.
- **Cloudflare Tunnel** — expose apps with no public IP and no port-forwarding: paste a Cloudflare connector token in Settings, and Outhaul runs `cloudflared` and routes each Cloudflare public hostname through Traefik. While the tunnel is the ingress, Cloudflare terminates HTTPS at its edge and Outhaul's Let's Encrypt automation stands down.
- **One-click rollback** — return to any past deployment that still has its image — no rebuild, same health-gated cutover.

### Organize your apps
- **Projects** — Dokploy-style workspaces that group the apps for a product or client.
- **Shared environment variables** — project-level values apps reference as `${{project.KEY}}`.
- **Secrets encrypted at rest** — env vars and generated credentials are sealed on disk.
- **App lifecycle** — stop, restart, and delete from the UI.
- **Single sign-on** — let people sign in through an OpenID Connect provider next to passwords. It has been tested so far only against a test provider, not a real one; see [docs/SSO.md](docs/SSO.md).
- **Protected routes** — any domain of an app can require sign-in to Outhaul, with a password or SSO, and the app receives the person's name in the `X-Outhaul-User` header, so an internal tool needs no login of its own. Checked so far against a real Traefik with a stand-in for Outhaul, not yet end to end; see [docs/PROTECTED-ROUTES.md](docs/PROTECTED-ROUTES.md).

### Batteries included
- **Managed databases** — one click provisions PostgreSQL, MySQL, or Redis with generated credentials and persistent storage, reachable over the internal network (and optionally on a published host port).
- **Template gallery** — deploy Uptime Kuma, Grafana, Umami, n8n, Vaultwarden, PocketBase, Ghost, WordPress, or MinIO in one click, with generated credentials and zero-DNS `sslip.io` domains — fully editable afterward like any compose app.
- **Scheduled backups & restore** — ship database dumps and compose volumes to any S3-compatible bucket (AWS, MinIO, R2, B2, …) on cron schedules, with retention and run history; restore any archive from the same page.
- **State backups** — a scheduled copy of Outhaul's own database (apps, domains, accounts, settings) to an S3-compatible bucket or Azure Blob, without sign-in sessions; see [docs/STATE-BACKUP.md](docs/STATE-BACKUP.md).

### See what's happening
- **Live runtime logs** — tail container logs in the browser, per service for compose stacks.
- **Live metrics** — CPU, memory, network, and uptime, aggregated across a stack.
- **Automatic disk cleanup** — keeps the newest images per app for rollback (`OUTHAUL_IMAGE_KEEP`) while a nightly sweep reclaims old images, dangling layers left by its own builds, and (unless `OUTHAUL_PRUNE_BUILD_CACHE=no`) stale build cache.

> See **[ARCHITECTURE.md](ARCHITECTURE.md)** for the design and what's
> intentionally out of scope for now (metrics history/alerts,
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

> [!NOTE]
> "Server" here means a **disposable test server**, not production — see the
> warning at the top. The installer sets up a long-running systemd service, so
> only point it at a box you're happy to wipe.

Don't run Outhaul as root. On the server:

```sh
curl -fsSL https://outhaul.sh/install | sh
```

That fetches [`deploy/bootstrap.sh`](deploy/bootstrap.sh) and runs it under
`sh`. From a checkout, run `sudo sh deploy/bootstrap.sh --from-checkout .`
instead — useful for testing local changes to the installer or app. Prefer to
run every step by hand? See [docs/MANUAL-DEPLOY.md](docs/MANUAL-DEPLOY.md).

The installer needs root once (systemd required; V1 targets Debian and
Ubuntu, prompting before continuing on anything else). Each step is skipped
when already done, so it is safe to re-run — and a re-run upgrades the binary
when the commit it targets differs, then restarts the service. It:

- installs **Docker** (via get.docker.com) when absent and allowed —
  `--install-docker` in an unattended run — **git**, and the pinned
  **nixpacks** when missing. A working Docker is used as it is, and the Docker
  install script never runs over an installed one;
- creates an `outhaul` **system user** (no login shell, home `/var/lib/outhaul`)
  in the `docker` group;
- walks you through ingress — Let's Encrypt, Cloudflare Tunnel, or local-only —
  and opens the right firewall ports, always keeping SSH reachable (when ufw
  is not yet active and other services are listening, it says which ports
  enabling it would block, and defaults to leaving the firewall alone). The
  admin port stays closed to the outside, but is allowed from Outhaul's Docker
  bridge, so Traefik can serve the admin UI at your public URL;
- builds the binary from source and installs it to `/usr/local/bin/outhaul`;
- writes `/etc/outhaul.env` (put `OUTHAUL_*` overrides there — ACME email,
  public URL) and installs and starts `outhaul.service`
  ([deploy/outhaul.service](deploy/outhaul.service));
- installs the **updater**: a small root script and two systemd units
  ([deploy/update.sh](deploy/update.sh)) that let Settings update Outhaul.

The installer prints the one-time setup URL, and it also lands in the
journal:

```sh
journalctl -u outhaul | grep -i setup
```

With Let's Encrypt (or the local CA and a public URL), it is under your public
URL. Otherwise it is on `localhost:8080`: reach it over an SSH tunnel,
`ssh -L 8080:127.0.0.1:8080 you@server`.

### Unattended

For cloud-init, an image build, or a configuration-management run, pass
`--non-interactive` and every answer as a flag. From a checkout of the commit
you want to install:

```sh
sh deploy/bootstrap.sh --non-interactive --from-checkout . \
  --ingress letsencrypt --public-url https://paas.example.com \
  --acme-email ops@example.com --git-ssh off --updates off
```

`--from-checkout` builds the tree you have. To install a commit you have not
checked out, use `--ref <full 40-character commit>` instead, which fetches it
from `OUTHAUL_REPO`; the two cannot be combined.

It never prompts or reads stdin. It installs Docker only with
`--install-docker`, and installs, enables or changes ufw only with
`--firewall ufw`. Its answers go to `/etc/outhaul/install.env` and a systemd
drop-in, and it never writes `/etc/outhaul.env`. It logs to
`/var/log/outhaul-install.log` and exits 1 on a failed run. Bad flags exit 2
before the log is opened, so nothing is logged for those. See
[docs/UNATTENDED-INSTALL.md](docs/UNATTENDED-INSTALL.md) for the flags, the env
files, re-runs and rollback.

**Why a dedicated user and not root?** The process needs exactly the Docker
socket, the `git`/`nixpacks`/`docker` CLIs, and its data dir. It never binds
a privileged port itself — Traefik's *container* publishes 80/443, and the
(root) Docker daemon does that binding. Note that `docker` group membership
is still root-equivalent (socket access can mount the host filesystem into a
container), so the dedicated user is least-privilege hygiene rather than a
hard security boundary; the unit layers systemd sandboxing on top
(`NoNewPrivileges`, `PrivateTmp`, `ProtectSystem=full`, `ProtectHome`).

### Updating

Settings → Updates compares the server with `main` and updates it on a click.
Your apps keep running while it builds. Outhaul restarts for a moment, and a
failed update rolls back to the version that was running. Changes to the
installer itself are not part of this: they need an installer run. A server
installed before the Updates panel existed gets it by re-running the installer
once.

An update that changes Traefik's image or configuration restarts Traefik, and
every app is unreachable for a few seconds; the panel says so before you
click Update.

Set `OUTHAUL_UPDATES=off` (or pass `--updates off` to an unattended install)
and the panel shows only the running version and no Update button. No check
leaves the server, and the root updater changes nothing: upgrade by re-running
the installer. The default, `main`, is as described above.

Each update's log is in `/var/lib/outhaul-updater/log` on the server
(`sudo cat` it), and `journalctl -u outhaul-update` has each update's result.
If an update leaves Outhaul down, the log says how to put the previous
version back.

### Restoring Outhaul's own state

`restore-state` puts a state copy back on a fresh install, from a file or from
the Azure container:

```sh
outhaul restore-state --from latest --key-file /root/secret.key
outhaul restore-state --from 20261111T021500Z --key-file /root/secret.key
outhaul restore-state --list
```

It fills an empty install only, proves the key before replacing anything, and
leaves the old database and key beside the new ones. The full steps, the
environment it needs and what a rebuild does not bring back are in
[docs/STATE-BACKUP.md](docs/STATE-BACKUP.md).

## Configuration

No config files — defaults with `OUTHAUL_*` environment overrides:

| Variable                | Default              | Purpose                                   |
|-------------------------|----------------------|-------------------------------------------|
| `OUTHAUL_DATA_DIR`      | `/var/lib/outhaul`   | SQLite DB and build work dirs             |
| `OUTHAUL_LISTEN_ADDR`   | `:8080`              | Admin UI / API listen address             |
| `OUTHAUL_DOCKER_HOST`   | (SDK env default)    | Docker endpoint                           |
| `OUTHAUL_TRAEFIK_IMAGE` | `traefik:v3.3`       | Managed Traefik image                     |
| `OUTHAUL_NETWORK`       | `outhaul`            | Shared Docker network for app containers  |
| `OUTHAUL_NETWORK_SUBNET`| (unset: Docker picks)| Fixed IPv4 subnet for the shared network, `/28` or larger — see docs/SHARED-HOST.md |
| `OUTHAUL_TRAEFIK_IP`    | (unset: Docker picks)| Traefik's fixed address in the subnet's lower half — see docs/SHARED-HOST.md |
| `OUTHAUL_ACME_EMAIL`    | (empty)              | Let's Encrypt email; set to enable HTTPS   |
| `OUTHAUL_ACME_STAGING`  | `false`              | Use the LE staging CA (testing)            |
| `OUTHAUL_ACME_DNS_PROVIDER` | (empty)          | A [lego DNS provider](https://go-acme.github.io/lego/dns/) code, such as `azuredns`; empty uses the HTTP-01 challenge, which needs port 80 |
| `OUTHAUL_ACME_DNS_ENV`  | (empty)              | Comma-separated names of environment variables passed to Traefik for the DNS provider, e.g. `AZURE_CLIENT_ID,AZURE_SUBSCRIPTION_ID` — see docs/BEHIND-A-PROXY.md |
| `OUTHAUL_TLS_WILDCARD`  | (empty)              | A wildcard such as `*.shared.demo.example.com`; one certificate covers every host exactly one label under it — see docs/BEHIND-A-PROXY.md |
| `OUTHAUL_REQUIRE_HEADER_NAME` | (empty)        | A request header every route requires, such as `X-Azure-FDID`; set with its value — see docs/BEHIND-A-PROXY.md |
| `OUTHAUL_REQUIRE_HEADER_VALUE` | (empty)       | The required header's value: letters, digits and `-._~+/=:`, at most 256 characters |
| `OUTHAUL_LOCAL_CA`      | `false`              | serve HTTPS from the built-in local CA (LAN installs; excludes OUTHAUL_ACME_EMAIL) — see docs/LOCAL-CA.md |
| `OUTHAUL_HTTPS_PORT`    | `443`                | Host port for HTTPS                        |
| `OUTHAUL_HTTP_PORT`     | `80`                 | `80`, or `off` for HTTPS only (no plain-HTTP entrypoint); `off` needs HTTPS and can't be combined with a Cloudflare Tunnel — see docs/BEHIND-A-PROXY.md |
| `OUTHAUL_ADMIN_DIRECT`  | `on`                 | `off` makes the admin UI listen only on loopback and Outhaul's Docker gateway, so it is reachable only through Traefik; needs `OUTHAUL_PUBLIC_URL` and HTTPS or a tunnel — see docs/BEHIND-A-PROXY.md |
| `OUTHAUL_HEALTH_TIMEOUT`| `60s`                | Deploy health-check deadline               |
| `OUTHAUL_IMAGE_KEEP`    | `5`                  | Built images kept per app for rollback (`0` keeps all) |
| `OUTHAUL_PRUNE_BUILD_CACHE` | `yes`            | Nightly prune of build cache unused for 7 days; it is daemon-wide, so set `no` on a host shared with other builds |
| `OUTHAUL_PUBLIC_URL`    | (empty)              | Public base URL of the admin UI; enables GitHub App + webhook URLs, SSO and protected routes |
| `OUTHAUL_SERVER_IP`     | (auto-detected)      | Public IP embedded in generated `sslip.io` template domains |
| `OUTHAUL_UPDATES`       | `main`               | `main` follows the newest commit on `main`; `off` stops the panel's update actions, the background check and the root updater (`releases` is not built) |
| `OUTHAUL_SSH_ADDR`      | `:2222`              | git-push SSH listen address; `off` opens no listener (the repositories and hook socket stay) |
| `OUTHAUL_BACKUP_AZURE_ACCOUNT` | (empty)      | Storage account for the Azure Blob backup destination; set with `OUTHAUL_BACKUP_AZURE_CONTAINER` — see docs/STATE-BACKUP.md |
| `OUTHAUL_BACKUP_AZURE_CONTAINER` | (empty)    | Private container for that destination; the host's managed identity needs "Storage Blob Data Contributor" on it |
| `OUTHAUL_STATE_BACKUP_SCHEDULE` | (empty)     | Five-field cron for a copy of Outhaul's own database; unset disables it — see docs/STATE-BACKUP.md |
| `OUTHAUL_STATE_BACKUP_DESTINATION` | (empty)  | Destination name for that copy; unset uses the Azure row |
| `OUTHAUL_STATE_BACKUP_KEEP` | `30`              | State copies kept; `0` keeps all |
| `OUTHAUL_STATE_BACKUP_PREFIX` | (empty)        | Key prefix for state copies, for hosts sharing a container |

`AZURE_CLIENT_ID` is not an `OUTHAUL_*` setting: when the VM has more than one
managed identity, set it to the one Outhaul should use.

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
