#!/usr/bin/env sh
# The updater (deploy/update.sh). systemd, Docker, curl, journalctl and the
# build are shell-function stubs, and every path points into a temp tree.
here=$(cd "$(dirname "$0")" && pwd)
. "$here/assert.sh"
# shellcheck disable=SC2034 # read by update.sh, which sources it
INSTALLER=$here/../bootstrap.sh
OUTHAUL_UPDATER_LIB=1 . "$here/../update.sh"
default_repo=$OUTHAUL_REPO
# The installer defines the real install_updater, which writes to system
# paths. Scenarios that want one define their own recording stub.
unset -f install_updater

umask 022 # systemd's default for a service
ROOT=$(mktemp -d)
OLD=aaaaaaa1111
NEW=bbbbbbb2222

exists() { if [ -e "$1" ]; then echo yes; else echo no; fi; }
wait_for() { # file: up to 10 s
	_w=0
	while [ ! -e "$1" ] && [ "$_w" -lt 100 ]; do command sleep 0.1; _w=$((_w + 1)); done
}

# A stand-in outhaul binary: `version` and `images` as the real one prints them.
fake_outhaul() { # path commit repo traefik_image
	cat > "$1" <<EOF
#!/bin/sh
case \$1 in
version) printf 'commit=%s\nrepo=%s\nlocal=false\n' '$2' '$3' ;;
images) printf 'traefik %s\ncloudflared cloudflare/cloudflared:1\n' '$4' ;;
*) exit 2 ;;
esac
EOF
	chmod 0755 "$1"
}

# ---------------------------------------------------------- write_status --

UPDATE_DIR=$ROOT/ws
mkdir -p "$UPDATE_DIR"
# shellcheck disable=SC2034 # used by functions sourced from update.sh
STARTED=1700000000
write_status running "$OLD" '' updating
st=$UPDATE_DIR/status
assert_eq "running status" "state=running|from=$OLD|to=|message=updating|started=1700000000" "$(paste -sd'|' "$st")"
assert_eq "status readable by outhaul" 644 "$(stat -c %a "$st")"
write_status updated "$OLD" "$NEW" 'updated from aaaaaaa to bbbbbbb'
assert_eq "final status keeps started" 1700000000 "$(sed -n 's/^started=//p' "$st")"
assert_eq "final status has finished" 1 "$(grep -c '^finished=[0-9][0-9]*$' "$st")"
assert_eq "no temp files left" "status" "$(ls -A "$UPDATE_DIR")"

# ---------------------------------------------------- new_version_healthy --

T=$ROOT/health
mkdir -p "$T"
curl() { echo "curl $*" >> "$T/calls"; printf '%b' "$CURL_OUT"; }
sleep() { :; }

CURL_OUT='HTTP/1.1 200 OK\r\nx-outhaul-commit: abc123\r\nContent-Length: 2\r\n\r\n'
assert_ok "commit header: name case-insensitive, CR stripped" new_version_healthy abc123 8080
CURL_OUT='HTTP/1.1 200 OK\r\nX-Outhaul-Commit:   abc123  \r\n\r\n'
assert_ok "commit header: spaces around the value" new_version_healthy abc123 8080
CURL_OUT='HTTP/1.1 200 OK\r\nX-Outhaul-Commit: abc1234\r\n\r\n'
assert_fail "commit header: a longer commit is not a match" new_version_healthy abc123 8080
CURL_OUT='HTTP/1.1 200 OK\r\nX-Other: abc123\r\n\r\n'
assert_fail "commit header: another header is not a match" new_version_healthy abc123 8080
CURL_OUT='HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\n'
assert_ok "unstamped build: any 200 counts" new_version_healthy '' 8080
CURL_OUT=''
assert_fail "unstamped build: no answer is not healthy" new_version_healthy '' 8080

: > "$T/calls"
CURL_OUT='HTTP/1.1 200 OK\r\nX-Outhaul-Commit: other\r\n\r\n'
new_version_healthy abc123 9090 || true
assert_eq "health window: 60 attempts" 60 "$(grep -c '^curl ' "$T/calls")"
assert_contains "health check: the admin port's /healthz, headers to stdout" "$(head -1 "$T/calls")" \
	"curl -sS --max-time 2 -D - -o /dev/null http://127.0.0.1:9090/healthz"
