#!/usr/bin/env sh
here=$(cd "$(dirname "$0")" && pwd)
. "$here/assert.sh"
OUTHAUL_INSTALLER_LIB=1 . "$here/../bootstrap.sh"

assert_eq "default admin url" "http://127.0.0.1:8080/" "$(admin_health_url '')"
assert_eq "custom listen addr" "http://127.0.0.1:9000/" "$(admin_health_url ':9000')"

log='Jul 13 12:00:00 host outhaul[1]: open: http://localhost:8080/setup?token=abc123XYZ_-'
assert_eq "extracts setup url" "http://localhost:8080/setup?token=abc123XYZ_-" "$(printf '%s\n' "$log" | extract_setup_url)"

assert_eq "admin port default" "8080" "$(admin_port '')"
assert_eq "admin port from addr" "9000" "$(admin_port '127.0.0.1:9000')"

# With a public URL, the binary prints that first and the localhost URL next.
log='Jul 13 12:00:00 host outhaul[1]:   open: https://paas.example.com/setup?token=abc
Jul 13 12:00:00 host outhaul[1]:   or, over an SSH tunnel to port 8080: http://localhost:8080/setup?token=abc'
assert_eq "extracts the public setup url" "https://paas.example.com/setup?token=abc" "$(printf '%s\n' "$log" | extract_setup_url)"
assert_eq "local setup url" "http://127.0.0.1:8080/setup?token=abc" "$(local_setup_url 'https://paas.example.com/setup?token=abc' 8080)"

assert_eq "url host" "paas.example.com" "$(url_host 'https://paas.example.com')"
assert_eq "url host drops port and path" "paas.example.com" "$(url_host 'https://paas.example.com:443/x')"

# The public check asks Traefik on this host for the admin UI's /healthz under
# the public name; Traefik's own 404 or 502 must not count.
# shellcheck disable=SC2034 # used by functions sourced from bootstrap.sh
COLOR=0
args=$(mktemp)
sleep() { :; }
curl() { printf '%s\n' "$*" > "$args"; return 0; }
assert_ok "public check passes" wait_public_healthy paas.example.com >/dev/null
assert_contains "asks Traefik on this host" "$(cat "$args")" "--resolve paas.example.com:443:127.0.0.1"
assert_contains "asks for the admin UI" "$(cat "$args")" "https://paas.example.com/healthz"
assert_contains "an error status fails" "$(cat "$args")" "-f"
curl() { return 22; }
assert_fail "public check fails while Traefik cannot answer" wait_public_healthy paas.example.com >/dev/null
unset -f curl sleep
rm -f "$args"

# A re-run restarts the service, which prints a new token; the journal for
# this boot still holds the old one, which no longer works.
log='Jul 13 12:00:00 host outhaul[1]:   open: http://localhost:8080/setup?token=OLD
Jul 13 12:05:00 host outhaul[2]:   open: https://paas.example.com/setup?token=NEW
Jul 13 12:05:00 host outhaul[2]:   or, over an SSH tunnel to port 8080: http://localhost:8080/setup?token=NEW'
assert_eq "re-run: the newest setup url" "https://paas.example.com/setup?token=NEW" "$(printf '%s\n' "$log" | extract_setup_url)"

[ "${TESTS_FAIL:-0}" -eq 0 ]
