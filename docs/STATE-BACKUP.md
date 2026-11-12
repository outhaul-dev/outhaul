# Backups to Azure Blob, and restoring Outhaul's own state

Outhaul can use an Azure Blob Storage container as a backup destination,
authenticated with the host's managed identity. No account key or SAS token is
stored anywhere. A later part of this feature copies Outhaul's own database to a
destination and puts it back on a rebuilt host; this page grows with each part.

The Azure destination is not entered on Settings. It belongs to the host's
environment: `outhaul serve` keeps one row named **Azure Blob** in step with the
two settings below. That row is what a database or volume schedule points at.

## Settings

| Variable | Values | Default |
| --- | --- | --- |
| `OUTHAUL_BACKUP_AZURE_ACCOUNT` | the storage account name: 3 to 24 lowercase letters and digits | unset |
| `OUTHAUL_BACKUP_AZURE_CONTAINER` | a container name: 3 to 63 lowercase letters, digits and single hyphens, starting and ending with a letter or digit | unset |
| `AZURE_CLIENT_ID` | the managed identity's client ID, a GUID; only when the VM has more than one identity | unset |

Set both `OUTHAUL_BACKUP_AZURE_*` variables or neither. Outhaul refuses to start
if only one is set, or if either value is malformed, and names the variable in
the error. `AZURE_CLIENT_ID` is not an `OUTHAUL_*` setting; it is read from the
environment like the settings are.

A first deployment's environment might look like this:

```sh
OUTHAUL_BACKUP_AZURE_ACCOUNT=examplebackups
OUTHAUL_BACKUP_AZURE_CONTAINER=outhaul
OUTHAUL_STATE_BACKUP_SCHEDULE=15 2 * * *
```

`OUTHAUL_STATE_BACKUP_SCHEDULE` is described in the next part; an install that
sets only the Azure pair can still use the destination for database and volume
schedules.

## The identity and the container

The VM needs a managed identity. Grant that identity the **Storage Blob Data
Contributor** role on the container (not on the whole account, unless the
container is all it should reach). Outhaul uses the identity for reads, writes,
listing and deletes, so that one role is enough.

The container must be **private**. A backup archive is not encrypted: sealed
values stay sealed inside it, but everything else is as readable as it is on the
host's disk, password hashes included. Do not put it behind a public access
level.

If the storage account has a firewall, it must admit the VM's network. When the
identity lacks the role, or the firewall refuses the host, Test and every run
say which of the two to fix.

## The Settings row

With the pair set, Settings shows one row named **Azure Blob**:

- the storage account and container;
- "managed identity" in place of a region;
- Test, which writes and deletes a small probe object.

The row has no remove button while the settings are set, and the delete route
refuses it with a 409. Unset the settings to remove it.

If the pair is unset while a database or volume schedule still points at the
row, Outhaul keeps the row and clears its account and container. Its Test and
runs then fail with `OUTHAUL_BACKUP_AZURE_ACCOUNT is not set`. Remove the
schedules, and the row goes away at the next start.

The add form on Settings is unchanged: destinations added there are
S3-compatible ones, with an access key and a secret.

## Scheduled copies of Outhaul's own state

A schedule copies `outhaul.db` — apps, domains, variables, accounts, SSO
settings — to a destination of either kind. The copy is made while Outhaul
runs and is consistent; it leaves out every sign-in session, protected-route
session and one-time code, so everyone signs in again after a restore.

It does **not** include `secret.key`, the `git/` repositories, app volumes,
managed database data, `acme.json`, the local CA's key or the SSH host key.
Each of those has its own story; the restore section says what comes back and
what does not.

| Variable | Values | Default |
| --- | --- | --- |
| `OUTHAUL_STATE_BACKUP_SCHEDULE` | a five-field cron expression in server-local time | unset: no state copies |
| `OUTHAUL_STATE_BACKUP_DESTINATION` | the name of a destination on Settings | unset: the Azure row |
| `OUTHAUL_STATE_BACKUP_KEEP` | a whole number; `0` keeps every copy | `30` |
| `OUTHAUL_STATE_BACKUP_PREFIX` | a key prefix: letters, digits, `-`, `_`, `.` and `/`, with no `/` at either end | empty |

The schedule needs a destination: either `OUTHAUL_STATE_BACKUP_DESTINATION`, or
the Azure pair above. A destination name that matches nothing is a warning at
startup and the failure reason of every run, not a refusal to start; add the
destination in Settings and the next run finds it.

`OUTHAUL_STATE_BACKUP_PREFIX` keeps hosts that share a container apart. A copy
lands at `<prefix>/_outhaul-state/<stamp>.db.gz`, where the stamp is UTC in
`20060102T150405Z` form. No app or database can be named `_outhaul-state`, so
this directory cannot collide with a schedule's objects.

Retention keeps the newest `OUTHAUL_STATE_BACKUP_KEEP` copies directly in that
directory; `0` keeps them all.

