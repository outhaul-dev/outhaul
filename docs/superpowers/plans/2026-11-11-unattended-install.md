# Unattended, Pinned Install and Updates Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make `deploy/bootstrap.sh` installable and re-runnable without a terminal, at an exact commit or tag, with pinned nixpacks, optional Docker and ufw handling, env files loaded by systemd, and `OUTHAUL_UPDATES`/`OUTHAUL_SSH_ADDR` switches that turn updates and git-push off.

**Architecture:** The installer keeps one code path for wizard and unattended runs: flags fill the same variables the prompts fill, then shared functions resolve the target commit, build only when needed, swap with the updater's snapshot/rollback functions, write `/etc/outhaul/install.env` and systemd drop-ins, and check the service through `service_env`. Go gains two settings, a version stamp, and an Updates panel that can be present while its routes are absent.

**Tech Stack:** Go 1.26, POSIX `sh` (dash, `bash --posix`, busybox sh), systemd, Docker Engine, git, nixpacks, shellcheck 0.9.0.

**Spec:** `docs/superpowers/specs/2026-11-11-unattended-install-design.md`. Read it first; this plan argues from it.

## Global Constraints

- The installer and updater are POSIX `sh`, tested as files under `dash`, `bash --posix` and `busybox sh`. CI's shellcheck is 0.9.0.
- Under `curl … | sh` the script is its own stdin: nothing may read answers from stdin there.
- `outhaul.service` runs as the `outhaul` user. The installer and the updater both install the unit file from the tree, so a line added to it is lost at the next update. Put installer lines in drop-ins.
- systemd reads `EnvironmentFile=` as root and expands nothing in it. A missing file without `-` stops the service.
- The repository is public: `example.com` names only, in code, tests, scripts and docs.
- Exact values: nixpacks `1.41.0`; amd64 SHA-256 `0f55de7874507b9cf7502113120bd96f2ab6979f78d10eaf2eb2ade9207b3af6`; arm64 SHA-256 `912bd02dd2bb6f9c3a9ed965fe8a68b4aa318dc7a2546e2eca6f2806a894ba39`; git-push default `2222`; `OUTHAUL_UPDATES` default `main`; `off` is accepted for `OUTHAUL_SSH_ADDR` and for no other address.
- Exit codes: `0` installed or nothing to do; `1` something failed; `2` bad flags with nothing changed.
- No secret, and no value of `OUTHAUL_REQUIRE_HEADER_VALUE`, appears in an argument, the log or the output. The setup URL never appears in an unattended run's output.
- A run with neither `--non-interactive` nor `--env-file` leaves the installer drop-ins alone.
- Before calling the plan done: `go build ./... && go vet ./... && go test ./...`, then `go test -race -timeout 30m ./...`, `sh deploy/test/run.sh`, and `shellcheck -s sh --severity=warning deploy/bootstrap.sh deploy/update.sh deploy/test/*.sh`.
- Commits look like `feat(scope): summary` or `fix(scope): summary`; the body says what the user saw go wrong and what changed. One series of commits on `enhancements`; each task is one commit.
- R1 to R4 (`docs/superpowers/plans/2026-11-11-proxy-ingress.md`) is a separate plan. Nothing here needs it built first; only `--ingress proxy`'s effect check requires an Outhaul that has those settings.

## Review Focus

The inputs most likely to bite that no requirement spells out. Each has its test in the task named.

1. A re-run with neither `--non-interactive` nor `--env-file` must not delete an earlier unattended run's drop-in: the service would silently start without its env files (Task 7).
2. A rollback must restore the previous `install.env` and drop-ins, and remove ones the old version did not have: an old binary may refuse what new flags wrote (Task 6).
3. `service_env` must prefer the running service's environment over the installer's own, so a listen port or HTTPS port set in an env file is the one health checks use (Task 8).
4. The `proxy` effect check must catch a wildcard-bound admin port and a published port 80 even when the public URL answers: an Outhaul from before R1 to R4 ignores the switches (Task 8).
5. `--setup-url-file` must not delete a pending setup file on a re-run that did not start the service, and must remove it when a start this run printed no URL (Task 9).

---

## File map

| File | Responsibility | Tasks |
|---|---|---|
| `internal/config/config.go` | `OUTHAUL_UPDATES`, `OUTHAUL_SSH_ADDR=off`, `parser.word`, `parser.addrOrOff` | 1, 2 |
| `internal/update/build.go` | the `version` stamp and `Build.Version` | 5 |
| `main.go` | `newUpdates`, `sshListenAddr`, the `off` wiring, `version=` output | 1, 2, 5 |
| `internal/server/updates.go`, `settings.go`, `templates/settings.tmpl` | the panel with updates off, no forms or badge, routes still 404 | 2 |
| `deploy/bootstrap.sh` | Docker states, nixpacks pin, `--ref`, marker, careful swap, flags, `install.env`, drop-ins, `service_env`, health checks, log, setup file | 1, 3–9 |
| `deploy/update.sh` | `OUTHAUL_UPDATES=off` refusal; sources the moved functions | 2, 6 |
| `deploy/test/test_docker.sh` | Docker's three states | 3 |
| `deploy/test/test_nixpacks.sh` | the pin, targets, checksum, table | 4 |
| `deploy/test/test_ref.sh` | ref resolution, fetch, stamp, marker, three states | 5 |
| `deploy/test/test_update.sh` | snapshot/restore of env files and drop-ins; `off` refusal | 2, 6 |
| `deploy/test/test_main.sh`, `test_env.sh` | flags and checks; `install.env` and drop-ins | 7 |
| `deploy/test/test_health.sh`, `test_firewall.sh` | `service_env`, header on stdin, `proxy` checks; `proxy` ports | 8 |
| `deploy/test/test_noninteractive.sh` | the whole unattended run | 9 |
| `deploy/test/integration.sh`, `Dockerfile.debian` | a real systemd Debian container | 10 |
| `README.md`, `docs/UNATTENDED-INSTALL.md`, `docs/MANUAL-DEPLOY.md`, `ARCHITECTURE.md`, `AGENTS.md`, unit files | the docs and the decisions to keep | 11 |

---

### Task 1: `OUTHAUL_SSH_ADDR=off`

Turn the git-push SSH server off from configuration, and make the wizard's "none" write that value. The hook socket and the repositories stay.

**Files:**
- Modify: `internal/config/config.go` (`Load`, `parser.addr`), `internal/config/config_test.go`
- Modify: `main.go` (`startGitPush`, `resolveSSHAddr`), `main_test.go`
- Modify: `deploy/bootstrap.sh` (`write_env_file` callers in `main`)
- Test: `deploy/test/test_env.sh`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces:
  - `Config.SSHAddr string` holds `":2222"` or `"off"`.
  - `func (c Config) SSHOff() bool` — true when `SSHAddr` is `off` in any case.
  - `func (p *parser) addrOrOff(key, fallback string) string` — empty takes the fallback; `off` in any case is stored as `"off"`; anything else goes through the address parser.
  - `func sshListenAddr(st *store.Store, cfg config.Config) (addr string, on bool)` — `("", false)` when `cfg.SSHOff()`; otherwise the stored `ssh_addr` setting when set, else `cfg.SSHAddr`, with `on == true`.
  - `func git_ssh_from_port(port string) string` — `":<port>"` when the port is non-empty, else `"off"`.

- [ ] **Step 1: Write the failing config tests**

In `internal/config/config_test.go` add:

```go
func TestSSHAddrOff(t *testing.T) {
	for _, v := range []string{"off", "OFF", "Off"} {
		c := Load(func(k string) string {
			if k == "OUTHAUL_SSH_ADDR" { return v }
			return ""
		})
		if !c.SSHOff() { t.Errorf("OUTHAUL_SSH_ADDR=%q: SSHOff() = false", v) }
		if err := c.Validate(); err != nil { t.Errorf("OUTHAUL_SSH_ADDR=%q: Validate: %v", v, err) }
	}
	c := Load(func(k string) string {
		if k == "OUTHAUL_SSH_ADDR" { return ":2200" }
		return ""
	})
	if c.SSHOff() { t.Error(":2200 read as off") }
}
```

Add `{"OUTHAUL_LISTEN_ADDR", "off"}` to `TestValidateRejectsMalformedValues`; it must still be refused.

- [ ] **Step 2: Run it to see it fail**

Run: `go test ./internal/config -run 'SSHAddrOff|Malformed' -v`
Expected: FAIL because `SSHOff` is undefined.

- [ ] **Step 3: Implement the setting**

In `internal/config/config.go`:
- Change `SSHAddr: p.addr("OUTHAUL_SSH_ADDR", ":2222")` to `p.addrOrOff("OUTHAUL_SSH_ADDR", ":2222")`.
- Add `SSHOff` beside `TLSEnabled`.
- Split `addr` into `addr(key, fallback)` and `addrValue(key, v, fallback)`; `addrOrOff` checks `strings.EqualFold(v, "off")` before calling `addrValue`.

- [ ] **Step 4: Write the failing main test**

In `main_test.go`:

```go
func TestSSHListenAddrOff(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "db"), nil)
	if err != nil { t.Fatal(err) }
	defer st.Close()
	if err := st.SetSetting(context.Background(), "ssh_addr", ":2200"); err != nil { t.Fatal(err) }

	if addr, on := sshListenAddr(st, config.Config{SSHAddr: "off"}); on || addr != "" {
		t.Errorf("off: got (%q, %v), want (\"\", false)", addr, on)
	}
	if addr, on := sshListenAddr(st, config.Config{SSHAddr: ":2222"}); !on || addr != ":2200" {
		t.Errorf("stored setting: got (%q, %v), want (\":2200\", true)", addr, on)
	}
}
```

- [ ] **Step 5: Run it to see it fail**

Run: `go test . -run SSHListenAddrOff -v`
Expected: FAIL because `sshListenAddr` is undefined.

- [ ] **Step 6: Implement `sshListenAddr` and wire it into `startGitPush`**

Replace `resolveSSHAddr` with `sshListenAddr`. In `startGitPush`, keep the existing git-dir, hook-socket and bridge setup first (the design says the hook socket and repositories stay), then:

```go
addr, on := sshListenAddr(st, cfg)
if !on {
	log.Printf("git-push SSH server is off (OUTHAUL_SSH_ADDR=off)")
	return nil
}
```

Only after that load the host key and listen. The returned `*gitssh.Server` is nil, so `srv.SetSSHControl` is not called and the Settings page keeps saying the server is not running.

- [ ] **Step 7: Write the failing shell tests**

In `deploy/test/test_env.sh`, after the existing `write_env_file` cases:

```sh
write_env_file "$dest" c '' '' off
assert_contains "ssh off is written" "$(cat "$dest")" "OUTHAUL_SSH_ADDR=off"
```

In `deploy/test/test_main.sh`:

```sh
assert_eq "git port -> ssh address" ":2222" "$(git_ssh_from_port 2222)"
assert_eq "no git port -> off" "off" "$(git_ssh_from_port '')"
```

- [ ] **Step 8: Run them to see them fail**

Run:
```sh
dash deploy/test/test_env.sh
dash deploy/test/test_main.sh
```
Expected: `test_env.sh` passes (the `write_env_file` call is already able to write `off`); `test_main.sh` fails because `git_ssh_from_port` is undefined.

- [ ] **Step 9: Implement the mapping**

Add `git_ssh_from_port` beside `normalize_git_port`:

```sh
git_ssh_from_port() { # port, empty for none
	if [ -n "$1" ]; then printf ':%s\n' "$1"; else printf 'off\n'; fi
}
```

In `main`, replace the `if [ -n "$GITPORT" ]; then SSH_ADDR=":$GITPORT"; else SSH_ADDR=''; fi` pair with `SSH_ADDR=$(git_ssh_from_port "$GITPORT")`. The firewall still receives the empty `GITPORT`, so no port is opened.

- [ ] **Step 10: Run the tests**

Run:
```sh
go test ./internal/config . -run 'SSH|Malformed|Version' -v
dash deploy/test/test_env.sh
dash deploy/test/test_main.sh
bash --posix deploy/test/test_env.sh
busybox sh deploy/test/test_env.sh
```
Expected: PASS.

- [ ] **Step 11: Commit**

```sh
git add internal/config/config.go internal/config/config_test.go main.go main_test.go deploy/bootstrap.sh deploy/test/test_env.sh deploy/test/test_main.sh
git commit -m "feat(config): OUTHAUL_SSH_ADDR=off turns the git-push server off" \
  -m "The wizard's \"none\" answer only kept the firewall port closed; Outhaul still listened on :2222. off now opens no SSH listener, ignores a stored ssh_addr, and leaves the hook socket and repositories alone."
```

---

### Task 2: `OUTHAUL_UPDATES=main|off`

Add the setting, make the panel show the version and an off notice without forms, and make the root updater refuse while off.

**Files:**
- Modify: `internal/config/config.go`, `internal/config/config_test.go`
- Modify: `main.go`, `main_test.go`
- Modify: `internal/server/server.go`, `internal/server/updates.go`, `internal/server/settings.go`, `internal/server/templates/settings.tmpl`, `internal/server/updates_test.go`
- Modify: `deploy/update.sh`, `deploy/test/test_update.sh`
- Modify: `deploy/outhaul-update.path`, `deploy/outhaul-update.service` (comments and `Description=`)

**Interfaces:**
- Consumes: nothing from Task 1.
- Produces:
  - `Config.Updates string` — `"main"` (default) or `"off"`, lower-cased.
  - `func (c Config) UpdatesOff() bool`.
  - `func (p *parser) word(key, fallback string, allowed ...string) string` — trims, matches case-insensitively, stores lower case; anything else is a problem whose message ends `want main or off`.
  - `func newUpdates(cfg config.Config) (d *update.Dir, k *update.Checker, off bool)` — off returns `(nil, nil, true)` and creates nothing.
  - `func (s *Server) SetUpdatesOff()`.
  - `updatesPanel.Off bool` and `updateOffCopy = "Updates are off. This server is upgraded by re-running the installer."`.

- [ ] **Step 1: Write the failing config tests**

In `internal/config/config_test.go`:

```go
func TestUpdatesSetting(t *testing.T) {
	for _, tc := range []struct{ in, want string; off bool }{
		{"", "main", false}, {"main", "main", false}, {"MAIN", "main", false},
		{"off", "off", true}, {"OFF", "off", true}, {" off ", "off", true},
	} {
		c := Load(func(k string) string {
			if k == "OUTHAUL_UPDATES" { return tc.in }
			return ""
		})
		if c.Updates != tc.want || c.UpdatesOff() != tc.off {
			t.Errorf("OUTHAUL_UPDATES=%q -> %q/%v, want %q/%v", tc.in, c.Updates, c.UpdatesOff(), tc.want, tc.off)
		}
	}
	for _, v := range []string{"weekly", "releases"} {
		c := Load(func(k string) string {
			if k == "OUTHAUL_UPDATES" { return v }
			return ""
		})
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "want main or off") {
			t.Errorf("OUTHAUL_UPDATES=%q: Validate = %v, want 'want main or off'", v, err)
		}
	}
}
```

