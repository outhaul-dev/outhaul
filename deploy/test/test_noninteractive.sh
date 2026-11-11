#!/usr/bin/env sh
# The unattended installer end to end. main runs in a subshell with every path
# in a temp tree, and the outside world (systemd, Docker, git, Go, journald)
# stubbed. The stubs record their calls, so the scenarios can prove what a run
# did and, just as often, what it did not do.
# shellcheck disable=SC2218 # later definitions stand in for the installer's functions
here=$(cd "$(dirname "$0")" && pwd)
. "$here/assert.sh"
OUTHAUL_INSTALLER_LIB=1 . "$here/../bootstrap.sh"

ROOT=$(mktemp -d)
# Two commits; OLD is installed first, NEW is the second ref.
OLD=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
NEW=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb

# Per-run knobs the stubs read.
TARGET=$OLD
TARGET_VERSION=''
HEALTHY_COMMITS=$OLD
BUILD_FAIL=0
NO_SETUP=0
RESTART_FAIL=0
BLOCKED_PORTS=''
LISTENING_PORTS=''

# The tree: every path main touches is under $T, as in test_update.sh.
setup() {
	T=$(mktemp -d "$ROOT/run.XXXXXX")
	LOGFILE=$T/log
	INSTALL_ENV=$T/etc/install.env
	ENV_FILE=$T/etc/outhaul.env
	SERVICE_DROPIN=$T/units/outhaul.service.d/10-install.conf
	# shellcheck disable=SC2034 # read by the installer's functions
	UPDATE_DROPIN=$T/units/outhaul-update.service.d/10-install.conf
	BIN=$T/bin/outhaul
	# shellcheck disable=SC2034 # read by the installer's functions
	UNIT_DIR=$T/units
	# shellcheck disable=SC2034 # read by the installer's functions
	DATA_DIR=$T/data
	# shellcheck disable=SC2034 # read by the installer's functions
	UPDATE_DIR=$T/data/update
	WORK_DIR=$T/work
	MARKER=$T/work/installed
	SRC_DIR=$T/src
	PROC_ROOT=$T/proc
	# shellcheck disable=SC2034 # read by the installer's functions
	LIB_DIR=$T/lib
	mkdir -p "$T/bin" "$T/units" "$T/data" "$T/work" "$T/proc/$$" "$T/lib" "$T/testbin"
	# The service's own environment, as root reads it from /proc. $$ is the
	# same in the test and in main's subshell under dash, bash --posix and
	# busybox sh.
	printf 'OUTHAUL_LISTEN_ADDR=:8080\0OUTHAUL_HTTPS_PORT=443\0OUTHAUL_UPDATES=main\0' >"$PROC_ROOT/$$/environ"
	printf '0\n' >"$T/stamp"
	# apt is the first apt-based step; its stub records the exported front end.
	cat >"$T/testbin/apt-get" <<EOF
#!/bin/sh
printf '%s\n' "\${DEBIAN_FRONTEND:-unset}" > '$T/frontend'
EOF
	chmod 0755 "$T/testbin/apt-get"
	PATH=$T/testbin:$PATH
	TARGET=$OLD TARGET_VERSION='' HEALTHY_COMMITS=$OLD
	BUILD_FAIL=0 NO_SETUP=0 BLOCKED_PORTS='' LISTENING_PORTS='' PUBLIC_OK=1
	RESTART_FAIL=0
	: >"$T/calls"
	unset OUTHAUL_REF
}

# main exits, so each run is a subshell under errexit, as under cloud-init.
run_main() {
	RC=0
	( set -e; main "$@" ) >"$T/stdout" 2>"$T/stderr" || RC=$?
}

