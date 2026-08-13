# Manual deployment

These are the manual equivalents of what `curl -fsSL https://outhaul.sh/install | sh`
([`deploy/bootstrap.sh`](../deploy/bootstrap.sh)) does, for when you want to see
and control every step, or the installer doesn't fit your host. Following them
end-to-end produces the same layout the installer creates, so a later installer
re-run will recognize the install and simply upgrade it.

> [!WARNING]
> Outhaul is **alpha, pre-release software** — only deploy it to a disposable
> test server you can afford to wipe. It needs Docker-socket access, which is
> root-equivalent, so treat the box as fully entrusted to Outhaul. See the
> warning in the [README](../README.md).

Target: a fresh **Debian** server (x86_64 or arm64) with **systemd**. Other
distros likely work but are untested for V1. All commands below run as root
(`sudo -i`) unless noted.

## Resulting layout

| Path | What |
|------|------|
| `/usr/local/bin/outhaul` | The binary |
| `/etc/systemd/system/outhaul.service` | Systemd unit (from [`deploy/outhaul.service`](../deploy/outhaul.service)) |
| `/etc/outhaul.env` | `OUTHAUL_*` configuration overrides |
| `/var/lib/outhaul` | Data dir: SQLite DB, build work dirs (owned by the `outhaul` user) |

## 1. Install the runtime dependencies

Outhaul needs a Docker daemon with the compose v2 plugin, `git`, and
(optionally) `nixpacks`.

```sh
# Docker (skip if already installed)
curl -fsSL https://get.docker.com | sh
systemctl enable --now docker
docker compose version   # must succeed; if not: apt-get install docker-compose-plugin

# git
apt-get update && apt-get install -y git

# nixpacks — optional but recommended: needed for auto-detected builds.
# Dockerfile and compose apps deploy without it.
curl -fsSL https://nixpacks.com/install.sh | bash
```

## 2. Build the binary

Outhaul is built from source with the Go toolchain pinned in `go.mod`
(currently **1.26.4** — check `toolchain`/`go` directives in
[`go.mod`](../go.mod) for the version to use).

If the server has less than 2 GiB of RAM+swap, the build may be OOM-killed;
add a temporary swapfile first:

```sh
fallocate -l 2G /var/lib/outhaul-build.swap
chmod 600 /var/lib/outhaul-build.swap
mkswap /var/lib/outhaul-build.swap && swapon /var/lib/outhaul-build.swap
```

Download and verify Go, then build (substitute the version from `go.mod` and
`amd64`/`arm64` for your architecture):

```sh
GO_VER=1.26.4
ARCH=amd64   # or arm64
cd "$(mktemp -d)"
curl -fsSLO "https://dl.google.com/go/go${GO_VER}.linux-${ARCH}.tar.gz"
curl -fsSLO "https://dl.google.com/go/go${GO_VER}.linux-${ARCH}.tar.gz.sha256"
echo "$(cat go${GO_VER}.linux-${ARCH}.tar.gz.sha256)  go${GO_VER}.linux-${ARCH}.tar.gz" | sha256sum -c -
tar -xzf "go${GO_VER}.linux-${ARCH}.tar.gz"
export PATH="$PWD/go/bin:$PATH" GOTOOLCHAIN=local

git clone --depth 1 https://github.com/outhaul-dev/outhaul.git
cd outhaul
GOFLAGS=-buildvcs=false go build -o outhaul .
```

(From an existing checkout, just `go build -o outhaul .` in the repo root.)

Install it:

```sh
install -m 0755 outhaul /usr/local/bin/outhaul
```

If you added the temporary swapfile, remove it now:

```sh
swapoff /var/lib/outhaul-build.swap && rm -f /var/lib/outhaul-build.swap
```

## 3. Create the service user

Don't run Outhaul as root. Create a dedicated system user with no login shell,
home at the data dir, and membership of the `docker` group:

```sh
useradd --system --home-dir /var/lib/outhaul --create-home \
        --shell "$(command -v nologin || echo /usr/sbin/nologin)" outhaul
usermod -aG docker outhaul
```

Note that `docker` group membership is still root-equivalent; the dedicated
user is least-privilege hygiene, not a hard security boundary. The systemd
unit layers sandboxing on top.

## 4. Write the configuration

