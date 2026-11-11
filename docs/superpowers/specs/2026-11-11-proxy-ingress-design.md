# Serving behind a TLS-terminating proxy

DNS-01 certificates, one wildcard, HTTPS only, and a required request header.

This covers requirements R1 to R4 of the brief "outhaul enhancements: running
behind Azure Front Door on a shared host" (10 November 2026). R5 onward get
their own designs. Code references are to `9df0e57`.

## Why

Outhaul is going onto a shared host that visitors reach only through a CDN.
The first one is Azure Front Door, but nothing here is specific to it. The CDN:

- terminates the visitor's TLS and connects to the host on 443 only;
- keeps the visitor's Host header, sends it as SNI, and refuses a certificate
  that isn't publicly trusted or doesn't cover that name;
- shares its source addresses with every other customer of the same CDN, so
  the network rule in front of the host admits all of them. Only a header the
  CDN sets (`X-Azure-FDID`, the ID of our profile) shows a request came through
  ours.

Port 80 is closed. Each host has one wildcard, `*.<host>.<zone>`, and only names
one label under it are routed to the host. The admin port (8080) can't be
reached from outside, so the admin UI, sign-in and webhooks go through the CDN
like any app. Other machines on the host's own network can still reach that
port, and it must not answer them: the admin UI is to be reachable only at its
public URL, on 443.

Outhaul can't serve anything there today:

- Traefik is given the HTTP-01 challenge only (`proxySpec`,
  `internal/traefik/proxy.go`), which needs port 80.
- Every router asks for a certificate for its own host (`RouteLabels`,
  `internal/traefik/labels.go`). Many apps and pull-request previews would hit
  Let's Encrypt's limits.
- Port 80 is always published, and every row has a plain-HTTP router.
- Rules match host and path only, so any customer of the CDN could reach every
  app and the admin UI.

## Goals

1. Certificates by DNS-01, with any lego DNS provider, its credentials passed
   to Traefik by name from Outhaul's own environment.
2. One wildcard certificate, requested once. A host under it never asks for its
   own.
3. An HTTPS-only mode: no port 80, no plain-HTTP router.
4. A required request header on every route. While it is set, no route answers
   a request without it.
5. The admin UI, password and SSO sign-in, protected routes and GitHub webhooks
   work through a TLS-terminating proxy, and a repeatable test shows it.
6. With a setting, the admin port answers only Traefik and the host itself, so
   the admin UI is reachable only at its public URL, on 443.
7. Every part is its own setting, off unless set, and usable without the others
   wherever that makes sense. These changes go into `main`, and other installs
   may need only some of them, or none.

## Non-goals

- The installer, updates, and anything else in R5 onward: container limits, a
  fixed Traefik address, default domains, trusting forwarded headers.
- An HTTP port other than 80.
- More than one wildcard.
- Keeping the header's value secret. Anyone with Docker access can read it in
  container labels. It is never shown in the UI or written to the log.