# A stand-in binary: version and images as the real one prints them.
fake_outhaul() { # path
	cat >"$1" <<EOF
#!/bin/sh
case \$1 in
version) printf 'commit=%s\nrepo=%s\nlocal=false\nversion=%s\n' '$TARGET' '$OUTHAUL_REPO' '$TARGET_VERSION' ;;
images) printf 'traefik traefik:v1\ncloudflared cloudflare/cloudflared:1\n' ;;
*) exit 2 ;;
esac
EOF
	chmod 0755 "$1"
}

# --------------------------------------------------------------- stubs --

systemctl() {
	printf 'systemctl %s\n' "$*" >>"$T/calls"
	case "$*" in
	'is-active --quiet outhaul') [ -e "$T/up" ] ;;
	'show -p MainPID --value outhaul') printf '%s\n' "$$" ;;
	'show -p InvocationID --value outhaul') printf 'inv1\n' ;;
	*ActiveEnterTimestamp*) cat "$T/stamp" 2>/dev/null || printf '0\n' ;;
	*EnvironmentFiles*) sed -n 's/^EnvironmentFile=//p' "$SERVICE_DROPIN" 2>/dev/null ;;
	'stop outhaul') rm -f "$T/up" ;;
	'start outhaul' | 'enable --now outhaul')
		: >"$T/up"
		_n=$(cat "$T/stamp" 2>/dev/null || printf '0')
		printf '%s\n' "$((_n + 1))" >"$T/stamp"
		;;
	'restart outhaul')
		# A restart the new env files stop: systemd refuses it and the
		# service is left as it was.
		[ "$RESTART_FAIL" = 1 ] && return 1
		: >"$T/up"
		_n=$(cat "$T/stamp" 2>/dev/null || printf '0')
		printf '%s\n' "$((_n + 1))" >"$T/stamp"
		;;
	*) : ;;
	esac
}

docker() {
	printf 'docker %s\n' "$*" >>"$T/calls"
	case "$*" in
	'port outhaul-traefik') printf '443/tcp -> 0.0.0.0:443\n' ;;
	'container inspect outhaul-cloudflared') return 1 ;;
	*) : ;;
	esac
}

# /healthz answers with the installed binary's commit once the service is up,
# and only for a commit in HEALTHY_COMMITS. A required header must arrive on
# stdin. PUBLIC_OK=0 fails the check through Traefik (it uses --resolve).
curl() {
	printf 'curl %s\n' "$*" >>"$T/calls"
	if [ -n "${HEADER:-}" ]; then
		_body=$(cat)
		case "$_body" in *"$HEADER"*) ;; *) return 7 ;; esac
	fi
	if [ "${PUBLIC_OK:-1}" != 1 ]; then
		case "$*" in *--resolve*) return 7 ;; esac
	fi
	[ -e "$T/up" ] || return 7
	_c=$("$BIN" version 2>/dev/null | sed -n 's/^commit=//p')
	case " $HEALTHY_COMMITS " in
	*" $_c "*) printf 'HTTP/1.1 200 OK\r\nX-Outhaul-Commit: %s\r\n\r\n' "$_c" ;;
	*) return 7 ;;
	esac
}

sleep() { :; }

# A start prints both setup hints, with the same token in each, after the
# failure that made the run read the journal.
journalctl() {
	printf 'journalctl %s\n' "$*" >>"$T/calls"
	[ "$NO_SETUP" = 1 ] && return 0
	printf 'Jul 13 12:00:00 host outhaul[1]: level=error msg="listen tcp :8080: bind: address already in use"\n'
	printf 'Jul 13 12:00:00 host outhaul[1]:   open: https://o.example.com/setup?token=abc\n'
	printf 'Jul 13 12:00:00 host outhaul[1]:   or, over an SSH tunnel to port 8080: http://localhost:8080/setup?token=abc\n'
}

git() {
	printf 'git %s\n' "$*" >>"$T/calls"
	case "$*" in
	*'rev-parse HEAD'*) printf '%s\n' "$TARGET" ;;
	*'status --porcelain'*) : ;;
	*'ls-remote'*) printf '%s\tHEAD\n' "$TARGET" ;;
	*) : ;;
	esac
}