unset -f curl

# ---------------------------------------------------- cached_go_toolchain --

CACHE_DIR=$ROOT/cache
fake_go() { # goroot version
	mkdir -p "$1/bin"
	printf '#!/bin/sh\n[ "$*" = "env GOVERSION" ] && echo go%s\n' "$2" > "$1/bin/go"
	chmod 0755 "$1/bin/go"
}
install_go_toolchain() {
	echo "download $1" >> "$ROOT/go_calls"
	[ "${GO_ON_PATH:-0}" = 1 ] && return 0
	GOROOT_TMP=$(mktemp -d "$ROOT/goroot.XXXXXX")
	fake_go "$GOROOT_TMP/go" "$1"
	PATH=$GOROOT_TMP/go/bin:$PATH
	export PATH
}

fake_go "$CACHE_DIR/go1.26.4" 1.26.4
: > "$ROOT/go_calls"
got=$(cached_go_toolchain 1.26.4 >/dev/null; command -v go) || true
assert_eq "cached Go is used" "$CACHE_DIR/go1.26.4/bin/go" "$got"
assert_eq "cached Go is not downloaded" "" "$(cat "$ROOT/go_calls")"

rm -rf "$CACHE_DIR"
fake_go "$CACHE_DIR/go1.26.3" 1.26.3
: > "$ROOT/go_calls"
got=$(cached_go_toolchain 1.26.4 >/dev/null; command -v go; exists "$GOROOT_TMP/go") || true
assert_eq "a new Go version is downloaded" "download 1.26.4" "$(cat "$ROOT/go_calls")"
assert_eq "a downloaded Go is kept and used" "$CACHE_DIR/go1.26.4/bin/go
no" "$got"
assert_eq "the cache holds only that version" "go1.26.4" "$(ls -A "$CACHE_DIR")"

fake_go "$CACHE_DIR/go1.26.4" 1.26.9
: > "$ROOT/go_calls"
got=$(cached_go_toolchain 1.26.4 >/dev/null; command -v go) || true
assert_eq "a cached Go of the wrong version is replaced" "download 1.26.4|$CACHE_DIR/go1.26.4/bin/go" \
	"$(paste -sd'|' "$ROOT/go_calls")|$got"

rm -rf "$CACHE_DIR"
: > "$ROOT/go_calls"
(GO_ON_PATH=1; unset GOROOT_TMP; cached_go_toolchain 1.26.4 >/dev/null) || true
assert_eq "a Go already on PATH: nothing to keep" no "$(exists "$CACHE_DIR/go1.26.4")"
unset -f install_go_toolchain fake_go

# ------------------------------------------------ snapshot, restore_snapshot --

T=$ROOT/snap
BIN=$T/bin/outhaul UNIT_DIR=$T/units DATA_DIR=$T/data UPDATE_DIR=$T/data/update
mkdir -p "$T/bin" "$UNIT_DIR" "$UPDATE_DIR"
fake_outhaul "$BIN" "$OLD" file:///srv/o.git traefik:v1
echo 'old unit' > "$UNIT_DIR/outhaul.service"
echo 'old data' > "$DATA_DIR/outhaul.db"
echo 'old wal' > "$DATA_DIR/outhaul.db-wal"
echo 'old shm' > "$DATA_DIR/outhaul.db-shm"
# A mode the umask would change: only a copy that keeps modes keeps it.
chmod 0664 "$DATA_DIR/outhaul.db"
mkdir -p "$UPDATE_DIR/previous" && echo stale > "$UPDATE_DIR/previous/outhaul.db-shm"
assert_ok "snapshot" snapshot
assert_eq "snapshot: binary, unit and database" "outhaul outhaul.db outhaul.db-shm outhaul.db-wal outhaul.service" \
	"$(ls "$UPDATE_DIR/previous" | paste -sd' ' -)"
