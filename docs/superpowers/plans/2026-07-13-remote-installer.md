# Outhaul Remote Installer Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build `deploy/bootstrap.sh` — a single, self-contained POSIX-sh installer served at `https://outhaul.sh/install` that turns a fresh Debian VPS into a running Outhaul instance via `curl -fsSL https://outhaul.sh/install | sh`, with a maximalist-but-degrading terminal UI, an interactive ingress-mode wizard, build-from-source, SSH-safe firewalling, DNS/port preflight, a low-RAM swap guard, and a post-install health check.

**Architecture:** One self-contained POSIX-sh script (runs under Debian `dash`, no bashisms). It is structured as a library of small functions plus a `main` orchestrator; a source-guard lets the test suite `source` it to unit-test the pure helpers without running `main`. Pure logic (Go-version parse, firewall-port derivation, swap decision, env-file generation, color-level detection, prompts) is unit-tested with a tiny zero-dependency POSIX harness. System-effecting steps are verified end-to-end in a systemd-enabled Debian container. Because there is no binary release channel, the script clones the repo and builds with a Go toolchain pinned to `go.mod` and downloaded from go.dev. Replaces the checkout-only `deploy/install.sh`; the systemd unit `deploy/outhaul.service` is unchanged.

**Tech Stack:** POSIX sh (`dash`-compatible), `ufw`, systemd, Docker (`get.docker.com`), Nixpacks, Go toolchain from go.dev, `shellcheck`, Docker-based integration harness (`jrei/systemd-debian` or `debian:13` + systemd).

**Spec:** `docs/superpowers/specs/2026-07-13-remote-installer-design.md`

---

## File Structure

- **Create** `deploy/bootstrap.sh` — the installer. All runtime logic lives here (must be one self-contained file, since it is served whole). Ends with a source-guard so tests can load its functions.
- **Create** `deploy/test/assert.sh` — minimal POSIX assertion helpers (`assert_eq`, `assert_ok`, `assert_fail`, `assert_contains`) and a pass/fail tally.
- **Create** `deploy/test/run.sh` — discovers and runs `deploy/test/test_*.sh`, prints a summary, exits non-zero on failure.
- **Create** `deploy/test/test_*.sh` — one unit-test file per pure-helper task.
- **Create** `deploy/test/integration.sh` — Docker-based end-to-end test (all three ingress modes + non-TTY output).
- **Create** `.github/workflows/installer.yml` — CI: `shellcheck` + unit tests (and integration on a schedule/manual).
- **Delete** `deploy/install.sh` — superseded (done in the final task, after parity is confirmed).
- **Unchanged** `deploy/outhaul.service`.

Global UI state set once by `init_ui`: `COLOR` (0=plain,1=16,2=256,3=truecolor), `UNICODE` (0/1), `WIDTH` (columns). Prompts read from fd 3 (opened to `/dev/tty`).

---

## Task 1: Test harness + script skeleton with source-guard

**Files:**
- Create: `deploy/test/assert.sh`
- Create: `deploy/test/run.sh`
- Create: `deploy/bootstrap.sh`
- Create: `deploy/test/test_smoke.sh`

- [ ] **Step 1: Write the assertion helpers**

Create `deploy/test/assert.sh`:

```sh
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
```

- [ ] **Step 2: Write the test runner**

Create `deploy/test/run.sh`:

```sh
#!/usr/bin/env sh
# Runs every deploy/test/test_*.sh in its own shell, aggregates results.
set -u
here=$(cd "$(dirname "$0")" && pwd)
total=0 failed=0
for t in "$here"/test_*.sh; do
	[ -e "$t" ] || continue
	printf '\n== %s ==\n' "$(basename "$t")"
	out=$(TESTS_RUN=0 TESTS_FAIL=0 sh "$t"; echo "EXIT:$?")
	printf '%s\n' "$out" | grep -v '^EXIT:'
	code=$(printf '%s\n' "$out" | sed -n 's/^EXIT://p' | tail -1)
	[ "${code:-1}" -eq 0 ] || failed=$((failed+1))
	total=$((total+1))
done
printf '\n%d test file(s), %d failed\n' "$total" "$failed"
[ "$failed" -eq 0 ]
```

Each `test_*.sh` must `exit 1` if any assertion failed; the smoke test below shows the pattern.

- [ ] **Step 3: Write the script skeleton with the source-guard**

Create `deploy/bootstrap.sh`:

```sh
#!/usr/bin/env sh
# Outhaul remote installer. Served at https://outhaul.sh/install.
# Usage: curl -fsSL https://outhaul.sh/install | sh
# Runs under Debian's dash — keep it POSIX, no bashisms.
set -eu

OUTHAUL_REPO="${OUTHAUL_REPO:-https://github.com/outhaul-dev/outhaul.git}"

# ------------------------------------------------------------------ helpers --
# (functions added by later tasks live above main)

main() {
	printf 'outhaul installer\n'
}

# Run main only when executed, not when sourced by the test suite.
if [ "${OUTHAUL_INSTALLER_LIB:-0}" != 1 ]; then
	main "$@"
fi
```

- [ ] **Step 4: Write the smoke test**

Create `deploy/test/test_smoke.sh`:

```sh
#!/usr/bin/env sh
here=$(cd "$(dirname "$0")" && pwd)
. "$here/assert.sh"
OUTHAUL_INSTALLER_LIB=1 . "$here/../bootstrap.sh"

assert_ok "main is defined" command -v main >/dev/null

[ "${TESTS_FAIL:-0}" -eq 0 ]
```

- [ ] **Step 5: Run the suite, expect PASS**

Run: `sh deploy/test/run.sh`
Expected: `1 test file(s), 0 failed` and exit 0.

- [ ] **Step 6: Commit**

```bash
git add deploy/test/assert.sh deploy/test/run.sh deploy/bootstrap.sh deploy/test/test_smoke.sh
git commit -m "test: installer test harness + bootstrap.sh skeleton with source-guard"
```

---

## Task 2: Parse the pinned Go version from go.mod

**Files:**
- Modify: `deploy/bootstrap.sh` (add `go_version_from_gomod`)
- Create: `deploy/test/test_goversion.sh`

- [ ] **Step 1: Write the failing test**

Create `deploy/test/test_goversion.sh`:

```sh
#!/usr/bin/env sh
here=$(cd "$(dirname "$0")" && pwd)
. "$here/assert.sh"
OUTHAUL_INSTALLER_LIB=1 . "$here/../bootstrap.sh"

tmp=$(mktemp)
printf 'module x\n\ngo 1.26.4\n' > "$tmp"
assert_eq "reads go directive" "1.26.4" "$(go_version_from_gomod "$tmp")"

printf 'module x\n\ngo 1.26\n\ntoolchain go1.26.4\n' > "$tmp"
assert_eq "prefers toolchain" "1.26.4" "$(go_version_from_gomod "$tmp")"
rm -f "$tmp"

[ "${TESTS_FAIL:-0}" -eq 0 ]
```

- [ ] **Step 2: Run it, expect FAIL**

Run: `sh deploy/test/run.sh 2>&1 | grep -A2 goversion`
Expected: FAIL — `go_version_from_gomod: not found`.

- [ ] **Step 3: Implement `go_version_from_gomod`**

Add above `main` in `deploy/bootstrap.sh`:

```sh
# Prints the concrete Go version to download (e.g. 1.26.4). Prefers the
# `toolchain goX.Y.Z` directive; falls back to the `go X.Y[.Z]` directive.
go_version_from_gomod() {
	f=${1:-go.mod}
	v=$(awk '/^toolchain[ \t]+go[0-9]/ { sub(/^go/,"",$2); print $2; exit }' "$f")
	[ -n "$v" ] || v=$(awk '/^go[ \t]+[0-9]/ { print $2; exit }' "$f")
	printf '%s\n' "$v"
}
```

- [ ] **Step 4: Run it, expect PASS**

Run: `sh deploy/test/run.sh 2>&1 | grep -A2 goversion`
Expected: two `ok` lines.

- [ ] **Step 5: Commit**

```bash
git add deploy/bootstrap.sh deploy/test/test_goversion.sh
git commit -m "feat(installer): parse pinned Go version from go.mod"
```

---

## Task 3: Color-level detection (pure core + wrapper)

**Files:**
- Modify: `deploy/bootstrap.sh` (add `_color_level`, `detect_color_level`)
- Create: `deploy/test/test_color.sh`

- [ ] **Step 1: Write the failing test**

Create `deploy/test/test_color.sh`:

```sh
#!/usr/bin/env sh
here=$(cd "$(dirname "$0")" && pwd)
. "$here/assert.sh"
OUTHAUL_INSTALLER_LIB=1 . "$here/../bootstrap.sh"

# args: no_color is_tty colorterm tput_colors
assert_eq "NO_COLOR forces plain" "0" "$(_color_level 1 1 truecolor 256)"
assert_eq "non-tty forces plain"  "0" "$(_color_level '' 0 truecolor 256)"
assert_eq "truecolor -> 3"        "3" "$(_color_level '' 1 truecolor 256)"
assert_eq "256 -> 2"              "2" "$(_color_level '' 1 '' 256)"
assert_eq "8 -> 1"                "1" "$(_color_level '' 1 '' 8)"
assert_eq "none -> 0"             "0" "$(_color_level '' 1 '' 0)"

[ "${TESTS_FAIL:-0}" -eq 0 ]
```

