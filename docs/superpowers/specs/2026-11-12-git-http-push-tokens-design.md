# git push over HTTPS, with push tokens

Push-to-deploy through the admin host's HTTPS address, signed in with a token
that expires.

This covers requirement R18 of the brief "outhaul enhancements: running behind
Azure Front Door on a shared host", added on 12 November 2026. It builds on the
designs for R1 to R4 (`2026-11-11-proxy-ingress-design.md`) and for R12 to R16
(`2026-11-11-client-ips-streams-sign-in-design.md`). Code references are to
`3ca88d7`.

## Why

Push-to-deploy is SSH only. Behind a TLS-terminating CDN the host accepts
nothing but HTTPS on 443, and such a host keeps the SSH listener off
(`OUTHAUL_SSH_ADDR=off`, R4). So `git push` can't reach Outhaul at all.

Pushing over HTTPS through the CDN keeps everything in front of it (the CDN,
the required header, a WAF) and opens no new port.

## Goals

1. `git push`, `git fetch` and `git clone` for push apps over HTTPS on the
   admin host, off unless `OUTHAUL_GIT_HTTP=on`.
2. The rules of an SSH push, unchanged: the role check before anything is
   created, the same hook and deploy, the same first push.
3. One way to sign in on those paths: HTTP Basic with a push token. A token is
   random, shown once, stored as a hash, and always expires.
4. A token of an SSO account stops working when the person hasn't signed in to
   Outhaul with SSO for a set time.
5. A build that takes minutes keeps the response moving, so a CDN doesn't close
   it.
6. An install that doesn't set `OUTHAUL_GIT_HTTP` sees no change: no route, no
   panel, no new Traefik flag.

## Non-goals

- Signing in from git through the browser (Git Credential Manager's OAuth flow
  with Outhaul as the OAuth server). The brief leaves it as a possible
  follow-up.
- Tokens for anything else. There is no API, and a token opens no page.
- Any change to SSH pushes or to push keys. A push key has no expiry and no SSO
  rule, as today.
- Git's "dumb" HTTP protocol, and git LFS.
- A limit on the size of a push. SSH has none either.
- Extending a token. A new one replaces it.
- Counting sign-ins to protected routes as SSO sign-ins to Outhaul. They create
  no account and touch none, as today.

## Anti-goals

The work has failed, even if it runs, if any of these happen:

- An account password, a session cookie, or anything but a live push token gets
  a request past these paths.
- A token, or its hash, appears in a log line, a URL, a process's arguments or
  environment, or on any page after the one that shows it once.
- A token is sent over plain HTTP because Outhaul asked for it there.
- A role change, a revoked token or a deleted account still pushes on the next
  request.
- Someone who can send bad tokens can keep another person's account or token
  from working.
- A quiet build's push is closed by the CDN while nothing in the terminal says
  where the build is.
- A push over HTTPS can do something a push over SSH by the same person can't,
  or the other way round, beyond what this document lists.

## Constraints

From `ARCHITECTURE.md`, `AGENTS.md` and the brief:

- Git work shells out to the `git` CLI. HTTP is `net/http` with the standard
  router.
- Settings are `OUTHAUL_*` variables read through the `parser` in
  `internal/config/config.go`, and `Validate` refuses bad values.
- A new route goes in `routes()` with an access level.
- A long-lived handler waits on `s.streamContext(r)`.
- Request bodies are capped at 1 MiB (`limitBodies`, `server.go`). A push
  can't be.
- Traefik's routing isn't touched: the admin host's router already carries
  every path, and the required header with it.
- This repository is public: `example.com` names only.

## Settings

| Variable | Values | Default | Needs |
|---|---|---|---|
| `OUTHAUL_GIT_HTTP` | `on` or `off` | `off` | `OUTHAUL_PUBLIC_URL`, and it must be an `https://` URL |
| `OUTHAUL_PUSH_TOKEN_DEFAULT_DAYS` | a whole number of days, 1 or more | `7`, or the maximum when that is lower | `OUTHAUL_GIT_HTTP=on`. At most the maximum |
| `OUTHAUL_PUSH_TOKEN_MAX_DAYS` | a whole number of days, 1 to 365 | `30` | `OUTHAUL_GIT_HTTP=on` |
| `OUTHAUL_PUSH_TOKEN_SSO_MAX_AGE` | days as `7d`, or a duration as `OUTHAUL_HEALTH_TIMEOUT` takes (`36h`). At least one hour | `7d` | `OUTHAUL_GIT_HTTP=on` |
| `OUTHAUL_UPLOAD_TIMEOUT` | a duration, such as `10m` | unset: Traefik's own 60 seconds, as today | nothing. It is not only for git; see [Uploads that take over a minute](#uploads-that-take-over-a-minute) |

`Validate` names the variable and what it wants. One of the three token
settings without `OUTHAUL_GIT_HTTP=on` is refused, as
`OUTHAUL_SSO_GROUPS_CLAIM` is without its mapping.