- [ ] **Step 2: Run it to see it fail**

Run: `go test ./internal/config -run UpdatesSetting -v`
Expected: FAIL because `Updates` and `UpdatesOff` are undefined.

- [ ] **Step 3: Implement the setting**

Add `Updates: p.word("OUTHAUL_UPDATES", "main", "main", "off")` to `Load`, the field to `Config`, and `UpdatesOff()`. Implement `word` beside `boolDefault`; `p.reject(key, v, "main or off")` gives the spec's message.

- [ ] **Step 4: Write the failing server test**

In `internal/server/updates_test.go`:

```go
func TestUpdatesPanelWhenOff(t *testing.T) {
	e := newTestEnv(t)
	e.srv.SetBuild(update.Build{Commit: testCommit, Repo: "https://github.com/o/r.git", Version: "v1.2.3"})
	e.srv.SetUpdatesOff()
	e.login(t)

	page := body(t, e.get(t, "/settings"))
	assertContains(t, page, `id="updates"`)
	assertContains(t, page, "Updates are off")
	assertContains(t, page, "v1.2.3")
	assertLacks(t, page, `id="update-button"`, "Check now", `class="nav-dot"`)

	for _, r := range []struct{ method, path string }{
		{http.MethodPost, "/settings/update"},
		{http.MethodPost, "/settings/update/check"},
		{http.MethodGet, "/settings/update/progress"},
	} {
		var resp *http.Response
		if r.method == http.MethodGet { resp = e.get(t, r.path) } else { resp = e.postForm(t, r.path, url.Values{}) }
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s %s = %d, want 404", r.method, r.path, resp.StatusCode)
		}
	}
}
```

- [ ] **Step 5: Run it to see it fail**

Run: `go test ./internal/server -run UpdatesPanelWhenOff -v`
Expected: FAIL because `SetUpdatesOff` is undefined and the panel is absent.

- [ ] **Step 6: Implement the server side**

- Add `updatesOff bool` to `Server`.
- `SetUpdatesOff()` sets it.
- `updatesPanelShown()` returns `s.updatesWired() || s.updatesOff`; use it in `renderSettingsWith`.
- `updatesPanel` starts with `if s.updatesOff { return &updatesPanel{Build: s.build, Off: true, Summary: updateOffCopy} }`.
- `updateAvailable` stays `updatesWired() && ...`, so off never shows the badge.
- In `settings.tmpl`, show `Running <short>` and, when `Build.Version` is non-empty, `version <version>`. Wrap everything after that in `{{if .Off}}<p>{{.Summary}}</p>{{else}} … existing … {{end}}`. The existing forms, last result and check-now stay inside the `else`.

- [ ] **Step 7: Write the failing main test**

In `main_test.go`:

```go
func TestNewUpdatesOff(t *testing.T) {
	d, k, off := newUpdates(config.Config{Updates: "off"})
	if !off || d != nil || k != nil {
		t.Fatalf("off: got (%v, %v, %v), want (nil, nil, true)", d, k, off)
	}
	d, k, off = newUpdates(config.Config{Updates: "main", DataDir: t.TempDir()})
	if off || d == nil || k == nil {
		t.Fatalf("main: got (%v, %v, %v), want a Dir and a Checker", d, k, off)
	}
}
```

- [ ] **Step 8: Run it to see it fail**

Run: `go test . -run NewUpdatesOff -v`
Expected: FAIL because `newUpdates` is undefined.

- [ ] **Step 9: Implement the wiring**

Add `newUpdates` in `main.go`, using `cfg.UpdateDir()`, `update.LogPath`, `time.Now`, and the existing `http.Client{Timeout: 15 * time.Second}`. In `serve`:
- `updates, checker, updatesOff := newUpdates(cfg)`
- `if !updatesOff { worker.SetHold(updates.InProgress); backups.SetHold(updates.InProgress); go checker.Run(workerCtx) }`
- after `srv.SetBuild`: `if updatesOff { srv.SetUpdatesOff() } else { srv.SetUpdates(updates, checker) }`.

Do not create a Checker when off: that is what keeps every request from leaving the server.

- [ ] **Step 10: Write the failing updater test**

In `deploy/test/test_update.sh`, after the `update_main` scenarios:

```sh
# Updates off: the request goes, nothing else changes.
setup
before=$(cat "$UPDATE_DIR/status")
OUTHAUL_UPDATES=off run_update
assert_eq "off: exit 0" 0 "$RC"
assert_eq "off: request removed" no "$(exists "$UPDATE_DIR/request")"
assert_eq "off: status unchanged" "$before" "$(cat "$UPDATE_DIR/status")"
assert_eq "off: Outhaul never stopped" "" "$(systemctl_calls)"
assert_contains "off: one journal line" "$(journal_lines)" "updates are off"

# An unknown value is off.
setup
OUTHAUL_UPDATES=weekly run_update
assert_eq "unknown value: exit 0" 0 "$RC"
assert_eq "unknown value: request removed" no "$(exists "$UPDATE_DIR/request")"
assert_eq "unknown value: Outhaul never stopped" "" "$(systemctl_calls)"
```

`run_update` runs `update_main` in a subshell; POSIX passes a `VAR=value` prefix on a function call into the function and its subshell, so `OUTHAUL_UPDATES=off run_update` works. Add `unset OUTHAUL_UPDATES` to `setup` so each scenario starts from `main`; the `main` scenarios leave it unset.

- [ ] **Step 11: Run it to see it fail**

Run: `dash deploy/test/test_update.sh`
Expected: FAIL because `update_main` ignores the setting.

- [ ] **Step 12: Implement the updater's refusal**

In `update_main`, after `exec 3<>/dev/null` and `exec 4>&2`, before `mkdir -p "$WORK_DIR"`:

```sh
_mode=$(printf '%s' "${OUTHAUL_UPDATES:-main}" | tr '[:upper:]' '[:lower:]')
if [ "$_mode" != main ]; then
	in_update_dir rm -f request 2>/dev/null || true
	to_journal "updates are off; nothing changed"
	exit 0
fi
```

`in_update_dir` refuses a symlinked update directory, so the request is not removed through a link; the run still exits 0. Update the two unit files' comments and `Description=` to say "updates from main, or nothing when OUTHAUL_UPDATES=off".

- [ ] **Step 13: Run the tests**

Run:
```sh
go test ./internal/config ./internal/server . -run 'Updates|NewUpdates|Version' -v
dash deploy/test/test_update.sh
bash --posix deploy/test/test_update.sh
busybox sh deploy/test/test_update.sh
```
Expected: PASS.

- [ ] **Step 14: Commit**

```sh
git add internal/config/config.go internal/config/config_test.go main.go main_test.go \
  internal/server/server.go internal/server/updates.go internal/server/settings.go \
  internal/server/templates/settings.tmpl internal/server/updates_test.go \
  deploy/update.sh deploy/test/test_update.sh deploy/outhaul-update.path deploy/outhaul-update.service
git commit -m "feat(update): OUTHAUL_UPDATES=off keeps the installed version" \
  -m "The panel, the six-hour check and the root updater all moved to main with no way to stop them. off shows the running commit and version, removes the forms and the badge, answers the update routes 404, starts no check, and makes the updater remove the request and change nothing."
```

---

### Task 3: Docker's three states

Use a working Docker as it is, install it only when it is absent and allowed, and never run `get.docker.com` on a host that already has it.

**Files:**
- Modify: `deploy/bootstrap.sh` (`ensure_docker`)
- Test: `deploy/test/test_docker.sh` (new)

**Interfaces:**
- Consumes: `NONINTERACTIVE` (set in Task 7; default `0` until then) and `INSTALL_DOCKER` (default `0`).
- Produces:
  - `have_docker()`, `have_docker_service()`, `have_docker_socket()` — small predicates tests override.
  - `docker_works()` — `have_docker || return 1`; then true when `docker version --format '{{.Server.Version}}'` and `docker compose version` both succeed; it waits up to 30 one-second tries.
  - `docker_problem()` — prints `the Docker daemon is not answering` or `the docker compose v2 plugin is missing`.
  - `ensure_docker()` — the three-state behavior below.

- [ ] **Step 1: Write the failing test**

`deploy/test/test_docker.sh`:

```sh
#!/usr/bin/env sh
here=$(cd "$(dirname "$0")" && pwd)
. "$here/assert.sh"
OUTHAUL_INSTALLER_LIB=1 . "$here/../bootstrap.sh"

T=$(mktemp -d)
CALLS=$T/calls
: > "$CALLS"
sleep() { :; }
quiet() { ( "$@" ) >/dev/null 2>&1; }
docker() { echo "docker $*" >> "$CALLS"; case "$*" in
  *"version --format"*) [ "${DOCKER_OK:-1}" = 1 ] ;;
  *"compose version"*) [ "${COMPOSE_OK:-1}" = 1 ] ;;
esac; }
systemctl() { echo "systemctl $*" >> "$CALLS"; }
# A file, not a shell variable: curl runs in a pipeline's subshell, so an
# assignment inside it would not reach the parent.
DOCKER_FLAG=$T/docker-installed
have_docker() { [ -e "$DOCKER_FLAG" ]; }
curl() {
	echo "curl $*" >> "$CALLS"
	case "$*" in *get.docker.com*) : > "$DOCKER_FLAG" ;; esac
}
LOGFILE=/dev/null
have_docker_service() { return 1; }
have_docker_socket() { return 1; }

# Working: used, nothing installed, enabled or changed.
: > "$DOCKER_FLAG"
: > "$CALLS"
assert_ok "working Docker is used" ensure_docker >/dev/null
assert_eq "working: no get.docker.com" 0 "$(grep -c 'get.docker.com' "$CALLS" || true)"
assert_eq "working: no systemctl" 0 "$(grep -c '^systemctl' "$CALLS" || true)"

# Absent, non-interactive, no --install-docker: fail before any install.
rm -f "$DOCKER_FLAG"
NONINTERACTIVE=1 INSTALL_DOCKER=0
: > "$CALLS"
assert_fail "absent: non-interactive fails" quiet ensure_docker
assert_eq "absent: no get.docker.com" 0 "$(grep -c 'get.docker.com' "$CALLS" || true)"

# Absent, --install-docker: get.docker.com runs, then Docker works.
INSTALL_DOCKER=1
DOCKER_OK=1 COMPOSE_OK=1
: > "$CALLS"
assert_ok "absent: --install-docker installs" ensure_docker >/dev/null
assert_eq "absent: get.docker.com once" 1 "$(grep -c 'get.docker.com' "$CALLS")"
assert_ok "absent: docker now exists" [ -e "$DOCKER_FLAG" ]

# Absent, the wizard: get.docker.com runs too.
rm -f "$DOCKER_FLAG"
NONINTERACTIVE=0 INSTALL_DOCKER=0
: > "$CALLS"
assert_ok "absent: the wizard installs" ensure_docker >/dev/null
assert_eq "absent: the wizard runs get.docker.com" 1 "$(grep -c 'get.docker.com' "$CALLS")"

# Present but not working: enable once, wait, fail, never get.docker.com.
: > "$DOCKER_FLAG"
have_docker_service() { return 0; }
DOCKER_OK=0
: > "$CALLS"
assert_fail "broken: fails" quiet ensure_docker
assert_eq "broken: enable --now once" 1 "$(grep -c '^systemctl enable --now docker$' "$CALLS")"
assert_eq "broken: no get.docker.com" 0 "$(grep -c 'get.docker.com' "$CALLS" || true)"

# Compose missing: the message names it.
DOCKER_OK=1 COMPOSE_OK=0
out=$(ensure_docker 2>&1) || true
assert_contains "broken: names compose" "$out" "docker compose v2 plugin is missing"

rm -rf "$T"
[ "${TESTS_FAIL:-0}" -eq 0 ]
```

- [ ] **Step 2: Run it to see it fail**

Run: `dash deploy/test/test_docker.sh`
Expected: FAIL because the predicates do not exist and `ensure_docker` runs `get.docker.com` whenever `docker` is missing.

- [ ] **Step 3: Implement the three states**

Replace `ensure_docker` with:

```sh
have_docker() { command -v docker >/dev/null 2>&1; }
have_docker_service() { systemctl cat docker.service >/dev/null 2>&1; }
have_docker_socket() { [ -e /var/run/docker.sock ]; }

docker_works() {
	have_docker || return 1
	_i=0
	while [ "$_i" -lt 30 ]; do
		if docker version --format '{{.Server.Version}}' >/dev/null 2>&1 &&
			docker compose version >/dev/null 2>&1; then
			return 0
		fi
		sleep 1
		_i=$((_i + 1))
	done
	return 1
}
```

`ensure_docker`:
1. If `docker_works`: `step_ok "Docker is working"`; return.
2. If absent (`! have_docker && ! have_docker_service && ! have_docker_socket`):
   - In non-interactive without `INSTALL_DOCKER=1`: `die "Docker is not installed; install it first or pass --install-docker"`.
   - Otherwise run `curl -fsSL https://get.docker.com | sh` to the log, then `systemctl enable --now docker`, then `docker_works`; on failure `die` naming the problem.
3. Anything else: `systemctl enable --now docker` once, `docker_works` again (30 more tries), then `die "$(docker_problem)"`. `get.docker.com` never runs.

`docker_problem` checks the daemon first, then compose, and prints the matching sentence.

- [ ] **Step 4: Run the test under all three shells**

Run:
```sh
dash deploy/test/test_docker.sh
bash --posix deploy/test/test_docker.sh
busybox sh deploy/test/test_docker.sh
```
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add deploy/bootstrap.sh deploy/test/test_docker.sh
git commit -m "feat(installer): use a working Docker, install it only when absent" \
  -m "A host with Docker already installed still ran get.docker.com whenever the daemon was slow, and a daemon that never answered was never noticed. The installer now tells working, absent and broken apart; non-interactive runs refuse an absent Docker unless --install-docker is given."
```

---

### Task 4: Pinned nixpacks

Replace `curl … | bash` with one version and a checked SHA-256.

**Files:**
- Modify: `deploy/bootstrap.sh` (`ensure_nixpacks`, cleanup)
- Test: `deploy/test/test_nixpacks.sh` (new)

**Interfaces:**
- Consumes: `NONINTERACTIVE` and `WANT_NIXPACKS`/`NIXPACKS_REPLACE` (set in Task 7; `NIXPACKS_REPLACE` defaults to `0`, and `--nixpacks on` sets it to `1`).
- Produces:
  - `NIXPACKS_VERSION=1.41.0`, `NIXPACKS_SHA256_AMD64=…`, `NIXPACKS_SHA256_ARM64=…`.
  - `nixpacks_target(uname_m)` → `x86_64-unknown-linux-musl` or `aarch64-unknown-linux-musl`.
  - `nixpacks_sha256(uname_m)`.
  - `nixpacks_installed_version()` → the version from `nixpacks --version`, or empty.
  - `install_nixpacks_pin()`.
  - `ensure_nixpacks()` — uses `NIXPACKS_REPLACE=1` to replace another version; `0` leaves it with a note.

- [ ] **Step 1: Write the failing test**

`deploy/test/test_nixpacks.sh`:

```sh
#!/usr/bin/env sh
here=$(cd "$(dirname "$0")" && pwd)
. "$here/assert.sh"
OUTHAUL_INSTALLER_LIB=1 . "$here/../bootstrap.sh"