assert_eq "snapshot: replaces the previous copies" "old shm" "$(cat "$UPDATE_DIR/previous/outhaul.db-shm")"
assert_eq "snapshot: only previous/ is left" "previous" "$(ls -A "$UPDATE_DIR")"
echo 'new data' > "$DATA_DIR/outhaul.db"
echo 'new wal' > "$DATA_DIR/outhaul.db-wal"
rm "$DATA_DIR/outhaul.db-shm"
fake_outhaul "$BIN" "$NEW" file:///srv/o.git traefik:v2
echo 'new unit' > "$UNIT_DIR/outhaul.service"
# The data dir belongs to outhaul: a planted dest.new link must not be followed.
echo 'not yours' > "$T/victim"
ln -s "$T/victim" "$DATA_DIR/outhaul.db.new"
assert_ok "restore_snapshot" restore_snapshot
assert_eq "restore: a planted link is not written through" "not yours" "$(cat "$T/victim")"
assert_eq "restore: binary, unit and database" "commit=$OLD|old unit|old data|old wal|old shm" \
	"$("$BIN" version | head -1)|$(cat "$UNIT_DIR/outhaul.service" "$DATA_DIR/outhaul.db" "$DATA_DIR/outhaul.db-wal" "$DATA_DIR/outhaul.db-shm" | paste -sd'|' -)"
assert_eq "restore: the database keeps its mode" 664 "$(stat -c %a "$DATA_DIR/outhaul.db")"
assert_eq "restore: no temp files left" "outhaul.db outhaul.db-shm outhaul.db-wal update" "$(ls "$DATA_DIR" | paste -sd' ' -)"

# ------------------------------------------------------------- update_main --

# Per scenario: a fresh tree with the running version (OLD), a request, and
# knobs the stubs read.
setup() {
	T=$(mktemp -d "$ROOT/run.XXXXXX")
	BIN=$T/bin/outhaul UNIT_DIR=$T/units DATA_DIR=$T/data UPDATE_DIR=$T/data/update CACHE_DIR=$T/cache
	mkdir -p "$T/bin" "$UNIT_DIR" "$UPDATE_DIR"
	fake_outhaul "$BIN" "$OLD" file:///srv/o.git traefik:v1
	echo 'old unit' > "$UNIT_DIR/outhaul.service"
	echo 'old data' > "$DATA_DIR/outhaul.db"
	echo 'the previous update' > "$UPDATE_DIR/log"
	printf 'state=up_to_date\nfrom=%s\nto=%s\nmessage=\nstarted=1\nfinished=2\n' "$OLD" "$OLD" > "$UPDATE_DIR/status"
	: > "$UPDATE_DIR/request"
	: > "$T/calls"
	MAIN=$NEW            # the commit of main
	HEALTHY="$OLD $NEW"  # commits that answer /healthz once started
	BUILD=ok PULL=ok CF_RUNNING=0 NEW_UNIT=1
	unset OUTHAUL_LISTEN_ADDR
}

# update_main exits, so it runs in a subshell, with errexit as under systemd.
run_update() {
	set +e
	(set -e; update_main)
	RC=$?
	set -e
}

field() { sed -n "s/^$1=//p" "$UPDATE_DIR/status"; }
log() { cat "$UPDATE_DIR/log"; }
called() { grep -c -x "$1" "$T/calls" || true; }
line_of() { grep -n "$1" "$T/calls" | tail -1 | cut -d: -f1; }
systemctl_calls() { sed -n 's/^systemctl //p' "$T/calls" | paste -sd'|' -; }
commit_of() { "$1" version | sed -n 's/^commit=//p'; }
inode() { ls -i "$1" | awk '{ print $1 }'; }
has_status_keys() { # file final(0|1)
	for _k in state from to message started; do grep -q "^$_k=" "$1" || return 1; done
	if [ "$2" = 1 ]; then grep -q '^finished=[0-9][0-9]*$' "$1"; else ! grep -q '^finished=' "$1"; fi
}

