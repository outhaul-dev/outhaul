#!/usr/bin/env sh
here=$(cd "$(dirname "$0")" && pwd)
. "$here/assert.sh"
OUTHAUL_INSTALLER_LIB=1 . "$here/../bootstrap.sh"

assert_ok "main is defined" command -v main >/dev/null

[ "${TESTS_FAIL:-0}" -eq 0 ]
