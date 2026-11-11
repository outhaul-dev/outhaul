#!/usr/bin/env sh
# shellcheck disable=SC2218 # the test replaces git with a stub near its end; shellcheck 0.9 takes the earlier calls for it
here=$(cd "$(dirname "$0")" && pwd)
. "$here/assert.sh"
OUTHAUL_INSTALLER_LIB=1 . "$here/../bootstrap.sh"
# die() writes the log; keep it off the real install log (not writable here).
# shellcheck disable=SC2034 # read by log_line, sourced from bootstrap.sh
LOGFILE=/dev/null

# --ref consumes the next argument even when it is empty: an empty ref means
# the remote's HEAD, and only a missing argument is an error.
REF=; ARG_ERRORS=
parse_args --ref ""
assert_eq "--ref with an empty value: REF stays empty" "" "$REF"
assert_eq "--ref with an empty value: no error" "" "$ARG_ERRORS"

REF=; ARG_ERRORS=
parse_args --ref
assert_contains "--ref with no value: error" "$ARG_ERRORS" "--ref needs a value"

REF=; ARG_ERRORS=
parse_args --ref "" --ref v1
assert_eq "--ref twice: last value wins" "v1" "$REF"
assert_eq "--ref twice: no error" "" "$ARG_ERRORS"

T=$(mktemp -d)
git init -q "$T/repo"
git -C "$T/repo" config user.email t@example.com
git -C "$T/repo" config user.name t
echo one > "$T/repo/f"; git -C "$T/repo" add f; git -C "$T/repo" commit -qm one
C1=$(git -C "$T/repo" rev-parse HEAD)
echo two > "$T/repo/f"; git -C "$T/repo" commit -qam two
C2=$(git -C "$T/repo" rev-parse HEAD)
git -C "$T/repo" reset -q --hard "$C1"   # the refs below point at C1
git -C "$T/repo" tag light
git -C "$T/repo" tag -a annotated -m annotated
git -C "$T/repo" branch feature
git -C "$T/repo" tag feature            # a tag and a branch with one name

resolve_ref light "$T/repo"
assert_eq "lightweight tag: commit" "$C1" "$TARGET"
assert_eq "lightweight tag: version" "light" "$TARGET_VERSION"

resolve_ref annotated "$T/repo"
assert_eq "annotated tag: peeled commit" "$C1" "$TARGET"
assert_eq "annotated tag: version" "annotated" "$TARGET_VERSION"

resolve_ref feature "$T/repo"
assert_eq "tag beats branch" "feature" "$TARGET_VERSION"

resolve_ref "$C1" "$T/repo"
assert_eq "full commit: no network needed" "$C1" "$TARGET"
assert_eq "full commit: no version" "" "$TARGET_VERSION"

# A failure runs in a subshell: die exits it, not the test.
quiet() { ( "$@" ) >/dev/null 2>&1; }
refused() { ( resolve_ref "$@" ); }
short=$(printf '%s' "$C1" | cut -c1-8)
assert_fail "short commit refused" refused "$short" "$T/repo"
assert_fail "unknown name refused" refused nope "$T/repo"
# No ref means HEAD.
resolve_ref '' "$T/repo"
assert_eq "no ref: HEAD" "$C1" "$TARGET"

# Fetching by commit and by a resolved tag lands on the same commit.
# shellcheck disable=SC2034 # read by fetch_source, sourced from bootstrap.sh
OUTHAUL_REPO=$T/repo
fetch_source "$C1" >/dev/null 2>&1
assert_eq "fetch by commit" "$C1" "$(git -C "$SRC_DIR" rev-parse HEAD)"
resolve_ref light "$T/repo"
fetch_source "$TARGET" >/dev/null 2>&1
assert_eq "fetch by tag" "$C1" "$(git -C "$SRC_DIR" rev-parse HEAD)"
# The updater calls it with no argument: it resolves the remote's HEAD.
fetch_source >/dev/null 2>&1
assert_eq "fetch with no ref: HEAD" "$C1" "$(git -C "$SRC_DIR" rev-parse HEAD)"

# A fetch whose HEAD is not the commit asked for is refused.
git() {
	case "$*" in
	*"rev-parse HEAD"*) echo deadbeef ;;
	*) command git "$@" ;;
	esac
}
assert_fail "fetch: HEAD is not the target" quiet fetch_source "$C1"
unset -f git

assert_eq "stamp without a version" \
  "-X github.com/outhaul-dev/outhaul/internal/update.commit=abc -X github.com/outhaul-dev/outhaul/internal/update.repo=r" \
  "$(build_ldflags abc r 0)"
assert_eq "stamp with a version" \
  "-X github.com/outhaul-dev/outhaul/internal/update.commit=abc -X github.com/outhaul-dev/outhaul/internal/update.repo=r -X github.com/outhaul-dev/outhaul/internal/update.version=v1" \
  "$(build_ldflags abc r 0 v1)"

MARKER=$T/marker
write_marker "$C1" light light
assert_eq "marker commit" "$C1" "$(read_marker_commit)"
assert_contains "marker ref" "$(cat "$MARKER")" "ref=light"
assert_contains "marker version" "$(cat "$MARKER")" "version=light"
assert_contains "marker time" "$(cat "$MARKER")" "installed="

assert_ok   "no binary: build" needs_build '' "$C1" ''
assert_fail "same commit and marker: no build" needs_build "$C1" "$C1" "$C1"
assert_ok   "same commit, no marker: build" needs_build "$C1" "$C1" ''
assert_ok   "different commit: build" needs_build "$C1" "$C2" "$C1"

[ "${TESTS_FAIL:-0}" -eq 0 ]
