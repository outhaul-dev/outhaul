#!/usr/bin/env sh
# install_updater: puts the updater and its units in place. chown and
# systemctl are recording stubs; every path points into a temp tree.
# shellcheck disable=SC2218 # a later test stands in for install_updater; shellcheck 0.9 takes the earlier calls for that
here=$(cd "$(dirname "$0")" && pwd)
. "$here/assert.sh"
OUTHAUL_INSTALLER_LIB=1 . "$here/../bootstrap.sh"

ROOT=$(mktemp -d)
LIB_DIR=$ROOT/lib
UNIT_DIR=$ROOT/units
UPDATE_DIR=$ROOT/update
WORK_DIR=$ROOT/work
CALLS=$ROOT/calls
: > "$CALLS"
# chown records the directory it ran in: install_updater chowns ".".
chown() { echo "chown $* in $(pwd -P)" >> "$CALLS"; }
systemctl() { echo "systemctl $*" >> "$CALLS"; }
# The tests cannot give files to the outhaul group: chgrp only records.
chgrp() { echo "chgrp $*" >> "$ROOT/chgrp_calls"; }
inode() { ls -i "$1" | awk '{ print $1 }'; }
differs() { if [ "$1" != "$2" ]; then echo yes; else echo no; fi; }
is_file() { [ -f "$1" ] && [ ! -L "$1" ]; }
quiet() { "$@" >/dev/null 2>&1; }

SRC=$ROOT/src
mkdir -p "$SRC/deploy"
for f in update.sh bootstrap.sh outhaul-update.path outhaul-update.service; do
	echo "v1 $f" > "$SRC/deploy/$f"
done
chmod 0600 "$SRC/deploy/update.sh" # modes come from the installer, not the source

# ----------------------------------------------------------- install_atomic --

echo one > "$ROOT/a"
install_atomic "$ROOT/a" "$ROOT/b" 0755
assert_eq "install_atomic: content" one "$(cat "$ROOT/b")"
assert_eq "install_atomic: mode" 755 "$(stat -c %a "$ROOT/b")"
assert_eq "install_atomic: no temp file left" no "$(if [ -e "$ROOT/b.new" ]; then echo yes; else echo no; fi)"
before=$(inode "$ROOT/b")
echo two > "$ROOT/a"
install_atomic "$ROOT/a" "$ROOT/b" 0644
assert_eq "install_atomic: replaced" two "$(cat "$ROOT/b")"
assert_eq "install_atomic: new inode" yes "$(differs "$before" "$(inode "$ROOT/b")")"

# ----------------------------------------------------------- install_updater --

install_updater "$SRC" >/dev/null
assert_eq "update.sh mode" 755 "$(stat -c %a "$LIB_DIR/update.sh")"
assert_eq "bootstrap.sh mode" 644 "$(stat -c %a "$LIB_DIR/bootstrap.sh")"
assert_eq "path unit mode" 644 "$(stat -c %a "$UNIT_DIR/outhaul-update.path")"
assert_eq "service unit mode" 644 "$(stat -c %a "$UNIT_DIR/outhaul-update.service")"
assert_eq "update.sh content" "v1 update.sh" "$(cat "$LIB_DIR/update.sh")"
assert_eq "bootstrap.sh content" "v1 bootstrap.sh" "$(cat "$LIB_DIR/bootstrap.sh")"
assert_eq "service unit content" "v1 outhaul-update.service" "$(cat "$UNIT_DIR/outhaul-update.service")"
assert_eq "update dir mode" 700 "$(stat -c %a "$UPDATE_DIR")"
assert_eq "the updater's own directory: root's, outhaul may only open the log" "710 chgrp outhaul $WORK_DIR" "$(stat -c %a "$WORK_DIR") $(head -1 "$ROOT/chgrp_calls")"
assert_eq "marker" 1 "$(cat "$UPDATE_DIR/updater")"
assert_eq "marker readable by outhaul" 644 "$(stat -c %a "$UPDATE_DIR/updater")"
assert_eq "no temp files left" "updater|" "$(ls -A "$UPDATE_DIR")|$(ls -A "$WORK_DIR")"
assert_eq "chown (of the update dir itself, not a link) and systemctl, in order" \
	"chown -h outhaul:outhaul . in $UPDATE_DIR|systemctl daemon-reload|systemctl enable --now outhaul-update.path" \
	"$(paste -sd'|' "$CALLS")"

