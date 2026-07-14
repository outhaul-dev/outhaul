# Minimal POSIX-sh assertion helpers. Source this in test_*.sh files.
# Tallies live in the caller's shell via these variables.
: "${TESTS_RUN:=0}" "${TESTS_FAIL:=0}"

_pass() { TESTS_RUN=$((TESTS_RUN+1)); printf '  ok   %s\n' "$1"; }
_fail() { TESTS_RUN=$((TESTS_RUN+1)); TESTS_FAIL=$((TESTS_FAIL+1)); printf '  FAIL %s\n' "$1" >&2; }

assert_eq() { # name expected actual
	if [ "$2" = "$3" ]; then _pass "$1"; else _fail "$1 (expected [$2], got [$3])"; fi
}
assert_contains() { # name haystack needle
	case "$2" in *"$3"*) _pass "$1";; *) _fail "$1 (missing [$3])";; esac
}
assert_ok() { # name; runs remaining args, expects exit 0
	n=$1; shift
	if "$@"; then _pass "$n"; else _fail "$n (exit $?)"; fi
}
assert_fail() { # name; runs remaining args, expects non-zero
	n=$1; shift
	if "$@"; then _fail "$n (expected non-zero)"; else _pass "$n"; fi
}