With `OUTHAUL_GIT_HTTP=on` and no `OUTHAUL_UPLOAD_TIMEOUT`, `serve` logs one
line at startup: a push that takes more than 60 seconds to upload is cut off
by Traefik, and which setting lifts that. It isn't refused: small repositories
never meet it.

The first deployment adds:

```
OUTHAUL_GIT_HTTP=on
OUTHAUL_UPLOAD_TIMEOUT=10m
```

## The address

```
https://outhaul.example.com/git/<app>.git
```

That is `OUTHAUL_PUBLIC_URL`, then `/git/`, then the app's name. The name
without `.git` works too, as it does over SSH. Three routes exist, and only
with `OUTHAUL_GIT_HTTP=on`:

| Route | For |
|---|---|
| `GET /git/{repo}/info/refs?service=git-receive-pack` or `=git-upload-pack` | the first request of every push, fetch and clone |
| `POST /git/{repo}/git-receive-pack` | a push |
| `POST /git/{repo}/git-upload-pack` | a fetch or a clone |

They are `levelPublic` in `routes()`, like the webhook routes: each request
signs itself in. Nothing else under `/git/` is routed, so the dumb protocol's
paths answer 404. With the setting off the routes are absent and answer 404.

**Only on the public URL's host.** A request whose `Host` isn't the public
URL's (`onPublicHost`, `sso_login.go`) answers 404 with no
`WWW-Authenticate` header. Git sends a token only after it has been asked for
one, so a remote that points at the admin port (`http://10.0.0.5:8080/git/…`)
never receives the challenge and never sends the token in the clear. The
public URL is `https://` by `Validate`. This is the same test the session
cookie's `Secure` flag uses, for the same reason: in tunnel mode
`X-Forwarded-Proto` says `http` while the person is on HTTPS, so that header
alone can't decide.

**Plain HTTP on the public host.** A remote written `http://outhaul.example.com/git/…`
reaches the same host. What happens to it:

- *Outhaul's own Traefik with TLS on:* port 80 redirects to HTTPS before the
  request reaches Outhaul (`proxySpec`), git follows, and the challenge is
  given over HTTPS.
- *A proxy in front that accepts plain HTTP and that Traefik trusts
  (`OUTHAUL_TRUSTED_PROXIES`):* Traefik passes on the proxy's
  `X-Forwarded-Proto: http`. When Traefik serves TLS itself and the tunnel is
  off (`s.tlsEnabled`, `store.TunnelEnabled`), Traefik never writes `http` in
  that header of its own accord, so `http` there means a trusted proxy said the
  person's connection was plain. Outhaul then answers 403 with no challenge and
  a line naming the `https://` address.