assert_eq "pinned version" "1.41.0" "$NIXPACKS_VERSION"
assert_eq "amd64 digest" "0f55de7874507b9cf7502113120bd96f2ab6979f78d10eaf2eb2ade9207b3af6" "$NIXPACKS_SHA256_AMD64"
assert_eq "arm64 digest" "912bd02dd2bb6f9c3a9ed965fe8a68b4aa318dc7a2546e2eca6f2806a894ba39" "$NIXPACKS_SHA256_ARM64"
assert_eq "amd64 target" "x86_64-unknown-linux-musl" "$(nixpacks_target x86_64)"
assert_eq "arm64 target" "aarch64-unknown-linux-musl" "$(nixpacks_target aarch64)"

T=$(mktemp -d)
quiet() { ( "$@" ) >/dev/null 2>&1; }
mkdir "$T/bin" "$T/src"
printf '#!/bin/sh\necho "nixpacks %s"\n' "$NIXPACKS_VERSION" > "$T/bin/nixpacks"
chmod +x "$T/bin/nixpacks"
PATH=$T/bin:$PATH
assert_eq "reads the pin" "$NIXPACKS_VERSION" "$(nixpacks_installed_version)"

# A fixture archive whose digest we set as the pin: real sha256sum.
mkdir "$T/archive"
printf '#!/bin/sh\necho nixpacks\n' > "$T/archive/nixpacks"
tar -C "$T/archive" -czf "$T/nixpacks.tar.gz" nixpacks
sum=$(sha256sum "$T/nixpacks.tar.gz" | awk '{print $1}')
NIXPACKS_SHA256_AMD64=$sum
NIXPACKS_PATH=$T/installed
curl() {
	while [ "$#" -gt 0 ]; do
		if [ "$1" = -o ]; then cp "$T/nixpacks.tar.gz" "$2"; return 0; fi
		shift
	done
	return 1
}
LOGFILE=/dev/null
assert_ok "installs the pin" install_nixpacks_pin >/dev/null
assert_ok "installs one executable" [ -x "$NIXPACKS_PATH" ]
```

`NIXPACKS_PATH` is the seam; it defaults to `/usr/local/bin/nixpacks`. The test must not touch the real path.

For the table:
- none → installs the pin;
- same version → nothing;
- another version with `NIXPACKS_REPLACE=0` → left, note names `1.41.0`;
- another version with `NIXPACKS_REPLACE=1` → replaced;
- `--nixpacks off` is tested in `test_noninteractive.sh` (Task 9): nothing runs.
- A wrong checksum: set `NIXPACKS_SHA256_AMD64=deadbeef…` (64 hex) and `assert_fail "wrong checksum installs nothing" quiet install_nixpacks_pin`, then assert `$NIXPACKS_PATH` does not exist.
- `grep -F '| bash' deploy/bootstrap.sh` finds nothing and `grep -F 'nixpacks.com/install.sh'` finds nothing.

- [ ] **Step 2: Run it to see it fail**

Run: `dash deploy/test/test_nixpacks.sh`
Expected: FAIL because the constants and functions do not exist.

- [ ] **Step 3: Implement the pin**

Use:

```sh
NIXPACKS_VERSION=1.41.0
NIXPACKS_SHA256_AMD64=0f55de7874507b9cf7502113120bd96f2ab6979f78d10eaf2eb2ade9207b3af6
NIXPACKS_SHA256_ARM64=912bd02dd2bb6f9c3a9ed965fe8a68b4aa318dc7a2546e2eca6f2806a894ba39
NIXPACKS_PATH=/usr/local/bin/nixpacks
NIXPACKS_REPLACE=${NIXPACKS_REPLACE:-0}
```

`install_nixpacks_pin` downloads `https://github.com/railwayapp/nixpacks/releases/download/v$NIXPACKS_VERSION/nixpacks-v$NIXPACKS_VERSION-$target.tar.gz` into a temp dir, checks `echo "$sum  $archive" | sha256sum -c -`, extracts, and `install_atomic "$tmp/nixpacks" "$NIXPACKS_PATH" 0755`. A wrong checksum is `die` in both modes. A failed download is a `note` and return 0 in the wizard, `die` when `NONINTERACTIVE=1`.

`ensure_nixpacks`:
- none → `install_nixpacks_pin`.
- same as pin → `step_ok "nixpacks $NIXPACKS_VERSION present"`.
- another version and `NIXPACKS_REPLACE=1` → `install_nixpacks_pin`.
- another version and `NIXPACKS_REPLACE=0` → `note "nixpacks $v is installed; the installer pins $NIXPACKS_VERSION (re-run with --non-interactive --nixpacks on to replace it)"` — the wizard cannot pass `--nixpacks on` without `--non-interactive`.

Add the temp dir to `cleanup` (or remove it on every path) so a failed run leaves nothing.

- [ ] **Step 4: Run the test under all three shells**

Run:
```sh
dash deploy/test/test_nixpacks.sh
bash --posix deploy/test/test_nixpacks.sh
busybox sh deploy/test/test_nixpacks.sh
```
Expected: PASS. Check the fixture's real digest path, not a stubbed `sha256sum`.

Before committing, download both release archives once and compare `sha256sum` with the two constants. The pin is only as good as those values.

- [ ] **Step 5: Commit**

```bash
git add deploy/bootstrap.sh deploy/test/test_nixpacks.sh
git commit -m "feat(installer): pin nixpacks to 1.41.0 with a checked checksum" \
  -m "The installer piped upstream's install script into bash, so every host got whatever was newest. It now downloads the pinned musl archive for the host's architecture, checks its SHA-256, and installs the one file inside; another installed version is replaced only when asked."
```

---

### Task 5: `--ref`, the version stamp, the marker, and no rebuild when unchanged

Install exactly the commit or tag asked for; stamp a tag's name into the binary; record what the last run installed; and skip the build when the same commit is already there.

**Files:**
- Modify: `internal/update/build.go`, `internal/update/build_test.go`
- Modify: `main.go` (`runVersion`), `main_test.go`
- Modify: `deploy/bootstrap.sh` (`parse_args`, `resolve_ref`, `fetch_source`, `build_ldflags`, `build_binary`, `main`)
- Modify: `deploy/test/test_update_build.sh`
- Test: `deploy/test/test_ref.sh` (new)

**Interfaces:**
- Consumes: `Config`/`Build` from Tasks 1–2; the installer's `OUTHAUL_REPO` and `FROM_CHECKOUT`/`CHECKOUT_DIR`.
- Produces:
  - `update.Build.Version string`; `update.Current()` reads a `version` link-time variable; `runVersion` prints `version=<v>`.
  - `resolve_ref(ref, repo)` sets `TARGET` (40-char commit), `TARGET_VERSION` (the tag name, or empty) and `TARGET_REF` (the ref as given, `HEAD` when empty).
  - `fetch_source(target)` initialises a repo, fetches `target` at depth 1, checks out `FETCH_HEAD`, and requires `HEAD == target`. With no argument it resolves the remote's `HEAD` first (`resolve_ref '' "$OUTHAUL_REPO"`), so `deploy/update.sh`'s existing `fetch_source` call keeps working.
  - `build_ldflags(commit, repo, local, version)`.
  - `build_field(name)` and `short(commit)` — copied from `update.sh` so the installer can read the running build before Task 6 moves the rest.
  - `needs_build(installed, target, marker)` — true when there is no installed commit, or `installed != target`, or `marker != target`.
  - `write_marker(commit, ref, version)`, `read_marker_commit()`; `MARKER=/var/lib/outhaul-updater/installed` (plain; tests override it after sourcing).

- [ ] **Step 1: Write the failing Go tests**

In `internal/update/build_test.go`, extend `TestCurrentReadsTheStamp` with `setStamp(t, "0123…", "https://…", "true", "v1.2.3")` and assert `Version == "v1.2.3"`; add a no-version case. In `main_test.go`, change `TestRunVersionPrintsTheBuild` to expect:

```go
want := "commit=abc\nrepo=https://github.com/o/r.git\nlocal=false\nversion=v1.2.3\n"
```

and add a build with `Version: ""` expecting `version=` (an empty line value).

- [ ] **Step 2: Run them to see them fail**

Run: `go test ./internal/update . -run 'Current|RunVersion' -v`
Expected: FAIL because `Build.Version` and the `version` stamp are undefined.

- [ ] **Step 3: Implement the stamp**

In `internal/update/build.go`: add `version string` to the link-time vars, `Version string` to `Build`, read it in `Current()`, and mention it in the doc comment. In `main.go` `runVersion`, print `version=%s\n` after `local=`. In `deploy/bootstrap.sh` `build_ldflags`, add `[ -n "${4:-}" ] && out="$out -X $pkg.version=$4"` — the `${4:-}` matters because the script runs under `set -u` and existing tests call it with three arguments.

Add `BIN=/usr/local/bin/outhaul` (plain, not `${BIN:-…}`) and `build_field`/`short` to `bootstrap.sh`, copied from `update.sh`; `update.sh`'s copies still override it until Task 6 deletes them. Tests set `BIN` after sourcing.

- [ ] **Step 4: Write the failing shell test**

`deploy/test/test_ref.sh`:

```sh
#!/usr/bin/env sh
here=$(cd "$(dirname "$0")" && pwd)
. "$here/assert.sh"
OUTHAUL_INSTALLER_LIB=1 . "$here/../bootstrap.sh"

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
```

`resolve_ref` and `fetch_source` call `die`; the `refused`/`quiet` subshells keep the test alive.

- [ ] **Step 5: Run it to see it fail**

Run: `dash deploy/test/test_ref.sh`
Expected: FAIL because `resolve_ref`, `fetch_source`, `build_ldflags`' fourth argument, `write_marker` and `needs_build` do not exist.

- [ ] **Step 6: Implement ref resolution and fetching**

`resolve_ref`:
- Empty ref: `TARGET=$(git ls-remote "$repo" HEAD | awk 'NR==1{print $1}')`; `TARGET_VERSION=''`; `TARGET_REF=HEAD`; fail when empty.
- A 40-character hex string: `TARGET` is that string, lower-cased; `TARGET_VERSION=''`; `TARGET_REF` is the input. No network.
- Another all-hex string of 7–39 characters: `die "a shortened commit cannot be fetched; give the full 40 characters"`.
- Anything else: run `git ls-remote "$repo"` once, then pick the first of `refs/tags/<ref>^{}` (annotated tag), `refs/tags/<ref>`, `refs/heads/<ref>` with `awk`. A tag match sets `TARGET_VERSION` to the ref name; a branch match leaves it empty. No match is `die`.
- Branch names containing `/` work because the ref is interpolated as a whole.

`fetch_source(target)`:
- `--from-checkout`: keep the existing behavior; `SRC_DIR` is the checkout and `HEAD` is the target. `--ref` with `--from-checkout` is refused in Task 7.
- With no argument, call `resolve_ref '' "$OUTHAUL_REPO"` and use `$TARGET`; this is the updater's path and keeps its `fetch_source` call unchanged.
- Otherwise `SRC_DIR=$(mktemp -d)`, `git init "$SRC_DIR"`, `git -C "$SRC_DIR" fetch --depth 1 "$OUTHAUL_REPO" "$target"`, `git -C "$SRC_DIR" checkout --detach FETCH_HEAD`, then `git -C "$SRC_DIR" rev-parse HEAD` must equal `target`; otherwise `die "fetched <sha>, not <target>"`.

Fetch by the resolved commit, not the tag or branch name, so a tag moved between `ls-remote` and `fetch` cannot change what is built. The log names the commit a tag resolved to.

- [ ] **Step 7: Implement the marker and the three states**

`write_marker` writes `commit=`, `ref=`, `version=`, `installed=<unix time>` to `MARKER.new` and renames it with `mv -fT`. Create the directory. `read_marker_commit` is `sed -n 's/^commit=//p' "$MARKER" 2>/dev/null`. `needs_build` is:

```sh
needs_build() { # installed target marker
	[ -z "$1" ] && return 0
	[ "$1" != "$2" ] && return 0
	[ "$3" != "$2" ] && return 0
	return 1
}
```

- [ ] **Step 8: Wire the decision into `main`**

Add `--ref` to `parse_args` (the full flag set comes in Task 7). Introduce `ARG_ERRORS=''` and `arg_error()` here, because `--ref` needs the exit-2 path:

```sh
arg_error() { ARG_ERRORS="${ARG_ERRORS}error: $1
"; }
```

In `parse_args`, `--ref` with a following value sets `REF=$2`; without one, `arg_error "--ref needs a value"`. `REF` starts as `${OUTHAUL_REF:-}` in `main` before `parse_args`, so the flag overrides the environment. If `FROM_CHECKOUT=1` and `REF` is non-empty, `arg_error "--ref cannot be used with --from-checkout"`. After `parse_args`, `main` prints `ARG_ERRORS` to stderr and exits 2 when it is non-empty. Task 7 extends the same helper to the other flags.

In `main`, before the build decision:
- `FROM_CHECKOUT=1`: call `fetch_source` first (it sets `SRC_DIR` from `CHECKOUT_DIR` and checks `go.mod`), then `TARGET=$(git -C "$SRC_DIR" rev-parse HEAD 2>/dev/null || true)`, `TARGET_VERSION=''`, `TARGET_REF=''`, `BUILD=1`. If `TARGET` is empty (git cannot read the tree), build unstamped, as `build_binary` does today; `new_version_healthy` treats an empty commit as "any 200".
- Else `resolve_ref "$REF" "$OUTHAUL_REPO"`; `I=$(build_field commit)`; `M=$(read_marker_commit)`; `if needs_build "$I" "$TARGET" "$M"; then BUILD=1; else BUILD=0; fi`.
- When `BUILD=0`, skip `fetch_source`, `go_version_from_gomod`, `maybe_add_swap`, `install_go_toolchain` and `build_binary`. When `BUILD=1`, call them with `TARGET`/`TARGET_VERSION`.
- Pass `TARGET_VERSION` to `build_binary` (`build_ldflags "$c" "$OUTHAUL_REPO" "$l" "$TARGET_VERSION"`).
- After `fetch_source`, when `$0` is a regular file and `$SRC_DIR/deploy/bootstrap.sh` exists, `cmp -s "$0" "$SRC_DIR/deploy/bootstrap.sh"`; when they differ, `note "this installer differs from the one in $TARGET; the fetched tree's copy is what future runs install"`. Under `curl … | sh`, `$0` is not a file and the check is skipped.
- After a successful health check, `write_marker "$TARGET" "$TARGET_REF" "$TARGET_VERSION"`.

