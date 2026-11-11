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