go() {
	printf 'go %s\n' "$*" >>"$T/calls"
	[ "$BUILD_FAIL" = 1 ] && return 1
	fake_outhaul "$SRC_DIR/outhaul"
}

fetch_source() { # target
	printf 'fetch %s\n' "$1" >>"$T/calls"
	SRC_DIR=$T/src
	rm -rf "$SRC_DIR"
	mkdir -p "$SRC_DIR/deploy"
	printf 'module example\n\ngo 1.26\n\ntoolchain go1.26.4\n' >"$SRC_DIR/go.mod"
	printf 'unit\n' >"$SRC_DIR/deploy/outhaul.service"
	: >"$SRC_DIR/deploy/update.sh"
	: >"$SRC_DIR/deploy/bootstrap.sh"
	: >"$SRC_DIR/deploy/outhaul-update.path"
	: >"$SRC_DIR/deploy/outhaul-update.service"
}

preflight() { printf 'preflight\n' >>"$T/calls"; }
ensure_docker() { printf 'ensure_docker\n' >>"$T/calls"; }
# The real ensure_git is apt's first caller; the stub stands in for it so the
# apt-get stub can record the DEBIAN_FRONTEND main exported.
ensure_git() {
	printf 'ensure_git\n' >>"$T/calls"
	apt-get install -y -qq git >>"$LOGFILE" 2>&1 || true
}
ensure_nixpacks() { printf 'ensure_nixpacks\n' >>"$T/calls"; }
maybe_add_swap() { printf 'swap %s\n' "$*" >>"$T/calls"; }
install_go_toolchain() {
	printf 'go-toolchain %s\n' "$1" >>"$T/calls"
	GOROOT_TMP=$T/goroot
	mkdir -p "$GOROOT_TMP"
}
migrate_slipway() { printf 'migrate_slipway\n' >>"$T/calls"; }
ensure_service_user() { printf 'ensure_service_user\n' >>"$T/calls"; }
setup_updater() { printf 'setup_updater %s\n' "${1:-}" >>"$T/calls"; }
network_bridge() { printf 'outhaul\n'; }
# Only proxy mode reaches the ACME check; record it so a scenario can prove
# it did not.
acme_preflight() { printf 'acme %s\n' "$1" >>"$T/acme"; }

# The ufw seam, decided per scenario.
ufw_active() { return 1; }
listening_ports() { printf '%s\n' "$LISTENING_PORTS"; }
ufw_would_block() { printf '%s\n' "$BLOCKED_PORTS"; }
ufw() { printf 'ufw %s\n' "$*" >>"$T/calls"; }
have_ss() { return 1; }

# A prompt is the failure this test exists to catch.
ask_yes_no() {
	printf 'PROMPT %s\n' "$*" >>"$T/prompts"
	return 1
}
ask_value() {
	printf 'PROMPT %s\n' "$*" >>"$T/prompts"
	printf '\n'
}
# Record a prompt's read (ask_* read into _a), and leave the installer's own
# `while read` loops alone. command read runs the builtin past this function.
read() {
	case "$*" in
	*' _a') printf 'READ\n' >>"$T/reads" ;;
	esac
	command read "$@"
}

# --env-file's checks ask for root ownership; the fixtures cannot be root's.
stat() {
	if [ "$1" = -c ] && [ "$2" = '%u %a' ]; then
		printf '0 600\n'
	else
		command stat "$@"
	fi
}

# ------------------------------------------------------------- helpers --

count() { grep -c -x "$1" "$T/calls" 2>/dev/null || true; }
inode() { ls -i "$1" | awk '{ print $1 }'; }
commit_of() { "$1" version 2>/dev/null | sed -n 's/^commit=//p'; }
log_has() { grep -c -- "$1" "$T/log" 2>/dev/null || true; }

LOCAL='--non-interactive --ingress local --git-ssh off --nixpacks on --firewall leave'

