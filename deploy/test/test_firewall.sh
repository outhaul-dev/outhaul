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

[ "${TESTS_FAIL:-0}" -eq 0 ]
