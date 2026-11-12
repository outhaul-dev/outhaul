# Serving behind a TLS-terminating proxy

This page is for an install whose visitors reach it only through a CDN or other
TLS-terminating proxy on port 443. The first one is Azure Front Door, but
nothing here is specific to it. Each part is a setting of its own, off unless
set, so an ordinary install needs none of them, and an install can use only the
ones it needs.

## The request path, and why each setting exists

The CDN terminates the visitor's TLS and connects to the host on 443 only. It
keeps the visitor's Host header, sends it as SNI, and refuses a certificate
that isn't publicly trusted or doesn't cover that name. Port 80 is closed.

The CDN shares its source addresses with every other customer of the same CDN,
so the network rule in front of the host admits all of them. Only a header the
CDN sets (`X-Azure-FDID`, the ID of our profile) shows a request came through
ours.

Each host has one wildcard, `*.<host>.<zone>`, and only names one label under it
are routed to the host. The admin port (8080) can't be reached from outside, so
the admin UI, sign-in and webhooks go through the CDN like any app. Other
machines on the host's own network can still reach that port, and it must not
answer them.

So the settings cover: certificates by DNS-01 with one wildcard
(`OUTHAUL_ACME_DNS_PROVIDER`, `OUTHAUL_ACME_DNS_ENV`, `OUTHAUL_TLS_WILDCARD`);
no port 80 (`OUTHAUL_HTTP_PORT`); a required request header on every route
(`OUTHAUL_REQUIRE_HEADER_NAME`, `OUTHAUL_REQUIRE_HEADER_VALUE`); and an admin
port only Traefik can reach (`OUTHAUL_ADMIN_DIRECT`). A fifth setting, which
proxies Traefik believes about the visitor's address (`OUTHAUL_TRUSTED_PROXIES`),
is covered under "Trusted proxies".

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
| `OUTHAUL_TRUSTED_PROXIES` | IP addresses and CIDRs, comma-separated, or `all` | unset: no proxy is trusted, as today | `all` should be paired with `OUTHAUL_REQUIRE_HEADER_NAME` and `..._VALUE`; `serve` warns when it is not |

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
OUTHAUL_TRUSTED_PROXIES=all
```

Outhaul passes Traefik exactly the variables named, and adds none. In
particular it never sets `AZURE_AUTH_METHOD`: with it unset, lego's `azuredns`
uses Azure's default credential chain, which honours `AZURE_CLIENT_ID`.

## Certificates

### DNS-01

`OUTHAUL_ACME_DNS_PROVIDER` names a lego DNS provider, such as `azuredns`.
Traefik then proves control of each name by a DNS TXT record instead of the
HTTP challenge, so no port 80 is needed. `OUTHAUL_ACME_DNS_ENV` lists the
environment variables the provider needs, comma-separated and by name, for
example `AZURE_CLIENT_ID,AZURE_SUBSCRIPTION_ID`. Outhaul passes Traefik exactly
those variables and adds none; it never sets `AZURE_AUTH_METHOD`, so lego's
`azuredns` uses Azure's default credential chain and honours `AZURE_CLIENT_ID`.
A named variable that is unset or empty stops startup rather than failing hours
later during a certificate request.

Traefik needs outbound DNS on port 53: before asking for validation, lego looks
the TXT record up at the zone's authoritative name servers from inside the
Traefik container. Outhaul sets no resolver or propagation flags.

### The wildcard

`OUTHAUL_TLS_WILDCARD=*.shared.demo.example.com` makes the `websecure`
entrypoint ask for one certificate for the wildcard. A router on that entrypoint
with no `tls` setting of its own inherits it, so every host exactly one label
under the wildcard is served without its own certificate request: for example
`app.shared.demo.example.com` and `outhaul.shared.demo.example.com`, but not
`shared.demo.example.com`, `a.b.shared.demo.example.com`, or
`app.shared.demo.example.com.`.

A host outside the wildcard gets its own certificate through the same resolver,
by DNS-01. On a CDN-fronted install the CDN usually routes only names under the
wildcard; a name outside it is still useful for an apex domain or a second
zone.

Checked on `traefik:v3.7.6`: a host that already had its own certificate keeps
it when the wildcard is turned on over the same `acme.json`; it is not
re-issued from the wildcard until the stored certificate is no longer used.
`deploy/test/integration_ingress.sh` reports what a run sees.

## HTTPS only

`OUTHAUL_HTTP_PORT=off` publishes no port 80. Traefik has no `web` entrypoint
and every router is on `websecure`; there is no plain-HTTP router and no
redirect from 80. Every domain row is served over HTTPS, whatever its own
"Automate HTTPS" box says.

It needs HTTPS: `OUTHAUL_ACME_EMAIL` or `OUTHAUL_LOCAL_CA`. With Let's Encrypt
it also needs a DNS-01 challenge (`OUTHAUL_ACME_DNS_PROVIDER`), because the
HTTP challenge uses port 80.

It can't be combined with a Cloudflare Tunnel: cloudflared reaches Traefik on
port 80, which this mode doesn't have. `outhaul serve` refuses to start with
both, and Settings refuses to enable a tunnel while it is set.

Every page shows a domain row as HTTPS and links it over HTTPS, including
preview links and the "is live at" line in a deploy log. A row's "Automate
HTTPS" box is shown on and can't be changed, with a note saying the server
serves HTTPS only.

## The required header

`OUTHAUL_REQUIRE_HEADER_NAME` and `OUTHAUL_REQUIRE_HEADER_VALUE` put a header
term in every router's rule: the admin route's too. Traefik answers 404 for a
request that lacks the header or carries another value; the name matches in any
case. Both settings or neither.

The value is limited to letters, digits and `-._~+/=:`, at most 256 characters,
so no rule writer or YAML emitter has to escape it. It is never shown in the UI
or written to a log line; an error names the variable, not the value. The name
appears on no page either.

This header is only as good as the network rule in front of the host. Anyone
who can reach the host directly can send it. It is not a secret: a container's
labels in Docker show it.

With port 80 open as well, its redirect to HTTPS answers before any rule is
read, so the header is not checked there; it is checked on 443.

## Trusted proxies

By default Traefik believes no one: it replaces `X-Forwarded-For`, `-Host`,
`-Proto` and `-Port` with what it sees on the connection, so an app sees the
CDN's address, not the visitor's. `OUTHAUL_TRUSTED_PROXIES` names the peers whose
forwarded headers Traefik keeps and adds to. It takes IP addresses and CIDRs,
comma-separated (`192.0.2.0/24,2001:db8::/32`), or `all`. Unset, it changes
nothing. A malformed list stops startup, naming the setting.

It applies to every entrypoint Traefik declares: `web` and `websecure`, `web`
alone in tunnel mode, and `websecure` alone with `OUTHAUL_HTTP_PORT=off`.

The addresses are the ones Traefik sees on the connection, not the visitor's. A
connection from the host itself can arrive from the Docker bridge gateway, not
from the host's own address, and a CDN's addresses are the ones its edge
connects from. An entry that matches no peer trusts nobody, which is what an
unset list does.

`all` trusts any peer that can connect. It is safe only when nothing but the
proxy can reach Traefik's ports, and it is meant to be paired with the required
header, so a request that skips the proxy matches no router. `outhaul serve`
logs a warning when `all` is set without both header settings; it doesn't refuse
to start, because a firewall may be doing that job. The warning never contains a
header value.

### What an app receives

With the CDN trusted, `X-Forwarded-For` is the visitor's address followed by the
CDN's, as Traefik adds the address it saw: `203.0.113.7, 198.51.100.20`. This
assumes the CDN appends the visitor's address to the chain, as a proxy does. If
it forwards a `X-Forwarded-For` the client supplied without adding an entry of
its own, the chain holds forged entries, and nothing before the CDN's own
address can be trusted; check what your CDN does first. Where it does append, the
entry before the last is the visitor's. Tell the app's framework there are two
proxies in front of it (Traefik and the CDN), or it will take the CDN's address,
or worse, the first entry, which a visitor can write. `X-Forwarded-Host` and
`-Proto` are what the CDN sent. A visitor's own values for them reach the app
only if the CDN passes them on, so the CDN should overwrite them.

`X-Real-Ip` is not the visitor's address unless the proxy sets it. Traefik keeps
the value a trusted proxy sends, or writes the peer's address when none arrives,
and then Outhaul's `outhaul-forwarded` middleware removes the header (see below),
so an app never receives `X-Real-Ip` while trust is on. Even before that, a
trusted proxy that forwards a client's own `X-Real-Ip` would let the client
choose it, so an app should read `X-Forwarded-For`.

### The headers removed

While trust is on, a middleware named `outhaul-forwarded`, in a file Outhaul
writes under Traefik's file provider (`outhaul-forwarded.yml`), is set on every
entrypoint. It removes `X-Real-Ip`, `X-Forwarded-Uri`, `X-Forwarded-Method`,
`X-Forwarded-Prefix`, `X-Forwarded-Tls-Client-Cert` and
`X-Forwarded-Tls-Client-Cert-Info` from every request, so a visitor can't forge
them through a trusted proxy.

Each of those entrypoints also drops every request header whose name holds an
underscore (`--entrypoints.<name>.http.underscoreheadersstrategy=delete`). Many
CGI-style stacks (PHP, and many WSGI and Rack setups) fold `X_Real_Ip` into the
same variable as `X-Real-Ip`, so without this a visitor could still choose the
address such an app reads. It is blunt on purpose: an app's own header with an
underscore in its name, such as `My_App_Header`, is dropped too, while trust is
on. An entrypoint that trusts nobody already drops them. The flag needs Traefik
v3.7.6 or newer, the version Outhaul bundles; if `OUTHAUL_TRAEFIK_IMAGE` pins an
older image, upgrade it before setting `OUTHAUL_TRUSTED_PROXIES`.

If the file is missing, Traefik disables the routers on the entrypoints that name
the middleware and answers 404, so it fails closed. It is not the gate: the gate
stays in each app's own labels, and the middleware is not on any router.
Outhaul replaces the file by writing a temporary copy and renaming it, and leaves
it alone when it already holds the right content, so a rewrite never shows
Traefik an empty file. The directory is all or nothing while trust is on: a
sibling file in it that fails to parse also takes the routes down when Traefik
starts, and the same fail-closed answer follows.

Turning the setting off removes the file only once the replacement Traefik has
started. If its image can't be pulled, the file stays and a failed pull can't take
every route down, with one exception: while a request header is required, an old
Traefik without the constraint must not keep serving, so it is replaced from a
local image if there is one, and otherwise stopped, which fails closed.

### The gate is unchanged

A protected route's forward-auth keeps `trustforwardheader=false`, so a client
can't choose what the gate is told: it receives the `X-Forwarded-*` headers
Traefik writes for that call, and never the client's `X-Real-Ip` or
`X-Forwarded-Prefix`. No label and no ingress stamp changes with this setting, so
no app redeploys when it does; changing it recreates Traefik once.

## Uploads that take over a minute

Checked on `traefik:v3.7.6`: with its default settings, Traefik cut a request
whose body took 90 seconds to arrive at 60 seconds. That is its entrypoint read
timeout (`respondingTimeouts.readTimeout`), which covers the whole request, body
included. It applies to every app's uploads, not only to a push of code.

`OUTHAUL_UPLOAD_TIMEOUT` sets it, in whole seconds (`600s`, `10m`). Each
entrypoint Outhaul declares gets:

```
--entrypoints.<name>.transport.respondingtimeouts.readtimeout=<value>
```

The flag is part of Traefik's config hash, so changing the setting recreates
Traefik once. With the setting unset, nothing changes. Like
`OUTHAUL_TRUSTED_PROXIES`, it changes no label, so no app redeploys. A CDN or
proxy in front of Traefik has timeouts of its own, and they apply as well.

Outhaul's own server has no read timeout past the headers, so nothing else on
the host is in the way.

## When a setting changes

Settings are read at startup. A change that alters Traefik's flags or
environment recreates the Traefik container; the pages and the deploy log say
so before it happens.

Containers keep the labels they were created with. At startup Outhaul lists its
containers and finds the routed ones whose stamp differs from the current
settings. For each running app it queues the deploy a protection change would
queue: a Nixpacks or Dockerfile app whose running image is still on the server
redeploys that image without building; otherwise, and always for a Compose app,
a normal deploy ships the head of the branch (or the newest matching tag), and
a template app redeploys its own Compose file. A stopped app is left alone;
its next deploy carries the change. The log has one line per app and one
summary line.

If the containers can't be listed, Outhaul tries again every minute until the
check has run once. Restart deploys instead of restarting when a container's
stamp is stale, so it never brings old rules back.

While a request header is required, an app whose containers Traefik is ignoring
says "This app isn't being served" and that deploying it brings it back. That is
what someone sees when the startup redeploy failed. If Traefik itself can't be
replaced, it is stopped rather than left serving containers without the check;
Outhaul retries the reconcile until it succeeds.

## The admin port

By default the admin UI listens on every interface
(`OUTHAUL_LISTEN_ADDR`, `:8080`), and anything that can reach that port gets
the admin UI over plain HTTP, past Traefik and so past the required header.

With `OUTHAUL_ADMIN_DIRECT=off`, Outhaul listens on two addresses only, on the
port `OUTHAUL_LISTEN_ADDR` gives:

- loopback, for the host itself: the installer's health check, and someone
  with a shell on the host;
- the gateway address of Outhaul's own Docker network (`OUTHAUL_NETWORK`),
  which is the host's address on that network's bridge. Traefik is on that
  network.

Traefik's `host.docker.internal` is mapped to that gateway address instead of
Docker's `host-gateway`, and the admin UI is reachable only through Traefik, at
the public URL. A connection to the host's other addresses is refused. The
gate's address in app labels stays `http://host.docker.internal:<port>/gate/check`,
so no app needs redeploying.

