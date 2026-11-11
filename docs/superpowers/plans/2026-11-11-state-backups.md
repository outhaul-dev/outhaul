# State Backups Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add an Azure Blob destination that authenticates with the host's managed identity, a scheduled copy of Outhaul's own database to either kind of destination, and an `outhaul restore-state` command that puts a copy back on a fresh host and lets the first start re-create what it can.

**Architecture:** `core.Destination` gains a `Kind`; `blobstore.Opener` picks the S3 or Azure client by it, and one process-wide managed-identity token source serves every Azure request. The state copy is `VACUUM INTO` plus session stripping, gzipped through the existing backup staging path and stored under `_outhaul-state/`. `restore-state` proves the key against every sealed column before it swaps a key and database aside, then writes a marker in `settings` that a retrying first-start pass consumes.

**Tech Stack:** Go 1.26, `modernc.org/sqlite` v1.53.0, stdlib `net/http` and `encoding/xml` (no SDK), Docker Engine API, POSIX `sh`, Azurite for the out-of-CI Azure check.

**Spec:** `docs/superpowers/specs/2026-11-11-state-backups-design.md`. Read it first; this plan argues from it.

## Global Constraints

- An install that sets none of the new settings notices nothing: no Settings panel, no request to `169.254.169.254`, no startup step, no new log line beyond the existing ones.
- New settings, exactly: `OUTHAUL_BACKUP_AZURE_ACCOUNT` (3–24 lowercase letters and digits), `OUTHAUL_BACKUP_AZURE_CONTAINER` (3–63 lowercase letters, digits and single hyphens, starting and ending with a letter or digit), `OUTHAUL_STATE_BACKUP_SCHEDULE` (5-field cron), `OUTHAUL_STATE_BACKUP_DESTINATION` (a destination name), `OUTHAUL_STATE_BACKUP_KEEP` (whole number, default `30`), `OUTHAUL_STATE_BACKUP_PREFIX` (letters, digits, `-`, `_`, `.`, `/`, no `/` at either end). `AZURE_CLIENT_ID` is read from the environment but is not an `OUTHAUL_*` setting.
- Settings are read through the `parser` in `internal/config/config.go`; `Validate` names the variable and what it wants, and refuses bad combinations before the database opens.
- No SDK: the Azure client and the IMDS token source use only the standard library.
- `secret.key` is never uploaded, logged or included in the copy. A state copy is not encrypted; the container must be private.
- No access token, key, sign-in session or one-time code appears in a log line, an error, a page or a container.
- Test first: write the failing test, watch it fail for the stated reason, then implement. Use the existing fakes (`docker.Fake`, `compose.Fake`, `github.Fake`, the fake runtime and deployer in `internal/server`); outside services are faked with `httptest`.
- The repository is public: `example.com` names only, in code, tests, scripts and docs.
- Commits look like `feat(scope): summary` or `fix(scope): summary`, with a body saying what someone saw go wrong and what changed. One series per part (Part 1, Part 2, Part 3) so they can be opened as three pull requests.
- Docs travel with the code: a task that adds a setting adds its row to `README.md`'s configuration table and its section to `docs/STATE-BACKUP.md` (created in Task 5, in the plain style of `docs/SSO.md`). Task 13 finishes that document.
- After each task: `go build ./... && go vet ./... && go test ./...`. Before calling the plan done: `go test -race -timeout 30m ./...`, and `shellcheck -s sh --severity=warning deploy/test/*.sh` for the scripts.
- Do not commit an `outhaul` binary left by `go build .`.

## Review Focus

The inputs most likely to bite that no requirement spells out. Each has its test in the task named.

1. A copy with no sealed values: `restore-state` must accept it without a key, and a wrong 32-byte key must not be reported as a failure when there is nothing to prove (Task 11).
2. An Azure row kept after `OUTHAUL_BACKUP_AZURE_ACCOUNT` and `..._CONTAINER` are unset: its endpoint and bucket are cleared, so Test and every run fail with the variable's name rather than using the old account (Tasks 5 and 8).
3. A copy from an older Outhaul: migrations run on the staged file, and a migration failure leaves the host's database and key untouched (Task 11).
4. A `-wal` and `-shm` left beside the old database: they move aside with it, and the new database never sits beside the old `-wal` (Task 11).
5. An Azure upload crossing the 8 MiB boundary while a token expires or a block request is retried: block IDs stay the same, the block list commits once, and a 401 costs one refresh and one retry (Task 3, `TestAzureBlockRetryAnd401`).

---

## File map

| File | Responsibility | Tasks |
|---|---|---|
| `internal/core/backup.go` | `Destination.Kind`, `Destination.AzureAccount`, `StateBackupRun` | 1, 5, 8 |
| `internal/core/restore.go` (new) | `RestoreMarker` | 11, 13 |
| `internal/store/migrations/0030_destination_kind.sql` (new) | the `kind` column | 1 |
| `internal/store/backups.go` | kind in create/scan, the Azure row sync, the state outcome | 1, 5, 8 |
| `internal/store/sealed.go` | the sealed-column list, `HasSealedValues`, `CheckSealed` | 1, 10 |
| `internal/store/snapshot.go` (new) | `Snapshot`, `StripSessions` | 7 |
| `internal/store/restore.go` (new) | `CheckDatabaseIdle`, `HasUserInFile`, `CheckStateCopy`, `AppliedMigrations`, `StateCounts`, the marker | 11, 13 |
| `internal/blobstore/imds.go` (new) | the managed-identity token source | 2 |
| `internal/blobstore/azure.go` (new) | the Azure Blob client | 3 |
| `internal/blobstore/blobstore.go` | `Open` becomes `OpenS3` | 5 |
| `internal/blobstore/opener.go` (new) | `Opener`, `NewOpener`, `Open` by kind | 5 |
| `internal/config/config.go` | the six settings, `AZURE_CLIENT_ID`, validation | 4, 8 |
| `internal/backup/manager.go` | dial parameter, state settings, state run, exported `Stamp`/`JoinKey`/`StateDir` | 5, 8 |
| `internal/server/server.go` | `Backups.RunStateNow`, `Databases.ProvisionSync`, `Runtime.ListImages`, setters | 5, 9, 13 |
| `internal/server/backups.go`, `settings.go`, `routes.go` | the Azure row, the state panel, Run now, the restore notice, dismiss | 5, 9, 13 |
| `internal/server/restorepass.go` (new) | the first-start pass | 13 |
| `internal/server/templates/settings.tmpl` | the Azure row, the state panel, the restore notice | 5, 9, 13 |
| `internal/gitrepo/repo.go` | `Exists` | 13 |
| `main.go` | sync, opener, state settings, the restore command dispatch, the pass | 5, 8, 9, 11, 13 |
| `restore_state.go` (new) | `restore-state`'s parsing, refusals, staging, swap, summary | 11, 12 |
| `deploy/test/integration_azure.sh` (new) | the Azurite check | 6 |
| `docs/STATE-BACKUP.md` (new) | settings, the role, what the copy holds, the steps, the real-host checks | 5, 8, 9, 11, 12, 13 |
| `README.md`, `ARCHITECTURE.md`, `AGENTS.md` | the feature, the settings, the kept rules | 5, 8, 9, 13 |

---

## Task 1: A destination kind, and the sealed-list condition

No visible change. Part 1's first commit.

**Files:**
- Create: `internal/store/migrations/0030_destination_kind.sql`
- Modify: `internal/core/backup.go`, `internal/store/backups.go:48,97-120`, `internal/store/sealed.go:13`
- Test: `internal/store/backups_test.go`, `internal/store/sealed_test.go`

This plan takes `0030`; the container-limits plan takes `0029`. Once `0030` ships it is never renumbered.

**Interfaces:**
- Produces:

```go
// internal/core/backup.go
const (
	DestinationS3    = "s3"    // an S3-compatible endpoint with an access key and secret
	DestinationAzure = "azure" // Azure Blob Storage, authenticated by the host's managed identity
)

type Destination struct {
	// ... existing fields ...
	Kind string // DestinationS3 | DestinationAzure; "" is treated as s3 when read
}
```

- [ ] **Step 1: Write the failing migration test**

```go
// internal/store/backups_test.go
func TestDestinationKindMigration(t *testing.T) {
	db := openMigratedBefore(t, "0030")
	if _, err := db.Exec(`INSERT INTO destinations
		(name, endpoint, region, bucket, access_key, secret_key, created_at)
		VALUES ('minio', 'http://minio:9000', '', 'b', 'AK', 'SK', '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if err := migrate(db); err != nil {
		t.Fatal(err)
	}
	var kind string
	if err := db.QueryRow(`SELECT kind FROM destinations WHERE name = 'minio'`).Scan(&kind); err != nil {
		t.Fatal(err)
	}
	if kind != "s3" {
		t.Errorf("kind = %q, want s3", kind)
	}
}
```

- [ ] **Step 2: Write the failing store tests**

```go
// internal/store/backups_test.go
func TestCreateDestinationDefaultsToS3(t *testing.T) {
	// CreateDestination{Name: "minio", ...} -> returned Kind == "s3"; GetDestination -> Kind == "s3"
}

func TestEmptyDestinationSecretNeedsNoBox(t *testing.T) {
	// Open(path, nil); insert an azure row with secret_key = '' by raw SQL;
	// GetDestination returns SecretKey == "" and Kind == "azure", no error.
}

