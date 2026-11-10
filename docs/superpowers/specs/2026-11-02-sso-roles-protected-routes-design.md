# Accounts, roles, SSO and protected routes

## Why

Outhaul has one admin account, and every app has to build its own login.

This project does three things:

1. **Accounts and roles.** Several people can use one server, with roles that
   stop accidental changes through the UI.
2. **SSO login.** People can sign in through an OIDC provider, such as Okta, as
   well as with a password.
3. **Protected routes.** A domain row can require sign-in, using the same
   sign-in, so someone building an app doesn't need to write their own auth.

The UI always says "SSO", never a provider's name. The button reads "Sign in
with SSO".

## Goals

- SSO sign-in as an option next to passwords, for any OIDC provider.
- Five states for an account: system admin, project admin, project member,
  project viewer, and no role. Roles are managed only in Outhaul.
- Password accounts are first class. Roles and protected routes work on a
  server with no SSO connected.
- A per-domain-row switch that requires sign-in, with the signed-in person's
  identity passed to the app.

## Non-goals

- **Containing a user.** Roles are guardrails for trusted people. Anyone who can
  deploy can run arbitrary code on the server, so a role can't contain someone
  who means to get round it. Restricting Compose files or separating apps on
  the Docker network is out of scope.
- Taking roles or groups from the SSO provider.
- A different role per project for one person.
- Limiting a route to particular people or teams. See "Who gets through".
- API tokens or service accounts for protected routes.
- SAML, or more than one SSO provider on a server.
- Sign-out at the SSO provider.
- Email delivery of invitations, and audit logging.
- Turning passwords off.

## Anti-goals

The project has failed, even if it works, if any of these happen:

- A route looks protected but is public.
- A deployed app receives an admin session, or a cookie that works anywhere
  other than its own host.
- An SSO outage locks everybody out of Outhaul.
- An app can be fooled by a forged identity header.
- An open route starts to depend on Outhaul being up.
- An existing single-admin install notices anything on upgrade, apart from new
  options.

## Constraints

From `ARCHITECTURE.md` and `AGENTS.md`:

- One static Go binary with CGO off, `net/http` with stdlib routing, no JS build
  step, and SQLite.
- Traefik routes come from Docker labels. The file provider already carries the
  admin route.
- Migrations are numbered and never edited after shipping. The next is 0027.
- Sealed values must be added to `sealedQueries` in `internal/store/sealed.go`.
- Test first. Fixes that depend on real Traefik or OIDC behaviour must also be
  checked against the real thing.
- "Single admin user" is a locked decision in `ARCHITECTURE.md`. The maintainer
  has now replaced it, so update that entry and add the new decisions to
  `AGENTS.md`. The repository is public, so don't list unfixed security issues
  in either file.

Use a maintained library for OIDC and token verification, not hand-written JWT
checks. `github.com/coreos/go-oidc/v3` with `golang.org/x/oauth2` is pure Go and
fits the constraints above.

## Accounts and roles

### What each role can do

Each person has one role, across every project shared with them. "Shared" means
projects shared with that person.

| | System admin | Project admin | Project member | Project viewer | No role |
|---|---|---|---|---|---|
| Users, server settings, updates, host-wide pages | Yes | No | No | No | No |
| Create projects | Yes | Yes | No | No | No |
| Share or delete a project | Any | Shared | No | No | No |
| Change apps, databases, domains and env vars; deploy | Any | Shared | Shared | No | No |
| See status and logs | Any | Shared | Shared | Shared | No |
| Pass protected routes | Yes | Yes | Yes | Yes | Yes |

Whoever creates a project has it shared with them automatically. System admins
see every project and aren't listed on any of them.

Viewers never see env values (project or app, secret or not), database passwords
or connection strings, or webhook URLs and secrets. They can read build and
runtime logs.

### Applying roles to the routes

`internal/server/server.go` registers about 75 routes behind one `requireAuth`.
Replace it so every route states what it needs. Add a test that walks every
registered route and fails if one has no stated level, and a table test of each
level against each role.

Check permissions on the server. Also hide controls a person can't use, and hide
sidebar entries for pages they can't open.

