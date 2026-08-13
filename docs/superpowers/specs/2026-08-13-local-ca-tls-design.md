# Local CA TLS for local-only installs — design

**Date:** 2026-08-13
**Status:** Approved (pending spec review)

## Overview

Local-only Outhaul installs currently run plain HTTP: `TLSEnabled()` is literally
"`OUTHAUL_ACME_EMAIL` is set", and all TLS is delegated to Traefik's ACME client
using the HTTP-01 challenge, which requires the host to be reachable from the
internet. This feature adds an mkcert-style **local certificate authority** built
into the Outhaul binary. Outhaul generates a CA once, mints per-domain leaf
certificates automatically, and serves them through Traefik's file provider. A
user installs the CA root on their devices once; after that, every
Outhaul-served name on the LAN (e.g. `app.local`, `https://192.168.x.x`) shows
as fully valid, with no human in the loop for issuance or rotation.

### Goals

- Valid (locally trusted) HTTPS for local-only installs with zero recurring effort.
- No new external dependencies: `crypto/x509` only, no extra containers.
- Certs hot-reload via Traefik's file provider; no proxy restarts on domain changes.
- Opt-in: existing installs change behavior only when `OUTHAUL_LOCAL_CA=true` is set.

### Non-goals (deferred)

- Publicly valid certs for internal hosts via Let's Encrypt **DNS-01** (manual TXT
  or provider APIs). Brainstormed and deliberately deferred; nothing in this
  design may preclude adding it later (acme-dns / CNAME delegation is the likely
  automation path).
- Wildcard certificates. Leafs are minted per domain automatically, so wildcards
  add nothing in local-CA mode.

## Approach chosen

Native Go CA inside Outhaul (option A). Rejected alternatives: bundling a
step-ca container (a second managed container and provisioner state for no
user-visible benefit at this scale) and Traefik's built-in default cert
(regenerates on restart, never trustable).

## Configuration

- New env var **`OUTHAUL_LOCAL_CA`** (bool, default `false`).
- `internal/config/config.go`:
  - New `LocalCAEnabled()`.
  - Current `TLSEnabled()` semantics split: `ACMEEnabled()` = `ACMEEmail != ""`;
    `TLSEnabled()` = `ACMEEnabled() || LocalCAEnabled()`. All existing call
    sites are audited: ACME-specific behavior (resolver flags, acme dir) gates
    on `ACMEEnabled()`; entrypoint/redirect/route-TLS behavior gates on
    `TLSEnabled()`.
  - New `CADir()` = `$DataDir/ca`, `CertsDir()` = `$DataDir/traefik/certs`.
- **Startup errors** (fail fast, clear message):
  - `OUTHAUL_LOCAL_CA=true` together with `OUTHAUL_ACME_EMAIL` set.
  - `OUTHAUL_LOCAL_CA=true` together with tunnel mode.
- `OUTHAUL_HTTPS_PORT` applies unchanged.

## New package: `internal/localca`

### CA creation (idempotent)

- On startup when enabled: load `$DataDir/ca/rootCA.pem` + `rootCA.key`; create
  if absent. Dir 0700, key 0600.
- ECDSA P-256. Validity 10 years. Subject CN: `Outhaul Local CA (<hostname>)`.
  `IsCA: true`, `KeyUsage: CertSign | CRLSign`, path length 0.
- If the key or cert exists but is unreadable/corrupt/mismatched: **refuse to
  start** with a pointed error. Never silently regenerate a CA that devices may
  already trust.

### Leaf certificates

- Minted for the admin host and every domain row whose per-domain `TLS` flag is
  true (same gate as ACME mode; the flag's stored-intent behavior in
  `internal/server/handlers.go` is preserved).
- If `OUTHAUL_PUBLIC_URL` is unset (common on local installs), there is no
  admin host: mint a **fallback cert** for the machine's hostname with the
  private-IPv4 SANs, and use it wherever this spec says "admin-host cert"
  (including as the Traefik default certificate).
- ECDSA P-256, validity **825 days** (Apple's trust ceiling, mkcert's choice),
  SANs: the DNS name; for the **admin host cert additionally** all non-loopback
  private IPv4 addresses of the host at mint time, so `https://192.168.x.x`
  is valid too.
