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

# The git-port prompt defaults to 2222, so an empty answer can't mean
# "disable" — an explicit word must.
assert_eq "git port: empty keeps default" "2222" "$(normalize_git_port 2222)"
assert_eq "git port: custom port"         "2022" "$(normalize_git_port 2022)"
assert_eq "git port: none disables"       ""     "$(normalize_git_port none)"
assert_eq "git port: no disables"         ""     "$(normalize_git_port no)"
assert_eq "git port: 0 disables"          ""     "$(normalize_git_port 0)"
assert_eq "git port: None disables"       ""     "$(normalize_git_port None)"
assert_eq "git port: NO disables"         ""     "$(normalize_git_port NO)"
# outhaul serve refuses a malformed OUTHAUL_SSH_ADDR, so a typo must not reach
# the env file: it falls back to the default port.
assert_eq "git port: garbage keeps default"      "2222" "$(normalize_git_port abc 2>/dev/null)"
assert_eq "git port: out of range keeps default" "2222" "$(normalize_git_port 70000 2>/dev/null)"
assert_contains "git port: garbage is reported" "$(normalize_git_port abc 2>&1 >/dev/null)" "abc"

# outhaul serve refuses a public URL without a scheme; the installer adds
# https:// (mode a is the Let's Encrypt mode).
assert_eq "public url: bare host gets https" "https://paas.example.com" "$(normalize_public_url paas.example.com)"
assert_eq "public url: https kept"           "https://paas.example.com" "$(normalize_public_url https://paas.example.com)"
assert_eq "public url: http kept"            "http://10.0.0.5:8080"     "$(normalize_public_url http://10.0.0.5:8080)"
assert_eq "public url: empty stays empty"    ""                         "$(normalize_public_url '')"

[ "${TESTS_FAIL:-0}" -eq 0 ]
