# Slipway

A single-binary, self-hosted PaaS. Point it at a fresh VPS, push a public Git
repo, and it clones, builds (Nixpacks), and runs your app behind Traefik on a
domain you choose — no Node, no Postgres, one ~23 MB Go binary and a SQLite file.

Slipway is a deliberately minimal alternative to Dokploy/Coolify.

> **Status: Milestone 1 (walking skeleton).** The thinnest end-to-end path works:
> log in, create an app, click Deploy, watch the build stream live, and reach the
> app on its domain. See [ARCHITECTURE.md](ARCHITECTURE.md) for the design and
> what is intentionally not built yet (TLS/ACME, webhooks, private repos, env
> vars, multiple users, databases, metrics, multi-server).

## Running

Requirements on the host: a reachable **Docker** daemon, **git**, and
**[nixpacks](https://nixpacks.com)** on `PATH`.

```sh
go build -o slipway .
./slipway serve
```

On first boot Slipway prints a one-time setup URL — open it to create the admin
account. The admin UI listens on `:8080` by default; Traefik is started
automatically and serves app traffic on `:80`.

## Configuration

No config files — defaults with `SLIPWAY_*` environment overrides:

| Variable                | Default              | Purpose                                   |
|-------------------------|----------------------|-------------------------------------------|
| `SLIPWAY_DATA_DIR`      | `/var/lib/slipway`   | SQLite DB and build work dirs             |
| `SLIPWAY_LISTEN_ADDR`   | `:8080`              | Admin UI / API listen address             |
| `SLIPWAY_DOCKER_HOST`   | (SDK env default)    | Docker endpoint                           |
| `SLIPWAY_TRAEFIK_IMAGE` | `traefik:v3.3`       | Managed Traefik image                     |
| `SLIPWAY_NETWORK`       | `slipway`            | Shared Docker network for app containers  |

Apps are expected to listen on `$PORT` (Slipway sets it and points Traefik at it).

## Development

```sh
go test ./...          # unit tests (fakes for Docker; no daemon needed)
go test -race ./...    # with the race detector
```

The codebase is a single Go module. `internal/core` is pure domain logic (the
deployment state machine); Docker, the builder, and git cloning sit behind
interfaces with fakes, so the test suite never touches a real daemon.