// internal/store/sealed_test.go
func TestHasSealedValuesIgnoresAnUnsealedDestination(t *testing.T) {
	// Open(path, nil); insert an azure row with secret_key = '';
	// HasSealedValues == false, nil.
}
```

- [ ] **Step 3: Run them to see them fail**

Run: `go test ./internal/store/ -run 'DestinationKind|DefaultsToS3|EmptyDestinationSecret|UnsealedDestination'`
Expected: FAIL (no `kind` column, unknown `DestinationS3`, scan fails on `''`).

- [ ] **Step 4: Implement**

- `0030_destination_kind.sql`:

```sql
-- Backup destinations gain a kind: s3 (the existing form) or azure, whose one
-- row the environment owns and whose secret is empty.
ALTER TABLE destinations ADD COLUMN kind TEXT NOT NULL DEFAULT 's3';
```

- `core`: the two constants and the `Kind` field. `CreateDestination` sets `Kind = DestinationS3` when it is empty, refuses `Kind == DestinationAzure` with `errors.New("store: the Azure destination comes from the environment")`, and inserts `kind`.
- `destinationCols` gains `kind`; `scanDestination` scans it. Open the secret only when `enc != ""`; an empty secret is `""` and needs no box. An unknown kind is kept as read (the opener refuses it later).
- `sealed.go`: the destinations entry becomes ``SELECT 1 FROM destinations WHERE secret_key <> '' LIMIT 1``.

- [ ] **Step 5: Run everything**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/core/backup.go internal/store
git commit -m "feat(store): give backup destinations a kind, and count only sealed destination secrets"
```

---

## Task 2: The managed-identity token source

**Files:**
- Create: `internal/blobstore/imds.go`, `internal/blobstore/imds_test.go`

**Interfaces:**
- Produces:

```go
// internal/blobstore/imds.go
// TokenSource supplies and refreshes the bearer token Azure Storage accepts.
type TokenSource interface {
	Token(ctx context.Context) (string, error)   // cached until five minutes before expiry
	Refresh(ctx context.Context) (string, error) // drop the cached token, fetch one
}

// NewTokenSource returns the IMDS-backed source for the host's managed identity.
// clientID is AZURE_CLIENT_ID; "" asks for the VM's only identity.
func NewTokenSource(clientID string) TokenSource
```

- [ ] **Step 1: Write the failing tests**

```go
// internal/blobstore/imds_test.go
func TestTokenSourceRequestAndCache(t *testing.T) {
	// httptest server: assert GET, path /metadata/identity/oauth2/token,
	//   api-version=2018-02-01, resource=https://storage.azure.com/,
	//   Metadata: true, and no client_id when the source has none; with
	//   clientID "8f7c1a2e-0000-4000-8000-000000000000", client_id is that.
	// answer {"access_token":"tok-1","expires_on":"<now+1h Unix>"}.
	// first Token -> "tok-1", one request; second Token -> "tok-1", still one.
}

func TestTokenSourceRefreshesFiveMinutesBeforeExpiry(t *testing.T) {
	// now is four minutes before expires_on -> Token fetches "tok-2".
	// now is six minutes before -> the cached token is reused.
}

func TestTokenSourceRefresh(t *testing.T) {
	// Refresh fetches immediately and replaces the cached token.
}

func TestTokenSourceUsesTheOldTokenWhenARefreshFails(t *testing.T) {
	// cached token is valid; the server then answers 500; Token returns the old
	// token and no error.
}

func TestTokenSourceNamesTheMetadataService(t *testing.T) {
	// a closed loopback port -> error contains "169.254.169.254" and
	// "managed identity"; returns in under 3s.
	// 400 {"error":"invalid_request","error_description":"bad client"} ->
	//   error contains "bad client" and "AZURE_CLIENT_ID".
	// 404, 410, 429, 500 -> retried, then the status is named.
	// 200 with no access_token -> "unexpected answer from the metadata service".
}

func TestTokenSourceNeverLeaksTheToken(t *testing.T) {
	// server answers 500 with a body containing "secret-token";
	// err.Error() does not contain "secret-token".
}
```

- [ ] **Step 2: Run them to see them fail**

Run: `go test ./internal/blobstore/ -run TokenSource`
Expected: FAIL (undefined `TokenSource`, `NewTokenSource`).

- [ ] **Step 3: Implement**

- Constants: `imdsTokenURL = "http://169.254.169.254/metadata/identity/oauth2/token"`, `tokenResource = "https://storage.azure.com/"`, `tokenAPIVersion = "2018-02-01"`.
- `azureTokenSource` fields: `clientID`, `url` (default `imdsTokenURL`, tests replace it), `http *http.Client`, `now func() time.Time`, `backoff []time.Duration`, `mu`, `token string`, `expires time.Time`.
- Transport: `Proxy: nil`, `DialContext` with a 2-second timeout, `TLSHandshakeTimeout: 2 * time.Second`.
- `Token`: if `now` is before `expires.Add(-5*time.Minute)`, return the cached token. Otherwise fetch. If the fetch fails and the cached token is still valid (`now` before `expires`), return it.
- `Refresh`: clear the cached token, fetch.
- `fetch`: build the query with `api-version`, `resource`, and `client_id` when set; set `Metadata: true`; retry 404, 410, 429 and 5xx with `backoff` (`1s, 2s, 4s, 8s, 5s` by default, about 20 seconds in all). A connect error is not retried. Parse `access_token` and `expires_on` (Unix seconds as a string). A 200 without a usable token is "unexpected answer from the metadata service". A 400 names `error` and `error_description` and says to check `AZURE_CLIENT_ID`. Never include the token in an error.

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/blobstore/ -run TokenSource`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/blobstore/imds.go internal/blobstore/imds_test.go
git commit -m "feat(blobstore): a managed-identity token source for Azure Storage"
```

---

## Task 3: The Azure Blob client

**Files:**
- Create: `internal/blobstore/azure.go`, `internal/blobstore/azure_test.go`

**Interfaces:**
- Consumes: `TokenSource` from Task 2.
- Produces:

```go
// NewAzureClient returns a Client for endpoint (scheme://host[:port][/account])
// and container. It sets no http.Client.Timeout; each call has its own deadline.
func NewAzureClient(endpoint, container string, tokens TokenSource) (Client, error)
```

- [ ] **Step 1: Write the failing tests**

Build a `fakeAzure` `httptest` server in `azure_test.go` that checks `Authorization: Bearer <token>`, `x-ms-version: 2023-11-03` and a non-empty `x-ms-date`, stores blobs and blocks, and can be told to answer 401/500/403/404 or stall.

```go
// internal/blobstore/azure_test.go
func TestAzurePutBoundaries(t *testing.T) {
	// exactly 8 MiB -> one PUT with x-ms-blob-type: BlockBlob
	// 8 MiB + 1 -> two PUT ?comp=block requests then one PUT ?comp=blocklist;
	//   the block IDs are base64("0000000000") and base64("0000000001"), in order
	// 50000*8MiB + 1 -> error before any request; error names the 390 GiB limit
}

func TestAzureBlockListBody(t *testing.T) {
	// the block list body is <BlockList><Latest>…</Latest>…</BlockList>,
	// Content-Type application/xml, and the blob is invisible until it is committed.
}

func TestAzureListPaginatesSortsAndDecodes(t *testing.T) {
	// pages: first has b and "a%20b" with Encoded="true", NextMarker "m2";
	// second has a. List returns a, "a b", b sorted by key, with Content-Length sizes.
}

func TestAzureGetMissingIsAnError(t *testing.T)          // 404 -> error
func TestAzureDelete404CountsAsDone(t *testing.T)        // 404 -> nil
func TestAzure401RefreshesOnce(t *testing.T)             // first 401, then 201; two requests, second bearer is the refreshed token
func TestAzureSecond401NamesTheRefusal(t *testing.T)     // 401 twice -> "the storage account refused the token"
func TestAzureRetries408429And5xx(t *testing.T)          // 500, 500, 201 -> success; 500 three times -> error
func TestAzureBlockRetryAnd401(t *testing.T) {
	// a 9 MiB Put: block 1 answers 500 once, then 201; the same block ID is sent
	// again. A second run: block 0 answers 401 once, the token refreshes, and the
	// same block ID is sent again. Each run commits one block list; the stored
	// bytes equal the input.
}
func TestAzure403NamesTheRoleAndFirewall(t *testing.T)   // error contains "Storage Blob Data Contributor" and "firewall"
func TestAzure404ContainerNotFound(t *testing.T)         // error contains "container doesn't exist"
func TestAzureStalledDownloadEnds(t *testing.T)          // a body that stops for longer than stall -> read error, not a hang
func TestAzureGetHeaderStallEnds(t *testing.T)           // a server that accepts but sends no headers -> Get fails in about stall
func TestAzureSlowUploadIsNotCutOff(t *testing.T)        // a block that takes 50ms per read still succeeds
func TestAzureNoTokenInErrors(t *testing.T)              // the token source's value never appears in an error
func TestAzureRequestShape(t *testing.T)                 // path /outhaul/<escaped key>, query encodings, headers
```

- [ ] **Step 2: Run them to see them fail**

Run: `go test ./internal/blobstore/ -run Azure`
Expected: FAIL (undefined `NewAzureClient`).

- [ ] **Step 3: Implement**

