#!/bin/sh
# Outhaul's updater. outhaul-update.service runs it as root when Outhaul
# creates <data dir>/update/request (outhaul-update.path watches for it).
# It updates to the newest main: clone, build, pull images, stop Outhaul,
# keep a copy of what ran, swap, start, check that the new commit answers,
# and roll back when it does not. It asks no questions.
#
# The update directory is shared with Outhaul, which reads "status" and
# "log" as user outhaul. The request carries nothing and is never read. It
# belongs to outhaul, so the updater keeps its own files in WORK_DIR, a
# root-owned directory: the rollback copy, temp files and the log. In the
# update directory it only renames the status in, links the log in and
# removes the request, by names inside the directory itself (in_update_dir).
#
# Runs under Debian's dash: keep it POSIX. It reuses the installer's
# functions; bootstrap.sh is installed next to this script.
set -eu

INSTALLER=${INSTALLER:-$(dirname "$0")/bootstrap.sh}
OUTHAUL_INSTALLER_LIB=1 . "$INSTALLER"

# Paths (tests point them at a temp tree after sourcing this file).
BIN=/usr/local/bin/outhaul
UNIT_DIR=/etc/systemd/system
DATA_DIR=${OUTHAUL_DATA_DIR:-/var/lib/outhaul}
UPDATE_DIR=$DATA_DIR/update
WORK_DIR=/var/lib/outhaul-updater
CACHE_DIR=/var/cache/outhaul

# The installer mirrors errors into its log file; here stdout and stderr
# already are the log.
log_line() { :; }

# Writes a line to the journal: the stderr systemd started the updater with,
# kept on fd 4 once stdout and stderr go to the log. The journal names the
# log and has each update's result, so they can be found with Outhaul down.
to_journal() { # line
	printf '%s\n' "$1" >&4
}

# Runs a command inside the update directory: see in_real_dir.
in_update_dir() { # command...
	in_real_dir "$UPDATE_DIR" "$@"
}

# Writes the status file Outhaul reads: made in WORK_DIR and renamed into the
# update directory, replacing whatever has that name (rename does not follow
# a link). It fails on a directory there. finished is only on final states.
write_status() { # state from to message
	_tmp=$(mktemp "$WORK_DIR/.status.XXXXXX") || return 1
	if {
		printf 'state=%s\nfrom=%s\nto=%s\nmessage=%s\nstarted=%s\n' "$1" "$2" "$3" "$4" "$STARTED"
		if [ "$1" != running ]; then printf 'finished=%s\n' "$(date +%s)"; fi
	} > "$_tmp" && chmod 0644 "$_tmp" && in_update_dir mv -fT "$_tmp" status; then
		return 0
	fi
	rm -f "$_tmp"
	return 1
}

# A field of the running binary's `outhaul version`; empty when it has none.
build_field() { # name
	"$BIN" version 2>/dev/null | sed -n "s/^$1=//p"
}

source_commit() {
	git -C "$SRC_DIR" rev-parse HEAD
}

short() { # commit
	if [ -n "$1" ]; then printf '%.7s\n' "$1"; else printf 'an unknown build\n'; fi
}

# Puts the Go toolchain go.mod asks for on PATH. An earlier update's copy in
# CACHE_DIR is used when it is that version; otherwise install_go_toolchain
# downloads it, and the download replaces whatever CACHE_DIR held.
cached_go_toolchain() { # version
	_go=$CACHE_DIR/go$1
	if [ "$("$_go/bin/go" env GOVERSION 2>/dev/null)" = "go$1" ]; then
		PATH=$_go/bin:$PATH
		GOTOOLCHAIN=local
		export PATH GOTOOLCHAIN
		step_ok "Go $1 (kept from an earlier update)"
		return 0
	fi
	install_go_toolchain "$1"
	# A matching go was already on PATH: nothing was downloaded.
	if [ -z "${GOROOT_TMP:-}" ] || [ ! -d "$GOROOT_TMP/go" ]; then return 0; fi
	mkdir -p "$CACHE_DIR"
	rm -rf "${CACHE_DIR:?}"/go[0-9]*
	# By way of a temp name, so an interrupted move is never taken for a toolchain.
	if mv "$GOROOT_TMP/go" "$_go.partial" && mv "$_go.partial" "$_go"; then
		PATH=$_go/bin:$PATH
		export PATH
		step_ok "Go $1 kept in $CACHE_DIR for the next update"
	else
		note "could not keep Go $1 in $CACHE_DIR; the next update downloads it again"
	fi
}