`--from-checkout` always builds: do not consult `needs_build` there.

- [ ] **Step 9: Update `test_update_build.sh`**

`build_binary` now passes a fourth argument. Add a case with `TARGET_VERSION=v1` and assert the `-ldflags` value ends `-X ...version=v1`; keep the existing no-version assertions unchanged.

- [ ] **Step 10: Run the tests**

Run:
```sh
go test ./internal/update . -run 'Current|RunVersion' -v
dash deploy/test/test_ref.sh
dash deploy/test/test_update_build.sh
bash --posix deploy/test/test_ref.sh
busybox sh deploy/test/test_ref.sh
```
Expected: PASS.

- [ ] **Step 11: Commit**

```bash
git add internal/update/build.go internal/update/build_test.go main.go main_test.go \
  deploy/bootstrap.sh deploy/test/test_ref.sh deploy/test/test_update_build.sh
git commit -m "feat(installer): install an exact ref and stamp its tag" \
  -m "The installer always cloned the default branch's head, so a host could not be pinned and a re-run always rebuilt. --ref resolves a tag, branch or full commit, fetches that commit at depth 1, checks HEAD, stamps a tag's name, records the run in /var/lib/outhaul-updater/installed, and skips the build when the same commit and marker are already installed."
```

---

### Task 6: The careful swap, with the updater's functions moved

Stop Outhaul, keep a copy, install, start, check, and roll back when the new version does not answer. Move the updater's functions into `bootstrap.sh` so both paths use them.

**Files:**
- Modify: `deploy/bootstrap.sh` (`build_field`, `short`, `pull_images`, `replace_file`, `snapshot`, `restore_snapshot`, `install_new`, `answers_commit`, `new_version_healthy`, `recovery_help`, `install_service`, `main`, `swap_guard`)
- Modify: `deploy/update.sh` (delete the moved definitions)
- Modify: `deploy/test/test_update.sh`, `deploy/test/test_update_install.sh`
- Test: `deploy/test/test_update.sh` (snapshot cases); `deploy/test/test_swap.sh` (new: the swap and its exit guard)

**Interfaces:**
- Consumes: `TARGET`, `TARGET_VERSION`, `TARGET_REF`, `BUILD`, `SRC_DIR`, `FROM_CHECKOUT`, `NONINTERACTIVE`.
- Produces:
  - The updater's functions, now defined in `bootstrap.sh` with the same names and behavior.
  - `snapshot` and `restore_snapshot` also cover `INSTALL_ENV` (default `/etc/outhaul/install.env`), `SERVICE_DROPIN` (default `/etc/systemd/system/outhaul.service.d/10-install.conf`) and `UPDATE_DROPIN` (default `/etc/systemd/system/outhaul-update.service.d/10-install.conf`).
  - `install_or_upgrade(src, target, version)` — returns 0 on a healthy service, 1 on a failed first install or a rolled-back upgrade. It never exits for a health failure, so the wizard can keep its completion screen and exit code; `main` exits 1 only when `NONINTERACTIVE=1`.
  - `rollback_install(from)` — returns 0 when the old version answers, 1 when it does not. With `SNAPSHOT_OK=0` it does not restore an older `previous/` copy; it starts the binary still in place.
  - `swap_guard()` — the exit guard: if `STAGE=swap` and `ROLLBACK_DONE=0`, call `rollback_install "$ROLLBACK_TO"`. It is installed here, not in Task 9.
  - Globals: `STARTED_SERVICE=0`, `SNAPSHOT_OK=0`, `ROLLED_BACK=0`, `HEALTHY=0`, `ADMIN_PORT`, `STAGE=''`, `ROLLBACK_TO=''`, `ROLLBACK_DONE=0`, `ENV_FILE_CHANGED=0`.

- [ ] **Step 1: Write the failing snapshot tests**

In `deploy/test/test_update.sh`, in the snapshot section, create the three files under `$T/etc` and `$T/units`, set `INSTALL_ENV`, `SERVICE_DROPIN`, `UPDATE_DROPIN`, then:

```sh
echo 'old install env' > "$INSTALL_ENV"
mkdir -p "$(dirname "$SERVICE_DROPIN")" "$(dirname "$UPDATE_DROPIN")"
echo 'old service dropin' > "$SERVICE_DROPIN"
echo 'old update dropin' > "$UPDATE_DROPIN"
assert_ok "snapshot keeps the env files" snapshot
assert_eq "snapshot: install.env" "old install env" "$(cat "$WORK_DIR/previous/install.env")"
assert_eq "snapshot: service drop-in" "old service dropin" "$(cat "$WORK_DIR/previous/outhaul.service.d.10-install.conf")"
assert_eq "snapshot: update drop-in" "old update dropin" "$(cat "$WORK_DIR/previous/outhaul-update.service.d.10-install.conf")"
```

Then remove the new files, change the old ones, restore, and assert the old content is back. Then delete `INSTALL_ENV` before a snapshot and assert `restore_snapshot` removes it (the old version had none):

```sh
rm -f "$INSTALL_ENV"
assert_ok "snapshot without install.env" snapshot
echo 'new install env' > "$INSTALL_ENV"
assert_ok "restore without install.env" restore_snapshot
assert_fail "restore removes install.env the snapshot lacks" [ -e "$INSTALL_ENV" ]
```

Add the same for the two drop-ins.

- [ ] **Step 2: Run it to see it fail**

Run: `dash deploy/test/test_update.sh`
Expected: FAIL because `snapshot` does not copy the new files.

- [ ] **Step 3: Extend `snapshot` and `restore_snapshot`**

In `deploy/update.sh` (before moving them), add the three paths with defaults:

```sh
INSTALL_ENV=/etc/outhaul/install.env
SERVICE_DROPIN=/etc/systemd/system/outhaul.service.d/10-install.conf
UPDATE_DROPIN=/etc/systemd/system/outhaul-update.service.d/10-install.conf
```

Plain assignments, not `${VAR:-…}`: tests set these after sourcing, and root's environment must not redirect them.

`snapshot` copies each to `previous/install.env`, `previous/outhaul.service.d.10-install.conf`, `previous/outhaul-update.service.d.10-install.conf` when it exists. `restore_snapshot` replaces it when the snapshot has it, else `rm -f` it, exactly as the `-wal` file is handled. Keep every step checked and `previous.new`/`previous` atomic.

- [ ] **Step 4: Run the snapshot tests**

Run: `dash deploy/test/test_update.sh`
Expected: the snapshot section passes; the rest still passes.

- [ ] **Step 5: Write the failing swap tests**

`deploy/test/test_swap.sh` (new) sources `bootstrap.sh`, points `BIN`, `UNIT_DIR`, `DATA_DIR`, `INSTALL_ENV`, `SERVICE_DROPIN`, `UPDATE_DROPIN`, `WORK_DIR`, `SRC_DIR`, `MARKER` and `ADMIN_PORT` into a temp tree, and stubs `systemctl`, `curl`, `journalctl`, `docker`, `chgrp`, `chown`. It calls `install_or_upgrade` in a subshell with `trap 'swap_guard; cleanup' EXIT`, as `main` will. Scenarios:

```sh
# 1. First install: no $BIN. Installs, starts, checks the target; no snapshot.
# 2. Upgrade healthy: stops, snapshots, installs, starts; old copy kept; return 0.
# 3. Upgrade unhealthy: rollback_install restores the binary, unit, database,
#    install.env and both drop-ins, starts the old version and returns 1.
# 4. A die in write_dropins after the stop: the trap's swap_guard restores the
#    same files, starts the old version, and the exit status is 1.
# 5. Snapshot fails: the old service is started again and SNAPSHOT_OK stays 0;
#    no previous/ copy is restored.
# 6. Restore fails: the old binary is started anyway, recovery_help is printed,
#    and the exit status is 1.
```

For scenario 4, define `write_dropins() { die "swap test: drop-in write failed"; }` after the snapshot; the `die` exits the subshell and the EXIT trap must leave the old version running. This is the guard's only test; it must exist here, not only in Task 9. The shape:

```sh
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
```

The same file's other scenarios assert the healthy upgrade keeps `previous/outhaul`, and the unhealthy upgrade returns 1 with the old files back.

- [ ] **Step 6: Run the swap tests to see them fail**

Run: `dash deploy/test/test_swap.sh`
Expected: FAIL because `install_or_upgrade`/`swap_guard` do not exist.

- [ ] **Step 7: Move the functions**

`build_field` and `short` are already in `bootstrap.sh` from Task 5; delete `update.sh`'s copies. Move these definitions from `deploy/update.sh` to `deploy/bootstrap.sh` unchanged: `pull_images`, `replace_file`, `snapshot`, `restore_snapshot`, `install_new`, `answers_commit`, `new_version_healthy`, `recovery_help`. Delete them from `update.sh`; it already sources `bootstrap.sh`. Keep `source_commit`, `cached_go_toolchain`, `finish`, `roll_back`, `update_exit` and `update_main` in `update.sh`.

Extend `recovery_help` with the files `snapshot` now keeps: when `SNAPSHOT_OK=1`, print a `cp -p previous/install.env $INSTALL_ENV` (or `rm -f $INSTALL_ENV` when the snapshot lacks it) and the same for `SERVICE_DROPIN` and `UPDATE_DROPIN`. A manual recovery must put back the env files the old binary expects, or it can start with the new flags still in place.

`bootstrap.sh` needs the updater's path defaults, as **plain assignments** so a root environment cannot redirect them; tests override them after sourcing:

```sh
BIN=/usr/local/bin/outhaul
UNIT_DIR=/etc/systemd/system
LIB_DIR=/usr/local/lib/outhaul
DATA_DIR=${OUTHAUL_DATA_DIR:-/var/lib/outhaul}   # the one intended setting
UPDATE_DIR=$DATA_DIR/update
WORK_DIR=/var/lib/outhaul-updater
CACHE_DIR=/var/cache/outhaul
INSTALL_ENV=/etc/outhaul/install.env
SERVICE_DROPIN=$UNIT_DIR/outhaul.service.d/10-install.conf
UPDATE_DROPIN=$UNIT_DIR/outhaul-update.service.d/10-install.conf
ENV_FILE=/etc/outhaul.env
```

`install_updater` uses these globals directly (`_lib=$LIB_DIR`, `_units=$UNIT_DIR`, `_upd=$UPDATE_DIR`, `_work=$WORK_DIR`) instead of its current `${LIB_DIR:-…}`-style reads.

`update.sh` keeps its own plain assignments for the updater's paths; `test_update.sh` and `test_update_install.sh` set their temp paths after sourcing. Do not use `${BIN:-…}`-style defaults for these paths.

- [ ] **Step 8: Implement `install_or_upgrade` and `swap_guard`**

Add to `bootstrap.sh`:

```sh
install_or_upgrade() { # src_dir target version
	_src=$1 _target=$2 _version=$3
	_from=$(build_field commit)
	# Branch on the binary, not the stamp: a binary from before the stamp
	# records no commit but is still running, and must go through the swap.
	if [ ! -e "$BIN" ]; then
		pull_images "$_src/outhaul" || return 1
		install_new || die "could not install the new version"
		write_install_env
		write_dropins
		systemctl daemon-reload || die "systemctl daemon-reload failed"
		systemctl enable --now outhaul || die "could not start outhaul"
		STARTED_SERVICE=1
		if ! new_version_healthy "$_target" "$ADMIN_PORT"; then
			step_fail "the new version did not answer"
			journalctl -u outhaul -n 20 --no-pager
			return 1
		fi
		return 0
	fi
	pull_images "$_src/outhaul" || return 1
	printf '  .. stopping outhaul\n'
	ROLLBACK_TO=$_from
	STAGE=swap
	SNAPSHOT_OK=0
	systemctl stop outhaul || die "outhaul did not stop"
	if snapshot; then
		SNAPSHOT_OK=1
	else
		systemctl start outhaul || true
		die "could not keep a copy of the running version"
	fi
	if ! install_new; then rollback_install "$_from" || true; return 1; fi
	write_install_env
	write_dropins
	if ! systemctl daemon-reload; then rollback_install "$_from" || true; return 1; fi
	if ! systemctl start outhaul; then rollback_install "$_from" || true; return 1; fi
	STARTED_SERVICE=1
	if ! new_version_healthy "$_target" "$ADMIN_PORT"; then
		rollback_install "$_from" || true
		return 1
	fi
	STAGE=''
	return 0
}
```

`rollback_install(from)` runs with `set +e` and `trap '' INT TERM` (it must finish while the exit trap is running), stops Outhaul, and when `SNAPSHOT_OK=1` calls `restore_snapshot`; with `SNAPSHOT_OK=0` it leaves the files in place, because the stop or the snapshot itself failed and there is no copy to trust. Then `systemctl daemon-reload`, starts, and checks the old version on **its** port. Read that port after the old service starts (`service_env` from Task 8; until then capture `admin_port "${OUTHAUL_LISTEN_ADDR:-:8080}"` before the stop), then call `new_version_healthy "$from" "$old_port"`. Check every step: a failed restore still starts the old binary and says what could not be restored. If the old version answers, set `ROLLED_BACK=1` and print `step_fail "the new version did not answer; rolled back to <short>"`; return 0. If it does not, print `recovery_help` and `step_fail "the new version did not answer, and the old one did not come back either"`; return 1.

Set `ROLLBACK_TO=$_from` and `STAGE=swap` **before** `systemctl stop`, as the updater does; clear `STAGE` once the new version is healthy. `rollback_install` sets `ROLLBACK_DONE=1` as its first action and clears `STAGE` when it finishes, so an explicit call from `install_or_upgrade` and the exit-trap call cannot both run.

Add the guard:

```sh
swap_guard() {
	[ "${STAGE:-}" = swap ] || return 0
	[ "${ROLLBACK_DONE:-0}" = 0 ] || return 0
	rollback_install "${ROLLBACK_TO:-}" || true
}
```

`rollback_install` restores `previous/` only when `SNAPSHOT_OK=1`. If a signal or a `die` arrived during the stop or the snapshot, there is no snapshot to restore; it starts the binary still in place instead of restoring an older `previous/` copy. This is the guard for a `die` from `write_install_env`/`write_dropins` after the old version was stopped: Task 9's `on_exit` calls `swap_guard` before logging. The explicit checks in `install_or_upgrade` stay for the failures they can see, but the exit guard covers the rest.

`ADMIN_PORT` is `admin_port "${OUTHAUL_LISTEN_ADDR:-:8080}"` until Task 8 replaces it with `service_env`. Task 8 moves the read to after `systemctl start`/`enable --now`, so the running service's port is the one checked.

`write_install_env` and `write_dropins` are defined in Task 7. Add minimal stubs here (`write_install_env() { :; }`, `write_dropins() { :; }`) so this task's swap tests run; Task 7 replaces them with the real writers.

