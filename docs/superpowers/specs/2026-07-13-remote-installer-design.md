# Outhaul Remote Installer — Design

**Date:** 2026-07-13
**Status:** Approved (brainstorming) → ready for implementation planning
**Scope:** A single-command remote installer for a fresh Debian VPS, invoked as
`curl -fsSL https://outhaul.sh/install | sh`.

## Goals

1. A visually stunning, "maximalist" terminal install experience — gradient
   hero banner, live spinners and progress, colored step ledger — that
   degrades gracefully on limited terminals.
2. Works on the MVP target: latest Debian (12/13). Other distros are detected
   and warned about, not supported in V1.
3. Installs dependencies, prompting the user for the genuinely-optional ones.
4. Configures the firewall (open the ports the chosen ingress needs) without
   ever locking the user out of SSH.
5. An interactive wizard that leaves the box actually working on first boot —
   HTTPS live where possible, service running, setup URL surfaced.

## Non-goals (YAGNI for V1)

- Prebuilt-binary releases / GoReleaser pipeline (the installer builds from
  source).
- Non-Debian package managers — detect and warn only.
- Multi-arch release matrix.
- Unattended / fully env-driven mode (no-prompt install).
- An uninstall script.
- Seeding the Cloudflare Tunnel connector token (architecturally not possible
  pre-boot; see §8).

## Delivery model

- **One self-contained script**, `deploy/bootstrap.sh`, served verbatim at
  `https://outhaul.sh/install`. Single versioned source of truth.
