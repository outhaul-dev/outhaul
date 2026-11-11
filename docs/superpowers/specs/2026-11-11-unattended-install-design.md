# Unattended, pinned install and updates

Flags instead of prompts, an exact version, and a setting that says where
updates come from.

This covers requirement R5 of the brief "outhaul enhancements: running behind
Azure Front Door on a shared host" (10 November 2026). It builds on the design
for R1 to R4, `2026-11-11-proxy-ingress-design.md`, and treats its settings as
decided. Code references are to `06150cc`.

The maintainer decided on 11 November 2026 that this round installs a pinned
commit or tag by building it from source on the host. Released binaries with
checksums come later, and so does an update mode that follows release tags.
[Later: `OUTHAUL_UPDATES=releases`](#later-outhaul_updatesreleases) and
[Later: released binaries](#later-released-binaries) describe those steps so
that adding them changes no flag and neither of this round's values.

## Why

The first hosts are built by cloud-init with nobody at a terminal. Production
must run a known version and change only when ops decide. The hosts already
have Docker, and their firewall is outside the machine. Today:

| Area | What the code does |
|---|---|
| Prompts | `deploy/bootstrap.sh` asks on the terminal. With no terminal it reads answers from stdin in prompt order when started from a file, or takes every default when piped into `sh` (`open_prompt_fd`). A list of answers breaks when a prompt is added |
| Docker | `ensure_docker` runs `get.docker.com` whenever `docker` is not on `PATH`, and never notices a daemon that doesn't answer |
| nixpacks | `ensure_nixpacks` pipes upstream's install script into `bash`: the newest version, no checksum |
| Firewall | The ufw question defaults to yes |
| Version | `fetch_source` clones the default branch's head at depth 1. Every run rebuilds, replaces the binary and restarts, and keeps no copy of what ran before |
| Failure | `main` exits 0 when Outhaul never answered, and empties the log at each run |
| Updates | Settings → Updates and `deploy/update.sh` move to the newest `main`. Nothing turns that off, and `Checker.Run` asks GitHub every six hours |
| Git push | Answering "none" writes no `OUTHAUL_SSH_ADDR`, and Outhaul then listens on `:2222` anyway (`config.Load`, `startGitPush`). Nothing turns the SSH server off |

## Goals

1. `--non-interactive`: every answer comes from a flag. The run never prompts
   or reads stdin, and names what is missing before it changes anything.
2. `--ref`: exactly that commit or tag, built from source. The same ref again
   changes nothing. Another upgrades, or puts the old version back.
3. `--env-file`, repeatable: root's own files, loaded into the service by
   systemd. A new ingress choice writes the two R1 to R4 switches.
4. A working Docker is used as it is, and ufw is touched only when asked for.
5. nixpacks is one fixed version with a checked SHA-256.
6. `OUTHAUL_UPDATES=off|main` decides what the Updates panel, the background
   check and the root updater do.
7. Failure is a non-zero exit, each run is logged, and no secret travels on a
   command line. Git-push over SSH can be turned off.

## Non-goals

- `OUTHAUL_UPDATES=releases`, released binaries and a release workflow:
  described below, not built now.
- The new ingress choice in the wizard's menu. Its values come from a file.
- Environment variables as a second way to give every answer.
- Telling an upgrade from a downgrade in the installer. Migrations only go
  forward (`internal/store/migrate.go`), so an older ref over a newer database
  starts and may misbehave. The docs say not to do it.
- Waiting for running deploys, backups or restores before an upgrade by
  re-run. The installer stops Outhaul as it always has, and Outhaul marks
  interrupted work as failed at its next start (`RecoverActive`).
- Updating Docker, git or the OS. Signing. An uninstall.

## Anti-goals

The work has failed, even if it runs, if any of these happen:

- A non-interactive run prompts, waits on a terminal, or reads its stdin.
- It exits 0 while Outhaul isn't answering, or leaves the previous version
  down when it could have been put back.
- `get.docker.com` runs on a host that has Docker, or ufw is installed,
  enabled or changed without `--firewall ufw`.
- A secret, or the required header's value, appears in an argument, the log
  or the output. Or the setup URL appears in an unattended run's output.
- The service starts without one of its env files.
- With `OUTHAUL_UPDATES=off`, the UI or anything running as the `outhaul` user
  changes the installed version. Or, in any mode, that user gains a say in
  what root installs: the update request still carries nothing.
- An Outhaul with none of the new settings shows another panel, makes another
  request or updates differently. Or the wizard asks different questions.

## Constraints

- The installer and updater are POSIX `sh`, tested as files under `dash`,
  `bash --posix` and `busybox sh`. CI's shellcheck is 0.9.0.
- Under `curl … | sh` the script is its own stdin, so nothing may read
  answers from stdin there.
- `outhaul.service` runs as the `outhaul` user. The installer and the updater
  both install the unit file from the tree, so a line added to it is lost at
  the next update.
- systemd reads `EnvironmentFile=` as root and expands nothing in it.
- The repository is public, and the installer knows no vendor.

No locked decision in `ARCHITECTURE.md` and no "Decisions to keep" entry in
`AGENTS.md` changes. These lines of earlier designs are touched, each opt-in:

| Earlier text | Where | Here |
|---|---|---|
| Non-goal: "Unattended / fully env-driven mode (no-prompt install)" | `2026-07-13-remote-installer-design.md` | Added behind `--non-interactive`. The wizard stays |
| "Never overwrite an existing `/etc/outhaul.env`" | same | Kept. Unattended answers go to another file |
| "An update brings the server to the newest commit on `main`. *(Maintainer's decision.)*" | `2026-10-23-ui-update-design.md` | `main` stays the default. `off` is a new value. `releases` comes later |
| Non-goal: "Choosing a branch, tag or commit, or downgrading" | same | The panel still can't. `--ref` belongs to the installer, run by root |
| Non-goal: "Prebuilt binaries or a release pipeline" | both | Still not built |
| Anti-goal: the request "carries no URL, ref or command, and the updater reads nothing from it" | same | Kept. The mode comes from root's env files, the target from the updater itself |

## Settings

| Variable | Values | Default | Needs |
|---|---|---|---|
| `OUTHAUL_UPDATES` | `main` or `off`, in any case | `main`: as today | nothing |
| `OUTHAUL_SSH_ADDR` | a listen address as today, or `off` in any case | `:2222` | nothing |

A new `parser` method in `internal/config/config.go` accepts the two words
and reports anything else (`OUTHAUL_UPDATES="weekly": want main or off`), so
`Validate` stops the start. `releases` is reported the same way until it is
built. `off` is accepted for `OUTHAUL_SSH_ADDR` and for no other address. Neither setting has a refused combination. The
installer's flags are not settings: `OUTHAUL_REF` is never written to a file.

## The command line

`parse_args` knows `--verbose`, `--from-checkout DIR` and `--help`. It gains:

| Flag | Values | When left out | Without `--non-interactive` |
|---|---|---|---|
| `--non-interactive` | | the wizard, as today | |
| `--ingress` | `letsencrypt`, `tunnel`, `local`, `local-ca`, `proxy` | exit 2 | exit 2 |
| `--public-url` | a URL, through `normalize_public_url` | exit 2 where the ingress needs it | exit 2 |
| `--acme-email` | an address | exit 2 where the ingress needs it | exit 2 |
| `--git-ssh` | `off`, or a port through `normalize_git_port` | `2222`, the prompt's default | exit 2 |
| `--nixpacks` | `on` or `off` | `on`, the prompt's default | exit 2 |
| `--firewall` | `leave` or `ufw` | `leave` | exit 2 |
| `--updates` | `main` or `off` | no line is written | exit 2 |
| `--install-docker` | | Docker must already be there | exit 2 |
| `--setup-url-file` | a path | no file | exit 2 |
| `--env-file` | a path; repeatable | none | allowed |
| `--ref` | a tag, a branch, or a 40-character commit | `OUTHAUL_REF`, else the default branch's head | allowed |

- With `--non-interactive`, fd 3 is `/dev/null`, as in `update_main`, and
  `main` takes the wizard's answers from the flags. Without it nothing
  changes: the terminal, then stdin when the script is a file, then defaults.
- Every flag is checked before anything is installed, and all problems are
  printed together: exit 2. `--ref` with `--from-checkout` is one of them.

| `--ingress` | The wizard's | Needs | Written to [`install.env`](#where-settings-go) |
|---|---|---|---|
| `letsencrypt` | 1 (`MODE=a`) | `--public-url`, `--acme-email` | `OUTHAUL_PUBLIC_URL`, `OUTHAUL_ACME_EMAIL` |
| `tunnel` | 2 (`b`) | nothing | nothing |
| `local` | 3 (`c`) without the local CA | nothing | nothing |
| `local-ca` | 3 with it | nothing | `OUTHAUL_LOCAL_CA=true` |
| `proxy` | not offered | `--public-url`, `--acme-email`, and a line `OUTHAUL_ACME_DNS_PROVIDER=` in one of the env files | `OUTHAUL_PUBLIC_URL`, `OUTHAUL_ACME_EMAIL`, `OUTHAUL_HTTP_PORT=off`, `OUTHAUL_ADMIN_DIRECT=off` |

Every ingress also writes `OUTHAUL_SSH_ADDR=:<port>` or `=off`,
`OUTHAUL_UPDATES` when `--updates` is given, and `OUTHAUL_PUBLIC_URL` when
`--public-url` is. `--acme-email` is refused where it isn't needed: with the
local CA, `Validate` refuses it.

The other prompts, in a non-interactive run:

| Prompt today | Non-interactive |
|---|---|
| Unsupported distro: continue anyway? | exit 1 |
| DNS doesn't point here (`acme_preflight`): continue? | a warning, and it goes on. Not checked for `proxy`: DNS points at the proxy |
| Low memory: add a temporary swapfile? | yes; removed after the build, as today |
| Remove the Go toolchain? | yes |
| Firewall: proceed? | not asked; see [ufw](#docker-nixpacks-ufw) |
| (apt's own questions) | `DEBIAN_FRONTEND=noninteractive` |

## Where settings go

`write_env_file` writes `/etc/outhaul.env` once and never again, and the unit
loads it (`EnvironmentFile=-/etc/outhaul.env`). A flag changed on a re-run
would be ignored without a word. The unit file can't hold `--env-file` paths
either, because every update replaces it.

| File | Written by | Holds |
|---|---|---|
| `/etc/outhaul.env` | the wizard, once; people after that | as today. A non-interactive run never writes it |
| `/etc/outhaul/install.env`, `root:root 0600` | a non-interactive run, when its content would differ | the lines in the table above, under a comment saying each run rewrites it |
| each `--env-file` | whoever builds the host | everything else. The installer never changes or parses them, apart from the one `proxy` check |
| `/etc/systemd/system/outhaul.service.d/10-install.conf`, and the same under `outhaul-update.service.d/` | a run with `--non-interactive` or `--env-file` | `EnvironmentFile=` lines for exactly this run's files: `install.env` in a non-interactive run, then each `--env-file` in the order given |

- **Order.** systemd loads the unit's own file, then the drop-in's, and a
  later file wins: `/etc/outhaul.env`, then the flags, then each `--env-file`.
  A line in `/etc/outhaul.env` that no flag covers keeps applying. If it
  clashes (`OUTHAUL_LOCAL_CA=true` there, and now `--ingress letsencrypt`),
  Outhaul's `Validate` stops the start and the run exits 1 with its message.
- **The updater's unit** gets the same drop-in: it must see the service's
  listen address, data directory, image overrides and `OUTHAUL_UPDATES`.
- **A missing file stops the service**: the paths have no `-` prefix. Started
  without the file, the service would run with no required header.
- **An `--env-file`** must be an absolute path to a regular file, owned by
  root, that group and others can't write. Root's updater loads it too. The
  `outhaul` user needn't read it. This is checked before anything changes.
- **A run with neither flag** leaves the drop-ins alone, so a plain
  `curl … | sh` on such a host doesn't drop its env files.
- **Editing an env file** restarts nothing, and a re-run doesn't notice.
  Whoever changes it runs `systemctl restart outhaul`.

### What the first deployment writes

The installer maps no name to another. The infrastructure side writes
Outhaul's own names, as literal lines, in the file it already writes:

```
# /etc/outhaul/azure.env, root:root 0640, written by cloud-init
AZURE_CLIENT_ID=<client ID>
AZURE_SUBSCRIPTION_ID=<subscription>
AZURE_RESOURCE_GROUP=<resource group>
AZURE_ZONE_NAME=demo.example.com
OUTHAUL_ACME_DNS_PROVIDER=azuredns
OUTHAUL_ACME_DNS_ENV=AZURE_CLIENT_ID,AZURE_SUBSCRIPTION_ID,AZURE_RESOURCE_GROUP,AZURE_ZONE_NAME
OUTHAUL_TLS_WILDCARD=*.shared.demo.example.com
OUTHAUL_REQUIRE_HEADER_NAME=X-Azure-FDID
OUTHAUL_REQUIRE_HEADER_VALUE=<the profile's GUID>
```

Outhaul doesn't read the contract's `AZURE_FDID` or `SE_WILDCARD_DOMAIN`.
Their values go under the last three names. Then cloud-init runs:

```sh
ref=<40-character commit>
curl -fsSL "https://raw.githubusercontent.com/outhaul-dev/outhaul/$ref/deploy/bootstrap.sh" -o /root/outhaul-install.sh
sh /root/outhaul-install.sh --non-interactive --ref "$ref" \
  --ingress proxy --public-url https://outhaul.shared.demo.example.com \
  --acme-email ops@example.com --env-file /etc/outhaul/azure.env \
  --git-ssh off --updates off
```

`install.env` then holds the public URL, the email, `OUTHAUL_HTTP_PORT=off`,
`OUTHAUL_ADMIN_DIRECT=off`, `OUTHAUL_SSH_ADDR=off` and `OUTHAUL_UPDATES=off`:
with the file above, the R1 to R4 design's example block. The installer is
fetched at the commit it installs, so the script root runs is as pinned as
what it builds. If it is a file and differs from `deploy/bootstrap.sh` in the
tree it fetched, it logs a warning.

## The version: `--ref`

| Part | Today | Change |
|---|---|---|
| What a ref is | always the default branch's head | Forty hex characters are a commit. Anything else is a name, resolved with `git ls-remote` to the first of `refs/tags/<ref>^{}` (the commit behind an annotated tag), `refs/tags/<ref>` and `refs/heads/<ref>`. No match is an error. A shortened commit is refused: git can't fetch one. No ref means the remote's `HEAD`, as today |
| Fetching (`fetch_source`) | `git clone --depth 1` of `OUTHAUL_REPO` | `git init`, `git fetch --depth 1 <repo> <ref>`, a checkout of `FETCH_HEAD`, and then `HEAD` must be the target commit. The updater calls the same function |
| The stamp (`build_ldflags`, `runVersion`) | the commit, the repository, and whether the build was local | A fourth value: the tag's name when the ref was a tag. Its `-X` is added only then, so today's stamp is unchanged. `update.Build` gains `Version`, and `outhaul version` prints a `version=` line |
| The marker | none | The installer's last step writes `/var/lib/outhaul-updater/installed`: `commit=`, `ref=`, `version=`, `installed=<unix time>`. It means a run for this commit finished, so the binary, the unit and the updater's files all came from that tree. The updater doesn't write it: it never replaces its own files |

The installed version is the commit in the binary. A run compares the target
commit T with the commit I that `/usr/local/bin/outhaul version` reports:

| State | Fetch and build | Binary and unit | Service |
|---|---|---|---|
| No binary | yes | installed | started |
| I = T and the marker says T | no | untouched | started if it isn't active; restarted if `install.env` or a drop-in changed; otherwise left alone |
| Anything else | yes | swapped, with a way back (below) | restarted once |

In the middle row the run still brings the rest into line (`install.env`, the
drop-ins, the path unit, nixpacks, ufw when asked) and ends with the health
checks. A commit ref with nothing to do needs no network. The table holds in
both modes, so a wizard re-run no longer rebuilds an unchanged `main`.
`--from-checkout` always builds.

A tag can be moved. The log names the commit a tag resolved to, and the docs
recommend a commit for production. `--ref` doesn't stop the Updates panel from
moving the server later; `OUTHAUL_UPDATES=off` does.

**The build needs**, as today: `git`, `curl`, `tar`, `sha256sum`; the Go
toolchain `go.mod` names, from `dl.google.com`, checked against its published
SHA-256 (`install_go_toolchain`); the modules, from the Go proxy, checked
against `go.sum`; and 2 GiB of memory and swap together, or a temporary
swapfile. No container is involved.

## Installing and upgrading

`install_binary` and `install_service` copy the files and restart. The updater
is more careful (`deploy/update.sh`): `pull_images` before anything stops, a
`snapshot` of the binary, the unit and the database, `install_new` by rename,
`new_version_healthy` (60 tries for `/healthz` on loopback to report the new
commit), and `restore_snapshot` when it doesn't.

The brief upgrades production by re-running the installer, so the installer
does the same, in both modes. `build_field`, `short`, `pull_images`,
`replace_file`, `snapshot`, `restore_snapshot`, `install_new`,
`answers_commit`, `new_version_healthy` and `recovery_help` move from
`update.sh` to `bootstrap.sh` unchanged. `update.sh` sources `bootstrap.sh`,
so `update_main` and its tests stay as they are. `snapshot` and
`restore_snapshot` also cover `install.env` and the drop-ins, and a file the
snapshot lacks is removed on restore, as the `-wal` file is today: an old
binary may refuse what new flags wrote.

| Case | Steps | When the new commit doesn't answer |
|---|---|---|
| First install | Pull the images, install, start, wait with `new_version_healthy`. `wait_healthy` goes: a slow image pull can outlast its 30 seconds | Exit 1. The service is left as it is, for its journal |
| Upgrade | Pull, stop Outhaul, `snapshot`, install, write `install.env` and the drop-ins, start, wait | Stop, `restore_snapshot`, start the old one, check that it answers. Exit 1, saying it rolled back. If the old one doesn't answer either, print what `recovery_help` prints |

## Docker, nixpacks, ufw

**Docker.** `ensure_docker` tells three states apart, in both modes. git is
still installed with apt when missing.

| State | How it is told | What happens |
|---|---|---|
| Working | `docker version --format '{{.Server.Version}}'` and `docker compose version` both succeed within 30 seconds (at first boot the daemon may still be starting) | Used. Nothing is installed, enabled or changed |
| Absent | no `docker` on `PATH`, no `docker.service`, no `/var/run/docker.sock` | The wizard: `get.docker.com`, as today. Non-interactive: exit 1, unless `--install-docker` |
| Anything else | | `systemctl enable --now docker` once, as today, and 30 more seconds. Then exit 1, naming what is missing. `get.docker.com` never runs |

**nixpacks.** Upstream publishes no checksum file, so the pin lives in
`bootstrap.sh`: `NIXPACKS_VERSION`, `NIXPACKS_SHA256_AMD64`,
`NIXPACKS_SHA256_ARM64`. `ensure_nixpacks` stops piping a script into `bash`.
It downloads `nixpacks-v<V>-<target>.tar.gz` from
`https://github.com/railwayapp/nixpacks/releases/download/v<V>/`, for
`x86_64-unknown-linux-musl` or `aarch64-unknown-linux-musl` (the builds
upstream's script picks), checks it with `sha256sum`, and installs the one
file inside to `/usr/local/bin/nixpacks` with `install_atomic`.

| nixpacks on the host | The wizard, answered yes | `--nixpacks on` | `--nixpacks off` |
|---|---|---|---|
| none | the pin is installed | the pin is installed | nothing |
| the pin (`nixpacks --version`) | nothing | nothing | nothing |
| another version | left, with a note naming the pin | replaced by the pin | nothing |

A wrong checksum is fatal in both modes. A failed download is a note in the
wizard, as today, and exit 1 in a non-interactive run. The pin: `1.41.0`, the
newest release (24 October 2025), with
`0f55de7874507b9cf7502113120bd96f2ab6979f78d10eaf2eb2ade9207b3af6` for amd64
and `912bd02dd2bb6f9c3a9ed965fe8a68b4aa318dc7a2546e2eca6f2806a894ba39` for
arm64. The builder downloads both again and checks them before committing.

**ufw.** With `--firewall leave` no `ufw` and no `apt-get install ufw` runs.
With `--firewall ufw` the ports are today's (`derive_firewall_ports`, which
gains `proxy`: 22 and 443), plus the bridge rule for the admin port. If
enabling ufw would block ports that are listening (`ufw_would_block`), the run
exits 1 before changing anything, where the wizard would ask.

## Health checks and hints

With `OUTHAUL_ADMIN_DIRECT=off`, Outhaul listens on `127.0.0.1` and on its
Docker network's gateway address only. Against the installer:

| Today | With the R1 to R4 settings | Change |
|---|---|---|
| `wait_healthy` asks `http://127.0.0.1:<port>/`, and `new_version_healthy` asks `/healthz` there | Still answered: loopback stays, and `/healthz` gives loopback callers the commit | none |
| The port comes from `OUTHAUL_LISTEN_ADDR` in the installer's own environment | Wrong when an env file sets it | read it from the service |
| `wait_public_healthy` asks Traefik on 443 for `https://<admin host>/healthz`, ignoring the certificate, so it doesn't wait for ACME | 404 without the required header | send the header; use the service's `OUTHAUL_HTTPS_PORT` |
| It runs for Let's Encrypt only | `proxy` has no other way to the admin UI | run it for `letsencrypt` and `proxy` |
| The setup hint's first `open:` line is the public URL when HTTPS is on (`setupURLs`) | The same | none |
| `completion_screen`: SSH-tunnel hints, and the ufw bridge rule | Loopback still serves an SSH tunnel; the gateway is on that bridge | `proxy` is shown like Let's Encrypt |

- **`service_env NAME`**, new, prints a variable as the running service has
  it, from `/proc/<MainPID>/environ` (root can read it, and it holds what
  systemd loaded), else from the installer's environment, else the default.
  It gives the admin and HTTPS ports, `OUTHAUL_UPDATES` and the header.
- **The header** reaches `curl` on stdin (`curl -K -`, one
  `header = "Name: value"` line), not as an argument, and is never logged.
- **A failed check** in a non-interactive run is exit 1, with the journal's
  last 20 lines in the log. The wizard keeps its screen and exit code.
- **`proxy` checks its own effect.** An Outhaul from before R1 to R4 ignores
  the two switches without a word (`Load` reads only names it knows), and
  starts with port 80 published and the admin port open. So after the public
  check, `ss -ltn` must show the admin port on no wildcard address, and
  `docker port outhaul-traefik` no port 80. Otherwise the run exits 1: an
  upgrade is rolled back, and a first install is stopped.

## Output, exit codes and the setup URL

**The log.** `main` empties `/var/log/outhaul-install.log` and sends commands'
raw output there; the step lines go to the terminal only. A non-interactive
run appends instead, mode `0600`: a first line with the UTC time and the
arguments (no flag takes a secret), each `step_ok`, `step_fail`, `note` and
`die` line, and a last line `OK …` or `FAILED …`. Its stdout has no colour.

**Exit codes.** 0: installed, or nothing to do. 1: something failed; the log
says what, and whether the old version is back. 2: bad flags, nothing changed.

**The setup URL.** Until the first account exists, Outhaul prints it to the
journal at each start, with a new token each time (`printSetupHint`). Under
cloud-init the installer's output is copied to cloud-init's own log and to
the serial console, so a non-interactive run doesn't print it, and points at
`journalctl -u outhaul`. With `--setup-url-file PATH` it also writes the
`open:` URL of the service's current start to PATH, `root:root 0600`, by
rename. If this run started the service and that start printed no URL, setup
is done and the file is removed. A later restart makes the file stale.

## `OUTHAUL_UPDATES`

`serve` (`main.go`) builds `update.NewDir` and `update.NewChecker`, starts
`checker.Run`, holds the worker and the backup scheduler while
`updates.InProgress`, and calls `srv.SetUpdates`. The updater's unit reads
`/etc/outhaul.env`.

| | `main` | `off` |
|---|---|---|
| Settings → Updates | as today | the running commit and version, and a line saying updates are off and the server is upgraded by re-running the installer. No Update, no Check now, no last result |
| `POST /settings/update` and `/check`, the progress poll | as today | 404, as when updates aren't wired (`updatesWired`) |
| Navigation badge | as today | never |
| Background check | GitHub, every six hours | none: no request leaves the server |
| Holds on the worker and backups | as today | none |
| `deploy/update.sh` | newest `main` | removes the request, writes one journal line, changes nothing, exits 0 |
| `outhaul-update.path` | enabled | disabled by the installer |

The updater reads `OUTHAUL_UPDATES` from its own environment, in any case, and
treats a value it doesn't know as `off`. The installer reads it with
`service_env` after the start, in both modes, and enables or disables the path
unit to match. Either guard is enough alone; the updater's covers an env file
edited by hand.

## `OUTHAUL_SSH_ADDR=off`

`startGitPush` always opens the SSH listener, on the stored `ssh_addr` setting
or else `OUTHAUL_SSH_ADDR`. With `off` it opens none and logs one line. The
stored setting is ignored, so Settings can't turn it back on: with no SSH
server the page already shows "not running", and `handleSetSSHAddr` already
refuses a new address. The hook socket and the repositories stay.
`--git-ssh off` writes it. So does the wizard's "none", which until now only
kept the firewall port closed.

## Later: `OUTHAUL_UPDATES=releases`

Not part of this round (the maintainer's decision, 11 November 2026): the
first hosts run `off`, and the repository has no release tag yet. Nothing in
this section is built now. It is kept so the value can be added without
changing a flag, or what `main` and `off` do.

A release is a tag `v<major>.<minor>.<patch>`, digits only, in the build's
repository, and the newest is the highest by number. Other tags, pre-releases
included, don't count. So `releases` would work from the first such tag the
maintainer pushes, built from source. There is none today.

In this mode the panel, the navigation badge and the background check work
as in `main`, against the newest release: the check reads the tags, then
GitHub. `deploy/update.sh` moves to the newest release, and
`outhaul-update.path` stays enabled.

| Where | Change |
|---|---|
| Go, `internal/update` | `Comparer.Compare` gains the head to compare with: `main`, or the tag. A new pure function picks the newest release from tag names, with `tagdeploy.ParseVersion` and `Version.Compare`. The names come from `git ls-remote --tags --refs <repo> 'refs/tags/v*'`, behind a function tests replace. `Checker` takes the mode, and the `Check` result gains the target tag and "no release yet" |
| The updater, `update_main` | It asks for the tags itself and picks the newest with a short `awk` program that follows the same rule. With no release it finishes `up_to_date`. Otherwise it fetches the tag with its history, not at depth 1, and goes on only if `git merge-base --is-ancestor <running commit> HEAD` succeeds. The rest is as it is. The build is stamped with the tag, and the request still carries nothing |

The panel (`updatesPanel`, `updateBlocked`) in `releases` mode. In `main` mode
it offers Update whenever it can run, as today.

| The check finds | Summary | Update |
|---|---|---|
| no release tag | "No release yet." | not offered |
| the running commit is the release's | "Up to date with v1.3.0." | not offered |
| behind it | "N commits behind v1.3.0.", with the commits | offered |
| ahead of it, or diverged | "This server runs a commit that isn't in the newest release, v1.3.0. The next release will be offered here." | not offered |
| the check failed | "Could not compare with the newest release: …" | not offered |

So root's updater, not only the panel, keeps `releases` from moving a server
backwards. Most will switch to it from a `main` build ahead of the last tag.

## Later: released binaries

Not part of this round. Nothing in this section is built now.

| Part | What it would be |
|---|---|
| Trigger | The maintainer pushes a tag `vX.Y.Z`: the tags `releases` follows |
| Workflow | `.github/workflows/release.yml`, on such a tag: the tests CI runs; then `CGO_ENABLED=0 go build -trimpath` for linux/amd64 and linux/arm64, with the `build_ldflags` stamp. Each binary goes into `outhaul-vX.Y.Z-linux-<arch>.tar.gz` beside `deploy/outhaul.service`, the two updater units, `deploy/update.sh` and `deploy/bootstrap.sh`, at their paths in the source tree. A `SHA256SUMS` file in `sha256sum` format covers both archives. All three are attached to a GitHub release for the tag |
| Installer and updater | One step before the build, in the function both call. When the ref is a release tag and the repository is on GitHub, it downloads `SHA256SUMS` and this architecture's archive, checks the archive, unpacks it where the source tree would be, and requires `outhaul version` from it to report the commit the tag resolves to. What follows reads `$SRC_DIR/outhaul` and `$SRC_DIR/deploy/`, as after a build. Another kind of ref, a tag with no release, or another git host builds from source, as now |
| What the checksum shows | It comes from the same release as the archive, so it catches a broken download, not a replaced release. A commit ref, always built from source, stays the strongest pin |
| Unchanged | `--ref`, `OUTHAUL_UPDATES`, the marker, the swap and the rollback. A host installing a release needs no Go toolchain |

## Checked so far

On 11 November 2026, in throwaway containers and repositories:

| What | Version | Result |
|---|---|---|
| systemd, Debian 13 | 257 | A `User=` unit got the variables of `root:root 0640` and `0600` files its user can't read. A drop-in's `EnvironmentFile=` lines load after the unit's, and a later file wins. Values arrive as written, `*` and `=` included. A missing file with no `-` fails the start (`enable --now` exits 1), and `Restart=on-failure` keeps retrying. `/proc/<MainPID>/environ` shows the result. `systemctl disable --now` on a unit that isn't there exits 1 |
| git | 2.53.0 | `ls-remote` gives an annotated tag's commit on the `^{}` line. `fetch --depth 1` and a checkout of `FETCH_HEAD` gave the one right commit for a tag, an annotated tag, a branch and a full commit, from a local repository and from GitHub (another public project). A 7-character commit was refused. After a fetch with history, `merge-base --is-ancestor` exits 0 for an ancestor and 128 for a commit that isn't in it |
| awk | GNU 5.3.2, busybox 1.37.0 | The release rule picked `v0.10.0` over `v0.2.0` and skipped `v0.3.0-rc.1` |
| nixpacks | 1.41.0 | Each archive holds one file, `nixpacks`. The SHA-256 values above match the digests GitHub reports. `nixpacks --version` prints `nixpacks 1.41.0` |
| curl | 8.18.0 | `-K -` sends a header given on stdin as `header = "…"` |

Not checked: a run under a real cloud-init; `mawk`, Debian's default `awk`; a
fetch by commit from Outhaul's own repository; an upgrade and a rollback under
a real systemd, which `integration.sh` below covers.

## Tests

Shell, in `deploy/test`, each under `dash`, `bash --posix` and `busybox sh`:

| File | Covers |
|---|---|
| `test_main.sh` | Each new flag; `--env-file` twice, in order; `--ref` over `OUTHAUL_REF`; a flag with no value. Exit 2 with nothing called: an answer flag without `--non-interactive`, each ingress without what it needs, `--ref` with `--from-checkout`, an env file that is relative, missing, not root's or writable by others, `proxy` with no provider line |
| `test_env.sh` | `install.env` per ingress, with `OUTHAUL_SSH_ADDR=off` and the updates line; the drop-in's lines and order; an unchanged file keeps its inode |
| `test_ref.sh`, new | Commit or name; `ls-remote` output with a lightweight tag, an annotated tag and a branch; no match; a short commit; `fetch_source` with a ref, and a `HEAD` that isn't the commit asked for; `build_ldflags` with a version; the marker; the three states |
| `test_nixpacks.sh`, new | The pins; the target per architecture; each cell of the table, with `curl` a stub and `sha256sum` real; a wrong checksum installs nothing; no `\| bash` left in the script |
| `test_docker.sh`, new | The three states from stubs; `curl` never called when Docker is working or broken; absent, with and without `--install-docker` |
| `test_firewall.sh` | `proxy` opens 22 and 443; `--firewall leave` calls neither `ufw` nor `apt-get`; `--firewall ufw` that would block a listening port exits 1 first |
| `test_health.sh` | `service_env` from a stand-in `/proc`; the header on `curl`'s stdin and not in its arguments; the service's HTTPS port; the two `proxy` checks |
| `test_noninteractive.sh`, new | `main` in a subshell, in the style of `test_update.sh`: paths in a temp tree, recording stubs, and a stdin of wrong answers that must come back unread. A first install; a re-run with no `git fetch`, no `go` and no restart; a changed flag (one restart); a new ref (swapped, copy kept); a new version that never answers (rolled back, exit 1, the old `install.env` back); a first install that never answers (exit 1, the journal's tail in the log); the log appended, with no setup URL in the output; the setup file `0600`, and removed once setup is done |
| `test_update.sh` | `off` removes the request and calls nothing else; a value it doesn't know does the same; `main` as it is. The moved functions keep their assertions |

`deploy/test/integration.sh` is rewritten, and stays out of CI like
`integration_gate.sh`. `Dockerfile.debian` becomes Debian 13 with systemd, git
and Docker CE; the R1 to R4 plan's `Dockerfile.proxytest` is the same image,
and whichever lands second uses the first. The script runs it privileged, as
`outhaul-installtest-*`, and installs with `OUTHAUL_REPO=file:///src` and
`--ref` the checkout's `HEAD`. It checks: exit 0 with no terminal and stdin
closed; the service active at that commit; a `root:root 0640` env file loaded;
no `ufw` and no `get.docker.com`; nothing on 2222; the setup file; a second
run that leaves the service's start time and the binary's inode alone; the
pinned nixpacks; an upgrade to a second commit; a third, made to fail at
start, rolled back with exit 1. `--ingress proxy` needs a real ACME server.

| Go package | Covers |
|---|---|
| `internal/config` | `OUTHAUL_UPDATES` values, case, a bad value, and `releases` refused; `OUTHAUL_SSH_ADDR=off`, and `off` refused for `OUTHAUL_LISTEN_ADDR` |
| `internal/update` | The version stamp. `Compare` and `Checker` are unchanged in this round |
| `internal/server` | The panel with `off`: no forms, no badge, three routes answering 404; the existing tests unchanged for `main` |
| `main` | `outhaul version` prints the version line; with `off` no check runs and nothing is held; with SSH off no listener opens and a stored `ssh_addr` is ignored |

## Docs

| File | Change |
|---|---|
| `README.md` | A short "Unattended" part, "Updating", and the configuration table (`OUTHAUL_UPDATES`, `OUTHAUL_SSH_ADDR`) |
| `docs/UNATTENDED-INSTALL.md`, new | The flags, the files and their order, what a re-run does, upgrading and what a rollback leaves, exit codes, the log, the setup URL, the example above, the host checks below. It links to `docs/BEHIND-A-PROXY.md` for what the proxy settings mean |
| `docs/MANUAL-DEPLOY.md` | nixpacks by pinned download, the drop-in, `OUTHAUL_SSH_ADDR=off`, and never an older version over a newer database |
| `ARCHITECTURE.md` | The two update modes |
| `AGENTS.md` | The new tests and `integration.sh`; how to move the nixpacks pin. Under "Decisions to keep": a non-interactive run never prompts and never touches Docker or ufw unasked; the request carries nothing in every mode; `off` is enforced by the updater as well as the UI |
| `deploy/outhaul-update.path`, `deploy/outhaul-update.service` | Their comments and `Description=`, which say "the newest main" |

## Order of work

One series of commits on `enhancements`. Each part can be its own pull
request, with its tests and docs:

1. `OUTHAUL_SSH_ADDR=off`.
2. `OUTHAUL_UPDATES` with `main` and `off`: the setting, the panel, the
   updater's refusal. `Validate` refuses `releases`.
3. The installer: Docker's three states.
4. The installer: pinned nixpacks.
5. The installer: `--ref`, the stamp, the marker, no rebuild when unchanged.
6. The installer: the careful swap, with the functions moved from `update.sh`.
7. The installer: `--non-interactive`, its flags and checks, `install.env`,
   the drop-ins, `service_env` and the health checks, the log, the exit codes,
   the setup file, `--ingress proxy`.
8. `integration.sh` and `docs/UNATTENDED-INSTALL.md`.

Later, not in this round: `OUTHAUL_UPDATES=releases`, the release workflow,
and installing from a release.

Nothing here needs R1 to R4 built first, except that `--ingress proxy` in
part 7 passes its own check only with an Outhaul that has them.

## What only the real host can show

1. cloud-init runs the command above on a fresh Debian 13 VM: exit 0, the
   install log ends with `OK`, and cloud-init's own log has no setup URL.
2. `outhaul version` prints the pinned commit, and so does
   `/var/lib/outhaul-updater/installed`.
3. `systemctl show outhaul -p EnvironmentFiles` lists `install.env` and
   `azure.env`. `azure.env` is still `root:root 0640`.
4. `command -v ufw` finds nothing. `docker version` is what cloud-init
   installed, and the log has no `get.docker.com`.
5. `ss -ltn` shows nothing on 2222, and the admin port on loopback and one
   bridge address only.
6. The same command again exits 0 in seconds, and
   `systemctl show outhaul -p ActiveEnterTimestamp` hasn't moved.
7. The command with a newer `--ref` upgrades. The old version is in
   `/var/lib/outhaul-updater/previous`.
8. Settings → Updates shows the version and that updates are off. Creating
   `/var/lib/outhaul/update/request` as the `outhaul` user changes nothing.
9. The URL from `journalctl -u outhaul`, or from the setup file, opens through
   the CDN and creates the first system admin.
10. After a reboot the service is active with both files loaded.
11. A Nixpacks app builds with the pinned nixpacks.

## Left to the builder

- Function and variable names in the scripts, the wording of messages, and
  how `main` is split so `test_noninteractive.sh` can point it at a temp tree.
- How the mode reaches `Server` and `Checker`, and the template's shape.
- `pull_images` asks the new binary for its images in the installer's own
  environment, so an image override in an env file isn't pulled beforehand.
- Whether a host without `ss` skips the `proxy` check with a warning or fails.
- How `integration.sh` makes a commit that fails at start.
- Nothing here writes the `LoadCredential=` line for R16's root-only secret
  file. Whoever builds the host puts it in a drop-in of their own beside the
  installer's (`2026-11-11-client-ips-streams-sign-in-design.md`, "The secret
  file"). The installer must leave other drop-ins in that directory alone.

## Calls made in this design

Each goes beyond the brief or chooses between readings of it. The maintainer
approved this design on 11 November 2026. The first four were put as
questions, and the answers are given here. The rest were this design's calls,
approved with it.

1. **Agreed.** An upgrade by re-run keeps a copy and rolls back, as the
   Updates panel does. The brief asks only for "safe to re-run".
2. **Agreed.** Unattended answers live in `/etc/outhaul/install.env`,
   rewritten by every run to match its flags. `/etc/outhaul.env` is never
   overwritten.
3. **Agreed.** The wizard changes too: Docker's three states, pinned
   nixpacks, no rebuild of the commit already installed, the careful swap,
   and "none" writing `OUTHAUL_SSH_ADDR=off`. It asks the same questions.
4. **Decided: later.** `OUTHAUL_UPDATES=releases` is not built in this round.
   The first draft built it now. `Validate` refuses the value until it is.
5. Flags are the interface. Only `OUTHAUL_REF`, which the brief names, and
   `OUTHAUL_REPO` come from the environment. Answer flags need
   `--non-interactive`; `--ref` and `--env-file` don't.
6. A flag left out takes the wizard's default, except the ingress (required)
   and the firewall (left alone).
7. `--ingress proxy` is the brief's "behind a TLS-terminating CDN with
   DNS-01". The wizard doesn't offer it. It requires a DNS provider line; a
   header and a wildcard stay optional, as in the R1 to R4 design.
8. The installer writes what its flags say, plus the two `proxy` switches.
   The provider, its variables, the wildcard and the header go in an env
   file, under Outhaul's names, written by whoever builds the host.
9. A `proxy` run fails when the two switches had no effect.
10. Env files are loaded from a drop-in, for the updater's unit too. They
    must be root's own regular files, and a missing one stops the service.
11. With no Docker, a non-interactive run fails unless `--install-docker` is
    given. A Docker that is installed but not working is always an error.
12. `--nixpacks on` replaces another version with the pin. The pin lives in
    `bootstrap.sh`, so the documented command fetches the installer at the
    commit it installs; a mismatch is only a warning.
13. A shortened commit is refused, a branch name accepted. The build is
    stamped with the tag's name, and a root-owned marker records the last run.
14. The first health check is the updater's: 60 tries, and the commit must
    match. A failed check is exit 1 in non-interactive mode only.
15. Non-interactive: a DNS mismatch under `letsencrypt` is a warning, and
    `--firewall ufw` that would block a listening port fails first.
16. The log is appended to, with step lines, in non-interactive mode only.
17. An unattended run doesn't print the setup URL. The optional file is
    written by the installer, so it goes stale when the service restarts.
18. `OUTHAUL_SSH_ADDR=off` is new: nothing could turn the SSH server off.
19. With `off`, the panel still shows the version, the routes answer 404, the
    updater refuses, and the path unit is disabled. The updater treats a
    value it doesn't know as `off`.
20. When `releases` is built, Update is offered only when the server is
    behind, and the updater checks that itself. `main` mode is unchanged.
21. Downgrades by `--ref` aren't detected, and an upgrade by re-run doesn't
    wait for running deploys or backups.
22. `integration.sh` is restored, and stays out of CI.
