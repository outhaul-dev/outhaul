# Update Outhaul from the admin UI — design

**Date:** 2026-10-23
**Status:** Draft, for maintainer review

## Why

Getting a fix onto a server today means SSHing in and re-running the installer
(`curl -fsSL https://outhaul.sh/install | sh`), which asks every installer
question again. Fixes land on `main` often: on 2026-10-23 two rounds of fixes
came straight out of a live install. The maintainer wants to bring a server up
to date from the admin UI instead.

## What "latest" means

Outhaul has no tags or releases, and the installer builds whatever is newest
on `main` of the upstream repository (`OUTHAUL_REPO`, by default
`https://github.com/outhaul-dev/outhaul.git`). An update brings the server to
the newest commit on `main`. *(Maintainer's decision.)* Following tagged
releases instead is a possible later change, not part of this work.

## Goals

- The admin sees which commit the server runs, whether `main` has moved on,
  and the commit messages in between.
- One button updates: fetch `main`, build, swap the binary, restart. The UI
  shows progress live.
- A failed update leaves the server as it was. A build failure changes
  nothing. A new version that does not come up healthy is rolled back
  automatically, binary and database, and the UI says what happened.
- Apps keep serving throughout, with one accepted exception: an update that
  changes Traefik's container (its version or settings) recreates it, and
  every app is unreachable for a few seconds, twice if the update rolls back.
  Re-running the installer does the same today, and a single Traefik cannot
  avoid it. The panel warns about it. *(Maintainer's decision.)*
- Only Outhaul itself is unavailable, briefly, while it restarts: the admin
  UI, GitHub webhooks and git push.
- No deploy, backup or restore is cut off by an update.

## Non-goals

- Automatic or scheduled updates: only an admin's click updates.
- Updating Docker, git, Nixpacks, Go or the OS.
- Applying the installer's own changes: firewall rules, `/etc/outhaul.env`,
  packages. The panel warns when an update includes them, and re-running the
  installer applies them. *(Maintainer's decision.)*
- Choosing a branch, tag or commit, or downgrading. The only way back is the
  automatic rollback to the version that was running.
- Prebuilt binaries or a release pipeline. The server builds, as the
  installer does today.
- Installs the installer did not make (manual deploy, `go run`, the dev
  container). They show that updating from the UI is not set up.
- Catching up GitHub pushes that arrive during the restart. GitHub does not
  redeliver a webhook that fails, so such a push is not deployed, as with any
  restart of Outhaul today. The confirmation says so. *(Design's call.)*

## Anti-goals (failures even if it "works")

- An update that leaves the server without a working admin UI and no way back
  short of SSH.
- The non-root Outhaul process gaining a general way to run things as root.
  The one request it can make is fixed: "update to the newest `main`". It
  carries no URL, ref or command, and the updater reads nothing from it.
- App downtime caused by an update, beyond the Traefik case above.
- Re-asking or rewriting installer choices: `/etc/outhaul.env`, the firewall,
  the ingress mode.

## Constraints (the system today)

- `outhaul.service` runs as user `outhaul` with `NoNewPrivileges=yes`,
  `ProtectSystem=full` (`/usr` and `/etc` read-only to it) and `PrivateTmp`.
  It cannot replace `/usr/local/bin/outhaul` or restart itself. Its data dir
  `/var/lib/outhaul` (`StateDirectory`, mode 0700) is writable to it.
- The installer (`deploy/bootstrap.sh`, POSIX `sh`; must work under `dash`,
  `bash --posix` and `busybox sh`) is interactive from start to finish, but
  its steps are separate functions: `fetch_source`, `go_version_from_gomod`,
  `maybe_add_swap`/`remove_build_swap`, `install_go_toolchain`,
  `build_binary`, `install_binary`, `install_service`, `wait_healthy`. It
  builds from a depth-1 clone with `GOFLAGS=-buildvcs=false`, so the binary
  records no commit today.
- Stopping Outhaul does not stop Traefik or app containers. Shutdown aborts
  in-flight deploys (they are marked failed) and cancels running backups and
  restores, which record their outcome (`shutdown` in `main.go`).
- Migrations (`internal/store/migrations`) only go forward and run at startup.
  An older binary on a newer schema starts without complaint but may
  misbehave. SQLite runs in WAL mode.
- `GET /healthz` answers `ok` as soon as the HTTP server is up.
- GitHub's API allows 60 unauthenticated requests an hour per IP.

## How it works

Everything in this section is the design's call unless marked as the
maintainer's.

### 1. The binary knows its commit

The build stamps the commit and the upstream repository URL into the binary
(`-ldflags -X`, from `git rev-parse HEAD` of the clone). The installer and the
updater both build this way. A binary without a stamp (`go build` by hand,
`go run`) reports its commit as unknown, and the Updates panel says updating
is not available for this build. A `--from-checkout` build stamps the
checkout's HEAD, and marks the build as local if the tree had uncommitted
changes.

### 2. Checking for updates

Outhaul calls GitHub's compare API, `<running commit>...main`, on the upstream
repository, without credentials. The Updates panel shows one of:

- **Up to date.**
- **N commits behind:** each commit's short hash, first line and date, newest
  first. Show up to 50, then a link to the full comparison on GitHub.
- **Running a commit `main` does not have** (ahead or diverged, e.g. a
  `--from-checkout` build): Update is still offered, with a warning that it
  replaces that build with `main`.
- **Could not check** (network, rate limit, a non-GitHub upstream): Update is
  still offered, since it always builds the newest `main`.

Two warnings go with a "behind" result, from the comparison's changed files
and their diffs:

- **Installer changes.** When the incoming commits change the installer
  (`deploy/bootstrap.sh`): "This update includes installer changes. Updating
  here does not apply them; re-run the installer to get them." Also shown
  after a rollback, since a version that needs an installer change may fail
  its health check until the installer has run.
- **Traefik restarts.** When the incoming commits change Traefik's image or
  settings: "This update restarts Traefik: every app is unreachable for a few
  seconds." The updater's log says so too when the image list of the new
  build differs from the old one. How the panel tells, from the diffs, is the
  builder's call; when it cannot tell, it says nothing.

Results are cached. Opening the panel checks at most once every 10 minutes,
and a **Check now** button forces a check. A background check every 6 hours
drives a small "update available" badge in the navigation.

### 3. Holding work during an update

The check that disables the button covers only the moment of the click. The
build that follows takes minutes, while webhooks, git push, the backup
schedule and the preview manager keep producing work. Stopping Outhaul would
then abort a deploy (perhaps mid-cutover, leaving the app with no container)
or cancel a backup. And a new version that rolls back could be stopped in the
middle of a deploy it had just started.

So one rule, from the click until the update is over: Outhaul accepts work
but starts none of it. Deploys queue as usual. Scheduled backups wait for the
schedule's next free moment. A manual backup or restore is refused, saying an
update is in progress. Both processes follow the rule: the old one until the
updater stops it or finishes without a restart ("already up to date", "build
failed"); the new one until the updater has confirmed it healthy. Once the
update is over, the waiting work starts.

"Update in progress" means: the request file exists, or the status file says
the updater is running. If the request is not picked up within two minutes
(the updater is not installed or not running), Outhaul deletes it, says the
updater did not start, and resumes. *(Design's call, after the builder check
raised it.)*

### 4. From the UI to root: a fixed request

The installer installs, as root:

- `outhaul-update.path`, which watches for a request file in Outhaul's data
  dir (`PathExists=/var/lib/outhaul/update/request`) and starts:
- `outhaul-update.service`, `Type=oneshot`, running as root, which runs the
  updater script installed root-owned under `/usr/local/lib/outhaul/`.

Outhaul, as user `outhaul`, asks for an update by creating the request file.
The updater deletes it first thing and reads nothing from it. systemd runs one
updater at a time. The installer also creates the `update` directory and a
marker there that tells Outhaul the updater is installed. Without the marker,
the panel says updating from the UI is not set up, and that re-running the
installer once sets it up.

### 5. The updater

The updater is the installer's upgrade-only path. It reuses the installer's
functions and asks no questions. Its steps:

1. Delete the request. Start a fresh log and a status file in the `update`
   directory, both readable by `outhaul`. Every later step appends to the log.
2. Clone `main` (depth 1). If its commit is the one running, finish with
   "already up to date".
3. Read the Go version from `go.mod`, add temporary swap if memory is low,
   and get the Go toolchain. The toolchain is cached under
   `/var/cache/outhaul/` between updates, and downloaded again only when
   `go.mod` asks for a different version. Build with the commit stamp. Then
   pull the container images the new build runs (Traefik, cloudflared), which
   the new binary lists itself (an `outhaul images` command, say). If any of
   this fails, finish with "build failed". Nothing on the server has changed
   at this point.

   Pulling first keeps a slow image download out of the health window: at
   boot, Outhaul reconciles Traefik before `/healthz` answers. *(Design's
   call.)*
4. Stop `outhaul`. Keep the running binary as `outhaul.previous`, and copy the
   database (`outhaul.db`, plus `-wal` and `-shm` if present) to a snapshot in
   the `update` directory.
5. Install the new binary, and the new tree's `outhaul.service`, updater
   units and updater script. Replace files by rename, never by writing in
   place: the shell is still reading the running updater script.
   `daemon-reload`, then start `outhaul`.
6. Wait up to 60 seconds for the admin port's `/healthz` to answer, and check
   that the process answering reports the new commit.
7. Healthy: finish with "updated from X to Y". The previous binary and the
   database snapshot stay until the next update replaces them.
8. Not healthy: stop `outhaul`, and put back the previous binary, the database
   snapshot and the previous unit files. Start it, check its health, and
   finish with "rolled back", the reason, and the last lines of the new
   version's journal. If the old version does not come back either, finish
   with "failed" and the exact commands to recover over SSH.

### 6. The UI

An **Updates** panel on the Settings page shows the running commit (short
hash, date, first line), the check result and the commit list, and an
**Update** button. The button:

- is disabled while a deploy, backup or restore is running, saying why
  (shutdown would abort the deploy or cancel the backup);
- asks for confirmation, saying that Outhaul restarts, the admin UI is gone
  for a minute or so, and apps keep running. GitHub pushes that arrive during
  the restart are not deployed; redeploy those apps by hand;
- then shows the updater's log live.

The connection drops when Outhaul restarts. The page keeps polling until the
admin UI answers again, then shows the result, which the restarted Outhaul
reads from the status file. The last update's result stays on the panel until
the next update.

Every endpoint uses the existing authentication and cross-origin protection.
A server-sent log stream waits on `s.streamContext(r)` (see AGENTS.md).

### 7. Installer and docs

- The installer stamps the commit when it builds. It installs or refreshes
  the updater script and units, enables the path unit, and creates the
  `update` directory and its marker.
- Re-running the installer once is how an existing server (such as the
  maintainer's Azure test server) gets the feature.
- README: the Updates panel, and how a server gets it. MANUAL-DEPLOY: that
  updating from the UI needs the installer's updater units, and how to
  install them by hand.

## Left to the builder

- File names, the status file's format, and the marker's contents.
- How the updater is factored out of `bootstrap.sh`: a flag on it, or a
  separate script sharing its functions. It stays POSIX `sh`, tested like the
  installer.
- The compare API client: small, cached, unauthenticated, faked in tests.
- How the panel spots a Traefik change in the comparison.
- How the health check confirms the new commit, e.g. a header on `/healthz`.
- The panel's layout, within the Settings page's existing style.

## Testing

Test first, as AGENTS.md asks.

- **Go:**
  - reading the stamped commit, and the unknown-build case;
  - the compare client against an `httptest` GitHub, covering every panel
    state and both warnings;
  - the request file is written only by an authenticated POST, and refused
    while a deploy, backup or restore runs;
  - the panel after a restart shows each result.
- **Updater** (`deploy/test`, stubbing `systemctl`, `git`, `go`, `curl` and
  `sleep` the way the firewall tests do):
  - the request is deleted first;
  - an up-to-date server makes no changes;
  - a failed build makes no changes;
  - an unhealthy new version gets the previous binary and the database
    snapshot back;
  - files are replaced by rename.

  Run under `dash`, `bash --posix` and `busybox sh`.
- **Real systemd:** the path unit firing, and a full update and a forced
  rollback, on a real host (a VM, or the maintainer's test server). This
  depends on how systemd really behaves, which the stubs cannot show.
