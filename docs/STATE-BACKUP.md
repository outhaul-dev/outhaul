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