fetch_source() {
	SRC_DIR=$T/src
	mkdir -p "$SRC_DIR/deploy"
	printf 'module example\n\ngo 1.26\n\ntoolchain go1.26.4\n' > "$SRC_DIR/go.mod"
	if [ "$NEW_UNIT" = 1 ]; then echo 'new unit' > "$SRC_DIR/deploy/outhaul.service"; fi
	printf '%s\n' "$OUTHAUL_REPO" > "$T/repo_seen"
	cp "$UPDATE_DIR/status" "$T/status_running"
	exists "$UPDATE_DIR/request" > "$T/request_seen"
}
source_commit() { printf '%s\n' "$MAIN"; }
maybe_add_swap() {
	echo "swap $*" >> "$T/calls"
	# The installer's prompts read fd 3: open, and answering nothing.
	if (: <&3) 2>/dev/null; then echo 'fd 3 open' >> "$T/calls"; fi
	if ask_yes_no "  Create a temporary 2 GB swapfile for the build?" y; then echo 'prompt: default' >> "$T/calls"; fi
}
cached_go_toolchain() { echo "go $1" >> "$T/calls"; }
build_binary() {
	case $BUILD in
	fail) die "build failed — see $LOGFILE" ;;
	block) : > "$T/building"; wait_for "$T/never" ;;
	esac
	fake_outhaul "$SRC_DIR/outhaul" "$NEW" file:///srv/o.git traefik:v2
}
systemctl() {
	echo "systemctl $*" >> "$T/calls"
	case $* in
	'stop outhaul') rm -f "$T/up" ;;
	'start outhaul')
		: > "$T/up"
		# The new version migrates the database as it starts.
		if [ "$(commit_of "$BIN")" = "$NEW" ]; then
			echo migrated > "$DATA_DIR/outhaul.db"
			echo 'new wal' > "$DATA_DIR/outhaul.db-wal"
		fi ;;
	esac
}
curl() {
	echo "curl $*" >> "$T/calls"
	if [ -e "$T/block_curl" ]; then
		rm -f "$T/block_curl"; : > "$T/curl_blocked"; wait_for "$T/release"; return 7
	fi
	[ -e "$T/up" ] || return 7
	_c=$(commit_of "$BIN")
	case " $HEALTHY " in
	*" $_c "*) printf 'HTTP/1.1 200 OK\r\nX-Outhaul-Commit: %s\r\nContent-Length: 2\r\n\r\n' "$_c" ;;
	*) return 7 ;;
	esac
}
docker() {
	echo "docker $*" >> "$T/calls"
	case $1 in
	container) [ "$CF_RUNNING" = 1 ] ;;
	pull) [ "$PULL" = ok ] ;;
	esac
}
journalctl() { echo "journalctl $*" >> "$T/calls"; echo 'outhaul: fatal: rollback test'; }

# updated
setup
# shellcheck disable=SC2034 # used by functions sourced from update.sh
OUTHAUL_LISTEN_ADDR=:9090
before=$(inode "$BIN")
run_update
assert_eq "updated: exit 0" 0 "$RC"
assert_eq "updated: state" updated "$(field state)"
assert_eq "updated: from, to" "$OLD $NEW" "$(field from) $(field to)"
assert_eq "updated: message" "updated from aaaaaaa to bbbbbbb" "$(field message)"
assert_ok "updated: final status format" has_status_keys "$UPDATE_DIR/status" 1
assert_ok "running status format" has_status_keys "$T/status_running" 0
assert_eq "status says running while in progress" running "$(sed -n 's/^state=//p' "$T/status_running")"
assert_eq "request already gone while running" no "$(cat "$T/request_seen")"
assert_eq "updated: request removed" no "$(exists "$UPDATE_DIR/request")"
assert_eq "clones the running build's repository" file:///srv/o.git "$(cat "$T/repo_seen")"
assert_eq "updated: the new binary is installed" "$NEW" "$(commit_of "$BIN")"
assert_fail "updated: the binary is a new inode (renamed into place)" [ "$before" = "$(inode "$BIN")" ]
assert_eq "updated: the old binary is kept" "$OLD" "$(commit_of "$UPDATE_DIR/previous/outhaul")"
assert_eq "updated: the new unit is installed, the old kept" "new unit|old unit" \
	"$(cat "$UNIT_DIR/outhaul.service")|$(cat "$UPDATE_DIR/previous/outhaul.service")"