# ------------------------------------------------- 1. the first install --

setup
run_main $LOCAL --updates off --ref "$TARGET"
assert_eq "first install: exit 0" 0 "$RC"
assert_ok "first install: the service is up" [ -e "$T/up" ]
assert_ok "first install: the binary is installed" [ -x "$BIN" ]
assert_eq "first install: the marker records the commit" "$TARGET" "$(sed -n 's/^commit=//p' "$MARKER")"
assert_contains "install.env: the SSH listener is off" "$(cat "$INSTALL_ENV")" "OUTHAUL_SSH_ADDR=off"
assert_contains "install.env: updates off" "$(cat "$INSTALL_ENV")" "OUTHAUL_UPDATES=off"
assert_contains "the drop-in loads install.env" "$(cat "$SERVICE_DROPIN")" "EnvironmentFile=$INSTALL_ENV"
assert_fail "a non-interactive run never writes /etc/outhaul.env" [ -e "$ENV_FILE" ]
assert_eq "the log ends OK" "OK installed" "$(tail -1 "$T/log")"
assert_eq "the log is 0600" 600 "$(stat -c %a "$T/log")"
assert_eq "the log starts with the UTC time" 1 "$(head -1 "$T/log" | grep -cE '^[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z ')"
assert_eq "the log names this run's flags" 1 "$(head -1 "$T/log" | grep -c -- '--non-interactive')"
assert_eq "no setup URL in stdout" 0 "$(grep -c '/setup?token=' "$T/stdout" || true)"
assert_eq "nothing prompted" "" "$(cat "$T/prompts" 2>/dev/null)"
assert_eq "nothing read" "" "$(cat "$T/reads" 2>/dev/null)"
assert_eq "no ufw" 0 "$(grep -c '^ufw ' "$T/calls" || true)"
assert_eq "no get.docker.com" 0 "$(cat "$T/log" "$T/stdout" 2>/dev/null | grep -c 'get.docker.com' || true)"
assert_eq "apt saw DEBIAN_FRONTEND=noninteractive" noninteractive "$(cat "$T/frontend")"
assert_fail "the Go toolchain is removed without asking" [ -e "$T/goroot" ]

# ------------------------------------- 2. the same commit: nothing moves --

setup
printf 'OUTHAUL_EXAMPLE=1\n' >"$T/extra.env"
run_main $LOCAL --env-file "$T/extra.env" --ref "$TARGET"
assert_eq "same commit: the first run installs" 0 "$RC"
bin_inode=$(inode "$BIN")
env_inode=$(inode "$INSTALL_ENV")
stamp=$(systemctl show -p ActiveEnterTimestamp outhaul)
: >"$T/calls"
# Editing a file the drop-in loads changes nothing: the installer never reads it.
printf 'OUTHAUL_EXAMPLE=2\n' >"$T/extra.env"
run_main $LOCAL --env-file "$T/extra.env" --ref "$TARGET"
assert_eq "same commit: exit 0" 0 "$RC"
assert_eq "same commit: no git fetch" 0 "$(grep -c '^git .*fetch' "$T/calls" || true)"
assert_eq "same commit: no Go download" 0 "$(grep -c '^go ' "$T/calls" || true)"
assert_eq "same commit: no start" 0 "$(count 'systemctl start outhaul')"
assert_eq "same commit: no restart" 0 "$(count 'systemctl restart outhaul')"
assert_eq "same commit: no daemon-reload" 0 "$(count 'systemctl daemon-reload')"
assert_eq "same commit: the binary keeps its inode" "$bin_inode" "$(inode "$BIN")"
assert_eq "same commit: install.env keeps its inode" "$env_inode" "$(inode "$INSTALL_ENV")"
assert_eq "same commit: the service did not start again" "$stamp" "$(systemctl show -p ActiveEnterTimestamp outhaul)"
assert_fail "same commit: no env-file copy is left behind" [ -e "$WORK_DIR/env-previous" ]