| Level | Routes |
|---|---|
| No sign-in | `/healthz`, `/ca.pem`, `/static/`, `/setup`, `/login`, `/logout`, `/github/callback`, `/github/setup`, `/webhooks/*`, the new setup-link, SSO and gate endpoints |
| Any account | `/account` and its forms |
| Any project role, filtered to shared projects | `/`, `/projects`, `/apps`, `/databases`, `/domains` |
| System admin | `/containers`, `/metrics`, `/metrics/sample`, `/volumes`, `/volumes/reclaim`, `/settings/*`, `/github/connect`, `/users/*`, placeholder pages |
| Viewer or above on the project | `GET` on `/projects/{id}`, `/apps/{id}` and its logs and stats, `/deployments/{id}` and its logs, `/databases/{id}` and its logs |
| Member or above on the project | Every other project-scoped change: app and database forms, env, domains, volumes, attachments, previews, deploy, stop, restart, cancel, rollback, delete app or database, and all of `/backups/*`. Also `/templates`, `POST /apps` and `POST /templates/{id}`, checked against the target project |
| Project admin or above | `POST /projects`, and deleting or sharing a project they're on |

Rules that apply across these:

- A project comes from the object: an app or database directly, a deployment
  through its app, a backup through `target_kind` and `target_id`.
- Attaching a database already requires both to be in one project, enforced in the
  store. Apps can't move between projects today, and this project doesn't add that.
- A cold `git push` creates its app in the default project, so it counts as a push
  to that project.
- Non-admins have no implied default project. An empty `project_id` falls back
  to the default project only if they can change it, so the create-app form
  lists only their projects.
- Members can pick from git sources and backup destinations but can't see their
  credentials or change them.
- An object outside someone's projects returns 404. A role too low for an action
  on a project they can see returns 403.
- A role is read on every request, so a change applies at once. A log stream that
  is already open may run on until it closes.
- A person with no role who signs in to Outhaul sees only a "no access yet" page,
  and `/account` if they use a password.
- The Overview page hides host-wide figures from everyone but system admins.

### Users page

This is a system-admin page in the System sidebar group. It lists each person,
how they sign in, their role (changed in place), project count and last sign-in.
A banner counts people waiting for a role.

- **Password accounts:** "New password account" takes a username and a role, then
  shows a one-time setup link to pass on. "Reset password" issues the same kind of
  link, stops the old password working and ends that account's sessions. It can't be
  used on your own account or on the last password system admin. Links last 7 days,
  work once, and are stored hashed.
- **SSO accounts:** these appear the first time someone signs in to Outhaul itself,
  with no role. Their role can't be set before that.
- **Removing someone** takes effect at once and ends all of their sessions,
  including protected-route sign-ins. An SSO user would come back with no role if
  they signed in again, so the page says they must also be removed at the provider.
- The last **password** system admin can't be removed or demoted, and the page says why. Otherwise an SSO outage
  or removing the SSO settings could leave nobody able to administer the server, and Outhaul has no recovery
  command.

### Sharing a project

The project page gets a People panel listing who's on the project and their role,
which is read-only there. System admins, and project admins on that project, can
add and remove people. The list of people to add holds only accounts with a
project role.

### Account page

Settings becomes system-admin only, so its "Change password" and "Push keys" panels
move to a new `/account` page available to everyone.

### Push keys

Today any registered key can push to any push-source app. A key now belongs to a
person. A push is allowed only if that person's role can change the app's project;
otherwise it is refused with a clear message. Removing an account removes its keys.
An SSO user removed only at the provider keeps their keys until they're removed in
Outhaul too, so the Users page wording must make that clear.

## SSO login

### Connecting a provider

A system admin fills in a "Single sign-on (SSO)" panel on Settings: issuer URL,
client ID and client secret. The panel shows the redirect URI to register. It runs
OIDC discovery on save and refuses settings that fail.

The client secret is sealed in the `settings` table and added to `sealedQueries`,
the same way as the Cloudflare token. It is never shown again. Removing the settings
removes the button. Passwords keep working.

Removing the settings, or changing the issuer or client ID, ends every SSO sign-in
session: Outhaul, gateway and route. Password sessions are unaffected, and so is replacing
only the client secret. Accounts are keyed by issuer, so changing the issuer needs a
confirmation that says everyone will come back as a new account with no role, while the old
accounts keep their roles and push keys until they're removed.