It needs `OUTHAUL_PUBLIC_URL` and HTTPS or a Cloudflare Tunnel: otherwise
nothing would publish the UI. `OUTHAUL_LISTEN_ADDR` may give a port but not a
host. First-boot setup uses the public URL, which the startup hint prints when
Traefik publishes the admin UI.

Reading the gateway needs Docker. If Docker can't be reached at startup,
Outhaul listens on loopback only and adds the gateway address once Docker
answers, checking every five seconds.

App containers on Outhaul's network can still reach the port at the gateway
address, as they can today; they get the same sign-in page as anyone. And when
a header is required with `OUTHAUL_ADMIN_DIRECT=on`, Outhaul logs a warning at
startup that the admin port isn't covered by the header. It doesn't refuse to
start: a firewall may be doing that job.

## Images and Compose files with their own Traefik labels

Docker copies an image's labels onto its containers, and Traefik reads container
labels. While a request header is required, an image that carries any
`traefik.*` label is refused for every single-container app, not only for fork
previews: its router would be Outhaul's not to write, and it would lack the
header term. The deploy fails and names the labels.

A Compose file can write `traefik.*` labels on its own services. While a request
header is required, such a file is refused before it is built, as is any service
image whose own labels carry `traefik.*` after the pull: either would add a route
without the header. On a service Outhaul doesn't route, Traefik's constraint
ignores the labels, but the file is refused anyway. `outhaul.*` labels are
refused too, because Outhaul finds a stack's containers and the ingress stamp by
them.