# --------------------------------------------- 3. a changed flag restarts --

setup
run_main $LOCAL --updates off --ref "$TARGET"
assert_eq "changed flag: the first run installs" 0 "$RC"
: >"$T/calls"
run_main $LOCAL --updates main --ref "$TARGET"
assert_eq "changed flag: exit 0" 0 "$RC"
assert_eq "changed flag: exactly one restart" 1 "$(count 'systemctl restart outhaul')"
assert_contains "changed flag: install.env has the new value" "$(cat "$INSTALL_ENV")" "OUTHAUL_UPDATES=main"

# ------------------------- 4. a changed --env-file reloads and restarts --

setup
printf 'OUTHAUL_EXAMPLE=1\n' >"$T/extra.env"
run_main $LOCAL --ref "$TARGET"
assert_eq "changed env file: the first run installs" 0 "$RC"
: >"$T/calls"
run_main $LOCAL --env-file "$T/extra.env" --ref "$TARGET"
assert_eq "changed env file: exit 0" 0 "$RC"
assert_eq "changed env file: exactly one daemon-reload" 1 "$(count 'systemctl daemon-reload')"
assert_eq "changed env file: exactly one restart" 1 "$(count 'systemctl restart outhaul')"
assert_contains "changed env file: the drop-in loads it" "$(cat "$SERVICE_DROPIN")" "EnvironmentFile=$T/extra.env"
assert_contains "changed env file: systemd sees it" "$(systemctl show -p EnvironmentFiles outhaul)" "$T/extra.env"

# ----------------------------------------------------- 5. a new ref swaps --

setup
run_main $LOCAL --ref "$TARGET"
assert_eq "new ref: the first run installs" 0 "$RC"
TARGET=$NEW HEALTHY_COMMITS="$OLD $NEW"
run_main $LOCAL --ref "$TARGET"
assert_eq "new ref: exit 0" 0 "$RC"
assert_eq "new ref: the new binary" "$NEW" "$(commit_of "$BIN")"
assert_eq "new ref: the old binary is kept" "$OLD" "$(commit_of "$WORK_DIR/previous/outhaul")"

# --------------------------- 6. a new version that never answers rolls back --

setup
run_main $LOCAL --updates off --ref "$TARGET"
assert_eq "rollback: the first run installs" 0 "$RC"
TARGET=$NEW HEALTHY_COMMITS=$OLD
run_main $LOCAL --updates main --ref "$TARGET"
assert_eq "rollback: exit 1" 1 "$RC"
assert_eq "rollback: the old binary is back" "$OLD" "$(commit_of "$BIN")"
assert_contains "rollback: install.env is the old one" "$(cat "$INSTALL_ENV")" "OUTHAUL_UPDATES=off"
assert_contains "rollback: the log says so" "$(cat "$T/log")" "rolled back"
assert_eq "rollback: the log ends FAILED" "FAILED exit 1" "$(tail -1 "$T/log")"
# The failed new version's journal lines, printed and logged once, before the
# old version is started over them. They carry no setup URL or token.
assert_eq "rollback: the journal's failure line is printed" 1 "$(grep -c 'address already in use' "$T/stdout" || true)"
assert_eq "rollback: the journal's failure line is in the log" 1 "$(log_has 'address already in use')"
assert_eq "rollback: the journal's two setup lines are in the log, withheld" 2 "$(log_has '<setup URL withheld>')"
assert_eq "rollback: the URL is not in the log" 0 "$(log_has 'o\.example\.com/setup')"
assert_eq "rollback: the token is not in the log" 0 "$(log_has 'token=abc')"
assert_eq "rollback: the URL is not in stdout" 0 "$(grep -c '/setup?token=' "$T/stdout" || true)"
assert_eq "rollback: the token is not in stdout" 0 "$(grep -c 'token=abc' "$T/stdout" || true)"

# ------------------------- 7. a first install that never answers fails --

