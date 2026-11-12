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

Two smaller rules also run while a request header is required
(`OUTHAUL_REQUIRE_HEADER_NAME`), even with strict mode off: a Compose file
can't add its own `traefik.*` or `outhaul.*` labels, and the Compose
environment is reduced so a file can't read the header's value. See
[BEHIND-A-PROXY.md](BEHIND-A-PROXY.md#the-required-header).

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
- An `env_file:` or `label_file:` outside the checkout, including one reachable
  through a symlink inside the checkout that resolves outside it, and a
  `build.context:` or `build.dockerfile:` outside it, or an `oci-layout://`
  context.
- `build.privileged:`, `build.ssh:`, `build.cache_from:`, `build.cache_to:`,
  `build.entitlements:` and `build.isolation:`.
- `use_api_socket:`. A service does not get the Docker socket.
- A build secret defined with `file:`; a build secret must use
  `environment:`.
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

Outhaul refuses a service label whose key starts with `traefik.` or `outhaul.`,
in any case, including one a `label_file:` adds. The rule is on under the strict
container policy, and while a request header is required
(`OUTHAUL_REQUIRE_HEADER_NAME`): a router written by the file would sit beside
Outhaul's own, without the header. Outhaul writes the routing labels from the
app's Domains page. A `traefik.*` label would add a route Outhaul did not write,
and could take a host or path that belongs to another app; an `outhaul.*` label
is reserved for Outhaul. Remove the label and use the app's page.

An image carries labels too, and Docker copies them onto every container made
from it, where Traefik reads them. So Outhaul checks images as well, in both
cases:

- For a Compose stack, every service's image is checked after the pull, and a
  stack with an image that carries `traefik.*` labels is not started. The
  message names the service and the image. A service that sets `pull_policy:`
  can get a different image after the check, by a re-pull or a rebuild, so
  leave it unset if you rely on this check.
- For a Nixpacks or Dockerfile app, the image Outhaul built is checked before
  it is deployed. A fork preview's image is checked this way even when neither
  setting is on, because the fork's author chose it.

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

While strict mode is on, or a request header is required even without strict
mode, `docker compose` runs with only the variables it needs: `PATH`, `HOME`,
`USER`, `LOGNAME`, the locale and temporary-directory settings, the TLS
certificate settings, the Docker, BuildKit, Buildx and Compose settings, and
the proxy settings. A Compose file's `${NAME}` therefore can't read Outhaul's
own `OUTHAUL_*` settings or the secrets in its environment, including the
required header's value. The stack's own `.env`, which Outhaul writes beside
the Compose file on each deploy, still supplies the app's variables.

## Docker Compose 2.35 or newer

Strict mode, or a required request header even without strict mode, reads a
Compose file by asking `docker compose` for its normalised JSON, with
`--no-env-resolution`. That flag arrived in Docker Compose 2.35, and it keeps
the contents of an `env_file` reference from being expanded into the model. On
an older host a Compose deploy fails before it starts, with a message that
includes:

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
- "This server refuses a Compose file that adds its own routing while a request
  header is required": the deploy log lists every refused label; remove the
  `traefik.*` or `outhaul.*` label, and use the app's Domains page.
- "reading a Compose file needs Docker Compose 2.35 or newer": update Docker
  Compose on the server.
- "This server's container policy doesn't allow published ports. Leave the
  field empty.": clear the database's external port. It is refused, and nothing
  is stored.
- "refusing service … image … carries traefik labels": the image is not
  Outhaul's to label; use one without `traefik.*` labels. For a single-container
  app the message is "refusing an image that carries traefik labels", and the
  labels come from the Dockerfile or its base image.

## Limits (opt-in)

Limits are separate from strict mode: each setting works alone, and an install
that sets none creates exactly the containers and override it did before. The
ceiling is per container, not per stack, and Outhaul does not change a running
container when a limit changes.

| Variable | Values | Default |
|---|---|---|
| `OUTHAUL_DEFAULT_CPUS` | `1`, `0.5`, `1.5`; at least `0.01`, at most the server's CPUs | unset (no CPU limit) |
| `OUTHAUL_DEFAULT_MEMORY` | `512m`, `1g`, `1.5g`, or bytes; at least `6m` | unset (no memory limit) |
| `OUTHAUL_DEFAULT_PIDS` | a whole number, 1 or more | unset (no process limit) |
| `OUTHAUL_MAX_CONCURRENT_BUILDS` | a whole number, 1 to 4 | unset (up to four) |

A CPU value above this host's CPU count, with no `OUTHAUL_DOCKER_HOST`, stops
`serve` from starting: Docker would refuse every container that asked for it.

### Which container gets which limit

| Container | Limit |
|---|---|
| A Nixpacks or Dockerfile app, and its `outhaul-deploy-<id>` health-check container | the app's |
| A preview | its parent's, read at each deploy |
| A Compose service | the app's, for each resource the service does not set itself |
| A managed database, a preview's included | the server default |
| A backup, restore or data-removal helper | the server default |
| Traefik, cloudflared | none |
| A build step | none; `OUTHAUL_MAX_CONCURRENT_BUILDS` bounds how many run |

There is no swap limit, no limit for one database alone, and no way to give one
app "no limit" while a server default is set. A large value does that.

### One app's limit

A system admin can give one app its own values from the **Limits** panel on its
page. The panel appears only when at least one `OUTHAUL_DEFAULT_*` is set, and
then everyone who can see the app sees the three values in force and whether
each is the server default or set for this app. Only a system admin gets the
form. An empty field means the server default. A preview shows its parent's
values and has no form: a preview uses its parent's limits.

A value may be above or below the server default. A change applies when the
app's container is next created — its next deploy. Changing a server default
redeploys nothing; a database takes the new default when its container is next
replaced.

### Compose services

A Compose service keeps any resource it sets itself: `cpus`, `mem_limit` and
`pids_limit`, or the same under `deploy.resources.limits`. Outhaul adds only
the missing ones, through its override:

- a service with no `deploy.resources.limits` block gets `cpus`, `mem_limit`
  (bytes) and `pids_limit` at the service level;
- a service with one gets the missing keys inside it, with `memory` as a quoted
  string of bytes — Compose refuses a service whose own key and `deploy` limit
  disagree.

A service that reserves more memory than the limit Outhaul would add fails the
deploy in `building`, naming both figures, rather than at `up` with Docker's
less clear message.

In strict mode a service may also not ask for more than the app's own limit:
that check is described above under the strict policy.

### The build cap

`OUTHAUL_MAX_CONCURRENT_BUILDS` limits how many deployments **that have to
build** are in `building` at once. A deployment has to build when it arrives
with no image: new code, and every Compose deploy. A rollback, or a redeploy
that carries an image, never waits for a build slot.

While every slot is taken, the queue offers only a deployment that already has
an image, and it never jumps an older queued build of its own app. A waiting
deployment stays `queued`, keeps its place, can be cancelled as usual, and
survives a restart that way. With the cap set, its page says "Waiting its turn.
This server runs N builds at a time."

### Live figures

The app page's Metrics panel names the limit beside live CPU and memory, and
adds a Processes figure ("12" and "of 512") when the container has a process
limit. For a stack the value appears when every running container has the same
one; "limits differ" appears when they don't. The Metrics page shows the same
beside each container. These are the live containers' limits, so a value set
but not yet deployed differs from the Limits panel. A container with no limit
shows exactly what it showed before.

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
      when strict mode or the required header reduces the environment.

When they have been run, this list records the results.

### Not yet run: the limits

These need a staging host and have not been run yet. When they have been, this
list records the results.

- [ ] `docker inspect` on a Nixpacks app, a Compose service, a database and a
      preview shows `NanoCpus`, `Memory` and `PidsLimit`.
- [ ] A container that allocates past its memory limit is killed with 137, and
      the other apps keep answering.
- [ ] Each of the nine templates deploys in strict mode with the default limits
      and stays up for a day. So does each database engine.
- [ ] With a cap of 1, two apps deployed together build one after the other, and
      a rollback of a third starts at once.
- [ ] `docker inspect` on a service that sets its own limit shows that value,
      not the server default.
- [ ] A service that reserves more memory than its limit fails before `up`,
      naming both figures.