assert_eq "updated: the database is kept" "old data" "$(cat "$UPDATE_DIR/previous/outhaul.db")"
assert_eq "updated: stop, reload, start" "stop outhaul|daemon-reload|start outhaul" "$(systemctl_calls)"
assert_eq "updated: Go toolchain from go.mod" 1 "$(called 'go 1.26.4')"
assert_eq "updated: swap offered with MemTotal and SwapTotal" 1 "$(grep -c '^swap [0-9][0-9]* [0-9][0-9]*$' "$T/calls")"
assert_eq "updated: pulls the new build's Traefik image" 1 "$(called 'docker pull traefik:v2')"
assert_eq "updated: no cloudflared pull without its container" 0 "$(called 'docker pull cloudflare/cloudflared:1')"
assert_ok "updated: the pulls come before the stop" \
	[ "$(line_of '^docker pull')" -lt "$(line_of '^systemctl stop outhaul$')" ]
assert_eq "updated: health checked on OUTHAUL_LISTEN_ADDR's port" 1 \
	"$(grep -c 'http://127.0.0.1:9090/healthz$' "$T/calls")"
assert_contains "updated: the log names the Traefik change" "$(log)" "traefik:v1 to traefik:v2"
assert_contains "updated: ... and its effect" "$(log)" "This update restarts Traefik: every app is unreachable for a few seconds."
assert_eq "updated: a fresh log" 0 "$(grep -c 'the previous update' "$UPDATE_DIR/log")"
assert_eq "updated: no updater refresh attempted when the installer has none" 0 "$(grep -c 'could not refresh' "$UPDATE_DIR/log" || true)"
assert_eq "updated: log readable by outhaul" 644 "$(stat -c %a "$UPDATE_DIR/log")"
assert_eq "updated: the clone is removed" no "$(exists "$T/src")"
assert_eq "updated: only status, log and previous/ are left" "log previous status" "$(ls -A "$UPDATE_DIR" | paste -sd' ' -)"

# install_updater (from a later installer) runs once the new version is healthy.
setup
install_updater() {
	printf '%s %s\n' "$1" "$(exists "$1/deploy/outhaul.service")" > "$T/install_updater"
	printf '%s\n' "$(field state)" >> "$T/install_updater"
}
run_update
assert_eq "install_updater: called with the new tree, before the final status" "$T/src yes|running" \
	"$(paste -sd'|' "$T/install_updater")"
assert_eq "install_updater: updated" updated "$(field state)"

setup
install_updater() { false; : > "$T/after_false"; }
run_update
assert_eq "install_updater fails: still updated" updated "$(field state)"
assert_eq "install_updater fails: it stopped at the failure" no "$(exists "$T/after_false")"
assert_contains "install_updater fails: the log says so" "$(log)" "could not refresh the updater"
unset -f install_updater

# A build that records no repository: clone the default.
setup
fake_outhaul "$BIN" "$OLD" '' traefik:v1
run_update
assert_eq "no repository in the build: the default" "$default_repo" "$(cat "$T/repo_seen")"

# up to date
setup
MAIN=$OLD
cp "$BIN" "$T/bin.orig"
run_update
assert_eq "up to date: exit 0" 0 "$RC"
assert_eq "up to date: state" up_to_date "$(field state)"
assert_eq "up to date: message" "aaaaaaa is the newest main" "$(field message)"
assert_ok "up to date: status format" has_status_keys "$UPDATE_DIR/status" 1
assert_ok "up to date: binary unchanged" cmp -s "$T/bin.orig" "$BIN"
assert_eq "up to date: Outhaul never stopped" "" "$(systemctl_calls)"
assert_eq "up to date: request removed" no "$(exists "$UPDATE_DIR/request")"
assert_eq "up to date: the clone is removed" no "$(exists "$T/src")"

# build failed
setup
BUILD=fail
cp "$BIN" "$T/bin.orig"
run_update
assert_fail "build failed: exit non-zero" [ "$RC" = 0 ]
assert_eq "build failed: state" build_failed "$(field state)"
assert_eq "build failed: message" "the build failed; nothing changed. See the log." "$(field message)"
assert_ok "build failed: status format" has_status_keys "$UPDATE_DIR/status" 1
assert_ok "build failed: binary unchanged" cmp -s "$T/bin.orig" "$BIN"
assert_eq "build failed: Outhaul never stopped" "" "$(systemctl_calls)"
assert_eq "build failed: request removed" no "$(exists "$UPDATE_DIR/request")"
assert_contains "build failed: the log has the error" "$(log)" "build failed — see $UPDATE_DIR/log"
assert_eq "build failed: ... once" 1 "$(grep -c 'build failed — see' "$UPDATE_DIR/log")"
assert_eq "build failed: the clone is removed" no "$(exists "$T/src")"

