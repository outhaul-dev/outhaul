#!/usr/bin/env sh
here=$(cd "$(dirname "$0")" && pwd)
. "$here/assert.sh"
OUTHAUL_INSTALLER_LIB=1 . "$here/../bootstrap.sh"

tmp=$(mktemp)
printf 'module x\n\ngo 1.26.4\n' > "$tmp"
assert_eq "reads go directive" "1.26.4" "$(go_version_from_gomod "$tmp")"

printf 'module x\n\ngo 1.26\n\ntoolchain go1.26.4\n' > "$tmp"
assert_eq "prefers toolchain" "1.26.4" "$(go_version_from_gomod "$tmp")"
rm -f "$tmp"

[ "${TESTS_FAIL:-0}" -eq 0 ]