- `blockSize = 8 << 20`; `maxBlocks = 50000`; `version = "2023-11-03"`.
- `azure` fields: `endpoint`, `container`, `tokens`, `http *http.Client`, `now func() time.Time`, `stall time.Duration` (default 60s), `retryDelay time.Duration` (default 100ms, doubled; tests set 0).
- Validate the endpoint (http/https, host) and a non-empty container; trim a trailing `/`.
- Request URL: `<endpoint>/<container>/<key>`, key escaped by segment with `uriEncode(key, true)` (`sigv4.go`). Headers: `Authorization: Bearer <token>`, `x-ms-version`, `x-ms-date` (`http.TimeFormat`, UTC). The token comes from `Token(ctx)` for every request.
- `Put`:
  - `size <= blockSize`: read the body into one buffer (so it can be replayed), `PUT` with `x-ms-blob-type: BlockBlob`, expect 201.
  - larger: refuse more than `maxBlocks` before reading. Read one block at a time into one 8 MiB buffer; `PUT ?comp=block&blockid=<base64(fmt.Sprintf("%010d", i))>`; collect IDs; `PUT ?comp=blocklist` with `<BlockList><Latest>…</Latest></BlockList>` and `Content-Type: application/xml`, expect 201. The blob does not exist until the list commits.
- `Get`: `GET`, expect 200. Bound the wait for response headers with a timer of `stall` (60s), cancelled when `Do` returns; then return a `stallReader` that ends if no byte arrives for `stall`. No total deadline. This matters because `restore-state --from latest` has no run timeout above it.
- `List`: `GET <container>?restype=container&comp=list&prefix=<p>&marker=<m>`. Decode `Blobs/Blob` (`Name` with `Encoded` attr, `Properties/Content-Length`), percent-decode an `Encoded="true"` name, follow `NextMarker` until empty, sort by key.
- `Delete`: `DELETE`, expect 202; 404 counts as done.
- Timeouts: 30s for a list page or delete; 60s for Put Block List; 5 minutes for one block or a small blob. The client has no `http.Client.Timeout`; the transport limits connecting and the TLS handshake to 10s each.
- Retries: a network error, 408, 429 or 5xx gets two more attempts with `retryDelay`; a block or small blob replays its buffer. A 401 drops the token with `Refresh` and sends once more; after that, the refusal text.
- Error mapping: 401 → "the storage account refused the token"; 403 → the role and firewall sentence; 404 with `ContainerNotFound` → "the container doesn't exist in this account"; otherwise the status and `x-ms-error-code` (falling back to the XML `<Code>`). Never include the token.

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/blobstore/ -run Azure`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/blobstore/azure.go internal/blobstore/azure_test.go
git commit -m "feat(blobstore): an Azure Blob client with blocks, retries and per-request deadlines"
```

---

## Task 4: The Azure settings and their validation

**Files:**
- Modify: `internal/config/config.go`
- Test: `internal/config/config_test.go`

**Interfaces:**
- Produces:

```go
// config.Config
BackupAzureAccount   string // OUTHAUL_BACKUP_AZURE_ACCOUNT
BackupAzureContainer string // OUTHAUL_BACKUP_AZURE_CONTAINER
AzureClientID        string // AZURE_CLIENT_ID, not an OUTHAUL_* setting
```

- [ ] **Step 1: Write the failing tests**

```go
// internal/config/config_test.go
func TestAzureDestinationSettings(t *testing.T) {
	// account "examplebackups", container "outhaul" -> Validate nil
	// account "Example", "ab", "a_b" -> error naming OUTHAUL_BACKUP_AZURE_ACCOUNT
	// container "out haul", "-outhaul", "outhaul-", "a--b", "ab" -> error naming OUTHAUL_BACKUP_AZURE_CONTAINER
	// account only -> error naming OUTHAUL_BACKUP_AZURE_CONTAINER
	// container only -> error naming OUTHAUL_BACKUP_AZURE_ACCOUNT
}

func TestAzureClientIDSetting(t *testing.T) {
	// pair + "8f7c1a2e-0000-4000-8000-000000000000" -> nil
	// pair + "not-a-guid" -> error naming AZURE_CLIENT_ID
	// no pair + "not-a-guid" -> nil (only checked when the pair is set)
}
```

- [ ] **Step 2: Run them to see them fail**

Run: `go test ./internal/config/ -run 'AzureDestination|AzureClientID'`
Expected: FAIL (unknown fields; no validation).

- [ ] **Step 3: Implement**

- `Load`: read the two `OUTHAUL_BACKUP_AZURE_*` variables and `AZURE_CLIENT_ID` with `getenv`.
- `Validate`:
  - one of the pair without the other: an error naming the missing variable;
  - account matches `^[a-z0-9]{3,24}$`;
  - container is 3–63 characters, matches `^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$`, and contains no `--`;
  - with the pair set and `AZURE_CLIENT_ID` non-empty, it matches the GUID form `^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`. With the pair unset, an invalid value is ignored.
- Error text names the variable and what it wants, never a secret. Use `errors.Join` like the existing rules.

- [ ] **Step 4: Run everything**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/config
git commit -m "feat(config): the Azure Blob destination settings, checked before startup"
```

---

## Task 5: The opener, the startup sync and the Settings row

Part 1's last code commit. The Azure row is visible, and S3 schedules keep working.

**Files:**
- Create: `internal/blobstore/opener.go`
- Modify: `internal/blobstore/blobstore.go:46` (rename `Open` to `OpenS3`), `internal/store/backups.go`, `internal/core/backup.go`, `internal/server/server.go`, `internal/server/settings.go:24-67`, `internal/server/backups.go:290-354`, `internal/server/templates/settings.tmpl:126-159`, `main.go` (`serve`), `internal/backup/manager.go:64-76`
- Test: `internal/blobstore/blobstore_test.go` (call `OpenS3`), `internal/blobstore/opener_test.go` (new), `internal/store/backups_test.go`, `internal/server/backups_test.go`

**Interfaces:**
- Consumes: `Destination.Kind` from Task 1, `NewAzureClient` from Task 3.
- Produces:

```go
// internal/blobstore/opener.go
type Opener struct{ tokens TokenSource }
func NewOpener(azureClientID string) *Opener
func (o *Opener) Open(d core.Destination) (Client, error)

// internal/blobstore/blobstore.go
func OpenS3(d core.Destination) (Client, error) // was Open

// internal/core/backup.go
func (d Destination) AzureAccount() string // "" when the endpoint is empty or not the Azure form

// internal/store/backups.go
// SyncAzureDestination keeps the single kind=azure row in step with the
// environment. Empty account and container remove it unless a backup schedule
// uses it; a kept row has its endpoint and bucket cleared.
func (s *Store) SyncAzureDestination(ctx context.Context, account, container string) error

// internal/server/server.go
func (s *Server) SetAzureDestinationSet(on bool)

// internal/backup/manager.go
func NewManager(st *store.Store, dc docker.Client, workDir string,
	dial func(core.Destination) (blobstore.Client, error)) *Manager
// nil dial defaults to NewOpener("").Open.
```

- [ ] **Step 1: Write the failing store tests**

```go
// internal/store/backups_test.go
func TestSyncAzureDestination(t *testing.T) {
	// set "examplebackups"/"outhaul" -> one row: name "Azure Blob", kind azure,
	//   endpoint "https://examplebackups.blob.core.windows.net", bucket "outhaul",
	//   region/access_key/secret_key empty; HasSealedValues false
	// set again with container "other" -> same ID, bucket "other"
	// unset with no schedule -> row gone
	// set, attach a backup schedule, unset -> row kept, endpoint and bucket empty
	// set again -> endpoint and bucket restored, same ID
}
```

- [ ] **Step 2: Write the failing opener test**

```go
// internal/blobstore/opener_test.go
func TestOpenerChoosesTheClientByKind(t *testing.T) {
	// Kind "" and "s3" -> *s3
	// Kind "azure" -> *azure
	// Kind "azure" with Endpoint "" -> error containing "OUTHAUL_BACKUP_AZURE_ACCOUNT is not set"
	// Kind "gcs" -> error naming the kind
}
```

- [ ] **Step 3: Write the failing server tests**

```go
// internal/server/backups_test.go
func TestAzureDestinationRow(t *testing.T) {
	// e.store.SyncAzureDestination(ctx, "examplebackups", "outhaul");
	// e.srv.SetAzureDestinationSet(true); GET /settings:
	//   contains "Azure Blob", "examplebackups", "outhaul", "managed identity",
	//   and /settings/destinations/<id>/test; no /settings/destinations/<id>/delete.
}
func TestAzureDestinationCannotBeRemovedWhileSet(t *testing.T) {
	// POST /settings/destinations/<id>/delete -> 409; row still there; text names
	// OUTHAUL_BACKUP_AZURE_ACCOUNT.
}
func TestAzureDestinationTestUsesTheManager(t *testing.T) {
	// POST .../test -> 303; e.backups.tested == ["Azure Blob"].
}
```

- [ ] **Step 4: Run them to see them fail**

Run: `go test ./internal/store/ ./internal/blobstore/ ./internal/server/ -run 'SyncAzure|Opener|AzureDestination'`
Expected: FAIL (undefined `SyncAzureDestination`, `Opener`; no row).

- [ ] **Step 5: Implement**

- Rename `blobstore.Open` to `OpenS3`; update `blobstore_test.go`'s `newTestClient` and the package comment to name both S3-compatible and Azure Blob storage. `Opener.Open` dispatches: `""`/`s3` → `OpenS3`; `azure` → `NewAzureClient`; anything else is an error. An Azure destination with an empty endpoint returns `errors.New("OUTHAUL_BACKUP_AZURE_ACCOUNT is not set")`.
- `NewManager` takes `dial`; when nil, `dial = NewOpener("").Open`. Update `manager_test.go` to pass a closure returning `fakeBlob` (or keep the field assignment after the call).
- `SyncAzureDestination`: find the row with `kind = 'azure'`. Set: insert or update name `Azure Blob`, endpoint `https://<account>.blob.core.windows.net`, bucket, empty region/access key/secret. Unset: call `DeleteDestination` (it opens its own transaction; do not wrap the sync in one — the store has a single connection, so an outer transaction would deadlock); if it returns `ErrDestinationInUse`, update the row to empty endpoint and bucket and keep it.
- `core.Destination.AzureAccount`: trim `http(s)://` and `.blob.core.windows.net`; `""` for an empty endpoint.
- `serve`: after `openStore` and `reserveAdminHost`, call `st.SyncAzureDestination(ctx, cfg.BackupAzureAccount, cfg.BackupAzureContainer)`; return its error.
- `server`: `azureDestinationSet bool` and `SetAzureDestinationSet`. `renderSettingsWith` sets `data["AzureDestinationSet"]`. The template's Azure row shows `AzureAccount` and `Bucket`, "managed identity", Test, and the remove button only when `not .AzureDestinationSet`. `handleDeleteDestination` loads the row first and refuses an azure row while `s.azureDestinationSet`, with a 409 naming the variable. `handleCreateDestination` sets `Kind: core.DestinationS3`.
- `main`: create `opener := blobstore.NewOpener(cfg.AzureClientID)` and pass `opener.Open` to `backup.NewManager`.
- `docs/STATE-BACKUP.md`: start the document with the Azure settings, the managed identity's role ("Storage Blob Data Contributor" on the container), the private-container rule, the first deployment's example environment block from the spec, and that the row belongs to the environment. `README.md`: add the two Azure rows and a note on `AZURE_CLIENT_ID`.

