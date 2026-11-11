#!/usr/bin/env sh
# install_or_upgrade: the careful swap and its exit guard. systemd, Docker,
# curl, journalctl, chgrp, chown and sleep are recording stubs; every path
# points into a temp tree.
here=$(cd "$(dirname "$0")" && pwd)
. "$here/assert.sh"
OUTHAUL_INSTALLER_LIB=1 . "$here/../bootstrap.sh"

ROOT=$(mktemp -d)
OLD=aaaaaaa1111
NEW=bbbbbbb2222
OLDER=ccccccc3333

exists() { if [ -e "$1" ]; then echo yes; else echo no; fi; }
commit_of() { "$1" version | sed -n 's/^commit=//p'; }

# A stand-in outhaul binary: `version` and `images` as the real one prints them.
fake_outhaul() { # path commit
	cat > "$1" <<EOF
#!/bin/sh
case \$1 in
version) printf 'commit=%s\nrepo=file:///srv/o.git\nlocal=false\n' '$2' ;;
images) printf 'traefik traefik:v1\ncloudflared cloudflare/cloudflared:1\n' ;;
*) exit 2 ;;
esac
EOF
	chmod 0755 "$1"
}

# ------------------------------------------------------------------ stubs --

systemctl() {
	# service_env asks for MainPID; it is not a state change, so it is not
	# part of the recorded sequence.
	case $* in
	'show -p MainPID --value outhaul') return 0 ;;
	esac
	printf 'systemctl %s\n' "$*" >>"$T/calls"
	printf '%s\n' "$*" >>"$T/systemctl"
	case $* in
	'stop outhaul') rm -f "$T/up" ;;
	'start outhaul' | 'enable --now outhaul')
		: >"$T/up"
		# The new version migrates the database as it starts.
		if [ "$(commit_of "$BIN")" = "$NEW" ]; then
			echo 'new data' > "$DATA_DIR/outhaul.db"
			echo 'new wal' > "$DATA_DIR/outhaul.db-wal"
		fi
		;;
	esac
	return 0
}
# The health check: answer as the binary in place, when HEALTHY names it.
curl() {
	printf 'curl %s\n' "$*" >>"$T/calls"
	[ -e "$T/up" ] || return 7
	_c=$(commit_of "$BIN")
	case " $HEALTHY " in
	*" $_c "*) printf 'HTTP/1.1 200 OK\r\nX-Outhaul-Commit: %s\r\nContent-Length: 2\r\n\r\n' "$_c" ;;
	*) return 7 ;;
	esac
}
journalctl() { printf 'journalctl %s\n' "$*" >>"$T/calls"; echo 'outhaul: fatal: swap test'; }
docker() {
	printf 'docker %s\n' "$*" >>"$T/calls"
	case $1 in
	container) return 1 ;;
	pull) return 0 ;;
	esac
	return 0
}
chgrp() { printf 'chgrp %s\n' "$*" >>"$T/calls"; }
chown() { printf 'chown %s\n' "$*" >>"$T/calls"; }
# The health check tries 60 times; without this the failures take a minute.
sleep() { :; }

