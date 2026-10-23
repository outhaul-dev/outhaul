#!/usr/bin/env sh
here=$(cd "$(dirname "$0")" && pwd)
. "$here/assert.sh"
OUTHAUL_INSTALLER_LIB=1 . "$here/../bootstrap.sh"

# args: mode(a/b/c) gitport
assert_eq "mode a + git 2222" "22 80 443 2222" "$(derive_firewall_ports a 2222)"
assert_eq "mode b no ports"   "22"             "$(derive_firewall_ports b '')"
assert_eq "mode c + git 2222" "22 2222"        "$(derive_firewall_ports c 2222)"
assert_eq "mode a no git"     "22 80 443"      "$(derive_firewall_ports a '')"
assert_eq "dedup git=22"      "22 80 443"      "$(derive_firewall_ports a 22)"

assert_eq "mode c + local CA opens web ports" "22 80 443" "$(derive_firewall_ports c '' 1)"
assert_eq "mode c without local CA stays closed" "22" "$(derive_firewall_ports c '' 0)"
assert_eq "mode a ignores localca arg" "22 80 443" "$(derive_firewall_ports a '' 1)"

# Listening TCP ports that enabling ufw would block: from `ss -Hltn` output,
# loopback-bound sockets excluded, deduped across v4/v6, sorted.
ss_out='LISTEN 0 128 0.0.0.0:22 0.0.0.0:*
LISTEN 0 128 [::]:22 [::]:*
LISTEN 0 4096 127.0.0.53%lo:53 0.0.0.0:*
LISTEN 0 4096 127.0.0.1:5432 0.0.0.0:*
LISTEN 0 511 *:3000 *:*
LISTEN 0 4096 [::1]:6379 [::]:*
LISTEN 0 4096 10.0.0.5%eth0:9100 0.0.0.0:*'
assert_eq "non-loopback listening ports, deduped and sorted" "22 3000 9100" \
	"$(printf '%s\n' "$ss_out" | ss_listen_ports)"
assert_eq "header line is ignored" "22" \
	"$(printf 'State Recv-Q Send-Q Local Address:Port Peer Address:Port\nLISTEN 0 128 0.0.0.0:22 0.0.0.0:*\n' | ss_listen_ports)"
assert_eq "no listeners" "" "$(printf '' | ss_listen_ports)"

assert_eq "ports the firewall would block" "3000 9100" "$(ports_not_in "22 3000 9100" "22 80 443")"
assert_eq "nothing blocked" "" "$(ports_not_in "22" "22 80 443")"
assert_eq "no listeners, nothing blocked" "" "$(ports_not_in "" "22 80 443")"

# Enabling ufw blocks the admin port on purpose: it is not one to warn about.
assert_eq "admin port is not 'would block'" "3000" "$(ufw_would_block "22 3000 8080" "22 80 443" 8080)"

# The interface Traefik's traffic to the admin port arrives on, from
# `docker network inspect -f '{{.Id}} {{index .Options "com.docker.network.bridge.name"}}'`.
id=0123456789abcdef0123456789abcdef
assert_eq "named bridge" "outhaul" "$(printf '%s outhaul\n' "$id" | bridge_from_inspect outhaul)"
assert_eq "older network: Docker's br-<id>" "br-0123456789ab" "$(printf '%s <no value>\n' "$id" | bridge_from_inspect outhaul)"
assert_eq "empty bridge option" "br-0123456789ab" "$(printf '%s \n' "$id" | bridge_from_inspect outhaul)"
assert_eq "no network yet: the name Outhaul gives it" "outhaul" "$(printf '' | bridge_from_inspect outhaul)"

# apply_firewall lets the bridge reach the admin port, before ufw is enabled:
# Traefik serves the admin UI from there, and ufw drops it otherwise.
calls=$(mktemp)
ufw() { printf '%s\n' "$*" >> "$calls"; }
LOGFILE=/dev/null apply_firewall "22 80 443" outhaul 8080 >/dev/null
rule=$(grep -n '^allow in on outhaul to any port 8080 proto tcp$' "$calls" | cut -d: -f1)
enable=$(grep -n '^--force enable$' "$calls" | cut -d: -f1)
if [ -n "$rule" ] && [ -n "$enable" ] && [ "$rule" -lt "$enable" ]; then
	_pass "bridge may reach the admin port, before enable"
else
	_fail "bridge may reach the admin port, before enable (ufw calls: $(paste -sd'|' "$calls"))"
fi
rm -f "$calls"
unset -f ufw

# Ports Docker publishes (Traefik's 80/443) bypass ufw, so they are not ones
# enabling it would block. `ss -ltnp` names the docker-proxy behind them.
ssp_out='LISTEN 0 4096 0.0.0.0:80 0.0.0.0:* users:(("docker-proxy",pid=812,fd=7))
LISTEN 0 4096 [::]:80 [::]:* users:(("docker-proxy",pid=820,fd=7))
LISTEN 0 4096 *:8080 *:* users:(("outhaul",pid=700,fd=9))
LISTEN 0 511 0.0.0.0:3000 0.0.0.0:* users:(("node",pid=900,fd=20))'
assert_eq "docker-published ports excluded" "3000 8080" "$(printf '%s\n' "$ssp_out" | ss_listen_ports)"

[ "${TESTS_FAIL:-0}" -eq 0 ]