An install with no account is never copied, on schedule or by Run now: the
installer starts an empty Outhaul on a rebuilt host before anyone restores, and
that empty state must not become the newest copy. The skip is logged as
`state backup: skipped: no account yet`.

The copy runs through the same scheduler as database and volume backups, so an
update in progress holds it back, and stopping Outhaul cancels it. The outcome
is logged (`WARNING` on failure) and kept as the last state-backup outcome shown
on Settings.

### The State backup panel

Settings shows a **State backup** panel only when `OUTHAUL_STATE_BACKUP_SCHEDULE`
is set. It lists:

- the schedule, in server-local time;
- the destination — the configured name, or **Azure Blob** when the schedule
  uses the environment's row;
- how many copies are kept;
- the key prefix, or a dash when there is none;
- the last run: its time and, on success, the object key and size; on failure,
  the reason.

**Run now** starts a copy immediately. It is a system-admin action, it does
nothing while an update is in progress, and it is skipped if a state copy is
already running.

## Restoring onto a rebuilt host

```
outhaul restore-state --from <file | latest | STAMP> [--key-file PATH]
outhaul restore-state --list
```

`--from` is a downloaded `.db.gz`, a plain SQLite file, `latest`, or a stamp
such as `20261111T021500Z`; the first bytes of a file say whether it is gzip or
SQLite. `latest` and a stamp are read from the Azure container the environment
names, under `OUTHAUL_STATE_BACKUP_PREFIX`. `--list` prints the stamps there,
one per line, oldest first. Without the Azure pair these say to pass a file.
Anything else is a path; a file actually named `latest` is `./latest`.

The command fills an **empty** install only. It refuses to touch one with an
account, and it refuses while another process has the database open, so stop
Outhaul first. There is no `--force`.

Run it with the service's environment, so it sees the same Azure pair, prefix
and schedule as `outhaul serve`:

```sh
systemd-run --wait --pipe --collect \
  -p EnvironmentFile=/etc/outhaul.env \
  -p EnvironmentFile=/etc/outhaul/azure.env \
  /usr/local/bin/outhaul restore-state --from latest \
  --key-file /root/secret.key
```

Once the unattended installer lands, that list also needs
`/etc/outhaul/install.env` and any `--env-file` the installer used.

### The key

`--key-file` names a file holding the 32 raw bytes of `secret.key`. Without
it, the `secret.key` already in the data directory is used. The binary parses
no encodings; to keep the key as text, encode and decode it around the copy:

```sh
base64 -w0 /root/secret.key > /root/secret.key.b64
base64 -d /root/secret.key.b64 > /root/secret.key
chmod 600 /root/secret.key
```

The supplied file is left where it is. The key is proved against every sealed
value in the staged copy before anything is replaced.

### What it refuses

Each refusal leaves the host's database and key as they were, and exits
non-zero.

| Case | The message says |
| --- | --- |
| No data directory | install Outhaul first |
| Outhaul is running | stop Outhaul first |
| The install has an account | this fills an empty install only; move the old database and key away yourself |
| Not a backup | not an Outhaul state copy, with what was wrong |
| From a newer Outhaul | install that version or newer, then restore |
| No key, the wrong key, or a key that is not 32 bytes | pass the original with `--key-file`. The key the fresh install made for itself can't work |

### What it does

1. Checks the data directory, and that no other process has the database open.
2. Unpacks the copy to `outhaul.db.restoring`, mode 0600, replacing one left
   by an earlier attempt — including its `-wal` and `-shm`. Checks it, and
   refuses a newer schema.
3. Opens it with the key. Migrations run on the staged file, so a copy from an
   older Outhaul is brought forward here; a migration failure leaves the host
   untouched.
4. Opens every sealed value with the key.
5. Removes the session rows again, for a file the state backup didn't make,
   and writes the first-start marker. Closes the file and refuses if a `-wal`
   survives that close.
6. Checks the database is still idle, then swaps **key first**: a differing
   `secret.key` moves to `secret.key.before-restore-<stamp>`, and the supplied
   key is written. Then `outhaul.db`, and any `-wal` or `-shm`, move to
   `*.before-restore-<stamp>`, and the staged file becomes `outhaul.db`.
   Nothing is deleted; an existing `.before-restore-<stamp>` gains a `-2`,
   `-3`, … suffix instead of being overwritten.
7. Prints the stamp, the account, app and database counts, how many sealed
   values opened, which migrations ran, what moved aside, and what the first
   start will do.

The running-server check is a guard against a forgotten `systemctl stop`, not
a lock: nothing holds off a start in the instant between the last check and the
rename. A `-wal` left beside a database that is renamed away would be replayed
into the new file, which is why the sidecars move with it.

### The first start after a restore

`serve` already fails deployments and databases that were in flight, re-creates
the Docker network and Traefik, and sweeps previews whose pull request has
closed. When the marker is present, a pass then runs once, after that and after
the deploy worker has started. It is retried each minute until it has run to its
end; a failure inside one step is recorded, not retried.

