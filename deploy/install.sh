#!/usr/bin/env bash
# Outhaul installer for a fresh Linux VPS (systemd required).
#
# Run as root from a checkout of the repository:
#
#   sudo deploy/install.sh
#
# The installer needs root once — to install packages, create the service
# user, and register the systemd unit. Outhaul itself does NOT run as root:
# it runs as the dedicated `outhaul` system user, whose only privilege is
# membership in the `docker` group. Safe to re-run; every step skips work
# that is already done, and a re-run after `git pull` upgrades the binary.

set -euo pipefail

BIN_DEST=/usr/local/bin/outhaul
ENV_FILE=/etc/outhaul.env
UNIT_DEST=/etc/systemd/system/outhaul.service
SERVICE_USER=outhaul

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(dirname "$SCRIPT_DIR")"

log() { printf '\033[1;32m==>\033[0m %s\n' "$*"; }
die() { printf '\033[1;31merror:\033[0m %s\n' "$*" >&2; exit 1; }

[ "$(id -u)" -eq 0 ] || die "run as root (sudo deploy/install.sh)"
command -v systemctl >/dev/null 2>&1 || die "systemd is required"

# --- dependencies -----------------------------------------------------------

if ! command -v docker >/dev/null 2>&1; then
	log "installing Docker (get.docker.com)"
	curl -fsSL https://get.docker.com | sh
fi
systemctl enable --now docker >/dev/null 2>&1 || true
docker compose version >/dev/null 2>&1 ||
	die "the docker compose v2 plugin is missing (package docker-compose-plugin)"

if ! command -v git >/dev/null 2>&1; then
	log "installing git"
	if command -v apt-get >/dev/null 2>&1; then
		apt-get update -qq && apt-get install -y -qq git
	elif command -v dnf >/dev/null 2>&1; then
		dnf install -y -q git
	elif command -v yum >/dev/null 2>&1; then
		yum install -y -q git
	elif command -v zypper >/dev/null 2>&1; then
		zypper --non-interactive install git
	else
		die "no supported package manager found; install git and re-run"
	fi
fi

if ! command -v nixpacks >/dev/null 2>&1; then
	log "installing nixpacks (nixpacks.com/install.sh)"
	curl -sSL https://nixpacks.com/install.sh | bash
fi

# --- migration from the old "slipway" name ------------------------------------
# Pre-rename installs kept state in /var/lib/slipway and the binary at
# /usr/local/bin/slipway. Carry the important state (SQLite DB, secret key,
# ACME certs) across; containers under the old names are NOT migrated — remove
# them with `docker rm -f $(docker ps -aq --filter label=slipway.managed)`
# and redeploy from the UI.

if [ -d /var/lib/slipway ] && [ ! -e /var/lib/outhaul ]; then
	log "migrating data dir /var/lib/slipway -> /var/lib/outhaul"
	systemctl stop outhaul 2>/dev/null || true
	mv /var/lib/slipway /var/lib/outhaul
	[ -f /var/lib/outhaul/slipway.db ] && mv /var/lib/outhaul/slipway.db /var/lib/outhaul/outhaul.db
fi
rm -f /usr/local/bin/slipway

# --- service user ------------------------------------------------------------

if ! id "$SERVICE_USER" >/dev/null 2>&1; then
	log "creating system user $SERVICE_USER"
	nologin="$(command -v nologin || echo /usr/sbin/nologin)"
	useradd --system --home-dir /var/lib/outhaul --create-home \
		--shell "$nologin" "$SERVICE_USER"
fi
# docker group access is how the service talks to the daemon. Note: socket
# access is root-equivalent; the dedicated user is hygiene, not a boundary.
usermod -aG docker "$SERVICE_USER"

# --- binary -------------------------------------------------------------------

if [ ! -x "$REPO_ROOT/outhaul" ]; then
	command -v go >/dev/null 2>&1 && [ -f "$REPO_ROOT/go.mod" ] ||
		die "no ./outhaul binary and no Go toolchain; build the binary (go build -o outhaul .) and re-run"
	log "building outhaul from source"
	(cd "$REPO_ROOT" && go build -o outhaul .)
fi
log "installing binary to $BIN_DEST"
# install(1) unlinks the destination first, so upgrading a running service
# does not trip over ETXTBSY.
install -m 0755 "$REPO_ROOT/outhaul" "$BIN_DEST"

# --- configuration + unit -----------------------------------------------------

if [ ! -f "$ENV_FILE" ]; then
	log "writing $ENV_FILE"
	cat >"$ENV_FILE" <<'EOF'
# Outhaul configuration: OUTHAUL_* overrides, one per line.
# Uncomment and edit, then: systemctl restart outhaul
#OUTHAUL_ACME_EMAIL=you@example.com     # set to enable automatic HTTPS
#OUTHAUL_PUBLIC_URL=https://paas.example.com
#OUTHAUL_LISTEN_ADDR=:8080
#OUTHAUL_DATA_DIR=/var/lib/outhaul
EOF
	chmod 0600 "$ENV_FILE"
fi

log "installing systemd unit"
install -m 0644 "$SCRIPT_DIR/outhaul.service" "$UNIT_DEST"
systemctl daemon-reload
if systemctl is-active --quiet outhaul; then
	systemctl restart outhaul
else
	systemctl enable --now outhaul
fi

log "done — Outhaul is running"
cat <<'EOF'

First boot prints a one-time setup URL. Show it with:

    journalctl -u outhaul | grep -i setup

The admin UI listens on :8080; app traffic is served by Traefik on :80
(and :443 once OUTHAUL_ACME_EMAIL is set in /etc/outhaul.env).
EOF