- [ ] **Step 6: Run everything**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal main.go README.md docs/STATE-BACKUP.md
git commit -m "feat(blobstore): one Azure Blob destination from the environment, and its Settings row"
```

---

## Task 6: The Azurite check, outside CI

**Files:**
- Create: `internal/blobstore/azure_azurite_test.go`, `deploy/test/integration_azure.sh`
- Modify: `AGENTS.md` ("Build, run, test")

**Interfaces:**
- Consumes: `NewAzureClient` from Task 3.
- Produces: `TestAzureAzurite`, skipped unless `OUTHAUL_AZURE_TEST_ENDPOINT` is set.

- [ ] **Step 1: Write the skipped test**

```go
// internal/blobstore/azure_azurite_test.go
func TestAzureAzurite(t *testing.T) {
	endpoint := os.Getenv("OUTHAUL_AZURE_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("set OUTHAUL_AZURE_TEST_ENDPOINT to an Azurite account URL")
	}
	// azuriteJWT() is base64url({"alg":"none","typ":"JWT"}) + "." +
	// base64url({"aud":"https://storage.azure.com","iss":"https://sts.windows.net/example-tenant/",
	//   "iat":now,"nbf":now,"exp":now+1h}) + "." (unsigned). Azurite refuses
	// "Bearer azurite" with 403 AuthenticationFailed and accepts this.
	c, err := NewAzureClient(endpoint, "outhaul", &stubTokens{token: azuriteJWT()})
	// trust the throwaway certificate: c.(*azure).http.Transport's TLS config
	// Put/Get a small object; Put/Get a 9 MiB object (blocks);
	// List sees both; Delete removes one; a missing Get is an error.
}
```

- [ ] **Step 2: Write the script**

`deploy/test/integration_azure.sh` follows `integration_gate.sh`'s conventions: POSIX `sh`, `set -eu`, a temp directory and a trap that removes the container, one `outhaul-aztest-*` container and a loopback port, `PASS`/`FAIL`/`INFO` lines, non-zero exit on failure.

- Generate a throwaway certificate with `openssl req -x509 -newkey rsa:2048 -nodes -subj /CN=127.0.0.1 ...`.
- Start `mcr.microsoft.com/azure-storage/azurite:3.37.0` with `azurite-blob --oauth basic --cert … --key … --blobHost 0.0.0.0 --blobPort 10000`, publishing `127.0.0.1::10000`. The image's entrypoint is not the CLI; flags alone fail with `node: bad option: --oauth`.
- Mint the same unsigned JWT in the script (`header.payload.`, base64url) and use it as `Authorization: Bearer $TOKEN`; `Bearer azurite` is refused with 403 AuthenticationFailed.
- Wait until `curl -k -s -o /dev/null https://127.0.0.1:$PORT/` connects. Create the `outhaul` container with a raw `PUT …?restype=container` carrying `Authorization: Bearer $TOKEN`, `x-ms-version: 2023-11-03` and `x-ms-date`, since the client only puts blobs.
- Run `OUTHAUL_AZURE_TEST_ENDPOINT="https://127.0.0.1:$PORT/devstoreaccount1" go test ./internal/blobstore -run TestAzureAzurite -count=1`.
- Say in the header what it cannot show: the metadata service, real token checks, roles, the firewall and real limits.

- [ ] **Step 3: Run it**

Run: `sh deploy/test/integration_azure.sh && shellcheck -s sh --severity=warning deploy/test/integration_azure.sh`
Expected: every line `PASS`, exit 0, no warnings, no `outhaul-aztest` container left.

- [ ] **Step 4: Add the line to `AGENTS.md`**

Beside `integration_gate.sh`, add:

```sh
sh deploy/test/integration_azure.sh  # the Azure client against Azurite (not in CI)
```

- [ ] **Step 5: Commit**

```bash
git add internal/blobstore/azure_azurite_test.go deploy/test/integration_azure.sh AGENTS.md
git commit -m "test(blobstore): the Azure client against Azurite, outside CI"
```

---

## Task 7: `Store.Snapshot`

Part 2's first commit. No visible change.

**Files:**
- Create: `internal/store/snapshot.go`, `internal/store/snapshot_test.go`

**Interfaces:**
- Produces:

```go
// Snapshot writes a consistent, compacted copy of the database to path, which
// must not exist, and removes every sign-in session from it. The file is mode
// 0600 and has no -wal or -shm beside it.
func (s *Store) Snapshot(ctx context.Context, path string) error

// StripSessions removes sessions, gate_sessions, route_sessions and gate_codes,
// then VACUUMs so their bytes leave the file.
func (s *Store) StripSessions(ctx context.Context) error
```

- [ ] **Step 1: Write the failing tests**

```go
// internal/store/snapshot_test.go
func TestSnapshotUnderWrites(t *testing.T) {
	// one goroutine upserts a setting 100 times; Snapshot to a new path;
	// open the copy read-only: PRAGMA integrity_check == "ok"; settings has one
	// row, with one of the written values; the copy is a single file.
}

func TestSnapshotRemovesSessions(t *testing.T) {
	// create a user, a session with token "super-secret-token", a gate session,
	// a route session and a gate code; Snapshot;
	// open the copy: each of the four tables has 0 rows;
	// bytes.Contains(file, []byte("super-secret-token")) is false.
}

func TestSnapshotIsOneFile(t *testing.T) {
	// mode 0600; os.Stat(path+"-wal") and path+"-shm" are ErrNotExist.
}

func TestSnapshotRefusesAnExistingTarget(t *testing.T) {
	// create the target first -> error.
}
```

- [ ] **Step 2: Run them to see them fail**

Run: `go test ./internal/store/ -run Snapshot`
Expected: FAIL (undefined `Snapshot`).

- [ ] **Step 3: Implement**

- `Snapshot`: refuse an existing target. `VACUUM INTO ?` on `s.db`. `os.Chmod(path, 0600)`.
- Open the copy with its own handle: `file:<escaped>?_pragma=busy_timeout(5000)&_pragma=journal_mode(DELETE)&_pragma=foreign_keys(1)`, `SetMaxOpenConns(1)`.
- `stripSessions(ctx, db)`: delete `gate_codes`, `route_sessions`, `gate_sessions`, then `sessions` (children first), then `VACUUM`.
- `PRAGMA integrity_check`; it must answer `ok`.
- Close the copy; remove any `-wal`/`-shm` beside it; check neither exists.
- `StripSessions` is `stripSessions(ctx, s.db)`.

- [ ] **Step 4: Run everything**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/store/snapshot.go internal/store/snapshot_test.go
git commit -m "feat(store): a consistent state snapshot without sign-in sessions"
```

---

## Task 8: The state backup settings and the run

**Files:**
- Modify: `internal/config/config.go`, `internal/core/backup.go`, `internal/store/backups.go`, `internal/backup/manager.go`, `main.go`
- Test: `internal/config/config_test.go`, `internal/store/backups_test.go`, `internal/backup/manager_test.go`, `internal/backup/state_test.go` (new)

**Interfaces:**
- Consumes: `Snapshot` from Task 7, `SyncAzureDestination` from Task 5.
- Produces:

```go
// config.Config
StateBackupSchedule    string // OUTHAUL_STATE_BACKUP_SCHEDULE
StateBackupDestination string // OUTHAUL_STATE_BACKUP_DESTINATION
StateBackupKeep        int    // OUTHAUL_STATE_BACKUP_KEEP, default 30
StateBackupPrefix      string // OUTHAUL_STATE_BACKUP_PREFIX

