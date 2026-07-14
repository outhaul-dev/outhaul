#!/usr/bin/env sh
here=$(cd "$(dirname "$0")" && pwd)
. "$here/assert.sh"
OUTHAUL_INSTALLER_LIB=1 . "$here/../bootstrap.sh"

# With COLOR=0 nothing should contain an ESC (\033) byte.
# shellcheck disable=SC2034 # used by functions sourced from bootstrap.sh
COLOR=0 UNICODE=0 WIDTH=80
esc=$(printf '\033')
out=$(paint 255 0 0 "hello")
assert_eq "plain paint has no escapes" "hello" "$out"
case "$(step_ok 'built' 2>&1)" in *"$esc"*) echo "  FAIL step_ok emitted ESC in plain mode" >&2; TESTS_FAIL=$((TESTS_FAIL+1));; *) echo "  ok   step_ok plain";; esac
assert_contains "step_ok shows label" "$(step_ok 'built')" "built"

# With truecolor, paint wraps in an escape.
# shellcheck disable=SC2034 # used by functions sourced from bootstrap.sh
COLOR=3
case "$(paint 255 0 0 hi)" in *"$esc"*) echo "  ok   truecolor paint has ESC";; *) echo "  FAIL truecolor paint missing ESC" >&2; TESTS_FAIL=$((TESTS_FAIL+1));; esac

[ "${TESTS_FAIL:-0}" -eq 0 ]
