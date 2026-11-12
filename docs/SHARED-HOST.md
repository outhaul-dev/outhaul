# A shared host

These settings suit a host that runs many people's apps. They are off unless
set, so an ordinary install generates `sslip.io` names and changes nothing.

## Default domains under one suffix

On a host whose visitors arrive by name through one wildcard, only a name one
label under that wildcard is routed and covered by a certificate. A
`sslip.io` name resolves to the server's IP, so the request does reach the
machine, but no router matches it and no certificate covers it. The domain
dialog's free-domain option generated exactly those names.

Set the suffix in `/etc/outhaul.env` and restart Outhaul:

```sh
OUTHAUL_DOMAIN_SUFFIX=shared.demo.example.com
```

Every name Outhaul generates is then one label under it:

| Where | Name |
|---|---|
| a template's `${domain}` | `<name>.<suffix>` |
| a template's `${domain:<label>}`, its second name | `<name>-<label>.<suffix>` |
| a template's `${email}` | `<random>@<name>.<suffix>` |
| a preview with no base domain | `[<service>-]<app>-pr-<n>.<suffix>` |
| the first `git push` for a new app | `<app>.<suffix>` |
| the new-app form, pre-filled as the name is typed, or left blank (a single-container app) | `<app>.<suffix>` |
| the domain dialog's "Default domain" | `<app>.<suffix>` |

A template's `${domain}` is now the same name every time it appears, where a
random `sslip.io` name changed on each render. A manifest that needs two names
writes `${domain:<label>}`, as the MinIO template's API name does with
`${domain:api}`; without a suffix it gives a zero-DNS `sslip.io` name, as
`${domain}` does (a different random one each time).

An app name is 2 to 40 characters of `a-z`, `0-9` and `-`, so it is always a
valid label, but `<app>-<label>` has to fit in one label of 63 characters:
choose short labels. For the same reason `OUTHAUL_DOMAIN_SUFFIX` is capped at
189 characters, which leaves room for a full 63-character label under a
253-character name.

The domain dialog's first option is "Default domain", which gives
`<app>.<suffix>`, and "Free sslip.io domain" is no longer offered. The server
still accepts it from a page rendered before the suffix was set, so a form left
open in a browser doesn't fail (the suffix-only rule may still refuse the host
it names).

A preview base domain that an app sets still wins over the suffix. Left empty,
it falls back to the suffix, so a preview is one label under it too.

### The suffix-only rule

`OUTHAUL_DOMAIN_SUFFIX_ONLY=yes` refuses any host that isn't exactly one label
under the suffix. Outhaul checks it in the transaction that writes a domain
row, so every writer is refused: the new-app form, the template deploy, a
domain row's dialog and a first `git push`. A system admin is refused too,
because the name would be routed nowhere, and the refusal says so: "Only names
one label under `<suffix>` reach this server, such as `demo.<suffix>`.
`<host>` wouldn't be routed."

Rows that already exist are left in place: nothing renames or deletes them.
Such a row can still be edited in place (to turn sign-in on, say) and
deleted, and startup writes one warning for each, naming the app, the host and
the suffix.

A preview base domain other than the suffix is refused when it is saved. An
empty one is left alone, since it already falls back to the suffix. A base
domain saved before this rule is what fails when its preview is created, and
the comment on the pull request says why.

### Certificates and the suffix

Set `OUTHAUL_TLS_WILDCARD=*.<suffix>` with it, so one certificate covers every
generated name (see [BEHIND-A-PROXY.md](BEHIND-A-PROXY.md)). Outhaul refuses to
start when the two name different domains: the error names both, because every
generated name would otherwise be covered by no certificate.

### What only the real host can show

One check needs the real host and the CDN in front of it. It is item 5 of the
[design](superpowers/specs/2026-11-11-shared-host-defaults-design.md), so it
keeps its number here:

```text
5. A new app is offered <app>.<suffix>, a template lands on <name>.<suffix> and a
   preview on <app>-pr-<n>.<suffix>, all served through the CDN with no new
   certificate. a.b.<suffix> is refused.
```
