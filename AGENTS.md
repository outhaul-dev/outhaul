# AGENTS.md

Guidance for coding agents (and people) working in this repository.

Outhaul is a single-binary, self-hosted PaaS: one Go binary plus a SQLite file,
driving Docker, Traefik and Nixpacks. Read these first:

- `README.md`: what it does, and how it is installed.
- `ARCHITECTURE.md`: locked decisions (do not revisit them), package layout,
  the deployment state machine, and the testing seams.
- `docs/`: local-CA HTTPS, manual deployment, SSO, protected routes,
  unattended installs, and past design specs and plans (`docs/superpowers/`).

## Build, run, test

Go version: see `go.mod` (1.26.x). The installer is POSIX `sh`.

```sh
go build ./...
go vet ./...
go test ./...          # what CI runs
go test -race -timeout 30m ./...  # run before calling a change done
sh deploy/test/run.sh  # installer unit tests (CI: when deploy/** changes)
shellcheck -s sh --severity=warning deploy/bootstrap.sh deploy/update.sh deploy/test/*.sh

sh deploy/test/integration.sh  # install, re-run, upgrade and rollback in a systemd container (not in CI)
sh deploy/test/integration_gate.sh  # protected-route labels on a real Traefik (not in CI)
sh deploy/test/integration_ingress.sh  # DNS-01, the wildcard and HTTPS-only ports on a real Traefik, Pebble and challtestsrv (not in CI)
sh deploy/test/integration_proxy.sh  # a real Outhaul and Traefik behind a stand-in CDN and OIDC provider, end to end (not in CI)
sh deploy/test/integration_azure.sh  # the Azure Blob client against Azurite (not in CI)
sh deploy/dev/run.sh   # the full stack in a container, against the host Docker daemon
```

Under `-race`, `internal/server` takes more than ten minutes, longer than
`go test`'s default timeout, hence `-timeout 30m`.

CI's shellcheck is Ubuntu's 0.9.0, which reports some errors that newer
versions don't. `koalaman/shellcheck:v0.9.0` runs the same version in Docker.

`deploy/test/integration_gate.sh` proves the labels Outhaul gives protected and
open domain rows on a real Traefik, with a stub in place of Outhaul. It needs
Docker and Go, starts only `outhaul-gatetest-*` containers and a network of the
same name, on loopback ports, and removes them when it ends. None of them
carries `traefik.enable`, so a Traefik that exposes only labelled containers,
like Outhaul's own, ignores them. It also checks the forwarded headers on a Traefik that trusts
its peer and one that doesn't, that `X-Real-Ip`, `X-Forwarded-Uri`, `-Method`
and `-Prefix` are removed on both, and that a missing middleware file closes the
entrypoint. `--outhaul HOST:PORT` uses a running
Outhaul's gate instead of the stub; the script's header says how. That mode
hasn't been run against a real Outhaul yet.

`deploy/test/integration_ingress.sh` starts Pebble and
`pebble-challtestsrv` and a Traefik built from `traefik.ProxyFlags`, and
checks DNS-01, the wildcard and the HTTPS-only ports. It needs outbound DNS
from its Traefik to challtestsrv; it starts only `outhaul-ingtest-*`
containers, on loopback ports, and removes them when it ends.

`deploy/test/integration_proxy.sh` builds a privileged Debian container with
its own Docker daemon (`deploy/test/Dockerfile.proxytest`), runs a real Outhaul
and Traefik inside it, and puts a stand-in CDN (nginx) and OIDC provider
(`deploy/test/oidcstub`) in front. It starts only `outhaul-proxytest-*`
containers and removes them when it ends. It has found real defects (an admin
route the YAML parser rejected), so run it after changing how a router or the
admin route is written.

`go build .` leaves an `outhaul` binary in the repository root. Don't commit
it.

## Conventions

- **Test first.** Write a failing test, watch it fail for the right reason,
  then fix. Unit tests use fakes (`docker.Fake`, `compose.Fake`,
  `github.Fake`, and the fake runtime and deployer in `internal/server`).
  Many past bugs lived exactly at those seams: label semantics, redirects,
  hijacked streams, compose `.env` parsing. When a fix depends on how Docker,
  Traefik or GitHub really behave, also check it against the real thing.
  `internal/docker/cancel_test.go` shows how to fake the Engine API with
  `httptest`.