- [ ] **Step 2: Run it, expect FAIL**

Run: `sh deploy/test/run.sh 2>&1 | grep -A6 color`
Expected: FAIL — `_color_level: not found`.

- [ ] **Step 3: Implement both functions**

Add above `main`:

```sh
# Pure: decide color level from explicit inputs (testable).
# 0=plain 1=16-color 2=256-color 3=truecolor
_color_level() { # no_color is_tty colorterm tput_colors
	[ -n "$1" ] && { echo 0; return; }
	[ "$2" != 1 ] && { echo 0; return; }
	case "$3" in *truecolor*|*24bit*) echo 3; return;; esac
	if [ "${4:-0}" -ge 256 ]; then echo 2
	elif [ "${4:-0}" -ge 8 ]; then echo 1
	else echo 0; fi
}

# Gathers the real environment and calls _color_level.
detect_color_level() {
	tc=$(tput colors 2>/dev/null || echo 0)
	if [ -t 1 ]; then tty=1; else tty=0; fi
	_color_level "${NO_COLOR:-}" "$tty" "${COLORTERM:-}" "$tc"
}
```

- [ ] **Step 4: Run it, expect PASS**

Run: `sh deploy/test/run.sh 2>&1 | grep -A6 color`
Expected: six `ok` lines.

- [ ] **Step 5: Commit**

```bash
git add deploy/bootstrap.sh deploy/test/test_color.sh
git commit -m "feat(installer): terminal color-level detection"
```

---

## Task 4: Derive the firewall port set

**Files:**
- Modify: `deploy/bootstrap.sh` (add `derive_firewall_ports`)
- Create: `deploy/test/test_firewall.sh`

- [ ] **Step 1: Write the failing test**

Create `deploy/test/test_firewall.sh`:

```sh
#!/usr/bin/env sh
here=$(cd "$(dirname "$0")" && pwd)
. "$here/assert.sh"
OUTHAUL_INSTALLER_LIB=1 . "$here/../bootstrap.sh"

# args: mode(a/b/c) gitport
assert_eq "mode a + git 2222" "22 80 443 2222" "$(derive_firewall_ports a 2222)"
assert_eq "mode b no ports"   "22"             "$(derive_firewall_ports b '')"
assert_eq "mode c + git 2222" "22 2222"        "$(derive_firewall_ports c 2222)"
assert_eq "mode a no git"     "22 80 443"      "$(derive_firewall_ports a '')"
assert_eq "dedup git=22"      "22 80 443"      "$(derive_firewall_ports a 22)"

[ "${TESTS_FAIL:-0}" -eq 0 ]
```

- [ ] **Step 2: Run it, expect FAIL**

Run: `sh deploy/test/run.sh 2>&1 | grep -A5 firewall`
Expected: FAIL — `derive_firewall_ports: not found`.

- [ ] **Step 3: Implement `derive_firewall_ports`**

Add above `main`:

```sh
# Prints the space-separated, sorted, de-duplicated port set to open.
# SSH (22) is ALWAYS included. Mode a adds 80/443. A non-empty git port is added.
derive_firewall_ports() { # mode gitport
	mode=$1; gitport=${2:-}
	set -- 22
	[ "$mode" = a ] && set -- "$@" 80 443
	[ -n "$gitport" ] && set -- "$@" "$gitport"
	printf '%s\n' "$@" | sort -n -u | paste -sd' ' -
}
```

- [ ] **Step 4: Run it, expect PASS**

Run: `sh deploy/test/run.sh 2>&1 | grep -A5 firewall`
Expected: five `ok` lines.

- [ ] **Step 5: Commit**

```bash
git add deploy/bootstrap.sh deploy/test/test_firewall.sh
git commit -m "feat(installer): derive SSH-safe firewall port set"
```

---

## Task 5: Low-RAM swap decision

**Files:**
- Modify: `deploy/bootstrap.sh` (add `need_swap`)
- Create: `deploy/test/test_swap.sh`

- [ ] **Step 1: Write the failing test**

Create `deploy/test/test_swap.sh`:

```sh
#!/usr/bin/env sh
here=$(cd "$(dirname "$0")" && pwd)
. "$here/assert.sh"
OUTHAUL_INSTALLER_LIB=1 . "$here/../bootstrap.sh"

# need_swap mem_kb swap_kb  -> exit 0 means "create swap"
assert_ok   "1GB no swap needs swap"   need_swap 1048576 0
assert_fail "4GB no swap is fine"      need_swap 4194304 0
assert_fail "1GB + 2GB swap is fine"   need_swap 1048576 2097152
assert_ok   "exactly under 2GiB"       need_swap 2097151 0

[ "${TESTS_FAIL:-0}" -eq 0 ]
```

- [ ] **Step 2: Run it, expect FAIL**

Run: `sh deploy/test/run.sh 2>&1 | grep -A4 swap`
Expected: FAIL — `need_swap: not found`.

- [ ] **Step 3: Implement `need_swap`**

Add above `main`:

```sh
# Returns success (0) when total RAM+swap is below 2 GiB and a build
# swapfile should be offered. Inputs are in KiB (as /proc/meminfo reports).
need_swap() { # mem_kb swap_kb
	total=$(( ${1:-0} + ${2:-0} ))
	[ "$total" -lt 2097152 ]
}
```

- [ ] **Step 4: Run it, expect PASS**

Run: `sh deploy/test/run.sh 2>&1 | grep -A4 swap`
Expected: four `ok` lines.

- [ ] **Step 5: Commit**

```bash
git add deploy/bootstrap.sh deploy/test/test_swap.sh
git commit -m "feat(installer): low-RAM swap decision helper"
```

---

## Task 6: Generate /etc/outhaul.env

**Files:**
- Modify: `deploy/bootstrap.sh` (add `write_env_file`)
- Create: `deploy/test/test_env.sh`

- [ ] **Step 1: Write the failing test**

Create `deploy/test/test_env.sh`:

```sh
#!/usr/bin/env sh
here=$(cd "$(dirname "$0")" && pwd)
. "$here/assert.sh"
OUTHAUL_INSTALLER_LIB=1 . "$here/../bootstrap.sh"

dest=$(mktemp)
# write_env_file dest mode url email sshaddr
write_env_file "$dest" a https://paas.dev me@x.com :2222
body=$(cat "$dest")
assert_contains "mode a writes url"   "$body" "OUTHAUL_PUBLIC_URL=https://paas.dev"
assert_contains "mode a writes email" "$body" "OUTHAUL_ACME_EMAIL=me@x.com"
assert_contains "writes ssh addr"     "$body" "OUTHAUL_SSH_ADDR=:2222"
assert_eq       "file is 0600"        "600" "$(stat -c '%a' "$dest")"

write_env_file "$dest" c '' '' :2222
body=$(cat "$dest")
case "$body" in *OUTHAUL_ACME_EMAIL*) echo "  FAIL mode c leaked email" >&2; TESTS_FAIL=$((TESTS_FAIL+1));; *) echo "  ok   mode c has no email";; esac
assert_contains "mode c keeps ssh"    "$body" "OUTHAUL_SSH_ADDR=:2222"
rm -f "$dest"

[ "${TESTS_FAIL:-0}" -eq 0 ]
```

- [ ] **Step 2: Run it, expect FAIL**

Run: `sh deploy/test/run.sh 2>&1 | grep -A6 test_env`
Expected: FAIL — `write_env_file: not found`.

- [ ] **Step 3: Implement `write_env_file`**

Add above `main`:

```sh
# Writes /etc/outhaul.env from wizard answers. Mode a includes the public
# URL + ACME email (enables HTTPS on first boot); other modes omit them.
# Comments go on their OWN line — systemd keeps an inline "# ..." as part of
# the value, which would corrupt e.g. the email.
write_env_file() { # dest mode url email sshaddr
	dest=$1 mode=$2 url=$3 email=$4 sshaddr=$5
	{
		printf '# Outhaul configuration — generated by the installer.\n'
		printf '# OUTHAUL_* overrides, one per line. Comments on their OWN line.\n'
		printf '# Edit, then: systemctl restart outhaul\n\n'
		if [ "$mode" = a ]; then
			printf 'OUTHAUL_PUBLIC_URL=%s\n' "$url"
			printf 'OUTHAUL_ACME_EMAIL=%s\n' "$email"
		fi
		[ -n "$sshaddr" ] && printf 'OUTHAUL_SSH_ADDR=%s\n' "$sshaddr"
	} > "$dest"
	chmod 0600 "$dest"
}
```

- [ ] **Step 4: Run it, expect PASS**

Run: `sh deploy/test/run.sh 2>&1 | grep -A6 test_env`
Expected: all `ok` lines, no FAIL.

