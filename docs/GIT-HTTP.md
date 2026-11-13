# git push over HTTPS

This page is for an install that wants `git push`, `git fetch` and `git clone`
for push-to-deploy apps over HTTPS, through the admin host's address. The usual
reason is a host that sits behind a CDN and accepts nothing but HTTPS on 443, so
the SSH listener (`OUTHAUL_SSH_ADDR`) is off. It is off unless you set it, and
SSH pushes are not changed by it.

Signing in is one thing only: a push token, which you make on the Account page,
and type as the password. An account password, a session cookie and a push key
do nothing on these paths.

## Turning it on

```
OUTHAUL_GIT_HTTP=on
OUTHAUL_UPLOAD_TIMEOUT=10m
```

| Variable | Values | Default | Needs |
|---|---|---|---|
| `OUTHAUL_GIT_HTTP` | `on` or `off` | `off` | `OUTHAUL_PUBLIC_URL`, and it must be an `https://` URL |
| `OUTHAUL_PUSH_TOKEN_DEFAULT_DAYS` | a whole number of days, 1 or more | `7`, or the maximum when that is lower | `OUTHAUL_GIT_HTTP=on`. At most the maximum |
| `OUTHAUL_PUSH_TOKEN_MAX_DAYS` | a whole number of days, 1 to 365 | `30` | `OUTHAUL_GIT_HTTP=on` |
| `OUTHAUL_PUSH_TOKEN_SSO_MAX_AGE` | days as `7d`, or a duration such as `36h`. At least one hour | `7d` | `OUTHAUL_GIT_HTTP=on` |
| `OUTHAUL_UPLOAD_TIMEOUT` | a duration of whole seconds, such as `10m` | unset: Traefik's own 60 seconds | nothing |

`serve` refuses a malformed value and names the variable. One of the three token
settings without `OUTHAUL_GIT_HTTP=on` is refused too.

