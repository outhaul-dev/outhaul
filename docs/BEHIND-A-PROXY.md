# Serving behind a TLS-terminating proxy

This page is for an install whose visitors reach it only through a CDN or other
TLS-terminating proxy on port 443. The proxy terminates the visitor's TLS and
connects to the host on 443 only. Each part is a setting of its own, off unless
set, so an ordinary install needs none of them.

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