- [ ] **Step 5: Commit**

```bash
git add deploy/bootstrap.sh deploy/test/test_env.sh
git commit -m "feat(installer): generate /etc/outhaul.env from wizard answers"
```

---

## Task 7: fd-based prompt helpers

**Files:**
- Modify: `deploy/bootstrap.sh` (add `ask_yes_no`, `ask_value`)
- Create: `deploy/test/test_prompt.sh`

Prompts read from fd 3 (the installer opens `/dev/tty` on fd 3 in `main`; tests redirect fd 3 from a file). Prompt text goes to stderr so it never pollutes captured stdout.

- [ ] **Step 1: Write the failing test**

Create `deploy/test/test_prompt.sh`:

```sh
#!/usr/bin/env sh
here=$(cd "$(dirname "$0")" && pwd)
. "$here/assert.sh"
OUTHAUL_INSTALLER_LIB=1 . "$here/../bootstrap.sh"

in=$(mktemp)

printf 'y\n' > "$in"; assert_ok   "explicit yes"        sh -c '. '"$here"'/../bootstrap.sh; ask_yes_no "?" n' 3<"$in" 2>/dev/null
printf 'n\n' > "$in"; assert_fail "explicit no"         sh -c '. '"$here"'/../bootstrap.sh; ask_yes_no "?" y' 3<"$in" 2>/dev/null
printf '\n'  > "$in"; assert_ok   "empty uses default y" sh -c '. '"$here"'/../bootstrap.sh; ask_yes_no "?" y' 3<"$in" 2>/dev/null

printf 'paas.example.dev\n' > "$in"
assert_eq "ask_value returns input" "paas.example.dev" "$(ask_value 'domain?' '' 3<"$in" 2>/dev/null)"
printf '\n' > "$in"
assert_eq "ask_value uses default"  "2222" "$(ask_value 'port?' '2222' 3<"$in" 2>/dev/null)"
rm -f "$in"

[ "${TESTS_FAIL:-0}" -eq 0 ]
```

Note: the sub-`sh -c` invocations set `OUTHAUL_INSTALLER_LIB` implicitly is not needed because they only call the function, but they DO run `main` guard — pass it via env. Prefix each with `OUTHAUL_INSTALLER_LIB=1`. Update the three lines accordingly:

```sh
printf 'y\n' > "$in"; assert_ok   "explicit yes"        env OUTHAUL_INSTALLER_LIB=1 sh -c '. '"$here"'/../bootstrap.sh; ask_yes_no "?" n' 3<"$in" 2>/dev/null
printf 'n\n' > "$in"; assert_fail "explicit no"         env OUTHAUL_INSTALLER_LIB=1 sh -c '. '"$here"'/../bootstrap.sh; ask_yes_no "?" y' 3<"$in" 2>/dev/null
printf '\n'  > "$in"; assert_ok   "empty uses default y" env OUTHAUL_INSTALLER_LIB=1 sh -c '. '"$here"'/../bootstrap.sh; ask_yes_no "?" y' 3<"$in" 2>/dev/null
```

- [ ] **Step 2: Run it, expect FAIL**

Run: `sh deploy/test/run.sh 2>&1 | grep -A6 prompt`
Expected: FAIL — `ask_yes_no`/`ask_value` not found.

- [ ] **Step 3: Implement the prompt helpers**

Add above `main`:

```sh
# Yes/no prompt read from fd 3. $2 is the default ('y' or 'n').
ask_yes_no() { # prompt default
	_p=$1; _d=$2
	if [ "$_d" = y ]; then _h='Y/n'; else _h='y/N'; fi
	printf '%s [%s] ' "$_p" "$_h" >&2
	read _a <&3 2>/dev/null || _a=''
	[ -z "$_a" ] && _a=$_d
	case "$_a" in [Yy]*) return 0;; *) return 1;; esac
}

# Free-text prompt read from fd 3; prints the answer (or default) to stdout.
ask_value() { # prompt default
	_p=$1; _d=$2
	if [ -n "$_d" ]; then printf '%s [%s] ' "$_p" "$_d" >&2; else printf '%s ' "$_p" >&2; fi
	read _a <&3 2>/dev/null || _a=''
	[ -z "$_a" ] && _a=$_d
	printf '%s\n' "$_a"
}
```

- [ ] **Step 4: Run it, expect PASS**

Run: `sh deploy/test/run.sh 2>&1 | grep -A6 prompt`
Expected: five `ok` lines.

- [ ] **Step 5: Commit**

```bash
git add deploy/bootstrap.sh deploy/test/test_prompt.sh
git commit -m "feat(installer): fd-based yes/no and value prompts"
```

---

## Task 8: UI init + static rendering primitives

**Files:**
- Modify: `deploy/bootstrap.sh` (add `init_ui`, `paint`, `hr`, `step_ok`, `step_fail`, `note`, `die`, `log_line`)
- Create: `deploy/test/test_render.sh`

- [ ] **Step 1: Write the failing test**

Create `deploy/test/test_render.sh`:

```sh
#!/usr/bin/env sh
here=$(cd "$(dirname "$0")" && pwd)
. "$here/assert.sh"
OUTHAUL_INSTALLER_LIB=1 . "$here/../bootstrap.sh"

# With COLOR=0 nothing should contain an ESC (\033) byte.
COLOR=0 UNICODE=0 WIDTH=80
esc=$(printf '\033')
out=$(paint 255 0 0 "hello")
assert_eq "plain paint has no escapes" "hello" "$out"
case "$(step_ok 'built' 2>&1)" in *"$esc"*) echo "  FAIL step_ok emitted ESC in plain mode" >&2; TESTS_FAIL=$((TESTS_FAIL+1));; *) echo "  ok   step_ok plain";; esac
assert_contains "step_ok shows label" "$(step_ok 'built')" "built"

# With truecolor, paint wraps in an escape.
COLOR=3
case "$(paint 255 0 0 hi)" in *"$esc"*) echo "  ok   truecolor paint has ESC";; *) echo "  FAIL truecolor paint missing ESC" >&2; TESTS_FAIL=$((TESTS_FAIL+1));; esac

[ "${TESTS_FAIL:-0}" -eq 0 ]
```

- [ ] **Step 2: Run it, expect FAIL**

Run: `sh deploy/test/run.sh 2>&1 | grep -A5 render`
Expected: FAIL — `paint`/`step_ok` not found.

- [ ] **Step 3: Implement UI init + primitives**

Add above `main`:

```sh
# Sets COLOR, UNICODE, WIDTH globals. Call once at startup.
init_ui() {
	COLOR=$(detect_color_level)
	case "${LC_ALL:-${LC_CTYPE:-${LANG:-}}}" in *[Uu][Tt][Ff]-8*|*[Uu][Tt][Ff]8*) UNICODE=1;; *) UNICODE=0;; esac
	[ "$COLOR" = 0 ] && UNICODE=0
	WIDTH=$(tput cols 2>/dev/null || echo 80)
	[ -n "$WIDTH" ] || WIDTH=80
}

# Apply an RGB (truecolor) / bold-magenta (16+) / plain color to text.
paint() { # r g b text...
	_r=$1; _g=$2; _b=$3; shift 3
	if [ "${COLOR:-0}" -ge 3 ]; then printf '\033[38;2;%d;%d;%dm%s\033[0m' "$_r" "$_g" "$_b" "$*"
	elif [ "${COLOR:-0}" -ge 1 ]; then printf '\033[1;35m%s\033[0m' "$*"
	else printf '%s' "$*"; fi
}

_c() { # color-code-or-plain: emit ANSI SGR $1 only when COLOR>=1
	[ "${COLOR:-0}" -ge 1 ] && printf '\033[%sm' "$1"
}

hr() {
	[ "${UNICODE:-0}" = 1 ] && ch='─' || ch='-'
	i=0; line=''
	while [ "$i" -lt "$((WIDTH<80?WIDTH:60))" ]; do line="$line$ch"; i=$((i+1)); done
	_c 90; printf '%s' "$line"; _c 0; printf '\n'
}

step_ok()   { [ "${UNICODE:-0}" = 1 ] && m='✔' || m='+'; _c '1;32'; printf '  %s ' "$m"; _c 0; printf '%s\n' "$1"; }
step_fail() { [ "${UNICODE:-0}" = 1 ] && m='✖' || m='x'; _c '1;31'; printf '  %s ' "$m"; _c 0; printf '%s\n' "$1"; }
note()      { _c 90; printf '    %s\n' "$1"; _c 0; }

# Raw log mirror (Task 20 wires LOGFILE); safe if unset.
log_line() { [ -n "${LOGFILE:-}" ] && printf '%s\n' "$*" >> "$LOGFILE" 2>/dev/null || true; }

die() { _c '1;31'; printf 'error: ' >&2; _c 0; printf '%s\n' "$1" >&2; log_line "ERROR: $1"; exit 1; }
```

- [ ] **Step 4: Run it, expect PASS**