It only creates what is missing:

1. **Images.** A deployment row whose `outhaul/…` image is not on this host is
   marked pruned, so no rollback offers it.
2. **Managed databases.** Each row with status `running` and no container is
   provisioned again with the same name, user and password, and an empty data
   directory, before any app. Rows that are `stopped` or `failed` are left.
3. **Apps.** Each app with a live deployment and no container of its own gets
   the deploy a protection change would queue: a kept image is started again,
   and without one the app is built from source. Ordinary apps go first, then
   previews. A push-deploy app whose repository is missing is skipped and
   listed.
4. The marker becomes done, with what was re-created, queued, failed, or waits
   for a push. Settings shows the same to system admins until one dismisses it.

Left to a person: restoring each database's and volume's data from its
schedule's Restore page, which needs the database running and the volume
present, so after the pass; and pushing push-deploy apps again.

Whether an app was stopped is read from Docker, not stored, so an app stopped
at backup time is deployed again. An app comes back at its branch's head, not
at the commit that was running, unless its image was kept.

### What a rebuild loses

| What | In the state backup | After a restore |
| --- | --- | --- |
| `outhaul.db` | yes, without sign-in sessions | put back by `restore-state` |
| `secret.key` | never | supplied by a person |
| Sign-in sessions, protected-route sessions, one-time codes, push tokens | no: removed from the copy | everyone signs in again and creates new push tokens |
| `traefik/acme/acme.json` (ACME account, certificates, private keys) | no | Traefik orders again: one wildcard under R1, otherwise one order per host, which a host with many names must weigh against Let's Encrypt's limits |
| `traefik/dynamic`, `traefik/certs`, `work/`, `update/`, `git-hook.sock` | no | written again at startup |
| `ca/` (the local CA's root and key) | no: a private key | a new root at first start, which devices must trust again, unless the old directory is copied back first |
| `git/<app>.git` (push-deploy repositories) | no | the next `git push` re-creates the repository and deploys. Until then the app is not redeployed |
| `ssh_host_ed25519_key` | no: a private key | a new host key; people who push see a host-key warning once |
| `databases/<name>/` (managed database data) | no | from each database's own schedule. One with no schedule, and every Redis, comes back empty |
| App volumes | no | from each app's own schedule, or empty |
| Images, containers, the network, Traefik | no | `ensureInfra` re-creates the network and Traefik; images are rebuilt or pulled |
| Build logs | in memory only | lost, as at any restart |
| The binary, the unit, `/etc/outhaul.env`, `/etc/outhaul/azure.env` | not Outhaul's state | the installer and the infrastructure write them |

### The steps a person follows

1. Build the host and install Outhaul as usual: the version that made the
   backup or a newer one, with the same environment files. The installer
   starts it. Don't create an account.
2. `systemctl stop outhaul`.
3. Put the key on the host, for example `/root/secret.key`, mode 0600.
4. Run the restore with the service's environment:

   ```
   systemd-run --wait --pipe --collect \
     -p EnvironmentFile=/etc/outhaul.env \
     -p EnvironmentFile=/etc/outhaul/azure.env \
     /usr/local/bin/outhaul restore-state --from latest \
     --key-file /root/secret.key
   ```

   With a file downloaded by hand, give its path in place of `latest`. If it
   refuses, nothing has changed.
5. `systemctl start outhaul`, and follow `journalctl -u outhaul -f`.
6. Sign in again. Settings lists what still needs its data.
7. For each database and app with a schedule, open its Restore page and
   restore the newest archive. Restart an app that started without its data.
8. Push each push-deploy app again, accepting the new host key.
9. Remove the key copy (`shred -u /root/secret.key`) and, once all is well,
   the `*.before-restore-*` files in the data directory.
10. Press Run now on the State backup panel, and see a new copy arrive.

### What only the real host can show

For the infrastructure side to run on the staging host. It goes in the pull
requests and here.

1. Test on the `Azure Blob` row succeeds. With the role taken away it names
   the role; with a container that doesn't exist it says so.
2. Run now on the State backup panel puts `_outhaul-state/<stamp>.db.gz` in
   the container, and the panel shows its size.
3. A database schedule and a volume schedule on `Azure Blob` run. An archive
   larger than 8 MiB restores from its Restore page.
4. A run more than a day after the first succeeds: the token was refreshed.
5. On a machine that isn't an Azure VM, with the pair set, Test fails within
   seconds and names the metadata service.
6. `restore-state` refuses with Outhaul running, and with a wrong key, and
   leaves the database and key as they were.
7. The drill: a new VM from an empty disk, the same environment files, the
   steps above. The apps answer again, `acme.json` holds one wildcard, and a
   database and a volume restore from their archives.
8. The next night's state backup appears, and the empty install made before
   the restore uploaded nothing.