# an image pull fails: nothing has changed yet
setup
PULL=fail
run_update
assert_eq "pull failed: build_failed" build_failed "$(field state)"
assert_eq "pull failed: Outhaul never stopped" "" "$(systemctl_calls)"

# cloudflared is pulled only where its container exists
setup
CF_RUNNING=1
run_update
assert_eq "tunnel in use: cloudflared pulled" 1 "$(called 'docker pull cloudflare/cloudflared:1')"
assert_eq "tunnel in use: updated" updated "$(field state)"

# rolled back
setup
HEALTHY=$OLD
run_update
assert_fail "rolled back: exit non-zero" [ "$RC" = 0 ]
assert_eq "rolled back: state" rolled_back "$(field state)"
assert_eq "rolled back: message" "the new version did not become healthy; rolled back to aaaaaaa" "$(field message)"
assert_ok "rolled back: status format" has_status_keys "$UPDATE_DIR/status" 1
assert_eq "rolled back: the old binary is back" "$OLD" "$(commit_of "$BIN")"
assert_eq "rolled back: the old unit is back" "old unit" "$(cat "$UNIT_DIR/outhaul.service")"
assert_eq "rolled back: the database is as before" "old data" "$(cat "$DATA_DIR/outhaul.db")"
assert_eq "rolled back: the new version's WAL is gone" no "$(exists "$DATA_DIR/outhaul.db-wal")"
assert_eq "rolled back: systemctl" "stop outhaul|daemon-reload|start outhaul|stop outhaul|daemon-reload|start outhaul" \
	"$(systemctl_calls)"
assert_eq "rolled back: 60 checks of the new version, then one of the old" 61 "$(grep -c '^curl ' "$T/calls")"
assert_eq "rolled back: the journal is read" 1 "$(called 'journalctl -u outhaul -n 20 --no-pager')"
assert_contains "rolled back: the journal is in the log" "$(log)" "outhaul: fatal: rollback test"

# failed: the old version does not come back either
setup
HEALTHY=''
run_update
assert_eq "failed: state" failed "$(field state)"
assert_ok "failed: status format" has_status_keys "$UPDATE_DIR/status" 1
assert_contains "failed: the message points at the log" "$(field message)" "See the log"
assert_eq "failed: the old binary is back" "$OLD" "$(commit_of "$BIN")"
assert_contains "failed: recovery: stop" "$(log)" "  systemctl stop outhaul"
assert_contains "failed: recovery: the binary" "$(log)" "  cp -p $UPDATE_DIR/previous/outhaul $BIN"
assert_contains "failed: recovery: the database" "$(log)" "  cp -p $UPDATE_DIR/previous/outhaul.db $DATA_DIR/outhaul.db"
assert_contains "failed: recovery: the new WAL" "$(log)" "  rm -f $DATA_DIR/outhaul.db-wal"
assert_contains "failed: recovery: start" "$(log)" "  systemctl start outhaul"

# the new tree cannot be installed: roll back
setup
NEW_UNIT=0
run_update
assert_eq "install failed: rolled back" rolled_back "$(field state)"
assert_eq "install failed: message" "could not install the new version; rolled back to aaaaaaa" "$(field message)"
assert_eq "install failed: the old binary is back" "$OLD" "$(commit_of "$BIN")"

# no copy of the running version could be kept: put nothing back, start it again
setup
cp() { case $* in *previous.new*) return 1 ;; esac; command cp "$@"; }
run_update
unset -f cp
assert_eq "snapshot failed: rolled back" rolled_back "$(field state)"
assert_eq "snapshot failed: message" "could not keep a copy of the running version; rolled back to aaaaaaa" "$(field message)"
assert_eq "snapshot failed: the binary was never replaced" "$OLD" "$(commit_of "$BIN")"
assert_eq "snapshot failed: started again" "stop outhaul|stop outhaul|daemon-reload|start outhaul" "$(systemctl_calls)"