Run: `sh deploy/test/run.sh 2>&1 | grep -A5 render`
Expected: all `ok`, no FAIL.

- [ ] **Step 5: Commit**

```bash
git add deploy/bootstrap.sh deploy/test/test_render.sh
git commit -m "feat(installer): UI init + color-aware rendering primitives"
```

---

## Task 9: Hero banner, spinner, and progress bar

**Files:**
- Modify: `deploy/bootstrap.sh` (add `hero`, `spinner_start`, `spinner_stop`, `progress_bar`)
- Create: `deploy/test/test_hero.sh`

- [ ] **Step 1: Write the failing test**

Create `deploy/test/test_hero.sh`:

```sh
#!/usr/bin/env sh
here=$(cd "$(dirname "$0")" && pwd)
. "$here/assert.sh"
OUTHAUL_INSTALLER_LIB=1 . "$here/../bootstrap.sh"

COLOR=0 UNICODE=0 WIDTH=100
assert_contains "wide hero mentions OUTHAUL/tagline" "$(hero)" "self-hosted PaaS"

WIDTH=40
assert_contains "narrow hero degrades to one line" "$(hero)" "Outhaul"

# progress_bar renders a percentage
COLOR=0 UNICODE=0 WIDTH=80
assert_contains "progress shows percent" "$(progress_bar 50 'build')" "50%"

[ "${TESTS_FAIL:-0}" -eq 0 ]
```

- [ ] **Step 2: Run it, expect FAIL**

Run: `sh deploy/test/run.sh 2>&1 | grep -A4 hero`
Expected: FAIL — `hero`/`progress_bar` not found.

- [ ] **Step 3: Implement hero, spinner, progress bar**

Add above `main`:

```sh
_hero_rgb() { case $1 in
	1) echo 247 120 186;; 2) echo 188 140 255;;
	3) echo 88 166 255;;  4) echo 63 185 80;; esac; }

# Gradient ASCII wordmark; degrades to a single line when narrow or plain.
hero() {
	if [ "${WIDTH:-80}" -lt 62 ]; then
		printf '\n'; _c '1;35'; printf '  Outhaul'; _c 0
		printf ' — self-hosted PaaS\n\n'; return
	fi
	printf '\n'
	i=0
	# shellcheck disable=SC2016
	printf '%s\n' \
'  ██████  ██    ██ ████████ ██   ██  █████  ██    ██ ██     ' \
'  ██   ██ ██    ██    ██    ██   ██ ██   ██ ██    ██ ██     ' \
'  ██   ██ ██    ██    ██    ███████ ███████ ██    ██ ██     ' \
'  ██████   ██████     ██    ██   ██ ██   ██  ██████  ███████' | while IFS= read -r line; do
		i=$((i+1))
		# read stops sharing $i with parent (subshell); recompute by row content length instead:
		:
		paint $(_hero_rgb "$i") "$line"; printf '\n'
	done
	_c 90; printf '   self-hosted PaaS · one binary, zero sprawl\n'; _c 0
	printf '\n'
}
```

Note: the `while` loop runs in a subshell (after the pipe), so `$i` resets there — that is fine because `i` is incremented inside the same subshell each iteration. Verify rows get colors 1–4 in order at Step 4; if the pipeline subshell proves flaky across shells, replace with a positional-parameter loop:

```sh
hero() {
	if [ "${WIDTH:-80}" -lt 62 ]; then
		printf '\n'; _c '1;35'; printf '  Outhaul'; _c 0; printf ' — self-hosted PaaS\n\n'; return
	fi
	printf '\n'
	set -- \
'  ██████  ██    ██ ████████ ██   ██  █████  ██    ██ ██     ' \
'  ██   ██ ██    ██    ██    ██   ██ ██   ██ ██    ██ ██     ' \
'  ██   ██ ██    ██    ██    ███████ ███████ ██    ██ ██     ' \
'  ██████   ██████     ██    ██   ██ ██   ██  ██████  ███████'
	i=0
	for line in "$@"; do i=$((i+1)); paint $(_hero_rgb "$i") "$line"; printf '\n'; done
	_c 90; printf '   self-hosted PaaS · one binary, zero sprawl\n'; _c 0; printf '\n'
}
```

Use the positional-parameter version (second one) as the implementation.

Then add the spinner and progress bar:

```sh
# Braille spinner on a background PID while a long step runs.
# Usage: spinner_start "message"; <work>; spinner_stop 0|1
SPIN_PID=''
spinner_start() {
	SPIN_MSG=$1
	if [ "${COLOR:-0}" = 0 ] || [ ! -t 1 ]; then printf '  .. %s\n' "$SPIN_MSG"; return; fi
	( frames='⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏'; [ "${UNICODE:-0}" = 1 ] || frames='|/-\\'
	  while :; do
		i=1
		while [ "$i" -le ${#frames} ]; do
			f=$(printf '%s' "$frames" | cut -c "$i")
			printf '\r  \033[35m%s\033[0m %s' "$f" "$SPIN_MSG"
			sleep 0.1; i=$((i+1))
		done
	  done ) &
	SPIN_PID=$!
}
spinner_stop() { # exitcode
	[ -n "${SPIN_PID:-}" ] && kill "$SPIN_PID" 2>/dev/null && wait "$SPIN_PID" 2>/dev/null
	SPIN_PID=''
	[ "${COLOR:-0}" = 0 ] && return 0
	printf '\r\033[K'
	if [ "${1:-0}" -eq 0 ]; then step_ok "$SPIN_MSG"; else step_fail "$SPIN_MSG"; fi
}

# A single-line gradient progress bar: progress_bar PERCENT LABEL
progress_bar() { # percent label
	pct=$1; label=${2:-}
	width=30; filled=$(( pct * width / 100 ))
	[ "$filled" -gt "$width" ] && filled=$width
	bar=''; i=0
	while [ "$i" -lt "$filled" ]; do bar="$bar#"; i=$((i+1)); done
	while [ "$i" -lt "$width" ]; do bar="$bar."; i=$((i+1)); done
	if [ "${COLOR:-0}" -ge 1 ]; then
		printf '\r  \033[32m%s\033[0m %3d%% %s' "$bar" "$pct" "$label"
	else
		printf '\r  %s %3d%% %s' "$bar" "$pct" "$label"
	fi
}
```

- [ ] **Step 4: Run it, expect PASS**

Run: `sh deploy/test/run.sh 2>&1 | grep -A4 hero`
Expected: three `ok` lines.

- [ ] **Step 5: Manual visual check (truecolor terminal)**

Run:
```bash
sh -c 'OUTHAUL_INSTALLER_LIB=1 . deploy/bootstrap.sh; init_ui; hero; spinner_start "compiling"; sleep 1; spinner_stop 0'
```
Expected: gradient wordmark, a spinner that resolves to a green check. (Skip cleanly if not on a TTY.)

- [ ] **Step 6: Commit**

```bash
git add deploy/bootstrap.sh deploy/test/test_hero.sh
git commit -m "feat(installer): gradient hero, braille spinner, progress bar"
```

---

## Task 10: Preflight checks (root, systemd, distro, arch, memory)

**Files:**
- Modify: `deploy/bootstrap.sh` (add `read_meminfo_kb`, `detect_distro`, `preflight`)
- Create: `deploy/test/test_preflight.sh`

- [ ] **Step 1: Write the failing test (pure parsing only)**

Create `deploy/test/test_preflight.sh`:

```sh
#!/usr/bin/env sh
here=$(cd "$(dirname "$0")" && pwd)
. "$here/assert.sh"
OUTHAUL_INSTALLER_LIB=1 . "$here/../bootstrap.sh"

os=$(mktemp)
printf 'ID=debian\nVERSION_ID="13"\nPRETTY_NAME="Debian GNU/Linux 13"\n' > "$os"
assert_eq "distro id from os-release" "debian" "$(detect_distro "$os")"
rm -f "$os"

mi=$(mktemp)
printf 'MemTotal:  1017880 kB\nSwapTotal:   524284 kB\n' > "$mi"
assert_eq "mem parsed"  "1017880" "$(read_meminfo_kb MemTotal "$mi")"
assert_eq "swap parsed" "524284"  "$(read_meminfo_kb SwapTotal "$mi")"
rm -f "$mi"

[ "${TESTS_FAIL:-0}" -eq 0 ]
```

- [ ] **Step 2: Run it, expect FAIL**

Run: `sh deploy/test/run.sh 2>&1 | grep -A4 preflight`
Expected: FAIL — `detect_distro`/`read_meminfo_kb` not found.

- [ ] **Step 3: Implement parsing helpers + preflight**

Add above `main`:

