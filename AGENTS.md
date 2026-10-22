# AGENTS.md

Guidance for coding agents (and people) working in this repository.

Outhaul is a single-binary, self-hosted PaaS: one Go binary plus a SQLite file,
driving Docker, Traefik and Nixpacks. Read these first:

- `README.md`: what it does, and how it is installed.
- `ARCHITECTURE.md`: locked decisions (do not revisit them), package layout,
  the deployment state machine, and the testing seams.
- `docs/`: local-CA HTTPS, manual deployment, and past design specs and plans
  (`docs/superpowers/`).

## Build, run, test

Go version: see `go.mod` (1.26.x). The installer is POSIX `sh`.

```sh
go build ./...
go vet ./...
go test ./...          # what CI runs
go test -race ./...    # run before calling a change done
sh deploy/test/run.sh  # installer unit tests (CI: when deploy/** changes)
shellcheck -s sh --severity=warning deploy/bootstrap.sh deploy/test/*.sh

sh deploy/dev/run.sh   # the full stack in a container, against the host Docker daemon
```

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
  and check `dash`, `bash --posix` and `busybox sh`.
- **Migrations** are embedded SQL in `internal/store/migrations`, numbered and
  applied in order. Never edit one that has shipped; add a new one. To test a
  data migration, seed old-schema rows with `openMigratedBefore`
  (`internal/store/migrate_webhook_token_test.go`).
- **Values sealed with `secret.key`.** When you add a sealed column, also add
  it to `sealedQueries` in `internal/store/sealed.go`. Startup uses that list
  to refuse to replace a lost key while values encrypted with it exist.
- **Long-lived HTTP handlers** (SSE) must wait on `s.streamContext(r)`, not
  `r.Context()`. Otherwise shutdown waits out its whole timeout.
- **New `OUTHAUL_*` settings** go through the `parser` in
  `internal/config/config.go`, so `Config.Validate` (run first by
  `outhaul serve`) reports malformed values instead of silently falling back
  to defaults.
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
- **Configuration is strict.** `Load` keeps defaults, so commands that only
  need paths still work, but `Validate` refuses bad values. Yes/no settings
  are case-insensitive, so `True` means on.
- **Login.** There is no per-IP or per-user backoff. Behind Traefik every
  request shares one remote address, and a per-user lockout lets an attacker
  lock out the only admin. The two-at-a-time limit on hashing caps the rate of
  guesses.

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
- Installer, tunnel and local-only modes: the firewall never opens the admin
  port, and no admin route is published. `deploy/test/integration.sh` is out
  of date and not run in CI.
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
  - the default project can be deleted;
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
