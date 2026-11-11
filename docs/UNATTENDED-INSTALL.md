# Unattended installs

`deploy/bootstrap.sh` normally asks every question on a terminal. With
`--non-interactive` it takes every answer from a flag instead: it never prompts,
never reads stdin, and names every missing or malformed flag before it changes
anything. That is the mode for cloud-init, an image build, or a
configuration-management run.

The wizard is unchanged. A run without `--non-interactive` asks the same
questions it always has, and a flag that is an answer to one of those questions
is refused with exit 2 unless `--non-interactive` is also given.

This page covers the unattended flags, where the answers go, what a re-run does,
how to pin a version, and how to read a failure. For the proxy settings that
`--ingress proxy` switches on, see [BEHIND-A-PROXY.md](BEHIND-A-PROXY.md).

## Before you start

- Run as root. The installer needs systemd, and V1 targets Debian and Ubuntu.
  An unsupported distro is exit 1 in this mode; there is no one to ask.
- Have a working Docker. A Docker that answers `docker version` and
  `docker compose version` is used as it is: nothing is installed, enabled or
  changed. If Docker is absent, the run exits 1 unless you pass
  `--install-docker`. If Docker is present but not working, the installer
  starts its service once and then reports what is missing; it never runs the
  Docker install script over an installed Docker.
- x86_64/arm64 only, as for the wizard. A temporary swapfile is added and
  removed when RAM and swap together are under 2 GiB.
- `git` is installed with apt when it is missing. The pinned nixpacks is
  installed when `--nixpacks` is on or left out, and no nixpacks is present.
- apt must not ask questions: the run sets `DEBIAN_FRONTEND=noninteractive`.

## Flags

| Flag | Value | Needs `--non-interactive` | When left out |
|---|---|---|---|
| `--non-interactive` | | | the wizard asks every question |
| `--ingress` | `letsencrypt`, `tunnel`, `local`, `local-ca`, `proxy` | yes | exit 2 |
| `--public-url` | a URL | yes | exit 2 for `letsencrypt` and `proxy`; no line for the others |
| `--acme-email` | an address | yes | exit 2 for `letsencrypt` and `proxy`; giving it with the others is exit 2 |
| `--git-ssh` | `off` or a port from 1 to 65535 | yes | `2222` |
| `--nixpacks` | `on` or `off` | yes | `on`, without replacing a different installed version |
| `--firewall` | `leave` or `ufw` | yes | `leave` |
| `--updates` | `main` or `off` | yes | no line is written |
| `--install-docker` | | yes | Docker must already be there |
| `--setup-url-file` | a path | yes | no file is written |
| `--env-file` | a path; repeatable | no | none |
| `--ref` | a tag, a branch, or a full 40-character commit | no | `OUTHAUL_REF`, then the remote's `HEAD` |
| `--from-checkout` | a directory | no | the installer fetches the repository |
| `--verbose` | | no | the log is not mirrored |
| `-h`, `--help` | | no | |

- Every flag is checked before anything is installed. All problems are printed
  together and the run exits 2, leaving the host untouched. A flag with no value
  is one of those problems.
- `--ref` cannot be used with `--from-checkout`; a local checkout always builds.
- `--env-file` must be an absolute path to a regular file, owned by root, that
  group and others cannot write. A path containing a newline, a double quote or
  `%` is refused, because it cannot be written into a systemd
  `EnvironmentFile=` line.
- `--acme-email` is refused with `tunnel`, `local` and `local-ca`: there is no
  ACME account to register. `--public-url` is accepted with them, and written
  if given.
- A flag left out takes the wizard's default, except the ingress (required) and
  the firewall (left alone).

### What replaces the prompts

| Prompt in the wizard | In an unattended run |
|---|---|
| Unsupported distro: continue anyway? | exit 1 |
| DNS does not point here (`letsencrypt` only) | a warning, then it goes on; not checked for `proxy` |
| Low memory: add a temporary swapfile? | yes, and it is removed after the build |
| Remove the Go toolchain? | yes |
| Configure the firewall? | not asked; see `--firewall` below |
| apt's own questions | `DEBIAN_FRONTEND=noninteractive` |

## Ingress