// core
type StateBackupRun struct {
	At        time.Time `json:"at"`
	OK        bool      `json:"ok"`
	Key       string    `json:"key"`
	SizeBytes int64     `json:"size_bytes"`
	Reason    string    `json:"reason"`
}

// store
func (s *Store) StateBackupRun(ctx context.Context) (core.StateBackupRun, bool, error)
func (s *Store) SetStateBackupRun(ctx context.Context, r core.StateBackupRun) error

// backup
const StateDir = "_outhaul-state"
func Stamp(t time.Time) string
func JoinKey(prefix string, parts ...string) string

type StateBackup struct {
	Schedule    string
	Destination string // name; "" = the kind=azure row
	Keep        int
	Prefix      string
}
func (m *Manager) SetStateBackup(s StateBackup)
func (m *Manager) RunStateNow()
```

- [ ] **Step 1: Write the failing config tests**

```go
// internal/config/config_test.go
func TestStateBackupSettings(t *testing.T) {
	// schedule "15 2 * * *" + the Azure pair -> nil
	// schedule + destination "minio" -> nil
	// schedule alone -> error naming OUTHAUL_STATE_BACKUP_SCHEDULE and both choices
	// schedule "15 2 * *" -> error naming the variable
	// destination without schedule -> error naming OUTHAUL_STATE_BACKUP_DESTINATION
	// prefix without schedule -> error naming OUTHAUL_STATE_BACKUP_PREFIX
	// keep set without schedule -> error naming OUTHAUL_STATE_BACKUP_KEEP
	// prefix "prod/state" -> nil; "prod//state", "/prod", "prod/" -> error
	// keep "-1", "many" -> error; unset -> 30
}
```

- [ ] **Step 2: Write the failing store test**

```go
// internal/store/backups_test.go
func TestStateBackupRunRoundTrips(t *testing.T) {
	// absent -> ok false
	// Set{At, OK: true, Key: "p/_outhaul-state/x.db.gz", SizeBytes: 42}
	// StateBackupRun -> ok true, equal fields
}
```

- [ ] **Step 3: Write the failing manager tests**

```go
// internal/backup/state_test.go
func TestStateBackupRun(t *testing.T) {
	// harness with a user, a destination named "minio", SetStateBackup{
	//   Schedule: "15 2 * * *", Destination: "minio", Keep: 2, Prefix: "prod"};
	// RunStateNow; waitRun;
	// one object "prod/_outhaul-state/<stamp>.db.gz";
	// gunzip and store.Open with the same box: the app and user are there, sessions are not;
	// the stored outcome is OK, with that key and its size; no backup-state-* file
	// remains in workDir.
}
func TestStateBackupSkipsWithoutAnAccount(t *testing.T) {
	// no user; RunStateNow; waitRun; no objects, no outcome, a log line says skipped.
}
func TestStateBackupRetention(t *testing.T) {
	// seed three old .db.gz objects in the directory; Keep 1; run; only the new one remains.
}
func TestStateBackupUsesTheAzureRow(t *testing.T) {
	// no destination name; SyncAzureDestination; dial records the destination's Kind;
	// run -> the dial saw azure.
}
func TestStateBackupMissingDestination(t *testing.T) {
	// Destination "nope"; run; outcome OK false, reason contains "nope"; no upload.
}
func TestStateBackupTickHonoursHold(t *testing.T) {
	// SetHold(true); Tick at the scheduled minute; no run.
}
func TestStateBackupStopCancels(t *testing.T) {
	// a dial whose Put blocks until ctx is done; Stop; the outcome says interrupted.
}
func TestStateBackupDoesNotOverlap(t *testing.T) {
	// a Put that blocks; RunStateNow twice; exactly one Put.
}
```

- [ ] **Step 4: Run them to see them fail**

Run: `go test ./internal/config/ ./internal/store/ ./internal/backup/ -run 'StateBackup'`
Expected: FAIL (unknown fields and methods).

- [ ] **Step 5: Implement**

- `config`: `p.cron` and `p.keyPrefix` helpers; the four fields; an unexported `stateBackupKeepSet` records whether `OUTHAUL_STATE_BACKUP_KEEP` was present; `Validate`:
  - schedule set needs `StateBackupDestination` or the Azure pair;
  - destination, prefix, or an explicitly set keep without a schedule is an error;
  - keep is `p.count` (default 30);
  - the prefix matches `^[A-Za-z0-9._-]+(/[A-Za-z0-9._-]+)*$`.
- `core`: `StateBackupRun`.
- `store`: `stateBackupRunKey = "state_backup_last"`; JSON marshal/unmarshal in `GetSetting`/`SetSetting`.
- `backup`: rename `stamp`/`joinKey` to exported `Stamp`/`JoinKey` and update callers. `StateDir = "_outhaul-state"`.
- Manager: `state StateBackup`; `SetStateBackup`.
  - `Tick` keeps its early `hold` return, then the schedules, then `tickState(ctx, now)`.
  - `tickState` parses the schedule and calls `RunStateNow` when it matches.
  - `RunStateNow` returns unless a schedule is set; `m.launch(0, func(ctx){ m.runState(ctx) })`.
  - `runState`: if `store.HasUser` is false, log `state backup: skipped: no account yet` and return without an outcome. Otherwise resolve the destination, `stage(true, …)` a gzip of a `Snapshot` temp file `backup-state-<Stamp>-<rand>.db` in `workDir`, upload to `JoinKey(prefix, StateDir, Stamp(now)+".db.gz")`, `prune` that directory with `Keep`, and record the outcome (success or `m.failure(ctx, err)`) with `context.WithoutCancel`. `defer os.Remove` the snapshot file, so it goes whether the copy stages or the run fails, not at the day-old sweep. Log `state backup: uploaded <key> (<size>)` on success and `WARNING: state backup failed: <reason>` on failure.
  - `stateDestination`: by name among `ListDestinations`, or the row with `Kind == DestinationAzure`; otherwise an error naming both choices.
- `main`: after the store is open, if `cfg.StateBackupDestination != ""` and no destination has that name, log `WARNING: OUTHAUL_STATE_BACKUP_DESTINATION=%q names no destination; state backups will fail until it is added in Settings`. After the manager is created, `backups.SetStateBackup(backup.StateBackup{...})` from the config, before `go backups.Run`.
- `docs/STATE-BACKUP.md`: the four settings, the schedule, the destination choice, retention, the prefix, and that an install with no account uploads nothing. `README.md`: the four rows.

- [ ] **Step 6: Run everything**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal main.go README.md docs/STATE-BACKUP.md
git commit -m "feat(backup): a scheduled copy of Outhaul's own state, to S3 or Azure"
```

---

## Task 9: The State backup panel and Run now

**Files:**
- Modify: `internal/server/server.go:59-67`, `internal/server/settings.go:30-67`, `internal/server/backups.go`, `internal/server/routes.go:140-152`, `internal/server/templates/settings.tmpl`, `internal/server/server_test.go` (`fakeBackups`), `main.go`
- Test: `internal/server/backups_test.go`

**Interfaces:**
- Consumes: `StateBackupRun` and the manager from Task 8.
- Produces:

```go
// internal/server/server.go
type Backups interface {
	// ... existing methods ...
	RunStateNow()
}

type StateBackupPanel struct {
	Schedule    string
	Destination string // the configured name, or "Azure Blob"
	Keep        int
	Prefix      string
}
func (s *Server) SetStateBackup(p StateBackupPanel)

// internal/server/backups.go
func (s *Server) handleRunStateBackup(w http.ResponseWriter, r *http.Request)
```

- [ ] **Step 1: Write the failing tests**

```go
// internal/server/backups_test.go
func TestStateBackupPanel(t *testing.T) {
	// before SetStateBackup: /settings does not contain "State backup".
	// SetStateBackup{Schedule: ""} -> still no panel (the schedule is what shows it).
	// SetStateBackup{Schedule: "15 2 * * *", Destination: "minio", Keep: 30, Prefix: "prod"}:
	//   the page contains all four, and "Run now".
	// SetStateBackupRun{OK: true, Key: "prod/_outhaul-state/x.db.gz", SizeBytes: 1234}:
	//   the page contains the key and "1.2 KiB".
	// SetStateBackupRun{OK: false, Reason: "destination \"nope\" does not exist"}:
	//   the page contains the reason.
}
func TestRunStateBackupNow(t *testing.T) {
	// e.srv.SetStateBackup(...); POST /settings/state-backup/run -> 303;
	// e.backups.stateRuns == 1.
}
func TestRunStateBackupNeedsSystemAdmin(t *testing.T) {
	// a project viewer's POST -> 403; no run.
}
```

- [ ] **Step 2: Run them to see them fail**

Run: `go test ./internal/server/ -run 'StateBackup'`
Expected: FAIL (undefined `SetStateBackup`, `stateRuns`, no route).

- [ ] **Step 3: Implement**