- **Installer tests.** Run them as files (`dash deploy/test/test_prompt.sh`),
  and check `dash`, `bash --posix` and `busybox sh`. The unattended path has
  its own files: `test_main.sh` (the flags and their refusals), `test_env.sh`
  (`install.env` and the drop-ins), `test_ref.sh` (ref resolution, the stamp
  and the marker), `test_nixpacks.sh` (the pin), `test_docker.sh` (Docker's
  three states), `test_firewall.sh` (ufw), `test_health.sh` (the service's
  ports, the header and the proxy checks), `test_swap.sh` (the careful swap
  and its exit guard) and `test_noninteractive.sh` (the whole run in a temp
  tree). `deploy/test/integration.sh` runs install, re-run, upgrade and
  rollback in a systemd container; it is not in CI.
- **The nixpacks pin.** `NIXPACKS_VERSION`, `NIXPACKS_SHA256_AMD64` and
  `NIXPACKS_SHA256_ARM64` in `deploy/bootstrap.sh` are the pin. To move it,
  change the three constants together — the version and both release assets'
  published SHA-256 — and update `docs/MANUAL-DEPLOY.md` and
  `docs/UNATTENDED-INSTALL.md`. `test_nixpacks.sh` asserts all three values.
- **Migrations** are embedded SQL in `internal/store/migrations`, numbered and
  applied in order. Never edit one that has shipped; add a new one. To test a
  data migration, seed old-schema rows with `openMigratedBefore`
  (`internal/store/migrate_webhook_token_test.go`).
- **Values sealed with `secret.key`.** When you add a sealed column, also add
  it to `sealedColumns` in `internal/store/sealed.go`, with a condition when
  the column may be empty. Startup uses that list to refuse to replace a lost
  key while values encrypted with it exist; `CheckSealed` uses the same list
  to prove a key against a state copy before `restore-state` replaces
  anything. The SSO client secret (`sso_client_secret`) is one example.
- **The Azure backup row belongs to the environment.** `SyncAzureDestination`
  keeps the single `kind=azure` row in step with
  `OUTHAUL_BACKUP_AZURE_ACCOUNT` and `..._CONTAINER`. It has no secret, it can't
  be added or removed on Settings while those settings are set, and when they
  are unset it is cleared but kept while a schedule still uses it.
- **`restore-state` moves, never deletes.** The old key and database go to
  `*.before-restore-<stamp>` together, key first; a survived `-wal` after the
  staged copy closes is a refusal, and the staged file is checked and stripped
  before anything is replaced. A copy from an older Outhaul is migrated on the
  staged file, so a migration failure leaves the host untouched.
- **The restore marker and first-start pass.** `restore-state` writes
  `state_restore` in settings; `serve` runs the pass while it is pending, once
  a minute, until it has run to its end. It only creates what is missing and
  records a per-target failure instead of stopping. See
  `docs/STATE-BACKUP.md`.
- **SSO accounts** are keyed by issuer and subject and are never linked by
  email. An SSO account has no password and can't use the password form. See
  `docs/SSO.md`.
- **Long-lived HTTP handlers** (SSE) must wait on `s.streamContext(r)`, not
  `r.Context()`. Otherwise shutdown waits out its whole timeout.
- **New `OUTHAUL_*` settings** go through the `parser` in
  `internal/config/config.go`, so `Config.Validate` (run first by
  `outhaul serve`) reports malformed values instead of silently falling back
  to defaults.
- **Pages that show secrets** must withhold them when `!canChange`, so a
  viewer sees that a value exists but not what it is.
- **Commits** look like `fix(scope): summary` or `feat(scope): summary`. The
  body says what the user saw go wrong, and what changed.

## Review remediation status (October 2026)

A whole-codebase review in October 2026 produced a ranked list of findings,
fixed in five batches. All five are on `main`.

### Done