- **POSIX sh** (must run under Debian's `dash`) — no bashisms, no re-exec into
  bash.
- **Replaces the current `deploy/install.sh`.** The existing script assumes it
  is run from a local checkout, which does not fit `curl | sh`. The new script
  supports a `--from-checkout` flag so developers can still run it against a
  local clone without re-downloading/cloning.
- **Builds from source:** because there is no binary release channel, the
  script clones the repo to obtain source and runs `go build`.
- **`curl | sh` stdin constraint:** the script's stdin is the script text, so
  all interactive prompts read from `/dev/tty`. If `/dev/tty` is unavailable
  (non-interactive/piped with no terminal), the script does not hang — it uses
  safe defaults and prints what it selected, or aborts with a clear message for
  steps that require an answer.

## Requirements

The installer needs root once (packages, service user, systemd unit, firewall).
Outhaul itself runs as the unprivileged `outhaul` system user (member of the
`docker` group) — unchanged from today.

## 1. Visual system (maximalist, graceful degradation)

Capability detection runs first and gates every visual decision:

- **Color:** truecolor (`COLORTERM=truecolor|24bit`) → 256-color → 16-color →
  plain (`NO_COLOR` set, or not a TTY).
- **Unicode:** braille/box-drawing when the locale/terminal supports it, ASCII
  fallback otherwise.
- **Width:** reflow or hide the hero banner below ~60 columns.
- **TTY:** when stdout is not a TTY (piped to a file / CI), collapse to plain
  prefixed log lines (`==> building…`) — never emit escape sequences into a
  pipe.

Elements:

- **Hero:** gradient ASCII `OUTHAUL` wordmark + tagline on entry.
- **Spinners:** braille spinner (`⠋⠙⠹⠸…`) driven by a backgrounded PID during
  long-running steps; cleaned up by a trap.
- **Build progress:** gradient progress bar; dimmed sub-log line showing current
  activity (e.g. the Go package being compiled).
- **Step ledger:** colored `✓` / `✗` per completed step.
- **Success finish:** boxed summary card plus a subtle confetti/sparkle
  flourish — the payoff for going maximalist.

## 2. Wizard flow

All prompts read from `/dev/tty`.

1. **Hero banner** + one line: what this does, that it needs root.
2. **Preflight**
   - root check; `systemctl` present (systemd required).
   - distro detect: assert Debian; on others, warn and offer to continue.
   - arch detect.
   - disk / memory sanity check (feeds the swap guard, §5).
3. **Ingress mode** — the central branch:
   - **(a) Public domain + Let's Encrypt** — prompt public URL + ACME email;
     plan to open 80/443. Runs DNS + port preflight (§4).
   - **(b) Cloudflare Tunnel** — no domain/ACME prompts, no public ports opened;
     flagged for the post-install token step (§8).
   - **(c) Local / decide later** — admin UI on `:8080` only; nothing else
     opened.
4. **Git-push port** — prompt; default `2222`; or "disabled." Sets
   `OUTHAUL_SSH_ADDR`.
5. **Optional dependencies** — each a `/dev/tty` yes/no:
   - Nixpacks (only needed for auto-detected/Nixpacks builds).
   - Firewall step (§6).
   - (asked after build) remove the Go toolchain to reclaim ~150 MB.
6. **Firewall confirmation** (if opted in) — show the exact port set before
   acting (§6).

## 3. Dependencies

- **Docker + compose plugin** (required): install via `get.docker.com`; verify
  `docker compose version` afterward.
- **git** (required): apt.
- **Go toolchain** (required, build-time): **download the exact version pinned
  in `go.mod` (currently `go1.26.4`) from `https://go.dev/dl/`** — NOT the apt
  package (Debian ships a far older Go). Verify the SHA256, use from a temp
  directory, and offer removal at the end (step 5c). The pinned version must be
  read from `go.mod`, not hardcoded, so it tracks the repo.
- **Nixpacks** (optional): `nixpacks.com/install.sh`.

## 4. DNS + port preflight (ingress mode a only)

Before committing to Let's Encrypt:

- Detect this box's public IP.
- Resolve the entered domain and compare to the detected public IP; if it does
  not point here, warn loudly (this is the most common ACME failure) and let the
  user proceed anyway or fix DNS first. Print the A record they should create.
- Check that ports 80 and 443 are not already bound by another service.

## 5. Low-RAM swap guard

Building Go 1.26 from source can OOM-kill on a 1 GB VPS (a very common cheap
tier). If detected RAM is below ~2 GB and swap is insufficient:

- Offer to create a temporary swapfile sized for the build.
- Build with the swapfile active.
- Remove the temporary swapfile afterward (leave any pre-existing swap intact).

## 6. Firewall

- Uses `ufw` (install if absent, when the firewall step is opted in).
- The port set is derived from the ingress mode plus the git-push port:
  - SSH (22) is **always** allowed.
  - Mode a: add 80 and 443.
  - Git-push port (if enabled): added.
- **Show the exact set and confirm before acting.**
- **Order matters:** run `ufw allow` for every port (especially SSH) *before*
  `ufw enable`, to guarantee the running SSH session is not dropped.
- Print a hint that additional ports can be opened later with `ufw allow <port>`.

## 7. Build, user, service, config

1. Clone the repo to a working directory (spinner).
2. `go build -o outhaul .` (spinner + gradient progress + dimmed sub-log).
3. Carry over the existing slipway→outhaul data-dir/binary migration logic.
4. Create the `outhaul` system user (home `/var/lib/outhaul`, nologin shell);
   add to the `docker` group.
5. Install the binary to `/usr/local/bin/outhaul` via `install(1)` (unlinks
   first to avoid `ETXTBSY` on upgrade).
6. Write `/etc/outhaul.env` from the wizard answers, `0600`, comments on their
   own lines (per the existing systemd inline-comment caveat):
   - Mode a: `OUTHAUL_PUBLIC_URL`, `OUTHAUL_ACME_EMAIL`, `OUTHAUL_SSH_ADDR`.
   - Mode b / c: minimal; HTTPS left off.
   - Never overwrite an existing `/etc/outhaul.env`.
7. Install the systemd unit, `daemon-reload`, then restart-or-start (enable +
   start if inactive, restart if already active).

## 8. Cloudflare Tunnel (ingress mode b)

The connector token is stored **sealed in the SQLite DB** and is only settable
through the authenticated Settings page (`SetCloudflareToken`); there is **no
env var** for it. The installer therefore cannot enable the tunnel before first
boot. Instead:

- Mode b skips the domain/ACME prompts and opens no public ports.
- The completion screen tells the user to log in and paste their Cloudflare
  connector token in **Settings → Tunnel**.

## 9. Post-install health check

After starting the service, actually probe the local admin endpoint and confirm
it returns HTTP 200 — do not trust `systemctl start` alone. On success, show a
green "reachable." On failure, show a real error plus the tail of
`journalctl -u outhaul`.

## 10. Completion screen

Boxed summary card:

- Admin/app URLs, the ports that were opened, and service status.
- **The one-time setup URL surfaced automatically** — poll
  `journalctl -u outhaul` for the setup line so the user does not have to grep
  for it.
- Mode b: the "paste your Cloudflare token in Settings → Tunnel" call-to-action.

## 11. Observability & safety

- Mirror all raw output to `/var/log/outhaul-install.log`; a `--verbose` flag
  streams the raw detail to the terminal too. The pretty output stays clean.
- `set -eu`; a `die` helper prints a red error with a remediation hint.
- A cleanup trap removes temp dirs, the temporary swapfile, and any background
  spinner PID on exit.
- **Idempotent / re-runnable:** every step skips work already done; re-running
  after a repo change rebuilds and upgrades in place.

## 12. Testing

- End-to-end run inside a **systemd-enabled Debian 12/13 Docker container or
  VM**: fresh-install happy path (each ingress mode), re-run idempotency, and
  the non-TTY degradation path (piped output produces clean logs, no escape
  soup).
- `shellcheck` on `deploy/bootstrap.sh` in CI.

## Open implementation notes

- The gradient/spinner rendering must be implemented with `printf` and raw ANSI
  in POSIX sh (no `tput` dependency assumed, though `tput` may be used if
  present for capability probing).
- The Go version is read dynamically from `go.mod`; the go.dev download URL and
  checksum are derived from it.