Reading a Compose file this way asks `docker compose` for its normalised JSON,
so a header-required Compose deploy needs Docker Compose 2.35 or newer even
when strict mode is off. Its `docker compose` runs also get the reduced
environment described in [GUARD-RAILS.md](GUARD-RAILS.md#the-environment-compose-sees),
so a file's `${OUTHAUL_REQUIRE_HEADER_VALUE}` is empty. The stack's own `.env`
still supplies the app's variables.

## Checking a real host

These need the staging host and its CDN.

1. `docker port outhaul-traefik` shows only 443.
2. `acme.json` holds one certificate, for the wildcard, and Traefik's log shows
   no HTTP challenge.
3. `https://outhaul.<host>.<zone>/` loads through the CDN, and password and Okta
   sign-in work.
4. An app deployed to `<name>.<host>.<zone>` is served through the CDN with the
   wildcard certificate.
5. From another machine in the network, `curl -k --resolve
   <app>.<host>.<zone>:443:<host IP> https://<app>.<host>.<zone>/` returns 404,
   and so does the same with a made-up `X-Azure-FDID`.
6. A protected app's sign-in works through the CDN.
7. `ss -ltn` on the host shows port 8080 on loopback and one Docker bridge
   address only. From another machine in the network, a connection to the
   host's port 8080 is refused.
8. With `OUTHAUL_TRUSTED_PROXIES` set to the CDN's addresses, an app that echoes
   its request headers, reached through the CDN, shows an `X-Forwarded-For`
   whose entry before the last is the visitor's address, and the CDN's address
   last.
9. The same request with a made-up value for each of `X-Forwarded-Host`,
   `X-Forwarded-Proto`, `X-Forwarded-Port`, `X-Real-Ip`, `X-Forwarded-Uri`,
   `X-Forwarded-Method` and `X-Forwarded-Prefix`, sent from the visitor's side,
   shows the first three overwritten by the CDN or absent, and the other four
   absent at the app. So are the same four with underscores in place of the
   hyphens (`X_Real_Ip`), and `X-Forwarded-Tls-Client-Cert` and
   `X-Forwarded-Tls-Client-Cert-Info`.
10. A protected app's sign-in still works through the CDN with trust on.

## Testing so far

Three scripts check this without a real CDN, and none is run by CI:

- `deploy/test/integration_ingress.sh` starts Pebble and
  `pebble-challtestsrv` and a Traefik built from `traefik.ProxyFlags`. It checks
  that the wildcard is issued once, that hosts under it cause no further order,
  that a host outside it gets its own certificate, that an HTTPS-only Traefik
  publishes only 443, that requests carry the right certificate, and that the
  header and the constraint behave as described.
- `deploy/test/integration_gate.sh` (with `GATETEST_HEADER` and
  `GATETEST_HTTP_OFF`) checks protected and open rows on a real Traefik with the
  header term and with no port 80. It also runs a Traefik that trusts its peer
  and one that trusts a list matching nobody, and checks which forwarded headers
  reach an app, that `X-Real-Ip`, `X-Forwarded-Uri`, `-Method`, `-Prefix`, their
  underscore spellings and the two TLS client-cert headers are removed in both, that the gate answers the same, and that with the middleware
  file gone the entrypoints answer 404.
- `deploy/test/integration_proxy.sh` runs a real Outhaul and Traefik behind a
  stand-in CDN and a stand-in OIDC provider. It checks first-boot setup,
  password and SSO sign-in, a protected app (the redirect, the code, the
  `__Host-` cookies, `X-Outhaul-User` reaching the app, an open path under it),
  a signed webhook delivery, requests straight to Traefik with the wrong or no
  header, the admin port's two listen addresses, and a restart with another
  header value (old value refused at once, apps redeployed at startup).

None of them can show a real CDN, a real DNS provider, a real identity
provider, or a real browser. Those are what "Checking a real host" is for.