- `Backups` gains `RunStateNow`; `fakeBackups` records `stateRuns`.
- `Server` gets `stateBackup *StateBackupPanel`; `SetStateBackup` stores a copy.
- `renderSettingsWith`: when `s.stateBackup != nil && s.stateBackup.Schedule != ""`, set `data["StateBackup"]`; read `store.StateBackupRun` and set `data["StateBackupLast"]` when present.
- `settings.tmpl`: a "State backup" panel shown only with `.StateBackup`, showing schedule, destination, keep, prefix, the last outcome (`fmtTime`, `fmtSize`, key or reason), and a `POST /settings/state-backup/run` button with `Run now`.
- Route: `{"POST /settings/state-backup/run", levelSystemAdmin, nil, s.handleRunStateBackup}`. The handler refuses during an update with `updateHeldCopy`, calls `s.backups.RunStateNow()`, and redirects to `/settings`.
- `main`: `srv.SetStateBackup(server.StateBackupPanel{Schedule: cfg.StateBackupSchedule, Destination: dest, Keep: cfg.StateBackupKeep, Prefix: cfg.StateBackupPrefix})`, where `dest` is the configured name or `"Azure Blob"`.
- `docs/STATE-BACKUP.md`: the panel and what each line means. `README.md`: the feature bullet.

- [ ] **Step 4: Run everything**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/server main.go README.md docs/STATE-BACKUP.md
git commit -m "feat(server): a State backup panel and Run now"
```

---

## Task 10: The sealed columns and `CheckSealed`

Part 3's first commit. No visible change.

**Files:**
- Modify: `internal/store/sealed.go`
- Test: `internal/store/sealed_test.go`

**Interfaces:**
- Produces:

```go
// sealedColumn is one column whose non-empty values are sealed with the box.
type sealedColumn struct {
	table  string
	column string
	where  string // "" = every row; otherwise a SQL condition on that table
}

// CheckSealed opens every sealed value with s.box and returns how many it
// opened. It is how restore-state proves a supplied key fits the copy.
func (s *Store) CheckSealed(ctx context.Context) (int, error)
```

- [ ] **Step 1: Write the failing tests**

```go
// internal/store/sealed_test.go
func TestCheckSealedWithTheRightKey(t *testing.T) {
	// seed one of each: app SSH key, app env, project env, database password,
	// destination secret, the three github_app_sources secrets, the Cloudflare
	// token and the SSO client secret -> n == 10, nil.
}
func TestCheckSealedWithTheWrongKey(t *testing.T) {
	// seed a sealed value; open a second Store on the same file with a different
	// box -> error contains "decryption failed".
}
func TestCheckSealedWithNothingSealed(t *testing.T) {
	// empty store -> 0, nil; also an azure destination row with secret_key = '' -> 0, nil.
}
func TestCheckSealedWithoutABox(t *testing.T) {
	// a sealed value and a nil box -> error says to pass the key with --key-file.
}
```

- [ ] **Step 2: Run them to see them fail**

Run: `go test ./internal/store/ -run CheckSealed`
Expected: FAIL (undefined `CheckSealed`).

- [ ] **Step 3: Implement**

- Replace `sealedQueries` with `sealedColumns`:

```go
var sealedColumns = []sealedColumn{
	{"apps", "ssh_private_key", "ssh_private_key <> ''"},
	{"app_env", "value", ""},
	{"project_env", "value", ""},
	{"databases", "password", "password <> ''"},
	{"destinations", "secret_key", "secret_key <> ''"},
	{"github_app_sources", "private_key", ""},
	{"github_app_sources", "webhook_secret", ""},
	{"github_app_sources", "client_secret", ""},
	{"settings", "value", "key = '" + cloudflareTokenKey + "'"},
	{"settings", "value", "key = '" + ssoSecretKey + "'"},
}
```

- `HasSealedValues` builds `SELECT 1 FROM <table> [WHERE <where>] LIMIT 1` and keeps its early return.
- `CheckSealed` builds `SELECT <column> FROM <table> [WHERE <where>]`, skips `''`, and calls `s.box.Open` on each value. With no box and at least one value, return an error naming `--key-file`. Wrap a failure with the table and column.
- Keep the existing `TestHasSealedValues` passing unchanged; it is the guard that every kind is still counted.

- [ ] **Step 4: Run everything**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/store/sealed.go internal/store/sealed_test.go
git commit -m "refactor(store): one sealed-column list, and a check that opens every value"
```

---

## Task 11: `restore-state` from a file

**Files:**
- Create: `restore_state.go`, `restore_state_test.go`, `internal/core/restore.go`, `internal/store/restore.go`
- Modify: `internal/secret/box.go`, `main.go` (`main`, `usage`), `docs/STATE-BACKUP.md`
- Test: `internal/secret/box_test.go`, `internal/store/restore_test.go`, `restore_state_test.go`

**Interfaces:**
- Consumes: `Snapshot`/`StripSessions` from Task 7, `CheckSealed` from Task 10.
- Produces:

```go
// internal/secret/box.go
func NewBox(raw []byte) (*Box, error) // exactly 32 bytes

// internal/store/restore.go
var ErrDatabaseInUse = errors.New("store: database is in use")
func CheckDatabaseIdle(path string) error       // BEGIN EXCLUSIVE, no busy timeout
func HasUserInFile(path string) (bool, error)   // read-only
var ErrNotStateCopy = errors.New("store: not an Outhaul state copy")
var ErrNewerStateCopy = errors.New("store: the copy comes from a newer Outhaul")
func CheckStateCopy(path string) ([]string, error) // integrity, schema_migrations, unknown names
func (s *Store) AppliedMigrations(ctx context.Context) ([]string, error)
func (s *Store) StateCounts(ctx context.Context) (accounts, apps, databases int, err error)

// internal/core/restore.go
type RestoreMarker struct {
	RestoredAt   time.Time `json:"restored_at"`
	From         string    `json:"from"`
	Pending      bool      `json:"pending"`
	Databases    []string  `json:"databases"`
	Queued       []string  `json:"queued"`
	Failed       []string  `json:"failed"`
	WaitingPush  []string  `json:"waiting_push"`
	PrunedImages int       `json:"pruned_images"`
}

// internal/store/restore.go
func (s *Store) RestoreMarker(ctx context.Context) (core.RestoreMarker, bool, error)
func (s *Store) SetRestoreMarker(ctx context.Context, m core.RestoreMarker) error
func (s *Store) ClearRestoreMarker(ctx context.Context) error

// restore_state.go
type restoreOptions struct {
	From    string // a path (Task 11); later "latest" or a stamp (Task 12)
	KeyFile string
}
func parseRestoreOptions(args []string) (restoreOptions, error)
func runRestoreState(args []string, getenv config.Getenv, stdout, stderr io.Writer) int
func restoreState(ctx context.Context, cfg config.Config, opts restoreOptions, out io.Writer) error
func closeStaged(st *store.Store, path string) error // refuses a surviving -wal; removes a -shm
```

- [ ] **Step 1: Write the failing store tests**

```go
// internal/store/restore_test.go
func TestCheckDatabaseIdle(t *testing.T) {
	// an open Store -> ErrDatabaseInUse; after Close -> nil; a missing file -> nil.
}
func TestHasUserInFile(t *testing.T) {
	// no users -> false; one user -> true; a non-Outhaul SQLite file -> error.
}
func TestCheckStateCopy(t *testing.T) {
	// a snapshot -> the migration names, nil
	// a text file, a SQLite file with no schema_migrations -> ErrNotStateCopy
	// a snapshot plus "migrations/9999_future.sql" -> ErrNewerStateCopy naming it
	// a corrupt SQLite file -> an error naming integrity
}
func TestAppliedMigrationsAndStateCounts(t *testing.T) {
	// the snapshot has the embedded names; one user, one app, one database -> 1,1,1.
}
func TestRestoreMarkerRoundTrips(t *testing.T) {
	// absent -> ok false; Set{Pending: true, From: "latest"}; read it back;
	// Clear -> absent.
}
```

- [ ] **Step 2: Write the failing command tests**

