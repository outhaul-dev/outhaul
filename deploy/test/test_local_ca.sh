#!/usr/bin/env sh
# choose_ingress mode 3: the local-CA prompt sets LOCAL_CA.
here=$(cd "$(dirname "$0")" && pwd)
. "$here/assert.sh"

in=$(mktemp)

printf '3\ny\n' > "$in"
out=$(OUTHAUL_INSTALLER_LIB=1 sh -c '. '"$here"'/../bootstrap.sh; choose_ingress; echo "MODE=$MODE LOCAL_CA=$LOCAL_CA"' 3<"$in" 2>/dev/null | tail -1)
assert_contains "mode 3 + yes enables local CA" "$out" "MODE=c LOCAL_CA=1"

printf '3\nn\n' > "$in"
out=$(OUTHAUL_INSTALLER_LIB=1 sh -c '. '"$here"'/../bootstrap.sh; choose_ingress; echo "MODE=$MODE LOCAL_CA=$LOCAL_CA"' 3<"$in" 2>/dev/null | tail -1)
assert_contains "mode 3 + no disables local CA" "$out" "MODE=c LOCAL_CA=0"

printf '2\n' > "$in"
out=$(OUTHAUL_INSTALLER_LIB=1 sh -c '. '"$here"'/../bootstrap.sh; choose_ingress; echo "LOCAL_CA=$LOCAL_CA"' 3<"$in" 2>/dev/null | tail -1)
assert_contains "tunnel mode leaves local CA off" "$out" "LOCAL_CA=0"

rm -f "$in"
[ "${TESTS_FAIL:-0}" -eq 0 ]