- **Batches 1–2 (`7454682`):**
  - cross-origin protection and domain validation;
  - deleting an attached database no longer destroys its data;
  - backup retention stays inside its own schedule;
  - env scopes no longer overwrite each other (migration 0024);
  - `$` escaping in compose `.env`;
  - the headless installer works under `dash`;
  - each Traefik router names its service;
  - health checks don't follow redirects;
  - image pull errors reported inside the stream are surfaced;
  - clone tokens go in a header, not the URL;
  - safer cutover.
- **Batch 3 (`9222ca6`), PR previews:**
  - manifest permissions and the `pull_request` event;
  - previews are torn down when their parent app is deleted;
  - preview hostnames include the app name, and a host and path can belong to
    only one app;
  - a queued preview manager with a sweeper;
  - the per-app webhook URL token is separate from its HMAC secret
    (migration 0025).
- **Fork-PR previews (`f669ca5`):**
  - a fork builds `refs/pull/<n>/head`, gets only "Preview only" variables,
    and only Nixpacks apps get fork previews;
  - a fork is refused if its image carries `traefik.*` labels, its checkout
    has symlinks leading out of the repository, or its Nixpacks config sets
    `buildImage` or `runImage`;
  - turning the setting off tears fork previews down.
- **Batch 4 (`42dac25`), robustness:**
  - in-memory build logs are bounded;
  - log streams for finished deployments end at once, and open log streams end
    when shutdown begins;
  - Docker run and exec honour cancellation;
  - backups and restores stop cleanly on shutdown, are recovered at boot, and
    a failed restore says it may be partly restored;
  - `secret.key` is never replaced over encrypted data;
  - malformed `OUTHAUL_*` values stop startup;
  - changing the password signs out other sessions;
  - request bodies are capped at 1 MiB, and password hashing is limited to two
    at a time;
  - the installer normalizes the git port and public URL answers.
- **Batch 5 (`9904889`, `c763c55`, `82d9b3c`), data safety:**
  - a volume restore validates the archive before it empties the volume;
  - reprovisioning a database keeps the old container until the replacement
    is running;
  - a stale backup/restore helper container is cleared before its name is
    reused.

### Decisions to keep

- **Fork previews.** Fork code runs on the server, so it gets only "Preview
  only" variables and its own empty databases. Fork previews are Nixpacks-only:
  a fork's Dockerfile or Compose file would control how it is built and run.
  The UI hint states the remaining risk: a fork's code can reach other
  services on the internal Docker network, and fork builds have no resource
  limits.
- **Shutdown** cancels only the log streams, through `Server.CloseStreams`.
  It does not use `http.Server.BaseContext`, which would also cancel ordinary
  in-flight requests.
- **Build logs are in memory only.** The broker keeps the 50 most recently
  finished deployments' logs, and the last 10,000 lines of each log.
- **Streams.** Every SSE log stream sends `: ping` every 20 seconds. A
  reconnect clears the page and replays from the start, so no line appears
  twice and no event ids are needed. The pages retry a broken stream for five
  minutes, then ask for a reload. Build logs stay in memory only. See
  `docs/STREAMS.md`.
- **Configuration is strict.** `Load` keeps defaults, so commands that only
  need paths still work, but `Validate` refuses bad values. Yes/no settings
  are case-insensitive, so `True` means on.
- **Unattended installs.** `--non-interactive` never prompts or reads stdin,
  and changes nothing until every flag is checked. It never installs Docker
  without `--install-docker`, and never installs, enables or changes ufw
  without `--firewall ufw`. Its answers go to `/etc/outhaul/install.env` and
  the two `10-install.conf` drop-ins; it never writes `/etc/outhaul.env`.
- **The update request carries nothing.** In every mode, the request file in
  `/var/lib/outhaul/update` has no URL, ref or command, and the updater reads
  nothing from it. The target comes from the updater itself, and the mode from
  root's env files.
- **`OUTHAUL_UPDATES=off` is enforced twice.** The panel and the background
  check stop, and the updater itself removes the request and changes nothing.
  Either guard alone is enough, so an env file edited by hand still stops it.