| `--ingress` | What it writes to `install.env` | What it needs |
|---|---|---|
| `letsencrypt` | `OUTHAUL_PUBLIC_URL`, `OUTHAUL_ACME_EMAIL` | `--public-url`, `--acme-email` |
| `tunnel` | nothing beyond the common lines | nothing |
| `local` | nothing beyond the common lines | nothing |
| `local-ca` | `OUTHAUL_LOCAL_CA=true` | nothing |
| `proxy` | `OUTHAUL_PUBLIC_URL`, `OUTHAUL_ACME_EMAIL`, `OUTHAUL_HTTP_PORT=off`, `OUTHAUL_ADMIN_DIRECT=off` | `--public-url`, `--acme-email`, and a non-empty `OUTHAUL_ACME_DNS_PROVIDER=` line in an `--env-file` |

Every ingress also writes `OUTHAUL_SSH_ADDR=:<port>` or `OUTHAUL_SSH_ADDR=off`,
and writes `OUTHAUL_UPDATES` when `--updates` is given. `--public-url` writes
`OUTHAUL_PUBLIC_URL` whenever it is given.

`proxy` is the ingress for a TLS-terminating CDN in front of the host. It is
not offered by the wizard. Its two switches are checked after the install: an
Outhaul that predates them ignores them without a word, so a run that leaves the
admin port on a wildcard address or Traefik publishing port 80 is a failure, not
a success. See [BEHIND-A-PROXY.md](BEHIND-A-PROXY.md) for what the settings do.

## Where the answers go

| File | Who writes it | What it holds |
|---|---|---|
| `/etc/outhaul.env` | the wizard, once; people after that | the ordinary `OUTHAUL_*` overrides. An unattended run never writes it |
| `/etc/outhaul/install.env` | each `--non-interactive` run, when its content differs | the lines above, under a comment saying each run rewrites it. `root:root`, mode 0600 |
| each `--env-file` | whoever builds the host | everything else. The installer never changes or parses it, apart from the one `proxy` provider check |
| `/etc/systemd/system/outhaul.service.d/10-install.conf` | a run with `--non-interactive` or `--env-file` | `EnvironmentFile=` lines for this run's files |
| `/etc/systemd/system/outhaul-update.service.d/10-install.conf` | the same | the same lines, for the root updater |

The drop-ins name `install.env` first (in a non-interactive run), then each
`--env-file` in the order it was given. systemd loads the unit's own
`/etc/outhaul.env` first, then the drop-in's files, and a later file wins. So
the order is `/etc/outhaul.env`, then the flags, then each `--env-file`. A line
in `/etc/outhaul.env` that no flag covers keeps applying.

The paths in the drop-in have no `-` prefix. A missing env file stops the
service instead of starting it with half its settings. A run with neither
`--non-interactive` nor `--env-file` leaves both drop-ins alone, so a plain
`curl … | sh` on such a host does not drop its env files.

Editing an env file restarts nothing, and a re-run does not notice. After
changing one, run `systemctl restart outhaul`.

## What a re-run does

The installer compares the commit it is about to install (the target) with the
commit `/usr/local/bin/outhaul version` reports (the installed version). It
also reads `/var/lib/outhaul-updater/installed`, the marker the last healthy run
wrote.

| Situation | What happens |
|---|---|
| No binary yet | fetch, build, install, start, check |
| Same target commit, and the marker names it | no fetch and no build. In a non-interactive run, `install.env` and the drop-ins are brought into line; the service is started if it is not active, restarted only if `install.env`, a drop-in or a newly written `/etc/outhaul.env` changed, otherwise left alone. The health checks still run |
| A different target commit | fetch, build, and the careful swap described below |
| Same commit but no marker (a build from before the marker, or a failed earlier run) | fetch and build again |

`--from-checkout` always builds. With no `--ref`, the target is the remote's
`HEAD`; with `OUTHAUL_REF` or `--ref`, it is that ref. A commit ref with
nothing to do needs no network.

A wizard re-run never writes `install.env` and, with no `--env-file`, leaves
the drop-ins alone. It still follows the same table, so it no longer rebuilds an
unchanged `main`.

## Upgrading and rollback

An upgrade by re-run is the same careful swap the Updates panel uses:

1. Pull the images the new binary needs, before anything stops.
2. Stop Outhaul and keep a copy in `/var/lib/outhaul-updater/previous`: the
   binary, `outhaul.service`, the database with its `-wal` and `-shm` files,
   `install.env`, and the two install drop-ins.
3. Install the new binary and unit, write `install.env` and the drop-ins, and
   start the service.
4. Wait for up to 60 attempts for `/healthz` on loopback to report the new
   commit.
5. On success, write the marker and leave the previous copy in place for the
   next upgrade.