The panel must warn: everyone the provider lets sign in can reach every protected
route, so limit who is assigned to this application at the provider. A provider such
as public Google accounts, where anyone can sign in, is not safe to use.

### Signing in

The login page shows "Sign in with SSO" above the password form when a provider is
connected, and is unchanged when none is.

- Use the authorization code flow with PKCE, `state` and `nonce`, and scopes
  `openid email profile`.
- An SSO account is identified by issuer and subject. Email is shown for display,
  refreshed at each sign-in, and never grants access by itself. Two accounts may show
  the same email.
- Use the `email` claim, falling back to `preferred_username`. Refuse sign-in if there
  is neither. An `email` that the provider marks `email_verified: false` is not used,
  because protected apps receive this name as the person's identity.
- Never link accounts automatically by email. The first-boot admin stays a password
  account. To become an SSO system admin, a person signs in with SSO once, then uses
  the password account to change that role.
- Password sign-in must refuse an SSO account, and an SSO sign-in can never reach a
  password account.
- If the provider can't be reached, show an error on the login page, with the password
  form still available.
- Sign-in sessions on the Outhaul host last 8 hours for SSO and keep today's 7 days for
  passwords. A person removed at the provider then loses Outhaul within 8 hours.

## Protected routes

### Who gets through

Anyone who can sign in: anyone the SSO provider lets through, and any password account,
with or without a role. Visiting a protected route with SSO doesn't create an account,
so the Users page doesn't fill with every colleague.

There is one group per server, so a route can't be limited to a single team.

### The switch

Each domain row gets a "Require sign-in" switch, off by default, on the last step of the
domain dialog, which becomes "HTTPS and sign-in". Paths that must stay open get their own
row with the switch off. `UNIQUE(app_id, host, path)` already allows this, and Traefik
prefers the longer rule, so `wiki.example.com/webhooks` wins over `wiki.example.com`.
The reverse is supported too: `shop.example.com/admin` protected under an open
`shop.example.com`.

The switch needs both of these:

- **A reachable Outhaul:** the condition in `ProxyConfig.adminRoutingEnabled`, meaning a
  public URL and HTTPS or a tunnel. Browsers are sent there to sign in. It also gives
  Traefik the host gateway.
- **HTTPS for visitors:** the row's TLS flag with TLS on for the server, or tunnel mode,
  where the edge terminates TLS.

If either is missing, the switch is disabled and says why. A row that is already protected
never silently becomes open if these later stop being true. It keeps its middleware and
sign-in fails, and the UI warns.

### Traefik

- Each protected row defines its own forward-auth middleware **in its labels**, the way
  `rewriteMiddlewares` defines strip and add-prefix. Its address is the gate-check endpoint on
  `host.docker.internal:<AdminPort>`, and `authResponseHeaders` lists `X-Outhaul-User`. It comes
  first in the middleware list, before any strip or add-prefix, on both the HTTP and HTTPS
  routers. Change `RouteLabels`, which `AppLabels` and the Compose override both use.
- **Don't define the gate in the file provider and reference it as `@file`.** Tested on
  `traefik:v3.7.6` with Docker labels: a router that references an undefined middleware is dropped,
  so `x.test/admin` falls through to the open `x.test` router and reaches the app, publicly.
  `writeAdminDynamicConfig` deletes that file whenever the admin route is off, so this would
  happen in practice. With the middleware defined in labels and its address unreachable, the
  same request returned a 500.
- If `AdminPort` changes, protected rows fail closed until their next deploy. That is accepted.
- `X-Outhaul-User` must not be forgeable on any open route, including an app that has since turned
  its last protected row off and still trusts the header. Every open row of every app removes that
  header from incoming requests, from that app's next deploy, using a `headers` middleware defined
  in its labels. It can't use `@file`, because that would make open routes depend on the file.
  Forward auth already replaces the header on protected rows, but check both against real Traefik.

### The sign-in flow

A cookie for the Outhaul host doesn't reach an app's host, and apps must never see it. So each
protected host gets its own cookie, through a one-time code:

1. Traefik calls the gate-check endpoint with `X-Forwarded-Host`, `X-Forwarded-Uri`,
   `X-Forwarded-Method` and the browser's cookies.