```go
// restore_state_test.go
func TestParseRestoreOptions(t *testing.T) {
	// --from ./copy.db.gz --key-file /k -> {./copy.db.gz, /k}
	// no --from, a positional argument -> error
}

func TestRestoreStateFromGzipAndSQLite(t *testing.T) {
	// source: a store with a user, app, database, sealed value and a session;
	// Snapshot to copy.db; gzip one copy; keep the other plain.
	// target: an empty data dir. For each copy:
	//   runRestoreState(--from <file> --key-file <key>) == 0;
	//   open target outhaul.db with store.Open and the key: user/app/database there,
	//   sessions 0, marker Pending true; secret.key equals the key; key file untouched.
}

func TestRestoreStateStripsSessionsFromAPlainCopy(t *testing.T) {
	// copy the source database file before Snapshot, so the plain SQLite file
	// still has the session row; restore it -> 0; the target has 0 sessions and
	// the session token's bytes are gone from the file.
}

func TestRestoreStateRefusals(t *testing.T) {
	// no data dir -> "install Outhaul first"
	// target DB open -> "stop Outhaul first"
	// target DB with a user -> "fills an empty install only"
	// text file; gzip of text; SQLite without schema_migrations -> "not an Outhaul state copy"
	// copy with a 9999 migration -> "newer Outhaul"
	// sealed copy with no key and no target key -> "pass the original with --key-file" and
	//   "the key the fresh install made for itself can't work"
	// sealed copy with a wrong 32-byte key -> the same sentence
	// a --key-file that is not 32 bytes -> the same sentence
	// each leaves the target DB and key as they were; the staged file and both sidecars are gone.
}

func TestRestoreStateSwapsKeyAndDatabase(t *testing.T) {
	// target has old outhaul.db and old secret.key;
	// restore a copy with another key -> outhaul.db.before-restore-<stamp> and
	// secret.key.before-restore-<stamp> hold the old bytes; the new files are the copy's.
}

func TestRestoreStateMovesLeftoverWAL(t *testing.T) {
	// target has outhaul.db, outhaul.db-wal and outhaul.db-shm;
	// after restore no -wal/-shm sits beside the new outhaul.db; any that
	// remained after CheckDatabaseIdle (the -shm, and a -wal if it was not
	// folded) is at *.before-restore-<stamp>; the old database's rows are intact.
}

func TestRestoreStateClearsAStaleStagedWAL(t *testing.T) {
	// seed outhaul.db.restoring and its -wal from an earlier copy holding the
	// setting "stale" = "yes" (copy the pair while a writer still holds it);
	// restore a good copy -> 0; the target DB does not contain "stale", the
	// stale staged file and its sidecars are gone, and the good copy's settings are there.
}

func TestRestoreStateRefusesASurvivingStagedWAL(t *testing.T) {
	// open a store on a staged path; hold a second write transaction open so
	// its -wal cannot be checkpointed; closeStaged(st, path) -> an error naming
	// the -wal; the caller's cleanup removes the staged file and both sidecars.
}

func TestRestoreStateMigratesAnOlderCopy(t *testing.T) {
	// take a snapshot, then ALTER TABLE destinations DROP COLUMN kind and delete
	// the migrations/0030_destination_kind.sql row; restore -> 0;
	// the target has the kind column and the summary names 0030_destination_kind.
}

func TestRestoreStateAcceptsACopyWithNothingSealedWithoutAKey(t *testing.T) {
	// a copy with no sealed values and no --key-file -> 0; the target has no
	// secret.key (or keeps the one it had); summary says 0 sealed values opened.
	// the same copy with a wrong 32-byte --key-file -> 0, not an error.
}
```

- [ ] **Step 3: Run them to see them fail**

Run: `go test ./internal/store/ ./internal/secret/ . -run 'CheckDatabaseIdle|HasUserInFile|CheckStateCopy|AppliedMigrations|StateCounts|RestoreState|ParseRestore'`
Expected: FAIL (undefined functions, no `restore-state`).

- [ ] **Step 4: Implement the store and secret pieces**

- `secret.NewBox`: exactly 32 bytes, else `"secret key is %d bytes, want 32"`. `Load`/`LoadExisting` keep their path-bearing error by wrapping it.
- `CheckDatabaseIdle`: if the file is missing, nil. Open `file:<escaped>?mode=rw&_pragma=busy_timeout(0)&_pragma=locking_mode(EXCLUSIVE)`, `SetMaxOpenConns(1)`, `BEGIN EXCLUSIVE`; a `SQLITE_BUSY`/`database is locked` error is `ErrDatabaseInUse`; otherwise commit and close.
- `HasUserInFile`: `mode=ro`; `SELECT COUNT(*) FROM users`; a missing table is an error.
- `CheckStateCopy`: `mode=ro`; a file that is not a SQLite database is `ErrNotStateCopy`; `PRAGMA integrity_check` must be `ok`; `SELECT name FROM schema_migrations` (a missing table is `ErrNotStateCopy`); compare with the embedded `migrations/*.sql` names; unknown names wrap `ErrNewerStateCopy` with "install that version or newer, then restore"; return the names found.
- `AppliedMigrations`: `SELECT name FROM schema_migrations ORDER BY name`.
- `StateCounts`: the three counts.
- `core.RestoreMarker`; the store key `state_restore`, JSON in `settings`, with get/set/clear.

- [ ] **Step 5: Implement the command**

- `main`: before the `serve` check, `if os.Args[1] == "restore-state" { os.Exit(runRestoreState(os.Args[2:], os.Getenv, os.Stdout, os.Stderr)) }`. Add both usage lines.
- `parseRestoreOptions`: `flag.NewFlagSet` with `SetOutput(io.Discard)`; `--from` and `--key-file`; no positional arguments; `--from` is required.
- `runRestoreState`: load config, `Validate`, then `restoreState`; a usage error is 2, every other error 1, printed as `outhaul: <err>`.
- `restoreState`:
  1. The data directory must exist and be a directory. If `outhaul.db` exists, `CheckDatabaseIdle`; then `HasUserInFile` and refuse a non-empty install with the text the spec gives.
  2. Remove `outhaul.db.restoring`, `outhaul.db.restoring-wal` and `outhaul.db.restoring-shm`; open the source (gzip or SQLite by its first bytes) and write the staged file mode 0600. `CheckStateCopy`. From here, every refusal removes the staged file and both sidecars.
  2b. Key: `--key-file` read as exactly 32 raw bytes (`secret.NewBox`), else the data directory's `secret.key`; a missing key is nil, and `CheckSealed` refuses a copy with sealed values. The supplied file is left in place.
  3. `store.Open(staged, box)`; a migration failure removes the staged file and leaves the host alone.
  4. `CheckSealed`; the count is for the summary.
  5. `StripSessions`; `SetRestoreMarker{RestoredAt: now, From: label, Pending: true}`; `closeStaged` (below). A `-wal` that survives the close is a refusal, not something to delete: it holds the marker or migrations that did not reach the main file. A `-shm` is removed (it is only an index). Remove the staged file and both sidecars and stop.
  6. `CheckDatabaseIdle` again. Swap key first: if a key was supplied and differs, move `secret.key` to `secret.key.before-restore-<stamp>` and write the supplied key through a temp file mode 0600. Then move `outhaul.db`, `-wal` and `-shm` to `*.before-restore-<stamp>`, and rename the staged file to `outhaul.db`. Nothing is deleted: when a `.before-restore-<stamp>` path already exists, append `-2`, `-3`, … to the suffix rather than overwrite it.
  7. Print the stamp, the account/app/database counts, the sealed count, the migrations applied, the paths moved aside, and the first-start sentence from the spec.
- Every key refusal — no key, a key file that is not 32 bytes, or a sealed value that does not open — carries: `pass the original with --key-file. The key the fresh install made for itself can't work.`
- `closeStaged`: close the store; if `path+"-wal"` exists, return an error naming it; remove `path+"-shm"` if it exists. The caller removes the staged file and both sidecars on every refusal, including this one.
- Ownership: when running as root, `os.Chown` every file the command creates (the staged DB, the new key) to the data directory's uid/gid before the rename; mode 0600. If not root, skip the chown.
- The stamp: the object's stamp for `latest`/a stamp; the file's base name when it is a stamp; otherwise `backup.Stamp(now)`.
- `docs/STATE-BACKUP.md`: the restore section — what the command does, that `--key-file` holds the 32 raw bytes (the docs give `base64` lines for keeping it as text), each refusal, the key-first swap, and that the running-server check is a guard, not a lock.

- [ ] **Step 6: Run the tests**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add main.go restore_state.go restore_state_test.go internal/secret internal/store docs/STATE-BACKUP.md
git commit -m "feat: outhaul restore-state, from a file, with the key proved before anything is replaced"
```

**Review gate:** stop for a separate review of the swap before Task 13; Task 12 may proceed. The swap is where a mistake costs data.

---

## Task 12: `restore-state` from `latest`, a stamp, and `--list`

**Files:**
- Modify: `restore_state.go`
- Test: `restore_state_test.go`

**Interfaces:**
- Consumes: `NewAzureClient` from Task 3, `backup.JoinKey`/`StateDir` from Task 8.
- Produces:

```go
// restoreOptions gains
List bool

// openStateBlob is the Azure client for the configured account and container;
// tests replace it with one pointed at a fake.
var openStateBlob = func(cfg config.Config) (blobstore.Client, error)
```

- [ ] **Step 1: Write the failing tests**

```go
// restore_state_test.go
func TestRestoreStateList(t *testing.T) {
	// fake blob has p/_outhaul-state/20261110T021500Z.db.gz,
	// p/_outhaul-state/20261111T021500Z.db.gz and p/other/x.gz;
	// --list prints the two stamps, one per line, ascending, nothing else.
}
func TestRestoreStateLatest(t *testing.T) {
	// --from latest restores the 20261111T021500Z copy.
}
func TestRestoreStateStamp(t *testing.T) {
	// --from 20261110T021500Z restores that copy.
}
func TestRestoreStateNeedsTheAzurePair(t *testing.T) {
	// --list and --from latest without the pair -> 1, the message names both
	// variables and says to pass a file.
}
func TestRestoreStateNoCopies(t *testing.T) {
	// an empty container -> latest fails with "no state copies".
}
```

- [ ] **Step 2: Run them to see them fail**

Run: `go test . -run 'RestoreStateList|RestoreStateLatest|RestoreStateStamp|NeedsTheAzurePair|NoCopies'`
Expected: FAIL (no Azure path).

- [ ] **Step 3: Implement**

- `parseRestoreOptions`: add `--list`; it excludes `--from`, and `--from` is required otherwise.
- `openStateBlob`: if either Azure variable is empty, return an error naming both and "pass a file". Otherwise `NewAzureClient("https://<account>.blob.core.windows.net", container, NewTokenSource(cfg.AzureClientID))`.
- `stateDir(cfg) = backup.JoinKey(cfg.StateBackupPrefix, backup.StateDir)`.
- `listStateCopies`: `List(ctx, dir+"/")`; keep keys directly under it ending `.db.gz`; return the stamps ascending.
- `--list`: print each stamp. `latest`: the last; empty is an error. A stamp: `Get` the exact key.
- `restoreState`'s source branch: a file path, or `latest`/a stamp read through `openStateBlob`; both feed the same gzip detection and staging. A file named `latest` is `./latest`.
- `docs/STATE-BACKUP.md`: `--list`, `latest`, a stamp, and the systemd-run line. The line names `/etc/outhaul.env` and `/etc/outhaul/azure.env`; once the unattended installer lands, add `/etc/outhaul/install.env` and any `--env-file` the installer used.

- [ ] **Step 4: Run everything**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add restore_state.go restore_state_test.go docs/STATE-BACKUP.md
git commit -m "feat: restore-state from the Azure container, by latest, stamp, or --list"
```

