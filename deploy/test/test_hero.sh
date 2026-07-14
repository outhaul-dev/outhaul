#!/usr/bin/env sh
here=$(cd "$(dirname "$0")" && pwd)
. "$here/assert.sh"
OUTHAUL_INSTALLER_LIB=1 . "$here/../bootstrap.sh"

# shellcheck disable=SC2034 # used by functions sourced from bootstrap.sh
COLOR=0 UNICODE=0 WIDTH=100
assert_contains "wide hero mentions OUTHAUL/tagline" "$(hero)" "self-hosted PaaS"

# shellcheck disable=SC2034 # used by functions sourced from bootstrap.sh
WIDTH=40
assert_contains "narrow hero degrades to one line" "$(hero)" "Outhaul"

# progress_bar renders a percentage
# shellcheck disable=SC2034 # used by functions sourced from bootstrap.sh
COLOR=0 UNICODE=0 WIDTH=80
assert_contains "progress shows percent" "$(progress_bar 50 'build')" "50%"

[ "${TESTS_FAIL:-0}" -eq 0 ]