2. A valid route cookie for that host gets a 200 with `X-Outhaul-User`.
3. Otherwise, a page navigation is redirected to a gate-start page on Outhaul's public URL with
   the original URL. Anything else (a non-`GET`/`HEAD` method, no `text/html` in `Accept`, or a
   WebSocket upgrade) gets a 401.
4. Gate-start refuses any URL unless the row Traefik would route it to has protection on: of the
   rows on that host whose path is a prefix of the URL's path, the longest. Checking only the host
   isn't enough. An open path under a protected host would receive the one-time code unredeemed,
   and whoever held it could sign in as that person. It always builds `https://` URLs, because
   `X-Forwarded-Proto` isn't reliable in tunnel mode.
5. A valid gateway session (below) skips straight to step 7. Otherwise the person sees the login
   page, naming the destination, with the same SSO button and password form.
6. Signing in creates a gateway session in its own cookie on the Outhaul host.
7. Outhaul mints a single-use code, valid for 60 seconds and tied to that host, and redirects to
   the original URL with the code in a reserved query parameter. The original path always falls
   under the protected router, so no extra Traefik route is needed.
8. Gate-check sees the parameter, redeems the code, sets the route cookie and redirects to the URL
   without the parameter, as an absolute `https://` URL on that host. A relative redirect built from
   a path such as `//evil.example/` would send the browser to another site. Traefik returns a non-2xx forward-auth response to the browser as it is.

Properties to keep:

- The route cookie is host-only, `Secure`, `HttpOnly`, `SameSite=Lax`, and named with the `__Host-`
  prefix. It lasts 8 hours and works only on the host it was issued for. It is checked against the
  host on every request. The app can read it, so it must be useless anywhere else.
- Signing in to Outhaul through `/login` also creates a gateway session, so there is no second
  prompt. Route visits made with SSO create a gateway session but no account.
- Route sessions belong to their gateway session. Ending the gateway session ends them all. So
  signing out of Outhaul, removing an account, or changing a password ends route access at once.
- Session tokens and codes are stored hashed. Expired rows get cleaned up.
- Gate-check runs on every request to a protected route, so it must be cheap. Caching is up to
  the builder, as long as sign-out and removal still apply within a few seconds.
- If Outhaul is down, protected routes fail and open routes are untouched. That is accepted: it
  fails closed.

### What the app receives

`X-Outhaul-User` holds the SSO email, or the username for a password account. An app can also link
to the sign-out page below. That is the whole contract. Document it in a new `docs/` page, along with
the open-path pattern and provider setup.

### Signing out

People who only use protected routes never see Outhaul's sidebar, so `<public URL>/gate/sign-out` is
a small page on the Outhaul host with a sign-out button. It ends the gateway session and so every route
session from it. Apps can link to it. An SSO user may be signed straight back in while their session at
the provider lasts, and the page should say so.

### Applying a change

Today, domain changes reach Traefik only on the next deploy. Protection can't wait for someone to
remember, so on a running app, any save that does one of these queues a deploy straight away: adds a
protected row, turns a row's switch on or off, or edits the host or path of a protected row.

Restart would bring back a container with its old labels, so a public route could return with the switch
on. If an app's container labels don't match its rows' protection, Restart runs a deploy by the rules below
instead of restarting the container.

The deploy works like this:

- If a deploy is already queued or running, follow it: a rollback in flight queues the same image, and a
  normal deploy in flight queues another normal deploy. Reusing the last image here would run after that
  deploy and put the old code back.
- A single-container app with a usable last image redeploys that image, through the same path as
  rollback (`CreateRollback`), without rebuilding.
- A Compose app has no per-deployment image, and an app's image may have been pruned. Outhaul also keeps
  no checkout and no record of the deployed commit. These run a normal deploy of the latest code (the branch
  head, or the newest matching tag in tag mode), even with auto-deploy off. The dialog warns first and names
  what will ship ("This will deploy the latest code from `main`"), so nothing ships unseen. If the build
  fails, the row stays "Not protected yet" and links to the failed deployment.
- An app that is stopped or has never been deployed queues nothing.

### What the row shows

