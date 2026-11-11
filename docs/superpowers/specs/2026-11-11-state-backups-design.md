# Backups to Azure Blob, and restoring onto a rebuilt host

An Azure Blob destination, a scheduled copy of Outhaul's own database, and a
command that puts that copy back on a fresh host.

This covers requirement R13 of the brief "outhaul enhancements: running behind
Azure Front Door on a shared host" (10 November 2026), in three parts that can
each be their own pull request. R1 to R4 are designed in
`2026-11-11-proxy-ingress-design.md`. Code references are to `06150cc`.

## Why

The shared host is an Azure VM that is rebuilt from an empty disk. It has a
managed identity and no stored cloud credentials. Today:

- A backup destination is S3-compatible storage with an access key and a
  secret (`internal/blobstore`, the `destinations` table). Azure Blob Storage
  can't be used, and nothing can authenticate without a stored key.
- Schedules back up managed databases and app volumes (`internal/backup`).
  Nothing backs up `outhaul.db`: the apps, domains, variables, accounts and
  SSO settings. A rebuild loses all of it.
- Sealed values in that database open only with `<data dir>/secret.key`.
  `openStore` (`main.go`) rightly refuses to make a new key over them, but
  nothing puts a database and its key back together on a new host.

## Goals

1. A backup destination in Azure Blob Storage, authenticated with the host's
   managed identity. No account key and no SAS, anywhere.
2. Existing database and volume schedules can use it. Test, Run now, retention
   and Restore work as they do for S3.
3. A scheduled, consistent copy of `outhaul.db` to a destination of either
   kind, with retention.
4. `outhaul restore-state` puts a copy back on a fresh install, given the key
   file. It proves the key fits before it replaces anything, and refuses
   whatever would lose data.
5. The first start after a restore re-creates what it can without a person:
   managed databases, empty, and apps. It says what is left to do.
6. Each part is switched on by its own setting, or by running the command.

## Non-goals

- Keeping, fetching or rotating `secret.key`. Outhaul never uploads it.
- Encrypting the copy. Sealed values stay sealed inside it. Everything else
  is as readable in it as on the host's disk, password hashes included, so
  the container must be private.
