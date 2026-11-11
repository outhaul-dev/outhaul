# Container guard rails

Outhaul can refuse to deploy a Compose file that reaches past its own app. This
is the strict container policy. It is off unless you set
`OUTHAUL_CONTAINER_POLICY=strict` in `/etc/outhaul.env` and restart Outhaul.

Strict mode is for a server that trusted people share. A role decides what
someone may do in Outhaul, not what their code may do on the server: everyone
who can deploy runs a container on the same host and the same Docker network.
Strict mode contains mistakes and casually chosen third-party images. It is not
containment: it doesn't stop someone who means to get out, and it doesn't
separate one app from another on the network. See
[ARCHITECTURE.md](../ARCHITECTURE.md#container-guard-rails-opt-in).

Turning strict mode on checks nothing that already runs. A running app keeps
running until its next deploy, and the next deploy is when the rules apply.

## What a Compose file may not contain

Strict mode works from an allow-list. A service key that isn't on it is
refused, including a key a newer Compose adds. The table covers the ones people
hit most; every refusal says where it is and what to change.

| The file asks for | What to write instead |
| --- | --- |
| `ports:` | Give the service a domain on the app's page. Services in one stack reach each other by service name on the stack's network; a host port isn't needed for that. |
| A host path in `volumes:` (a bind mount, such as `/srv/data:/data` or `./data:/data`) | A named volume. Declare it under the top-level `volumes:` and mount it by name, `data:/data`. A `tmpfs` volume is also allowed. |
| `privileged: true` | Nothing. There is no substitute; tell the Outhaul admin what the app needs. |
| `cap_add:` | Nothing. You may use `cap_drop:` if the app needs to drop capabilities. |
| `devices:` | Nothing. Host devices are not available to a service. |
| `network_mode: host` | Remove it. Give the service a domain if it must be reached from outside, and reach the other services by name on the stack's network. `network_mode: none` and `network_mode: service:<name>` are allowed. |
| A secret or config defined with `file:` | Define it with `environment:` (a secret's value) or `content:` (a config's value). |

These are refused in the same way:

- `pid: host`, `ipc: host`, `uts: host`, and a `cgroup:` other than `private`.
- `container_name:`. Outhaul names the containers.
- `external_links:` and `volumes_from: container:...`. Use a service of the
  same stack.
- A fixed `ipv4_address`, `ipv6_address` or `mac_address`. Use a service name.
- `security_opt:` other than `no-new-privileges`.
- An `env_file:` or `label_file:` outside the checkout, and a `build.context:`
  or `build.dockerfile:` outside it, or an `oci-layout://` context.
- `build.privileged:`, `build.ssh:`, `build.cache_from:` and `build.cache_to:`.
- `driver_opts:` on a volume (it can turn a named volume into a host path), an
  `external:` volume, and a volume or network whose `name:` isn't the default
  one.
- A network driver other than `bridge`, and an `ipam` with a fixed subnet.
- GPU or generic resource reservations in `deploy:`.
- An image or a build tag under `outhaul/` for a service that builds, because
  that is where Outhaul keeps the images it rolls back to.
- `models:`, `gpus:`, `runtime:`, `provider:`, `pre_start:`, and a
  `post_start` or `pre_stop` hook that runs privileged.

The allow-list is in `internal/compose/policy.go`. If you need a key that
isn't on it and you think it is safe, ask the maintainer before working around
it.

## Extension keys and anchors

Top-level `x-*` keys are allowed, so you can keep shared fragments and anchors
there. Compose resolves an anchor before Outhaul reads the file, so anything an
anchor puts into a service is checked as if you wrote it in that service. An
`x-*` key written inside a service is dropped by Compose before Outhaul sees
the file, so keep shared fragments at the top level.

## Labels

Strict mode refuses a service label whose key starts with `traefik.` or
`outhaul.`, in any case, including one a `label_file:` adds. Outhaul writes the
routing labels from the app's Domains page. A `traefik.*` label would add a
route Outhaul did not write, and could take a host or path that belongs to
another app; an `outhaul.*` label is reserved for Outhaul. Remove the label and
use the app's page.

An image carries labels too, and Docker copies them onto every container made
from it, where Traefik reads them. So strict mode checks images as well:

- For a Compose stack, every service's image is checked after the pull, and a
  stack with an image that carries `traefik.*` labels is not started. The
  message names the service and the image. A service that sets `pull_policy:`
  can get a different image after the check, by a re-pull or a rebuild, so
  leave it unset if you rely on this check.
- For a Nixpacks or Dockerfile app, the image Outhaul built is checked before
  it is deployed. A fork preview's image is checked this way even without
  strict mode, because the fork's author chose it.

## Where strict mode applies

Strict mode applies to:

- **Compose deploys.** The file is checked before anything is built, and each
  service's image is checked after the pull.
- **Database ports.** The database forms refuse an external port, and a
  database provisioned under strict mode publishes none. A database that
  already had a port keeps it until its container is next replaced; Outhaul
  names it in a warning at startup.
- **Single-container image checks.** A Nixpacks or Dockerfile app's image is
  checked for `traefik.*` labels before it is deployed.

It does not apply to Traefik or cloudflared, which Outhaul runs itself.

It does not apply to builds. A Dockerfile or Nixpacks config for a
single-container app isn't read; strict mode checks the image that comes out,
not how it was made. A Compose file's `build:` section is checked, but a build
can still do things the container it produces cannot.

## The environment Compose sees

While strict mode is on, `docker compose` runs with only the variables it
needs: `PATH`, `HOME`, the locale and temporary-directory settings, the TLS
certificate settings, the Docker, BuildKit, Buildx and Compose settings, and
the proxy settings. A Compose file's `${NAME}` therefore can't read Outhaul's
own `OUTHAUL_*` settings or the secrets in its environment. The stack's own
`.env`, which Outhaul writes beside the Compose file on each deploy, still
supplies the app's variables.

## Docker Compose 2.35 or newer

Strict mode reads a Compose file by asking `docker compose` for its normalised
JSON, with `--no-env-resolution`. That flag arrived in Docker Compose 2.35, and
it keeps the contents of an `env_file` reference from being expanded into the
model. On an older host a Compose deploy fails before it starts, with a message
that includes:

```
reading a Compose file needs Docker Compose 2.35 or newer: ...
```

Upgrade Docker Compose on the server. An app that isn't a Compose app doesn't
need it.

## If it doesn't work

- "This server's container policy refuses this Compose file
  (`OUTHAUL_CONTAINER_POLICY=strict`)": the deploy log lists every refused item
  as `service "name": option: reason`, and the deployment's reason is the first
  one plus a count of the rest. Change what the log names.
- "reading a Compose file needs Docker Compose 2.35 or newer": update Docker
  Compose on the server.
- "This server's container policy doesn't allow published ports. Leave the
  field empty.": clear the database's external port. It is refused, and nothing
  is stored.
- "refusing service … image … carries traefik labels": the image is not
  Outhaul's to label; use one without `traefik.*` labels. For a single-container
  app the message is "refusing an image that carries traefik labels", and the
  labels come from the Dockerfile or its base image.

## Testing so far

The real-Compose tests have been run on a development host with Docker Compose
v5.6.0, and pass: every fixture that Compose can load gives the expected
answer, and every one-click template passes the check. The checks below need
Docker and Compose on a staging host and have not been run yet.

Not yet run:

- [ ] A Compose app with `privileged: true`, with a host path, and with
      `ports:` is each refused, and the message names the service and the
      option.
- [ ] A Compose file with a `traefik.http.routers…` label is refused.
- [ ] A database can't be given an external port, and `ss -ltn` shows none.
- [ ] `docker compose version` on the host is 2.35 or newer.
- [ ] A Compose file that reads `${OUTHAUL_REQUIRE_HEADER_VALUE}` gets nothing
      when strict mode reduces the environment.

When they have been run, this list records the results.
