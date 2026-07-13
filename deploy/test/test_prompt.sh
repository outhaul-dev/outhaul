#!/usr/bin/env sh
here=$(cd "$(dirname "$0")" && pwd)
. "$here/assert.sh"
OUTHAUL_INSTALLER_LIB=1 . "$here/../bootstrap.sh"

in=$(mktemp)

printf 'y\n' > "$in"; assert_ok   "explicit yes"        env OUTHAUL_INSTALLER_LIB=1 sh -c '. '"$here"'/../bootstrap.sh; ask_yes_no "?" n' 3<"$in" 2>/dev/null
printf 'n\n' > "$in"; assert_fail "explicit no"         env OUTHAUL_INSTALLER_LIB=1 sh -c '. '"$here"'/../bootstrap.sh; ask_yes_no "?" y' 3<"$in" 2>/dev/null
printf '\n'  > "$in"; assert_ok   "empty uses default y" env OUTHAUL_INSTALLER_LIB=1 sh -c '. '"$here"'/../bootstrap.sh; ask_yes_no "?" y' 3<"$in" 2>/dev/null

printf 'paas.example.dev\n' > "$in"
assert_eq "ask_value returns input" "paas.example.dev" "$(ask_value 'domain?' '' 3<"$in" 2>/dev/null)"
printf '\n' > "$in"
assert_eq "ask_value uses default"  "2222" "$(ask_value 'port?' '2222' 3<"$in" 2>/dev/null)"
rm -f "$in"

[ "${TESTS_FAIL:-0}" -eq 0 ]
