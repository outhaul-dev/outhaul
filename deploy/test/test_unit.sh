#!/usr/bin/env sh
# The unit file must start after Docker and after the network is up, because
# Docker owns app restarts: Outhaul reconciles Traefik and cloudflared at boot
# and starts no app itself.
here=$(cd "$(dirname "$0")" && pwd)
. "$here/assert.sh"

unit="$here/../outhaul.service"
after=" $(sed -n 's/^After=//p' "$unit") "
wants=" $(sed -n 's/^Wants=//p' "$unit") "

assert_contains "After: docker.service" "$after" " docker.service "
assert_contains "After: network-online.target" "$after" " network-online.target "
assert_contains "Wants: docker.service" "$wants" " docker.service "
assert_contains "Wants: network-online.target" "$wants" " network-online.target "

[ "${TESTS_FAIL:-0}" -eq 0 ]
