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

Target: a fresh **Debian** or **Ubuntu** server (x86_64 or arm64) with
**systemd**. Other distros likely work but are untested for V1. All commands below run as root
(`sudo -i`) unless noted.

## Resulting layout

| Path | What |
|------|------|
| `/usr/local/bin/outhaul` | The binary |
| `/etc/systemd/system/outhaul.service` | Systemd unit (from [`deploy/outhaul.service`](../deploy/outhaul.service)) |
| `/etc/outhaul.env` | `OUTHAUL_*` configuration overrides |
| `/etc/outhaul/install.env` | Unattended answers, rewritten by each `--non-interactive` run (the installer writes it; optional here) |
| `/etc/systemd/system/outhaul.service.d/10-install.conf` | `EnvironmentFile=` drop-in written by an unattended or `--env-file` run (the same file exists under `outhaul-update.service.d/`) |
| `/var/lib/outhaul` | Data dir: SQLite DB, build work dirs (owned by the `outhaul` user) |
| `/usr/local/lib/outhaul`, `/var/lib/outhaul/update` | The updater (optional), and the directory it shares with Outhaul: see "Updating from the UI" |
| `/var/lib/outhaul-updater` | The updater's own directory (root's, group `outhaul`, mode 0710): the copy of the running version it rolls back to, and its log |
| `/var/cache/outhaul` | The Go toolchain the updater keeps between updates (root's) |

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
# Dockerfile and compose apps deploy without it. This is the installer's pin:
# one version, with the SHA-256 GitHub publishes for the release archive.
NIXPACKS_VERSION=1.41.0
case "$(uname -m)" in
x86_64 | amd64)
  target=x86_64-unknown-linux-musl
  sum=0f55de7874507b9cf7502113120bd96f2ab6979f78d10eaf2eb2ade9207b3af6
  ;;
aarch64 | arm64)
  target=aarch64-unknown-linux-musl
  sum=912bd02dd2bb6f9c3a9ed965fe8a68b4aa318dc7a2546e2eca6f2806a894ba39
  ;;
*)
  echo "unsupported architecture: $(uname -m)" >&2
  exit 1
  ;;
esac
NIXPACKS_TMP=$(mktemp -d)
curl -fsSL "https://github.com/railwayapp/nixpacks/releases/download/v${NIXPACKS_VERSION}/nixpacks-v${NIXPACKS_VERSION}-${target}.tar.gz" \
  -o "$NIXPACKS_TMP/nixpacks.tar.gz"
echo "$sum  $NIXPACKS_TMP/nixpacks.tar.gz" | sha256sum -c -
tar -C "$NIXPACKS_TMP" -xzf "$NIXPACKS_TMP/nixpacks.tar.gz"
install -m 0755 "$NIXPACKS_TMP/nixpacks" /usr/local/bin/nixpacks
rm -rf "$NIXPACKS_TMP"
nixpacks --version   # nixpacks 1.41.0
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

`OUTHAUL_SSH_ADDR` enables git-push-to-deploy on that port. Set it to `off` to
turn the SSH listener off (the default `:2222` applies when the line is
dropped); the bare repositories and the hook socket stay either way, and a
stored address in Settings is ignored while it is off.

### Env files and the installer drop-in

`/etc/outhaul.env` is the file the unit loads. The installer's unattended
answers live in `/etc/outhaul/install.env` (`root:root`, mode 0600, rewritten by
each `--non-interactive` run), and both the service and the updater load it
through a drop-in rather than by editing the unit — every update replaces the
unit file:

```ini
# /etc/systemd/system/outhaul.service.d/10-install.conf
[Service]
EnvironmentFile=/etc/outhaul/install.env
EnvironmentFile=/etc/outhaul/extra.env
```

The updater gets the same lines in
`/etc/systemd/system/outhaul-update.service.d/10-install.conf`, because it
needs the service's listen address, data dir, image overrides and
`OUTHAUL_UPDATES`. systemd loads the unit's own `/etc/outhaul.env` first, then
the drop-in's files in the order written, and a later file wins. The paths have
no `-` prefix, so a missing file stops the service instead of starting it with
half its settings. Run `systemctl daemon-reload` after writing or changing one.