# stopped mid-build (systemctl stop, the 30-minute timeout): build_failed at once
setup
BUILD=block
(set -e; update_main) &
pid=$!
wait_for "$T/building"
kill -TERM "$pid" || true
wait "$pid" || true
assert_eq "stopped mid-build: build_failed" build_failed "$(field state)"
assert_eq "stopped mid-build: Outhaul never stopped" "" "$(systemctl_calls)"
assert_eq "stopped mid-build: the clone is removed" no "$(exists "$T/src")"

# stopped while the new version is being checked: roll back
setup
: > "$T/block_curl"
(set -e; update_main) &
pid=$!
wait_for "$T/curl_blocked"
kill -TERM "$pid" || true
: > "$T/release"
wait "$pid" || true
assert_eq "stopped mid-swap: rolled back" rolled_back "$(field state)"
assert_eq "stopped mid-swap: the old binary is back" "$OLD" "$(commit_of "$BIN")"
assert_eq "stopped mid-swap: the database is as before" "old data" "$(cat "$DATA_DIR/outhaul.db")"
assert_eq "stopped mid-swap: message" \
	"the updater was stopped before the new version was confirmed; rolled back to aaaaaaa" "$(field message)"

# stopped while rolling back: the rollback still finishes
setup
HEALTHY=$OLD
journalctl() { : > "$T/in_rollback"; wait_for "$T/release"; }
(set -e; update_main) &
pid=$!
wait_for "$T/in_rollback"
kill -TERM "$pid" || true
: > "$T/release"
wait "$pid" || true
assert_eq "stopped mid-rollback: rolled back" rolled_back "$(field state)"
assert_eq "stopped mid-rollback: the old binary is back" "$OLD" "$(commit_of "$BIN")"
journalctl() { echo "journalctl $*" >> "$T/calls"; echo 'outhaul: fatal: rollback test'; }

# stopped once the new version is healthy: it stays
setup
install_updater() { : > "$T/refreshing"; wait_for "$T/release"; }
(set -e; update_main) &
pid=$!
wait_for "$T/refreshing"
kill -TERM "$pid" || true
: > "$T/release"
wait "$pid" || true
unset -f install_updater
assert_eq "stopped once healthy: updated" updated "$(field state)"
assert_eq "stopped once healthy: the new binary stays" "$NEW" "$(commit_of "$BIN")"
assert_eq "stopped once healthy: no rollback" "stop outhaul|daemon-reload|start outhaul" "$(systemctl_calls)"

# The ruling's order: a fresh log, then status running, then the request goes.
setup
rm() {
	case $* in
	*/request)
		# The call that takes the request away.
		[ -e "$UPDATE_DIR/request" ] || { command rm "$@"; return; }
		{
			sed -n 's/^state=//p' "$UPDATE_DIR/status" || true
			grep -c 'the previous update' "$UPDATE_DIR/log" || true
		} > "$T/at_rm" 2>&1 ;;
	esac
	command rm "$@"
}
run_update
unset -f rm
assert_eq "request removed after the status says running, and the log is fresh" "running|0" "$(paste -sd'|' "$T/at_rm")"
assert_eq "fd 3 is open for the installer's prompts" 1 "$(called 'fd 3 open')"
assert_eq "an installer prompt takes its default" 1 "$(called 'prompt: default')"

# The real paths, and the data dir from /etc/outhaul.env.
assert_eq "default paths" "/usr/local/bin/outhaul /etc/systemd/system /var/lib/outhaul /var/lib/outhaul/update /var/cache/outhaul" \
	"$(unset OUTHAUL_DATA_DIR; OUTHAUL_UPDATER_LIB=1 . "$here/../update.sh"; echo "$BIN $UNIT_DIR $DATA_DIR $UPDATE_DIR $CACHE_DIR")"
assert_eq "OUTHAUL_DATA_DIR moves the data and update dirs" "/srv/o /srv/o/update" \
	"$(OUTHAUL_DATA_DIR=/srv/o OUTHAUL_UPDATER_LIB=1 . "$here/../update.sh"; echo "$DATA_DIR $UPDATE_DIR")"

rm -rf "$ROOT"
[ "${TESTS_FAIL:-0}" -eq 0 ]