# Pulls the images the new build runs, so a slow download happens before
# Outhaul stops rather than in the health window: Outhaul reconciles Traefik
# before /healthz answers. cloudflared only where the tunnel runs.
pull_images() { # binary
	_images=$("$1" images) || die "the new build could not list its images"
	_traefik=$(printf '%s\n' "$_images" | awk '$1 == "traefik" { print $2 }')
	_cloudflared=$(printf '%s\n' "$_images" | awk '$1 == "cloudflared" { print $2 }')
	[ -n "$_traefik" ] || die "the new build lists no Traefik image"
	_was=$("$BIN" images 2>/dev/null | awk '$1 == "traefik" { print $2 }')
	if [ -n "$_was" ] && [ "$_was" != "$_traefik" ]; then
		note "Traefik's image changes from $_was to $_traefik."
		note "This update restarts Traefik: every app is unreachable for a few seconds."
	fi
	spinner_start "pulling $_traefik"
	docker pull "$_traefik" >>"$LOGFILE" 2>&1 || die "could not pull $_traefik"
	spinner_stop 0
	if [ -n "$_cloudflared" ] && docker container inspect outhaul-cloudflared >/dev/null 2>&1; then
		spinner_start "pulling $_cloudflared"
		docker pull "$_cloudflared" >>"$LOGFILE" 2>&1 || die "could not pull $_cloudflared"
		spinner_stop 0
	fi
	step_ok "images pulled"
}

# Copies src over dest by way of a temp file and a rename: a new inode, so a
# running program, or the shell reading this script, keeps the old file.
# The temp file is dest.new, or tmp when given. cp -p keeps the owner and
# mode, and -T replaces dest itself, whatever it is.
replace_file() { # src dest [mode [tmp]]
	_rf=${4:-$2.new}
	rm -f "$_rf" || return 1
	cp -p "$1" "$_rf" || return 1
	if [ -n "${3:-}" ]; then chmod "$3" "$_rf" || return 1; fi
	mv -fT "$_rf" "$2"
}

# Keeps the running version in WORK_DIR/previous: the binary,
# outhaul.service, and the database with its -wal and -shm files when
# present. Outhaul must be stopped. Built in previous.new, so previous/ is
# always a whole copy. Every step is checked: callers test its status.
snapshot() {
	_new=$WORK_DIR/previous.new
	rm -rf "$_new" || return 1
	mkdir -m 0700 "$_new" || return 1
	cp -p "$BIN" "$_new/outhaul" || return 1
	if [ -f "$UNIT_DIR/outhaul.service" ]; then
		cp -p "$UNIT_DIR/outhaul.service" "$_new/outhaul.service" || return 1
	fi
	for _f in outhaul.db outhaul.db-wal outhaul.db-shm; do
		if [ -f "$DATA_DIR/$_f" ]; then cp -p "$DATA_DIR/$_f" "$_new/$_f" || return 1; fi
	done
	rm -rf "$WORK_DIR/previous" || return 1
	mv "$_new" "$WORK_DIR/previous"
}

# Puts back what snapshot kept. A -wal or -shm file the snapshot does not
# have is the new version's, and SQLite would replay it onto the old
# database: it is removed. The data dir belongs to outhaul, so the database
# files are copied in WORK_DIR and renamed into it (cp -p keeps them
# outhaul's). Every step is checked: callers test its status.
restore_snapshot() {
	_p=$WORK_DIR/previous
	[ -f "$_p/outhaul" ] || return 1
	replace_file "$_p/outhaul" "$BIN" || return 1
	if [ -f "$_p/outhaul.service" ]; then
		replace_file "$_p/outhaul.service" "$UNIT_DIR/outhaul.service" || return 1
	else
		rm -f "$UNIT_DIR/outhaul.service" || return 1
	fi
	for _f in outhaul.db outhaul.db-wal outhaul.db-shm; do
		if [ -f "$_p/$_f" ]; then
			replace_file "$_p/$_f" "$DATA_DIR/$_f" '' "$WORK_DIR/.restore" || return 1
		elif [ "$_f" != outhaul.db ]; then
			rm -f "$DATA_DIR/$_f" || return 1
		fi
	done
}

install_new() {
	replace_file "$SRC_DIR/outhaul" "$BIN" 0755 || return 1
	replace_file "$SRC_DIR/deploy/outhaul.service" "$UNIT_DIR/outhaul.service" 0644
}