setup
HEALTHY_COMMITS=''
run_main $LOCAL --ref "$TARGET"
assert_eq "never answers: exit 1" 1 "$RC"
assert_fail "never answers: no marker" [ -e "$MARKER" ]
assert_eq "never answers: the journal's two lines are in the log, withheld" 2 "$(log_has '<setup URL withheld>')"
assert_eq "never answers: the URL is not in the log" 0 "$(log_has 'o\.example\.com/setup')"
assert_eq "never answers: the token is not in the log" 0 "$(log_has 'token=abc')"
assert_eq "never answers: the URL is not in stdout" 0 "$(grep -c '/setup?token=' "$T/stdout" || true)"
assert_eq "never answers: the token is not in stdout" 0 "$(grep -c 'token=abc' "$T/stdout" || true)"
assert_eq "never answers: the log ends FAILED" "FAILED exit 1" "$(tail -1 "$T/log")"

# --------------------------------------------------- 8. the setup file --

setup
run_main $LOCAL --setup-url-file "$T/setup" --ref "$TARGET"
assert_eq "setup file: exit 0" 0 "$RC"
assert_eq "setup file: mode 0600" 600 "$(stat -c %a "$T/setup")"
assert_eq "setup file: the open: URL" "https://o.example.com/setup?token=abc" "$(cat "$T/setup")"
# A run that starts nothing leaves the file alone.
NO_SETUP=1
run_main $LOCAL --setup-url-file "$T/setup" --ref "$TARGET"
assert_eq "setup file: a no-start run exits 0" 0 "$RC"
assert_ok "setup file: a no-start run leaves it" [ -e "$T/setup" ]
# A run that starts the service and sees no URL has nothing left to set up.
rm -f "$T/up"
run_main $LOCAL --setup-url-file "$T/setup" --ref "$TARGET"
assert_eq "setup file: a started run exits 0" 0 "$RC"
assert_fail "setup file: a start with no URL removes it" [ -e "$T/setup" ]

# ------------------------------------------------------- 9. exit codes --

setup
printf 'sentinel\n' >"$T/log"
run_main --non-interactive --git-ssh off
assert_eq "bad flags: exit 2" 2 "$RC"
assert_eq "bad flags: the log is untouched" sentinel "$(cat "$T/log")"
assert_eq "bad flags: the service is untouched" 0 "$(grep -c '^systemctl' "$T/calls" || true)"
assert_fail "bad flags: nothing started" [ -e "$T/up" ]

setup
BUILD_FAIL=1
run_main $LOCAL --ref "$TARGET"
assert_eq "failed build: exit 1" 1 "$RC"
assert_fail "failed build: no binary" [ -e "$BIN" ]
assert_contains "failed build: the log says so" "$(cat "$T/log")" "build failed"
assert_eq "failed build: the log ends FAILED" "FAILED exit 1" "$(tail -1 "$T/log")"

# ------------------- 10. a blocked firewall is refused before any change --

setup
BLOCKED_PORTS=5432 LISTENING_PORTS=5432
run_main --non-interactive --ingress local --git-ssh off --nixpacks on --firewall ufw --ref "$TARGET"
assert_eq "blocked firewall: exit 1" 1 "$RC"
assert_fail "blocked firewall: no binary" [ -e "$BIN" ]
assert_fail "blocked firewall: no service start" [ -e "$T/up" ]
assert_fail "blocked firewall: no install.env" [ -e "$INSTALL_ENV" ]
assert_contains "blocked firewall: the log says it refused" "$(cat "$T/log")" "refusing to enable ufw"
assert_eq "blocked firewall: nothing was fetched" 0 "$(grep -c '^fetch ' "$T/calls" || true)"
assert_eq "blocked firewall: nothing was built" 0 "$(grep -c '^go ' "$T/calls" || true)"
assert_eq "blocked firewall: no Docker pull" 0 "$(grep -c '^docker pull' "$T/calls" || true)"

