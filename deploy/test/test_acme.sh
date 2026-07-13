#!/usr/bin/env sh
here=$(cd "$(dirname "$0")" && pwd)
. "$here/assert.sh"
OUTHAUL_INSTALLER_LIB=1 . "$here/../bootstrap.sh"

# domain_ip_matches PUBLIC_IP "space separated resolved ips" -> 0 if match
assert_ok   "match when public ip in set" domain_ip_matches 203.0.113.7 "198.51.100.1 203.0.113.7"
assert_fail "no match"                    domain_ip_matches 203.0.113.7 "198.51.100.1 198.51.100.2"
assert_fail "empty resolved set"          domain_ip_matches 203.0.113.7 ""

[ "${TESTS_FAIL:-0}" -eq 0 ]
