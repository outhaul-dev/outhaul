# Requiring sign-in for an app (protected routes)

Any domain of an app can require sign-in. With its "Require sign-in" switch on,
a visitor has to sign in to Outhaul before a request reaches the app, and the app
is told who signed in. An internal wiki or a shop's admin pages don't need a login
of their own.

People sign in the same way they sign in to Outhaul itself: with a password
account, or with SSO when a provider is connected (see [SSO.md](SSO.md)).

This has been checked against a real Traefik with a stand-in for Outhaul, but not
yet end to end with a real Outhaul and a real SSO provider. See
[Testing so far](#testing-so-far).

## Who gets through

Everyone who can sign in to Outhaul gets through every protected route on the
server:

- every password account, whatever its role, including one with no role;
- everyone your SSO provider lets sign in. Signing in to a protected route with SSO
  doesn't create an Outhaul account, so these people appear on the Users page only if
  they also sign in to Outhaul itself.

There is one group per server. A route can't be limited to a team or to particular
people. So:

- limit who is assigned to Outhaul's application at the provider (see
  [Who can sign in](SSO.md#who-can-sign-in)). A provider where anyone can sign in,
  such as public Google accounts, is not safe to use;
- remember that Outhaul's system admins decide who gets a password account;
- if an app needs more than "someone signed in", it keeps its own list of who may do
  what. See [What your app receives](#what-your-app-receives).

## Before you start

The switch needs both of these:

- **A public address for Outhaul.** `OUTHAUL_PUBLIC_URL` must be set, and visitors
  must be able to reach it, because they are sent there to sign in.
- **HTTPS for visitors.** Either HTTPS on the server (`OUTHAUL_ACME_EMAIL` for
  Let's Encrypt, or `OUTHAUL_LOCAL_CA`, see [LOCAL-CA.md](LOCAL-CA.md)), or a
  Cloudflare tunnel, whose edge serves HTTPS. On a server with HTTPS, a domain that
  requires sign-in is always served over HTTPS, even with its "Automate HTTPS" box
  off.

If either is missing, the switch is greyed out and the dialog says why.

For visitors to get through, two more things must hold:

- Their browser accepts cookies for the app's address. A browser that refuses them
  is stopped after a few tries, on a page that says so.
- Traefik can reach Outhaul's admin port (8080 unless `OUTHAUL_LISTEN_ADDR` says
  otherwise) on the host, as it does to publish the admin UI. Traefik asks Outhaul
  about every request to a protected route there. The installer's firewall rules
  allow this.

## Requiring sign-in on a domain

You need to be a project member or above on the app's project.

Apps that share an address that requires sign-in share its sign-in, so they should
belong to people who trust each other. Only a system admin can put a domain on an
address where an app in another project requires sign-in, or require sign-in where an
app in another project has a domain; anyone else is told "Another project already uses
this address, so a system admin has to set this up."

1. Open the app, and its Networking tab.
2. Press "+ New domain", or "edit" on an existing domain.
3. On the last step, "HTTPS and sign-in", turn on "Require sign-in".
4. Save.

### The save redeploys the app

Traefik learns a domain's setting only from the labels a deploy puts on the app's
containers. So when the app is running, the save redeploys it straight away:

- A Nixpacks or Dockerfile app whose running image is still on the server redeploys
  that image, without building anything, the way a rollback does.
- Otherwise it deploys the latest code: the head of the app's branch, or the newest
  matching tag in tag mode. This happens for Compose apps, and when the image the app
  is running can't be reused, for example because it has been cleaned up. The dialog
  says so before you save, for example "This will
  deploy the latest code from main." A template app is deployed again from its own
  Compose file.
- If a deploy is already waiting in the queue, that deploy applies the change. If one
  is already building, another deploy follows it.
- A stopped app, or one that has never been deployed, isn't deployed. Its next
  deploy applies the change.

If the deploy that turns sign-in on fails, the old container goes on serving the
address without sign-in, and the Access column says "Not protected yet" with a link
to the failed deployment. Until the column says "Sign-in required", treat the address
as open.

These saves redeploy a running app:

- turning the switch on or off;
- adding a domain with the switch on;
- changing the host or path of a domain that requires sign-in;
- removing a domain that doesn't require sign-in, or changing its host or path, when
  another domain on the same host, in any app, does. Otherwise the old container
  would go on serving that path with no sign-in. The Domains panel marks these
  domains.

Other domain changes apply on the app's next deploy, as before.

Removing a domain that requires sign-in doesn't redeploy the app. Until its next
deploy, the old container still asks for sign-in at that address, but Outhaul only
signs people in to addresses that require sign-in now. Deploy the app to finish
removing the domain.

### Other apps it can redeploy

Requiring sign-in on a host can redeploy other apps too, and the dialog lists them
before you save:

- another app that still serves an old route on that host without sign-in, because
  its domain there was removed or moved since its last deploy. An app in a project you
  don't share is named only as "an app in a project you don't share", which "may
  deploy its latest code";
- this app's pull-request previews. See [Previews](#previews).

The dialog can list other apps in advance only for addresses the app already has, and
only once something on the server requires sign-in. Otherwise, for example for an
address new to the app, the first save may be refused with the list of other apps it
would redeploy. Nothing is changed; check the list and save again to confirm.

### Restart

If an app's containers, running or stopped, don't match its domains' sign-in
settings, the app page says "Restart will deploy this app instead", and Restart
deploys the app by the rules above. Restarting the old container would bring back its
old settings.

### Turning it off

Turning the switch off redeploys the app the same way. Until that deploy finishes,
the address still asks for sign-in.

The switch can always be turned off, even when signing in isn't available, so an
address that nobody can get through can be opened again. If a requirement stops
being met later, for example the public URL is removed, a domain that requires
sign-in goes on requiring it, nobody can get through, and the app page warns about
it.

## The Access column

The Domains panel on the app page, and the Domains page, show each domain's Access.
It is read from the labels on the running containers, not from the switch, so it shows
what Traefik is doing now. A container Docker is restarting counts as running. A
container's route counts for a domain only when it is for that domain's host and path:
after a domain's host or path is changed, the old container doesn't serve the new
address.

| Access | What it means |
|---|---|
| Sign-in required | Visitors must sign in. |
| Open | Anyone can visit. |
| Not protected yet, redeploying | The switch is on, and the deploy that applies it is queued or running. Until it finishes, the running container doesn't ask for sign-in. |
| Not protected yet, the last deploy failed | The switch is on, but the deploy failed, and the running container doesn't ask for sign-in. Fix the deploy and deploy again. |
| Not protected yet | The switch is on, but the running container doesn't ask for sign-in at this address. Deploy the app. |
| Not protected yet, an old route of this app still answers part of this address | This app's running container asks for sign-in here, but still serves an old open route under this address, for example a domain removed since its last deploy. Deploy the app. |
| Not protected yet, *app* still answers part of this address | Another app's running container serves part of this address without sign-in: an old route it still has. Deploy that app. When you don't share its project, it is "another app", and so is a container whose app was removed. |
| Sign-in required, until the redeploy finishes | The switch is off, and the running container asks for sign-in until the deploy that applies it finishes. |
| Sign-in required, or Open, applies on the next deploy | The app isn't running, or (Open) it is running but doesn't serve this domain yet, for example one added or moved since its last deploy. This is the setting its next deploy applies. |
| Unknown, Docker can't be reached | Outhaul couldn't read the containers. |

A new domain that requires sign-in shows "Not protected yet" until a deploy gives it
its own route, because another domain on the same host may be serving its path in the
meantime.

## What your app receives

Every request that passes sign-in carries the header `X-Outhaul-User`, holding the
name of the person who signed in:

- with SSO, the `email` the provider sent, or its `preferred_username` when there is
  no email or the provider marks it unverified (`email_verified: false`);
- with a password account, its username.

Reading it is one line, for example `request.headers.get("X-Outhaul-User")` in a
Flask app, or `req.get("X-Outhaul-User")` in an Express app.

**Trust it only for requests that came through Outhaul's proxy.** In an app deployed
by this version of Outhaul, Traefik replaces the header on every protected route and
removes it on every open one, so a visitor can't send their own. Anything that reaches
the container another way, such as another container on Outhaul's Docker network or a
port you publish for the app yourself, can send any header it likes.

**It is a name, not an ID.** It is whatever the provider sent, or whatever username a
system admin gave a password account:

- it may be an email address the provider hasn't verified: Outhaul uses the
  `preferred_username` instead only when the provider marks the email unverified, and
  not every provider says;
- it may be a `preferred_username`, which may not be an email address at all;
- a password account's username can look like an email address;
- two people can have the same name.

So all the header proves is that someone who can sign in to Outhaul made the request.
Don't use it as a unique key, or as proof that someone owns an email address. An app
that needs more, such as a shop that lets only some people into `/admin`, keeps its own
list of who may do what, and Outhaul's admins decide who gets a password account. With
SSO, don't let people change their own email or username at the provider (see
[The name Outhaul shows](SSO.md#the-name-outhaul-shows)).

## Leaving a path open

A wiki at `wiki.example.com` requires sign-in, but your Git host posts webhooks to
`wiki.example.com/webhooks`, and a webhook can't sign in. Give that path its own
domain, with the switch off:

| Host | Path | Require sign-in |
|---|---|---|
| `wiki.example.com` | | On |
| `wiki.example.com` | `/webhooks` | Off |

Traefik sends each request to the domain with the longest path that matches it, so
`wiki.example.com/webhooks/github` is open and every other page asks for sign-in.
Anyone can reach the open path, so the wiki has to check those requests itself, for
example with the webhook's secret.

On a server with HTTPS, turn on "Automate HTTPS" for the open path's domain too. A
domain that requires sign-in is always served over HTTPS, but an open one only when
its own box is on; without it, requests for the open path reach the protected domain
and ask for sign-in.

This is the safe way to mix the two: protect the whole address, then open the paths
that need it by name.

## Protecting one path

The reverse works too. A shop at `shop.example.com` is open to everyone, and only its
admin pages need sign-in:

| Host | Path | Require sign-in |
|---|---|---|
| `shop.example.com` | | Off |
| `shop.example.com` | `/admin` | On |

Paths match as plain prefixes, not whole path segments: `/admin` also covers
`/administrator`, and `/webhooks` also covers `/webhooks-old`. Choose paths with that
in mind.

Paths are matched exactly as written, including case: `/admin` doesn't cover `/ADMIN`
or `/Admin`. So protecting one path under an open address is only safe when the app
itself treats paths exactly as well. An app that also answers `/ADMIN` with its admin
pages serves them from the open domain, without sign-in. If you can't be sure of that,
protect the whole address and open the other paths instead, as in
[Leaving a path open](#leaving-a-path-open).

Outhaul's routes don't accept a path that contains a `..` segment, however it is
encoded.

## Scripts, API calls and WebSockets

Only a page load in a browser is sent to sign in: a `GET` or `HEAD` request that
accepts `text/html`, as a browser's does when it opens a page. Without a sign-in,
anything else gets `401 Unauthorized`:

- form posts and other methods;
- `fetch` and XMLHttpRequest calls, such as a single-page app's API calls once the
  sign-in has ended;
- WebSocket handshakes, which browsers send without `text/html`;
- CORS preflight requests (`OPTIONS`). Browsers send these without cookies, so a call
  from another origin that needs one fails even when the person is signed in.

Requests from the app's own pages carry the sign-in cookie, so they work while the
sign-in lasts. When a call gets a 401, the sign-in has ended: reload the page, and the
browser is sent to sign in.

There are no API tokens or service accounts, so a script or another server can't get
through a protected route. Put the endpoints that machines call on an open path, as in
the wiki example, and check those requests in the app.

## Sessions and signing out

- A sign-in lasts 8 hours. After that, the next page load asks again.
- One sign-in covers every protected route in that browser. Opening a second protected
  app passes through Outhaul and straight back, without asking again.
- Signing in to Outhaul's own pages at its public address also signs you in to
  protected routes, for 8 hours.
- Sign-ins are kept when Outhaul restarts.

To sign out, open `<public URL>/gate/sign-out`, for example
`https://outhaul.example.com/gate/sign-out`, and press "Sign out". That ends access to
every protected route in that browser. An app can link to it:

```html
<a href="https://outhaul.example.com/gate/sign-out">Sign out</a>
```

This page doesn't sign you out of Outhaul's own pages. "Sign out" in Outhaul's sidebar
does both. Outhaul doesn't sign anyone out at the SSO provider, so while their session
there lasts, the next protected route may sign them straight back in. The page says so.

Access to protected routes also ends at once when:

- the person's account is removed on the Users page;
- their password is changed or reset;
- the SSO settings are removed, or the issuer or client ID is changed (SSO sign-ins
  only).

People who sign in with SSO can pass protected routes whether or not they have an
Outhaul account. Removing someone's account on the Users page ends their sign-ins now,
but they can sign in again while the provider lets them, so remove them at the
provider too. Once they're removed there, a sign-in they already have lasts up to 8
hours.

## Cookies

After signing in, the browser holds a cookie named `__Host-outhaul_route` on the app's
address. The browser sends it to the app with every request, so the app can read it,
but it works only on that address: Outhaul checks it against the host on every
request, and the browser never sends it anywhere else. While it lasts it lets its
holder into that address, so don't log it or pass it on.

During sign-in, the app's address also gets a short-lived `__Host-outhaul_pending`
cookie, and Outhaul sends the browser back with a one-time code in the
`__outhaul_code` query parameter. On a protected address, a request with that
parameter never reaches the app, so don't use a parameter of that name.

Outhaul's own sign-in cookie stays on Outhaul's address and never reaches an app.

## A CDN in front

Every response on an address that requires sign-in carries
`CDN-Cache-Control: no-store` and `Cloudflare-CDN-Cache-Control: no-store`, which tell
a CDN in front of Outhaul, such as a Cloudflare tunnel or proxied DNS, not to keep it.
Browsers ignore these headers, so the app's own `Cache-Control` still decides what a
browser keeps. A CDN rule that overrides the origin's caching headers, such as "Cache
Everything", would serve a stored page to anyone without sign-in, so don't apply one
to a protected address.

## When Outhaul is down or restarting

Traefik asks Outhaul about every request to a protected route. While Outhaul is stopped
or restarting, including during an update from Settings, those requests get an error
(`500`) instead of the app. Open routes, including open paths under a protected host,
don't ask Outhaul, and keep working.

If Outhaul can't read its database, a protected route answers "Signing in isn't
available right now." (`503`) and lets nobody through.

The address Traefik uses to reach Outhaul is in each app's labels. If you change the
port Outhaul listens on (`OUTHAUL_LISTEN_ADDR`), protected routes answer an error until
each app that has one is deployed again.

## Previews

Pull-request previews copy their protection from the parent app's domains, fork
previews included. When several of the parent's domains become one domain on the
preview, it requires sign-in if any of them does.

Turning sign-in on for one of the parent's domains, or adding one with it on, also
turns it on for the app's existing previews and redeploys the ones that are running.
The dialog lists them. Turning it off leaves existing previews protected; the dialog
says "Existing previews stay protected. New ones will be open." New previews follow
the parent's domains as they are when the preview is made.

The pull-request comment marks a protected link "(requires sign-in)".

## If it doesn't work

- **The switch is greyed out.** The dialog says why: "Set OUTHAUL_PUBLIC_URL so people
  can be sent to sign in." or "Protected routes need HTTPS. Turn on HTTPS for the
  server, or use a tunnel."
- **Every protected address answers 500.** Traefik can't reach Outhaul: Outhaul is
  stopped, a firewall keeps Docker's network from the admin port, or Outhaul's port has
  changed since the app was last deployed.
- **"Signing in isn't available right now."** Outhaul has no public URL, or couldn't
  read its database. Outhaul's log says which.
- **"Your browser didn't keep the sign-in cookie for …"** Allow cookies for the app's
  address, then open it again.
- **"This address isn't protected by Outhaul."** The sign-in was for an address that
  doesn't require sign-in now, for example a domain turned off while the sign-in page
  was open. Open the app's address again.
- **"Outhaul can't sign you in to that exact address."** The address's path is written
  in an unusual way, for example with `%2F` or `..` in it. Open the app's main page and
  sign in from there.
- **"This sign-in link is incomplete."** Open the app's address again.
- **"This domain was changed by someone else."** Someone changed the same domain while
  your dialog was open. Check it and save again.
- **The Access column says "Not protected yet".** See [The Access column](#the-access-column).
- **An app's API calls get 401.** The sign-in has ended. Reload the page.

## Testing so far

`deploy/test/integration_gate.sh` puts the labels Outhaul gives protected and open
domains on a real Traefik (`traefik:v3.7.6`, Outhaul's default), with a small
stand-in answering for Outhaul, and checks what reaches the app. The rest is covered
by Outhaul's unit tests.

`deploy/test/integration_proxy.sh` runs a real Outhaul and Traefik behind a
stand-in CDN, with a stand-in OIDC provider, and checks sign-in, the route cookie,
`X-Outhaul-User` reaching an app, an open path under a protected host, webhooks and
the required header. The whole flow with a real CDN, a real browser and a real SSO
provider hasn't been run yet, and neither has the script's mode that uses a running
Outhaul in place of the stand-in (`--outhaul`). Tunnel mode hasn't been tried. If
something behaves differently from what's described here, open an issue.
