#!/usr/bin/env sh
# shellcheck disable=SC2034 # the sourced bootstrap.sh reads these globals
here=$(cd "$(dirname "$0")" && pwd)
. "$here/assert.sh"
OUTHAUL_INSTALLER_LIB=1 . "$here/../bootstrap.sh"

T=$(mktemp -d)
CALLS=$T/calls
: > "$CALLS"
sleep() { :; }
quiet() { ( "$@" ) >/dev/null 2>&1; }
docker() { echo "docker $*" >> "$CALLS"; case "$*" in
  *"version --format"*) [ "${DOCKER_OK:-1}" = 1 ] ;;
  *"compose version"*) [ "${COMPOSE_OK:-1}" = 1 ] ;;
esac; }
systemctl() { echo "systemctl $*" >> "$CALLS"; }
# A file, not a shell variable: curl runs in a pipeline's subshell, so an
# assignment inside it would not reach the parent.
DOCKER_FLAG=$T/docker-installed
have_docker() { [ -e "$DOCKER_FLAG" ]; }
curl() {
	echo "curl $*" >> "$CALLS"
	case "$*" in *get.docker.com*) : > "$DOCKER_FLAG" ;; esac
}
LOGFILE=/dev/null
have_docker_service() { return 1; }
have_docker_socket() { return 1; }

# Working: used, nothing installed, enabled or changed.
: > "$DOCKER_FLAG"
: > "$CALLS"
assert_ok "working Docker is used" ensure_docker >/dev/null
assert_eq "working: no get.docker.com" 0 "$(grep -c 'get.docker.com' "$CALLS" || true)"
assert_eq "working: no systemctl" 0 "$(grep -c '^systemctl' "$CALLS" || true)"

# Absent, non-interactive, no --install-docker: fail before any install.
rm -f "$DOCKER_FLAG"
NONINTERACTIVE=1 INSTALL_DOCKER=0
: > "$CALLS"
assert_fail "absent: non-interactive fails" quiet ensure_docker
assert_eq "absent: no get.docker.com" 0 "$(grep -c 'get.docker.com' "$CALLS" || true)"

# Absent, --install-docker: get.docker.com runs, then Docker works.
INSTALL_DOCKER=1
DOCKER_OK=1 COMPOSE_OK=1
: > "$CALLS"
assert_ok "absent: --install-docker installs" ensure_docker >/dev/null
assert_eq "absent: get.docker.com once" 1 "$(grep -c 'get.docker.com' "$CALLS")"
assert_ok "absent: docker now exists" [ -e "$DOCKER_FLAG" ]

# Absent, the wizard: get.docker.com runs too.
rm -f "$DOCKER_FLAG"
NONINTERACTIVE=0 INSTALL_DOCKER=0
: > "$CALLS"
assert_ok "absent: the wizard installs" ensure_docker >/dev/null
assert_eq "absent: the wizard runs get.docker.com" 1 "$(grep -c 'get.docker.com' "$CALLS")"

# Present but not working: enable once, wait, fail, never get.docker.com.
: > "$DOCKER_FLAG"
have_docker_service() { return 0; }
DOCKER_OK=0
: > "$CALLS"
assert_fail "broken: fails" quiet ensure_docker
assert_eq "broken: enable --now once" 1 "$(grep -c '^systemctl enable --now docker$' "$CALLS")"
assert_eq "broken: no get.docker.com" 0 "$(grep -c 'get.docker.com' "$CALLS" || true)"

# Compose missing: the message names it.
DOCKER_OK=1 COMPOSE_OK=0
out=$(ensure_docker 2>&1) || true
assert_contains "broken: names compose" "$out" "docker compose v2 plugin is missing"

rm -rf "$T"
[ "${TESTS_FAIL:-0}" -eq 0 ]
