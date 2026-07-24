#!/usr/bin/env sh
here=$(cd "$(dirname "$0")" && pwd)
. "$here/assert.sh"
OUTHAUL_INSTALLER_LIB=1 . "$here/../bootstrap.sh"

good=1153d3d50e0ac764b447adfe05c2bcf08e889d42a02e0fe0259bd47f6733ad7f

assert_ok   "accepts a real digest"      is_sha256 "$good"
assert_ok   "accepts uppercase hex"      is_sha256 "$(printf '%s' "$good" | tr a-f A-F)"

assert_fail "rejects empty"              is_sha256 ""
assert_fail "rejects short"              is_sha256 "1153d3d5"
assert_fail "rejects long"               is_sha256 "${good}00"
assert_fail "rejects non-hex chars"      is_sha256 "z153d3d50e0ac764b447adfe05c2bcf08e889d42a02e0fe0259bd47f6733ad7f"
# The actual regression: go.dev/dl/<file>.sha256 answers 200 with an HTML stub.
assert_fail "rejects html stub"          is_sha256 "<!DOCTYPEhtml><html><head>"

[ "${TESTS_FAIL:-0}" -eq 0 ]
