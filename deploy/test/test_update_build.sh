#!/usr/bin/env sh
here=$(cd "$(dirname "$0")" && pwd)
. "$here/assert.sh"
OUTHAUL_INSTALLER_LIB=1 . "$here/../bootstrap.sh"

pkg=github.com/outhaul-dev/outhaul/internal/update
repo=https://github.com/o/r.git
base="-X $pkg.commit=abc -X $pkg.repo=$repo"

assert_eq "stamp" "$base" "$(build_ldflags abc "$repo" 0)"
assert_eq "local build is marked" "$base -X $pkg.local=true" "$(build_ldflags abc "$repo" 1)"

# build_binary hands the stamp to go build.
tmp=$(mktemp -d)
SRC_DIR=$tmp/src; mkdir "$SRC_DIR"
rec=$tmp/rec
# shellcheck disable=SC2034 # read by build_binary, sourced from bootstrap.sh
OUTHAUL_REPO=$repo
# shellcheck disable=SC2034 # read by build_binary
FROM_CHECKOUT=0
go() { printf '%s\n' "$*" > "$rec"; : > "$SRC_DIR/outhaul"; chmod +x "$SRC_DIR/outhaul"; }
git() { echo abc; }
LOGFILE=/dev/null build_binary >/dev/null
case $(cat "$rec") in
*"-ldflags $base"*) assert_eq "build_binary passes the stamp" ok ok ;;
*) assert_eq "build_binary passes the stamp" "*-ldflags $base*" "$(cat "$rec")" ;;
esac

# A checkout with uncommitted changes is stamped local; a clean one is not.
# shellcheck disable=SC2034 # read by build_binary
FROM_CHECKOUT=1
git() { case $* in *status*) echo ' M x' ;; *) echo abc ;; esac; }
LOGFILE=/dev/null build_binary >/dev/null
case $(cat "$rec") in
*"update.local=true"*) assert_eq "dirty checkout is local" ok ok ;;
*) assert_eq "dirty checkout is local" "*local=true*" "$(cat "$rec")" ;;
esac
git() { case $* in *status*) : ;; *) echo abc ;; esac; }
LOGFILE=/dev/null build_binary >/dev/null
case $(cat "$rec") in
*"update.local=true"*) assert_eq "clean checkout is not local" "no local" "$(cat "$rec")" ;;
*) assert_eq "clean checkout is not local" ok ok ;;
esac
rm -rf "$tmp"
[ "${TESTS_FAIL:-0}" -eq 0 ]