# A fresh tree with the running version (OLD) and a new build (NEW) in SRC_DIR.
setup_swap() {
	T=$(mktemp -d "$ROOT/swap.XXXXXX")
	BIN=$T/bin/outhaul
	UNIT_DIR=$T/units
	DATA_DIR=$T/data
	UPDATE_DIR=$DATA_DIR/update
	WORK_DIR=$T/work
	INSTALL_ENV=$T/etc/install.env
	SERVICE_DROPIN=$UNIT_DIR/outhaul.service.d/10-install.conf
	UPDATE_DROPIN=$UNIT_DIR/outhaul-update.service.d/10-install.conf
	SRC_DIR=$T/src
	# shellcheck disable=SC2034 # read by the sourced functions
	MARKER=$T/updater/installed
	LOGFILE=$T/log
	# shellcheck disable=SC2034 # install_or_upgrade reads OUTHAUL_LISTEN_ADDR
	ADMIN_PORT=8080
	# shellcheck disable=SC2034 # read by cleanup, so SRC_DIR is kept
	FROM_CHECKOUT=1
	HEALTHY="$OLD $NEW"
	mkdir -p "$T/bin" "$UNIT_DIR" "$DATA_DIR" "$UPDATE_DIR" "$WORK_DIR" \
		"$SRC_DIR/deploy" "$(dirname "$INSTALL_ENV")" \
		"$(dirname "$SERVICE_DROPIN")" "$(dirname "$UPDATE_DROPIN")"
	fake_outhaul "$BIN" "$OLD"
	fake_outhaul "$SRC_DIR/outhaul" "$NEW"
	echo 'old unit' > "$UNIT_DIR/outhaul.service"
	echo 'new unit' > "$SRC_DIR/deploy/outhaul.service"
	echo 'old data' > "$DATA_DIR/outhaul.db"
	echo 'old install env' > "$INSTALL_ENV"
	echo 'old service dropin' > "$SERVICE_DROPIN"
	echo 'old update dropin' > "$UPDATE_DROPIN"
	: > "$LOGFILE"
	: > "$T/systemctl"
	: > "$T/calls"
	: > "$T/up"
	# Task 7 replaces these stubs; here they only need to change the files so
	# a rollback has something to put back.
	write_install_env() { echo 'new install env' > "$INSTALL_ENV"; }
	write_dropins() {
		echo 'new service dropin' > "$SERVICE_DROPIN"
		echo 'new update dropin' > "$UPDATE_DROPIN"
	}
}

# Runs install_or_upgrade the way main will: in a subshell whose EXIT trap
# rolls back, recording the state it ended in.
run_swap() {
	set +e
	(
		set -e
		trap 'swap_guard; printf "%s %s\n" "${SNAPSHOT_OK:-0}" "${ROLLED_BACK:-0}" > "$T/state"; cleanup' EXIT
		install_or_upgrade "$SRC_DIR" "$NEW" ""
	) >"$T/out" 2>&1
	RC=$?
	set -e
}

calls() { paste -sd'|' "$T/systemctl"; }
line_of() { grep -n "$1" "$T/calls" | tail -1 | cut -d: -f1; }

# 1. First install: no $BIN. Installs, starts, checks the target; no snapshot.
setup_swap
rm -f "$BIN"
: > "$T/up"
HEALTHY=$NEW
run_swap
assert_eq "first install: exit 0" 0 "$RC"
assert_eq "first install: the new binary" "$NEW" "$(commit_of "$BIN")"
assert_eq "first install: the new unit" "new unit" "$(cat "$UNIT_DIR/outhaul.service")"
assert_eq "first install: reloaded and started, never stopped" "daemon-reload|enable --now outhaul" "$(calls)"
assert_eq "first install: no snapshot" no "$(exists "$WORK_DIR/previous")"
assert_eq "first install: the image is pulled" 1 "$(grep -c '^docker pull traefik:v1$' "$T/calls" || true)"
assert_eq "first install: state" "0 0" "$(cat "$T/state")"

# 2. Upgrade healthy: stops, snapshots, installs, starts; the old copy is
#    kept; return 0.
setup_swap
run_swap
assert_eq "healthy upgrade: exit 0" 0 "$RC"
assert_eq "healthy upgrade: the new binary" "$NEW" "$(commit_of "$BIN")"
assert_eq "healthy upgrade: the new unit" "new unit" "$(cat "$UNIT_DIR/outhaul.service")"
assert_eq "healthy upgrade: the old binary is kept" "$OLD" "$(commit_of "$WORK_DIR/previous/outhaul")"
assert_eq "healthy upgrade: the old unit is kept" "old unit" "$(cat "$WORK_DIR/previous/outhaul.service")"
assert_eq "healthy upgrade: the old database is kept" "old data" "$(cat "$WORK_DIR/previous/outhaul.db")"
assert_eq "healthy upgrade: the old install.env is kept" "old install env" "$(cat "$WORK_DIR/previous/install.env")"
assert_eq "healthy upgrade: the old service drop-in is kept" "old service dropin" \
	"$(cat "$WORK_DIR/previous/outhaul.service.d.10-install.conf")"