- Account keys, SAS tokens, other Azure clouds, or a custom endpoint.
- Azure destinations entered on Settings, or more than one (question 1).
- Restoring database and volume data automatically (question 2).
- Backing up anything in the data directory but the database (question 3, and
  the table in [What a rebuild loses](#what-a-rebuild-loses)).
- Restoring over an install that is in use, or merging two databases.
- Fetching the copy from an S3 destination in `restore-state`: on a fresh host
  that destination's keys are inside the database being restored.
- A state backup before each update, point-in-time recovery, replication.
- Changing the S3 client. Its whole-upload timeout stays an open problem.

## Anti-goals

The work has failed, even if it runs, if any of these happen:

- An install that sets none of the new settings notices anything: a new row or
  panel on Settings, a request to `169.254.169.254`, a step at startup.
- `secret.key`, an access token or a sign-in session ends up in the container
  or in a log.
- `restore-state` replaces a database that is in use, or leaves a host half
  done: a new database under the old key, or a stale `-wal` beside a new file.
- A wrong key is found at the next start, not before anything is replaced.
- An upload still moving bytes is ended by a timer, or a failed one passes.
- A freshly installed, empty host uploads its state and becomes "the latest".
- The first start after a restore removes or overwrites anything on the host.

## Constraints

From `ARCHITECTURE.md`, `AGENTS.md` and the brief:

- Settings are `OUTHAUL_*` variables read through the `parser` in
  `internal/config/config.go`; `Validate` refuses bad values and combinations.
- No SDK. The S3 client is hand-rolled, and so is this one.
- Outhaul runs on the host, not in a container, so R8's block on the metadata
  address for containers doesn't affect it. cloud-init carries no secrets.
- Test first, with the existing fakes; outside services are faked with
  `httptest`. The repository is public: `example.com` names only.

Two kept rules are touched, and neither is overridden. The sealed-list
convention in `AGENTS.md` changes shape: one list of columns in place of
`sealedQueries`. And `secret.key` is still never replaced over values sealed
with it: `restore-state` moves a key aside only with the database it belongs
to. `ARCHITECTURE.md` names "non-S3 destinations" as a seam; Part 1 fills it.

## Settings

| Variable | Values | Default | Needs |
|---|---|---|---|
| `OUTHAUL_BACKUP_AZURE_ACCOUNT` | a storage account name: 3 to 24 lowercase letters and digits | unset | `OUTHAUL_BACKUP_AZURE_CONTAINER` |
| `OUTHAUL_BACKUP_AZURE_CONTAINER` | 3 to 63 lowercase letters, digits and single hyphens, starting and ending with a letter or digit | unset | `OUTHAUL_BACKUP_AZURE_ACCOUNT` |
| `OUTHAUL_STATE_BACKUP_SCHEDULE` | a five-field cron expression in server-local time, as on the backup form (`cron.Parse`) | unset: no state backups | a destination: the next setting, or the Azure pair |
| `OUTHAUL_STATE_BACKUP_DESTINATION` | the name of a destination on Settings | unset: the Azure destination | `OUTHAUL_STATE_BACKUP_SCHEDULE` |
| `OUTHAUL_STATE_BACKUP_KEEP` | a whole number; `0` keeps every copy | `30` | `OUTHAUL_STATE_BACKUP_SCHEDULE` |
| `OUTHAUL_STATE_BACKUP_PREFIX` | a key prefix: letters, digits, `-`, `_`, `.` and `/`, with no `/` at either end | empty | `OUTHAUL_STATE_BACKUP_SCHEDULE` |

The Azure pair and the state backup work without each other: the pair alone
gives database and volume schedules an Azure destination, and the state backup
can go to an S3 one. The prefix keeps hosts that share a container apart.

`AZURE_CLIENT_ID` is not an Outhaul setting. It is the managed identity's
client ID, and reaches Outhaul's environment from `/etc/outhaul/azure.env` the
way R1's DNS variables do. With the Azure pair set, it must look like a GUID
if present. If absent, the token request names no identity, which works on a
VM with exactly one.

`Validate` names the variable and what it wants. It runs before the database
opens, so an `OUTHAUL_STATE_BACKUP_DESTINATION` that matches no destination is
a `WARNING` at startup and the failure reason of every run, not a refusal to
start.

The first deployment sets:

```
OUTHAUL_BACKUP_AZURE_ACCOUNT=examplebackups
OUTHAUL_BACKUP_AZURE_CONTAINER=outhaul
OUTHAUL_STATE_BACKUP_SCHEDULE=15 2 * * *
```

## Part 1: an Azure Blob destination

### Destinations today

A destination is a row in `destinations`: name, endpoint, region, bucket,
access key, and a secret sealed with `secret.key`. A system admin adds it on
Settings (`handleCreateDestination`, `internal/server/backups.go`); its name
is lowercase letters, digits and hyphens. A schedule (a `backups` row) points
at one by foreign key. `blobstore.Open(d)` returns the S3 client behind
`blobstore.Client`: `Put(ctx, key, r, size)`, `Get`, `List(ctx, prefix)`
sorted by key, and `Delete`, where a missing key is not an error.
`backup.Manager` reaches it through its `dial` field. Test is
`blobstore.Probe`: put, then delete, `.outhaul-probe`. The Azure client
implements the same interface, so the manager, retention, the restore pages
and Test don't change.

### One destination, from the environment

The two settings define one destination. It has nothing secret to type, and
`restore-state` needs it before any database exists, so it is not entered on
the page. Schedules need a row to point at, so `outhaul serve` keeps one in
step with the settings, after `openStore` and before the backup manager
starts. A restored database's row follows the new host's settings this way.

| Settings | Row of kind `azure` | At startup |
|---|---|---|
| set | none | inserted |
| set | exists | endpoint and bucket updated |
| unset | exists, no schedule uses it | deleted |
| unset | exists, schedules use it | kept. Its runs and Test fail with "`OUTHAUL_BACKUP_AZURE_ACCOUNT` is not set". It can be removed once its schedules are |

What changes:

- A new migration, the next free number (`0029` today; other designs may take
  it first): `ALTER TABLE destinations ADD COLUMN kind TEXT NOT NULL DEFAULT
  's3'`. `core.Destination` (`internal/core/backup.go`) gains `Kind`.
- The row: name `Azure Blob`, which the form's name rule can't produce;
  endpoint `https://<account>.blob.core.windows.net`; bucket, the container;
  region and access key empty; the secret stored as `''`, not sealed.
- `scanDestination` (`internal/store/backups.go`) opens the secret only when
  it isn't empty. Today it would fail on `''`.
- The destinations entry in `sealedQueries` (`internal/store/sealed.go`)
  becomes `WHERE secret_key <> ''`. Otherwise this row counts as a sealed
  value, and a host that lost its key with nothing sealed couldn't make one.
- Settings (`settings.tmpl`, "Backup destinations"): the row shows the account
  and container, "managed identity", and Test. It has no remove button while
  the settings are set, and `handleDeleteDestination` refuses it then. The add
  form is unchanged.
- `blobstore.Open` chooses the client by `Kind`. The Azure client needs one
  token source for the whole process, so `Open` becomes a method on a small
  opener that holds it, passed to `backup.NewManager` as `dial`.

### The client

New files `internal/blobstore/azure.go` and `imds.go`. The client is built
from a `core.Destination`, its endpoint and bucket, so a test points it at an
`httptest` server and `restore-state` builds one from the settings with no
row. Every request goes to `<endpoint>/<container>/<key>`, the key escaped by
segment with `uriEncode(key, true)` (`sigv4.go`), and carries `Authorization:
Bearer <token>`, `x-ms-version: 2023-11-03` and `x-ms-date`. That version is
deployed everywhere, is accepted by Azurite, and has every limit below (they
arrived in `2019-12-12`).

| `Client` method | Blob service call | Notes |
|---|---|---|
| `Put`, size up to 8 MiB | Put Blob: `PUT`, `x-ms-blob-type: BlockBlob` | expects 201; overwrites |
| `Put`, larger | Put Block per 8 MiB: `PUT ?comp=block&blockid=<id>`, then Put Block List: `PUT ?comp=blocklist` with each ID under `<Latest>` | IDs are Base64 of a fixed-width counter, so all have one length. The blob doesn't exist until the list is committed. At most 50,000 blocks: about 390 GiB, refused before the upload starts |
| `Get` | Get Blob: `GET` | 200; the body is streamed. A missing blob is an error |
| `List` | List Blobs: `GET <container>?restype=container&comp=list&prefix=<p>&marker=<m>` | no delimiter. Follow `NextMarker` until it is empty: a page may hold fewer than the 5,000 maximum. Size is `Properties/Content-Length`. The client sorts the keys itself; a name marked `Encoded="true"` is percent-decoded |
| `Delete` | Delete Blob: `DELETE` | expects 202; 404 counts as done |

Blocks are read one at a time into one buffer, so memory use is 8 MiB and
every request body can be sent again. A block or small blob that fails on a
network error, 408, 429 or 5xx is retried twice. Blocks never committed are
discarded by the service after a week. A single Put Blob may carry 5,000 MiB,
but one request for a whole archive needs one deadline for the whole upload,
which is the S3 client's open problem. Blocks avoid it.

| Answer | What the run or Test says |
|---|---|
| 401 | the token is dropped, fetched once more, and the request sent once more. After that: the storage account refused the token |
| 403 | the identity may not write to this container; it needs the "Storage Blob Data Contributor" role on it, and the account's firewall must admit this host |
| 404 `ContainerNotFound` | the container doesn't exist in this account |
| anything else | the status and the service's error code (`x-ms-error-code`) |

### Timeouts

Today the S3 client is `http.Client{Timeout: 15 * time.Minute}`, which covers
a whole upload. The Azure client sets no `http.Client.Timeout`:

- Each request has its own deadline: 30 seconds for a list page or a delete;
  60 for Put Block List; 5 minutes for one block or small blob, which fails
  only below about 28 KiB a second.
- A download has no total deadline. It ends if no byte arrives for 60 seconds.
- The transport limits connecting and the TLS handshake to 10 seconds each.
- The transfer as a whole is bounded only by the run's context: the manager's
  two-hour `runTimeout`, and `Stop` at shutdown.

### The token

`GET http://169.254.169.254/metadata/identity/oauth2/token` with the query
`api-version=2018-02-01&resource=https://storage.azure.com/`, the header
`Metadata: true`, and `&client_id=<AZURE_CLIENT_ID>` when that is set. The
answer is JSON whose fields are strings: `access_token`, and `expires_on` in
Unix seconds.

- The token is kept in memory only, never logged, and never part of an error.
- It is reused until five minutes before `expires_on`. Each request asks the
  source, so a long run picks up a new token between blocks. If a refresh
  fails while the old token is still valid, the old one is used.
- Nothing is requested at startup. The first Test or run makes the first call.
- The transport for this address ignores proxy settings, which the service
  requires, and gives up connecting after 2 seconds.

| What happens | What the caller sees | Retried |
|---|---|---|
| no connection within 2 seconds | can't reach the instance metadata service at `169.254.169.254`; a managed identity works only on an Azure VM that has one assigned | no |
| 400 | the service's `error` and `error_description`, and "check `AZURE_CLIENT_ID`" | no |
| 404, 410, 429, 5xx | the status, after the retries | with backoff, about 20 seconds in all |
| 200 with no usable token | unexpected answer from the metadata service | no |

So a host with no such service fails a Test in about two seconds, and one on
another cloud, whose metadata service answers 404, in about twenty.

### An Azurite check, outside CI

One script, `deploy/test/integration_azure.sh`, starts Azurite with `--oauth
basic` over HTTPS and a throwaway certificate, and runs a Go test, skipped
unless an environment variable gives it the endpoint, that drives the real
client with a stand-in token source through every call. It shows the requests
are accepted by an implementation we didn't write. It can't show the metadata
service, real token checks, roles, the firewall or real limits. It is not in
CI, like `integration_gate.sh`.

## Part 2: Outhaul's own state

### What a rebuild loses

| What | In the state backup | After a restore |
|---|---|---|
| `outhaul.db` | yes, without sign-in sessions | put back by `restore-state` |
| `secret.key` | never | supplied by a person |
| Sign-in sessions, protected-route sessions, one-time codes | no: removed from the copy | everyone signs in again |
| `traefik/acme/acme.json` (ACME account, certificates, private keys) | no | Traefik orders again: one wildcard under R1, otherwise one order per host, which a host with many names must weigh against Let's Encrypt's limits |
| `traefik/dynamic`, `traefik/certs`, `work/`, `update/`, `git-hook.sock` | no | written again at startup |
| `ca/` (the local CA's root and key) | no: a private key | a new root at first start, which devices must trust again, unless the old directory is copied back first |
| `git/<app>.git` (push-deploy repositories) | no (question 3) | the next `git push` re-creates the repository (`gitrepo.Manager.Init`) and deploys. Until then the app is not redeployed |
| `ssh_host_ed25519_key` | no: a private key | a new host key; people who push see a host-key warning once |
| `databases/<name>/` (managed database data) | no | from each database's own schedule. One with no schedule, and every Redis, comes back empty |
| App volumes | no | from each app's own schedule, or empty |
| Images, containers, the network, Traefik | no | `ensureInfra` re-creates the network and Traefik; images are rebuilt or pulled |
| Build logs | in memory only | lost, as at any restart |
| The binary, the unit, `/etc/outhaul.env`, `/etc/outhaul/azure.env` | not Outhaul's state | the installer and the infrastructure write them |

### The copy

A new `func (s *Store) Snapshot(ctx, path string) error` in `internal/store`:

1. `VACUUM INTO ?` on the store's own connection. It is plain SQL, takes a
   consistent copy while the server runs, and compacts it. The store has one
   connection, so other queries wait: about 70 ms for a 20 MB database. The
   driver's backup API needs driver types through `sql.Conn.Raw` and more
   code, for no gain here. The target must not exist, so the file gets a new
   name, `backup-state-<stamp>-<random>.db` in `<data dir>/work`, mode 0600.
   The pruner's `cleanWorkDir` already removes day-old `backup-*` files.
2. On the copy, through its own handle: delete every row of `sessions`,
   `gate_sessions`, `route_sessions` and `gate_codes`, then `VACUUM`, then
   `PRAGMA integrity_check`, which must answer `ok`. `sessions.token` signs a
   browser in to the admin UI, and none of these rows is any use after a
   restore. A `DELETE` alone leaves the bytes in the file; `VACUUM` removes
   them. The result is one file, with no `-wal` beside it.

### Where it goes

The manager gzips the copy through `stage(true, …)` and uploads it as
`<prefix>/_outhaul-state/<stamp>.db.gz`, with `stamp()`'s `20060102T150405Z`.
No app or database can be named `_outhaul-state`, so the directory can't
collide with a schedule's. The existing `prune` then keeps the newest
`OUTHAUL_STATE_BACKUP_KEEP` objects that sit directly in that directory and
end in `.db.gz`.

### When it runs, and what shows it

It is not a `backups` row: that table's targets are databases and apps, and
its page is per project. `backup.Manager` is given the state settings, and
`Tick` checks them after the schedules, under the same `hold` (no run during
an update). The run goes through `launch` with the reserved ID `0` (schedule
IDs start at 1), so it never overlaps itself, obeys `runTimeout`, and is
cancelled by `Stop`.

- The destination is found at each run: by name among `ListDestinations`, or
  the row of kind `azure`.
- **An install with no account is not backed up**, on schedule or by Run now.
  The installer starts an empty Outhaul on the rebuilt host, with the same
  settings, before anyone restores. Its state must not become the newest copy.
- The outcome is logged, with `WARNING` on failure, and kept as one `settings`
  value: when, whether it worked, the key, the size, or the reason.
- Settings gets a "State backup" panel, shown only when the schedule is set:
  schedule, destination, the last outcome, and Run now, which is a new route
  at `levelSystemAdmin` in `routes()` and a new method on the `Backups`
  interface (`internal/server/server.go`).

## Part 3: restoring

### The command

```
outhaul restore-state --from <file | latest | STAMP> [--key-file PATH]
outhaul restore-state --list
```

A new case in `main()` beside `ca` and `images`, with a line in `usage()`. It
reads its configuration from the environment like `serve`, runs `Validate`
first, and doesn't read environment files itself.

- `--from` with a path: the uploaded `.db.gz`, or a plain SQLite file. The
  first bytes say which.
- `--from latest` or a stamp such as `20261111T021500Z`: read from the Azure
  container the environment names, under the state prefix. `--list` prints the
  stamps there. Without the Azure pair these say to pass a file. Anything else
  is a path; a file called `latest` is `./latest`.
- It may run as root. Every file it makes gets the data directory's owner and
  group, so the service can open them.

### The key

`--key-file` names a file holding the 32 raw bytes of `secret.key`. Without
it, the `secret.key` already in the data directory is used. How the file
reaches the host is the infrastructure side's choice. The docs give `base64`
lines for keeping it as text; the binary parses no encodings. The supplied
file is left where it is.

The key is proved before anything is replaced: the command opens every sealed
value in the staged copy with it. So `sealedQueries` becomes one list of
table, column and condition, from which `HasSealedValues` builds its existence
queries and a new `CheckSealed` reads and opens each value. The columns are
the ones the list covers today: `apps.ssh_private_key`, `app_env.value`,
`project_env.value`, `databases.password`, `destinations.secret_key`, the
three secrets in `github_app_sources`, and the Cloudflare token and SSO client
secret in `settings`. A copy with nothing sealed can't prove a key; the
command says so, and the key is then optional.

### What it refuses

Each refusal leaves the database and the key as they were, and exits non-zero.
There is no `--force`.

| Case | How it is known | Message says |
|---|---|---|
| No data directory | it doesn't exist | install Outhaul first |
| Outhaul is running | opening the existing `outhaul.db` with `locking_mode(EXCLUSIVE)` and no busy timeout answers `SQLITE_BUSY` at once while another process has it open | stop Outhaul first |
| The install is in use | the existing database has an account (`HasUser`) | this fills an empty install only; to replace this one, stop Outhaul and move `outhaul.db`, its `-wal` and `-shm`, and `secret.key` away yourself |
| Not a backup | not gzip or SQLite, `integrity_check` fails, or no `schema_migrations` table | what was wrong with the file |
| From a newer Outhaul | the copy's `schema_migrations` has a name this binary doesn't embed | install that version or newer, then restore |
| No key, or the wrong one | no key file, not 32 bytes, or a sealed value doesn't open | pass the original with `--key-file`. The key the fresh install made for itself can't work |

### What it does

1. Checks the data directory and, if there is a database, that no other
   process has it open and that it has no account. The handle is closed again,
   which folds a `-wal` left by a crash into the file.
2. Fetches or opens the file and gunzips it to `outhaul.db.restoring` in the
   data directory, mode 0600, replacing one left by an earlier attempt. Checks
   it, and refuses a newer schema. From here a refusal removes this file.
3. Opens it with `store.Open` and the key. That runs `migrate`, so a copy from
   an older Outhaul is brought forward here, on the staged file. If a
   migration fails, the host is untouched.
4. Runs `CheckSealed`.
5. Deletes the session rows again, for a file the state backup didn't make,
   and writes a marker in `settings`: restored, when, from what, and the
   first-start pass still to run. Closes it, so it is one file again.
6. Repeats the open-elsewhere check, then swaps, key first. A differing
   `secret.key` moves to `secret.key.before-restore-<stamp>` and the supplied
   key is written and renamed into place, mode 0600. Then `outhaul.db`, and
   any `-wal` or `-shm`, move to `*.before-restore-<stamp>`, and the staged
   file is renamed to `outhaul.db`. Nothing is deleted. Stopped between the
   two, the host has the new key and the empty database, which holds nothing
   sealed, so it still starts.
7. Prints the copy's stamp, how many accounts, apps and databases it holds,
   how many sealed values opened, which migrations ran, what moved aside, and
   what the first start will do.

A database renamed into place beside another database's `-wal` reads the
other's pages, and `integrity_check` still answers `ok`. So no handle on the
old database stays open across the swap, and its `-wal` and `-shm` are gone
before the new file arrives. The running-server check guards against a
forgotten `systemctl stop`. It is not a lock: nothing holds off a start in the
instant between the last check and the rename.

### The first start after a restore

`serve` already does part of this for any database: `RecoverActive` fails
deployments that were in flight, `RecoverCreatingDatabases` fails databases
left in `creating`, `ensureInfra` re-creates the network and Traefik, and the
preview sweeper removes previews whose pull request has since closed.

When the marker is present, a pass runs once, after `ensureInfra` and the
worker have started. It is retried each minute until it has run to its end.
One failure inside it is recorded, not retried. It only creates what is
missing:

1. **Images.** A deployment row whose `outhaul/…` image is not on this host
   (`RetainedImages` against `ListImages`) is marked pruned
   (`MarkImagePruned`), so no rollback offers it.
2. **Managed databases.** Each row with status `running` and no container is
   provisioned again with `dbaas.Manager.ProvisionSync`: same name, user and
   password, and an empty data directory, before any app. Rows that are
   `stopped` or `failed` are left.
3. **Apps.** Each app that has a live deployment (`LiveDeployment`) and no
   container of its own on this host, running or stopped, gets the deploy a
   protection change would queue: `enqueueProtectionDeploy(ctx, app, true,
   serving)`, where `serving` is the live row's image if it is on this host
   and empty otherwise. So a kept image is started again, and without one the
   app is built from its source, as `chooseProtectionDeploy` decides. Ordinary
   apps go first, then previews. A push-deploy app whose repository is missing
   is skipped and listed.
4. The marker becomes "done", with what was re-created, queued, failed, or
   waits for a push. The log has a line for each and a summary. Settings shows
   the same to system admins until one dismisses it, through a new route at
   `levelSystemAdmin`.

Left to a person: restoring each database's and volume's data from its
schedule's Restore page, which needs the database running and the volume
present, so after the pass; and pushing push-deploy apps again.

Whether an app was stopped is read from Docker, not stored, so an app stopped
at backup time is deployed again. An app comes back at its branch's head, not
at the commit that was running, unless its image was kept.

The pass and R3's `Server.RedeployStale` share the choice of deploy and the
retry-until-done loop. That one takes running containers with a stale stamp,
this one takes apps with no container, so they can't both pick an app, and
neither needs the other.

### The steps a person follows

These go in the docs as written.

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

## Checked so far

On 11 November 2026, with throwaway programs and containers:

- `modernc.org/sqlite` v1.53.0, the driver in `go.mod`: `VACUUM INTO ?` works
  on the single connection while another goroutine writes, holds exactly the
  committed rows, and passes `integrity_check`. It fails if the target exists
  or inside a transaction. The copy is one rollback-journal file, mode 0644.
  A `DELETE` leaves the row's bytes in the file and `VACUUM` removes them.
- The exclusive open answers `SQLITE_BUSY` at once while another process has
  the database open in WAL mode, even idle. After that process exits or is
  killed it succeeds, folds a leftover `-wal` into the file and removes it,
  and leaves the `-shm`.
- With a handle left open on the old database, a one-row database renamed into
  its place read the old one's rows, and `integrity_check` said `ok`.
- Azurite 3.37.0 with `--oauth basic`, `x-ms-version: 2023-11-03`: every call
  in the client's table, with the status codes given; a blob invisible until
  its block list is committed; list pages through `NextMarker`.
- systemd 259: `systemd-run -p EnvironmentFile=…` passes the file's values to
  the command, tried as a user unit.

Not checked: the real metadata service, real token validation, the role, the
storage firewall, a token refresh, and a restore on a real rebuilt host. The
limits, headers and errors for those are from Microsoft's public
documentation, read the same day.

## Tests

- `internal/config`: each setting's values and each refused combination.
- `internal/blobstore`: both services faked with `httptest`, as `fakeS3` and
  `internal/docker/cancel_test.go` do. Uploads at the 8 MiB boundary, the
  block list's body, pagination and sorting, a missing blob; the token cached,
  refreshed, and fetched once more after a 401; which answers are retried; a
  closed port failing fast; a slow but moving upload not cut off, a stalled
  download ended; no token in any error.
- `internal/store`: the migration; the startup sync's four cases; an empty
  secret; `HasSealedValues` with only the Azure row; `CheckSealed` with the
  right key, the wrong one and nothing sealed; `Snapshot` under writes,
  without session rows or their bytes; an old-schema file
  (`openMigratedBefore`) brought forward; an unknown migration found.
- `internal/backup`, with `fakeBlob`: the state run's key and content,
  retention, the skip with no account, the guard, `hold`, `Stop`, the recorded
  outcome, a destination that doesn't exist.
- `internal/server`, with the fake runtime and deployer: the Azure row and its
  refusal to be removed; the panel and Run now; the pass in order, skipping
  apps that have containers or were never live, choosing by image, listing
  push apps, running once, and doing nothing without the marker.
- `main`: `restore-state` on temporary directories: both file kinds, each
  refusal leaving the database and key as they were, the swap order, a
  leftover `-wal` and `-shm` moved aside, an older copy migrated.

## Docs

- A new `docs/STATE-BACKUP.md`: the settings, the role and container the
  identity needs, what the copy holds and doesn't, that the container must be
  private and the key kept apart from it, the steps above, and the list in
  [What only the real host can show](#what-only-the-real-host-can-show).
- `README.md`: the feature list, the configuration table, the commands.
- `ARCHITECTURE.md`: the backups section, with state backup and restore.
- `AGENTS.md`: the sealed list's new shape; that the `Azure Blob` row belongs
  to the environment; that `restore-state` moves a key and database aside
  together and never deletes either; the marker and the first-start pass.

## Order of work

One series of commits per part on `enhancements`, each with its tests and
docs, so they can be opened as three pull requests:

1. Part 1: the `kind` column and the sealed-list entry. No visible change.
2. Part 1: the token source and the client, against the fakes.
3. Part 1: the settings, the startup sync, the Settings row, the opener, and
   `integration_azure.sh`.
4. Part 2: `Store.Snapshot`.
5. Part 2: the settings, the state run, the panel and Run now.
6. Part 3: the sealed list as columns, and `CheckSealed`.
7. Part 3: `restore-state` from a file, then `latest`, a stamp and `--list`.
8. Part 3: the marker, the first-start pass and its notice.

Part 2 works without Part 1, with an S3 destination. Part 3 needs Part 2's
file format, and Part 1 only for `latest`, a stamp and `--list`. None of it
needs R1 to R4 built first.

## What only the real host can show

For the infrastructure side to run on the staging host. It goes in the pull
requests and in `docs/STATE-BACKUP.md`.

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

## Left to the builder

- Names of files, functions, routes and `settings` keys, the shape of the
  stored values, the wording of messages, and the retry and backoff numbers.
- How the opener reaches the manager, and how the store exposes the exclusive
  check, the account check and the unknown-migration check.
- Confirming each sealed column against the code that reads it, including
  whether an empty value is ever stored unsealed.
- Where the first-start pass lives, and whether it shares a helper with
  `RedeployStale` if that exists by then. `ProvisionSync` is on
  `*dbaas.Manager` but not on the server's `Databases` interface.
- Which deployment rows hold an `outhaul/…` image, for apps deployed from a
  registry image.
- How a test shows the running-server refusal: the probe used two processes.
  And running the documented `systemd-run` line as root on Debian 13.

## Calls made in this design

These go beyond the brief, or choose between readings of it. The maintainer
approved this design on 11 November 2026. The second was put to the
maintainer as a question. The rest were the designer's calls, approved with
it.

1. There is one Azure destination, from the environment. Azure destinations
   aren't entered on Settings: the add form would gain a kind, the row would
   carry an account, and `restore-state` would still need the settings to
   find a copy.
2. **Agreed.** After a restore, each database's and volume's data is put back
   by a person, from the Restore pages, and not automatically from its newest
   archive. The pass would otherwise also run restores, and have to decide
   what to do when one fails or an archive is stale.
3. The state backup doesn't carry push-deploy repositories. Those apps come
   back at the next push. The copy would otherwise become an archive of the
   database and `git/`.
4. `VACUUM INTO`, not the driver's backup API.
5. Sign-in sessions are removed from the copy, and a restore ends them all.
   The copy is not encrypted.
6. The state backup is configured by settings, has no `backups` row, and goes
   to a destination of either kind.
7. The directory `_outhaul-state`, 30 copies by default, and a prefix setting.
8. An install with no account uploads nothing. A destination name that matches
   nothing is a warning and a failed run, not a refusal to start.
9. `x-ms-version: 2023-11-03`; 8 MiB blocks for anything larger than one;
   per-request deadlines and a stall check, with no deadline for a whole
   transfer but the run's.
10. Nothing is asked of the metadata service at startup. A 401 costs one new
    token and one more try. `AZURE_CLIENT_ID` is optional.
11. The row is named `Azure Blob`, stays while schedules use it, and can't be
    removed on the page while the settings are set. The destinations entry in
    the sealed list counts only rows with a secret.
12. `restore-state` reads Azure for `latest`, a stamp and `--list`, and a file
    otherwise. It has no flag for an S3 destination.
13. It fills only an install with no account, has no `--force`, and moves the
    old database and key aside instead of deleting them.
14. It refuses a copy from a newer Outhaul, and migrates an older one on the
    staged file.
15. It takes the key as a raw file, and proves it against every sealed value.
16. It finds a running server by the database's lock, not by a port or a PID
    file. The check is a guard, not a lock.
17. It reads the environment only. The docs run it under `systemd-run`.
18. The first-start pass is switched on by the marker, not by a setting.
19. Managed databases are re-created empty before apps deploy, so apps start
    against empty databases until their data is restored.
20. Previews are redeployed like other apps, after them. An app that was
    stopped when the copy was taken is deployed again.
21. Rows whose image is gone are marked pruned. The ACME store, the local CA's
    key and the SSH host key are not in the copy.
22. The Azurite check is one script outside CI.
