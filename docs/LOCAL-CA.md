# HTTPS on a local network — the built-in CA

Local-only installs can't get Let's Encrypt certificates (the challenge
requires a host reachable from the internet). Instead, Outhaul ships an
mkcert-style certificate authority: set `OUTHAUL_LOCAL_CA=true` (the
installer's local-only mode offers this) and Outhaul creates a CA on first
boot, then automatically mints and rotates a certificate for every app
domain with TLS enabled. Nothing to renew by hand — you install the CA root
on each of your devices once. Apps deployed before the CA was enabled need a
redeploy to pick up their HTTPS routers.

## Install the root certificate

Get the root, either way:

- from the server: `sudo outhaul ca root > outhaul-ca.pem` (the CA dir is
  `0700` and owned by the service user, so this needs root)
- from any browser: `http://<server>:8080/ca.pem` (also linked from
  Settings)

Then trust it:

- **Debian/Ubuntu**: `sudo cp outhaul-ca.pem /usr/local/share/ca-certificates/outhaul-ca.crt && sudo update-ca-certificates`
- **Fedora/Arch**: `sudo trust anchor outhaul-ca.pem`
- **macOS**: open Keychain Access → System → import the file → double-click
  it → Trust → "Always Trust". Or:
  `sudo security add-trusted-cert -d -k /Library/Keychains/System.keychain outhaul-ca.pem`
- **Windows**: double-click the file → Install Certificate → Local Machine →
  "Trusted Root Certification Authorities".
- **iOS**: AirDrop/mail the file, install the profile under Settings →
  General → VPN & Device Management, then enable full trust under Settings →
  General → About → Certificate Trust Settings.
- **Android**: Settings → Security → More → Encryption & credentials →
  Install a certificate → CA certificate (exact path varies by vendor).
- **Firefox** (uses its own store): Settings → Privacy & Security →
  Certificates → View Certificates → Authorities → Import.

Until a device trusts the root it will show certificate warnings — Outhaul
redirects HTTP to HTTPS whenever TLS is on, same as in Let's Encrypt mode.

## Details

- Root: `$DataDir/ca/rootCA.pem` (public) + `rootCA.key` (0600, never leaves
  the server). ECDSA P-256, 10-year validity.
- Leafs: 825-day validity (Apple's trust ceiling), re-minted automatically
  30 days before expiry and whenever a domain's SANs change.
- The admin-host certificate also carries the server's private IPv4
  addresses, so `https://192.168.x.x` is valid too.
- Back up `$DataDir/ca/` with the rest of the data dir: a lost CA means
  re-trusting a new root on every device. The CA is never silently
  regenerated — if its files are corrupt, Outhaul refuses to start and says
  so.
- `OUTHAUL_LOCAL_CA` is mutually exclusive with `OUTHAUL_ACME_EMAIL` and
  with a Cloudflare Tunnel.
