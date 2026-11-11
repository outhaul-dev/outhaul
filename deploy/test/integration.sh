#!/usr/bin/env sh
# The unattended installer, end to end, on a real systemd, Docker and service.
#
#   sh deploy/test/integration.sh
#
# Not run by CI. It builds the Debian image in deploy/test/Dockerfile.debian,
# starts it privileged with systemd as PID 1, mounts a clone of this checkout
# read-only at /src, and runs deploy/bootstrap.sh inside it:
#
#   1. a first install at this checkout's commit, unattended, from
#      file:///src, with an extra root-owned env file, SSH off, the pinned
#      nixpacks and a setup-URL file;
#   2. the same run again, which must not rebuild or restart anything;
#   3. an upgrade to a second (empty) commit in a copy at /srv/repo;
#   4. an upgrade to a third commit whose serve() fails at once, which must
#      roll back to the second.
#
# --ingress proxy is not run: it needs a real ACME server.
#
# The container and the temp clone are removed on exit. Prints one ok/FAIL
# line per assertion, and INTEGRATION PASS when every one holds.
set -eu

root=$(CDPATH='' cd -- "$(dirname -- "$0")/../.." && pwd)
img=outhaul-installtest
name=outhaul-installtest-$$
tmp=$(mktemp -d)
cid=''

cleanup() {
	_rc=$?
	if [ -n "$cid" ]; then docker rm -f "$cid" >/dev/null 2>&1 || true; fi
	rm -rf "$tmp"
	exit "$_rc"
}
trap cleanup EXIT

fail() { printf 'FAIL %s\n' "$1" >&2; exit 1; }
pass() { printf 'ok   %s\n' "$1"; }

# Runs a command in the container.
x() { docker exec "$cid" "$@"; }

# Checks a name, then a command's exit status.
check() {
	_name=$1
	shift
	if "$@"; then pass "$_name"; else fail "$_name"; fi
}

check_eq() { # name expected actual
	if [ "$2" = "$3" ]; then pass "$1"; else fail "$1 (expected [$2], got [$3])"; fi
}

check_contains() { # name haystack needle
	case "$2" in
	*"$3"*) pass "$1" ;;
	*) fail "$1 (missing [$3] in [$2])" ;;
	esac
}

# Retries a command for up to 60 seconds, then checks it.
wait_for() { # name, then command
	_name=$1
	shift
	_i=0
	while [ "$_i" -lt 60 ]; do
		if "$@" >/dev/null 2>&1; then pass "$_name"; return 0; fi
		sleep 1
		_i=$((_i + 1))
	done
	fail "$_name"
}

installed_commit() { x /usr/local/bin/outhaul version | sed -n 's/^commit=//p'; }
previous_commit() { x /var/lib/outhaul-updater/previous/outhaul version | sed -n 's/^commit=//p'; }
log_lines() { x sh -c 'wc -l < /var/log/outhaul-install.log' | tr -d ' '; }