Configuration is environment-only — `OUTHAUL_*` variables read from
`/etc/outhaul.env` (see the full table in the [README](../README.md#configuration)).
Comments must be on their **own line**: systemd keeps an inline `# ...` as
part of the value.

Pick the block matching how apps should be reachable:

**Public domain + automatic HTTPS (Let's Encrypt).** First make sure a DNS A
record for your domain points at this server's public IP, and that nothing
else is listening on ports 80/443. Then:

```sh
cat > /etc/outhaul.env <<'EOF'
# OUTHAUL_* overrides, one per line. Comments on their OWN line.
# Edit, then: systemctl restart outhaul
OUTHAUL_PUBLIC_URL=https://paas.example.com
OUTHAUL_ACME_EMAIL=you@example.com
OUTHAUL_SSH_ADDR=:2222
EOF
chmod 0600 /etc/outhaul.env
```

**Local network HTTPS (built-in CA, no public domain).** For LAN-only
installs that still want HTTPS (no cert warnings once the root is trusted):
set `OUTHAUL_LOCAL_CA=true` instead of `OUTHAUL_ACME_EMAIL`. Outhaul creates
a certificate authority on first boot and mints/rotates a certificate for
every app domain automatically — see [docs/LOCAL-CA.md](LOCAL-CA.md) for
installing the root on your devices.

```sh
cat > /etc/outhaul.env <<'EOF'
# OUTHAUL_* overrides, one per line. Comments on their OWN line.
# Edit, then: systemctl restart outhaul
OUTHAUL_LOCAL_CA=true
OUTHAUL_SSH_ADDR=:2222
EOF
chmod 0600 /etc/outhaul.env
```

`OUTHAUL_LOCAL_CA` and `OUTHAUL_ACME_EMAIL` are mutually exclusive.

**Cloudflare Tunnel** (no public IP or open ports needed) or **plain
local-only**: omit `OUTHAUL_PUBLIC_URL` and `OUTHAUL_ACME_EMAIL` — for a
tunnel you'll paste the connector token in **Settings → Tunnel** after setup;
for local-only you can add them later, or add `OUTHAUL_LOCAL_CA=true` (above)
for HTTPS without a public domain.

```sh
cat > /etc/outhaul.env <<'EOF'
# OUTHAUL_* overrides, one per line. Comments on their OWN line.
# Edit, then: systemctl restart outhaul
OUTHAUL_SSH_ADDR=:2222
EOF
chmod 0600 /etc/outhaul.env
```

`OUTHAUL_SSH_ADDR` enables git-push-to-deploy on that port; drop the line to
disable it.

## 5. Install and start the service

The unit ships in the repo — install it verbatim:

```sh
install -m 0644 deploy/outhaul.service /etc/systemd/system/outhaul.service
systemctl daemon-reload
systemctl enable --now outhaul
```

The unit runs as the `outhaul` user, owns `/var/lib/outhaul` via
`StateDirectory`, and applies sandboxing (`NoNewPrivileges`, `PrivateTmp`,
`ProtectSystem=full`, `ProtectHome`). The binary binds only the admin UI port
(`:8080` by default); Traefik's *container* publishes 80/443 through the
Docker daemon.

## 6. Firewall (optional but recommended)

With ufw, **allow every needed port — especially SSH — before enabling**, or
you'll cut off your own session:

```sh
apt-get install -y ufw
ufw allow 22/tcp          # SSH — always. If sshd listens elsewhere, allow that port too.
ufw allow 80/tcp          # Let's Encrypt and local-CA modes
ufw allow 443/tcp         # Let's Encrypt and local-CA modes
ufw allow 2222/tcp        # only if git-push-to-deploy is enabled
ufw --force enable
```

Cloudflare Tunnel and plain local-only modes need no inbound app ports — just
SSH (and the git port, if enabled). The local CA needs 80/443 like Let's
Encrypt mode: Traefik serves the HTTPS redirect and the certificates there.

## 7. First-run setup

Check the service came up and grab the one-time setup URL from the journal:

```sh
systemctl status outhaul
curl -sS -o /dev/null -w '%{http_code}\n' http://127.0.0.1:8080/   # any HTTP answer means it's up
journalctl -u outhaul -b | grep -i setup
```

Open the printed `http://…/setup?token=…` URL to create the admin account.
For a local-only install, reach it over an SSH tunnel:
`ssh -L 8080:127.0.0.1:8080 you@server`, then browse `http://127.0.0.1:8080`.

## Upgrading

Rebuild and swap the binary; data and config are untouched:

```sh
cd /path/to/outhaul && git pull
GOFLAGS=-buildvcs=false go build -o outhaul .
install -m 0755 outhaul /usr/local/bin/outhaul
install -m 0644 deploy/outhaul.service /etc/systemd/system/outhaul.service
systemctl daemon-reload
systemctl restart outhaul
```

(Or just re-run the installer — it skips completed steps and upgrades the
binary.) There are **no data-migration guarantees** between alpha versions.

## Uninstalling

```sh
systemctl disable --now outhaul
rm -f /etc/systemd/system/outhaul.service /usr/local/bin/outhaul /etc/outhaul.env
systemctl daemon-reload
userdel outhaul
rm -rf /var/lib/outhaul        # DESTROYS the database and all app state
```

App containers, images, volumes, and the `outhaul` Docker network are left
behind — inspect with `docker ps -a` / `docker volume ls` and remove what you
no longer want.