An unattended run writes `install.env` and both drop-ins. A wizard run with
`--env-file` writes the drop-ins only, without `install.env`. A run with neither
flag leaves them alone, so a plain installer re-run does not drop a host's env
files. See [UNATTENDED-INSTALL.md](UNATTENDED-INSTALL.md) for the exact order
and validation.

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
ufw allow in on outhaul to any port 8080 proto tcp   # Traefik → admin UI
ufw --force enable
```

Cloudflare Tunnel and plain local-only modes need no inbound app ports — just
SSH (and the git port, if enabled). The local CA needs 80/443 like Let's
Encrypt mode: Traefik serves the HTTPS redirect and the certificates there.

Keep the admin port (8080) closed to the outside, but allow it from the
`outhaul` bridge. Traefik serves the admin UI at your public URL by connecting
to that port on the host, from its container on the `outhaul` Docker network,
and ufw drops that like any other inbound traffic: the public URL then times
out. Outhaul names the network's bridge after the network (`outhaul`, unless
you changed `OUTHAUL_NETWORK`; names over 15 bytes keep Docker's default) when
it creates the network. A network created by an older version has Docker's
`br-<id>` name instead, and the rule goes stale if that network is ever
recreated, so check the name with:

```sh
docker network inspect outhaul -f '{{.Id}} {{index .Options "com.docker.network.bridge.name"}}'
# "<id> outhaul" → outhaul; "<id> <no value>" → br-<first 12 characters of id>
```

## 7. First-run setup

Check the service came up and grab the one-time setup URL from the journal:

```sh
systemctl status outhaul
curl -sS -o /dev/null -w '%{http_code}\n' http://127.0.0.1:8080/   # any HTTP answer means it's up
journalctl -u outhaul -b | grep -i setup
```

Open the printed `open: …/setup?token=…` URL to create the admin account.
With a public URL and HTTPS (Let's Encrypt, or the local CA), it is under the
public URL. Otherwise, or while that does not answer yet, use the
`http://localhost:8080/setup?token=…` URL over an SSH tunnel:
`ssh -L 8080:127.0.0.1:8080 you@server`.

## Upgrading

Rebuild and swap the binary; data and config are untouched. Stamp the build
with its commit, as in "Updating from the UI" below, so the Updates panel
can tell which commit is running:

```sh
cd /path/to/outhaul && git pull
GOFLAGS=-buildvcs=false go build \
  -ldflags "-X github.com/outhaul-dev/outhaul/internal/update.commit=$(git rev-parse HEAD) -X github.com/outhaul-dev/outhaul/internal/update.repo=<your clone URL>" \
  -o outhaul .
install -m 0755 outhaul /usr/local/bin/outhaul
install -m 0644 deploy/outhaul.service /etc/systemd/system/outhaul.service
systemctl daemon-reload
systemctl restart outhaul
```

(Or just re-run the installer — it skips completed steps and upgrades the
binary when the commit it targets differs.) There are **no data-migration
guarantees** between alpha versions.

**Never install an older commit over a database a newer one has opened.**
Migrations only go forward, and the old binary does not detect a newer schema:
it starts and may misbehave. Install the same commit or a newer one. To go
back, restore a backup of the database and the matching binary together.

### Updating from the UI

Settings → Updates needs the updater. Install it once, as root, from the
checkout:

```sh
mkdir -p /usr/local/lib/outhaul
install -m 0755 deploy/update.sh /usr/local/lib/outhaul/update.sh
install -m 0644 deploy/bootstrap.sh /usr/local/lib/outhaul/bootstrap.sh
install -m 0644 deploy/outhaul-update.path deploy/outhaul-update.service /etc/systemd/system/
install -d -m 0710 -g outhaul /var/lib/outhaul-updater
# The update directory is in Outhaul's data dir: make it as the outhaul user.
runuser -u outhaul -- sh -c 'mkdir -p -m 0700 /var/lib/outhaul/update && echo 1 > /var/lib/outhaul/update/updater'
systemctl daemon-reload
systemctl enable --now outhaul-update.path
```

The file `updater` tells Outhaul the updater is there. The updater keeps its
own files in `/var/lib/outhaul-updater`, which only root can change: the
copy of the running version it rolls back to, and its log,
`/var/lib/outhaul-updater/log`, which Outhaul reads to show it in the admin
UI. Outhaul's `/var/lib/outhaul/update` gets the status. If an update leaves
Outhaul down, `sudo cat /var/lib/outhaul-updater/log` says how to put the
previous version back.

Keep `/var/lib/outhaul` and `/var/lib/outhaul-updater` on one filesystem, as
they are by default. The updater writes the status, and puts the database
back in a rollback, by renaming files from its directory into Outhaul's;
across filesystems a rename becomes a copy, which can be seen half-written.

Build the binary with the version stamp too, or the panel cannot tell which
commit is running. The installer's `build_ldflags` prints the flags:

```sh
GOFLAGS=-buildvcs=false go build \
  -ldflags "-X github.com/outhaul-dev/outhaul/internal/update.commit=$(git rev-parse HEAD) -X github.com/outhaul-dev/outhaul/internal/update.repo=<your clone URL>" \
  -o outhaul .
```

## Uninstalling

```sh
systemctl disable --now outhaul
systemctl disable --now outhaul-update.path   # if the updater is installed
rm -f /etc/systemd/system/outhaul.service /usr/local/bin/outhaul /etc/outhaul.env
rm -f /etc/systemd/system/outhaul-update.path /etc/systemd/system/outhaul-update.service
systemctl daemon-reload
rm -rf /usr/local/lib/outhaul /var/cache/outhaul
rm -rf /var/lib/outhaul-updater   # the updater's log, and its copy of the database
userdel outhaul
rm -rf /var/lib/outhaul        # DESTROYS the database and all app state
```

App containers, images, volumes, and the `outhaul` Docker network are left
behind — inspect with `docker ps -a` / `docker volume ls` and remove what you
no longer want.