- *Anything else* (the tunnel, a Traefik that serves plain HTTP behind a proxy,
  a proxy Traefik doesn't trust): Outhaul can't tell the two cases apart and
  gives the challenge. The docs say the edge must redirect HTTP to HTTPS for
  the admin host.

**Not behind the gate.** These paths are on the admin host, which is never a
protected row. They are on the admin host's router, so the required header
(R3) applies with no change to Traefik.

**Cross-origin protection stays.** The routes are inside
`http.NewCrossOriginProtection`, as every route is. Git sends neither
`Origin` nor `Sec-Fetch-Site`, so it passes; a page on another origin doesn't.

## Signing in

Every one of the three requests is checked from the start. Nothing is
remembered between them: no cookie is read and none is set.

| Step | Refused with |
|---|---|
| 1. The request is on the public URL's host, and not known to be plain HTTP | 404 for the host, 403 for plain HTTP. No challenge |
| 2. It carries `Authorization: Basic …` | 401 and the challenge. Not a failed attempt: git's first request never has credentials |
| 3. The password is a live push token: the right shape, known, and not expired | 401 and the challenge. A failed attempt. Once the client address has none left (see [Failed attempts](#failed-attempts)): 429 and `Retry-After`, no challenge |
| 4. The token's owner may still use it: an SSO account's last SSO sign-in is recent enough | 403 |
| 5. The owner's role can change the app's project | 403 |
| 6. The app is a push app; for a fetch or clone, the app exists | 404 |

Then git runs. Steps 4 to 6 are not failed attempts: the token was good.

- **The challenge** is `WWW-Authenticate: Basic realm="outhaul"`.
- **The username is ignored.** An SSO account's internal name is a hash nobody
  can type (`core.SSOGateSubject`), so it can't be required. The docs say to
  type anything.
- **The password must be a push token.** Anything that isn't `ohp_` and 64 hex
  characters is refused at step 3 without a store lookup and without a
  password check, so an account password is never compared with anything and
  takes no hashing slot.
- **A session cookie is never read** on these routes.
- **Every refusal has a short `text/plain` body**, starting `outhaul: `. Git
  prints it as a `remote:` line (checked, for 401, 403 and 429).
- **401 or 403 matters to git.** After a 401, git tells the credential helper
  to forget the token it sent (checked). That is right for a token that is
  wrong, expired or revoked: the helper's copy is useless. After a 403 git
  keeps it (checked). So an SSO account past the limit gets 403: the token
  works again once the person signs in.

The messages:

| When | Body |
|---|---|
| Plain HTTP | `outhaul: this remote uses http://. Change it to <public URL>/git/<app>.git` |
| No credentials | `outhaul: sign in with any username and a push token as the password. Create one at <public URL>/account` |
| Unknown or wrong-shaped token | `outhaul: that push token isn't valid. Create one at <public URL>/account` |
| Expired token | `outhaul: that push token expired on <date>. Create a new one at <public URL>/account` |
| Too many failed attempts | `outhaul: too many failed sign-ins from your address. Try again in <wait>.` |
| SSO sign-in too old | `outhaul: your last SSO sign-in to Outhaul was more than <age> ago. Sign in at <public URL>/login, then push again.` |
| Role | `outhaul: you don't have permission to use '<app>'`, the SSH wording |
| Not a push app | `outhaul: app '<app>' is not push-deployable`, the SSH wording |
| Fetch of a name with no app | `outhaul: there is no app called '<app>'` |

Telling an expired token from an unknown one says something only to whoever
holds that token.

### The role check

The rule is the one `gitssh.authorize` applies today (`internal/gitssh/server.go`):
a system admin may use every project; anyone else needs project member or
above and a place on the app's project. A name with no app counts as the
default project. The role and the membership are read at each request, so a
change applies to the next one.

The part of `authorize` after the key lookup moves to one function that both
transports call, so the two can't drift.

Permission comes before the app's kind, as over SSH, so nobody learns from
these paths which app names exist in projects they aren't on.

### The SSO rule

For an account with `sign_in = 'sso'`, a token is refused while
`users.last_sign_in_at` is older than `OUTHAUL_PUSH_TOKEN_SSO_MAX_AGE`, or
empty. `finishLogin` (`internal/server/auth.go`) stamps that column, and for
an SSO account the only way to reach it is the SSO callback, so the column is
already "the last SSO sign-in to Outhaul". No migration is needed for it.

So someone removed at the identity provider loses push access within that
time, even while their Outhaul account still exists. Someone moved out of a
mapped group (`OUTHAUL_SSO_ROLE_GROUPS`) keeps the old role until their next
sign-in, as today, and the rule puts a limit on how long that can be for
pushes over HTTPS.

A password account is bound by the token's expiry only.

## Push tokens

### The token

`ohp_` followed by 64 lowercase hex characters: 32 bytes from `crypto/rand`,
256 bits. A secret scanner can match `ohp_[0-9a-f]{64}`.

The database holds only the SHA-256 of the whole token, in hex. A request's
token is hashed, the row is found by that hash, and the stored and computed
hashes are then compared with `subtle.ConstantTimeCompare`. A fast hash is
enough for a secret with 256 random bits; no hashing slot is used.

### The table

A new migration (the next free number) adds `push_tokens`:

| Column | |
|---|---|
| `id` | |
| `user_id` | `REFERENCES users(id) ON DELETE CASCADE`, as `push_keys.user_id` is |
| `label` | 1 to 64 characters, no control characters, unique within the account |
| `token_hash` | unique |
| `created_at`, `expires_at` | |
| `last_used_at`, `last_used_ip` | empty until first used |

Nothing here is sealed with `secret.key`, so `sealedColumns`
(`internal/store/sealed.go`) doesn't change.

**A state backup holds no tokens.** `stripSessions`
(`internal/store/snapshot.go`) empties `push_tokens` with the session tables,
both when the copy is made and when `restore-state` stages one. Otherwise a
restore would bring back a token that was revoked after the backup was taken.
After a restore everyone signs in again, as today, and creates a new token.

### Lifetime

- **Expiry is required.** The form asks for a number of days, from 1 to
  `OUTHAUL_PUSH_TOKEN_MAX_DAYS`, filled in with
  `OUTHAUL_PUSH_TOKEN_DEFAULT_DAYS`. The server checks the range.
- **The maximum also applies at each use.** A token is refused once it is older
  than `OUTHAUL_PUSH_TOKEN_MAX_DAYS`, whatever expiry it was created with, so
  lowering the setting shortens tokens that already exist. Pages show the
  earlier of the two times.
- **Revoking deletes the row.** The next request with that token is refused. A
  request already running finishes.
- **An expired token stays in the list**, marked expired, so its owner sees
  why pushes stopped. Outhaul deletes it 30 days after it expired.
- **An account holds at most 25 tokens**, expired ones included.
- **Deleting an account deletes its tokens**, through the foreign key.
- **A password change or reset leaves tokens alone.** Each has its own revoke
  button, and an expiry.
- **Whatever ends every SSO sign-in deletes SSO accounts' tokens.** Removing
  SSO, or changing its issuer or client ID, signs every SSO account out today
  (`endSSOSessions`, `internal/store/sso.go`: the Settings save and remove, and
  `SyncSSOStartup`). The same statement deletes those accounts' push tokens, in
  the same transaction. The sign-in a token rested on no longer counts, and
  under the SSO rule alone the token would have gone on working for up to
  `OUTHAUL_PUSH_TOKEN_SSO_MAX_AGE`. Password accounts' tokens stay. Push keys
  stay, as today.
- **Turning `OUTHAUL_GIT_HTTP` off** hides the panels and removes the routes.
  The rows stay and age out; a token that hasn't expired works again if the
  setting comes back.

### Last use

A request that passes step 3 records the time and the client address on the
token. A clone makes several requests, so the row is written at most once a
minute unless the address changed. A refused token records nothing.

### The Account page

A "Push tokens" panel, under "Push keys", only with `OUTHAUL_GIT_HTTP=on`:

- A table: label, created, expires (or "expired"), last used (time and
  address, or "never"), and a revoke button.
- A form: label, and "Expires in [7] days (at most 30)".
- For an SSO account, one line: "Your tokens work until <last sign-in plus the
  limit>. Sign in with SSO again to move that on."
- **Shown once.** The response to the create request renders the page with
  the token in a box, a copy button, and "Copy it now. It won't be shown
  again." The response carries `Cache-Control: no-store`. The token is never
  put in a URL, a cookie or the session, and nothing can show it again.

Routes: `POST /account/push-tokens` and
`POST /account/push-tokens/{id}/delete`, both `levelAccount` behind
`accountPage`, as the push-key routes are. Someone else's token id deletes
nothing.

### A system admin

The Users page gains a "Push tokens" panel, only with `OUTHAUL_GIT_HTTP=on`:
every account's tokens, with the owner, label, created, expires, last used,
and a revoke button. Route: `POST /users/{id}/push-tokens/{tokenID}/delete`,
`levelSystemAdmin`. No page shows a token or its hash.

The Users page's note about removing someone says their push tokens go with
their push keys.

## Running git

A new package, `internal/githttp`, is the HTTP transport, as `internal/gitssh`
is the SSH one. It runs the same two git commands SSH runs, with git's
`--stateless-rpc` flag, and not `git http-backend`: nothing new has to be
installed (some distributions package `git-http-backend` apart from git), no
second program parses the request's path, and Outhaul controls when the
response is flushed.

| Request | Command | Notes |
|---|---|---|
| `info/refs` | `git <receive-pack or upload-pack> --stateless-rpc --advertise-refs <dir>` | The answer is `# service=git-<service>\n` as a pkt-line, a flush packet, then git's output. With `Git-Protocol: version=2` on an upload-pack request the service line is left out, as `git http-backend` does. Content type `application/x-git-<service>-advertisement` |
| `git-receive-pack`, `git-upload-pack` | `git <service> --stateless-rpc <dir>` | The request body is git's standard input, and git's standard output is the response body. Content type `application/x-git-<service>-result` |

- **The directory** comes from `gitrepo.Manager.Path` for a name that passes
  `core.ValidAppName`, as over SSH. The bare repository is created on demand
  (`Manager.Init`) after the checks pass, as over SSH, with one difference: a
  fetch or clone of a name with no app answers 404 and creates nothing. Over
  SSH that creates an empty repository today; this design leaves SSH alone.
- **Flushing.** Every write git makes is flushed to the client at once
  (`http.ResponseController.Flush`). Without that, the server holds short
  lines back until 4 KiB has gathered.
- **The environment** is `PATH` and `HOME`, as `gitssh` sets, and
  `GIT_PROTOCOL` from the request's `Git-Protocol` header when that header is
  only letters, digits, `=` and `:`. A receive-pack also gets the two values
  under [The push's log line](#the-pushs-log-line).
- **Request bodies.** A body with `Content-Encoding: gzip` is unpacked before
  git reads it; git compresses large fetch requests. A `POST` whose content
  type isn't `application/x-git-<service>-request` answers 415.
- **No 1 MiB cap.** `limitBodies` leaves the two `POST` routes alone. A body is
  read only after the request has passed every check, so someone with no
  token can't make Outhaul read one.
- **Caching.** Every answer carries the three no-cache headers
  `git http-backend` sends (`Expires`, `Pragma`, `Cache-Control`).
- **Ending.** The command runs under `s.streamContext(r)`, so it ends when the
  client goes away and when shutdown begins. Git's standard error goes to
  Outhaul's log, a few lines at most per request.
- **If the hook bridge didn't start** (`startGitPush` logs why), the routes
  answer 503, as an SSH push fails today.

## A build that takes minutes

The push's response stays open while the build runs, and the build log streams
into the terminal as `remote:` lines, as over SSH. Nothing new carries it: the
`post-receive` hook's output is already sent by `git receive-pack` as sideband
messages inside the response, and with the flushing above each line arrives
as it is written (checked).

**Keeping the response moving.** While a hook prints nothing, git itself sends
an empty sideband packet every five seconds (`receive.keepAlive`, default 5).
Checked: a hook silent for 12 seconds produced one at 5 and one at 10. So a
quiet build sends bytes every five seconds, well inside the brief's 20. Outhaul
runs git with `-c receive.keepAlive=5` and `-c uploadpack.keepAlive=5`, so a
host's own git configuration can't turn that off. No heartbeat of Outhaul's
own is added.

**If the connection drops anyway.** The refs are stored before the hook runs,
and the deployment is queued by the hook's first exchange with the bridge. So
a push whose connection is cut mid-build has still pushed, and its deploy runs
to the end (checked for the git half: the ref had moved, and `receive-pack`
was stopped with the request). To let the person find it, the first line of a
deploy started over HTTPS is the address of its log:

```
remote: → build log: https://outhaul.example.com/deployments/42
```

An SSH push doesn't print that line: an install that sets nothing sees no
change.

**The time limit on following a deploy stays.** The bridge follows one push's
deploy for `OUTHAUL_HEALTH_TIMEOUT` plus five minutes (`startGitPush`,
`main.go`), six minutes by default, for SSH today. Past that the push prints
`✗ timed out after …` and ends, and the deploy carries on. The same limit
applies over HTTPS: it is the bridge's, and raising it would change SSH pushes.
A person whose build is longer has the log's address from the first line. The
docs say so, and say that raising `OUTHAUL_HEALTH_TIMEOUT` raises it.

A failed deploy still ends `git push` with status 0, as over SSH. That is an
open item in `AGENTS.md` and isn't changed here.

### The push's log line

The brief asks for one line per push: who, which token, which app, which
commit. The transport knows the first three and the hook knows the fourth, so
the hook's request to the bridge carries what the transport knew.

- `githttp` sets two variables on the `git receive-pack` process: the token's
  row id and the client address. Git passes its environment to the hook.
  Neither is a secret.
- `outhaul git-hook` (`githook.RunHook`) sends them to the bridge as two new
  optional lines of its request (`wire.go`). The hook script on disk doesn't
  change, so existing repositories need nothing.
- The bridge (`githook.Bridge`) then knows the push came over HTTPS. It logs
  one line: the account's display name and id, the token's label and id, the
  client address, the app, and each ref with its new commit. It prints the
  build-log line above when a deployment is queued.
- A request without the lines is an SSH push, and is handled exactly as today.

A fetch or clone isn't logged. A refused request is logged at most once a
minute per kind of refusal (`onceAMinute`), with the client address.

**Never logged:** the `Authorization` header, the token, its hash, and the
username typed (people type passwords into that box).

## The client address

Outhaul reads no visitor's address today; the R12 design left that out on
purpose. R18 needs one for a token's last use, the log line, and the count of
failed attempts. So this adds one function, used by `githttp` only.

- **A connection that isn't from Outhaul's own Traefik** is taken at its own
  address, and its headers are ignored. That covers loopback, the admin port
  with `OUTHAUL_ADMIN_DIRECT=on`, and an app container calling the Docker
  gateway.
- **A connection from Traefik** is read from `X-Forwarded-For`, from the right:

  | `OUTHAUL_TRUSTED_PROXIES` | The client is |
  |---|---|
  | unset | the last entry. Traefik wrote it from its own connection |
  | addresses | the first entry from the right that is outside the list |
  | `all` | the entry before the last, or the last when there is one. `all` can't say how many proxies there are, so it is read as one |

  An entry may be an address, an address with a port, or a bracketed IPv6
  address with a port. An entry that is none of those ends the walk at the
  last good one.
- **Traefik's address** is `OUTHAUL_TRAEFIK_IP` when set. Otherwise Outhaul
  asks Docker for its Traefik container's address on the shared network
  (`docker.Client.ContainerIP`), and remembers the answer for a minute. While
  it can't be learnt, no header is believed, and everyone arriving through
  Traefik counts as one address.

This matches what R12 found Traefik sends: from a trusted peer, what arrived
and then the peer's address; otherwise the peer's address alone.

Why not believe the header from any private address: every app container
shares the Docker network with Traefik and can call the admin port directly.
A container could then choose the address Outhaul records, and use up another
person's allowance of failed attempts.

## Failed attempts

Counted per client address, never per username or per token, so nobody can
lock someone else out by knowing their name.

- **The allowance.** Each address may fail ten times at once, and earns one
  more attempt each minute, up to ten.
- **With none left**, a request from that address whose password isn't a live
  token answers 429 with `Retry-After` and no challenge, and isn't logged.
  Other addresses, the account and the token are untouched.
- **A live token is never held back.** A request with a good token goes
  through whatever its address has used up. Several setups make every client
  arrive as one address: the tunnel or a CDN with `OUTHAUL_TRUSTED_PROXIES`
  unset, a Traefik whose address can't be learnt, an office behind one address.
  Tokens expire after seven days by default, so an expired token in someone's
  credential helper or CI job is an ordinary event. If a used-up allowance held
  good tokens back, one stale CI job, or anyone at all, could stop every push
  through that address. That is the anti-goal above.
- **What counts:** a request refused at step 3. Not a request with no
  credentials, and not one refused for the SSO rule, the role or the app.
- **An IPv6 address** is counted by its /64.
- **In memory**, behind a mutex, with an injected clock, like the sign-in
  backoff (`internal/server/backoff.go`). A restart forgets it. The map holds
  at most 10,000 addresses; the one touched longest ago goes first, and an
  address back at ten is dropped.
- **Always on** with `OUTHAUL_GIT_HTTP=on`. It doesn't depend on
  `OUTHAUL_SIGNIN_BACKOFF`, which stays per username and for password
  sign-ins.

People behind one office address share one allowance. A CI job that retries
an expired token in a tight loop uses it up, and a colleague who then mistypes
a token is told to wait instead of being told the token is wrong. A colleague
with a good token isn't affected. Tokens always expire, so the docs say what an
expired token looks like in CI.

Guessing a token isn't what this stops: 256 random bits can't be guessed at
any rate, and a well-formed guess is still looked up after the allowance is
gone (one SHA-256 and one indexed read). The limit tells a broken client to
back off and keeps a flood out of the log.

## Uploads that take over a minute

Checked on `traefik:v3.7.6`: with its default settings, Traefik cut a request
whose body took 90 seconds to arrive at 60 seconds. That is its entrypoint
read timeout (`respondingTimeouts.readTimeout`), which covers the whole
request, body included. It applies to every app's uploads today, not only to
git.

`OUTHAUL_UPLOAD_TIMEOUT` sets it. Each entrypoint `proxySpec` declares gets:

```
--entrypoints.<name>.transport.respondingtimeouts.readtimeout=<value>
```

The flag is in the config hash, so changing the setting recreates Traefik
once. With the setting unset, `proxySpec` returns today's spec and hash. It
sits beside `OUTHAUL_TRUSTED_PROXIES` in `config.Config`, `ProxyConfig` and
`infraController.proxyConfig`, and like it changes no label.

It is its own setting because it changes what every app on the host gets, and
an install may want it without git over HTTPS, or git over HTTPS without it.

Outhaul's own server has no read timeout past the headers
(`newHTTPServer`, `main.go`), so nothing else is in the way on the host.

## Pages

**The app page.** `handleAppDetail` shows "Deploy with git push" for a push
app to someone who can change it. With `OUTHAUL_GIT_HTTP=on` the panel shows
the HTTPS remote:

```
git remote add outhaul https://outhaul.example.com/git/<app>.git
git push outhaul <branch>
```

with the tag line for a tag-deploy app, as today, and a note: "Git asks for a
username and a password. Type any username, and a push token from Account →
Push tokens as the password. Don't put the token in the remote's address."
The SSH lines are shown beside it when the SSH server is running
(`sshAddr() != ""`), and not otherwise. With `OUTHAUL_GIT_HTTP` off the panel
is as today.

**The Account page's push-keys text** names the HTTPS remote too when both
are on.

## Checked so far

On 12 November 2026, with throwaway programs and one throwaway container.

| Checked | On |
|---|---|
| A push, a clone and a fetch through a small server that runs `git receive-pack` and `git upload-pack` with `--stateless-rpc` and flushes each write. Git asked first with no credentials, then with them. The clone and fetch used protocol version 2 | git 2.53.0, Go 1.27.1 |
| A `post-receive` hook's lines reached `git push` as they were written, over 15 seconds. A variable set on `receive-pack` reached the hook | the same |
| While the hook was silent, `receive-pack` wrote the five bytes `0005\x01` every 5 seconds | the same |
| A 401 with a `text/plain` body, sent to a request with credentials: git printed the body as `remote:`, said "Authentication failed", and removed the token from the `store` credential helper | the same |
| A 403 and a 429 with a `text/plain` body: git printed the body and kept the token in the helper | the same |
| `git push` killed mid-hook: the ref had already moved, and `receive-pack` was stopped with the request | the same |
| `--advertise-refs` gives the same advertisement as the newer `--http-backend-info-refs` | the same |
| A 9 MB body sent at 100 kB/s through Traefik with default settings was cut at 60 seconds. With `readtimeout=600s` on the entrypoint it arrived whole | `traefik:v3.7.6` |

Not checked:

- Anything a real CDN does: whether it passes the response on as it arrives,
  how it treats a long upload, its limits on a request's size and time, and
  what its WAF makes of a pack.
- A pack big enough to still be arriving when git starts to answer. Git reads
  the whole request before it writes (its keep-alives wait for the end of the
  pack for that reason), so the server's ordinary half-duplex handling should
  do. If a test shows otherwise, `http.ResponseController.EnableFullDuplex`
  is the remedy.
- A gzip-compressed request body. Git sends one only for a large fetch
  request, and none of the checks made one.
- Git older than 2.53, other git clients, and the credential helpers the docs
  describe (macOS keychain, Git Credential Manager), which are written from
  their documentation.
- The whole path through Outhaul's Traefik. The R14 checks found Traefik
  passes a streamed response on as written; `integration_proxy.sh` checks
  this path once it exists.

## Tests

Test first, as `AGENTS.md` asks. The `githttp` tests drive the real `git`
client against `httptest`, with its configuration and credential helper in a
temporary directory.

- `internal/config`: each setting's values; each refused combination; the
  default days when the maximum is lower.
- `internal/store`: the migration; create, find by hash, list, revoke for the
  owner only; the 25-token cap and the unique label; deleting an account
  deletes its tokens; the database holds the hash and never the token; last
  use, and its once-a-minute write; a snapshot and `StripSessions` leave no
  token; ending SSO sign-ins deletes SSO accounts' tokens and keeps password
  accounts'.
- `internal/githttp`, the brief's acceptance list:
  - a valid token pushes, and a deployment is queued;
  - refused: a wrong token, an expired one, a revoked one, one older than the
    maximum; an account's own password; a valid session cookie with no token;
    a token whose owner's role can't change the project, with no repository
    created and no app; an SSO account past the limit, with the message, and
    let in again after a sign-in;
  - a first push creates the app under the suffix, with sign-in required when
    `OUTHAUL_PROTECT_NEW_DOMAINS` is on;
  - fetch and clone work; a clone of a name with no app creates nothing;
  - a new token starts `ohp_`;
  - with a hook that prints nothing, and `receive.keepAlive` shortened, the
    response never goes quiet for longer than the interval;
  - repeated bad tokens from one address get 429, while a good token still
    works from that same address and from another;
  - a request on another host gets 404 and no challenge;
  - with TLS on and the tunnel off, a request marked
    `X-Forwarded-Proto: http` gets 403 and no challenge; with the tunnel on it
    is challenged as usual;
  - the log captured across all of the above holds no token and no hash.
- The limiter alone, with a fake clock: the allowance, the refill, the /64,
  the cap.
- The client address: each row of its table, a direct connection, an unknown
  Traefik address, entries with ports.
- `internal/githook`: a request with the new lines logs the push and prints
  the build-log line; one without behaves as today.
- `internal/server`: the routes are absent with the setting off; the Account
  panel creates, shows once, lists and revokes; another account's token id
  deletes nothing; the Users panel revokes anyone's; the app page's HTTPS and
  SSH blocks in each combination; the two `POST` routes take a body over
  1 MiB and every other route still refuses one.
- `internal/traefik`: `proxySpec` with the upload timeout, per mode; the spec
  and hash unchanged without it.
- `deploy/test/integration_proxy.sh`, the brief's end-to-end case: through the
  stand-in CDN, a token made on the Account page and put in a credential
  helper pushes and deploys; `git clone` works; the same request sent
  straight to Traefik without the required header gets 404.

## Docs

- `docs/GIT-HTTP.md`, new: turning it on; creating a token; storing it in a
  credential helper (macOS keychain, Git Credential Manager,
  `git config credential.helper`); why not `https://user:token@…`; expiry and
  what to do when a token runs out, in CI too; the SSO rule; revoking; the
  limit on failed attempts; how long a push follows a deploy; and for a CDN:
  redirect HTTP to HTTPS for the admin host, don't cache or buffer `/git/`, a
  WAF that inspects request bodies may need an exclusion for `/git/`,
  `OUTHAUL_UPLOAD_TIMEOUT`, and `http.postBuffer` for a CDN that refuses a
  chunked upload. Then the list under
  [What only the real host can show](#what-only-the-real-host-can-show).
- `README.md`: the configuration table.
- `docs/STATE-BACKUP.md`: push tokens are left out of the copy, with the
  sessions.
- `docs/BEHIND-A-PROXY.md`: the upload timeout, and a pointer to the new page.
- `docs/SSO.md`: the SSO rule for tokens.
- `ARCHITECTURE.md`: "Push-to-deploy" gains the HTTPS transport; the package
  layout gains `githttp/`; the locked "Auth" entry gains one sentence and
  loses none: "A push token signs `git` in over HTTPS and nothing else
  (opt-in)."
- `AGENTS.md`, "Decisions to keep": push tokens (Basic only, hashed, always
  expire, 401 against 403, the SSO rule); that Outhaul believes
  `X-Forwarded-For` only from its own Traefik, and only `githttp` reads it;
  and under "Login", that failed push tokens are counted per address while
  password sign-ins stay per username.

## Order of work

Each step has its tests, and the docs it changes.

1. `OUTHAUL_UPLOAD_TIMEOUT`. It stands alone.
2. The settings, the migration, the store, and the Account and Users panels.
3. The client address and the limiter.
4. `internal/githttp`, the routes, and the body cap's exception.
5. The hook's new lines, the log line and the build-log line.
6. The app page, `docs/GIT-HTTP.md` and the other docs.
7. `integration_proxy.sh`.

## What only the real host can show

For the staging host and its CDN.

1. `git push` with a token in a credential helper deploys end to end, and the
   build log arrives line by line, not in lumps.
2. A build with a step that prints nothing for four minutes, and under six
   minutes in all: the push stays open and ends with the result. (Past
   `OUTHAUL_HEALTH_TIMEOUT` plus five minutes the push ends with "timed out"
   and the deploy carries on, as over SSH.)
3. A push whose upload takes more than two minutes arrives whole.
4. A first push of a few hundred megabytes passes the CDN and its WAF.
5. `git clone` works.
6. A token's last use shows the pusher's own address, not the CDN's.
7. Ten bad tokens from one machine: the eleventh attempt says to wait, and a
   good token still pushes, from that machine and from another network.
8. A remote written `http://…`: the CDN redirects it, or Outhaul answers with
   the line naming the `https://` address. Either way git never asks for the
   token.
9. For an SSO account whose last sign-in is past the limit: the push prints
   the message, and works after signing in, with the same stored token.

## Left to the builder

- Names: the package's types, the setters, the store functions, the two
  variables the hook relays and the two request lines.
- Where the shared role check and the client-address function live.
- How `limitBodies` leaves the two routes alone.
- How the page shows the new token once: rendering in the create request's
  response is what is asked; the markup is free.
- Where the 30-day removal of expired tokens runs.
- The wording of messages and log lines beyond those quoted.
- The migration's number: the next free one when it lands.

## Calls made in this design

These go beyond the brief, or choose between readings of it.

1. Git runs as `receive-pack` and `upload-pack` with `--stateless-rpc`, not
   `git http-backend`. The brief allows either.
2. The build streams into the push. The brief's other option, ending the push
   with the log's address, isn't taken, but the address is printed first, so
   a dropped connection loses nothing.
3. Git's own five-second keep-alive is what keeps the response moving, pinned
   on the command line. The brief says "a line at least every 20 seconds";
   this sends bytes, not a visible line. A visible "still building" line
   would be a change to the bridge and would travel the same way.
4. `OUTHAUL_UPLOAD_TIMEOUT` is new and not in the brief. Without it Traefik
   cuts any upload at 60 seconds. It is a setting of its own, not something
   `OUTHAUL_GIT_HTTP=on` turns on, because it changes every app's uploads.
5. The username is ignored. The brief allows that or requiring the account's
   name.
6. A request not on the public URL's host gets 404 and no challenge, so a
   remote that points at the admin port is never asked for a token. Plain HTTP
   on the public host itself is call 21.
7. An expired, wrong or revoked token gets 401, which makes git forget it. The
   SSO rule and the role get 403, which doesn't.
8. The token is `ohp_` and 64 hex characters, hashed with SHA-256.
9. `OUTHAUL_PUSH_TOKEN_MAX_DAYS` also applies at each use, so lowering it
   shortens existing tokens.
10. A token can't be extended. Expired tokens stay listed for 30 days. An
    account holds at most 25.
11. A password change or reset doesn't revoke tokens.
12. A sign-in to a protected route doesn't count for the SSO rule. A push key
    has no such rule.
13. The token settings are refused without `OUTHAUL_GIT_HTTP=on`, and the
    panels are hidden with it off.
14. Outhaul reads the client address itself, for these paths only, and
    believes `X-Forwarded-For` only from its own Traefik. `all` is read as
    one proxy in front.
15. Failed attempts: ten at once and one more a minute, per address, IPv6 by
    /64. A live token is never held back (see call 20).
16. A fetch or clone of a name with no app creates nothing. Over SSH it still
    creates an empty repository.
17. A system admin sees everyone's tokens in one panel on the Users page.
18. With git over HTTPS on, the app page shows the SSH lines only while the
    SSH server runs.
19. Each push over HTTPS is logged once, by the bridge. Fetches aren't logged.

### Calls made after the builder check

A reviewer read this document as its builder and asked five questions. The
brief's author was away and had said to carry on, so these were settled without
them. Each changes a security property, so they are the ones to check first.

20. **A live token is never held back by the failed-attempt limit.** The first
    draft made a used-up address wait even with a good token. In several
    setups every client arrives as one address, and there anyone could have
    stopped every push. The cost: the limit no longer slows guessing at all;
    the token's 256 bits do that. Changing this back is one branch in the
    handler (answer 429 before the lookup).
21. **Plain HTTP is refused only where Outhaul can be sure of it:** Traefik
    serves TLS itself, the tunnel is off, and `X-Forwarded-Proto` says `http`.
    Everywhere else the edge's redirect and the docs carry it, because that
    header says `http` for people on HTTPS in tunnel mode. So the anti-goal
    "a token is sent over plain HTTP because Outhaul asked for it there" holds
    on Outhaul's own Traefik and behind a trusted proxy, and depends on the
    edge otherwise.
22. **The bridge's time limit stays** at `OUTHAUL_HEALTH_TIMEOUT` plus five
    minutes for both transports. A longer build ends the push with "timed out"
    while the deploy carries on; the log's address is the first line.
23. **A state backup holds no push tokens**, and `restore-state` strips them
    from a copy that has them, as with sessions.
24. **Ending every SSO sign-in deletes SSO accounts' push tokens.** The
    reviewer would have left them, as push keys are left. They are deleted
    because the brief ties a token to a recent SSO sign-in, and that sign-in
    has just been voided.