- [ ] **Step 9: Replace `install_service` and `wait_healthy` in `main`**

Delete `install_service` and `wait_healthy`; the first install now uses `new_version_healthy`, which is the updater's 60-try check and does not time out on a slow image pull.

In the build path, call `install_or_upgrade "$SRC_DIR" "$TARGET" "$TARGET_VERSION"` instead of `install_binary` + `install_service`. In the no-build path, reconcile the files and start/restart as the design says:

```sh
write_install_env
write_dropins
# A drop-in is a unit-file change: systemd reads it on daemon-reload, not on restart.
if [ "$DROPIN_CHANGED" = 1 ]; then systemctl daemon-reload; fi
if ! systemctl is-active --quiet outhaul; then
	systemctl enable --now outhaul; STARTED_SERVICE=1
elif [ "$ENV_CHANGED" = 1 ] || [ "$DROPIN_CHANGED" = 1 ] || [ "$ENV_FILE_CHANGED" = 1 ]; then
	systemctl restart outhaul; STARTED_SERVICE=1
fi
if ! new_version_healthy "$TARGET" "$ADMIN_PORT"; then
	if [ "$NONINTERACTIVE" = 1 ]; then die "outhaul did not answer"; fi
	HEALTHY=1
fi
```

In both paths, after the health result:

```sh
if [ "$HEALTHY" = 0 ] && [ "$ROLLED_BACK" = 0 ]; then
	write_marker "$TARGET" "$TARGET_REF" "$TARGET_VERSION"
fi
```

A failed first install or upgrade sets `HEALTHY=1`; non-interactive runs `exit 1`, while the wizard continues to `completion_screen` and keeps its exit code, as the spec's call 14 says. `completion_screen` gets a note when `ROLLED_BACK=1`.

- [ ] **Step 10: Update `test_update_install.sh`**

The test currently asserts `install_updater` ends with `systemctl enable --now outhaul-update.path`. This task does not touch `install_updater`, so leave that assertion; Task 8 changes it when it moves the call to `set_update_path`. `install_new` now lives in `bootstrap.sh`; the test still sources `bootstrap.sh`, so it sees it.

- [ ] **Step 11: Run the shell tests**

Run:
```sh
dash deploy/test/test_update.sh
dash deploy/test/test_update_install.sh
dash deploy/test/test_update_build.sh
dash deploy/test/test_swap.sh
bash --posix deploy/test/test_swap.sh
busybox sh deploy/test/test_swap.sh
```
Expected: PASS. The updater's behavior is unchanged; the moved functions are the same code.

- [ ] **Step 12: Commit**

```bash
git add deploy/bootstrap.sh deploy/update.sh deploy/test/test_update.sh deploy/test/test_update_install.sh deploy/test/test_swap.sh
git commit -m "feat(installer): swap versions carefully and roll back a bad start" \
  -m "The installer replaced the binary and restarted in place, keeping no copy. It now pulls images first, stops Outhaul, snapshots the binary, unit, database, install.env and drop-ins, installs, starts, checks the commit, and puts the old files back when the new version does not answer. The updater's functions moved into bootstrap.sh so both paths use one implementation."
```

---

### Task 7: `--non-interactive`, its flags and checks, `install.env`, and the drop-ins

Add the unattended interface and the files that carry its answers.

**Files:**
- Modify: `deploy/bootstrap.sh` (`parse_args`, new `check_args`, `apply_answers`, `install_env_content`, `write_if_changed`, `write_install_env`, `write_dropins`, `main`)
- Modify: `deploy/test/test_main.sh`, `deploy/test/test_env.sh`

**Interfaces:**
- Consumes: `resolve_ref`/`needs_build` from Task 5; `install_or_upgrade` from Task 6.
- Produces:
  - Globals: `NONINTERACTIVE=0`, `INGRESS=''`, `PUBLIC_URL=''`, `ACME_EMAIL=''`, `GIT_SSH=''`, `NIXPACKS=''`, `FIREWALL=''`, `UPDATES=''`, `INSTALL_DOCKER=0`, `SETUP_URL_FILE=''`, `ENV_FILES=''`, `REF=${OUTHAUL_REF:-}`, `ARG_ERRORS=''`, `ENV_FILE_CHANGED=0`.
  - `parse_args` collects errors with `arg_error`; `check_args` validates combinations; `main` prints all and exits 2 before anything changes.
  - `apply_answers` sets `MODE` (`a`, `b`, `c`, `p`), `LOCAL_CA`, `PUBLIC_URL`, `ACME_EMAIL`, `GITPORT`, `SSH_ADDR`, `WANT_NIXPACKS`, `NIXPACKS_REPLACE`, `WANT_FW`, `UPDATES`.
  - `install_env_content` prints the file's body; `write_install_env` writes it only when it differs; `write_dropins` writes both drop-ins.
  - `write_if_changed(path, content, mode)` returns 0 when it wrote, 1 when the file already matched.

- [ ] **Step 1: Write the failing flag tests**

In `deploy/test/test_main.sh`, add cases that run `parse_args`/`check_args` in a subshell and capture the exit code:

```sh
arg_rc() { ( parse_args "$@"; check_args ) >/dev/null 2>&1; echo $?; }
assert_eq "answer flag needs --non-interactive" 2 "$(arg_rc --ingress local)"
assert_eq "missing ingress" 2 "$(arg_rc --non-interactive)"
assert_eq "letsencrypt needs a URL" 2 "$(arg_rc --non-interactive --ingress letsencrypt --acme-email a@example.com)"
assert_eq "letsencrypt needs an email" 2 "$(arg_rc --non-interactive --ingress letsencrypt --public-url https://o.example.com)"
assert_eq "local-ca refuses an email" 2 "$(arg_rc --non-interactive --ingress local-ca --acme-email a@example.com)"
assert_eq "ref with from-checkout" 2 "$(arg_rc --ref abc --from-checkout /tmp)"
assert_eq "bad git port" 2 "$(arg_rc --non-interactive --ingress local --git-ssh 70000)"
assert_eq "bad nixpacks" 2 "$(arg_rc --non-interactive --ingress local --nixpacks maybe)"
assert_eq "bad firewall" 2 "$(arg_rc --non-interactive --ingress local --firewall always)"
assert_eq "bad updates" 2 "$(arg_rc --non-interactive --ingress local --updates weekly)"
assert_eq "a missing value" 2 "$(arg_rc --ingress)"
assert_eq "an unknown option" 2 "$(arg_rc --bogus)"
assert_eq "the accepted command" 0 "$(arg_rc --non-interactive --ingress local --git-ssh off --nixpacks on --firewall leave --updates off)"
```

For the env-file checks, define `T=$(mktemp -d)`, create temp files, and stub `stat` to say root owns them:

```sh
stat() { echo '0 600'; }
assert_eq "relative env file" 2 "$(arg_rc --non-interactive --ingress local --env-file rel.env)"
assert_eq "missing env file" 2 "$(arg_rc --non-interactive --ingress local --env-file /no/such/file)"
stat() { echo '1000 600'; }
assert_eq "not root's env file" 2 "$(arg_rc --non-interactive --ingress local --env-file "$T/env")"
stat() { echo '0 666'; }
assert_eq "writable env file" 2 "$(arg_rc --non-interactive --ingress local --env-file "$T/env")"
stat() { echo '0 600'; }
: > "$T/a\"b"; : > "$T/a%b"; : > "$T/a
b"
assert_eq "quote in an env path" 2 "$(arg_rc --non-interactive --ingress local --env-file "$T/a\"b")"
assert_eq "percent in an env path" 2 "$(arg_rc --non-interactive --ingress local --env-file "$T/a%b")"
assert_eq "newline in an env path" 2 "$(arg_rc --non-interactive --ingress local --env-file "$T/a
b")"
mkdir -p "$T/with space"; : > "$T/with space/env"
assert_eq "a space is allowed" 0 "$(arg_rc --non-interactive --ingress local --env-file "$T/with space/env")"
```

Add a proxy case with a provider line:

```sh
printf 'OUTHAUL_ACME_DNS_PROVIDER=azuredns\n' > "$T/env"
stat() { echo '0 600'; }
assert_eq "proxy without a provider" 2 "$(arg_rc --non-interactive --ingress proxy --public-url https://o.example.com --acme-email a@example.com)"
assert_eq "proxy with a provider" 0 "$(arg_rc --non-interactive --ingress proxy --public-url https://o.example.com --acme-email a@example.com --env-file "$T/env")"
```

Also assert `--env-file` twice keeps order and `--ref` overrides `OUTHAUL_REF`:

```sh
parse_args --env-file /a --env-file /b
assert_eq "env files in order" "/a
/b" "$ENV_FILES"
assert_eq "flag beats OUTHAUL_REF" from-flag \
  "$( OUTHAUL_REF=from-env REF=from-env; parse_args --ref from-flag; printf '%s\n' "$REF" )"
```

- [ ] **Step 2: Run it to see it fail**

Run: `dash deploy/test/test_main.sh`
Expected: FAIL because the flags and `check_args` do not exist.

- [ ] **Step 3: Write the failing env-file tests**

In `deploy/test/test_env.sh`, define `T=$(mktemp -d)` and `CALLS=$T/calls` at the top of the new section, then:

```sh
# Every ingress writes its lines, in the design's order.
assert_eq "letsencrypt" \
'# Outhaul unattended install answers — written by --non-interactive.
# Each --non-interactive run rewrites this file.
# Put anything else in /etc/outhaul.env or an --env-file.
OUTHAUL_PUBLIC_URL=https://o.example.com
OUTHAUL_ACME_EMAIL=ops@example.com
OUTHAUL_SSH_ADDR=:2222' "$(MODE=a PUBLIC_URL=https://o.example.com ACME_EMAIL=ops@example.com SSH_ADDR=:2222 install_env_content)"

assert_contains "local CA" "$(MODE=c LOCAL_CA=1 SSH_ADDR=off install_env_content)" "OUTHAUL_LOCAL_CA=true"
assert_contains "proxy HTTP off" "$(MODE=p PUBLIC_URL=https://o.example.com ACME_EMAIL=ops@example.com SSH_ADDR=off install_env_content)" "OUTHAUL_HTTP_PORT=off"
assert_contains "proxy admin direct off" "$(MODE=p PUBLIC_URL=https://o.example.com ACME_EMAIL=ops@example.com SSH_ADDR=off install_env_content)" "OUTHAUL_ADMIN_DIRECT=off"
assert_contains "updates line" "$(MODE=c SSH_ADDR=off UPDATES=off install_env_content)" "OUTHAUL_UPDATES=off"

# An unchanged file keeps its inode.
INSTALL_ENV=$T/install.env
NONINTERACTIVE=1 MODE=c SSH_ADDR=off write_install_env
before=$(ls -i "$INSTALL_ENV" | awk '{print $1}')
NONINTERACTIVE=1 MODE=c SSH_ADDR=off write_install_env
assert_eq "unchanged install.env keeps its inode" "$before" "$(ls -i "$INSTALL_ENV" | awk '{print $1}')"
assert_eq "install.env is 0600" 600 "$(stat -c '%a' "$INSTALL_ENV")"

# The drop-in lists install.env first, then each env file in order.
SERVICE_DROPIN=$T/units/outhaul.service.d/10-install.conf
UPDATE_DROPIN=$T/units/outhaul-update.service.d/10-install.conf
ENV_FILES="/etc/outhaul/a.env
/etc/outhaul/b.env"
NONINTERACTIVE=1 write_dropins
body=$(cat "$SERVICE_DROPIN")
assert_contains "drop-in service section" "$body" "[Service]"
assert_contains "drop-in install.env" "$body" "EnvironmentFile=/etc/outhaul/install.env"
assert_contains "drop-in a" "$body" "EnvironmentFile=/etc/outhaul/a.env"
assert_contains "drop-in b" "$body" "EnvironmentFile=/etc/outhaul/b.env"
assert_eq "drop-in order" "1" "$(printf '%s\n' "$body" | grep -n 'install.env' | cut -d: -f1)"
assert_eq "update drop-in matches" "$body" "$(cat "$UPDATE_DROPIN")"
assert_eq "drop-in is 0644" 644 "$(stat -c '%a' "$SERVICE_DROPIN")"
```

Add a case with `NONINTERACTIVE=0` and `ENV_FILES=''` that writes a sentinel drop-in, runs `write_dropins`, and asserts it is unchanged.

- [ ] **Step 4: Run it to see it fail**

Run: `dash deploy/test/test_env.sh`
Expected: FAIL because `install_env_content`, `write_install_env` and `write_dropins` do not exist.

- [ ] **Step 5: Implement `parse_args` and `check_args`**

`parse_args`:
- Start with `ARG_ERRORS=''`, so each call reports only its own problems.
- Keep `--verbose`, `--from-checkout DIR`, `--help`.
- Add `--non-interactive`, `--ingress`, `--public-url`, `--acme-email`, `--git-ssh`, `--nixpacks`, `--firewall`, `--updates`, `--install-docker`, `--setup-url-file`, `--env-file` (repeatable), `--ref`.
- A flag without its value is `arg_error "<flag> needs a value"`; an unknown option is `arg_error "unknown option: <arg>"` (exit 2, not the old `die`/exit 1).
- `--env-file` rejects a value containing a newline (`arg_error "--env-file path contains a newline"`) because `ENV_FILES` is newline-separated; otherwise it appends `$2` and a newline to `ENV_FILES`. Quotes and `%` are rejected in `check_args`, where the file can be tested.
- `--help` prints the full usage and exits 0.

`check_args`:
- If `NONINTERACTIVE=0`, each answer flag present is an error (`--ingress` through `--setup-url-file`); `--env-file` and `--ref` are allowed.
- `--non-interactive` needs `--ingress` in `letsencrypt tunnel local local-ca proxy`.
- `letsencrypt` and `proxy` need `--public-url` and `--acme-email`.
- `proxy` needs a non-empty `OUTHAUL_ACME_DNS_PROVIDER=` line in one `--env-file`.
- `--acme-email` is refused for `tunnel`, `local` and `local-ca`.
- `--ref` with `--from-checkout` is an error.
- `--git-ssh` is `off` or a port 1–65535; `--nixpacks` is `on|off`; `--firewall` is `leave|ufw`; `--updates` is `main|off`; values are case-insensitive and normalized.
- Each `--env-file` is absolute, `[ -f ]`, `stat -c '%u %a'` says uid 0 and no group/other write bits, and its path contains no newline, double quote or `%` (systemd cannot load it, and the drop-in line would be wrong).
- If `ARG_ERRORS` is non-empty: print each line to stderr and `exit 2` before `init_log` or `preflight`.

`apply_answers` maps the flags to the variables in the Interfaces block. `--ingress local-ca` sets `MODE=c LOCAL_CA=1`; `proxy` sets `MODE=p`; `--nixpacks on` sets `NIXPACKS_REPLACE=1` (the wizard leaves it `0`).