# Reads response headers on stdin: true when they carry X-Outhaul-Commit:
# <commit>. An unstamped build (empty commit) sends no such header, so then
# any 200 counts.
answers_commit() { # commit
	awk -v want="$1" '
		{ sub(/\r$/, "") }
		NR == 1 && want == "" && $2 == "200" { ok = 1 }
		{
			i = index($0, ":")
			if (want == "" || i == 0 || tolower(substr($0, 1, i - 1)) != "x-outhaul-commit") next
			v = substr($0, i + 1)
			gsub(/^[ \t]+|[ \t]+$/, "", v)
			if (v == want) ok = 1
		}
		END { exit !ok }'
}

# True once the Outhaul answering on the admin port reports commit: up to 60
# one-second attempts.
new_version_healthy() { # commit port
	_i=0
	while [ "$_i" -lt 60 ]; do
		if curl -sS --max-time 2 -D - -o /dev/null "http://127.0.0.1:$2/healthz" 2>/dev/null | answers_commit "$1"; then
			return 0
		fi
		sleep 1
		_i=$((_i + 1))
	done
	return 1
}

# The commands to put the previous version back by hand.
recovery_help() {
	_p=$WORK_DIR/previous
	printf 'To recover, run as root on the server:\n'
	printf '  systemctl stop outhaul\n'
	if [ "$SNAPSHOT_OK" = 1 ]; then
		printf '  cp -p %s %s\n' "$_p/outhaul" "$BIN"
		if [ -f "$_p/outhaul.service" ]; then
			printf '  cp -p %s %s\n' "$_p/outhaul.service" "$UNIT_DIR/outhaul.service"
		fi
		for _f in outhaul.db outhaul.db-wal outhaul.db-shm; do
			if [ -f "$_p/$_f" ]; then
				printf '  cp -p %s %s\n' "$_p/$_f" "$DATA_DIR/$_f"
			elif [ "$_f" != outhaul.db ]; then
				printf '  rm -f %s\n' "$DATA_DIR/$_f"
			fi
		done
		printf '  systemctl daemon-reload\n'
	fi
	printf '  systemctl start outhaul\n'
	printf 'then check: journalctl -u outhaul -n 50 --no-pager\n'
}

# Logs the outcome, in the log and the journal, and writes the final status.
# Not being able to write it does not change the outcome.
finish() { # state message
	STAGE=done
	printf '%s\n' "$2"
	to_journal "$1: $2"
	if ! write_status "$1" "$FROM" "$TO" "$2"; then
		note "could not write $UPDATE_DIR/status: the admin UI does not show this result"
	fi
}

# The new version is not running right: put the previous one back, start it,
# and check that it answers. It also runs from the EXIT trap, so it checks
# every step itself, and nothing interrupts it.
roll_back() { # reason
	set +e
	trap '' INT TERM
	STAGE=rollback
	printf '%s\n' "$1"
	printf 'the last lines of the journal:\n'
	journalctl -u outhaul -n 20 --no-pager
	printf '  .. rolling back to %s\n' "$(short "$FROM")"
	systemctl stop outhaul
	_restored=1
	if [ "$SNAPSHOT_OK" = 1 ] && ! restore_snapshot; then
		_restored=0
		note "could not put back every file from $WORK_DIR/previous"
	fi
	systemctl daemon-reload
	systemctl start outhaul
	if [ "$_restored" = 1 ] && new_version_healthy "$FROM" "$PORT"; then
		finish rolled_back "$1; rolled back to $(short "$FROM")"
	else
		recovery_help
		recovery_help >&4
		finish failed "$1, and rolling back to $(short "$FROM") failed too. See $LOGFILE on the server for how to recover."
	fi
}

# Every exit ends with a final status. Before Outhaul is stopped, nothing has
# changed: build_failed. Between the stop and a healthy new version: roll
# back. Once healthy: updated.
update_exit() {
	_rc=$?
	set +e
	case ${STAGE:-} in
	build)
		if [ "$_rc" -gt 128 ]; then printf 'the updater was stopped\n'; fi
		finish build_failed 'the build failed; nothing changed. See the log.' ;;
	swap)
		if [ "$_rc" -gt 128 ]; then
			roll_back "the updater was stopped before the new version was confirmed"
		else
			roll_back "the updater failed unexpectedly (exit $_rc)"
		fi ;;
	healthy) finish updated "updated from $(short "$FROM") to $(short "$TO")" ;;
	esac
	cleanup
}

