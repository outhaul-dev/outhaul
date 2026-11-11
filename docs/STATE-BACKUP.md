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
