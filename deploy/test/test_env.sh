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

write_env_file "$dest" c '' '' off
assert_contains "ssh off is written" "$(cat "$dest")" "OUTHAUL_SSH_ADDR=off"
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

# --- install.env and the drop-ins -------------------------------------------
T=$(mktemp -d)
CALLS=$T/calls
: >"$CALLS"
systemctl() { echo "systemctl $*" >>"$CALLS"; }

# Every ingress writes its lines, in the design's order.
assert_eq "letsencrypt" \
	'# Outhaul unattended install answers — written by --non-interactive.
# Each --non-interactive run rewrites this file.
# Put anything else in /etc/outhaul.env or an --env-file.
OUTHAUL_PUBLIC_URL=https://o.example.com
OUTHAUL_ACME_EMAIL=ops@example.com
OUTHAUL_SSH_ADDR=:2222' "$(MODE=a PUBLIC_URL=https://o.example.com ACME_EMAIL=ops@example.com SSH_ADDR=:2222 install_env_content)"

assert_contains "local CA" "$(MODE=c LOCAL_CA=1 SSH_ADDR=off install_env_content)" "OUTHAUL_LOCAL_CA=true"
assert_contains "proxy HTTP off" "$(MODE=p PUBLIC_URL=https://o.example.com ACME_EMAIL=ops@example.com SSH_ADDR=off install_env_content)" "OUTHAUL_HTTP_PORT=off"
assert_contains "proxy admin direct off" "$(MODE=p PUBLIC_URL=https://o.example.com ACME_EMAIL=ops@example.com SSH_ADDR=off install_env_content)" "OUTHAUL_ADMIN_DIRECT=off"
assert_contains "updates line" "$(MODE=c SSH_ADDR=off UPDATES=off install_env_content)" "OUTHAUL_UPDATES=off"

# An unchanged file keeps its inode.
INSTALL_ENV=$T/install.env
NONINTERACTIVE=1 MODE=c SSH_ADDR=off write_install_env
before=$(ls -i "$INSTALL_ENV" | awk '{print $1}')
NONINTERACTIVE=1 MODE=c SSH_ADDR=off write_install_env
assert_eq "unchanged install.env keeps its inode" "$before" "$(ls -i "$INSTALL_ENV" | awk '{print $1}')"
assert_eq "install.env is 0600" 600 "$(stat -c '%a' "$INSTALL_ENV")"
# An interrupted run can leave the right content with the wrong mode: the next
# run corrects the mode rather than leaving the file readable.
chmod 0644 "$INSTALL_ENV"
NONINTERACTIVE=1 MODE=c SSH_ADDR=off write_install_env
assert_eq "the same content with the wrong mode is corrected" 600 "$(stat -c '%a' "$INSTALL_ENV")"

# The drop-in names the real path; write_dropins only reads INSTALL_ENV, and
# the file itself was written above through this test's own path.
INSTALL_ENV=/etc/outhaul/install.env
# The drop-in lists install.env first, then each env file in order.
SERVICE_DROPIN=$T/units/outhaul.service.d/10-install.conf
UPDATE_DROPIN=$T/units/outhaul-update.service.d/10-install.conf
# shellcheck disable=SC2034 # read by write_dropins, sourced from bootstrap.sh
ENV_FILES="/etc/outhaul/a.env
/etc/outhaul/b.env"
NONINTERACTIVE=1 write_dropins
body=$(cat "$SERVICE_DROPIN")
assert_contains "drop-in service section" "$body" "[Service]"
assert_contains "drop-in install.env" "$body" "EnvironmentFile=/etc/outhaul/install.env"
assert_contains "drop-in a" "$body" "EnvironmentFile=/etc/outhaul/a.env"
assert_contains "drop-in b" "$body" "EnvironmentFile=/etc/outhaul/b.env"
# [Service] is line 1, so the first file it loads is line 2.
assert_eq "drop-in order" "2" "$(printf '%s\n' "$body" | grep -n 'install.env' | cut -d: -f1)"
assert_eq "update drop-in matches" "$body" "$(cat "$UPDATE_DROPIN")"
assert_eq "drop-in is 0644" 644 "$(stat -c '%a' "$SERVICE_DROPIN")"

# A wizard re-run must not drop an earlier unattended run's env files.
SERVICE_DROPIN=$T/wizard/outhaul.service.d/10-install.conf
UPDATE_DROPIN=$T/wizard/outhaul-update.service.d/10-install.conf
mkdir -p "$(dirname "$SERVICE_DROPIN")" "$(dirname "$UPDATE_DROPIN")"
printf 'sentinel\n' >"$SERVICE_DROPIN"
printf 'sentinel\n' >"$UPDATE_DROPIN"
NONINTERACTIVE=0 ENV_FILES='' write_dropins
assert_eq "wizard run keeps the service drop-in" sentinel "$(cat "$SERVICE_DROPIN")"
assert_eq "wizard run keeps the update drop-in" sentinel "$(cat "$UPDATE_DROPIN")"

# A wizard run with --env-file rewrites both drop-ins, so it must keep an
# earlier unattended run's install.env first: its OUTHAUL_SSH_ADDR=off,
# OUTHAUL_UPDATES=off and proxy switches would be lost at the next restart.
INSTALL_ENV=$T/keep/install.env
mkdir -p "$(dirname "$INSTALL_ENV")"
printf 'OUTHAUL_UPDATES=off\n' >"$INSTALL_ENV"
: >"$T/keep/extra.env"
SERVICE_DROPIN=$T/keep/outhaul.service.d/10-install.conf
UPDATE_DROPIN=$T/keep/outhaul-update.service.d/10-install.conf
NONINTERACTIVE=0 ENV_FILES=$T/keep/extra.env write_dropins >/dev/null
assert_eq "a wizard --env-file run keeps install.env first" \
	"[Service]
EnvironmentFile=$INSTALL_ENV
EnvironmentFile=$T/keep/extra.env" "$(cat "$SERVICE_DROPIN")"
assert_eq "the update drop-in gets the same lines" "$(cat "$SERVICE_DROPIN")" "$(cat "$UPDATE_DROPIN")"

# Writing the files is all the writers do; the caller reloads and restarts.
assert_eq "the writers run no systemctl" 0 "$(grep -c '^systemctl' "$CALLS" || true)"

rm -rf "$T"

[ "${TESTS_FAIL:-0}" -eq 0 ]
