# Client addresses, long streams, sign-in backoff, and SSO from configuration

Trusting a proxy's forwarded headers, keeping log streams open, slowing
password guessing, and connecting SSO without the Settings page.

This covers requirements R12, R14, R15 and R16 of the brief "outhaul
enhancements: running behind Azure Front Door on a shared host" (10 November
2026). The four are independent: each is its own series of commits and can be
used without the others. It builds on the design for R1 to R4,
`2026-11-11-proxy-ingress-design.md`. Code references are to `06150cc`.

## Why

- **R12.** Behind a CDN, Traefik sees the CDN's address, and gives apps that
  address as the client's. Apps should be able to see the visitor's.
- **R14.** Build and log views are Server-Sent Events streams with no
  keep-alive. A build step that prints nothing for a few minutes sends nothing,
  and a CDN may close a connection that quiet.
- **R15.** The admin UI will be on the internet. The only limit on password
  guessing is the cap of two password checks at a time.
- **R16.** A rebuilt host should come up with SSO connected, and people
  shouldn't wait for an admin to give them a role at their first sign-in.

## Goals

1. A setting that makes Traefik trust forwarded headers from named addresses,
   without changing what the protected-routes gate is told.
2. Every stream sends something at least every 20 seconds, and a page whose
   stream breaks picks it up again with no repeated or missing lines.
3. An opt-in, temporary lock per username after repeated failed password
   sign-ins, and a command on the host that creates a system admin, so a lock
   can never keep the operators out.
4. SSO settings read from the environment, with the client secret from a file
   only root provides.
5. An opt-in mapping from the provider's groups to Outhaul's roles, applied at
   each sign-in.