---

## Task 13: The first-start pass and its notice

**Files:**
- Create: `internal/server/restorepass.go`, `internal/server/restorepass_test.go`
- Modify: `internal/server/server.go:50-81`, `internal/server/settings.go`, `internal/server/routes.go`, `internal/server/templates/settings.tmpl`, `internal/server/server_test.go` (`fakeDatabases`, `fakeRuntime`), `internal/gitrepo/repo.go`, `main.go`, `README.md`, `ARCHITECTURE.md`, `AGENTS.md`, `docs/STATE-BACKUP.md`
- Test: `internal/gitrepo/repo_test.go`, `internal/server/restorepass_test.go`

**Interfaces:**
- Consumes: `core.RestoreMarker` and its store get/set/clear methods from Task 11, `StateCounts`/`CheckStateCopy` from Task 11, `ProvisionSync` on `*dbaas.Manager`, `enqueueProtectionDeploy` and `appContainers` from `protection.go`, `LiveDeployment`, `RetainedImages`, `MarkImagePruned`, `ListImages`.
- Produces:

```go
// internal/server/server.go
type Databases interface {
	// ... existing methods ...
	ProvisionSync(ctx context.Context, d core.Database) error
}
type Runtime interface {
	// ... existing methods ...
	ListImages(ctx context.Context, refPattern string) ([]string, error)
}

// Server gains: restoreEvery time.Duration // default time.Minute; tests shorten it

// internal/server/restorepass.go
// RunRestorePass runs the pass while the marker is pending, once a minute,
// until it has run to its end or ctx ends.
func (s *Server) RunRestorePass(ctx context.Context)
func (s *Server) runRestorePassOnce(ctx context.Context, m core.RestoreMarker) error
func (s *Server) handleDismissRestoreNotice(w http.ResponseWriter, r *http.Request)

// internal/gitrepo/repo.go
func (m *Manager) Exists(app string) bool
```

- [ ] **Step 1: Write the failing gitrepo test**

```go
// internal/gitrepo/repo_test.go
func TestExists(t *testing.T) {
	// a fresh Manager -> false; Init(app) -> true; Remove(app) -> false.
}
```

- [ ] **Step 2: Write the failing pass tests**

Add to `fakeDatabases` a `provisionedSync []core.Database` and `ProvisionSync` (returning `failWith` when set); add to `fakeRuntime` a `byName map[string]*docker.Container` for `FindContainer`, and `images []string` for `ListImages` (filtered with `path.Match`).

```go
// internal/server/restorepass_test.go
func TestRestorePassOrderAndChoices(t *testing.T) {
	// marker Pending.
	// databases: "db1" running with no container; "db2" stopped -> only db1 provisioned.
	// apps: a (live outhaul/a:1, image present), b (live outhaul/b:1, image absent),
	//   c (live, container present), d (no live), p (preview, live outhaul/p:1, present),
	//   push (SourcePush, live, no repo).
	// runRestorePassOnce:
	//   ProvisionSync called once for db1;
	//   a queued with Image "outhaul/a:1", RollbackOf the live row;
	//   b queued with Image "";
	//   c and d not queued;
	//   p queued after a and b;
	//   push in WaitingPush;
	//   marker Pending false, Databases ["db1"], Queued ["a","b","p"].
}
func TestRestorePassRecordsFailures(t *testing.T) {
	// marker Pending; db1 running with no container and fakeDatabases.failWith
	// "pull failed"; app a live with no container and runtime.listErrOf["a"]
	// "docker down"; app b live with no container, healthy.
	// runRestorePassOnce -> nil (the pass ran to its end), marker Pending false;
	// db1's row is failed with "pull failed"; Failed contains
	// "database db1: pull failed" and "app a: docker down"; b is queued.
}
func TestRestorePassDoesNothingWithoutTheMarker(t *testing.T) {
	// RunRestorePass with a short interval returns; no database or deploy calls.
}
func TestRestorePassRunsOnce(t *testing.T) {
	// run a successful pass (marker Pending false); call RunRestorePass again
	// with a short interval; ProvisionSync calls and queued deployments do not grow.
}
func TestRestorePassRetriesUntilItRuns(t *testing.T) {
	// runtime.listErr set; RunRestorePass with restoreEvery 1ms; clear the error;
	// the marker becomes done and the pass ran at least twice.
}
func TestRestoreNoticeOnSettings(t *testing.T) {
	// pending marker -> "retries each minute"; done marker -> "Restored", the
	// lists and "Dismiss"; POST /settings/restore/dismiss -> 303 and the panel goes.
}
func TestDismissRestoreNoticeNeedsSystemAdmin(t *testing.T) {
	// a project viewer's POST -> 403.
}
```

- [ ] **Step 3: Run them to see them fail**

Run: `go test ./internal/gitrepo/ ./internal/server/ -run 'Exists|RestorePass|RestoreNotice|DismissRestore'`
Expected: FAIL (undefined functions and fields).

- [ ] **Step 4: Implement**

- `gitrepo.Manager.Exists`: `Path`, then `os.Stat(dir/HEAD)`.
- Server:
  - `RunRestorePass`: a ticker of `s.restoreEvery` (default `time.Minute`, tests shorten it). Read the marker; return when absent or not pending; call `runRestorePassOnce`; log an error and leave it pending for the next tick; stop when the marker is no longer pending or ctx ends.
  - `runRestorePassOnce`:
    1. Images: `RetainedImages` against `ListImages(ctx, "outhaul/*")`; every retained `outhaul/` tag not on the host gets `MarkImagePruned`; count them.
    2. Databases: `ListDatabases`; for each `Status == core.DBRunning` with no container (`FindContainer(dbaas.ContainerName(name)) == nil`), `ProvisionSync`. On error, `SetDatabaseStatus(ctx, db.ID, core.DBFailed, err.Error())`, append `"database <name>: <err>"` to Failed, and continue. (`ProvisionSync` sets `creating` first and never sets `failed`; the pass must.)
    3. Apps: `ListApps`; stable-sort ordinary apps before `Ephemeral` ones. For each with a `LiveDeployment` and no container of its own (`appContainers`, plus the compose project's containers for a compose app), skip a `SourcePush` app whose repo is missing (`s.repos == nil || !s.repos.Exists`) and append it to WaitingPush. Otherwise `serving` is the live row's image when it is not pruned and is on the host, else `""`; `enqueueProtectionDeploy(ctx, app, true, serving)`; append to Queued when it queued or a deployment is already in flight. An error listing the app's containers or enqueuing its deploy is recorded as `"app <name>: <err>"` in Failed, and the pass continues.
    4. Save the marker after each step with `Pending: true`, deduplicating the lists, so a retry keeps what already happened; on the last step set `Pending: false`, save, and log one line per outcome plus a summary.
  - A failure of a global list (`ListDatabases`, `ListApps`, `RetainedImages`, `ListImages`) returns an error so the pass retries; a per-database or per-app failure is recorded in Failed and the pass continues.
- `main`: after the server is built and the worker is running, `go srv.RunRestorePass(workerCtx)`.
- Settings: `renderSettingsWith` sets `data["RestoreNotice"]` from `store.RestoreMarker`; the template shows a panel with the pending text or the done details and a `POST /settings/restore/dismiss` button. The route is `levelSystemAdmin`; the handler clears the marker and redirects.
- Docs: finish `docs/STATE-BACKUP.md` with the first-start section, the "What a rebuild loses" table, the steps a person follows (verbatim from the spec) and the "What only the real host can show" list; add the marker/pass to `ARCHITECTURE.md`'s backups section, the kept rules to `AGENTS.md` (the sealed-column list, the Azure row belongs to the environment, the key and database move aside together, the marker and pass), and the `restore-state` commands to `README.md`.

- [ ] **Step 5: Run everything**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal main.go README.md ARCHITECTURE.md AGENTS.md docs/STATE-BACKUP.md
git commit -m "feat(server): the first start after a restore re-creates databases and apps, and says what is left"
```

---

## Final verification

- [ ] `go build ./... && go vet ./...`
- [ ] `go test ./...`
- [ ] `go test -race -timeout 30m ./...`
- [ ] `shellcheck -s sh --severity=warning deploy/test/*.sh`
- [ ] `git status --short` shows no `outhaul` binary and no uncommitted generated file.
- [ ] Re-read the spec's "Anti-goals" and "What only the real host can show": the first is covered by the tests above; the second belongs in `docs/STATE-BACKUP.md` and the three pull requests, and cannot be run here.
