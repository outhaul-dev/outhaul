#!/usr/bin/env sh
here=$(cd "$(dirname "$0")" && pwd)
. "$here/assert.sh"
OUTHAUL_INSTALLER_LIB=1 . "$here/../bootstrap.sh"

assert_eq "default admin url" "http://127.0.0.1:8080/" "$(admin_health_url '')"
assert_eq "custom listen addr" "http://127.0.0.1:9000/" "$(admin_health_url ':9000')"

log='Jul 13 12:00:00 host outhaul[1]: open: http://localhost:8080/setup?token=abc123XYZ_-'
assert_eq "extracts setup url" "http://localhost:8080/setup?token=abc123XYZ_-" "$(printf '%s\n' "$log" | extract_setup_url)"

[ "${TESTS_FAIL:-0}" -eq 0 ]