assert_eq "healthy upgrade: the old update drop-in is kept" "old update dropin" \
	"$(cat "$WORK_DIR/previous/outhaul-update.service.d.10-install.conf")"
assert_eq "healthy upgrade: the new install.env and drop-ins are in place" "new install env|new service dropin|new update dropin" \
	"$(cat "$INSTALL_ENV" "$SERVICE_DROPIN" "$UPDATE_DROPIN" | paste -sd'|' -)"
assert_eq "healthy upgrade: stop, reload, start" "stop outhaul|daemon-reload|start outhaul" "$(calls)"
assert_ok "healthy upgrade: the pull comes before the stop" \
	[ "$(line_of '^docker pull')" -lt "$(line_of '^systemctl stop outhaul$')" ]
assert_eq "healthy upgrade: state" "1 0" "$(cat "$T/state")"

# 3. Upgrade unhealthy: roll back the binary, unit, database, install.env and
#    both drop-ins; start the old version; return 1.
setup_swap
HEALTHY=$OLD
run_swap
assert_eq "unhealthy upgrade: exit 1" 1 "$RC"
assert_eq "unhealthy upgrade: the old binary is back" "$OLD" "$(commit_of "$BIN")"
assert_eq "unhealthy upgrade: the old unit is back" "old unit" "$(cat "$UNIT_DIR/outhaul.service")"
assert_eq "unhealthy upgrade: the old database is back" "old data" "$(cat "$DATA_DIR/outhaul.db")"
assert_eq "unhealthy upgrade: the new WAL is gone" no "$(exists "$DATA_DIR/outhaul.db-wal")"
assert_eq "unhealthy upgrade: the old install.env is back" "old install env" "$(cat "$INSTALL_ENV")"
assert_eq "unhealthy upgrade: the old service drop-in is back" "old service dropin" "$(cat "$SERVICE_DROPIN")"
assert_eq "unhealthy upgrade: the old update drop-in is back" "old update dropin" "$(cat "$UPDATE_DROPIN")"
assert_eq "unhealthy upgrade: install, then roll back" \
	"stop outhaul|daemon-reload|start outhaul|stop outhaul|daemon-reload|start outhaul" "$(calls)"
assert_eq "unhealthy upgrade: state" "1 1" "$(cat "$T/state")"
assert_contains "unhealthy upgrade: it says it rolled back" "$(cat "$T/out")" "rolled back to aaaaaaa"

# The caller can say what failed: a proxy failure says its switches did not
# take effect, rather than claiming the new version did not answer.
HEALTHY=$OLD
( SNAPSHOT_OK=1 ROLLBACK_DONE=0 rollback_install "$OLD" "the proxy's switches did not take effect" ) >"$T/reason" 2>&1 || true
assert_contains "rollback: the caller's reason is used" "$(cat "$T/reason")" \
	"the proxy's switches did not take effect; rolled back to aaaaaaa"

# The wizard calls the rollback directly, so it must leave errexit and the
# signal traps as it found them: on after a wizard call, still off when the
# exit trap (already without errexit) called it.
out=$( ( set -e; trap 'exit 130' INT; rollback_install "$OLD" >/dev/null 2>&1; case $- in *e*) echo errexit-on ;; *) echo errexit-off ;; esac ) )
assert_eq "rollback: errexit is back after a wizard call" errexit-on "$out"
out=$( ( set +e; rollback_install "$OLD" >/dev/null 2>&1; case $- in *e*) echo errexit-on ;; *) echo errexit-off ;; esac ) )
assert_eq "rollback: the exit trap's errexit stays off" errexit-off "$out"
out=$( ( trap 'exit 130' INT; rollback_install "$OLD" >/dev/null 2>&1; trap ) 2>/dev/null )
assert_contains "rollback: the INT trap is back" "$out" "exit 130"

