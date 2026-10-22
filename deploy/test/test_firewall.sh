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

[ "${TESTS_FAIL:-0}" -eq 0 ]