# The installer's output is shown and also kept per run; the exit status is
# the installer's, not cat's.
run_install_repo() { # repo, then the installer's arguments
	_repo=$1
	shift
	set +e
	docker exec -i "$cid" env OUTHAUL_REPO="$_repo" \
		sh /src/deploy/bootstrap.sh "$@" </dev/null >"$tmp/install.out" 2>&1
	_rc=$?
	set -e
	cat "$tmp/install.out"
	return "$_rc"
}
run_install() { run_install_repo file:///src "$@"; }

# The unattended answers every run uses. The env file is the same one, so a
# re-run sees no change; the setup file proves the one-time URL was written.
install_here() { # ref
	run_install --non-interactive --ref "$1" --ingress local --git-ssh off \
		--nixpacks on --firewall leave --env-file /etc/outhaul/test.env \
		--setup-url-file /root/setup
}
install_copy() { # ref
	run_install_repo file:///srv/repo --non-interactive --ref "$1" \
		--ingress local --git-ssh off --nixpacks on --firewall leave \
		--env-file /etc/outhaul/test.env --setup-url-file /root/setup
}

# A linked worktree's .git is a file pointing at the host's main repository;
# that path does not exist in the container. Clone to a normal repository and
# mount the clone instead.
printf '== cloning the checkout\n'
git clone --no-hardlinks "$root" "$tmp/repo"
ref=$(git -C "$tmp/repo" rev-parse HEAD)

printf '== building %s\n' "$img"
docker build -f "$root/deploy/test/Dockerfile.debian" -t "$img" "$root"

printf '== starting the systemd container\n'
# --cgroupns=host: systemd exits at once in a private cgroup namespace here,
# before it can start a single unit.
cid=$(docker run -d --name "$name" --privileged --cgroupns=host \
	--tmpfs /run --tmpfs /run/lock \
	-v /sys/fs/cgroup:/sys/fs/cgroup:rw -v "$tmp/repo:/src:ro" "$img")

wait_for "systemd is up" x sh -c \
	'case $(systemctl is-system-running 2>/dev/null) in running | degraded) exit 0 ;; *) exit 1 ;; esac'
wait_for "Docker answers" x docker info

# git refuses a repository owned by another user; the mounts are the host's.
# The file:// transport also checks the .git directory it reads.
x git config --global --add safe.directory /src
x git config --global --add safe.directory /src/.git
x git config --global --add safe.directory /srv/repo
x git config --global --add safe.directory /srv/repo/.git
x mkdir -p /etc/outhaul

# A root-owned env file the installer must not rewrite. 0640 is readable by
# the service and writable only by root, which is what --env-file accepts.
x sh -c 'printf "OUTHAUL_IMAGE_KEEP=2\n" > /etc/outhaul/test.env
	chown root:root /etc/outhaul/test.env
	chmod 0640 /etc/outhaul/test.env'
check_eq "test.env starts root:root 0640" "root:root 640" \
	"$(x stat -c '%U:%G %a' /etc/outhaul/test.env)"

printf '\n== first install at %s\n' "$ref"
rc=0
install_here "$ref" || rc=$?
check_eq "first install exits 0" 0 "$rc"
check "service is active" x systemctl is-active --quiet outhaul
check_eq "installed commit is the checkout" "$ref" "$(installed_commit)"

envfiles=$(x systemctl show outhaul -p EnvironmentFiles)
check_contains "service loads install.env" "$envfiles" /etc/outhaul/install.env
check_contains "service loads test.env" "$envfiles" /etc/outhaul/test.env
check_eq "test.env is still root:root 0640" "root:root 640" \
	"$(x stat -c '%U:%G %a' /etc/outhaul/test.env)"

check "no ufw was installed" x sh -c '! command -v ufw >/dev/null 2>&1'
check "no get.docker.com in the log" x sh -c \
	'! grep -q get.docker.com /var/log/outhaul-install.log'
check "nothing listens on 2222" x sh -c \
	"! ss -ltn | grep -Eq '[:.]2222[[:space:]]'"
check_eq "the setup file is 0600" 600 "$(x stat -c '%a' /root/setup)"
check_eq "nixpacks is pinned to 1.41.0" 1.41.0 "$(x nixpacks --version | awk '{print $NF}')"
check "the install log ends OK" x sh -c \
	"tail -n 1 /var/log/outhaul-install.log | grep -q '^OK'"

printf '\n== second run at the same commit\n'
ts=$(x systemctl show outhaul -p ActiveEnterTimestamp --value)
ino=$(x stat -c '%i' /usr/local/bin/outhaul)
lines=$(log_lines)
rc=0
install_here "$ref" || rc=$?
check_eq "second run exits 0" 0 "$rc"
check_eq "the service was not restarted" "$ts" \
	"$(x systemctl show outhaul -p ActiveEnterTimestamp --value)"
check_eq "the binary was not replaced" "$ino" \
	"$(x stat -c '%i' /usr/local/bin/outhaul)"
delta=$(x sh -c "tail -n +$((lines + 1)) /var/log/outhaul-install.log")
check_contains "the log says it did not rebuild" "$delta" "is already installed; not rebuilding"
if printf '%s\n' "$delta" | grep -Eq 'git (init|fetch)|go build|building outhaul|fetching outhaul'; then
	fail "second run fetched or built"
else
	pass "second run did not fetch or build"
fi

printf '\n== upgrade to a second commit\n'
x cp -a /src /srv/repo
x git -C /srv/repo -c user.email=t@example.com -c user.name=t \
	commit --allow-empty -qm second
second=$(x git -C /srv/repo rev-parse HEAD)
check_eq "the second commit is new" 0 \
	"$([ "$second" != "$ref" ] && echo 0 || echo 1)"
rc=0
install_copy "$second" || rc=$?
check_eq "upgrade exits 0" 0 "$rc"
check_eq "the binary is the second commit" "$second" "$(installed_commit)"
check_eq "previous/outhaul is the first commit" "$ref" "$(previous_commit)"
check "service is active after the upgrade" x systemctl is-active --quiet outhaul
check "the upgrade log ends OK" x sh -c \
	"tail -n 1 /var/log/outhaul-install.log | grep -q '^OK'"

printf '\n== a third commit that fails at start rolls back\n'
x sed -i 's|^func serve() error {$|func serve() error {\n\treturn errors.New("integration test: failing at start")|' \
	/srv/repo/main.go
check "main.go now fails at start" x grep -q 'failing at start' /srv/repo/main.go
x git -C /srv/repo -c user.email=t@example.com -c user.name=t commit -qam third
failed=$(x git -C /srv/repo rev-parse HEAD)
lines=$(log_lines)
rc=0
install_copy "$failed" || rc=$?
check_eq "the failed start exits 1" 1 "$rc"
check_eq "the binary is still the second commit" "$second" "$(installed_commit)"
delta=$(x sh -c "tail -n +$((lines + 1)) /var/log/outhaul-install.log")
check_contains "the log reports the rollback" "$delta" "rolled back"
check "service is active at the second commit" x systemctl is-active --quiet outhaul

printf '\nINTEGRATION PASS\n'