- [ ] **Step 6: Implement the env files**

`install_env_content` prints the comment lines — including "Each --non-interactive run rewrites this file" — and, in order: `OUTHAUL_PUBLIC_URL` when non-empty, `OUTHAUL_ACME_EMAIL` when non-empty, `OUTHAUL_LOCAL_CA=true` for `c` with `LOCAL_CA=1`, `OUTHAUL_HTTP_PORT=off` and `OUTHAUL_ADMIN_DIRECT=off` for `p`, `OUTHAUL_SSH_ADDR=<:port|off>`, and `OUTHAUL_UPDATES` when non-empty.

`write_if_changed` writes to a temp file in the destination's directory, `chmod`s it, `cmp -s` against the destination, and renames only when different. Create the directory first. It returns 0 when it wrote and 1 when the file already matched; a failure to create, write or rename is `die`, not `1`.

`write_install_env` returns at once unless `NONINTERACTIVE=1` (the wizard never writes this file), writes mode `0600` to `INSTALL_ENV`, and sets `ENV_CHANGED=1` on a write; `write_dropins` writes mode `0644` to both drop-ins and sets `DROPIN_CHANGED=1`. Both wrappers **always return 0 on success**, wrote or not: the installer runs under `set -e`, and a standalone call that returned 1 for "unchanged" would abort the run. Use `if write_if_changed ...; then ENV_CHANGED=1; fi` inside them, not a bare call.

`write_dropins` writes when `NONINTERACTIVE=1` or `ENV_FILES` is non-empty:
```
[Service]
EnvironmentFile=/etc/outhaul/install.env     # only when non-interactive
EnvironmentFile=<each env file, in order>
```
Write the same body to both drop-ins. Write each path **bare**, even when it contains a space: systemd 259 accepts a bare path with a space and ignores a quoted one ("path is not absolute, ignoring"). `check_args` therefore refuses an `--env-file` path containing a newline, a double quote or `%`; those cannot be written safely. When neither condition holds, touch nothing. Never remove or rewrite another file in those directories: the host's own drop-ins, such as the `LoadCredential=` one for R16, sit beside this one.

- [ ] **Step 7: Wire the flags into `main`**

In `main`: `REF=${OUTHAUL_REF:-}`; `parse_args "$@"; check_args`. If `NONINTERACTIVE=1`, call `apply_answers` instead of `choose_ingress` and the other prompts, set `DEBIAN_FRONTEND=noninteractive`, and `exec 3<>/dev/null`. Keep the wizard path unchanged.

The existing `/etc/outhaul.env` block must not run unattended:

```sh
ENV_FILE_CHANGED=0
if [ "$NONINTERACTIVE" = 0 ]; then
	if [ ! -f "$ENV_FILE" ]; then
		write_env_file "$ENV_FILE" "$MODE" "$PUBLIC_URL" "$ACME_EMAIL" "$SSH_ADDR" "${LOCAL_CA:-0}"
		ENV_FILE_CHANGED=1
		step_ok "wrote $ENV_FILE"
	else
		note "$ENV_FILE exists — left unchanged"
	fi
fi
```

Run this **before** `install_or_upgrade`/the no-build reconcile, so the wizard's first start loads the file. `ENV_FILE` defaults to `/etc/outhaul.env` and is the test seam. A non-interactive run never creates or changes that file. The no-build restart condition includes `ENV_FILE_CHANGED`. Call `write_install_env`/`write_dropins` in `install_or_upgrade` (Task 6 stubs) and in the no-build path.

- [ ] **Step 8: Run the tests**

Run:
```sh
dash deploy/test/test_main.sh
dash deploy/test/test_env.sh
bash --posix deploy/test/test_main.sh
bash --posix deploy/test/test_env.sh
busybox sh deploy/test/test_main.sh
busybox sh deploy/test/test_env.sh
```
Expected: PASS.

- [ ] **Step 9: Commit**

```bash
git add deploy/bootstrap.sh deploy/test/test_main.sh deploy/test/test_env.sh
git commit -m "feat(installer): answer every prompt with a flag and write install.env" \
  -m "Unattended runs had no way to give answers without a terminal, and a flag changed on a re-run would be ignored because /etc/outhaul.env is written once. --non-interactive now takes every answer from a flag, checks them all before changing anything, writes /etc/outhaul/install.env, and loads it and each --env-file through a drop-in for both the service and the updater."
```

---

### Task 8: `service_env`, the health checks, `--ingress proxy`, and the path unit

Read the running service's environment, send the required header without putting its value in an argument, and check that `proxy` actually took effect.

**Files:**
- Modify: `deploy/bootstrap.sh` (`service_env`, `wait_public_healthy`, `proxy_effective`, `install_updater`, `setup_updater`, `main`, `completion_screen`)
- Modify: `deploy/test/test_health.sh`, `deploy/test/test_firewall.sh`, `deploy/test/test_update_install.sh`

**Interfaces:**
- Consumes: `install.env`, the drop-ins, `MODE`, `PUBLIC_URL`, `ADMIN_PORT`, `BUILD`.
- Produces:
  - `service_env(name, default)` — reads `/proc/<MainPID>/environ` (under `${PROC_ROOT:-/proc}`), then the installer's own environment, then the default.
  - `wait_public_healthy(host, https_port, header_name, header_value)` — sends the header on `curl -K -`'s stdin, never in argv.
  - `have_ss()` — `command -v ss`, a seam tests override.
  - `proxy_effective(admin_port)` — true when no wildcard listener has the admin port and `docker port outhaul-traefik` has no `80/`.
  - `configure_firewall()` — does nothing when `WANT_FW=0`; otherwise computes the ports, checks `ufw_would_block`, and calls `apply_firewall`. In non-interactive mode a would-block port is `die` (exit 1) before any `ufw` or `apt-get` call.
  - `set_update_path(on|off)`; `install_updater` no longer enables the path unit.
  - `derive_firewall_ports` accepts `p` (proxy): 22 and 443, plus a git port when one is on.
  - `check_firewall_early()` — in non-interactive `--firewall ufw`, checks `ufw_would_block` before any install or swap, and `die`s before changing anything.
  - Globals: `HAD_BINARY=0`, `PREV_COMMIT=''` (the running commit, possibly empty).

- [ ] **Step 1: Write the failing `service_env` tests**

In `deploy/test/test_health.sh`, define `T=$(mktemp -d)` and `CALLS=$T/calls` at the top of the new section, then:

```sh
PROC_ROOT=$T/proc
mkdir -p "$PROC_ROOT/123"
printf 'OUTHAUL_HTTPS_PORT=8443\0OUTHAUL_REQUIRE_HEADER_NAME=X-Test\0OUTHAUL_REQUIRE_HEADER_VALUE=s3cret\0' > "$PROC_ROOT/123/environ"
systemctl() { case "$*" in *MainPID*) echo 123 ;; esac; }
assert_eq "service env: the running service wins" 8443 "$(service_env OUTHAUL_HTTPS_PORT 443)"
assert_eq "service env: header name" X-Test "$(service_env OUTHAUL_REQUIRE_HEADER_NAME '')"
assert_eq "service env: default" :8080 "$(service_env OUTHAUL_LISTEN_ADDR :8080)"
systemctl() { echo 0; }
OUTHAUL_HTTPS_PORT=9443
assert_eq "service env: the installer's own environment next" 9443 "$(service_env OUTHAUL_HTTPS_PORT 443)"
unset OUTHAUL_HTTPS_PORT
assert_eq "service env: default last" 443 "$(service_env OUTHAUL_HTTPS_PORT 443)"
```

- [ ] **Step 2: Write the failing header and proxy tests**

```sh
args=$(mktemp); input=$(mktemp)
curl() { printf '%s\n' "$*" > "$args"; cat > "$input"; return 0; }
sleep() { :; }
assert_ok "public check with a header" wait_public_healthy o.example.com 443 X-Test s3cret >/dev/null
assert_contains "header not in argv" "$(cat "$args")" "-K -"
assert_eq "header value not in argv" 0 "$(grep -c s3cret "$args" || true)"
assert_contains "header on stdin" "$(cat "$input")" 'header = "X-Test: s3cret"'
assert_contains "the service's HTTPS port" "$(cat "$args")" "--resolve o.example.com:443:127.0.0.1"

have_ss() { return 0; }
ss() { printf 'LISTEN 0 128 0.0.0.0:8080 0.0.0.0:*\n'; }
docker() { printf '80/tcp -> 0.0.0.0:80\n'; }
assert_fail "proxy: wildcard admin port fails" proxy_effective 8080
ss() { printf 'LISTEN 0 128 127.0.0.1:8080 0.0.0.0:*\nLISTEN 0 128 172.17.0.1:8080 0.0.0.0:*\n'; }
assert_fail "proxy: published 80 fails" proxy_effective 8080
docker() { printf '443/tcp -> 0.0.0.0:443\n'; }
assert_ok "proxy: loopback and bridge only, no 80" proxy_effective 8080
have_ss() { return 1; }
assert_ok "proxy: no ss warns and passes" proxy_effective 8080

# The header is optional.
curl() { printf '%s\n' "$*" > "$args"; return 0; }
assert_ok "public check without a header" wait_public_healthy o.example.com 443 '' '' >/dev/null
assert_eq "no header: no -K" 0 "$(grep -c -- '-K -' "$args" || true)"
```

- [ ] **Step 3: Write the failing firewall and path-unit tests**

In `test_firewall.sh`, define `T=$(mktemp -d)` and `CALLS=$T/calls` at the top of the new section, then:

```sh
assert_eq "proxy opens 22 and 443" "22 443" "$(derive_firewall_ports p '')"
assert_eq "proxy with a git port" "22 443 2222" "$(derive_firewall_ports p 2222)"

# leave calls neither ufw nor apt-get.
ufw() { echo "ufw $*" >> "$CALLS"; }
apt-get() { echo "apt-get $*" >> "$CALLS"; }
: > "$CALLS"
WANT_FW=0 configure_firewall
assert_eq "leave: no firewall calls" "" "$(cat "$CALLS")"

# ufw that would block a port exits 1 before any call.
WANT_FW=1 NONINTERACTIVE=1 MODE=c GITPORT='' LOCAL_CA=0 PORTS="22" ADMIN_PORT=8080 BRIDGE=outhaul
ufw_would_block() { echo 3000; }
quiet() { ( "$@" ) >/dev/null 2>&1; }
assert_fail "ufw would block: exits first" quiet configure_firewall
assert_eq "ufw would block: no changes" "" \
  "$(grep -E '^(ufw allow|ufw --force|apt-get)' "$CALLS" || true)"
unset -f ufw_would_block
```

In `test_update_install.sh`, change the expected `install_updater` calls to end with `systemctl daemon-reload` (no enable), then:

```sh
: > "$CALLS"
set_update_path on
assert_eq "path unit on" "systemctl enable --now outhaul-update.path" "$(cat "$CALLS")"
: > "$CALLS"
set_update_path off
assert_eq "path unit off" "systemctl disable --now outhaul-update.path" "$(cat "$CALLS")"
```

- [ ] **Step 4: Run them to see them fail**

Run:
```sh
dash deploy/test/test_health.sh
dash deploy/test/test_firewall.sh
dash deploy/test/test_update_install.sh
```
Expected: FAIL because the functions and the `p` mode do not exist.

- [ ] **Step 5: Implement `service_env`**

```sh
service_env() { # name default
	_pid=$(systemctl show -p MainPID --value outhaul 2>/dev/null || true)
	if [ -n "$_pid" ] && [ "$_pid" != 0 ] && [ -r "${PROC_ROOT:-/proc}/$_pid/environ" ]; then
		_v=$(tr '\0' '\n' < "${PROC_ROOT:-/proc}/$_pid/environ" | sed -n "s/^$1=//p" | head -n 1)
		if [ -n "$_v" ]; then printf '%s\n' "$_v"; return 0; fi
	fi
	eval "_v=\${$1-}"
	[ -n "$_v" ] || _v=$2
	printf '%s\n' "$_v"
}
```

`PROC_ROOT` is the test seam; it defaults to `/proc`. Never print the whole environment.

- [ ] **Step 6: Implement the health checks**

`wait_public_healthy host https_port header_name header_value`:
- `_port=${2:-443}`.
- Build `_args="-fsSk --max-time 2 --resolve $1:$_port:127.0.0.1 -o /dev/null https://$1:$_port/healthz"`.
- With a header: `printf 'header = "%s: %s"\n' "$3" "$4" | curl $_args -K -`.
- Without: `curl $_args`.
- Keep the 20 one-second tries and the spinner.
- Add `# shellcheck disable=SC2086` for the intentional split of `_args`.

In `main`, replace `OUTHAUL_LISTEN_ADDR` reads with `service_env OUTHAUL_LISTEN_ADDR :8080`; set `ADMIN_PORT`, `HTTPS_PORT`, `HEADER_NAME`, `HEADER_VALUE` from `service_env`. Read `ADMIN_PORT` **after** the service has started: `install_or_upgrade` and the no-build path set it immediately before `new_version_healthy`, and `main` reads it again after the install for the firewall, public and proxy checks. A first install whose `--env-file` sets `OUTHAUL_LISTEN_ADDR` would otherwise be checked on the wrong port. Run the public check for `MODE=a` and `MODE=p`; for `p`, after a successful public check, use `if ! proxy_effective "$ADMIN_PORT"; then ... fi` (not a bare call, which would abort under `set -e`).

A failed `proxy_effective` must not leave the new version running: capture `HAD_BINARY=1` (and `PREV_COMMIT=$(build_field commit)`, which may be empty for an unstamped build) before `install_or_upgrade`. On failure, when `BUILD=1` and `HAD_BINARY=1` (an upgrade this run performed), call `rollback_install "${PREV_COMMIT:-}"`; then exit 1 in non-interactive mode (the wizard sets `HEALTHY=1` and says the previous version is back). A first install has no previous binary: run `systemctl stop outhaul` — the service it just started ignored the switches, so it must not keep serving port 80 and the open admin port — then exit 1, leaving the binary and journal for diagnosis.

`proxy_effective`:
- If `have_ss` (default `command -v ss >/dev/null 2>&1`), read `ss -ltn` and fail on a `LISTEN` line whose local address is `0.0.0.0:<port>`, `*:<port>`, or `[::]:<port>`.
- If `ss` is absent, `note "ss not found; skipping the admin-port check"` and continue.
- Fail when `docker port outhaul-traefik 2>/dev/null | grep -q '^80/'`.
- Otherwise pass.

