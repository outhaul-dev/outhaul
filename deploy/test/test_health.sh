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
assert_contains "asks for the admin UI" "$(cat "$args")" "https://paas.example.com:443/healthz"
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

# The running service's environment is what the health checks must use: a
# port set in an env file is not in the installer's own environment.
T=$(mktemp -d)
# shellcheck disable=SC2034 # a seam for a test that records calls
CALLS=$T/calls
PROC_ROOT=$T/proc
mkdir -p "$PROC_ROOT/123"
printf 'OUTHAUL_HTTPS_PORT=8443\0OUTHAUL_REQUIRE_HEADER_NAME=X-Test\0OUTHAUL_REQUIRE_HEADER_VALUE=s3cret\0' > "$PROC_ROOT/123/environ"
systemctl() { case "$*" in *MainPID*) echo 123 ;; esac; }
assert_eq "service env: the running service wins" 8443 "$(service_env OUTHAUL_HTTPS_PORT 443)"
assert_eq "service env: header name" X-Test "$(service_env OUTHAUL_REQUIRE_HEADER_NAME '')"
assert_eq "service env: default" :8080 "$(service_env OUTHAUL_LISTEN_ADDR :8080)"
systemctl() { echo 0; }
# shellcheck disable=SC2034 # read by service_env through eval
OUTHAUL_HTTPS_PORT=9443
assert_eq "service env: the installer's own environment next" 9443 "$(service_env OUTHAUL_HTTPS_PORT 443)"
unset OUTHAUL_HTTPS_PORT
assert_eq "service env: default last" 443 "$(service_env OUTHAUL_HTTPS_PORT 443)"

# The header reaches curl on stdin, never in an argument, and the public
# check asks the service's own HTTPS port.
args=$(mktemp); input=$(mktemp)
curl() { printf '%s\n' "$*" > "$args"; cat > "$input"; return 0; }
sleep() { :; }
assert_ok "public check with a header" wait_public_healthy o.example.com 443 X-Test s3cret >/dev/null
assert_contains "header not in argv" "$(cat "$args")" "-K -"
assert_eq "header value not in argv" 0 "$(grep -c s3cret "$args" || true)"
assert_contains "header on stdin" "$(cat "$input")" 'header = "X-Test: s3cret"'
assert_contains "the service's HTTPS port" "$(cat "$args")" "--resolve o.example.com:443:127.0.0.1"

have_ss() { return 0; }
ss() { printf 'LISTEN 0 128 0.0.0.0:8080 0.0.0.0:*\n'; }
docker() { printf '80/tcp -> 0.0.0.0:80\n'; }
assert_fail "proxy: wildcard admin port fails" proxy_effective 8080
ss() { printf 'LISTEN 0 128 127.0.0.1:8080 0.0.0.0:*\nLISTEN 0 128 172.17.0.1:8080 0.0.0.0:*\n'; }
assert_fail "proxy: published 80 fails" proxy_effective 8080
docker() { printf '443/tcp -> 0.0.0.0:443\n'; }
assert_ok "proxy: loopback and bridge only, no 80" proxy_effective 8080
have_ss() { return 1; }
assert_ok "proxy: no ss warns and passes" proxy_effective 8080

# A failed proxy check acts only on what this run installed or started: an
# upgrade it performed is rolled back, a first install's new service is
# stopped, a no-build run stops a service it started from stopped, and a
# service it found serving is left up.
assert_eq "proxy failure: an upgrade rolls back" rollback "$(proxy_failure_action 1 1 1 0)"
assert_eq "proxy failure: a first install stops" stop "$(proxy_failure_action 1 0 1 0)"
assert_eq "proxy failure: a no-build run that started a stopped service stops" stop "$(proxy_failure_action 0 1 1 0)"
assert_eq "proxy failure: a no-build run leaves a serving service" none "$(proxy_failure_action 0 1 1 1)"
assert_eq "proxy failure: a no-build run that changed nothing leaves it" none "$(proxy_failure_action 0 1 0 1)"
assert_eq "proxy failure: no build and no binary stops" stop "$(proxy_failure_action 0 0 0 0)"

# The header is optional.
curl() { printf '%s\n' "$*" > "$args"; return 0; }
assert_ok "public check without a header" wait_public_healthy o.example.com 443 '' '' >/dev/null
assert_eq "no header: no -K" 0 "$(grep -c -- '-K -' "$args" || true)"

rm -rf "$T"

[ "${TESTS_FAIL:-0}" -eq 0 ]
