#!/usr/bin/env sh
here=$(cd "$(dirname "$0")" && pwd)
. "$here/assert.sh"
OUTHAUL_INSTALLER_LIB=1 . "$here/../bootstrap.sh"

dest=$(mktemp)
# write_env_file dest mode url email sshaddr
write_env_file "$dest" a https://paas.dev me@x.com :2222
body=$(cat "$dest")
assert_contains "mode a writes url"   "$body" "OUTHAUL_PUBLIC_URL=https://paas.dev"
assert_contains "mode a writes email" "$body" "OUTHAUL_ACME_EMAIL=me@x.com"
assert_contains "writes ssh addr"     "$body" "OUTHAUL_SSH_ADDR=:2222"
assert_eq       "file is 0600"        "600" "$(stat -c '%a' "$dest")"

write_env_file "$dest" c '' '' :2222
body=$(cat "$dest")
case "$body" in *OUTHAUL_ACME_EMAIL*) echo "  FAIL mode c leaked email" >&2; TESTS_FAIL=$((TESTS_FAIL+1));; *) echo "  ok   mode c has no email";; esac
assert_contains "mode c keeps ssh"    "$body" "OUTHAUL_SSH_ADDR=:2222"
rm -f "$dest"

dest2=$(mktemp)
# write_env_file dest mode url email sshaddr localca
write_env_file "$dest2" c '' '' :2222 1
body=$(cat "$dest2")
assert_contains "mode c + local CA writes flag" "$body" "OUTHAUL_LOCAL_CA=true"
write_env_file "$dest2" c '' '' :2222 0
body=$(cat "$dest2")
case "$body" in *OUTHAUL_LOCAL_CA*) echo "  FAIL local CA off leaked flag" >&2; TESTS_FAIL=$((TESTS_FAIL+1));; *) echo "  ok   local CA off writes no flag";; esac
write_env_file "$dest2" a https://paas.dev me@x.com :2222 0
body=$(cat "$dest2")
case "$body" in *OUTHAUL_LOCAL_CA*) echo "  FAIL mode a leaked local CA flag" >&2; TESTS_FAIL=$((TESTS_FAIL+1));; *) echo "  ok   mode a has no local CA flag";; esac
rm -f "$dest2"

[ "${TESTS_FAIL:-0}" -eq 0 ]