`configure_firewall`:
```sh
configure_firewall() {
	[ "${WANT_FW:-0}" = 1 ] || return 0
	PORTS=$(derive_firewall_ports "${MODE:-c}" "${GITPORT:-}" "${LOCAL_CA:-0}")
	if [ -n "${SSH_CONNECTION:-}" ]; then
		assh=${SSH_CONNECTION##* }
		case " $PORTS " in *" $assh "*) ;; *) [ -n "$assh" ] && PORTS="$PORTS $assh" ;; esac
	fi
	if [ "$NONINTERACTIVE" = 1 ]; then
		note "firewall will open: $PORTS"
	else
		printf '  Firewall will open (SSH always preserved): %s\n' "$PORTS"
	fi
	fw_default=y
	if ! ufw_active; then
		blocked=$(ufw_would_block "$(listening_ports)" "$PORTS" "$ADMIN_PORT")
		if [ -n "$blocked" ]; then
			step_fail "ufw is inactive; enabling it would also block ports listening now: $blocked"
			note "containers with published ports are not affected; open others with: ufw allow <port>"
			if [ "$NONINTERACTIVE" = 1 ]; then die "refusing to enable ufw while it would block: $blocked"; fi
			fw_default=n
		fi
	fi
	if [ "$NONINTERACTIVE" = 1 ]; then
		apply_firewall "$PORTS" "$BRIDGE" "$ADMIN_PORT"
	elif ask_yes_no "  Proceed?" "$fw_default"; then
		apply_firewall "$PORTS" "$BRIDGE" "$ADMIN_PORT"
	else
		PORTS=''
	fi
}
```
`BRIDGE=$(network_bridge outhaul)` is set before the call; `ADMIN_PORT` is set from `service_env`. The wizard's "Firewall will open …" line is preserved before the prompt.

`check_firewall_early()` is called after `apply_answers` and before `ensure_docker`/`ensure_git`/the build, so a non-interactive `--firewall ufw` run that would cut off a listening port exits 1 **before changing anything**:

```sh
check_firewall_early() {
	[ "$NONINTERACTIVE" = 1 ] && [ "${WANT_FW:-0}" = 1 ] || return 0
	_ports=$(derive_firewall_ports "${MODE:-c}" "${GITPORT:-}" "${LOCAL_CA:-0}")
	_admin=$(admin_port "$(service_env OUTHAUL_LISTEN_ADDR :8080)")
	if ! ufw_active; then
		_blocked=$(ufw_would_block "$(listening_ports)" "$_ports" "$_admin")
		[ -n "$_blocked" ] && die "refusing to enable ufw while it would block: $_blocked"
	fi
}
```

The later `configure_firewall` still checks the new service's ports; the early check is the one the design's "before changing anything" asks for. The wizard still asks at the firewall step.

`completion_screen`: add `p)` to the `case`, shown like `a)`; the setup tunnel hint applies to `a` and `p`.

- [ ] **Step 7: Implement the path unit split**

Remove `systemctl enable --now outhaul-update.path` from `install_updater`. Add:

```sh
set_update_path() { # on|off
	if [ "$1" = off ]; then
		systemctl disable --now outhaul-update.path >/dev/null 2>&1 || true
	else
		systemctl enable --now outhaul-update.path >/dev/null 2>&1 || true
	fi
}
```