# A second run replaces the files by rename: a running update.sh is not
# written to in place.
before=$(inode "$LIB_DIR/update.sh")
bbefore=$(inode "$LIB_DIR/bootstrap.sh")
echo "v2 update.sh" > "$SRC/deploy/update.sh"
: > "$CALLS"
install_updater "$SRC" >/dev/null
assert_eq "second run: new content" "v2 update.sh" "$(cat "$LIB_DIR/update.sh")"
assert_eq "second run: update.sh has a new inode" yes "$(differs "$before" "$(inode "$LIB_DIR/update.sh")")"
assert_eq "second run: bootstrap.sh has a new inode" yes "$(differs "$bbefore" "$(inode "$LIB_DIR/bootstrap.sh")")"
assert_eq "second run: mode kept" 755 "$(stat -c %a "$LIB_DIR/update.sh")"
assert_eq "second run: marker" 1 "$(cat "$UPDATE_DIR/updater")"

# The update directory is in outhaul's data dir. What is left there is
# replaced or refused, never written through.
echo 'unchanged' > "$ROOT/target"
rm "$UPDATE_DIR/updater"; ln -s "$ROOT/target" "$UPDATE_DIR/updater"
install_updater "$SRC" >/dev/null
assert_eq "a link at the marker: not written through" "unchanged" "$(cat "$ROOT/target")"
assert_ok "a link at the marker: replaced by a file" is_file "$UPDATE_DIR/updater"

rm "$UPDATE_DIR/updater"; mkdir "$UPDATE_DIR/updater"
: > "$CALLS"
assert_fail "a directory at the marker: fails" quiet install_updater "$SRC"
assert_eq "a directory at the marker: nothing lands in it" "" "$(ls -A "$UPDATE_DIR/updater")"
assert_eq "a directory at the marker: systemd untouched" "" "$(grep '^systemctl' "$CALLS" || true)"
assert_eq "a directory at the marker: no temp file left" "" "$(ls -A "$WORK_DIR")"
rm -rf "$UPDATE_DIR/updater"

mv "$UPDATE_DIR" "$ROOT/update.real"
mkdir -m 0755 "$ROOT/elsewhere"
ln -s "$ROOT/elsewhere" "$UPDATE_DIR"
: > "$CALLS"
assert_fail "a link at the update dir: fails" quiet install_updater "$SRC"
assert_eq "a link at the update dir: nothing written where it leads" "" "$(ls -A "$ROOT/elsewhere")"
assert_eq "a link at the update dir: its mode unchanged" 755 "$(stat -c %a "$ROOT/elsewhere")"
assert_eq "a link at the update dir: nothing chowned, systemd untouched" "" "$(cat "$CALLS")"
assert_ok "a link at the update dir: left as it is" [ -L "$UPDATE_DIR" ]
rm "$UPDATE_DIR"

echo 'a file' > "$UPDATE_DIR"
: > "$CALLS"
assert_fail "a file at the update dir: fails" quiet install_updater "$SRC"
assert_eq "a file at the update dir: left as it is" "a file" "$(cat "$UPDATE_DIR")"
assert_eq "a file at the update dir: nothing chowned, systemd untouched" "" "$(cat "$CALLS")"
rm "$UPDATE_DIR"; mv "$ROOT/update.real" "$UPDATE_DIR"

# A missing source file stops it before systemd is touched.
rm "$SRC/deploy/outhaul-update.path"
rm "$UNIT_DIR/outhaul-update.path"
: > "$CALLS"
assert_fail "missing unit fails" quiet install_updater "$SRC"
assert_eq "missing unit: systemd untouched" "" "$(grep '^systemctl' "$CALLS" || true)"

# setup_updater is what main calls: a failure must not stop the install.
install_updater() { echo "install_updater $*" >> "$CALLS"; return 1; }
: > "$CALLS"
out=$(set -e; setup_updater "$SRC" 2>&1; echo "reached")
assert_contains "setup_updater: a failure does not abort" "$out" reached
assert_contains "setup_updater: names the updater" "$out" "could not install the updater"
assert_contains "setup_updater: says what is unavailable" "$out" "re-run the installer"
assert_eq "setup_updater: passes the source dir" "install_updater $SRC" "$(cat "$CALLS")"
unset -f install_updater

rm -rf "$ROOT"
[ "${TESTS_FAIL:-0}" -eq 0 ]