# 4. A die in write_dropins after the stop: the trap's swap_guard restores the
#    same files, starts the old version, and the exit status is 1.
setup_swap
write_dropins() { die "swap test: drop-in write failed"; }
set +e
( set -e; trap 'swap_guard; cleanup' EXIT; install_or_upgrade "$SRC_DIR" "$NEW" "" ) 2>"$T/err"
rc=$?
set -e
assert_fail "die after the stop: exit 1" [ "$rc" = 0 ]
assert_eq "die after the stop: old binary" "$OLD" "$("$BIN" version | sed -n 's/^commit=//p')"
assert_eq "die after the stop: old unit" "old unit" "$(cat "$UNIT_DIR/outhaul.service")"
assert_eq "die after the stop: old install.env" "old install env" "$(cat "$INSTALL_ENV")"
assert_eq "die after the stop: old service drop-in" "old service dropin" "$(cat "$SERVICE_DROPIN")"
assert_eq "die after the stop: old version started" "start outhaul" "$(tail -1 "$T/systemctl")"
assert_contains "die after the stop: the reason" "$(cat "$T/err")" "drop-in write failed"

# 5. Snapshot fails: the old service is started again and SNAPSHOT_OK stays 0;
#    no previous/ copy is restored.
setup_swap
HEALTHY=$OLD
mkdir -p "$WORK_DIR/previous"
fake_outhaul "$WORK_DIR/previous/outhaul" "$OLDER"
cp() { case $* in *previous.new*) return 1 ;; esac; command cp "$@"; }
run_swap
unset -f cp
assert_eq "snapshot failed: exit 1" 1 "$RC"
assert_eq "snapshot failed: SNAPSHOT_OK stays 0" "0 1" "$(cat "$T/state")"
assert_eq "snapshot failed: the running binary is still the old one" "$OLD" "$(commit_of "$BIN")"
assert_eq "snapshot failed: no older previous/ copy was restored" "$OLDER" "$(commit_of "$WORK_DIR/previous/outhaul")"
assert_eq "snapshot failed: the old service was started again" "start outhaul" "$(tail -1 "$T/systemctl")"
assert_eq "snapshot failed: stop, start, then the rollback starts it" \
	"stop outhaul|start outhaul|stop outhaul|daemon-reload|start outhaul" "$(calls)"
assert_contains "snapshot failed: it says why" "$(cat "$T/out")" "could not keep a copy of the running version"

# 6. Restore fails: the old binary is started anyway, recovery_help is printed,
#    and the exit status is 1.
setup_swap
HEALTHY=$OLD
restore_snapshot() { return 1; }
run_swap
assert_eq "restore failed: exit 1" 1 "$RC"
assert_eq "restore failed: state" "1 0" "$(cat "$T/state")"
assert_eq "restore failed: the old binary is still kept" "$OLD" "$(commit_of "$WORK_DIR/previous/outhaul")"
assert_eq "restore failed: the service was started anyway" "start outhaul" "$(tail -1 "$T/systemctl")"
assert_contains "restore failed: it says what could not be put back" "$(cat "$T/out")" "could not put back every file"
assert_contains "restore failed: recovery help" "$(cat "$T/out")" "To recover, run as root on the server:"
assert_contains "restore failed: recovery help names install.env" "$(cat "$T/out")" \
	"cp -p $WORK_DIR/previous/install.env $INSTALL_ENV"

rm -rf "$ROOT"
[ "${TESTS_FAIL:-0}" -eq 0 ]
