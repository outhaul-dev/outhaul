#!/usr/bin/env sh
# The firewall never opens the admin port, and tunnel / local-only installs
# publish no admin route through Traefik — so the completion screen must say
# how to reach the admin UI (an SSH tunnel) whenever the firewall was applied.
here=$(cd "$(dirname "$0")" && pwd)
. "$here/assert.sh"
OUTHAUL_INSTALLER_LIB=1 . "$here/../bootstrap.sh"
# shellcheck disable=SC2034 # used by functions sourced from bootstrap.sh
COLOR=0 UNICODE=0 WIDTH=80
hint='ssh -L 8080:127.0.0.1:8080'

assert_contains "tunnel + firewall: ssh hint"   "$(completion_screen b '' '22' '' 0 0)" "$hint"
assert_contains "local CA + firewall: ssh hint" "$(completion_screen c '' '22 80 443' '' 0 1)" "$hint"
case "$(completion_screen a 'https://paas.example.com' '22 80 443' '' 0 0)" in
	*"$hint"*) _fail "LE mode serves the admin over HTTPS: no ssh hint" ;;
	*) _pass "LE mode: no ssh hint" ;;
esac
case "$(completion_screen c '' '' '' 0 0)" in
	*"$hint"*) _fail "no firewall: :8080 is reachable, no ssh hint" ;;
	*) _pass "no firewall: no ssh hint" ;;
esac

setup='https://paas.example.com/setup?token=abc'
ok=$(completion_screen a 'https://paas.example.com' '22 80 443' "$setup" 0 0 0)
# The check runs on this server: it cannot see DNS or a cloud firewall, so
# say what it checked and always give the SSH-tunnel way in.
assert_contains "LE mode: says what was checked" "$ok" "checked from this server"
assert_contains "LE mode: ssh fallback even when the check passes" "$ok" "$hint"
assert_contains "LE mode: local setup url even when the check passes" "$ok" "http://127.0.0.1:8080/setup?token=abc"
bad=$(completion_screen a 'https://paas.example.com' '22 80 443' "$setup" 0 0 1)
assert_contains "LE mode, public route down: says so" "$bad" "admin UI not answering at https://paas.example.com"
assert_contains "LE mode, public route down: ssh hint" "$bad" "$hint"
assert_contains "LE mode, public route down: local setup url" "$bad" "http://127.0.0.1:8080/setup?token=abc"
assert_contains "LE mode, public route down: the ufw rule Traefik needs" "$bad" "ufw allow in on outhaul to any port 8080 proto tcp"
assert_contains "proxy mode: shown like Let's Encrypt" \
	"$(completion_screen p 'https://paas.example.com' '22 443' '' 0 0)" \
	"HTTPS: Let's Encrypt configured for https://paas.example.com"

# A rolled-back upgrade says the previous version is running again.
# shellcheck disable=SC2034 # read by completion_screen
ROLLED_BACK=1
rolled=$(completion_screen c '' '' '' 0 0)
assert_contains "rolled back: the screen says the old version is back" "$rolled" \
	"the new version did not answer; the previous version is running again"
# shellcheck disable=SC2034 # read by completion_screen
ROLLED_BACK=0

# An unattended run points at the setup file only while it is there: a run
# whose start printed no URL removes it, and the screen must not name it.
T=$(mktemp -d)
: >"$T/setup"
# shellcheck disable=SC2034 # read by completion_screen and log_event
NONINTERACTIVE=1
# shellcheck disable=SC2034 # read by log_event, sourced from bootstrap.sh
LOGFILE=/dev/null
# shellcheck disable=SC2034 # read by completion_screen
SETUP_URL_FILE=$T/setup
assert_contains "an existing setup file is named" "$(completion_screen c '' '' '' 0 0)" \
	"the one-time setup URL is in $T/setup"
rm -f "$SETUP_URL_FILE"
case "$(completion_screen c '' '' '' 0 0)" in
	*"the one-time setup URL is in"*) _fail "a removed setup file is not named" ;;
	*) _pass "a removed setup file is not named" ;;
esac
rm -rf "$T"

[ "${TESTS_FAIL:-0}" -eq 0 ]
