# Serving behind a TLS-terminating proxy

This page is for an install whose visitors reach it only through a CDN or other
TLS-terminating proxy on port 443. The proxy terminates the visitor's TLS and
connects to the host on 443 only. Each part is a setting of its own, off unless
set, so an ordinary install needs none of them.

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

## Images and Compose files with their own Traefik labels

Docker copies an image's labels onto its containers, and Traefik reads container
labels. While a request header is required, an image that carries any
`traefik.*` label is refused for every single-container app, not only for fork
previews: its router would be Outhaul's not to write, and it would lack the
header term. The deploy fails and names the labels.

A Compose file can write `traefik.*` labels on its own services. On a service
Outhaul doesn't route, Traefik's constraint ignores them. On a service Outhaul
does route, they sit beside Outhaul's own labels and are not checked yet.

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