- Refusing domains outside the wildcard when one is saved (R9).
- Refusing `traefik.*` labels a Compose file writes for itself (see
  [Images and Compose files](#images-and-compose-files)).
- Settings for lego's DNS resolvers or its propagation check.
- A new response header for CDNs that ignore `CDN-Cache-Control` (see
  [Docs](#docs)).
- Keeping app containers from the admin port (see
  [The admin port](#the-admin-port)).

## Anti-goals

The work has failed, even if it runs, if any of these happen:

- An install that sets none of the new settings notices anything: a changed
  label, a changed Traefik flag, a recreated Traefik, a redeploy at startup.
- With the header required, some route answers a request without it.
- A protected route is weakened: its gate no longer comes first, an identity
  header gets through, a `..` path is routed, or routers order differently.
- A host under the wildcard triggers its own certificate request.
- A settings change leaves apps unreachable and nothing says why.

## Constraints

From `ARCHITECTURE.md`, `AGENTS.md` and the brief:

- Settings are `OUTHAUL_*` environment variables read through the `parser` in
  `internal/config/config.go`. `Validate` reports bad values and bad
  combinations, and `outhaul serve` refuses to start.
- App routes come from Docker labels. The file provider carries only the admin
  route and the local CA's certificates. No locked decision changes here.
- Test first. What depends on Traefik's behaviour is checked on a real Traefik.
- This repository is public. Use `example.com` names in code, tests and docs.

## Settings

| Variable | Values | Default | Needs |
|---|---|---|---|
| `OUTHAUL_ACME_DNS_PROVIDER` | a lego DNS provider's code, such as `azuredns` | unset: HTTP-01, as today | `OUTHAUL_ACME_EMAIL` |
| `OUTHAUL_ACME_DNS_ENV` | names of environment variables, comma-separated | unset | `OUTHAUL_ACME_DNS_PROVIDER`, and every named variable set and not empty |
| `OUTHAUL_TLS_WILDCARD` | `*.` and a domain, such as `*.shared.demo.example.com` | unset | `OUTHAUL_ACME_DNS_PROVIDER` |
| `OUTHAUL_HTTP_PORT` | `80` or `off` | `80` | `off` needs HTTPS (Let's Encrypt or the local CA). With Let's Encrypt it also needs `OUTHAUL_ACME_DNS_PROVIDER`. It can't be combined with a Cloudflare tunnel |
| `OUTHAUL_REQUIRE_HEADER_NAME` | a header name: letters, digits and `-` | unset | `OUTHAUL_REQUIRE_HEADER_VALUE` |
| `OUTHAUL_REQUIRE_HEADER_VALUE` | letters, digits and `-._~+/=:`, at most 256 characters | unset | `OUTHAUL_REQUIRE_HEADER_NAME` |
| `OUTHAUL_ADMIN_DIRECT` | `on` or `off` | `on` | `off` needs `OUTHAUL_PUBLIC_URL`, and HTTPS or a tunnel to publish it. `OUTHAUL_LISTEN_ADDR` may then give a port but not a host |

Each setting works without the others, apart from what its "Needs" column
says. An install can use DNS-01 with no wildcard, the header without HTTPS
only, or `OUTHAUL_ADMIN_DIRECT=off` with none of the rest.

`Validate` names the variable and what it wants, as it does for the existing
settings. A missing variable named in `OUTHAUL_ACME_DNS_ENV` is an error at
startup, not a certificate failure hours later.

The tunnel is a stored setting, not a variable, so the tunnel rule is enforced
where the local CA's is: `outhaul serve` refuses to start with
`OUTHAUL_HTTP_PORT=off` and a tunnel enabled, and Settings refuses to enable a
tunnel while it is set (`infraController.Enable`, `main.go`). cloudflared
reaches Traefik on port 80, which this mode doesn't have.

The first deployment sets these. systemd doesn't expand variables in an
environment file, so each value is written out:

```
OUTHAUL_PUBLIC_URL=https://outhaul.shared.demo.example.com
OUTHAUL_ACME_EMAIL=ops@example.com
OUTHAUL_ACME_DNS_PROVIDER=azuredns
OUTHAUL_ACME_DNS_ENV=AZURE_CLIENT_ID,AZURE_SUBSCRIPTION_ID,AZURE_RESOURCE_GROUP,AZURE_ZONE_NAME
OUTHAUL_TLS_WILDCARD=*.shared.demo.example.com
OUTHAUL_HTTP_PORT=off
OUTHAUL_REQUIRE_HEADER_NAME=X-Azure-FDID
OUTHAUL_REQUIRE_HEADER_VALUE=<the profile's GUID>
OUTHAUL_ADMIN_DIRECT=off
```

Outhaul passes Traefik exactly the variables named, and adds none. In
particular it never sets `AZURE_AUTH_METHOD`: with it unset, lego's `azuredns`
uses Azure's default credential chain, which honours `AZURE_CLIENT_ID`.

## Traefik's own configuration

`proxySpec` builds Traefik's flags, ports, mounts and environment, and hashes
them, so a change recreates the container.

**DNS-01.** With a provider set, the `le` resolver gets
`--certificatesresolvers.le.acme.dnschallenge=true` and
`--certificatesresolvers.le.acme.dnschallenge.provider=<provider>`, and no
`httpchallenge` flag. The email, storage and `OUTHAUL_ACME_STAGING` flags stay
as they are. Each variable named in `OUTHAUL_ACME_DNS_ENV` is added to the
container's environment as `NAME=value`. Values never appear in a flag or a log
line. The hash already covers the environment.

Before it asks for validation, lego looks the TXT record up itself at the
zone's authoritative name servers, from inside the Traefik container. So
Traefik needs outbound DNS on port 53. The first host's outbound traffic is
open, and Outhaul sets no resolver or propagation flags.

**The wildcard.** With `OUTHAUL_TLS_WILDCARD` set, the `websecure` entrypoint
gets `--entrypoints.websecure.http.tls.certresolver=le` and
`--entrypoints.websecure.http.tls.domains[0].main=<wildcard>`. A router on that
entrypoint with no `tls` setting of its own inherits both, and Traefik requests
the wildcard once for all of them.

**HTTPS only.** With `OUTHAUL_HTTP_PORT=off` there is no `web` entrypoint, no
port 80 mapping and no redirect flags.

**The header.** With a required header set, the Docker provider gets a
constraint; see [No route without the header](#no-route-without-the-header).

With none of the new settings, `proxySpec` must return exactly today's spec and
hash. Pin today's hash for each existing mode in a test before changing the
function.

Traefik `v3.7.6`, the pinned image, ships lego `v5.2.2`. Its `azuredns`
credential code is the same as in lego `v5.6.0`: with no auth method it builds
`DefaultAzureCredential`, and `msi` builds a managed-identity credential with
no client ID. So this needs no image change.

## Routers

`RouteLabels` takes nine positional arguments today, and `compose.Override` and
`adminDynamicConfig` repeat some of them. Start by passing the server-wide
routing settings as one value built from the configuration (HTTPS on, resolver,
wildcard, HTTP off, required header, gate address), with no change in output.
Every writer of a router then reads the same settings.

### The rule

A rule is its address, then the traversal term, then the header term when one
is required:

```
Host(`app.shared.demo.example.com`) && PathPrefix(`/api`) && !PathRegexp(`…`) && Header(`X-Azure-FDID`, `<value>`)
```

- `priority` stays the length of the address alone, as today.
- `RuleAddress` accepts an address, an address with the traversal term, and
  that followed by a header term with any name and value. A container labelled
  under an earlier value still has an address the pages can read. Anything else
  has no address, as today.
- `SameAddress` still compares host and path only.

With HTTP and HTTPS both on, port 80 answers every request with the redirect to
HTTPS before any rule is read, as today. A redirect serves nothing and names no
route, so it stays, and the header is checked on 443. With HTTP on and no
HTTPS, and in tunnel mode, the `web` routers serve the apps and carry the
header term.

### Which routers a row gets

| Mode | Routers |
|---|---|
| HTTP on (today) | `<router>` on `web`, and `<router>-tls` on `websecure` when the row is served over HTTPS |
| HTTP off | `<router>` on `websecure` only |

With HTTP off there is no plain HTTP, so every row is served over HTTPS
whatever its own "Automate HTTPS" box says. The single router keeps the row's
base name, so `RowHasGate` and the code that matches a container's router to a
row work unchanged.

A router on `websecure` gets its TLS labels by where its certificate comes
from:

| Certificate | Labels |
|---|---|
| The wildcard covers the host | none: it inherits the entrypoint's resolver and wildcard |
| Let's Encrypt, any other host | `tls=true` and `tls.certresolver=le`, as today |
| Local CA | `tls=true`, as today |

The wildcard covers a host that is exactly one label under it, compared without
case. This is one pure function, used by every writer of a router.

A host the wildcard doesn't cover asks for its own certificate through the same
resolver, by DNS-01. That keeps an apex domain or a second zone working for
installs that have them. On the first deployment the CDN doesn't route such
names, and R9 will refuse them when they are saved.

The middlewares don't change: the gate first, the identity headers removed,
then any path rewrite.

### Where routers are written

- Single-container apps: `AppLabels`.
- Compose services: `compose.Override`. The header value's characters are
  limited so that the YAML and Compose's `$` interpolation need no new escaping.
- Previews and template apps are apps, deployed by the same worker, so they
  need nothing of their own.
- The admin UI: [The admin route](#the-admin-route).

The gate has no router. Sign-in pages are on the admin host, and Traefik calls
`/gate/check` on the host directly.

## No route without the header

Containers keep the labels they were created with. So when the header is
turned on, or its value changes, every container deployed before then still
carries rules without it. Three things together close that.

### A stamp, and a Traefik constraint

When any of the header, `OUTHAUL_HTTP_PORT=off` or the wildcard is set, every
container Outhaul gives routes to also carries `outhaul.ingress=<stamp>`. The
stamp is a short hash of those three settings. With none of them set there is
no stamp label, so today's labels don't change.

While a header is required, Traefik's Docker provider is started with

```
--providers.docker.constraints=Label(`outhaul.ingress`,`<stamp>`)
```

Traefik then ignores every container that doesn't carry the current stamp. That
covers a container labelled before the setting or under another value, one
whose redeploy failed, and one Outhaul didn't create. From the moment Traefik
is recreated with the constraint, such a container has no routes at all, and
the request gets Traefik's 404. There is no catch-all router.

The constraint is a flag, so the hash covers it and a change recreates Traefik.
It applies to the Docker provider only. The admin route is in the file
provider, which Outhaul rewrites itself.

Without a required header there is no constraint. A change to
`OUTHAUL_HTTP_PORT` or the wildcard alone leaves old containers serving through
whatever routers Traefik still accepts until they are redeployed.

### When Traefik can't be replaced

The constraint holds only once Traefik runs with it. Today `EnsureProxy` pulls
the image before it replaces the container, so a failed pull leaves the old
Traefik running with its old flags, and `ensureInfra` logs a warning and
doesn't try again. With a header newly required, that old Traefik would go on
serving every old container without the check.

The brief says no route may exist without the check, so while a header is
required this path fails closed:

- If the pull fails and the image is already on the server, Traefik is
  recreated from the local image. A change of flags alone never needs a pull.
- If the pull fails and the image isn't on the server, the old Traefik is
  stopped. Nothing is served until the new one starts.
- Either way, when the reconcile at startup fails, Outhaul tries again until it
  has succeeded once, and logs each failure.

So whenever Docker can be reached, the running Traefik has the current
constraint or there is none running, and the pages can go by the settings. With
no header required, `EnsureProxy` behaves as it does today.

### Redeploying at startup

Settings change only at a restart. So after Traefik is ensured at startup,
Outhaul lists its containers and finds the routed ones whose stamp differs from
the current one. No label counts as an empty stamp, and so does no setting, so
an install that never set any of this finds nothing.

For each app that has such a container and is running, it queues the deploy a
protection change queues (`chooseProtectionDeploy`,
`internal/server/protection.go`):

- a queued deploy that hasn't started applies the change itself;
- a Nixpacks or Dockerfile app whose running image is still on the server
  redeploys that image without building;
- otherwise, and always for a Compose app, a normal deploy. That ships the head
  of the branch, or the newest matching tag. A template app redeploys its own
  Compose file.

Outhaul logs one line per app saying what it queued and why, and one summary
line. A stopped app isn't deployed, and its next deploy applies the change. A
container whose app no longer exists is named in a warning and left alone.

If the containers can't be listed at startup, Outhaul tries again until the
check has run once.

Restart already deploys instead of restarting when a container's labels
disagree with its rows' protection (`protectionMismatch`). A stale stamp counts
the same way, so Restart never brings old rules back.

### Images and Compose files

Docker copies an image's labels onto its containers. An image that carries
`traefik.*` labels would give its container a router Outhaul didn't write,
without the header term. Fork previews are already refused for this
(`refuseRoutingLabels`, `internal/deploy/pipeline.go`). While a header is
required, the same check runs for every single-container deploy, and the deploy
fails naming the labels.

A Compose file can write `traefik.*` labels on its own services. On a service
Outhaul doesn't route, the constraint makes Traefik ignore them. On a service
Outhaul does route, they sit beside Outhaul's own and are not checked here.
Checking them needs the normalised Compose model, which R6 introduces. R6's
list of refused options should include `traefik.*` labels.

## The admin route

`writeAdminDynamicConfig` writes the admin router into the file provider on
every reconcile, and Traefik reloads it. It follows the same rules as an app's
router:

- its rule ends with the header term when one is required;
- with HTTP off, or under Let's Encrypt or the local CA, it is on `websecure`;
- when the wildcard covers the admin host it has no `tls` key and inherits the
  entrypoint's; otherwise `tls.certResolver: le`, or `tls: {}` under the local
  CA;
- in tunnel mode it stays on `web` with no TLS, as today.

## The admin port

Today the admin UI listens on every interface (`OUTHAUL_LISTEN_ADDR`, `:8080`).
Traefik reaches it as `host.docker.internal`, which Docker maps to the host
(`host-gateway`). Anything else that can reach the port gets the admin UI over
plain HTTP, past Traefik and so past the required header.

With `OUTHAUL_ADMIN_DIRECT=off`, Outhaul listens on two addresses only, on the
port `OUTHAUL_LISTEN_ADDR` gives:

- loopback, for the host itself: the installer's health check, and someone
  with a shell on the host;
- the gateway address of Outhaul's own Docker network (`OUTHAUL_NETWORK`),
  which is the host's address on that network's bridge. Traefik is on that
  network.

Traefik's `host.docker.internal` is then mapped to that gateway address
instead of `host-gateway`. The hash covers the mapping, so switching the
setting recreates Traefik once. The gate's address in app labels stays
`http://host.docker.internal:<port>/gate/check`, so no app needs redeploying.

The port isn't open on the host's other addresses at all, so another machine
gets a refused connection. The admin UI is reachable only through Traefik, at
the public URL.

- Reading the gateway address needs Docker, and the Docker client needs a way
  to read a network's gateway. `ensureInfra` creates the network before the
  server starts listening. If Docker can't be reached then, Outhaul listens on
  loopback only, and adds the other address once Docker answers.
- First-boot setup uses the public URL, which the startup hint already prints
  when Traefik publishes the admin UI.
- App containers on Outhaul's network can still reach the port at the gateway
  address, as they can today. They get the same sign-in page as anyone.
- When a header is required and `OUTHAUL_ADMIN_DIRECT` is on, Outhaul logs a
  warning at startup that the admin port isn't covered by the header. It
  doesn't refuse to start: a firewall may be doing this job.

## What the pages show

- With HTTP off, a row's "Automate HTTPS" box is shown on and can't be changed,
  with a line saying the server serves HTTPS only. Every link to a row uses
  `https`, including preview links, pull-request comments and the "is live at"
  line in a deploy log.
- Wherever a page asks whether Traefik routes a container (`routed`,
  `internal/server/protection.go`), the answer follows the constraint: while a
  header is required, a container without the current stamp isn't routed. Its
  rows read "applies on the next deploy", which is true.
- The app page says when an app's containers aren't served because they were
  deployed before the server's routing settings changed, and that deploying it
  fixes that. This is what someone sees when the startup redeploy failed.
- The header's name and value appear on no page.

## Through the proxy, end to end

Little should need changing for R4, because Outhaul already builds its URLs
from `OUTHAUL_PUBLIC_URL`, and Traefik sets `X-Forwarded-Proto` itself:

- sign-in callback, webhook and gate URLs come from the public URL
  (`baseURL`, `ssoRedirectURI`, the gate's sign-in pages);
- session cookies are `Secure` on the public host; the gate's cookies always;
- cross-origin protection compares against the Host the proxy passes through.

So R4 is mostly a test, and the fixes for whatever it finds. Nothing has run a
real Outhaul behind a real Traefik end to end before.

**`deploy/test/integration_proxy.sh`**, not run by CI, in the style of
`integration_gate.sh`. It runs a real Outhaul with its own Docker daemon, in a
privileged container as `deploy/test/integration.sh` does. Outhaul's Traefik
has a fixed container name, so a second Outhaul can't share a daemon with one
that is already running.

- Outhaul runs with the local CA, `OUTHAUL_HTTP_PORT=off`, a required header,
  `OUTHAUL_ADMIN_DIRECT=off` and a public URL.
- A stand-in for the CDN sits in front of Traefik. It terminates TLS, forwards
  over HTTPS with the request's Host as both Host and SNI, verifies the origin
  certificate against the local CA's root, and overwrites the required header.
  nginx does all four with stock directives.
- A small stand-in OIDC provider, built from the one `internal/sso`'s tests
  use.

It checks, through the stand-in:

1. first-boot setup and password sign-in;
2. SSO sign-in;
3. a protected app: the redirect to sign in, the code, the `__Host-` cookies,
   `X-Outhaul-User` reaching the app, and an open path under it;
4. a signed GitHub push delivery to an app's webhook URL queues a deploy;
5. straight to Traefik, past the stand-in: no header gives 404, a wrong value
   gives 404, for an app and for the admin host;
6. the admin port: it answers on loopback, refuses a connection on the
   machine's main address, and every check above still passes, the gate's
   included.

**`deploy/test/integration_ingress.sh`**, not run by CI. It checks Traefik's
flags and labels as the real code writes them (a helper prints them, as
`deploy/test/gatelabels` does), with Pebble as the ACME server and
`pebble-challtestsrv` as the DNS, reached through lego's `httpreq` provider and
a small adapter. Its variables are passed by name, which exercises
`OUTHAUL_ACME_DNS_ENV`. The script adds the flags that point Traefik at Pebble;
Outhaul gets no setting for another ACME server. It checks:

1. the wildcard is issued once, and `acme.json` holds only it;
2. apps under the wildcard cause no further order (Pebble's log);
3. a host outside the wildcard gets its own certificate;
4. with HTTP off, only 443 is published and Traefik logs no router error;
5. the header: right value 200, wrong or missing 404, on 443 with HTTP on and
   with HTTP off;
6. the constraint: a container with no stamp, or another stamp, has no router;
7. `integration_gate.sh`'s protected and open rows behave the same with the
   header term and with HTTP off. Run its checks against these labels rather
   than copying them.

## Checked against real Traefik so far

On `traefik:v3.7.6`, 11 November 2026, with throwaway containers:

- A router on `websecure` with no `tls` key inherits the entrypoint's
  `certResolver` and `domains`. Traefik asked for `*.probe.test` once, and for
  no host under it.
- A router with its own `tls.certResolver` asks for its own host.
- A router that names a missing `web` entrypoint is disabled with an error:
  the reason no label may name `web` with HTTP off.
- ``Header(`X-Probe-Id`, `abc-123`)`` after the traversal term: the right value
  routes, a wrong or missing one gets 404, and the header's name matches in any
  case.
- With ``--providers.docker.constraints=Label(`outhaul.ingress`,`abc123`)``,
  only the container with that exact label got a router.

And on Docker 29.9.1: a process on the host listening only on a Docker
network's gateway address was reached from a container on that network through
a host mapping to that address, saw the container's own address as the client,
and refused connections on the host's main address.

Not checked yet: a real DNS-01 order, and whether a certificate already issued
for a host is kept when the wildcard arrives. The scripts above cover the
first.

## Tests

- `internal/config`: each new setting's values, and each refused combination.
- `internal/traefik`: `proxySpec` per mode, and unchanged spec and hash with
  nothing set; `RouteLabels` per mode; `RuleAddress` on old rules, new rules,
  rules under another header value, and rules it must still refuse; the
  wildcard-covers function; the stamp; the admin config per mode.
- `internal/compose`: the override per mode.
- `internal/deploy`: the image-label refusal runs for every single-container
  deploy while a header is required, and only for forks otherwise.
- `internal/server`: the startup redeploy picks what a protection change picks,
  skips stopped apps, and does nothing with nothing set; Restart deploys on a
  stale stamp; the Access column for an unrouted container.
- `main`: the tunnel refusals; with `OUTHAUL_ADMIN_DIRECT=off`, the two listen
  addresses, loopback alone while Docker can't be reached, and the warning
  when a header is required with it on.
- `internal/docker`: reading a network's gateway, against a faked Engine API
  as `cancel_test.go` does.

## Docs

- `README.md`: the configuration table.
- A new `docs/BEHIND-A-PROXY.md`: what each setting does, the example above,
  what restarts and redeploys when a setting changes, that the header is only
  as good as the network rule in front of it and covers the admin port only
  with `OUTHAUL_ADMIN_DIRECT=off`, that Traefik needs outbound DNS for DNS-01,
  and the list in
  [What only the real host can show](#what-only-the-real-host-can-show).
- `docs/PROTECTED-ROUTES.md`: "Before you start", for the new HTTPS-only mode.
  "A CDN in front": the two `no-store` headers follow a convention that not
  every CDN honours. Behind one that doesn't, caching must stay off for the
  routes to this host, or a protected page could be served from the CDN's
  store to someone who never signed in. The first deployment has caching off.
  Protected rows' labels don't change.
- `ARCHITECTURE.md`: a new section for this mode, and the rule's three parts.
- `AGENTS.md`, "Decisions to keep": the rule's shape and what `RuleAddress`
  accepts; the stamp and the constraint; that no label may name `web` with HTTP
  off.

## Order of work

One series of commits per requirement on `enhancements`, each with its tests
and docs, so they can be opened as separate pull requests:

1. Pass the routing settings as one value. No change in output.
2. R2: HTTPS only.
3. R1: DNS-01 and the wildcard, with `integration_ingress.sh`.
4. R3: the required header, the stamp and constraint, the startup redeploy.
5. R4: the admin port (`OUTHAUL_ADMIN_DIRECT`).
6. R4: `integration_proxy.sh`, and what it finds.

## What only the real host can show

For the infrastructure side to run on the staging host. It goes in the pull
requests and in `docs/BEHIND-A-PROXY.md`.

1. `docker port outhaul-traefik` shows only 443.
2. `acme.json` holds one certificate, for the wildcard, and Traefik's log shows
   no HTTP challenge.
3. `https://outhaul.<host>.<zone>/` loads through the CDN, and password and
   Okta sign-in work.
4. An app deployed to `<name>.<host>.<zone>` is served through the CDN with the
   wildcard certificate.
5. From another machine in the network, `curl -k --resolve
   <app>.<host>.<zone>:443:<host IP> https://<app>.<host>.<zone>/` returns 404,
   and so does the same with a made-up `X-Azure-FDID`.
6. A protected app's sign-in works through the CDN.
7. `ss -ltn` on the host shows port 8080 on loopback and one Docker bridge
   address only. From another machine in the network, a connection to the
   host's port 8080 is refused.

## Left to the builder

- The name and shape of the value that carries the routing settings, and the
  exact wording of errors, notices and log lines.
- Whether a Traefik that can't be replaced is stopped only when it lacks the
  current constraint, or whenever its configuration differs while a header is
  required, which is simpler.
- What goes into the stamp's hash beyond the three settings, and its length.
- Where the startup redeploy lives. The choice of deploy is in
  `internal/server` today.
- The stand-in proxy and the OIDC stand-in for `integration_proxy.sh`.

## Calls made in this design

These go beyond the brief, or choose between readings of it. Each is the
designer's call, not something the maintainer has agreed to.

1. `OUTHAUL_HTTP_PORT` takes `80` or `off` only. Another port would need page
   links and the redirect to follow it.
2. One wildcard, not a list.
3. A host outside the wildcard gets its own certificate by DNS-01 rather than
   being refused.
4. With HTTP off, every row is served over HTTPS whatever its own box says.
5. The stamp and the Traefik constraint. The brief asks for redeploys and a
   check at startup; the constraint also covers the time before those finish,
   and a redeploy that fails.
6. While a header is required, an image that carries `traefik.*` labels is
   refused for every single-container app, not only for forks.
7. `OUTHAUL_HTTP_PORT=off` and a Cloudflare tunnel can't be combined.
8. A variable named in `OUTHAUL_ACME_DNS_ENV` but not set stops startup.
9. No setting for another ACME server. Pebble is used at the Traefik level, and
   the end-to-end test uses the local CA.
10. The header value's characters are limited, so no writer has to escape it.
11. While a header is required, a Traefik that can't be replaced is stopped
    rather than left serving without the check.
12. With HTTP and HTTPS both on, port 80's redirect answers without the header.
13. A CDN that ignores `CDN-Cache-Control` gets a warning in the docs, not a
    new header on protected rows.
14. The startup redeploy ships the head of the branch for a Compose app, and
    for any app whose running image is no longer on the server. The brief asks
    for the redeploy; this is what it means for those apps.
15. How the admin port is closed: a setting of its own, `OUTHAUL_ADMIN_DIRECT`,
    that narrows where Outhaul listens. The maintainer asked for the admin UI
    to be reachable only at its public URL on 443; the mechanism and the name
    are the designer's. It isn't implied by the required header, so each can be
    used alone.
16. A required header with the admin port still open is a warning at startup,
    not a refusal to start.
