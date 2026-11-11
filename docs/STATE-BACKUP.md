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
outhaul restore-state --from <file> [--key-file PATH]
```

`--from` is the downloaded `.db.gz` or a plain SQLite file; the first bytes say
which. The command fills an **empty** install only. It refuses to touch one
with an account, and it refuses while another process has the database open,
so stop Outhaul first. There is no `--force`.

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