- Files: `$DataDir/traefik/certs/<domain>.pem` / `<domain>.key` (key 0600).
- **Rotation:** the reconcile loop re-mints any leaf with < 30 days remaining,
  or whose SAN set no longer matches (domain edits, changed host IPs). No human
  in the loop.
- A failed mint logs and skips that domain — the route still serves (default
  cert) and deploys are never blocked by cert errors.

## Traefik integration

- `internal/traefik/proxy.go` `proxySpec`, when `LocalCAEnabled()`:
  - `websecure` entrypoint on `HTTPSPort` and web→websecure HTTPS redirect, as
    in ACME mode.
  - **No** ACME/certresolver flags.
  - New read-only mount: `$DataDir/traefik/certs` → `/etc/traefik/certs`.
  - `LocalCA` participates in `hashConfig` (and the new mount is part of the
    fingerprint) so toggling recreates the proxy container.
- New file-provider config `$DynamicDir/outhaul-local-certs.yml`, rewritten by
  Outhaul whenever leafs change:
  - `tls.certificates`: one entry per leaf (container paths).
  - `tls.stores.default.defaultCertificate`: the admin-host cert, so Traefik
    never serves its built-in generated cert.
  - Traefik selects per-route certs by SNI; the file provider hot-reloads.
- `internal/traefik/labels.go`: in local-CA mode the `<router>-tls` router sets
  `tls: true` with **no** `certresolver`.
- `writeAdminDynamicConfig`: admin router uses `tls: {}` (no `certResolver`)
  when in local-CA mode.

## CA root distribution

- CLI: `outhaul ca root` prints the CA certificate PEM to stdout
  (pipe-friendly); `outhaul ca root --path` prints the file path instead.
- Admin UI: a "Download CA certificate" link on the settings page serving
  `rootCA.pem` (`application/x-pem-file`). The root cert is public material.
- Docs: short guide for installing the root on Linux / macOS / Windows / iOS /
  Android, plus the caveat that devices see warnings until they trust the root
  (the HTTP→HTTPS redirect is on regardless, matching ACME mode).

## Installer (`deploy/bootstrap.sh`)

- Ingress mode (c) "local-only" gains a prompt: `Enable HTTPS with a local
  certificate authority? [Y/n]` → writes `OUTHAUL_LOCAL_CA=true` to
  `/etc/outhaul.env` (existing files remain untouched on re-run, as today).
- `derive_firewall_ports()`: mode c with local CA opens 80 and 443.
- Post-install summary prints how to fetch the CA root (`outhaul ca root`, or
  the admin UI link) and points at the trust-installation doc.

## Error handling summary

| Condition | Behavior |
|---|---|
| LocalCA + ACME email both set | startup error |
| LocalCA + tunnel mode | startup error |
| CA files corrupt/unreadable/mismatched | startup error; never regenerate |
| Leaf mint failure | log + skip domain; route serves default cert |
| Dynamic YAML write failure | log + retry next reconcile |

## Testing

- `internal/localca` unit tests: idempotent CA creation; corrupt-key refusal;
  leaf SAN correctness (DNS + admin IP SANs); 825-day validity; rotation
  trigger at <30 days and on SAN drift.
- `internal/traefik` table tests: `proxySpec` flags/mounts with LocalCA on/off;
  `hashConfig` changes on toggle; `RouteLabels` without certresolver;
  `outhaul-local-certs.yml` rendering including default certificate.
- Config tests: `TLSEnabled`/`ACMEEnabled`/`LocalCAEnabled` matrix; startup
  error conditions.
- `deploy/test/test_local_ca.sh` following the existing shell-test pattern:
  prompt handling, env var written (and not leaked in other modes), firewall
  ports for mode c ± local CA.

## Documentation updates

README config table (`OUTHAUL_LOCAL_CA`), ARCHITECTURE.md ingress-posture
section, `docs/MANUAL-DEPLOY.md` env block, new trust-installation guide.

## Future work

- Manual DNS-01 issuance (CLI first, admin UI later; wildcard + per-domain).
- Automated DNS-01 via provider APIs or acme-dns CNAME delegation.
- Renewal visibility surface (UI banner + `outhaul cert list`) shared between
  local-CA and future ACME certs.