If the new version does not answer, the run stops it, puts every file from
`previous/` back, starts the old version and checks that it answers. A file the
copy does not have is removed: an `install.env` or drop-in the old binary did
not have, and the new version's `-wal`/`-shm` files, which SQLite would
otherwise replay onto the old database. The database goes back to the copy taken
at the stop, so anything the failed new version wrote is lost too. The marker is
not updated, so the next run tries the target again.

In this mode, a rolled-back upgrade exits 1 and says it rolled back. If the old
version does not come back either, the output says so and prints the commands
that put the copied files back by hand. A successful upgrade leaves the
previous copy in `previous/` until the next upgrade replaces it.

Migrations only go forward. Installing an older commit over a database a newer
one has opened is not detected: the old binary starts and may misbehave. Install
the same commit or a newer one. To go back, restore the database and the binary
together.

## Output, exit codes and the log

| Exit code | Meaning |
|---|---|
| 0 | installed, or nothing to do |
| 1 | something failed; the log says what, and whether the old version is back |
| 2 | bad flags; nothing changed |

In this mode a health check that never passes is exit 1; the wizard keeps its
old completion screen and exit code.

A non-interactive run appends to `/var/log/outhaul-install.log`, mode 0600. Its
first line is the UTC time and the arguments (no flag takes a secret). It then
records each `ok`, `fail`, `note` and `die` line, and ends with `OK installed` or
`FAILED exit <code>`. A log that cannot be written does not stop the install.
The wizard still truncates the log, and its step lines go to the terminal.

The run's stdout is plain, without colour, because under cloud-init it is copied
to cloud-init's own log and the serial console.

## The setup URL

Until the first account exists, Outhaul prints a one-time setup URL to its
journal at each start, with a new token each time. An unattended run does not
print it: its output would land in cloud-init's log and on the console. Read it
from the journal instead:

```sh
journalctl -u outhaul | grep -i setup
```

With `--setup-url-file PATH`, the run also writes the URL of the service's
current start to `PATH`, `root:root`, mode 0600, by rename. If this run started
the service and that start printed no URL, setup is already done and the file is
removed. A later restart prints a new token and makes the file stale; re-run the
installer with the flag, or read the journal, to get the current URL.

The URL never goes to the install log. A failed health check prints the
journal's last lines with any line carrying `/setup?token=` replaced.

## Pinning a version: `--ref`

`--ref` accepts:

- a full 40-character commit, which is used as it is;
- a tag or branch name, resolved with `git ls-remote` against the repository.
  A tag wins over a branch of the same name, and an annotated tag resolves to
  the commit behind it;
- nothing, which means `OUTHAUL_REF`, and then the remote's `HEAD`.

A shortened commit is refused: git cannot fetch one. A name that matches no tag
or branch is an error. The log's fetch line names the commit the ref resolved
to (in short form); `/var/lib/outhaul-updater/installed` records the full
commit, the ref and the tag's name.

A tag can move. A run that names a tag installs whatever that tag points at when
it runs, so a moved tag changes what a re-run installs. **For production, pin a
full 40-character commit.** A commit cannot move, and `--ref` with a commit
needs no network when that commit is already installed.

The repository is `OUTHAUL_REPO`, by default the upstream repository. The
installer itself should be fetched at the commit it installs, so the script root
runs is as pinned as what it builds.

## Cloud-init example

The infrastructure side writes Outhaul's own names, as literal lines, in the
file it already writes:

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

Outhaul does not read the contract's `AZURE_FDID` or `SE_WILDCARD_DOMAIN`. Their
values go under the last three names. Then cloud-init runs:

```sh
ref=0123456789abcdef0123456789abcdef01234567  # a full 40-character commit
# Fetch the installer at the commit it will install. Replace the example host
# with wherever you serve the installer.
curl -fsSL "https://example.com/outhaul/$ref/deploy/bootstrap.sh" \
  -o /root/outhaul-install.sh
sh /root/outhaul-install.sh --non-interactive --ref "$ref" \
  --ingress proxy --public-url https://outhaul.shared.demo.example.com \
  --acme-email ops@example.com --env-file /etc/outhaul/azure.env \
  --git-ssh off --updates off
```

`install.env` then holds the public URL, the email, `OUTHAUL_HTTP_PORT=off`,
`OUTHAUL_ADMIN_DIRECT=off`, `OUTHAUL_SSH_ADDR=off` and `OUTHAUL_UPDATES=off`.
The installer is fetched at the commit it installs, so the script root runs is
as pinned as what it builds. If it is a file and differs from
`deploy/bootstrap.sh` in the tree it fetched, it logs a warning.