6. An install that sets none of the new settings sees one change only: the
   stream fixes of goal 2 (see [Calls](#calls-made-in-this-design), 9).

## Non-goals

- Outhaul reading a visitor's address itself: in its log, or to limit
  sign-ins by address. R15 counts by username, as the brief asks.
- Turning on Traefik's access log.
- Event ids on streams, or resuming runtime logs from the exact line. A
  reconnect replays; see [Reconnecting](#reconnecting).
- Limits or locks on SSO sign-in. The provider does that.
- Resetting an existing account's password, or clearing a lock, from the
  command line. A new account is the way in; see
  [A system admin from the host](#a-system-admin-from-the-host).
- A different role per project for one person. Still a non-goal, as in
  `2026-11-02-sso-roles-protected-routes-design.md`.
- Sharing projects by group, groups read from the provider's UserInfo
  endpoint, nested claims, and more than one SSO provider.
- Writing the unit's credential line for R16. Whoever builds the host does
  that, in a drop-in. This design and `docs/SSO.md` give the line.

## Anti-goals

The work has failed, even if it runs, if any of these happen:

- A visitor can choose what the gate is told about a request, or an identity
  header reaches an app, because Traefik trusts a proxy.
- A reconnecting log view shows a line twice, or stops and says nothing.
- A lock can be made permanent, a few attempts show a visitor which usernames
  exist, or a flood of other names wins back guesses at an account.
- Every operator is locked out and nothing on the host gets them back in.
- The client secret appears in the environment, a command line, a log line, a
  page or the database when it was given as a file.
- A person removed from a mapped group keeps the role the group gave them
  after their next sign-in.
- A password account's role is changed by anything a provider sends.
- An SSO provider that is down at boot stops Outhaul starting.

## Constraints

From `ARCHITECTURE.md`, `AGENTS.md`, the brief and the R1 to R4 design:

- Settings are `OUTHAUL_*` variables read through the `parser` in
  `internal/config/config.go`. `Validate` reports bad values and bad
  combinations, and `outhaul serve` refuses to start.
- Streams are Server-Sent Events, not WebSockets (a locked decision). Build
  logs are in memory only, and long-lived handlers wait on
  `s.streamContext(r)` (decisions to keep). None of these change.
- SSO accounts are keyed by issuer and subject and never linked by email. An
  SSO account has no password. The last password system admin can't be removed
  or demoted.
- The gate's labels don't change. What depends on Traefik's behaviour is
  checked on a real Traefik.
- No secrets on a command line. This repository is public: `example.com`
  names only.

## Settings

| Variable | Values | Default | Needs |
|---|---|---|---|
| `OUTHAUL_TRUSTED_PROXIES` | IP addresses and CIDRs, comma-separated, or `all` | unset: trust nothing, as today | nothing. `all` without a required header (R3) is a warning at startup |
| `OUTHAUL_SIGNIN_BACKOFF` | `on` or `off` | `off` | nothing |
| `OUTHAUL_SSO_ISSUER` | the provider's issuer URL | unset: the Settings page, as today | the next two, and `OUTHAUL_PUBLIC_URL` |
| `OUTHAUL_SSO_CLIENT_ID` | text | unset | the other two |
| `OUTHAUL_SSO_CLIENT_SECRET_FILE` | an absolute path, or the name of a systemd credential | unset | the other two. The file must be readable at start and not empty |
| `OUTHAUL_SSO_ROLE_GROUPS` | `role:group` pairs, comma-separated | unset: roles by hand, as today | nothing: it works with SSO from the environment or from the Settings page |
| `OUTHAUL_SSO_GROUPS_CLAIM` | a claim name | `groups` | `OUTHAUL_SSO_ROLE_GROUPS` |
| `OUTHAUL_SSO_SCOPES` | extra scopes to request, separated by spaces or commas | none | nothing |

R14 has no setting. `outhaul admin create` is a command, so it has none
either.

`Validate` names the variable and what it wants:

- `OUTHAUL_TRUSTED_PROXIES`: each entry must parse as an address or a CIDR
  (IPv4 or IPv6). An empty entry, or `all` mixed with addresses, is refused.
- The three SSO variables: all or none. The issuer follows the Settings
  form's rules (the second half of `validate` in `internal/sso/sso.go`: an
  absolute `https://` URL, or `http://` on loopback). Split that check out,
  export it, and call it at startup.
  The secret file's value is an absolute path, or a name with no `/` in it.
- `OUTHAUL_SSO_ROLE_GROUPS`: the role is one of `core.AssignableRoles`
  (`system_admin`, `project_admin`, `project_member`, `project_viewer`). Each
  entry is split at its first colon, so a group name may hold colons and
  spaces but not a comma. Spaces around a role or a group are dropped. A
  group may be at most 256 bytes, and may appear under one role only. Several
  groups may give the same role. Don't read this variable with `firstField`:
  a group name may hold a space or a `#`.
- `OUTHAUL_SSO_GROUPS_CLAIM` without `OUTHAUL_SSO_ROLE_GROUPS` is refused.
- `OUTHAUL_SSO_SCOPES`: printable ASCII with no space, `"` or `\` in a scope.

The secret file is read in `serve`, straight after `Validate`, because `Load`
does no I/O. A file that is missing, unreadable, empty, or over 4 KiB stops
startup with the variable's name and the path, never the content.

The first deployment adds these to the R1 to R4 settings:

```
OUTHAUL_TRUSTED_PROXIES=all
OUTHAUL_SIGNIN_BACKOFF=on
OUTHAUL_SSO_ISSUER=https://idp.example.com
OUTHAUL_SSO_CLIENT_ID=<client id>
OUTHAUL_SSO_CLIENT_SECRET_FILE=sso-client-secret
OUTHAUL_SSO_ROLE_GROUPS=system_admin:outhaul-admins,project_admin:outhaul-engineers
OUTHAUL_SSO_SCOPES=groups
```

## R12: Trusting a proxy's forwarded headers

### Today

`proxySpec` (`internal/traefik/proxy.go`) gives no entrypoint a
`forwardedHeaders` option. So Traefik removes every `X-Forwarded-*` header and
`X-Real-Ip` that arrives, and writes its own from the connection. Behind a CDN
an app sees the CDN's address.

Outhaul reads a forwarded header in two places and a client address in one:

- `requestIsHTTPS` (`internal/server/auth.go`) reads `X-Forwarded-Proto`, to
  decide a cookie's `Secure` flag and the scheme of `baseURL` when no public
  URL is set. On an HTTPS public URL's own host the cookie is `Secure`
  whatever the header says.
- `readGateVisit` (`internal/server/gate.go`) reads `X-Forwarded-Host`, `-Uri`
  and `-Method` in `/gate/check`.
- `fromLoopback` (`internal/server/updates.go`) reads the connection's own
  address, never a header.

Cross-origin protection (`http.NewCrossOriginProtection`) uses `Origin`,
`Sec-Fetch-Site` and `Host` only.

### The flags

With `OUTHAUL_TRUSTED_PROXIES` set, every entrypoint `proxySpec` declares
(`web`, `websecure`, or the one that is left with `OUTHAUL_HTTP_PORT=off`)
gets one flag:

| Value | Flag, per entrypoint |
|---|---|
| addresses | `--entrypoints.<name>.forwardedheaders.trustedips=<a>,<b>` |
| `all` | `--entrypoints.<name>.forwardedheaders.insecure=true` |

`config.Config` gains the parsed list and an "all" flag. `ProxyConfig` gains
the same two fields, filled in `infraController.proxyConfig` (`main.go`), in
tunnel mode too. They are not part of `traefik.Routing`: no label changes, so
no app is redeployed and the R3 stamp is untouched. The flags are in the
config hash, so changing the setting recreates Traefik once. With the setting
unset, `proxySpec` returns today's spec and hash.

The addresses are the ones Traefik sees on the connection. In the probe, a
connection from the host itself arrived from the Docker bridge's gateway.
What a peer outside the host arrives as wasn't checked. An entry that matches
no peer trusts nobody, which is today's behaviour.

### What an app receives

Checked on `traefik:v3.7.6` (see [Checked so far](#checked-so-far)), sending
every header below with made-up values:

| Header | Today, or from an address that isn't trusted | From a trusted address |
|---|---|---|
| `X-Forwarded-For` | the peer's address | what arrived, then the peer's address |
| `X-Forwarded-Host`, `-Proto`, `-Port` | Traefik's own, from the request | what arrived; Traefik's own when none did |
| `X-Real-Ip` | the peer's address | what arrived; the peer's address when none did |
| `X-Forwarded-Uri`, `-Method`, `-Prefix` | removed | what arrived |
| `X-Forwarded-Server` | Traefik's host name | Traefik's host name |
| `Forwarded` | what arrived | what arrived |

So with a CDN trusted, the visitor's address is the entry before the last in
`X-Forwarded-For`: the CDN adds the visitor's, and Traefik adds the CDN's.
Anything further left is whatever the visitor sent. An app's framework should
be told two proxies are in front of it.

Trust is all or nothing per address: Traefik has no way to trust one header
and not another. `X-Real-Ip` is the trap. It is never the visitor's address
unless the proxy sets it, and if the proxy passes on what a visitor sent, the
visitor chooses it. Today no visitor can.

### Headers a proxy has no reason to send

So that the setting opens only what it is for, Traefik removes four headers
from every request while it trusts a proxy: `X-Real-Ip`, `X-Forwarded-Uri`,
`X-Forwarded-Method` and `X-Forwarded-Prefix`.

- `EnsureProxy` writes `outhaul-forwarded.yml` beside the admin route's file,
  before it creates Traefik, and removes it when the setting is unset, as
  `writeAdminDynamicConfig` does for its own. An empty value removes a
  header:

  ```yaml
  http:
    middlewares:
      outhaul-forwarded:
        headers:
          customRequestHeaders:
            X-Real-Ip: ""
            X-Forwarded-Uri: ""
            X-Forwarded-Method: ""
            X-Forwarded-Prefix: ""
  ```

- Each entrypoint gets `--entrypoints.<name>.http.middlewares=outhaul-forwarded@file`.
- The file provider's two flags and its mount are added whenever the setting
  is on. Today they are added only for the admin route and the local CA.
  With the setting on and no `DynamicDir`, `EnsureProxy` returns an error.

Checked: through a trusting entrypoint with this middleware an app got
`X-Forwarded-For`, `-Host`, `-Proto` and `-Port` as sent, and none of the
four. With the middleware missing, every router on that entrypoint is
disabled and answers 404, open ones too. So a lost file closes routes; it
never opens one. This is not the gate, which stays in each app's own labels.

An app that read `X-Real-Ip` gets none while the setting is on. Before, it got
the CDN's address there.

`X-Forwarded-Host`, `-Proto` and `-Port` can't be handled this way: removed,
an app would get none at all. They are the proxy's to set. Item 2 of the
real-host list checks that it does.

### The gate

A protected row's gate is a forward-auth middleware with
`trustforwardheader=false` (`identityMiddlewares`,
`internal/traefik/labels.go`). On every entrypoint checked, trusting or not,
the gate received `X-Forwarded-Host`, `-Uri`, `-Method`, `-Proto`, `-Port` and
`-For` written by Traefik from the request itself, whatever the client sent.
The client's `X-Real-Ip` and `X-Forwarded-Prefix` never reached it. The app
behind it received `X-Outhaul-User` from the gate's answer, and an open row's
app received none.

So the setting can be offered as the brief words it. Nothing a client sends
changes what the gate decides from. `deploy/test/integration_gate.sh` already
runs its forged-header checks through a Traefik that trusts everyone; the R12
series adds a `trustedips` Traefik and the four removed headers to it.

`/gate/check` can also be called on the admin host, through the admin route,
which has no forward-auth in front. A trusted proxy that passes on a
visitor's `X-Forwarded-Host` then reaches `readGateVisit` with it. That is the
case its comment already covers: anyone who can reach the admin port, an app
container included, can send those headers, and gets nothing without a live
cookie or code for the host they name. Nothing is added for it.

### `all`

`all` trusts whoever connects. It is safe only when nothing but the proxy can
reach Traefik's ports. On the first host that is two things together: the
network rule admits only the CDN's addresses, and the required header (R3)
refuses any tenant of the CDN but ours. A request without the header matches
no router, so its forwarded headers reach nothing.

`Validate` doesn't refuse `all` without a required header: a host whose
firewall admits one private proxy is a fair use. `serve` logs a warning at
startup instead, naming both settings. The docs state the pairing.

## R14: Streams that stay open

### Today

There are three streams, all in `internal/server`:

| Route | Handler | Source |
|---|---|---|
| `GET /deployments/{id}/logs` | `handleLogsSSE` (`sse.go`) | the broker (`internal/logstream`): the held history, then live lines |
| `GET /apps/{id}/logs` | `handleRuntimeLogsSSE`, then `streamContainerLogs` | Docker: the last `tail` lines, then a follow |
| `GET /databases/{id}/logs` | `handleDatabaseLogsSSE` (`databases.go`), then `streamContainerLogs` | the same |

- Nothing is written while the source is quiet.
- Events are `log`, `done` and `err`, with no `id` field. `handleLogsSSE`
  replays the whole history on every connection, and `streamContainerLogs`
  the whole tail.
- The pages (`deployment.tmpl`, `app.tmpl`, `database.tmpl`) open an
  `EventSource`, append each `log` line, and on an error only change a label
  to "disconnected".
- A `git push` gets its build output over SSH (`internal/githook`), not
  through the CDN. That isn't touched.

A browser reconnects an `EventSource` by itself after a network error, about
three seconds later. The handlers then replay, and the page appends, so every
line it already had shows twice. A browser does not retry when the answer
isn't a stream: a 502 from Traefik while Outhaul restarts, or the sign-in page
once the session has ended. The page then stays "disconnected". (This is the
HTML standard's rule. It wasn't tried in a browser for this design.)

Traefik is not what closes a quiet stream. On `traefik:v3.7.6` with default
timeouts, a stream that sent nothing for 130 seconds stayed open, over
HTTP/1.1 and HTTP/2, and comment lines sent every 20 seconds arrived as they
were written.

### Pings

Every stream writes the comment `: ping`, and flushes, each 20 seconds. A
client ignores a comment line: `EventSource` raises no event for it.

- `sse.go` gets `ssePingEvery` (a package variable, so tests can shorten it)
  and a `writeSSEComment` beside `writeSSE`.
- `handleLogsSSE` adds a ticker to its `select`.
- `streamContainerLogs` writes nothing until the first log line, so a quiet
  container's stream hasn't even sent its headers. Flush once the logs are
  open, so the page's `open` fires at once.
- `streamContainerLogs` blocks in `sc.Scan()` today. Move the scan into a
  goroutine that sends lines on a channel and closes it at the end of the
  stream, and `select` on the lines, the ticker and the context in the
  handler, so one goroutine does all the writing. The goroutine's send gives
  up when the context ends. The handler keeps waiting on `s.streamContext(r)`.

The interval is fixed and there is no setting. Twenty seconds is inside the
brief's 15 to 30, and puts two pings inside any 60-second limit.

### Reconnecting

The rule for all three pages: every time a stream opens, the page empties its
log box and takes what the server replays. The server already replays the
right thing, so no handler changes for this, and no event needs an id.

| The stream | What a new connection gets |
|---|---|
| A build still running | the log held so far, then live lines |
| A build that finished meanwhile | the log, then `done`. The page reloads and shows the result |
| A build whose log is gone: Outhaul restarted, or 50 newer builds finished | `done` at once. The page reloads, and says the log is no longer kept, as it does today |
| Runtime logs | the last lines the page's tail choice asks for, then live lines |
| Not a stream: the session ended, or Outhaul is still down | the page tries again itself; see below |

In the pages' script:

- On `open`, clear the box and any count the page keeps (`received` in
  `deployment.tmpl`), and set the label to "streaming…".
- On `error` with the `EventSource` still connecting, the browser is
  retrying: set the label to "reconnecting…".
- On `error` with it closed, open a new one after 2 seconds, doubling to 30,
  and stop after 5 minutes with "disconnected. Reload the page to try again."
  An `open` sets the wait back to 2 seconds. A `done` or `err` event still
  closes the stream for good, as today.

A build's log is at most 20,000 lines (`defaultMaxLines`, twice over), so a
replay is bounded. A viewer who had scrolled back loses their place on a
reconnect. Runtime logs come back as the last 100 to 5,000 lines, so lines
older than that leave the page; a larger tail choice brings them back.

### WebSockets

Outhaul's own pages use none. Apps' WebSockets pass through Traefik, and the
gate tells a WebSocket handshake from a page load already (`readGateVisit`).
The docs say that a CDN that passes WebSockets needs nothing from Outhaul.

## R15: Sign-in backoff, and a way back in

### Today

`checkPassword` (`internal/server/gate_signin.go`) serves both password forms:
`/login` (`handleLoginSubmit`, `auth.go`) and a protected route's sign-in
(`handleGateSignIn`). It looks the name up, and checks the password inside
`withHashSlot` (`password.go`), which lets two checks run at a time and
refuses a request that waited 10 seconds for a slot. A wrong name and a wrong
password get the same message. Nothing counts failures.

`AGENTS.md` keeps this on purpose ("Login"): a lock per username lets someone
who knows the only admin's name keep them out. The maintainer has accepted the
backoff, opt-in, given a command on the host that creates a system admin.

### The backoff

With `OUTHAUL_SIGNIN_BACKOFF=on`, failed attempts are counted per username,
as typed. Usernames are compared exactly, as the store compares them.

| Failed attempts in a row for one name | The next attempt for that name |
|---|---|
| 1 to 4 | is checked at once |
| 5 | is refused for 30 seconds |
| 6, 7, 8, 9 | is refused for 1, 2, 4, 8 minutes |
| 10 or more | is refused for 15 minutes |

- **A refused attempt** isn't checked, takes no hash slot, isn't counted, and
  doesn't make the wait longer. The page says "Too many failed sign-ins for
  this username. Try again in 4 minutes." (or "in less than a minute"), with
  status 429 and a `Retry-After` header. The same goes for the right
  password: it works once the wait is over.
- **Counting.** An attempt is counted, and any wait set, when it is let
  through, before its password is checked, so attempts sent together can't
  all be checked. A right password clears the name. An attempt that was never
  checked, because no hash slot came free, is given back. The Account page's
  check of the current password (`account.go`) isn't a sign-in and isn't
  counted.
- **What clears a name:** a successful sign-in; one hour with no failed
  attempt; a restart; and completing a setup link for that account
  (`handleAccountSetupSubmit`, `users.go`), which signs the person in without
  the password form anyway.
- **Names with no account** are counted and refused in the same way, with the
  same answers, so a few attempts don't show which names exist. That covers
  SSO accounts' internal names too.
- **Where it lives:** in memory, in a new `internal/server/backoff.go`: a map
  from name to count, last failure and locked-until, behind a mutex, with an
  injected clock. The key is the SHA-256 of the typed name: a typed name can
  be as long as the request cap allows, and first-boot setup puts no upper
  limit on a real one. Nothing is written to the database, so a flood of
  attempts costs no disk and no write lock.
- **Two maps.** One is for the names `GetUserByName` finds. It is as large as
  the number of password accounts, and nothing but the hour removes a name
  from it. The other is for every other name, and holds at most 10,000: when
  it is full, the name with the oldest failure goes. So a flood of made-up
  names can't push a real account's count out.
- **Where it is checked:** in `checkPassword`, after the store lookup, which
  says which map, and before the hash slot. A new error carries the wait, and
  `handleLoginSubmit` and `handleGateSignIn` render it through `loginPage`
  and `gateLoginPage`. `main` turns it on with a setter, called before
  `Handler`.
- **The hashing limit stays.** It caps memory and total work. The lock comes
  first, so a locked name can't take the two slots from everyone else.
- **Logging.** One line when an account's name is locked, at most once a
  minute (`onceAMinute`). A name with no account isn't logged: people type
  passwords into that box.

SSO sign-in doesn't pass through any of this.

### What someone who knows a username can and can't do

| They | What |
|---|---|
| can | keep that name's password sign-in refused, the right password included, for as long as they keep sending wrong ones: one checked guess every 15 minutes renews the lock |
| can't | make the lock outlast them: it ends 15 minutes after their last counted guess |
| can't | end a session that is already signed in (a password session lasts 7 days) |
| can't | lock a name they haven't guessed, or any SSO sign-in |
| can't | get more than about a hundred guesses a day at one account, however many other names they send. Today two hash slots allow several a second |
| can't | lock the operators out: a new system admin, made on the host under a name nobody has guessed, signs in at once |

One thing a lock can show. A made-up name's count can be pushed out by 10,000
newer made-up names, and an account's can't. So someone who locks a name,
sends that flood and tries the name again learns whether it is an account's.
The other way round, one map for all names, hides that, and lets the same
flood win back five guesses at a real account each time.

### A system admin from the host

```
sudo -u outhaul outhaul admin create NAME
```

It creates a password system admin called NAME. `main.go` dispatches it as it
does `ca`, `version` and `images`, and `usage` lists it.

- **The password** is asked for twice at the terminal, with no echo. When
  standard input isn't a terminal, the first line of it is the password. It
  is never an argument or a variable. The name and password pass the checks
  the UI makes (`validUsername` and `validateCredentials` in
  `internal/server`): 3 to 64 characters with no space and no `sso:` prefix,
  and a password of 8 or more.
- **It writes the database itself.** It opens `outhaul.db` with no secret
  box, hashes with `HashPassword`, and inserts with `store.CreateUser`
  (`internal/store/auth.go`), the function first-boot setup uses. The server
  reads accounts from the database on each request, so the account works at
  once, with the server running or stopped.
- **It finds the data directory as `ca root` does,** with `config.Load`:
  `/var/lib/outhaul`, unless `OUTHAUL_DATA_DIR` is in the command's own
  environment. It doesn't read the service's environment file.
- **It never creates or migrates a database.** `store.Open` does both, so the
  command needs a second way to open: fail if the file is missing, and fail
  if `schema_migrations` doesn't hold exactly this binary's migrations, with
  a message to use the installed binary.
- **Two processes on one database.** Each process has one connection, in WAL
  mode with a 5-second busy timeout. Checked with the repository's driver
  (`modernc.org/sqlite` v1.53.0): the command's one-statement insert waits
  for a write the server holds, then succeeds. A server transaction that read
  before the insert and writes after it fails once with "database is locked"
  (SQLite's snapshot rule), and works when tried again. So in the instant the
  command commits, one thing the server was doing in a transaction can fail
  once. Nothing is corrupted.
- **Who can run it.** The data directory is `outhaul`'s, mode 0700
  (`StateDirectoryMode` in `deploy/outhaul.service`), so only `outhaul` and
  root can open the database. Either could already edit it directly, so the
  command gives them nothing new. It refuses to run as any user but the
  database file's owner, and prints the `sudo -u` line: root would otherwise
  leave files there that the service can't write.
- **A name that exists** is refused: "There is already an account called
  NAME." It doesn't change that account.
- **Output.** `Created system admin "NAME".` on success, and exit status 0.
  Otherwise the reason, and status 1 (2 for a usage error). It never prints
  the password or its hash. It writes nothing to Outhaul's log: `sudo`
  records who ran it and with which name, and the account shows on the Users
  page with its creation time.
- **On a fresh install** it can create the first account. The one-time setup
  URL then stops working, as it does after any first account.

It doesn't reset a password or clear a lock. A lock is in the server's
memory, where another process can't reach it, and clearing one helps for five
guesses while someone is still guessing. A forgotten password is covered
already: create a new admin, sign in, and reset the old account from the
Users page.

### What the kept decisions say afterwards

`AGENTS.md`, "Decisions to keep":

- **Login.** "There is no per-IP backoff: behind Traefik every request shares
  one remote address. A per-username backoff exists only with
  `OUTHAUL_SIGNIN_BACKOFF=on`. It is in memory, and a lock ends by itself
  within 15 minutes. Someone who keeps guessing at a name can hold that name
  locked, so the way back in is a new account: `outhaul admin create`, run on
  the host. The two-at-a-time limit on hashing always applies."
- **Roles**, the last-admin sentence. The rule stays. Only its reason
  changes: "The last password system admin can't be removed or demoted: it is
  how people get in when SSO is down, and the only other way,
  `outhaul admin create`, needs a shell on the host."

`ARCHITECTURE.md`'s locked "Auth" entry gains one sentence and loses none: "A
system admin can also be created on the host with `outhaul admin create`."

## R16: SSO from configuration, and roles from groups

Two parts, each usable without the other.

### Settings from the environment

**Today.** A system admin saves the issuer, client ID and client secret on
Settings (`handleSaveSSO`, `internal/server/sso_settings.go`). They are stored
in `settings`, the secret sealed (`internal/store/sso.go`). Five places read
them with `store.GetSSOSettings`: `ssoProvider`, `ssoStillCurrent`,
`ssoPanel`, `handleSaveSSO` and `ssoConnected`.

**Reading.** With the three variables set, `serve` reads the secret file once
and hands the server the settings with a setter, called before `Handler`. All
five places go through one new method, `Server.ssoSettings`, which returns the
environment's settings when they were given, and the stored ones otherwise.

**Which wins.** The environment, for as long as it is set. Stored settings
are neither used, changed nor deleted, and are used again if the variables are
removed. The secret from the file stays in memory: it is never written to the
database. So this adds no sealed value, `sealedQueries`
(`internal/store/sealed.go`) doesn't change, and a rebuilt host with a new
`secret.key` connects to SSO with nothing to unseal. A stored
`sso_client_secret` still counts in `HasSealedValues`, as today.

**Starting.** Startup doesn't wait for the provider. Discovery runs on the
first sign-in, as today (`ssoProvider`), and one attempt runs in the
background at startup so the log says at once whether the provider answered.
A provider that is down never stops Outhaul.

**Sessions.** Saving a new issuer or client ID on Settings ends every SSO
sign-in in the same transaction. A restart must do the same when the
environment changes them. Outhaul remembers the issuer and client ID it last
ran with, in two plain `settings` rows:

| At startup | SSO sign-ins |
|---|---|
| The same issuer and client ID as last run. With no record yet, the stored settings count as last run's | kept |
| A different issuer or client ID, from either source | all ended (`endSSOSessions`), in one transaction with the new record, and logged |
| No settings from either source, and a record exists | all ended, and the record removed |

`SetSSOSettings` and `ClearSSOSettings` write the record in their own
transaction, so a change saved on Settings isn't counted again at the next
start.

Accounts are keyed by issuer, so a new issuer makes everyone a new account.
The Settings form asks for a confirmation before that. From the environment
nobody can be asked, so the log line says it.

**The Settings panel.** `ssoPanel` marks the settings as coming from the
environment, and `settings.tmpl` then shows the issuer, the client ID and the
redirect URI as text, with "The client secret was read from a file when
Outhaul started." There is no form and no Remove button. If settings are also
stored, one line says they are kept but not used. `handleSaveSSO` and
`handleDeleteSSO` refuse with 409, naming the variables. Settings is a
system-admin page, and the secret is on no page, as today.

**The secret file.** The brief wants a file only root provides. The service
runs as `outhaul`, which can't read one. systemd can: it reads a credential
as root when the unit starts and gives the service a copy only that unit's
user can read. The unit needs one line:

```
LoadCredential=sso-client-secret:/etc/outhaul/sso-client-secret
```

and the environment file one:

```
OUTHAUL_SSO_CLIENT_SECRET_FILE=sso-client-secret
```

The line goes in a drop-in under `/etc/systemd/system/outhaul.service.d/`,
written by whoever builds the host. `deploy/outhaul.service` doesn't change,
for two reasons. The installer and the updater replace the unit file, so a
line added on the host would be lost. And systemd's manual implies that it
won't start the unit when a file named by an absolute path is missing, so
the line belongs only on a host that has the file.

A value that isn't an absolute path is a name in `$CREDENTIALS_DIRECTORY`,
which systemd sets for the service. If that variable isn't set, startup stops
and says so. An environment file can't hold the directory itself, because
systemd expands nothing in one. An absolute path is read as it is, for a file
the `outhaul` user can read by other means.

One trailing newline is dropped from the content. Replacing the file takes
effect at the next restart.

### Roles from the provider's groups

**Today.** A new SSO account has no role, and waits on the Users page for a
system admin (`FindOrCreateSSOUser`, `internal/store/users.go`).
`2026-11-02-sso-roles-protected-routes-design.md` lists "Taking roles or
groups from the SSO provider" as a non-goal. This replaces that, opt-in.

**What a role is.** A person has one role, in `users.role`, for the whole
server (`internal/core/role.go`). Which projects it applies to is separate:
the projects shared with them (`project_members`). So
`project_member:<group>` gives the role and shares nothing. A project member
on no project sees no project and can't create one. A project admin can
create projects, and one they create is shared with them. To let people start
on their own at first sign-in, map their group to `project_admin`.

**The claim and the scope.**

- `sso.Config` gains the claim's name and the extra scopes. `sso.Identity`
  gains `Groups []string`. `Exchange` reads the claim from the ID token only
  when a name is given, and the server gives one only when the mapping is
  set.
- The claim is a top-level one, `groups` unless
  `OUTHAUL_SSO_GROUPS_CLAIM` names another. Its value may be an array of
  strings, or one string. Anything else, or no claim at all, is no groups: a
  provider may leave the claim out for a person in none.
- Group names are compared exactly.
- `Discover` requests `openid email profile` and then whatever
  `OUTHAUL_SSO_SCOPES` lists. Nothing is added by default, because providers
  differ: some send the claim only when a `groups` scope is asked for, and
  some refuse a scope they don't define.
- The token must hold the groups themselves. A token that only points at
  them (too many groups) reads as no groups.

**At each sign-in to Outhaul.** `handleSSOCallback`
(`internal/server/sso_login.go`) works out the highest role any of the
person's groups maps to, with a pure function in `internal/core`, and passes
it to `FindOrCreateSSOUser`, which applies it in its own transaction. A new
column, `users.role_from_groups` (a new migration, default 0), records that
an account's role came from a group.

| The person's groups | The account's role came from | The role after sign-in |
|---|---|---|
| map to a role | anything | that role. It is marked as from a group |
| map to nothing | a group | none. The mark is removed |
| map to nothing | an admin's hand, or the account is new | unchanged: today's behaviour |

With `OUTHAUL_SSO_ROLE_GROUPS` unset, none of this runs and the column is
never read.

- **A role given by hand.** `SetUserRole` clears the mark. For someone in a
  mapped group, the next sign-in sets the mapped role again, whether the
  hand-given one was higher or lower. So for those people the provider
  decides: to change their role, change their groups. An account deleted on
  the Users page comes back with its role at their next sign-in. The Users
  page says so beside the role of a marked account: "From an SSO group. A
  change here lasts until they next sign in."
- **When a change shows.** At the person's next sign-in to Outhaul. SSO
  sessions last 8 hours, and push keys go by the stored role, so someone
  removed from a group keeps the old role until then. To cut them off at
  once, also delete the account on the Users page, which ends its sessions.
- **A sign-in for a protected route** creates no account and changes no role
  (`finishRouteSSO`), as today.
- **The last password admin.** Only an SSO sign-in runs the mapping, and it
  only ever touches the SSO account for that issuer and subject. A password
  account's role can't be changed by it, so `guardLastPasswordAdmin` is never
  in question, and the rule holds as it is.
- **Logging.** One line when a sign-in changes a role, with the account, the
  old and new role, and how many groups the claim held. Group names aren't
  logged.
- **Pages.** `core.User` gains the mark, and `userCols` and `scanUser` read
  it. Settings lists the mapping, the claim's name and the extra scopes,
  read-only.

A `system_admin` mapping means whoever can change that group at the provider
can make system admins. The docs say so.

## Checked so far

On 11 November 2026, with throwaway containers and files. The sections above
give what each check found.

| Checked | On |
|---|---|
| What the gate and an app receive on five entrypoints: no trust, `insecure=true`, `trustedips` with and without the client's address, and `insecure=true` over TLS | `traefik:v3.7.6`. Routers and middlewares from the file provider, with the options `identityMiddlewares` writes as labels. The client was the host itself |
| The entrypoint middleware that removes four headers, present and missing | the same |
| Streams quiet for 130 seconds, and with a comment every 20, over HTTP/1.1 and HTTP/2 | the same, default timeouts |
| An insert from a second process, during the first one's write and during its read | `modernc.org/sqlite` v1.53.0, the store's own settings |
| `LoadCredential=` sets `$CREDENTIALS_DIRECTORY` and puts the file there, mode 0400, owned by the unit's user | systemd 259, a transient user unit |

Not checked:

- Anything a real CDN does: which forwarded headers it overwrites, and when
  it closes a quiet connection.
- The address Traefik sees for a peer outside the host.
- systemd reading a root-only file for a unit that runs as another user, and
  what it does when the file is missing. Both are from its manual.
- A real provider's `groups` claim. The mapping is tested against the
  stand-in in `internal/sso/sso_test.go`.
- The pages' reconnect script in a browser.
- Under an entrypoint middleware: router labels from the Docker provider,
  which the R12 series checks with `integration_gate.sh`, and the HTTP-01
  challenge, which it can't.

## Tests

- `internal/config`: each new setting's values, and each refused combination
  under [Settings](#settings).
- `internal/traefik`: `proxySpec` with addresses and with `all`, per mode,
  and with the file provider otherwise off; the spec and hash unchanged with
  nothing set (the golden files of the R1 to R4 plan, Task 1); the
  forwarded-headers file written and removed.
- `internal/server`, R14: with `ssePingEvery` shortened, each of the three
  handlers writes a comment while its source is quiet; `streamContainerLogs`
  still ends with `done`, sends its headers before the first line, and
  returns when the stream context ends (the fake runtime's `ContainerLogs`).
  No test in the repository runs a page's script, so the reconnect is checked
  by hand in a browser during the series (stop and start Outhaul with each of
  the three pages open), and by item 5 of the real-host list.
- `internal/server`, R15: the limiter alone, with a fake clock (each row of
  the table, the hour, the cap, a given-back attempt, attempts sent
  together); `/login` and `/gate/sign-in` answer 429 with the message; a
  locked name with the right password is refused, then let in after the
  wait; an unknown name locks the same way; an account stays locked after
  10,000 other names fail; nothing changes with the setting off.
- `main`, R15: `admin create` creates a system admin who can then sign in;
  refuses an existing name, a short password, a missing database and a
  database with other migrations; reads the password from standard input.
- `internal/store`: opening without migrating; `FindOrCreateSSOUser` for each
  row of the role table, and for a nil mapping; `SetUserRole` clears the
  mark; the new migration, seeded with `openMigratedBefore`; the startup
  record of issuer and client ID, each row of its table.
- `internal/sso`: the stand-in provider (`idp` in `sso_test.go`) sends the
  claim as an array, a string, another type, and not at all, and under
  another name; the extra scopes appear in the authorization URL. `sso.Fake`
  returns `Identity.Groups`.
- `internal/core`: the highest mapped role.
- `internal/server`, R16: settings from the environment win and the panel is
  read-only; saving and removing answer 409; a sign-in applies the mapped
  role and logs a change; a route sign-in changes nothing.
- `main`, R16: the secret file by absolute path and by credential name; each
  refused file.

## Docs

- `README.md`: the configuration table, and `admin create` under the
  commands.
- `docs/BEHIND-A-PROXY.md` (the R1 to R4 plan creates it; whichever series
  lands first does): trusted proxies, what an app receives, the two proxies
  to tell a framework about, `X-Real-Ip`, the pairing of `all` with the
  required header; streams and WebSockets; the list in
  [What only the real host can show](#what-only-the-real-host-can-show).
- `docs/PROTECTED-ROUTES.md`: one paragraph: the gate is told the same under
  trusted proxies.
- `docs/SSO.md`: settings from the environment and the unit line; roles from
  groups, the table, what `project_member` does and doesn't give, the Okta
  steps for a groups claim and scope (written from Okta's documentation
  until it is tried); and "Keeping a password system admin", which today
  says there is no command-line way back in.
- `ARCHITECTURE.md`: the Auth sentence above. The same locked entry says SSO
  is "configured on Settings" and that an SSO account "starts with no role";
  add "or in the environment" and "unless one of its groups is mapped to
  one". A section each for trusted proxies and for SSO from configuration,
  the second saying it replaces the 2026-11-02 non-goal, opt-in.
- `AGENTS.md`: the two entries above; that streams ping and replay on
  reconnect; that the forwarded-headers middleware is the one middleware in
  the file provider and why that is safe; that roles from groups never touch
  a password account.

## Order of work

One series per requirement, each with its tests and docs. The four can go in
any order.

1. **R14.** Pings; the pages' reconnect; docs.
2. **R15.** `outhaul admin create`, with the Roles wording and `docs/SSO.md`;
   then the backoff, with the Login entry.
3. **R16.** Settings from the environment; then roles from groups.
4. **R12.** The flags and the warning; the forwarded-headers middleware; the
   additions to `integration_gate.sh`.

R14, R15 and R16 don't depend on R1 to R4. R12 edits `proxySpec`, which that
plan rewrites, and its warning reads `Config.HeaderRequired` (Task 6), so it
goes after.

## What only the real host can show

For the infrastructure side to run on the staging host.

1. An app that prints its request headers, reached through the CDN:
   `X-Forwarded-For` ends with an address of the CDN's, and the entry before
   it is the visitor's.
2. The same request sent with made-up `X-Forwarded-Host`, `X-Forwarded-Proto`,
   `X-Forwarded-Port` and `X-Real-Ip`: note which the CDN overwrites. The app
   must not receive the made-up host or scheme, and receives no `X-Real-Ip`.
3. A protected app's sign-in still works through the CDN.
4. A build with a step that prints nothing for four minutes: the log page
   stays "streaming…", and the browser's network panel shows one `/logs`
   request for the whole build.
5. `systemctl restart outhaul` while a build log and a runtime log are open:
   both pages say "reconnecting…", then show the log once, with no line
   twice.
6. An app that uses a WebSocket works through the CDN.
7. Five wrong passwords for one name: the sixth attempt is refused with a
   wait, the right password included, and works after it. Meanwhile
   `sudo -u outhaul outhaul admin create NAME` makes an account that signs in
   at once.
8. With the secret in a file owned by root, mode 0600, and the unit's
   `LoadCredential` line: Outhaul starts, Settings shows the SSO panel
   read-only, and Okta sign-in works. `systemctl show outhaul -p Environment`
   holds no secret.
9. A first Okta sign-in by a member of a mapped group arrives with its role,
   and the log line gives a group count above zero. Taken out of the group,
   the same person has no role after their next sign-in.

## Left to the builder

- Names: the setters, the limiter's type, the store functions, the mark's
  field, the two `settings` keys.
- The wording of messages, notices and log lines, beyond those quoted.
- Whether the three pages share one script (a static file, or a block in
  `base.tmpl`).
- How the terminal's echo is turned off: `golang.org/x/term`, or the two
  calls it makes through `golang.org/x/sys/unix`, which is already in the
  module graph.
- How `validUsername`, `validateCredentials` and `sso`'s `validate` are
  shared with `main` and `config`: exported, or moved.
- Whether `config` imports `internal/core` for the role names. `core` imports
  no other Outhaul package, so there is no cycle.
- The migration's number: the next free one when it lands. Other designs in
  this batch may add migrations too.

## Calls made in this design

These go beyond the brief, or choose between readings of it. The maintainer
approved this design on 11 November 2026. Calls 9, 23 and 25 were put to the
maintainer as questions and stand as written. The rest were the designer's
calls, approved with it.

1. `OUTHAUL_TRUSTED_PROXIES` applies to every entrypoint, not only
   `websecure`, so it also serves a plain-HTTP install and tunnel mode.
2. `all` without a required header is a warning at startup, not a refusal.
3. While it trusts a proxy, Traefik removes `X-Real-Ip`, `X-Forwarded-Uri`,
   `-Method` and `-Prefix`, through one entrypoint middleware in the file
   provider. The brief asks only for the trust. Dropping this call removes
   one file, one flag per entrypoint and the file provider's flags, and
   leaves a visitor able to choose those four for every app.
4. Nothing is added for forwarded headers on the admin route.
5. Outhaul doesn't start reading client addresses itself. The brief's "apps
   and logs" is read as apps and their logs: Outhaul logs no requests, and
   Traefik's access log stays off.
6. Pings are every 20 seconds, fixed.
7. A reconnect replays and the page clears, for all three streams. No event
   ids, and no change to the broker or to `docker.Client`. "Resumes cleanly"
   is read as: the same log, once.
8. When the browser gives up, the page retries for 5 minutes, then stops.
9. The pings and the reconnect have no setting, so every install gets them.
   This is the one place the design departs from "an install that sets
   nothing sees no change". A comment line is invisible to a browser, and a
   log view that repeats its lines after a network blip is a fault on any
   install. The other way is a setting for the interval, off by default.
10. The backoff's numbers are fixed, behind one on/off setting, and it is in
    memory: a restart forgets it.
11. Names with no account are counted like real ones, in a map of their own
    capped at 10,000 names. A flood then can't reset an account's count, but
    it can show which names are accounts.
12. The right password is refused during a lock. Completing a setup link
    clears that name's count.
13. `outhaul admin create` only creates: no password reset, no unlocking.
    The maintainer asked for a way to add an admin from the command line;
    what else it does was left open.
14. The command writes the database directly, with the server running. The
    other way was a socket to the running server, which wouldn't work with
    the server stopped.
15. The command can create the first account on a fresh install, refuses to
    run as a user who doesn't own the database, and writes no line to
    Outhaul's log.
16. The last-password-admin rule stays, with a new reason.
17. The environment's SSO settings win, and stored ones are kept, unused.
18. Outhaul records the issuer and client ID it last ran with, so a change in
    the environment ends SSO sign-ins as a change on Settings does.
19. `OUTHAUL_SSO_CLIENT_SECRET_FILE` takes a credential's name as well as a
    path, so the unit needs one line.
20. A provider that can't be reached at startup is logged, not fatal.
21. The claim's name is a setting, and so are the extra scopes. Nothing is
    requested by default, so the brief's example needs
    `OUTHAUL_SSO_SCOPES=groups` at a provider that wants that scope.
22. Groups come from the ID token only. A missing claim is no groups.
23. A mapped group decides the role at every sign-in, whatever an admin gave
    by hand, and a role that came from a group goes when no group maps any
    more. The brief doesn't say what happens to a role a group gave once.
24. A sign-in for a protected route doesn't run the mapping.
25. `project_member:<group>` shares no project. The docs point to
    `project_admin` for people who should start on their own.
26. A group may be listed under one role only, and a group name can't hold a
    comma.
