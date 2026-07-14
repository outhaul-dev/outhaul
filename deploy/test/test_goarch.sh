#!/usr/bin/env sh
here=$(cd "$(dirname "$0")" && pwd)
. "$here/assert.sh"
OUTHAUL_INSTALLER_LIB=1 . "$here/../bootstrap.sh"

assert_eq "x86_64 -> amd64" "amd64" "$(go_dl_arch x86_64)"
assert_eq "amd64 -> amd64"  "amd64" "$(go_dl_arch amd64)"
assert_eq "aarch64 -> arm64" "arm64" "$(go_dl_arch aarch64)"
assert_eq "arm64 -> arm64"  "arm64" "$(go_dl_arch arm64)"

[ "${TESTS_FAIL:-0}" -eq 0 ]