```sh
# Prints the numeric KiB value for a /proc/meminfo key (default file /proc/meminfo).
read_meminfo_kb() { # key file?
	awk -v k="$1:" '$1==k {print $2; exit}' "${2:-/proc/meminfo}"
}

# Prints the distro ID from an os-release file (default /etc/os-release).
detect_distro() { # file?
	awk -F= '$1=="ID"{gsub(/"/,"",$2); print $2; exit}' "${1:-/etc/os-release}"
}

# Full preflight. Aborts on hard failures; warns (via fd 3 confirm) on soft ones.
preflight() {
	[ "$(id -u)" -eq 0 ] || die "run as root (curl -fsSL https://outhaul.sh/install | sudo sh)"
	command -v systemctl >/dev/null 2>&1 || die "systemd is required (no systemctl found)"
	distro=$(detect_distro)
	if [ "$distro" != debian ]; then
		step_fail "unsupported distro: ${distro:-unknown} (V1 targets Debian)"
		ask_yes_no "  Continue anyway?" n || die "aborted on unsupported distro"
	else
		step_ok "Debian detected"
	fi
	arch=$(uname -m)
	case "$arch" in x86_64|amd64|aarch64|arm64) step_ok "architecture $arch";;
		*) die "unsupported architecture: $arch";; esac
	MEM_KB=$(read_meminfo_kb MemTotal); SWAP_KB=$(read_meminfo_kb SwapTotal)
	note "memory: $((MEM_KB/1024)) MB RAM, $((SWAP_KB/1024)) MB swap"
}
```

- [ ] **Step 4: Run it, expect PASS**

Run: `sh deploy/test/run.sh 2>&1 | grep -A4 preflight`
Expected: three `ok` lines.

- [ ] **Step 5: Commit**

```bash
git add deploy/bootstrap.sh deploy/test/test_preflight.sh
git commit -m "feat(installer): preflight checks + os-release/meminfo parsing"
```

---

## Task 11: Dependency installation (Docker, git, Nixpacks)

**Files:**
- Modify: `deploy/bootstrap.sh` (add `ensure_docker`, `ensure_git`, `ensure_nixpacks`)

These are system-effecting; verified in the Docker integration harness (Task 21). Implement with spinners and idempotent guards.

- [ ] **Step 1: Implement the dependency installers**

Add above `main`:

```sh
ensure_docker() {
	if command -v docker >/dev/null 2>&1; then step_ok "Docker present"; else
		spinner_start "installing Docker (get.docker.com)"
		curl -fsSL https://get.docker.com | sh >>"${LOGFILE:-/dev/null}" 2>&1
		spinner_stop $?
	fi
	systemctl enable --now docker >/dev/null 2>&1 || true
	docker compose version >/dev/null 2>&1 || die "docker compose v2 plugin missing (install docker-compose-plugin)"
	step_ok "docker compose v2 available"
}

ensure_git() {
	if command -v git >/dev/null 2>&1; then step_ok "git present"; return; fi
	spinner_start "installing git"
	{ apt-get update -qq && apt-get install -y -qq git; } >>"${LOGFILE:-/dev/null}" 2>&1
	spinner_stop $?
}

# Optional — caller gates on ask_yes_no.
ensure_nixpacks() {
	if command -v nixpacks >/dev/null 2>&1; then step_ok "nixpacks present"; return; fi
	spinner_start "installing nixpacks"
	curl -fsSL https://nixpacks.com/install.sh | bash >>"${LOGFILE:-/dev/null}" 2>&1
	spinner_stop $?
}
```

- [ ] **Step 2: Syntax + lint check**

Run: `sh -n deploy/bootstrap.sh && shellcheck -s sh deploy/bootstrap.sh`
Expected: no syntax errors; shellcheck clean (add targeted `# shellcheck disable=` only where justified).

- [ ] **Step 3: Commit**

```bash
git add deploy/bootstrap.sh
git commit -m "feat(installer): idempotent Docker/git/nixpacks installers"
```

---

## Task 12: Pinned Go toolchain download + verify

**Files:**
- Modify: `deploy/bootstrap.sh` (add `go_dl_arch`, `install_go_toolchain`)
- Create: `deploy/test/test_goarch.sh`

- [ ] **Step 1: Write the failing test (arch mapping is pure)**

Create `deploy/test/test_goarch.sh`:

```sh
#!/usr/bin/env sh
here=$(cd "$(dirname "$0")" && pwd)
. "$here/assert.sh"
OUTHAUL_INSTALLER_LIB=1 . "$here/../bootstrap.sh"

assert_eq "x86_64 -> amd64" "amd64" "$(go_dl_arch x86_64)"
assert_eq "amd64 -> amd64"  "amd64" "$(go_dl_arch amd64)"
assert_eq "aarch64 -> arm64" "arm64" "$(go_dl_arch aarch64)"
assert_eq "arm64 -> arm64"  "arm64" "$(go_dl_arch arm64)"

[ "${TESTS_FAIL:-0}" -eq 0 ]
```

- [ ] **Step 2: Run it, expect FAIL**

Run: `sh deploy/test/run.sh 2>&1 | grep -A4 goarch`
Expected: FAIL — `go_dl_arch: not found`.

- [ ] **Step 3: Implement the arch map + toolchain installer**

Add above `main`:

```sh
# Maps uname -m to the go.dev download arch token.
go_dl_arch() { # uname_m
	case "$1" in
		x86_64|amd64)  echo amd64;;
		aarch64|arm64) echo arm64;;
		*) return 1;;
	esac
}

# Downloads the go.mod-pinned Go toolchain to a temp dir, verifies its SHA256
# against the value published in the accompanying .sha256, and exports PATH.
# Sets GOROOT_TMP so the caller can remove it later.
install_go_toolchain() { # version srcdir
	ver=$1; srcdir=$2
	if command -v go >/dev/null 2>&1 && [ "$(go env GOVERSION 2>/dev/null)" = "go$ver" ]; then
		step_ok "Go $ver present"; return
	fi
	a=$(go_dl_arch "$(uname -m)") || die "unsupported arch for Go download"
	GOROOT_TMP=$(mktemp -d)
	tarball="go${ver}.linux-${a}.tar.gz"
	url="https://go.dev/dl/${tarball}"
	spinner_start "downloading Go $ver ($a)"
	{
		curl -fsSL "$url" -o "$GOROOT_TMP/$tarball" &&
		curl -fsSL "$url.sha256" -o "$GOROOT_TMP/$tarball.sha256"
	} >>"${LOGFILE:-/dev/null}" 2>&1
	rc=$?; spinner_stop $rc; [ $rc -eq 0 ] || die "could not download Go $ver from $url"
	sum=$(cat "$GOROOT_TMP/$tarball.sha256")
	echo "$sum  $GOROOT_TMP/$tarball" | sha256sum -c - >>"${LOGFILE:-/dev/null}" 2>&1 \
		|| die "Go toolchain checksum mismatch — refusing to build"
	step_ok "Go $ver checksum verified"
	tar -C "$GOROOT_TMP" -xzf "$GOROOT_TMP/$tarball"
	PATH="$GOROOT_TMP/go/bin:$PATH"; export PATH
	GOTOOLCHAIN=local; export GOTOOLCHAIN   # do not let go fetch a different one
	step_ok "Go $ver ready"
}
```

- [ ] **Step 4: Run it, expect PASS**

Run: `sh deploy/test/run.sh 2>&1 | grep -A4 goarch`
Expected: four `ok` lines.

- [ ] **Step 5: Lint**

Run: `shellcheck -s sh deploy/bootstrap.sh`
Expected: clean.

- [ ] **Step 6: Commit**

```bash
git add deploy/bootstrap.sh deploy/test/test_goarch.sh
git commit -m "feat(installer): download + checksum-verify go.mod-pinned Go toolchain"
```

---

## Task 13: Swap guard (create/remove temporary swapfile)

**Files:**
- Modify: `deploy/bootstrap.sh` (add `maybe_add_swap`, `remove_build_swap`)

System-effecting; verified in Task 21. Uses the pure `need_swap` from Task 5.

- [ ] **Step 1: Implement swap create/remove**

Add above `main`:

```sh
BUILD_SWAP=/var/lib/outhaul-build.swap

# Offers a temporary build swapfile when RAM+swap is under 2 GiB.
maybe_add_swap() { # mem_kb swap_kb
	need_swap "$1" "$2" || return 0
	note "low memory — the Go build may be OOM-killed without extra swap"
	ask_yes_no "  Create a temporary 2 GB swapfile for the build?" y || return 0
	[ -e "$BUILD_SWAP" ] && return 0
	spinner_start "creating build swapfile"
	{
		fallocate -l 2G "$BUILD_SWAP" 2>/dev/null || dd if=/dev/zero of="$BUILD_SWAP" bs=1M count=2048
		chmod 600 "$BUILD_SWAP" && mkswap "$BUILD_SWAP" && swapon "$BUILD_SWAP"
	} >>"${LOGFILE:-/dev/null}" 2>&1
	spinner_stop $?
	SWAP_ADDED=1
}

# Removes only the swapfile we created.
remove_build_swap() {
	[ "${SWAP_ADDED:-0}" = 1 ] || return 0
	swapoff "$BUILD_SWAP" 2>/dev/null || true
	rm -f "$BUILD_SWAP"
	SWAP_ADDED=0
}
```

- [ ] **Step 2: Lint + syntax**