## Docker, nixpacks and ufw

**Docker.** A working Docker is used as it is. Absent means exit 1 unless
`--install-docker` is given. Present but broken means one `systemctl enable
--now docker` and 30 more seconds, then exit 1 naming what is missing. The
Docker install script never runs over an installed Docker.

**nixpacks.** The installer pins nixpacks `1.41.0` and checks the release
archive's SHA-256 before installing it. The amd64 digest is
`0f55de7874507b9cf7502113120bd96f2ab6979f78d10eaf2eb2ade9207b3af6`; arm64 is
`912bd02dd2bb6f9c3a9ed965fe8a68b4aa318dc7a2546e2eca6f2806a894ba39`. A wrong
checksum is fatal in every mode.

| nixpacks on the host | `--nixpacks on` | `--nixpacks off` | Flag left out |
|---|---|---|---|
| none | the pin is installed | nothing | the pin is installed |
| the pin | nothing | nothing | nothing |
| another version | replaced by the pin | nothing | left, with a note naming the pin |

A failed download is exit 1 in a non-interactive run. The wizard keeps its old
behaviour: a note, and Dockerfile and compose apps still work.

**ufw.** `--firewall leave` is the default: the run does not install, enable or
change ufw, and does not run `apt-get install ufw`. `--firewall ufw` opens the
ports the ingress needs (SSH always), plus the admin port from Outhaul's Docker
bridge. Before anything is installed, a run that would enable ufw over a port
that is listening now exits 1 instead of cutting it off.

## Updates on the host

`OUTHAUL_UPDATES` decides what the Updates panel, the background check and the
root updater do. `--updates off` writes `OUTHAUL_UPDATES=off` into
`install.env`; on a successful run the installer then disables
`outhaul-update.path`. The panel shows the running version and says the server
is upgraded by re-running the installer. No check leaves the server, and the
updater removes any request and changes nothing. `--updates main` writes the
default explicitly. Leaving the flag out writes no line, so `/etc/outhaul.env`
or the default `main` applies.

With updates off, upgrade by running the installer again with a newer `--ref`.
The updater's own refusal is independent of the installer: an env file edited
by hand to `off` still stops it.

## What only the real host can show

These are worth checking on a fresh machine, because the unit tests fake
systemd, Docker and the journal:

1. cloud-init runs the command above: exit 0, the install log ends with `OK`,
   and cloud-init's own log has no setup URL.
2. `outhaul version` prints the pinned commit, and so does
   `/var/lib/outhaul-updater/installed`.
3. `systemctl show outhaul -p EnvironmentFiles` lists `install.env` and
   `azure.env`. `azure.env` is still `root:root 0640`.
4. `command -v ufw` finds nothing. `docker version` is what cloud-init
   installed, and the log shows no Docker install.
5. `ss -ltn` shows nothing on 2222, and the admin port on loopback and one
   bridge address only.
6. The same command again exits 0 in seconds, and
   `systemctl show outhaul -p ActiveEnterTimestamp` has not moved.
7. The command with a newer `--ref` upgrades. The old version is in
   `/var/lib/outhaul-updater/previous`.
8. Settings → Updates shows the version and that updates are off. Creating
   `/var/lib/outhaul/update/request` as the `outhaul` user changes nothing.
9. The URL from `journalctl -u outhaul`, or from the setup file, opens through
   the CDN and creates the first system admin.
10. After a reboot the service is active with both files loaded.
11. A Nixpacks app builds with the pinned nixpacks.

## If it fails

- Exit 1 and no log line: the log could not be written; the terminal output
  still says what failed.
- A failed health check: the log has the journal's last lines with the setup URL
  withheld, and says whether the old version is back.
- `the proxy's switches did not take effect`: the installed Outhaul does not
  honour `OUTHAUL_HTTP_PORT=off` and `OUTHAUL_ADMIN_DIRECT=off`. Install a
  version that does; the run rolls an upgrade back, or stops a service it
  started.
- A service that will not start: `journalctl -u outhaul -n 50 --no-pager`.
  `install.env` and the drop-ins are on disk, so the journal names the setting
  that was refused.
- Updates off but a request file appears: the updater removes it and changes
  nothing; `journalctl -u outhaul-update` says so.