`setup_updater(src_dir)` calls `install_updater "$src_dir"` only when `src_dir` is non-empty (a no-build run has no tree, and the marker says the updater's files already came from that commit), then lower-cases `service_env OUTHAUL_UPDATES main` and compares it with `off`:

```sh
setup_updater() { # src_dir, empty on a no-build run
	if [ -n "${1:-}" ]; then
		if ! install_updater "$1"; then
			step_fail "could not install the updater"
			note "updating from the UI stays unavailable; re-run the installer to try again"
			return
		fi
	fi
	_upd=$(service_env OUTHAUL_UPDATES main | tr '[:upper:]' '[:lower:]')
	if [ "$_upd" = off ]; then set_update_path off; else set_update_path on; fi
}
```

This reads the service's own environment, so `OUTHAUL_UPDATES=off` in `/etc/outhaul.env` works as well as `--updates off`, in any case. The path unit is set on every run, including a no-build one.

- [ ] **Step 8: Run the tests**

Run:
```sh
dash deploy/test/test_health.sh
dash deploy/test/test_firewall.sh
dash deploy/test/test_update_install.sh
bash --posix deploy/test/test_health.sh
busybox sh deploy/test/test_health.sh
```
Expected: PASS.

- [ ] **Step 9: Commit**

```bash
git add deploy/bootstrap.sh deploy/test/test_health.sh deploy/test/test_firewall.sh deploy/test/test_update_install.sh
git commit -m "feat(installer): check the service's own ports and the proxy's effect" \
  -m "Health checks read the installer's environment, so an env-file port was missed, and the public check could not pass a required header. The installer now reads /proc/<MainPID>/environ, sends the header through curl's stdin, checks proxy for a wildcard admin port and a published 80, opens 22 and 443, and enables the update path unit only when the service says updates are on."
```

---

### Task 9: The log, the exit codes, the setup file, and the whole unattended run

Finish the unattended path and prove it end to end.

**Files:**
- Modify: `deploy/bootstrap.sh` (`init_log`, `log_event`, `step_ok`/`step_fail`/`note`/`die`, `on_exit`, `write_setup_file`, `main`, `completion_screen`, `preflight`, `acme_preflight`, `maybe_add_swap`)
- Test: `deploy/test/test_noninteractive.sh` (new); extend `deploy/test/test_main.sh`, `test_health.sh`, `test_preflight.sh`

**Interfaces:**
- Consumes: everything from Tasks 1–8.
- Produces:
  - `NONINTERACTIVE=1` appends to `/var/log/outhaul-install.log` mode `0600`, with a first line `YYYY-MM-DDTHH:MM:SSZ <arguments>`, `step_ok`/`step_fail`/`note`/`die` lines, and a last `OK …` or `FAILED …` line.
  - `--setup-url-file PATH` writes the `open:` URL by rename, root:root `0600`; removes it when a start this run printed no URL.
  - `preflight` exits 1 on an unsupported distro; `acme_preflight` warns and continues; `maybe_add_swap` says yes; the Go toolchain is removed.

- [ ] **Step 1: Write the failing end-to-end test**

`deploy/test/test_noninteractive.sh` follows `test_update.sh`'s shape: source `bootstrap.sh`, point every path into a temp tree, stub the outside world, run `main` in a subshell under `set -e`, and record calls.

Stubs and seams:
- Paths: `LOGFILE=$T/log`, `INSTALL_ENV=$T/etc/install.env`, `ENV_FILE=$T/etc/outhaul.env`, `SERVICE_DROPIN=$T/units/outhaul.service.d/10-install.conf`, `UPDATE_DROPIN=$T/units/outhaul-update.service.d/10-install.conf`, `BIN=$T/bin/outhaul`, `UNIT_DIR=$T/units`, `DATA_DIR=$T/data`, `UPDATE_DIR=$T/data/update`, `WORK_DIR=$T/work`, `MARKER=$T/work/installed`, `SRC_DIR=$T/src`, `PROC_ROOT=$T/proc`, `LIB_DIR=$T/lib`.
- `systemctl`: records calls; `is-active` reads `$T/up`; `show -p MainPID --value` prints `$$`; `show -p InvocationID --value` prints `inv1`; `show -p ActiveEnterTimestamp` prints a fixed value; `start`/`restart`/`enable --now outhaul` sets `$T/up`; `stop` removes it. Create `$PROC_ROOT/$$/environ` with `OUTHAUL_LISTEN_ADDR=:8080`, `OUTHAUL_HTTPS_PORT=443` and `OUTHAUL_UPDATES=main`, and update it when a scenario changes one of those.
- `docker`: `pull` succeeds; `port outhaul-traefik` prints `443/tcp -> 0.0.0.0:443`; `version`/`compose version` succeed.
- `curl`: for `/healthz`, prints `HTTP/1.1 200 OK\r\nX-Outhaul-Commit: <commit>\r\n\r\n` when `$T/up` exists and the commit is in `HEALTHY`; for the public check, requires the header on stdin when `HEADER` is set.
- `journalctl`: prints `open: https://o.example.com/setup?token=abc` for the current invocation, or nothing when `NO_SETUP=1`.
- `git`: for `--ref` a 40-character commit, no network; `rev-parse`/`status` as `build_binary` needs.
- `go`: writes a fake `outhaul` that prints `commit=$TARGET`, `repo=...`, `local=false`, `version=$TARGET_VERSION`, and `images` prints `traefik traefik:v1` and `cloudflared cloudflare/cloudflared:1`.
- `fetch_source`: creates `$SRC_DIR` with `go.mod`, `deploy/outhaul.service`, `deploy/update.sh`, `deploy/bootstrap.sh`, and the two updater units; records `TARGET`.
- `preflight`, `ensure_docker`, `ensure_git`, `ensure_nixpacks`, `maybe_add_swap`, `install_go_toolchain`, `migrate_slipway`, `ensure_service_user`, `setup_updater`, `network_bridge`: record calls (the last echoes `outhaul`). `preflight` is stubbed because the test runs as a normal user; `test_preflight.sh` owns its checks. `setup_updater` is stubbed because `test_update_install.sh` owns the path-unit tests.
- `ask_yes_no`/`ask_value`: `fail` the test if called (`echo PROMPT >> "$T/prompts"`), and `read`: `echo READ >> "$T/reads"`.

Scenarios:
1. **First install.** `main --non-interactive --ingress local --git-ssh off --nixpacks on --firewall leave --updates off`. Exit 0; `$T/up` exists; binary installed; marker `commit=$TARGET`; `install.env` has `OUTHAUL_SSH_ADDR=off` and `OUTHAUL_UPDATES=off`; the drop-in has `install.env`; **`$ENV_FILE` does not exist**; the log ends `OK`; no setup URL in stdout; `$T/prompts` and `$T/reads` are empty; no `ufw`, no `get.docker.com`.
2. **Same commit again.** `main` with the same flags; record the binary inode and `systemctl show -p ActiveEnterTimestamp`; assert no `git fetch`, no `go`, no `systemctl restart/start`, and the same inode. `install.env` keeps its inode. Edit the *contents* of an existing `--env-file` between the runs; the run still does not restart: the installer does not read it.
3. **A changed flag.** `--updates main`; one `systemctl restart outhaul`; `install.env` now has `OUTHAUL_UPDATES=main`.
4. **A changed `--env-file` on the same commit.** Add a second env file to the command; assert exactly one `systemctl daemon-reload` and one `systemctl restart outhaul`, and that the service's drop-in (and `systemctl show -p EnvironmentFiles`) now includes it. A drop-in change without the reload must fail this scenario.
5. **A new ref.** Set `TARGET` to a second commit; `main`; the binary is the new commit, `$WORK_DIR/previous/outhaul` is the old one, exit 0.
6. **A new version that never answers.** `HEALTHY` excludes the new commit; `main` exits 1; the old binary is back; `install.env` is the old one; the log says rolled back.
7. **A first install that never answers.** No binary; `HEALTHY=''`; exit 1; the log has the journal's last lines; no marker. Make the journal stub print both the `open:` line and the `or, over an SSH tunnel` line with the token; assert the URL and the token appear in neither the log nor stdout.
8. **The setup file.** With `--setup-url-file $T/setup`, the file is `0600` and contains the `open:` URL. With `NO_SETUP=1`, a run that starts the service removes it; a run that does not start the service leaves it.
9. **Exit codes.** `--ingress` missing exits 2 and leaves the log and the service untouched; a failed build exits 1.
10. **A blocked firewall is refused before anything changes.** `--firewall ufw`, with `ufw_would_block` stubbed to report a listening port; exit 1, no binary, no service start, no `install.env`, and the log says it refused. This is the early check, not the post-upgrade one.

- [ ] **Step 2: Run it to see it fail**

Run: `dash deploy/test/test_noninteractive.sh`
Expected: FAIL: the non-interactive path still prompts, writes no `install.env`, and exits 0 when the service never answers.

- [ ] **Step 3: Implement the log and exit handling**

- `init_log "$@"`: non-interactive creates the log mode `0600` (or `LOGFILE=/dev/null` when it cannot; the run must not fail because the log is unwritable), appends `date -u '+%Y-%m-%dT%H:%M:%SZ'` and the arguments; interactive keeps truncating. Skip `chmod` when `LOGFILE=/dev/null`, so the device is not changed.
- `log_event`: appends only when `NONINTERACTIVE=1`, and ignores a failed append (`2>/dev/null || true`); a full disk must not stop the install. Call it from `step_ok` (`step_ok <text>`), `step_fail`, `note`, and `die` (`die <text>`). Replace `die`'s `log_line "ERROR: $1"` with `log_event "die $1"`; the stderr message stays.
- `on_exit`: capture `$?`. Before logging the result, call `swap_guard` (defined and tested in Task 6); it rolls back only when `STAGE=swap`, and sets `ROLLBACK_DONE` itself so the trap cannot re-enter. Then append `OK installed` or `FAILED exit <n>`, run `cleanup`, exit with the same code. Install `trap on_exit EXIT`, `trap 'exit 130' INT` and `trap 'exit 143' TERM` after `init_log`, so an interrupted run logs `FAILED`, not `OK`.
- `init_ui`: with `NONINTERACTIVE=1`, force `COLOR=0` and `UNICODE=0`, so stdout has no colour.
- `--verbose` keeps `LOGFILE=/dev/stderr` only in the wizard; the unattended log stays the file.

- [ ] **Step 4: Implement the non-interactive prompts and setup file**

- `preflight`: unsupported distro → `die` in non-interactive, prompt in the wizard.
- `acme_preflight`: only `MODE=a` calls it. A non-interactive mismatch is `step_fail` + `note`, then the run continues; the wizard still asks.
- `maybe_add_swap`: non-interactive takes yes without calling `ask_yes_no`.
- Remove the Go toolchain without asking in non-interactive.
- `write_setup_file(path, url)`: temp in the same directory, `printf '%s\n'`, `chmod 0600`, `mv -fT`; a failure is `die "could not write <path>"`.
- In `main`, extract `SETUP` as today. If `SETUP_URL_FILE` is non-empty: write it when `SETUP` is non-empty; else if `STARTED_SERVICE=1`, remove it; else leave it. Pass `SETUP=''` to `completion_screen` when non-interactive, and note `journalctl -u outhaul | grep -i setup` (or the file path).
- `completion_screen`: `p)` shown like `a)`; no setup URL in unattended output.

- [ ] **Step 5: Finish `main`'s non-interactive flow**

Ensure the order is: flags → log/trap → `init_ui`/`hero` → `preflight` → `apply_answers` → `acme_preflight` → `check_firewall_early` → `ensure_docker`/`ensure_git`/nixpacks → `resolve_ref`/`needs_build` → build or no-build path → `migrate_slipway`/`ensure_service_user` → write `$ENV_FILE` (wizard only) → capture `HAD_BINARY`/`PREV_COMMIT` → `install_or_upgrade` or the no-build reconcile → `setup_updater "${SRC_DIR:-}"` (only when the service answered; `SRC_DIR` is empty on a no-build run, so only the path unit is set) → remove Go → `configure_firewall` → health → setup file → `write_marker` → `completion_screen`. `write_marker` runs only when the service answered (`HEALTHY=0`) and nothing rolled back. A failed install or a rollback leaves the old updater files alone; `setup_updater` is skipped. `configure_firewall` handles both `leave` and `ufw`; with `ufw` it exits 1 before any `ufw` or `apt-get` call when a port would be blocked.

In non-interactive mode, `export DEBIAN_FRONTEND=noninteractive` before `ensure_git`/`ensure_docker`/`apply_firewall`, so apt's own questions never appear. `apply_answers` is called before any prompt would be reached; the wizard's `choose_ingress` path is untouched. After `apply_answers` and `PUBLIC_URL=$(normalize_public_url "$PUBLIC_URL")`, call `acme_preflight "$(url_host "$PUBLIC_URL")"` only when `MODE=a`; `MODE=p` skips it because DNS points at the proxy.

Add `redact_setup_url`: it replaces **any line containing `/setup?token=`** with `<setup URL withheld>`, not only `open:` lines. `printSetupHint` prints a second line (`or, over an SSH tunnel to port 8080: …/setup?token=…`) whenever a public URL is set, and that line carries the same token. Wrap every `journalctl -u outhaul -n 20 --no-pager` in `install_or_upgrade` and `rollback_install` with it. In non-interactive mode, append the redacted tail to `$LOGFILE` as well as printing it; the setup URL and its token must not reach the log or the output.

A failed first install or a rolled-back upgrade:
- `install_or_upgrade` returns 1;
- `main` sets `HEALTHY=1` and continues to `completion_screen` in the wizard (which exits 0, as the spec's call 14 requires);
- in non-interactive mode, `main` exits 1, with the journal's last 20 lines already printed by `install_or_upgrade`/`rollback_install`.

The public check follows the same rule: `PUBLIC=1` sets `HEALTHY=1`; non-interactive prints the journal's last 20 lines and exits 1, while the wizard shows its screen. The `proxy` effect check is stricter: a failure after an upgrade rolls back (Task 8), then follows the same exit rule.

`completion_screen` gets a note when `ROLLED_BACK=1`: "The new version did not answer; the previous version is running again." It still prints the setup hint only in the wizard.

- [ ] **Step 6: Run the test**

Run:
```sh
dash deploy/test/test_noninteractive.sh
bash --posix deploy/test/test_noninteractive.sh
busybox sh deploy/test/test_noninteractive.sh
```
Expected: PASS.

- [ ] **Step 7: Extend the smaller tests**

- `test_main.sh`: `parse_args --help` exits 0 and mentions `--non-interactive`; bad flags exit 2 with no log.
- `test_preflight.sh`: `preflight` with `NONINTERACTIVE=1` and an unsupported distro exits 1; with the wizard it prompts. `acme_preflight` with `NONINTERACTIVE=1` and a mismatched IP returns 0 without calling `ask_yes_no`; with `MODE=p` it is not called at all.
- `test_noninteractive.sh`: stub `apt-get` to write `$DEBIAN_FRONTEND`, and assert it is `noninteractive`.
- The `--setup-url-file` writing/removal cases stay in `test_noninteractive.sh` scenario 8; `test_health.sh` keeps only the header, HTTPS-port and `proxy` checks.

- [ ] **Step 8: Run the whole shell suite**

Run:
```sh
sh deploy/test/run.sh
```
Expected: all files pass.

- [ ] **Step 9: Commit**

```bash
git add deploy/bootstrap.sh deploy/test/test_noninteractive.sh deploy/test/test_main.sh \
  deploy/test/test_preflight.sh deploy/test/test_health.sh
git commit -m "feat(installer): unattended runs log, exit honestly and write the setup URL" \
  -m "An unattended run emptied the log, printed the setup URL to the serial console, exited 0 when Outhaul never answered, and asked about swap, Go and the firewall. It now appends a 0600 log with step and result lines, exits 1 on a failed check, takes the safe defaults, and writes or removes the optional setup file."
```

---

### Task 10: `integration.sh` on a real systemd Debian container

Prove the installer, upgrade and rollback against a real systemd, Docker and service.

**Files:**
- Modify: `deploy/test/Dockerfile.debian`
- Modify: `deploy/test/integration.sh`
- Modify: `AGENTS.md` (the command)

**Interfaces:**
- Consumes: the installer's flags from Task 7 and the swap from Task 6.
- Produces: an out-of-CI script that runs as `outhaul-installtest-*`, mounts the checkout read-only at `/src`, and checks the spec's list.

- [ ] **Step 1: Rewrite the Dockerfile**

`deploy/test/Dockerfile.debian`:
- `FROM debian:13`
- Install `systemd systemd-sysv curl ca-certificates git iproute2` and Docker CE from Docker's apt repository (keyring and `docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin`), then remove the lists. `iproute2` provides `ss` for the proxy/admin-port checks.
- `STOPSIGNAL SIGRTMIN+3`, `CMD ["/sbin/init"]`.
- Do not `COPY . /src`; the script mounts the checkout. If the R1 to R4 plan's `Dockerfile.proxytest` exists when this task runs, make this file use the same image instead of a second one (whichever lands second uses the first).

- [ ] **Step 2: Rewrite `integration.sh`**

Shape:

```sh
#!/usr/bin/env sh
set -eu
root=$(cd "$(dirname "$0")/../.." && pwd)
img=outhaul-installtest
name=outhaul-installtest-$$
# A linked worktree's .git is a file pointing at the host's main repository;
# that path does not exist in the container. Clone to a normal repository and
# mount the clone instead.
tmp=$(mktemp -d)
git clone --no-hardlinks "$root" "$tmp/repo"
ref=$(git -C "$tmp/repo" rev-parse HEAD)
docker build -f "$root/deploy/test/Dockerfile.debian" -t "$img" "$root"
cid=''
trap 'if [ -n "$cid" ]; then docker rm -f "$cid" >/dev/null 2>&1 || true; fi; rm -rf "$tmp"' EXIT
cid=$(docker run -d --name "$name" --privileged --tmpfs /run --tmpfs /run/lock \
  -v /sys/fs/cgroup:/sys/fs/cgroup:rw -v "$tmp/repo:/src:ro" "$img")
sleep 5
# git refuses a repository owned by another user; the mount is the host's.
docker exec "$cid" git config --global --add safe.directory /src
docker exec "$cid" mkdir -p /etc/outhaul
run_install_repo() { # repo, then the installer's arguments
	_repo=$1; shift
	docker exec -i "$cid" env OUTHAUL_REPO="$_repo" sh /src/deploy/bootstrap.sh "$@" </dev/null
}
run_install() { run_install_repo file:///src "$@"; }
```

First install:
```sh
docker exec "$cid" sh -c 'printf "OUTHAUL_IMAGE_KEEP=2\n" > /etc/outhaul/test.env; chown root:root /etc/outhaul/test.env; chmod 0640 /etc/outhaul/test.env'
run_install --non-interactive --ref "$ref" --ingress local --git-ssh off --nixpacks on --firewall leave \
  --env-file /etc/outhaul/test.env --setup-url-file /root/setup
```
Check: exit 0; `systemctl is-active outhaul`; `outhaul version` reports `$ref`; `systemctl show outhaul -p EnvironmentFiles` includes `/etc/outhaul/install.env` and `/etc/outhaul/test.env`; `test.env` is still `root:root 0640`; no `ufw`; no `get.docker.com` in the log; `ss -ltn` has nothing on 2222; `/root/setup` is `0600`; nixpacks is `1.41.0`; the log ends `OK`.

Second run: record `systemctl show -p ActiveEnterTimestamp` and the binary inode; run the same command, including `--env-file /etc/outhaul/test.env`; exit 0; the timestamp and inode are unchanged; no new `git fetch`/`go` in the log.

Upgrade: copy `/src` to `/srv/repo` inside the container (`cp -a /src /srv/repo`), make a commit there (`git -C /srv/repo -c user.email=t@example.com -c user.name=t commit --allow-empty -qm second`), run `run_install_repo file:///srv/repo --non-interactive --ref <new> ...`; exit 0; the binary is the new commit; `/var/lib/outhaul-updater/previous/outhaul` is the old one.

Failed start: make a third commit in `/srv/repo` whose `main.go`'s `serve()` returns an error immediately (a `sed` insertion is enough), commit, run `run_install_repo file:///srv/repo --ref <failed> ...`; exit 1; the binary is the second commit; the log says rolled back; the service is active at the second commit.

`--ingress proxy` is not run: it needs a real ACME server.

- [ ] **Step 3: Run it**

Run: `sh deploy/test/integration.sh`
Expected: `INTEGRATION PASS`, and `docker ps -a` has no `outhaul-installtest-*` container afterwards.

- [ ] **Step 4: Commit**

```bash
git add deploy/test/Dockerfile.debian deploy/test/integration.sh AGENTS.md
git commit -m "test(installer): run the unattended install in a systemd container" \
  -m "integration.sh was out of date and did not exercise the new path. It now installs from a local repository at an exact commit, checks the env file, SSH off, the setup file and the pinned nixpacks, re-runs without a rebuild, upgrades to a second commit, and rolls back a third that fails at start."
```

---

### Task 11: Docs and the decisions to keep

Write the operator-facing document and update the places that say updates always follow main.

**Files:**
- Modify: `README.md` (Install, Updating, Configuration table)
- Create: `docs/UNATTENDED-INSTALL.md`
- Modify: `docs/MANUAL-DEPLOY.md`
- Modify: `ARCHITECTURE.md`
- Modify: `AGENTS.md`
- Verify: `deploy/outhaul-update.path`, `deploy/outhaul-update.service` (Task 2 finishes their comments and `Description=`; this task checks them against the docs)

**Interfaces:**
- Consumes: the flag names, file paths, exit codes and exact values from Tasks 1–10.
- Produces: documentation that matches the code, with no vendor names.

- [ ] **Step 1: Write `docs/UNATTENDED-INSTALL.md`**

Cover, in the plain style of `docs/SSO.md`:
- the flags table, including which need `--non-interactive`;
- `/etc/outhaul.env` versus `/etc/outhaul/install.env` and each `--env-file`, and their order;
- what a re-run does (same ref, changed flag, new ref, no flags);
- upgrading and what a rollback leaves;
- exit codes and the log;
- the setup URL and `--setup-url-file`, including that a later restart makes the file stale;
- the cloud-init example from the spec;
- that a tag can move, that the log names the commit it resolved to, and that a full commit is the recommended pin for production;
- the host checks from "What only the real host can show";
- a link to `docs/BEHIND-A-PROXY.md` for the proxy settings; it is created by the R1 to R4 plan.

- [ ] **Step 2: Update the other docs**

- `README.md`: a short "Unattended" part, a sentence in "Updating" that `OUTHAUL_UPDATES=off` stops the panel, the check and the updater, and rows for `OUTHAUL_UPDATES` and `OUTHAUL_SSH_ADDR` in the configuration table.
- `docs/MANUAL-DEPLOY.md`: nixpacks by pinned download, the installer drop-in, `OUTHAUL_SSH_ADDR=off`, and never install an older version over a newer database.
- `ARCHITECTURE.md`: the two update modes, `main` and `off`; `releases` is not built.
- `AGENTS.md`: the new test files and `integration.sh`; how to move the nixpacks pin (the three constants and the two digests); under "Decisions to keep", a non-interactive run never prompts and never touches Docker or ufw unasked, the request carries nothing in every mode, and `off` is enforced by the updater as well as the UI.

- [ ] **Step 3: Check the docs against the code**

Run:

```sh
for v in OUTHAUL_UPDATES OUTHAUL_SSH_ADDR; do
  grep -q "$v" internal/config/config.go && grep -q "$v" README.md || echo "MISSING $v"
done
grep -noE '[a-z0-9.-]+\.(com|org|net|io|dev|cloud)\b' docs/UNATTENDED-INSTALL.md | grep -v 'example\.\(com\|org\)' || true
grep -q '1.41.0' docs/UNATTENDED-INSTALL.md && grep -q '0f55de78' docs/UNATTENDED-INSTALL.md || echo "MISSING the pin"
```

Expected: no `MISSING` lines and no vendor domain.

- [ ] **Step 4: Run the full verification**

Run:

```sh
go build ./...
go vet ./...
go test ./...
sh deploy/test/run.sh
shellcheck -s sh --severity=warning deploy/bootstrap.sh deploy/update.sh deploy/test/*.sh
```

Then, before calling the branch done:

```sh
go test -race -timeout 30m ./...
```

Expected: all pass.

- [ ] **Step 5: Commit**

```bash
git add README.md docs/UNATTENDED-INSTALL.md docs/MANUAL-DEPLOY.md ARCHITECTURE.md AGENTS.md \
  deploy/outhaul-update.path deploy/outhaul-update.service
git commit -m "docs: unattended installs, pinned versions and updates off" \
  -m "The README still said updates always follow main, and there was no page for the flags or the env files. This adds the unattended guide, the two settings, the nixpacks pin and the decisions to keep, and points the updater's unit comments at the mode it now honors."
```

---

## Self-review

- **Spec coverage:** every goal in the spec maps to a task: `--non-interactive` and its flags (7), `--ref` (5), `--env-file` and the drop-ins (7), Docker/ufw (3, 8), nixpacks (4), `OUTHAUL_UPDATES` (2, 8), failure/log/setup URL (9), `OUTHAUL_SSH_ADDR=off` (1), the careful swap (6), integration (10), docs (11). The "Later" sections are explicitly not built.
- **Swap coverage:** Task 6 has its own `test_swap.sh`, including a `die` in `write_dropins` after the stop; the exit guard is implemented and tested there, and Task 9 only calls it from `on_exit`.
- **Step scan:** each step names one test, one function or one command. The two places that could be read as "write the body" are `resolve_ref` and `install_or_upgrade`; both have the precedence/rollback algorithm spelled out because the signature and tests do not determine it.
- **Host-safety calls:** the swap branches on the binary, not the stamp; a drop-in change gets `daemon-reload`; `EnvironmentFile=` paths are bare and `check_args` refuses newline/quote/`%`; the setup redaction matches any line with `/setup?token=`; a first-install proxy failure stops the service; a non-interactive run never writes `$ENV_FILE`; the ufw would-block check runs before the install; `STAGE=swap` is set before the stop and `SNAPSHOT_OK` guards the restore.
- **Type consistency:** `Build.Version`, `Config.Updates`, `Config.SSHOff`, `updatesPanel.Off`, `git_ssh_from_port`, `service_env`, `proxy_effective`, `configure_firewall`, `set_update_path`, `write_if_changed`, `install_or_upgrade`, `rollback_install`, `resolve_ref`/`TARGET`/`TARGET_VERSION`/`TARGET_REF`, and `STAGE`/`ROLLBACK_TO`/`ROLLBACK_DONE` are used with the same names in every task.
- **Review Focus:** the five lines have tests in Tasks 6, 7, 8 and 9.
- **Proportion:** the plan names decisions, exact values and test assertions; it does not transcribe the shell bodies except where the algorithm is the decision.
