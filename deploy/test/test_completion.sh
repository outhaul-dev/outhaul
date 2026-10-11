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

[ "${TESTS_FAIL:-0}" -eq 0 ]