Run: `sh -n deploy/bootstrap.sh && shellcheck -s sh deploy/bootstrap.sh`
Expected: clean.

- [ ] **Step 3: Commit**

```bash
git add deploy/bootstrap.sh
git commit -m "feat(installer): temporary build swapfile guard for low-RAM VPS"
```

---

## Task 14: Clone repo + build with progress

**Files:**
- Modify: `deploy/bootstrap.sh` (add `fetch_source`, `build_binary`)

System-effecting; verified in Task 21.

- [ ] **Step 1: Implement clone + build**

Add above `main`:

```sh
# Clones the repo (or uses --from-checkout dir). Prints the source dir via SRC_DIR.
fetch_source() {
	if [ "${FROM_CHECKOUT:-0}" = 1 ]; then
		SRC_DIR=$(cd "$CHECKOUT_DIR" && pwd)
		[ -f "$SRC_DIR/go.mod" ] || die "--from-checkout: no go.mod in $SRC_DIR"
		step_ok "using local checkout $SRC_DIR"
		return
	fi
	SRC_DIR=$(mktemp -d)
	spinner_start "cloning outhaul"
	git clone --depth 1 "$OUTHAUL_REPO" "$SRC_DIR" >>"${LOGFILE:-/dev/null}" 2>&1
	spinner_stop $?
}

# Builds ./outhaul in SRC_DIR. Streams go build output to the log; shows a
# spinner (progress bar is best-effort since `go build` has no clean %).
build_binary() {
	( cd "$SRC_DIR" || die "source dir vanished"
	  spinner_start "building outhaul (this can take a few minutes)"
	  GOFLAGS=-buildvcs=false go build -o outhaul . >>"${LOGFILE:-/dev/null}" 2>&1
	  spinner_stop $? )
	[ -x "$SRC_DIR/outhaul" ] || die "build failed — see ${LOGFILE:-the install log}"
	step_ok "binary built"
}
```

- [ ] **Step 2: Lint + syntax**

Run: `sh -n deploy/bootstrap.sh && shellcheck -s sh deploy/bootstrap.sh`
Expected: clean.

- [ ] **Step 3: Commit**

```bash
git add deploy/bootstrap.sh
git commit -m "feat(installer): clone source and build the binary"
```

---

## Task 15: DNS + port preflight (ingress mode a)

**Files:**
- Modify: `deploy/bootstrap.sh` (add `host_public_ip`, `domain_resolves_to`, `port_in_use`, `acme_preflight`)
- Create: `deploy/test/test_acme.sh`

- [ ] **Step 1: Write the failing test (comparison logic is pure)**

Create `deploy/test/test_acme.sh`:

```sh
#!/usr/bin/env sh
here=$(cd "$(dirname "$0")" && pwd)
. "$here/assert.sh"
OUTHAUL_INSTALLER_LIB=1 . "$here/../bootstrap.sh"

# domain_ip_matches PUBLIC_IP "space separated resolved ips" -> 0 if match
assert_ok   "match when public ip in set" domain_ip_matches 203.0.113.7 "198.51.100.1 203.0.113.7"
assert_fail "no match"                    domain_ip_matches 203.0.113.7 "198.51.100.1 198.51.100.2"
assert_fail "empty resolved set"          domain_ip_matches 203.0.113.7 ""

[ "${TESTS_FAIL:-0}" -eq 0 ]
```

- [ ] **Step 2: Run it, expect FAIL**

Run: `sh deploy/test/run.sh 2>&1 | grep -A4 acme`
Expected: FAIL — `domain_ip_matches: not found`.

- [ ] **Step 3: Implement preflight helpers**

Add above `main`:

```sh
# Pure: does PUBLIC_IP appear in the resolved-IP set?
domain_ip_matches() { # public_ip resolved_ips
	pub=$1; set -- $2
	[ "$#" -gt 0 ] || return 1
	for ip in "$@"; do [ "$ip" = "$pub" ] && return 0; done
	return 1
}

host_public_ip() {
	curl -fsS --max-time 5 https://api.ipify.org 2>/dev/null ||
	curl -fsS --max-time 5 https://ifconfig.me 2>/dev/null || true
}

# Space-separated A records for a name, using whatever resolver tool exists.
resolve_a() { # name
	if command -v getent >/dev/null 2>&1; then
		getent ahostsv4 "$1" 2>/dev/null | awk '{print $1}' | sort -u | paste -sd' ' -
	else
		host "$1" 2>/dev/null | awk '/has address/{print $NF}' | paste -sd' ' -
	fi
}

port_in_use() { # port
	if command -v ss >/dev/null 2>&1; then ss -ltn 2>/dev/null | awk '{print $4}' | grep -qE "[:.]$1\$"
	else netstat -ltn 2>/dev/null | awk '{print $4}' | grep -qE "[:.]$1\$"; fi
}

# Warns (does not hard-fail) about DNS/port issues before choosing ACME.
acme_preflight() { # domain
	dom=$1
	pub=$(host_public_ip)
	ips=$(resolve_a "$dom")
	note "this server's public IP: ${pub:-unknown}"
	note "$dom resolves to: ${ips:-<nothing>}"
	if [ -n "$pub" ] && ! domain_ip_matches "$pub" "$ips"; then
		step_fail "$dom does not point at this server"
		note "create a DNS A record:  $dom  A  $pub"
		ask_yes_no "  Continue with Let's Encrypt anyway?" n ||
			die "fix DNS, then re-run the installer"
	else
		step_ok "$dom points at this server"
	fi
	for p in 80 443; do
		port_in_use "$p" && step_fail "port $p already in use — Traefik may fail to bind" || true
	done
}
```

- [ ] **Step 4: Run it, expect PASS**

Run: `sh deploy/test/run.sh 2>&1 | grep -A4 acme`
Expected: three `ok` lines.

- [ ] **Step 5: Commit**

```bash
git add deploy/bootstrap.sh deploy/test/test_acme.sh
git commit -m "feat(installer): DNS + port preflight for Let's Encrypt mode"
```

---

## Task 16: SSH-safe firewall

**Files:**
- Modify: `deploy/bootstrap.sh` (add `apply_firewall`)

System-effecting; verified in Task 21. Uses `derive_firewall_ports` (Task 4).

- [ ] **Step 1: Implement `apply_firewall`**

Add above `main`:

```sh
# Opens the given port set with ufw. CRITICAL: `ufw allow` every port
# (especially SSH/22) BEFORE `ufw enable`, so the running session survives.
apply_firewall() { # ports (space separated, must include 22)
	ports=$1
	command -v ufw >/dev/null 2>&1 || {
		spinner_start "installing ufw"
		{ apt-get update -qq && apt-get install -y -qq ufw; } >>"${LOGFILE:-/dev/null}" 2>&1
		spinner_stop $?
	}
	for p in $ports; do
		ufw allow "$p"/tcp >>"${LOGFILE:-/dev/null}" 2>&1 || true
	done
	# --force avoids the interactive "proceed?" prompt; SSH is already allowed above.
	ufw --force enable >>"${LOGFILE:-/dev/null}" 2>&1 || true
	step_ok "firewall: opened $ports (SSH preserved)"
	note "open more later with: ufw allow <port>"
}
```

- [ ] **Step 2: Lint + syntax**

Run: `sh -n deploy/bootstrap.sh && shellcheck -s sh deploy/bootstrap.sh`
Expected: clean.

- [ ] **Step 3: Commit**

```bash
git add deploy/bootstrap.sh
git commit -m "feat(installer): SSH-safe ufw firewall (allow before enable)"
```

---

## Task 17: Service user, binary install, env, migration, systemd

**Files:**
- Modify: `deploy/bootstrap.sh` (add `migrate_slipway`, `ensure_service_user`, `install_binary`, `install_service`)

System-effecting; verified in Task 21. Reuses logic from the old `deploy/install.sh`.

- [ ] **Step 1: Implement the system-install functions**

Add above `main`:

```sh
# Carry legacy slipway state across the rename (mirrors old install.sh).
migrate_slipway() {
	if [ -d /var/lib/slipway ] && [ ! -e /var/lib/outhaul ]; then
		systemctl stop outhaul 2>/dev/null || true
		mv /var/lib/slipway /var/lib/outhaul
		[ -f /var/lib/outhaul/slipway.db ] && mv /var/lib/outhaul/slipway.db /var/lib/outhaul/outhaul.db
		step_ok "migrated /var/lib/slipway -> /var/lib/outhaul"
	fi
	rm -f /usr/local/bin/slipway
}

ensure_service_user() {
	if ! id outhaul >/dev/null 2>&1; then
		nologin=$(command -v nologin || echo /usr/sbin/nologin)
		useradd --system --home-dir /var/lib/outhaul --create-home --shell "$nologin" outhaul
		step_ok "created system user outhaul"
	fi
	usermod -aG docker outhaul
}

install_binary() {
	install -m 0755 "$SRC_DIR/outhaul" /usr/local/bin/outhaul
	step_ok "installed /usr/local/bin/outhaul"
}

# Installs the systemd unit shipped in the repo and (re)starts the service.
install_service() {
	install -m 0644 "$SRC_DIR/deploy/outhaul.service" /etc/systemd/system/outhaul.service
	systemctl daemon-reload
	if systemctl is-active --quiet outhaul; then systemctl restart outhaul
	else systemctl enable --now outhaul; fi
	step_ok "outhaul service started"
}
```

