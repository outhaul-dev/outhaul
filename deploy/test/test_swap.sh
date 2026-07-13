#!/usr/bin/env sh
here=$(cd "$(dirname "$0")" && pwd)
. "$here/assert.sh"
OUTHAUL_INSTALLER_LIB=1 . "$here/../bootstrap.sh"

# need_swap mem_kb swap_kb  -> exit 0 means "create swap"
assert_ok   "1GB no swap needs swap"   need_swap 1048576 0
assert_fail "4GB no swap is fine"      need_swap 4194304 0
assert_fail "1GB + 2GB swap is fine"   need_swap 1048576 2097152
assert_ok   "exactly under 2GiB"       need_swap 2097151 0

[ "${TESTS_FAIL:-0}" -eq 0 ]
