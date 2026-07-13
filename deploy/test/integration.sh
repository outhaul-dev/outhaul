#!/usr/bin/env sh
# End-to-end installer test in a systemd Debian container.
# Requires Docker with --privileged. Exercises mode c (local, no DNS/ACME),
# non-TTY log cleanliness, and re-run idempotency.
set -eu
img=outhaul-installer-test
root=$(cd "$(dirname "$0")/../.." && pwd)

docker build -f "$root/deploy/test/Dockerfile.debian" -t "$img" "$root"
cid=$(docker run -d --privileged --tmpfs /run --tmpfs /run/lock -v /sys/fs/cgroup:/sys/fs/cgroup:rw "$img")
trap 'docker rm -f "$cid" >/dev/null 2>&1 || true' EXIT
sleep 5  # let systemd come up

# Non-interactive run (mode c): feed answers on fd 0; installer reads /dev/tty
# which in this container falls back to fd 0. Choose 3 (local), disable git port,
# skip nixpacks, skip firewall.
docker exec -i "$cid" sh -c '
  printf "3\n\nn\nn\n" | sh /src/deploy/bootstrap.sh --from-checkout /src
' | tee /tmp/outhaul-install.out

# 0) sanity: the installer must have produced output — otherwise the escape
# check below would false-pass on an empty file.
[ -s /tmp/outhaul-install.out ] || { echo "FAIL: installer produced no output"; exit 1; }

# 1) escape-soup check: piped output must not contain raw ESC sequences.
if grep -q "$(printf "\033")\[" /tmp/outhaul-install.out; then
	echo "FAIL: escape sequences leaked into piped output"; exit 1
fi
echo "ok: clean non-TTY output"

# 2) service is active
docker exec "$cid" systemctl is-active --quiet outhaul && echo "ok: service active" || { echo "FAIL: service not active"; exit 1; }

# 3) binary present
docker exec "$cid" test -x /usr/local/bin/outhaul && echo "ok: binary installed"

# 4) idempotent re-run succeeds
docker exec -i "$cid" sh -c 'printf "3\n\nn\nn\n" | sh /src/deploy/bootstrap.sh --from-checkout /src' >/dev/null && echo "ok: idempotent re-run"

echo "INTEGRATION PASS"
