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

[ "${TESTS_FAIL:-0}" -eq 0 ]