`OUTHAUL_UPLOAD_TIMEOUT` is not only for git: it is how long Traefik waits for a
whole request, body included, for every app. Without it, Traefik cuts off a push
whose upload takes more than 60 seconds. Small repositories never meet that, so
`serve` does not refuse the combination; it logs one line at startup when git
over HTTPS is on and the setting is not. See
[Uploads that take over a minute](BEHIND-A-PROXY.md#uploads-that-take-over-a-minute).

The address is `OUTHAUL_PUBLIC_URL`, then `/git/`, then the app's name:

```
https://outhaul.example.com/git/<app>.git
```

The name without `.git` works too. Only the public URL's host answers: a request
for any other host, such as the admin port reached by IP address, gets 404 and
no request for a password. So a remote that points at the wrong place never
receives the question, and never sends a token.

The app's page shows this remote, with the `git remote add` and `git push`
lines, to anyone who can change the app. The SSH lines are shown next to it
only while the SSH server is running.

## Creating a token

Sign in, open Account, and use "Push tokens". Give it a label (1 to 64
characters) and a number of days. The form starts at
`OUTHAUL_PUSH_TOKEN_DEFAULT_DAYS` and allows up to
`OUTHAUL_PUSH_TOKEN_MAX_DAYS`.

- **The token is shown once.** The page that answers the form shows it with a
  copy button. It is not in a URL, a cookie or the session, and nothing can show
  it again. Outhaul keeps only a hash. A lost token is replaced, not recovered.
- **It looks like `ohp_…`: `ohp_` and 64 hex characters.** A secret scanner can match
  that.
- **Every token expires.** Nothing extends one: make a new one. A token is also
  refused once it is older than `OUTHAUL_PUSH_TOKEN_MAX_DAYS`, whatever expiry
  it was given, so lowering that setting shortens the tokens that exist.
- **An expired token stays in the list** for 30 days, marked expired, so you can
  see why pushes stopped.
- **An account holds at most 25 tokens**, expired ones included, and a label is
  used once per account.
- **The table shows each token's last use**: the time, and the address the
  request came from. See [Behind a CDN](#behind-a-cdn) for that address.

## Pushing

```sh
git remote add outhaul https://outhaul.example.com/git/<app>.git
git push outhaul main
```

Git asks for a username and a password. Type any username (Outhaul ignores it),
and the token as the password.

A first push to a name that has no app creates the app, as over SSH. A fetch or
clone of a name with no app answers 404 and creates nothing. A clone over HTTPS
checks out the app's branch; over SSH, pass `-b <branch>` to `git clone` if the
host's git names another default branch.

### Keeping the token in a credential helper

Without a helper, git asks every time. Set one up once:

- **macOS keychain:** `git config --global credential.helper osxkeychain`
- **Git Credential Manager** (Windows, macOS and Linux): once installed, it
  asks for the password the first time and keeps the token in the system's own
  store.
- **`git config --global credential.helper store`** writes the token, as plain
  text, to `~/.git-credentials`. Anyone who can read that file can push as you
  until the token expires. Prefer a helper that uses the system's store.
- **`git config --global credential.helper 'cache --timeout=3600'`** keeps it in
  memory for an hour, and forgets it after that.

These are written from each tool's documented behaviour. The tests and the
checks made so far used git's `store` helper and `GIT_ASKPASS`; the macOS
keychain and Git Credential Manager were not tried.

### Why not put the token in the address

`https://user:token@…` works, and it is a bad idea. The address is kept in
`.git/config`, appears in your shell history and in the list of running
processes, and is what a proxy or an error message is most likely to print.
Outhaul's app page says not to, and a token typed as a password never has these
problems.

## In CI

Give the job the token as a secret variable, and let git ask a small script for
it, so the token is in no command line and no file in the repository:

```sh
#!/bin/sh
# ask-pass.sh: git runs this and reads its output as the answer
case "$1" in
  Username*) echo ci ;;
  *) echo "$OUTHAUL_PUSH_TOKEN" ;;
esac
```

```sh
chmod +x ask-pass.sh
GIT_ASKPASS=$PWD/ask-pass.sh git push https://outhaul.example.com/git/<app>.git HEAD:main
```

Tokens always expire, so a CI job will meet that. An expired token looks like
this in the job's output:

```
remote: outhaul: that push token expired on 14 November 2026. Create a new one at https://outhaul.example.com/account
fatal: Authentication failed for 'https://outhaul.example.com/git/<app>.git/'
```

Make a new token and replace the secret. A job that retries a refused push in a
loop will also use up its address's allowance; see
[Failed attempts](#failed-attempts).

## What each refusal means

Every refusal has a short plain-text body starting `outhaul: `, which git prints
as a `remote:` line. `<url>` is `OUTHAUL_PUBLIC_URL`.

| Status | The line | What to do |
|---|---|---|
| 403 | `outhaul: this remote uses http://. Change it to <url>/git/<app>.git` | Change the remote to `https://`. See [Plain HTTP](#plain-http) |
| 401 | `outhaul: sign in with any username and a push token as the password. Create one at <url>/account` | Git's first request never has credentials, so you see this only when git has none to send |
| 401 | `outhaul: that push token isn't valid. Create one at <url>/account` | The password is not a push token, or the token was revoked. An account password never works |
| 401 | `outhaul: that push token expired on <date>. Create a new one at <url>/account` | Make a new token |
| 429 | `outhaul: too many failed sign-ins from your address. Try again in <wait>.` | See [Failed attempts](#failed-attempts) |
| 403 | `outhaul: your last SSO sign-in to Outhaul was more than <age> ago. Sign in at <url>/login, then push again.` | See [SSO accounts](#sso-accounts) |
| 403 | `outhaul: you don't have permission to use '<app>'` | Your role can't change that app's project. The same rule as SSH |
| 404 | `outhaul: app '<app>' is not push-deployable` | The app takes its code from somewhere else |
| 404 | `outhaul: there is no app called '<app>'` | A fetch or clone of a name with no app |
| 404 | `outhaul: that isn't a valid app name` | |
| 415 | `outhaul: a git request must have Content-Type …` | Not a git client |
| 503 | `outhaul: git push is not available; see the server log` | The hook bridge didn't start; the log says why |

After a 401, git tells its credential helper to forget the token it sent, and
asks again. That is right for a token that is wrong, expired or revoked. After
a 403 or a 429, git keeps the token. So the SSO line and the permission line do
not make you type the token again.

## SSO accounts

An SSO account has no password, but it can have push tokens. Its tokens work
only while its last SSO sign-in to Outhaul is no older than
`OUTHAUL_PUSH_TOKEN_SSO_MAX_AGE` (`7d` unless you set it). Past that, a push is
refused with the SSO line above. Sign in at the address it names, and the same
stored token works again.

So someone removed at the identity provider loses push access within that time,
even while their Outhaul account exists. A sign-in to a protected route does not
count: it creates no account and touches none.

The Account page tells an SSO account when its tokens stop working. A password
account is bound by each token's expiry only.

## Revoking tokens

- Revoke one on the Account page. The next request with that token is refused;
  one already running finishes.
- A system admin sees every account's tokens on the Users page, with the owner,
  and can revoke any of them. No page shows a token or its hash.
- Deleting an account deletes its tokens.
- Removing SSO, or changing its issuer or client ID, ends every SSO sign-in, and
  deletes SSO accounts' tokens with them. Password accounts' tokens stay.
- A password change leaves tokens alone.
- Push tokens are left out of a state backup, and `restore-state` removes any a
  copy holds, as it does sessions. After a restore, make new ones. See
  [docs/STATE-BACKUP.md](STATE-BACKUP.md).
- With `OUTHAUL_GIT_HTTP=off` the panels and routes go away. The tokens stay in
  the database and age out; one that has not expired works again if the setting
  comes back.

## Failed attempts

Outhaul counts a request whose password isn't a live token, per client address.
It never counts by account name or by token, so nobody can lock someone else
out by knowing their name.

- Each address may fail ten times at once, and earns one more attempt each
  minute, up to ten. IPv6 addresses count by their /64.
- With none left, a request from that address with a bad token answers 429 and
  `Retry-After`. Other addresses and every account are untouched.
- **A live token is never held back.** Several setups make every client arrive
  as one address, and a stale token in a CI job is an ordinary event, so a
  used-up allowance never stops a good token. Only a bad one is told to wait.
- A request with no credentials, and one refused for the SSO rule, the role or
  the app, are not failed attempts.
- The count is in memory, and a restart forgets it. It is always on with git
  over HTTPS, and is separate from `OUTHAUL_SIGNIN_BACKOFF`, which stays per
  username for password sign-ins.

This slows a client that keeps sending bad tokens and keeps a flood out of the
log. It is not what protects a token: 256 random bits can't be guessed.

## The build in your terminal

The build streams into `git push` as `remote:` lines, as it does over SSH, each
as it is written. The first line is the address of the build log:

```
remote: → build log: https://outhaul.example.com/deployments/42
```

If the connection drops, the push has still happened and the deploy runs to
the end; that address is how to find it.

While the build prints nothing, git sends an empty packet every five seconds so
a connection that is closed when it is quiet stays open. Outhaul runs git with
that interval set, so the host's own git configuration can't turn it off.

A push follows its deploy for `OUTHAUL_HEALTH_TIMEOUT` plus five minutes, six
by default. Past that the push prints `✗ timed out after …` and ends, and the
deploy carries on. The limit is the same over SSH. A longer build has the
log's address from the first line; raising `OUTHAUL_HEALTH_TIMEOUT` raises the
limit.

A failed deploy still ends `git push` with status 0, as over SSH.

Each push over HTTPS is logged once, when it reaches the deploy: the account,
the token's label and id, the client address, the app, and the commits. The
token is never logged, nor its hash, the `Authorization` header or the username
typed. A fetch or clone isn't logged. A refused request is logged at most once
a minute per kind of refusal, with the client address.

## Plain HTTP

Git sends a token only after it has been asked for one, and Outhaul never asks
over a connection it knows to be plain HTTP.

- **Outhaul's own Traefik** redirects port 80 to HTTPS, git follows, and the
  question is asked over HTTPS.
- **Behind a trusted proxy** (`OUTHAUL_TRUSTED_PROXIES`) with Traefik serving
  TLS itself and the tunnel off, a request the proxy marks
  `X-Forwarded-Proto: http` is refused with the `https://` address named, and no
  question is asked.
- **Everywhere else** (the tunnel, a Traefik that serves plain HTTP behind a
  proxy, a proxy Traefik doesn't trust), Outhaul can't tell plain HTTP from
  HTTPS, so the edge must redirect HTTP to HTTPS for the admin host. If yours
  doesn't, a remote written `http://` would be asked for its token in the clear.

## Behind a CDN

See [docs/BEHIND-A-PROXY.md](BEHIND-A-PROXY.md) for the rest of the setup.

- **Redirect HTTP to HTTPS** for the admin host, as above.
- **Don't cache or buffer `/git/`.** A cached advertisement is stale, and a
  response that is buffered until it ends loses the build log and the
  keep-alive. Every answer carries the no-cache headers; a CDN may still need
  a rule.
- **A WAF that inspects request bodies** may need an exclusion for `/git/`. A
  pack is binary and can be large; it is not a form or JSON.
- **`OUTHAUL_UPLOAD_TIMEOUT`** must cover your slowest push's upload. The CDN's
  own time and size limits apply as well.
- **A CDN that refuses a chunked upload** may need
  `git config http.postBuffer 524288000` (a size in bytes, larger than your
  pushes), so git sends a length instead of chunks.
- **`OUTHAUL_TRUSTED_PROXIES`** is how a token's last use shows the pusher's own
  address and not the CDN's. Outhaul believes `X-Forwarded-For` only from its
  own Traefik, and Traefik believes it only from the addresses listed. Without
  it, everyone behind the CDN counts as one address, for last use and for
  failed attempts.

## What is the same as SSH, and what isn't

The same: the role check (a system admin may use every project; anyone else
needs project member or above on the app's project), made at each request and
before anything is created; the same hook and deploy; the same first push, with
the same default project and domain; and the repository's hook is rewritten on
every push request, as an SSH push does.

Different:

- A fetch or clone of a name with no app answers 404 and creates nothing. Over
  SSH it creates an empty repository.
- A token expires; a push key doesn't.
- An SSO account's token has the sign-in rule; a push key has none.
- The first line of a push is the build log's address. An SSH push doesn't print
  it.
- Fetches aren't logged.
- Git's dumb HTTP protocol and git LFS are not served.

## What only the real host can show

Not checked yet. For the staging host and its CDN.

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
9. For an SSO account whose last sign-in is past the limit: the push prints the
   message, and works after signing in, with the same stored token.

Also not checked: git older than 2.53, other git clients, a gzip-compressed
request body, and a pack still arriving when git starts to answer. What was
checked, with git 2.53.0 and `traefik:v3.7.6`: a push, clone and fetch; the
hook's lines arriving as written; git's five-second keep-alive; what git does
with a 401, a 403 and a 429; and that Traefik's default cuts a long upload at 60
seconds.