The Domains table gets an Access column. Its state comes from the **labels on the running container**,
not from a stored flag, so it can't disagree with what Traefik is doing. A new row counts as lacking the
gate, because an open row above it may already be serving that path:

| Setting | Running container | Shows |
|---|---|---|
| On | Has the gate | Sign-in required |
| On | Lacks the gate | Not protected yet, with the deploy in progress or the failed deployment |
| Off | Has the gate | Sign-in required until the redeploy finishes |
| Off | Lacks the gate | Open |
| Either | Not running | The setting, marked as applying on the next deploy |

The `/domains` page shows the same state.

### Previews

`mintDomains` (`internal/previewmgr/manager.go`) copies protection from the parent's rows, fork previews
included. Several parent rows can collapse into one preview row, and protected wins. Adding a protected
row to a parent, or turning a parent row's switch on, also protects existing previews and redeploys them,
by the rules above. Turning it off affects new previews only.

## Upgrade

- The existing account becomes system admin, with the same username, password and sessions.
- Existing projects are visible only to system admins until shared. Existing push keys go to that account.
- `domains` gains a protection column defaulting to off, and no app redeploys on upgrade. The header-stripping
  labels arrive with each app's next deploy.
- The Traefik container and its file-provider config don't change.

## Data, as a starting point

The builder can reshape this, as long as the behaviour above holds.

- `users`: role, sign-in kind, SSO issuer and subject (unique together), email, last sign-in.
  `password_hash` is `NOT NULL`, so SSO accounts need a value that can never verify.
- `project_members`: project and user, deleted with either.
- Setup links: hashed token, user, expiry.
- Gateway sessions and route sessions, as described above, plus one-time codes.
- `domains.protected`, and an owner on `push_keys`.
- SSO settings in `settings`, with the secret sealed.

## Build order

Each part can ship by itself:

1. Accounts, roles, the Users page, sharing, the Account page, push keys.
2. SSO login.
3. Protected routes.

## Testing

- Unit tests use the existing fakes, plus `httptest` for a fake OIDC provider.
- Run the whole flow against real Traefik and a real OIDC provider (Dex or Keycloak in Docker will do)
  before calling part 3 done. In particular, check:
  - a 302 with `Set-Cookie` from forward auth reaches the browser;
  - the identity header is replaced and stripped as designed;
  - the rule priority for an open path under a protected host;
  - behaviour when Outhaul is stopped;
  - a tunnel-mode request, if a tunnel is available.
- Test the upgrade with `openMigratedBefore`.

## Decisions

**Made by the maintainer in discussion:** the role table, one role per person, roles managed only in
Outhaul, trusted-users scope, passwords working everywhere SSO does, anyone able to sign in passing
protected routes, the switch per domain row, open paths as separate rows, "SSO" in the UI, the 8-hour
sign-ins, the identity header, automatic redeploy when the switch changes, a warning before that redeploy
ships the latest code on apps with no reusable image, previews inheriting protection, no API tokens, and the
HTTPS and public address requirements.

**Calls made while writing this up, not yet confirmed:**

- Settings becomes system-admin only, so password changes and push keys move to `/account`.
- Push keys belong to a person, and a push needs member or above on the project.
- One sign-in covers all protected hosts, and signing out of Outhaul ends route access everywhere.
- Turning a parent's switch on protects existing previews. Turning it off affects new previews only.
- Protected paths under an open host are supported, so the gate is defined in labels, not the file provider.
- Every open row of every app strips `X-Outhaul-User`, from its next deploy.
- Adding a protected row or editing a protected row's host or path also queues a deploy, and Restart redeploys when
  labels don't match.
- The last password system admin can't be removed or demoted.
- `/gate/sign-out` lets people who only use protected routes sign out, and apps can link to it.
- "Reset password" stops the old password working at once, and can't be used on yourself or the last
  password system admin.
- A protection redeploy that lands during another deploy follows that deploy, rather than reusing the last image.
- Removing SSO settings, or changing the issuer or client ID, ends all SSO sessions.
- Members can create apps and databases, rename projects, edit shared env vars, and run restores in their
  projects. Only project admins and above can delete or share a project.
- Existing projects start out visible to system admins only.
- Out-of-scope objects return 404, and setup links last 7 days.
- One OIDC provider per server.