- **Roles.** Roles are guardrails for trusted people, not containment: a
  project member's code still runs on the same server. New routes go in
  `routes()` with an access level; a route without one fails at startup. The
  last password system admin can't be removed or demoted: it is how people get
  in when SSO is down, and the only other way, `outhaul admin create`, needs a
  shell on the host. A `git push`, fetch or clone over SSH follows the same rule as the web:
  the key's owner must be a system admin, or project member or above on the
  app's project.
- **Login.** There is no per-IP backoff: behind Traefik every request shares
  one remote address. A per-username backoff exists only with
  `OUTHAUL_SIGNIN_BACKOFF=on`. It is in memory, and a lock ends by itself
  within 15 minutes. Someone who keeps guessing at a name can hold that name
  locked, so the way back in is a new account: `outhaul admin create`, run on
  the host. The two-at-a-time limit on hashing always applies.
- **Strict container policy is an allow-list.** A service key that isn't in
  `internal/compose/policy.go` is refused, including a key a newer Compose
  adds, so allowing a new key is a deliberate change with a test.
- **Top-level `x-*` keys** are the one exception: they are allowed as extension
  fields, and whatever an anchor puts into a service is checked in that
  service.
- **The Compose model is never logged.** It is `docker compose config` output
  with its variables interpolated, so it can carry secrets.
- **A refusal names the service and the option** (`service "web": ports: ...`)
  and never quotes a value from the file, so the deploy log says what to
  change.
- **Database ports.** Under strict mode a database publishes no host port: the
  form refuses one and provisioning omits it. See `docs/GUARD-RAILS.md`.
- **Limits.** Every path that creates a container Outhaul owns sets its limits:
  a single-container app and its health-check container use the app's, a
  Compose service gets what it didn't set itself through the override, a
  preview uses its parent's, and a database or helper uses the server default.
  Traefik and cloudflared get none. `app.Limits.Or(cfg.DefaultLimits)` fills
  each zero field; a changed limit applies when the container is next created,
  and a changed default redeploys nothing. A preview stores no limits. The
  Compose model is never logged, and the limits route and panel exist only
  when a default is set.
- **Build cap.** `OUTHAUL_MAX_CONCURRENT_BUILDS` (1 to 4) counts only
  deployments that have to build. A deployment that arrives with an image never
  waits for a slot, and with none free the queue offers only such a deployment,
  never one that jumps an older queued build of its own app. A slot is released
  when the deployment leaves `building`, not when its pipeline ends.
- **Protected routes.** The gate is defined in each app's own labels, never in
  the file provider: Traefik drops a router that names a middleware the file
  provider doesn't define, and its requests fall to an open router. Every open
  row removes the identity header (`X-Outhaul-User`), so a visitor can't send
  an app a forged one through Traefik. A row's Access state comes from the running
  containers' labels (`traefik.RowHasGate`), never from the stored flag. A
  one-time sign-in code works only in the browser that asked for it, through
  the pending cookie on the app's host. A protection change takes effect by
  redeploying, because Traefik reads it only from labels; Restart deploys
  instead when labels and rows disagree. Everyone who can sign in gets
  through: there is one group per server. Every rule ends in a term that
  refuses `..` path segments in any spelling, and every router names its
  priority as its rule's length without that term, so containers labelled
  before it keep their order; code matching a router to a row compares
  addresses (`traefik.RuleAddress`), not rule strings. Only a system admin can
  bring two projects together on a protected host. See
  `docs/PROTECTED-ROUTES.md`.
- **Behind a TLS-terminating proxy.** Every router is written from one
  `traefik.Routing` value (`AppLabels`, `compose.Override`, the admin route),
  never from separate positional settings. A rule is its address, the
  traversal term, then the header term when one is required; `priority` is the
  length of the address alone, and `traefik.RuleAddress` accepts the first two
  or all three, so an old container's rule still names its row. With a request
  header required, every routed container carries `outhaul.ingress=<stamp>`
  and Traefik's Docker provider runs with a constraint on that stamp; nothing
  else may create a route on a protected install. With `OUTHAUL_HTTP_PORT=off`
  no label or file-provider router may name the `web` entrypoint. The header's
  value is never logged, shown on a page or put in a flag other than Traefik's
  constraint and rules. `OUTHAUL_ADMIN_DIRECT=off` listens on loopback and the
  Docker gateway only. See `docs/BEHIND-A-PROXY.md`; the real-Traefik and
  end-to-end checks are `deploy/test/integration_ingress.sh` and
  `deploy/test/integration_proxy.sh`.
