#!/usr/bin/env sh
here=$(cd "$(dirname "$0")" && pwd)
. "$here/assert.sh"
OUTHAUL_INSTALLER_LIB=1 . "$here/../bootstrap.sh"

os=$(mktemp)
printf 'ID=debian\nVERSION_ID="13"\nPRETTY_NAME="Debian GNU/Linux 13"\n' > "$os"
assert_eq "distro id from os-release" "debian" "$(detect_distro "$os")"
printf 'ID=ubuntu\nID_LIKE=debian\nVERSION_ID="24.04"\n' > "$os"
assert_eq "ubuntu id from os-release" "ubuntu" "$(detect_distro "$os")"
rm -f "$os"

assert_ok   "debian is supported"   distro_supported debian
assert_ok   "ubuntu is supported"   distro_supported ubuntu
assert_fail "fedora is unsupported" distro_supported fedora
assert_fail "empty id is unsupported" distro_supported ""

mi=$(mktemp)
printf 'MemTotal:  1017880 kB\nSwapTotal:   524284 kB\n' > "$mi"
assert_eq "mem parsed"  "1017880" "$(read_meminfo_kb MemTotal "$mi")"
assert_eq "swap parsed" "524284"  "$(read_meminfo_kb SwapTotal "$mi")"
rm -f "$mi"

# An unsupported distro is a hard stop unattended, and a question in the
# wizard. preflight's root and systemd checks are stubbed: this file runs as a
# normal user.
T=$(mktemp -d)
id() { printf '0\n'; }
systemctl() { :; }
detect_distro() { printf 'fedora\n'; }
# shellcheck disable=SC2034 # read by preflight's die and step lines
LOGFILE=/dev/null
if ( NONINTERACTIVE=1; preflight ) >/dev/null 2>&1; then _rc=0; else _rc=$?; fi
assert_eq "unsupported distro: an unattended run exits 1" 1 "$_rc"

ask_yes_no() { printf '%s\n' "$1" >"$T/continued"; return 0; }
NONINTERACTIVE=0
preflight >/dev/null 2>&1
assert_contains "unsupported distro: the wizard asks" "$(cat "$T/continued")" "Continue anyway"

# A DNS mismatch is a warning unattended: it goes on without asking.
host_public_ip() { printf '203.0.113.1\n'; }
resolve_a() { printf '198.51.100.1\n'; }
port_in_use() { return 1; }
: >"$T/acme_asked"
ask_yes_no() { printf '%s\n' "$1" >>"$T/acme_asked"; return 1; }
if ( NONINTERACTIVE=1; acme_preflight o.example.com ) >/dev/null 2>&1; then _rc=0; else _rc=$?; fi
assert_eq "acme mismatch: an unattended run continues" 0 "$_rc"
assert_eq "acme mismatch: it does not prompt" "" "$(cat "$T/acme_asked")"
# shellcheck disable=SC2034 # read by acme_preflight, not by this line
( NONINTERACTIVE=0; acme_preflight o.example.com ) >/dev/null 2>&1 || true
assert_contains "acme mismatch: the wizard asks" "$(cat "$T/acme_asked")" "Continue with Let's Encrypt anyway"
# proxy mode's DNS points at the proxy, so main skips this check entirely;
# test_noninteractive.sh scenario 11 proves that end to end.
rm -rf "$T"

[ "${TESTS_FAIL:-0}" -eq 0 ]
