#!/usr/bin/env sh
# Headless runs (cloud-init, CI, `ssh host 'curl … | sh'`) have no controlling
# terminal. The installer must survive that and take prompt defaults (or read
# answers from stdin when it was started from a file).
here=$(cd "$(dirname "$0")" && pwd)
. "$here/assert.sh"
OUTHAUL_INSTALLER_LIB=1 . "$here/../bootstrap.sh"
lib="$here/../bootstrap.sh"

# Run a command detached from any controlling terminal when setsid can wait
# for it (CI runners have no terminal to begin with).
detached() { if setsid -w true 2>/dev/null; then setsid -w "$@"; else "$@"; fi; }
export OUTHAUL_INSTALLER_LIB=1

assert_eq "prompt source: tty wins"            tty   "$(prompt_source 1 "$lib")"
assert_eq "prompt source: file script → stdin" stdin "$(prompt_source 0 "$lib")"
assert_eq "prompt source: piped script → null" null  "$(prompt_source 0 sh)"
assert_eq "prompt source: no script → null"    null  "$(prompt_source 0 '')"

# Opening /dev/tty with no controlling terminal must not kill the shell.
out=$(detached sh -c '. "$1"; open_prompt_fd sh; ask_value "q?" dflt; echo alive' _ "$lib" </dev/null 2>/dev/null)
assert_contains "no tty: shell survives"        "$out" "alive"
assert_contains "no tty: prompt takes default"  "$out" "dflt"

# Started from a file with answers on stdin: prompts read them.
out=$(printf 'answer\n' | detached sh -c '. "$1"; open_prompt_fd "$1"; ask_value "q?" dflt' _ "$lib" 2>/dev/null)
assert_eq "no tty, script file: answers come from stdin" "answer" "$out"

[ "${TESTS_FAIL:-0}" -eq 0 ]