update_main() {
	# No questions: every installer prompt reads EOF and takes its default.
	exec 3<>/dev/null
	# The journal, for to_journal.
	exec 4>&2
	# An updater installed by an earlier installer has no directory of its own.
	mkdir -p "$WORK_DIR"
	chmod 0700 "$WORK_DIR"
	# A fresh log: a new file, renamed into place.
	_log=$(mktemp "$WORK_DIR/.log.XXXXXX")
	chmod 0644 "$_log"
	mv -fT "$_log" "$WORK_DIR/log"
	exec >>"$WORK_DIR/log" 2>&1
	LOGFILE=$WORK_DIR/log
	to_journal "the log of this update: $LOGFILE"
	# Outhaul reads it as the update directory's "log": a second name (a hard
	# link) for the same file, so the updater never opens it by that name.
	if ! in_update_dir ln -fT "$LOGFILE" log; then
		in_update_dir rm -f log 2>/dev/null || true
		note "could not show the log in $UPDATE_DIR"
	fi

	STARTED=$(date +%s)
	FROM=$(build_field commit)
	TO=''
	SNAPSHOT_OK=0
	_repo=$(build_field repo)
	if [ -n "$_repo" ]; then OUTHAUL_REPO=$_repo; fi
	PORT=$(admin_port "${OUTHAUL_LISTEN_ADDR:-:8080}")
	STAGE=build
	trap update_exit EXIT
	trap 'exit 130' INT
	trap 'exit 143' TERM
	if ! write_status running "$FROM" '' updating; then
		# Outhaul would not know an update is running: start none.
		STAGE=done
		in_update_dir rm -f request 2>/dev/null || true
		printf 'could not write %s/status; nothing changed\n' "$UPDATE_DIR"
		to_journal "could not write $UPDATE_DIR/status; nothing changed"
		exit 1
	fi
	# Only now: Outhaul holds work while the request exists or the status says running.
	in_update_dir rm -f request
	printf 'Outhaul updater, %s\n' "$(date -u '+%Y-%m-%d %H:%M:%S UTC')"
	printf 'running %s; updating to the newest main of %s\n' "$(short "$FROM")" "$OUTHAUL_REPO"

	fetch_source
	TO=$(source_commit)
	[ -n "$TO" ] || die "could not read the commit of the clone"
	if [ "$TO" = "$FROM" ]; then
		finish up_to_date "$(short "$TO") is the newest main"
		exit 0
	fi
	step_ok "main is at $(short "$TO")"

	_gover=$(go_version_from_gomod "$SRC_DIR/go.mod")
	[ -n "$_gover" ] || die "no Go version in go.mod"
	maybe_add_swap "$(read_meminfo_kb MemTotal)" "$(read_meminfo_kb SwapTotal)"
	cached_go_toolchain "$_gover"
	build_binary
	remove_build_swap
	pull_images "$SRC_DIR/outhaul"

	# From here on, a failure puts the previous version back.
	STAGE=swap
	printf '  .. stopping outhaul\n'
	if ! systemctl stop outhaul; then roll_back "outhaul did not stop"; exit 1; fi
	if ! snapshot; then roll_back "could not keep a copy of the running version"; exit 1; fi
	SNAPSHOT_OK=1
	step_ok "kept the running version in $WORK_DIR/previous"
	# An earlier updater kept its copies in the update directory.
	in_update_dir rm -rf previous || true
	if ! install_new; then roll_back "could not install the new version"; exit 1; fi
	if ! systemctl daemon-reload; then roll_back "systemctl daemon-reload failed"; exit 1; fi
	printf '  .. starting %s\n' "$(short "$TO")"
	if ! systemctl start outhaul || ! new_version_healthy "$TO" "$PORT"; then
		roll_back "the new version did not become healthy"
		exit 1
	fi
	STAGE=healthy
	step_ok "$(short "$TO") is up"

	# A later installer refreshes the updater itself from the new tree.
	if command -v install_updater >/dev/null 2>&1; then
		set +e
		(set -e; install_updater "$SRC_DIR")
		_rc=$?
		set -e
		if [ "$_rc" != 0 ]; then note "could not refresh the updater (exit $_rc); it stays as it was"; fi
	fi
	finish updated "updated from $(short "$FROM") to $(short "$TO")"
}

if [ "${OUTHAUL_UPDATER_LIB:-0}" != 1 ]; then
	update_main
fi