- [ ] **Step 2: Lint + syntax**

Run: `sh -n deploy/bootstrap.sh && shellcheck -s sh deploy/bootstrap.sh`
Expected: clean.

- [ ] **Step 3: Commit**

```bash
git add deploy/bootstrap.sh
git commit -m "feat(installer): service user, binary install, slipway migration, systemd unit"
```

---

## Task 18: Health check + setup-URL extraction

**Files:**
- Modify: `deploy/bootstrap.sh` (add `admin_health_url`, `wait_healthy`, `extract_setup_url`)
- Create: `deploy/test/test_health.sh`

- [ ] **Step 1: Write the failing test (URL derivation + log parsing are pure)**

Create `deploy/test/test_health.sh`:

```sh
#!/usr/bin/env sh
here=$(cd "$(dirname "$0")" && pwd)
. "$here/assert.sh"
OUTHAUL_INSTALLER_LIB=1 . "$here/../bootstrap.sh"

assert_eq "default admin url" "http://127.0.0.1:8080/" "$(admin_health_url '')"
assert_eq "custom listen addr" "http://127.0.0.1:9000/" "$(admin_health_url ':9000')"

log='Jun 01 xx outhaul[1]: one-time setup URL: http://127.0.0.1:8080/setup/abc123'
assert_eq "extracts setup url" "http://127.0.0.1:8080/setup/abc123" "$(printf '%s\n' "$log" | extract_setup_url)"

[ "${TESTS_FAIL:-0}" -eq 0 ]
```

Note: confirm the exact setup-line wording emitted by the binary before finalizing the regex — grep the Go source for the log string (`grep -rin "setup" main.go internal/`) and adjust `extract_setup_url` to match it.

- [ ] **Step 2: Run it, expect FAIL**

Run: `sh deploy/test/run.sh 2>&1 | grep -A4 health`
Expected: FAIL — `admin_health_url: not found`.

- [ ] **Step 3: Implement health + setup-URL helpers**

Add above `main`:

```sh
# Local admin URL to probe. Accepts OUTHAUL_LISTEN_ADDR-style ":8080".
admin_health_url() { # listen_addr
	addr=${1:-:8080}; [ -n "$addr" ] || addr=:8080
	port=${addr##*:}
	printf 'http://127.0.0.1:%s/\n' "$port"
}

# Extracts the one-time setup URL from a stream of journald lines.
extract_setup_url() {
	grep -oE 'https?://[^ ]+/setup/[A-Za-z0-9_-]+' | head -1
}

# Polls the admin endpoint up to ~30s. Returns 0 when it answers.
wait_healthy() { # url
	url=$1; i=0
	spinner_start "waiting for outhaul to answer"
	while [ "$i" -lt 30 ]; do
		if curl -fsS --max-time 2 -o /dev/null "$url" 2>/dev/null; then spinner_stop 0; return 0; fi
		sleep 1; i=$((i+1))
	done
	spinner_stop 1; return 1
}
```

- [ ] **Step 4: Run it, expect PASS**

Run: `sh deploy/test/run.sh 2>&1 | grep -A4 health`
Expected: three `ok` lines.

- [ ] **Step 5: Commit**

```bash
git add deploy/bootstrap.sh deploy/test/test_health.sh
git commit -m "feat(installer): post-install health check + setup-URL extraction"
```

---

## Task 19: Completion screen

**Files:**
- Modify: `deploy/bootstrap.sh` (add `completion_screen`)

- [ ] **Step 1: Implement `completion_screen`**

Add above `main`:

```sh
# Final boxed summary + confetti. Args passed as KEY=VALUE-ish positional data.
completion_screen() { # mode url ports setup_url healthy(0/1)
	mode=$1; url=$2; ports=$3; setup=$4; healthy=$5
	printf '\n'; hr
	_c '1;32'; printf '  Outhaul is installed\n'; _c 0
	[ "$healthy" = 0 ] && step_ok "admin UI reachable" || step_fail "admin UI did not answer yet (check: journalctl -u outhaul)"
	[ -n "$ports" ] && note "firewall: $ports open"
	case "$mode" in
		a) note "HTTPS: Let's Encrypt configured for $url";;
		b) _c '1;33'; printf '  → Finish Cloudflare Tunnel: log in, then paste your connector token in Settings → Tunnel\n'; _c 0;;
		c) note "admin UI on :8080 — set OUTHAUL_PUBLIC_URL + OUTHAUL_ACME_EMAIL later for HTTPS";;
	esac
	if [ -n "$setup" ]; then
		printf '\n  Open this one-time setup URL to create your admin account:\n'
		_c '1;36'; printf '    %s\n' "$setup"; _c 0
	else
		note "find the setup URL with: journalctl -u outhaul | grep -i setup"
	fi
	# Confetti flourish (skipped in plain mode).
	if [ "${COLOR:-0}" -ge 1 ] && [ "${UNICODE:-0}" = 1 ]; then
		printf '\n  '; for s in '✦' '✧' '·' '✦' '✧' '·' '✦' '✧'; do printf '%s ' "$s"; done; printf '\n'
	fi
	hr
}
```

- [ ] **Step 2: Manual visual check**

Run:
```bash
sh -c 'OUTHAUL_INSTALLER_LIB=1 . deploy/bootstrap.sh; init_ui; completion_screen a https://paas.dev "22 80 443" https://paas.dev/setup/abc 0'
```
Expected: boxed summary, cyan setup URL, sparkle line.

- [ ] **Step 3: Commit**

```bash
git add deploy/bootstrap.sh
git commit -m "feat(installer): maximalist completion screen"
```

---

## Task 20: Wizard orchestration (`main`)

**Files:**
- Modify: `deploy/bootstrap.sh` (replace `main`, add arg parsing, fd 3 setup, log file, traps, `choose_ingress`)

- [ ] **Step 1: Implement argument parsing, traps, and `main`**

Replace the placeholder `main` with:

```sh
LOGFILE=/var/log/outhaul-install.log
VERBOSE=0
FROM_CHECKOUT=0
CHECKOUT_DIR=.

parse_args() {
	while [ "$#" -gt 0 ]; do
		case "$1" in
			--verbose) VERBOSE=1;;
			--from-checkout) FROM_CHECKOUT=1; CHECKOUT_DIR=${2:-.}; shift;;
			-h|--help) printf 'usage: bootstrap.sh [--verbose] [--from-checkout DIR]\n'; exit 0;;
			*) die "unknown option: $1";;
		esac
		shift
	done
}

cleanup() {
	[ -n "${SPIN_PID:-}" ] && kill "$SPIN_PID" 2>/dev/null || true
	remove_build_swap
	[ "${FROM_CHECKOUT:-0}" = 1 ] || { [ -n "${SRC_DIR:-}" ] && rm -rf "$SRC_DIR"; }
	[ -n "${GOROOT_TMP:-}" ] && rm -rf "$GOROOT_TMP"
}

# Ingress-mode menu. Sets MODE (a/b/c), PUBLIC_URL, ACME_EMAIL.
choose_ingress() {
	printf '\n  How should apps be reachable?\n'
	printf '    1) Public domain + automatic HTTPS (Let'\''s Encrypt)\n'
	printf '    2) Cloudflare Tunnel (no public IP / ports needed)\n'
	printf '    3) Local only for now (admin UI on :8080)\n'
	choice=$(ask_value "  Choose 1/2/3:" 1)
	case "$choice" in
		2) MODE=b; PUBLIC_URL=''; ACME_EMAIL='';;
		3) MODE=c; PUBLIC_URL=''; ACME_EMAIL='';;
		*) MODE=a
		   PUBLIC_URL=$(ask_value "  Public URL (e.g. https://paas.example.com):" '')
		   dom=${PUBLIC_URL#*://}; dom=${dom%%/*}
		   acme_preflight "$dom"
		   ACME_EMAIL=$(ask_value "  Email for Let's Encrypt:" '');;
	esac
}

main() {
	parse_args "$@"
	: > "$LOGFILE" 2>/dev/null || LOGFILE=/dev/null
	[ "$VERBOSE" = 1 ] && LOGFILE=/dev/stderr
	trap cleanup EXIT INT TERM
	# Open the controlling terminal on fd 3 for prompts; fall back to stdin.
	exec 3</dev/tty 2>/dev/null || exec 3<&0

	init_ui
	hero
	preflight

	choose_ingress

	GITPORT=$(ask_value "  Git-push-to-deploy port (blank to disable):" 2222)
	SSH_ADDR=''; [ -n "$GITPORT" ] && SSH_ADDR=":$GITPORT"

	ask_yes_no "  Install Nixpacks (needed for auto-detected builds)?" y && WANT_NIXPACKS=1 || WANT_NIXPACKS=0
	ask_yes_no "  Configure the firewall (ufw) now?" y && WANT_FW=1 || WANT_FW=0

	ensure_docker
	ensure_git
	[ "$WANT_NIXPACKS" = 1 ] && ensure_nixpacks

	fetch_source
	GO_VER=$(go_version_from_gomod "$SRC_DIR/go.mod")
	maybe_add_swap "${MEM_KB:-0}" "${SWAP_KB:-0}"
	install_go_toolchain "$GO_VER" "$SRC_DIR"
	build_binary
	remove_build_swap

	migrate_slipway
	ensure_service_user
	install_binary
	[ ! -f /etc/outhaul.env ] && write_env_file /etc/outhaul.env "$MODE" "$PUBLIC_URL" "$ACME_EMAIL" "$SSH_ADDR" \
		&& step_ok "wrote /etc/outhaul.env" || note "/etc/outhaul.env exists — left unchanged"
	install_service

	if [ -n "${GOROOT_TMP:-}" ]; then
		ask_yes_no "  Remove the downloaded Go toolchain (~150 MB)?" y && { rm -rf "$GOROOT_TMP"; GOROOT_TMP=''; step_ok "removed Go toolchain"; }
	fi

	PORTS=''
	if [ "$WANT_FW" = 1 ]; then
		PORTS=$(derive_firewall_ports "$MODE" "$GITPORT")
		printf '  Firewall will open (SSH always preserved): %s\n' "$PORTS"
		ask_yes_no "  Proceed?" y && apply_firewall "$PORTS" || PORTS=''
	fi

	HURL=$(admin_health_url "${OUTHAUL_LISTEN_ADDR:-:8080}")
	wait_healthy "$HURL" && HEALTHY=0 || HEALTHY=1
	SETUP=$(journalctl -u outhaul --no-pager 2>/dev/null | extract_setup_url)

	completion_screen "$MODE" "$PUBLIC_URL" "$PORTS" "$SETUP" "$HEALTHY"
}
```