- **Trusted proxies.** `OUTHAUL_TRUSTED_PROXIES` is opt-in: unset, Traefik
  believes no forwarded header. It is held in `traefik.ProxyConfig`, never in
  `traefik.Routing`, and changes Traefik's flags and one file, never a router
  label or the ingress stamp. `all` is meant to be paired with the required
  header, and `serve` warns when it isn't. While trust is on, every entrypoint
  names the `outhaul-forwarded` middleware, which removes `X-Real-Ip`,
  `X-Forwarded-Uri`, `-Method`, `-Prefix` and the two `X-Forwarded-Tls-Client-Cert*`
  headers, and `underscoreheadersstrategy=delete` drops every header with an
  underscore in its name (needs Traefik v3.7.6 or newer); if the file is missing,
  the entrypoint answers 404 (fails closed). The file is written before Traefik
  is created, replaced by rename, and removed only after a replacement starts. It is the only middleware
  in the file provider, and that doesn't contradict the gate's rule: an
  entrypoint middleware that is missing closes the entrypoint, while a router
  that names a missing file-provider middleware is dropped and its requests fall
  to an open router. So the gate stays in each app's own labels, with
  `trustforwardheader=false`. See `docs/BEHIND-A-PROXY.md`.
- **The fixed network.** `OUTHAUL_NETWORK_SUBNET` gives Docker the gateway
  (the subnet's first address) and the upper half as its dynamic range;
  `OUTHAUL_TRAEFIK_IP` must be in the lower half, after the gateway. Outhaul
  never recreates a network that exists with another subnet: startup says so
  and the docs give the move.

### Open: reliability and UX

Nothing is planned for these yet. They come from the review, and haven't been
checked again since. Security-relevant open findings are deliberately not
listed here, because this repository is public. Ask the maintainer.

- A bad Cloudflare tunnel token switches Traefik to tunnel mode while
  `cloudflared` crash-loops.
- Domain rows saved with TLS off redirect, then 404, once HTTPS is enabled.
- Changing an app's build kind leaves the old runtime serving, and breaks
  compose apps.
- Saving the source form generates a new SSH deploy key even when nothing
  changed.
- Local-CA leaf certificates are not re-issued when the CA changes.
- Reinstalling a GitHub App breaks its git source. `ListRepos` ignores
  pagination, so only the first 100 repositories appear.
- Installer, tunnel and local-only modes: no admin route is published, so
  the admin UI needs an SSH tunnel. `deploy/test/integration.sh` is not run
  in CI.
- The blobstore client's 15-minute timeout includes the upload itself, so a
  large backup on a slow uplink fails.
- The build-log bound counts lines, not bytes, so output that redraws itself
  with `\r` makes one very long line.
- A cancelled exec frees Outhaul's goroutine, but the process inside the
  database container (e.g. `pg_dump`) keeps running.
- The build-log stream returns 404 for any store error, not only for an
  unknown deployment.
- Previews:
  - two apps on one repository overwrite each other's sticky PR comment;
  - previews in `teardown_failed` count toward the cap and are never retried;
  - fork previews created before the fork policy are not migrated;
  - the fork fetch is not pinned to the PR's head SHA.
- From the review's low-priority list:
  - a crash can leave a preview's status at "building";
  - deleting a branch triggers a deploy;
  - template deploys skip the certificate sync;
  - some forms save part of their changes before validating;
  - the "Running" count is taken from the latest deployment row;
  - temporary containers leak when they fail to start;
  - anonymous volumes leak on every deploy;
  - a cancelled Nixpacks build's child `docker build` keeps running;
  - compose services named `app` or `db` collide with canonical container
    names;
  - cron day-of-week ranges ending in 7 (`0-7`, `5-7`) are wrong;
  - the pruner treats store errors as "not in flight";
  - `githook.Serve` exits on an Accept error and leaves the listener open;
  - a failed deploy still exits `git push` with 0;
  - list pages make N+1 queries, and some pages call GitHub one request at a
    time.