# --------------------- 11. proxy mode skips the ACME DNS check ----------

setup
printf 'OUTHAUL_ACME_DNS_PROVIDER=azuredns\n' >"$T/proxy.env"
run_main --non-interactive --ingress proxy --public-url https://o.example.com \
	--acme-email a@example.com --env-file "$T/proxy.env" --ref "$TARGET"
assert_eq "proxy: exit 0" 0 "$RC"
assert_fail "proxy: DNS points at the proxy, so there is no ACME check" [ -e "$T/acme" ]
assert_contains "proxy: install.env carries the switches" "$(cat "$INSTALL_ENV")" "OUTHAUL_HTTP_PORT=off"

# ------------------ 12. a failed public check fails the unattended run ----

setup
PUBLIC_OK=0
run_main --non-interactive --ingress letsencrypt --public-url https://o.example.com \
	--acme-email a@example.com --git-ssh off --nixpacks on --firewall leave --ref "$TARGET"
assert_eq "public check: exit 1" 1 "$RC"
assert_fail "public check: no marker" [ -e "$MARKER" ]
assert_ok "public check: letsencrypt runs the ACME check" [ -e "$T/acme" ]
assert_eq "public check: the journal's two lines are in the log, withheld" 2 "$(log_has '<setup URL withheld>')"
assert_eq "public check: the URL is not in the log" 0 "$(log_has 'o\.example\.com/setup')"
assert_eq "public check: the token is not in stdout" 0 "$(grep -c 'token=abc' "$T/stdout" || true)"
assert_eq "public check: the log ends FAILED" "FAILED exit 1" "$(tail -1 "$T/log")"

# ------ 13. a no-build run whose new env files stop the service goes back --

# A no-build run rewrites install.env and the drop-ins before it restarts the
# service. If the new files stop it, the old ones must come back: there is no
# snapshot to restore, because nothing stopped the service.
setup
run_main $LOCAL --updates off --ref "$TARGET"
assert_eq "failed env change: the first run installs" 0 "$RC"
old_env=$(cat "$INSTALL_ENV")
old_dropin=$(cat "$SERVICE_DROPIN")
: >"$T/calls"
printf 'OUTHAUL_EXAMPLE=1\n' >"$T/extra.env"
RESTART_FAIL=1
run_main $LOCAL --updates main --env-file "$T/extra.env" --ref "$TARGET"
assert_eq "failed env change: exit 1" 1 "$RC"
assert_eq "failed env change: the old install.env is back" "$old_env" "$(cat "$INSTALL_ENV")"
assert_contains "failed env change: install.env still has the old setting" "$(cat "$INSTALL_ENV")" "OUTHAUL_UPDATES=off"
assert_eq "failed env change: the old service drop-in is back" "$old_dropin" "$(cat "$SERVICE_DROPIN")"
assert_eq "failed env change: the old update drop-in is back" "$old_dropin" "$(cat "$UPDATE_DROPIN")"
assert_eq "failed env change: the old configuration was started" "systemctl start outhaul" "$(tail -1 "$T/calls")"
assert_ok "failed env change: the service is running" [ -e "$T/up" ]
# The journal's last lines are the failed configuration's, printed and logged
# with the setup URL withheld.
assert_eq "failed env change: the journal's failure line is printed" 1 "$(grep -c 'address already in use' "$T/stdout" || true)"
assert_eq "failed env change: the journal's failure line is in the log" 1 "$(log_has 'address already in use')"
assert_eq "failed env change: the URL is not in the log" 0 "$(log_has 'o\.example\.com/setup')"
assert_eq "failed env change: the token is not in stdout" 0 "$(grep -c 'token=abc' "$T/stdout" || true)"
assert_eq "failed env change: the log ends FAILED" "FAILED exit 1" "$(tail -1 "$T/log")"

rm -rf "$ROOT"
[ "${TESTS_FAIL:-0}" -eq 0 ]