- [ ] **Step 2: Syntax + lint check**

Run: `sh -n deploy/bootstrap.sh && shellcheck -s sh deploy/bootstrap.sh`
Expected: no syntax errors; shellcheck clean.

- [ ] **Step 3: Run the full unit suite (must still pass — main is guarded)**

Run: `sh deploy/test/run.sh`
Expected: all test files pass, exit 0.

- [ ] **Step 4: Commit**

```bash
git add deploy/bootstrap.sh
git commit -m "feat(installer): wizard orchestration, ingress modes, traps, logging"
```

---

## Task 21: Docker end-to-end integration test

**Files:**
- Create: `deploy/test/integration.sh`
- Create: `deploy/test/Dockerfile.debian`

- [ ] **Step 1: Write the integration Dockerfile**

Create `deploy/test/Dockerfile.debian`:

```dockerfile
# Systemd-enabled Debian for installer integration testing.
FROM debian:13
RUN apt-get update && apt-get install -y systemd systemd-sysv curl ca-certificates && rm -rf /var/lib/apt/lists/*
COPY . /src
WORKDIR /src
# The container is run with --privileged and /sbin/init so systemctl works.
STOPSIGNAL SIGRTMIN+3
CMD ["/sbin/init"]
```

- [ ] **Step 2: Write the integration harness**

Create `deploy/test/integration.sh`:

```sh
#!/usr/bin/env sh
# End-to-end installer test in a systemd Debian container.
# Requires Docker with --privileged. Exercises mode c (local, no DNS/ACME),
# non-TTY log cleanliness, and re-run idempotency.
set -eu
img=outhaul-installer-test
root=$(cd "$(dirname "$0")/../.." && pwd)

docker build -f "$root/deploy/test/Dockerfile.debian" -t "$img" "$root"
cid=$(docker run -d --privileged --tmpfs /run --tmpfs /run/lock -v /sys/fs/cgroup:/sys/fs/cgroup:rw "$img")
trap 'docker rm -f "$cid" >/dev/null 2>&1 || true' EXIT
sleep 5  # let systemd come up

# Non-interactive run (mode c): feed answers on fd 0; installer reads /dev/tty
# which in this container falls back to fd 0. Choose 3 (local), disable git port,
# skip nixpacks, skip firewall.
docker exec -i "$cid" sh -c '
  printf "3\n\nn\nn\n" | sh /src/deploy/bootstrap.sh --from-checkout /src
' | tee /tmp/outhaul-install.out

# 1) escape-soup check: piped output must not contain raw ESC sequences.
if grep -q "$(printf "\033")\[" /tmp/outhaul-install.out; then
	echo "FAIL: escape sequences leaked into piped output"; exit 1
fi
echo "ok: clean non-TTY output"

# 2) service is active
docker exec "$cid" systemctl is-active --quiet outhaul && echo "ok: service active" || { echo "FAIL: service not active"; exit 1; }

# 3) binary present
docker exec "$cid" test -x /usr/local/bin/outhaul && echo "ok: binary installed"

# 4) idempotent re-run succeeds
docker exec -i "$cid" sh -c 'printf "3\n\nn\nn\n" | sh /src/deploy/bootstrap.sh --from-checkout /src' >/dev/null && echo "ok: idempotent re-run"

echo "INTEGRATION PASS"
```

- [ ] **Step 3: Run the integration test**

Run: `sh deploy/test/integration.sh`
Expected: `ok:` lines and `INTEGRATION PASS`. (If Docker/`--privileged` is unavailable in the dev environment, document that this runs in CI and note the manual VM alternative.)

- [ ] **Step 4: Fix any real failures found**

Iterate on `deploy/bootstrap.sh` until the integration test passes. Common fixes: `/dev/tty` fallback when absent (already handled via `exec 3<&0`), `docker compose` availability inside the container, and journald setup-line wording.

- [ ] **Step 5: Commit**

```bash
git add deploy/test/integration.sh deploy/test/Dockerfile.debian
git commit -m "test(installer): systemd Debian end-to-end integration harness"
```

---

## Task 22: CI, docs, and retire the old installer

**Files:**
- Create: `.github/workflows/installer.yml`
- Delete: `deploy/install.sh`
- Modify: `README.md` (installer instructions)

- [ ] **Step 1: Write the CI workflow**

Create `.github/workflows/installer.yml`:

```yaml
name: installer
on:
  push:
    paths: [deploy/**, .github/workflows/installer.yml]
  pull_request:
    paths: [deploy/**]
jobs:
  lint-and-unit:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - name: shellcheck
        run: shellcheck -s sh deploy/bootstrap.sh deploy/test/*.sh
      - name: unit tests
        run: sh deploy/test/run.sh
```

- [ ] **Step 2: Run CI steps locally to confirm**

Run: `shellcheck -s sh deploy/bootstrap.sh deploy/test/*.sh && sh deploy/test/run.sh`
Expected: clean lint, all unit tests pass.

- [ ] **Step 3: Update README install instructions**

In `README.md`, replace any "run `sudo deploy/install.sh` from a checkout" guidance with the remote one-liner:

```markdown
## Install on a fresh Debian VPS

    curl -fsSL https://outhaul.sh/install | sh

The installer walks you through ingress (Let's Encrypt or Cloudflare Tunnel),
opens the right firewall ports (keeping SSH), builds from source, and prints a
one-time setup URL. Developers with a local checkout can run
`sudo sh deploy/bootstrap.sh --from-checkout .`.
```

- [ ] **Step 4: Delete the superseded installer**

```bash
git rm deploy/install.sh
```

Confirm nothing else references it: `grep -rn "install.sh" --exclude-dir=.git .` — update any remaining references (docs) to `bootstrap.sh`.

- [ ] **Step 5: Final verification**

Run: `sh deploy/test/run.sh && shellcheck -s sh deploy/bootstrap.sh deploy/test/*.sh`
Expected: all green.

- [ ] **Step 6: Commit**

```bash
git add .github/workflows/installer.yml README.md
git rm deploy/install.sh
git commit -m "ci+docs: installer CI, README one-liner, retire deploy/install.sh"
```

---

## Self-Review Notes (addressed)

- **Spec coverage:** delivery/POSIX/replace install.sh (T1, T20, T22); visuals + degradation (T8, T9, T19); wizard flow + ingress modes (T20); `/dev/tty` prompts (T7, T20); deps incl. Go-from-go.dev pinned (T11, T12); firewall SSH-safe (T4, T16); DNS/port preflight (T15); swap guard (T5, T13); build-from-source (T14); env write (T6); Cloudflare guidance (T19, T20); health check + setup URL (T18); completion screen (T19); log file + `--verbose` (T20); idempotency/traps (T13, T17, T20); testing Debian + shellcheck (T21, T22).
- **Website serving note:** `outhaul.sh/install` must serve `deploy/bootstrap.sh` verbatim — this is a deployment/website concern outside this repo's code; flagged here so it is not forgotten.
- **Setup-line wording caveat:** T18 requires confirming the binary's actual setup-URL log string before locking the regex.
