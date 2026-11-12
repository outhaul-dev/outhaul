# Defaults for a shared host

A fixed Traefik address, Compose services that come back after a reboot,
default domains under one suffix, and sign-in required on new domains.

This covers requirements R8, R10, R9 and R11 of the brief "outhaul
enhancements: running behind Azure Front Door on a shared host" (10 November
2026), in the brief's order of work. The four are independent. Each has its
own settings and its own series of commits, and can be merged and used without
the others. Code references are to `06150cc`. The design for R1 to R4
(`2026-11-11-proxy-ingress-design.md`) is approved but not built. Where this
design meets it, the section says so.

## Why

The shared host runs many people's demo apps behind a CDN. Four of Outhaul's
defaults don't fit it.

- **R8.** Traefik gets its certificate by DNS-01 with the host's own cloud
  identity. Anything on the host can get a token for that identity from the
  metadata service at `169.254.169.254`, and the token can rewrite DNS. Only
  Traefik needs it. The host's firewall will let forwarded traffic to that
  address through from Traefik's address only, so that address must be fixed
  and known. Today Docker picks the network's subnet and every container's
  address (`EnsureNetwork` and `CreateContainer`, `internal/docker/real.go`).
- **R10.** The host may reboot any night. Traefik, single-container apps and
  databases are created with `unless-stopped` and come back. A Compose service
  comes back only if its own file says so: the override
  (`internal/compose/override.go`) adds networks and labels, no restart policy.
- **R9.** Only names one label under the host's wildcard reach the host.
  Templates, the "free domain" option, a first `git push` and previews all
  generate `sslip.io` names, which point at the server's IP and aren't routed
  there. Any other name can be typed, and nothing says it won't work.
- **R11.** The apps are for staff and invited guests. "Require sign-in" is a
  switch on each domain row that starts off, so forgetting it makes an app
  public.

## Goals

1. One setting gives Outhaul's Docker network a fixed subnet. Another gives
   Traefik a fixed address on it, kept across recreates, updates and reboots,
   which Docker never hands to another container. A network that exists with
   another subnet stops startup.
2. With a setting, every Compose service that states no restart policy gets
   `unless-stopped`. A policy the file states is never changed.
3. With a setting, generated names are `<name>.<suffix>`: new apps, templates,
   first pushes and previews.
4. With a second setting, a domain that isn't exactly one label under the
   suffix is refused, whoever writes it, with a message that says why.
5. With a setting, a new domain row starts with sign-in required, from every
   path that creates one. Opening a host or a path stays a deliberate act, and
   the docs say how.
6. Each setting is off unless set. With none set nothing changes: the same
   Traefik container, the same override file, the same names, the same pages.

## Non-goals

- The firewall rule. The infrastructure writes it. This design gives it a
  stable address and says which containers are on which network.
- Stopping a container that may choose or forge its address, or use the host's
  network. What a Compose file may ask for is R6.
- Moving an existing install onto a fixed subnet automatically. The docs give
  the steps. IPv6 on the network. An `outhaul info` command.
- A policy other than `unless-stopped`. Redeploying existing stacks to give
  them the policy. Starting apps at boot from Outhaul.
- More than one suffix, or a suffix per project. Renaming existing domains.
- A pattern or regex that opens the same path on many apps. The two existing
  ways cover the need for now: a row with sign-in off opens a whole host, and
  a path row with sign-in off under a protected host opens that path.
- A setting that lets only a system admin open a whole host (call 22).
- Protecting rows that exist when `OUTHAUL_PROTECT_NEW_DOMAINS` is turned on.

## Anti-goals

- An install that sets none of these sees a change: Traefik recreated at the
  upgrade, a different override file or generated name, a new form field.
- Outhaul runs on a network with another subnet, or Traefik at an address
  Docker chose, and nothing says so. Or after a reboot another container
  holds Traefik's address.
- A restart policy the Compose file states is changed. Or a service that
  another waits on to finish is restarted, and the deploy never completes.
- A name Outhaul generates is refused by the suffix rule. Or two apps want one
  name and one of them is left half-created.
- With `OUTHAUL_PROTECT_NEW_DOMAINS` on, a new row is left open because nobody
  chose anything. When sign-in can't work, no row is made, not an open one.
- Opening a host or a path on purpose needs a new mechanism.

## Constraints

From `ARCHITECTURE.md`, `AGENTS.md` and the brief:

- Settings are `OUTHAUL_*` environment variables read through the `parser` in
  `internal/config/config.go`. `Validate` reports bad values and bad
  combinations, and `outhaul serve` refuses to start. No config files.
- Traefik stays on a bridge network. Host networking would break the
  `host.docker.internal` gate address and port publishing.
- "The `unless-stopped` restart policy handles reboot", and there is no
  desired-state column (`ARCHITECTURE.md`). Docker brings containers back.
- The override is rendered by hand, and `go.mod` has no YAML dependency.
- Protected routes keep every "Decisions to keep" entry: the gate lives in
  labels, a change takes effect by deploying, previews only gain protection,
  and only a system admin brings two projects together on a protected host.
- Each requirement builds on `06150cc` or on R1 to R4, in either order.

## Settings

| Variable | For | Values | Default | Needs |
|---|---|---|---|---|
| `OUTHAUL_NETWORK_SUBNET` | R8 | an IPv4 network in CIDR form, `/28` or larger, such as `172.30.0.0/24` | unset: Docker picks | nothing |
| `OUTHAUL_TRAEFIK_IP` | R8 | an IPv4 address in the lower half of the subnet, after its first two addresses | unset: Docker picks | `OUTHAUL_NETWORK_SUBNET` |
| `OUTHAUL_COMPOSE_RESTART` | R10 | `unless-stopped` | unset: only what the Compose file says | nothing |
| `OUTHAUL_DOMAIN_SUFFIX` | R9 | a domain, such as `shared.demo.example.com` | unset: `sslip.io` names | if `OUTHAUL_TLS_WILDCARD` (R1) is set, it must be `*.` and this suffix |
| `OUTHAUL_DOMAIN_SUFFIX_ONLY` | R9 | yes or no | no | `OUTHAUL_DOMAIN_SUFFIX` |
| `OUTHAUL_PROTECT_NEW_DOMAINS` | R11 | yes or no | no | `OUTHAUL_PUBLIC_URL`, and HTTPS or a tunnel |

The brief named four of these. `OUTHAUL_COMPOSE_RESTART` and
`OUTHAUL_DOMAIN_SUFFIX_ONLY` are new (calls 8 and 14).

`Validate` names the variable and what it wants, as it does today:

- The subnet needs a new `parser` helper. It refuses IPv6, a prefix longer
  than `/28`, and a value with host bits set (`172.30.0.5/24`).
- The Traefik address is refused without the subnet, outside it, in its upper
  half, or equal to its network address or its first address (the gateway).
  The message gives the range it may be in: `172.30.0.2` to `172.30.0.127`.
- The suffix is a bare host name, checked as a domain row's host is, and
  short enough that a 63-character label fits in 253. The wildcard check
  exists once both it and R1 are built.
- The tunnel is a stored setting, so the last "needs" is enforced where the
  local CA's tunnel rule is: `serve` refuses to start with the setting on and
  neither HTTPS nor a tunnel, and `infraController.Disable` (`main.go`)
  refuses to turn the tunnel off when that would leave neither.

The first shared host sets all six, beside the R1 to R4 settings: for example
`172.30.0.0/24`, `172.30.0.2`, `shared.demo.example.com`, and the rest on.

## R8: a fixed subnet and Traefik address

### The network

Today `EnsureProxy` (`internal/traefik/proxy.go`) and `EnsureConnector`
(`internal/tunnel/connector.go`) call `dc.EnsureNetwork(ctx, name)`. The real
client returns if a network of that name exists. Otherwise it creates a
bridge with one option, the host interface's name, which is the network's
name when that is 15 bytes or fewer (`bridgeOptions`). Nothing reads an
existing network's subnet.

`EnsureNetwork` takes a `docker.NetworkSpec` instead: name, subnet, gateway
and dynamic range. One pure function derives the last two from the subnet.

| | For `172.30.0.0/24` | Rule |
|---|---|---|
| Gateway | `172.30.0.1` | the first address, given to Docker explicitly |
| Fixed addresses | `172.30.0.2` to `172.30.0.127` | the rest of the lower half. Docker never hands these out |
| Dynamic range | `172.30.0.128/25` | the upper half, passed as the network's `IPRange` |

| Subnet | Network | `EnsureNetwork` does |
|---|---|---|
| unset | any | what it does today, with the same request |
| set | missing | creates it with the subnet, gateway and range |
| set | exists with the same three | nothing |
| set | exists, anything else | returns a `NetworkMismatchError` with what it has and what was asked |

Outhaul never removes or recreates a network: other containers are on it.

The range is what makes the address hold. Docker gives a new container the
lowest free address, and a stopped container gives its address up (see
[Checked](#checked-against-real-docker-and-compose-so-far)). Without a range,
a container started while Traefik is being replaced, or before it at boot,
can take `.2`, and Traefik then fails with `Address already in use`. An
address outside the range is never handed out. The range is set even when
only the subnet is, so adding `OUTHAUL_TRAEFIK_IP` later needs no new network.
The gateway is given explicitly because otherwise Docker took it from the
range. The cost is room: a `/24` holds 127 containers on the shared network,
one for each single-container app, database and published Compose service.

### Traefik's address

- `docker.ContainerSpec` gains `IPv4`, a fixed address on the spec's first
  network. `CreateContainer` builds an empty `EndpointSettings` per network
  today. It sets `IPAMConfig.IPv4Address` on that one.
- `traefik.ProxyConfig` gains `Subnet` and `IPv4`, and `proxySpec` sets the
  spec's address. `tunnel.ConnectorConfig` gains `Subnet` for its own
  `EnsureNetwork` call. `main.go` fills them from the settings
  (`infraController.proxyConfig`, and where the connector's config is built).
- `hashConfig` covers the address only when one is set. With none the hash is
  today's, so an upgrade recreates nothing. Setting or changing the address
  recreates Traefik once.
- When Traefik must be created and an address is set, `EnsureProxy` first
  reads who is on the network. If another container holds the address, it
  returns an error that names it, before it stops the old Traefik. Only a
  container given that address on purpose can hold it.

That needs a new client call, `InspectNetwork(ctx, name)`: the network's
subnet, range and gateway, and each attached container's address. R4's plan
adds `NetworkGateway(ctx, name)` to the same interface, to listen on the
gateway and map `host.docker.internal` to it. Whichever is built second is
built on the other. R4 doesn't change: it reads the subnet's first address.

### Starting up

Today `ensureInfra` (`main.go`) logs a warning and carries on when Docker
can't be reached or the reconcile fails, so the admin UI still starts. With a
subnet set, `serve` ensures the network first and returns an error in three
cases, so systemd shows the failure and tries again:

| Case | The message says |
|---|---|
| Docker can't be reached | that the network can't be checked, and why |
| the network differs | both sets of values, that Outhaul won't change a network containers are on, and where the steps to move are |
| Docker refuses to create it, for example because it overlaps another network | the variable, and Docker's own message |

With no subnet set, startup is as today. A mismatch found later, when
Settings reconciles the proxy, is returned as other reconcile errors are, and
Traefik isn't replaced. This narrows "the admin UI starts while Docker is
down", only for installs that set a subnet (call 4). On the shared host the
admin UI is reached through Traefik, so nothing is lost. R4's "listen on
loopback until Docker answers" isn't reached with a subnet set.

The two values are the configured ones, so the infrastructure can write the
firewall rule from the same source. What is derived is fixed by rule and in
the docs: the gateway, the range, and the host interface, named after the
network. `serve` logs them in one line. There is no `outhaul info` (call 6).

### What needs the metadata service

This is read from the code. Nothing in the repository names the address.

| What | Runs as | Network | Needs it |
|---|---|---|---|
| Traefik | container | shared, fixed address | yes, for DNS-01 with the host's identity |
| cloudflared | container | shared, dynamic | no: it dials out with its own token |
| Single-container apps, and each deploy's health-check container `outhaul-deploy-<id>` | container | shared, dynamic | no |
| Databases, `outhaul-db-<name>` and the `-new` replacement | container | shared, dynamic | no |
| Compose stacks | containers | the stack's own network, on a subnet Docker picks. Published services are also on the shared one | no |
| Previews | apps and databases | as above | no |
| Backup, restore and data-removal helpers | container | Docker's default bridge | no: they copy files |
| Image builds, Nixpacks or Dockerfile | build steps | Docker's default bridge (probed) | no: they need registries and package mirrors |
| Outhaul, `git clone`, the Nixpacks plan, blob-store uploads | host process | the host's | not forwarded traffic, so the rule doesn't see it. Outhaul doesn't ask today |

So the rule can drop everything forwarded to that address, on every bridge,
except from Traefik's address, and Outhaul keeps working. The rule matches a
source address. This design keeps that address stable and keeps Docker from
giving it to another container. It doesn't stop a container that may pick
its own. For R6: refuse a Compose file `network_mode: host`, `network_mode:
container:…`, an `ipv4_address` on the shared network, added capabilities
and `privileged`.

## R10: Compose services after a reboot

### What gets the policy

With `OUTHAUL_COMPOSE_RESTART=unless-stopped`, a deploy gives the policy to
each service of the stack except:

| Service | Why it is left alone |
|---|---|
| has a `restart` key, whatever its value, `"no"` included | the brief: never override an explicit value. An override's `restart` replaces the file's (probed) |
| has `deploy.restart_policy` | Compose turns it into the container's restart policy, and it wins over `restart` (probed) |
| another service waits on it with `condition: service_completed_successfully` | it is meant to run once. Restarted, it never completes, and the deploy times out (probed) |

A run-once service that nothing waits on can't be told from a server by its
file. With the policy it runs again each time it exits, and the deploy still
succeeds. The deploy log names every service given the policy, and every one
left alone with the reason, and the docs say to give such a service
`restart: "no"`.

The setting changes how a deploy ends too. Checked on Compose v5.6.0 with
Outhaul's `up -d --wait` flags: today a service with no restart policy that
exits at once fails the deploy — `container … exited (1)`, and the same for
exit 0. Given the policy, `up` exits 0 and the service restarts in a loop; the
same service with a healthcheck still fails `up --wait` (`is unhealthy`). So
the setting can turn a deploy that failed into one that succeeds with a
service crash-looping inside it. That is the second reason it is a setting, it
is why the docs say so, and it is why no new check runs on a container after
`up`.

### Reading the model

The override knows only the services that have domain rows. To know them all,
the Compose pipeline (`internal/deploy/pipeline_compose.go`) asks Compose,
after it writes `.env` and before it writes the override.

The R6 and R7 design (`2026-11-11-container-guard-rails-design.md`, "Reading
the Compose model") owns that reader, and this design uses it as written
there. Its first commit is the reader alone: `Runner.Config`, `ParseModel`,
and `compose.ReadModel`, which returns a `Model` of `Service`s. R10 is built
on that commit, and builds it first if R6 hasn't.

- A service whose `HasRestart()` is true is left alone. That is the first two
  rows of the table: it sets `restart` or `deploy.restart_policy`.
- The third row is R10's own. A pure function in `internal/compose` reads
  each service's `depends_on` from `Service.Raw` and returns the services
  that another waits on to complete. It adds no field to `Service`.
- `ReadModel` runs when R10's setting is on, as that design says. A file
  Compose can't load fails the deploy there. The JSON is never logged.

### The override

Today `Override(app, domains, network, tlsEnabled, certResolver,
gateAddress)` writes `networks:` and `labels:` for each service with a domain
row, then a top-level `networks:` block naming the shared network as
external. The pipeline writes the file only when the app has domain rows.

That design gives `Override` a last parameter, `extra map[string]Extra`, for
what each service gains, and leaves a place on `Extra` for R10. R10 adds
`Restart string` there, and a function that builds the map from the `Model`
and the setting, merged with R7's when both apply. Built before R6 and R7,
R10 adds the parameter and `Extra` with that one field.

- A service with `Extra.Restart` gets `restart: unless-stopped`, beside its
  networks and labels if it has domain rows.
- The top-level `networks:` block is written only when a service is published.
- The pipeline writes the file when there are domain rows or extras. With
  neither, there is no file, and with no extras its bytes are today's.

The policy reaches a stack at its next deploy, and nothing redeploys stacks
that exist. Stop is `docker compose stop` (`handleStopApp`), and a container
stopped that way stays stopped after a reboot, as a single-container app does.

### What comes back at boot

`deploy/outhaul.service` already has `After=network-online.target
docker.service` and the same in `Wants=`. It doesn't change.

| What | Restart policy | After a reboot |
|---|---|---|
| Traefik, cloudflared | `unless-stopped` | Docker starts them. `ensureInfra` then starts one that is stopped and replaces one whose settings changed |
| Single-container apps, databases | `unless-stopped` | Docker starts them, unless they were stopped on purpose |
| Compose services | the file's | today, only if the file says so. With the setting, as apps |
| A deploy's health-check container, backup helpers | none | they don't come back. The deployment or backup is marked failed at startup, and isn't retried |

So Outhaul reconciles Traefik and cloudflared at boot, and not apps. Docker
brings apps back. That is the decision in `ARCHITECTURE.md`, and this design
keeps it: Outhaul has no record of which apps were meant to be running.

## R9: domains under one suffix

### Where names are made

`core.OneLabelUnder(host, suffix)` is true when `host`, lower-cased and
without a trailing dot, is one non-empty label, a dot, and the suffix. The
bare suffix and `a.b.<suffix>` are not. R1's `traefik.WildcardCovers` asks the
same of a wildcard. Whichever is built second calls the other.

The server's IP reaches `server.New`, `githook.NewBridge`, `previewmgr.New`,
`catalog.Render` and `core.PreviewHost` as a `serverIP string`. The first
commit replaces it with one value, a `core.Naming` holding the IP, the suffix
and the "only" flag, with no change in any name. After that:

| Where | Today | With a suffix |
|---|---|---|
| Template `${domain}` (`catalog.Render`) | `<app>-<6 hex>-<ip>.sslip.io`, different each time it appears | `<app>.<suffix>`, the same each time |
| Template `${domain:<label>}` | read as `${domain}` | `<app>-<label>.<suffix>` |
| Template `${email}` | `<random>@<app>-<ip>.sslip.io` | `<random>@<app>.<suffix>` |
| Preview with no base domain (`core.PreviewHost`, and `Server.mintedPreviewHost`, which calls it) | `<label>.<ip>.sslip.io` | `<label>.<suffix>`. The label is `[<service>-]<app>-pr-<n>`, cut to 63 characters, as today |
| Domain dialog (`parseDomainForm`, `domainform.tmpl`) | `host_kind=sslip` gives `<app>.<ip>.sslip.io` | that option is replaced by `host_kind=default`, which gives `<app>.<suffix>` |
| First `git push` of a single-container app (`githook.coldCreate`) | `<app>.<ip>.sslip.io` | `<app>.<suffix>` |
| New-app form (`appform.tmpl`, `handleCreateApp`) | the domain is typed, and `validateApp` requires it for a single container | filled with `<name>.<suffix>` as the name is typed, until it is edited. Left empty for a single container, the server uses that name |

An app name is 2 to 40 characters of `a-z`, `0-9` and `-` (`core.AppNameRe`),
so it is always a valid label. A preview base domain that an app sets still
wins over the suffix.

### Templates

A template that uses `${domain}` twice gets two names today. Under a suffix
it would get one name twice, and its second row would be refused. The MinIO
template does this for its console and its API, so its `api_domain` becomes
`${domain:api}`. The helper syntax already parses `name:arg` (`generate`,
`internal/catalog/expand.go`), so without a suffix `${domain:api}` gives what
`${domain}` gives today.

`handleDeployTemplate` (`internal/server/catalog.go`) creates the app, then
adds each row, and can stop half-way with "App created, but a domain could
not be added". With random names it didn't. Now it can, so the handler
creates the app and its rows in one transaction (`CreateAppWithDomains`),
and first asks `sharedHostRefused` about each host, as `handleCreateApp` does.

### Suffix only

With `OUTHAUL_DOMAIN_SUFFIX_ONLY` on, the rule is enforced where the
reserved admin host is:

- The store is given the suffix at startup, as `SetReservedHost` is given the
  admin host. `checkRouteFreeTx` (`internal/store/domains.go`) returns a new
  `ErrHostOutsideSuffix` for a host that isn't one label under it. That
  covers every writer: `AddDomain`, `CreateAppWithDomains`, and a move in
  `UpdateDomain`. It holds for everyone, system admins included (call 15).
- The forms check first, to say why: "Only names one label under `<suffix>`
  reach this server, such as `demo.<suffix>`. `<host>` wouldn't be routed."
  `parseDomainForm`, `handleCreateApp` and `coldCreate` do.
- Rows already outside stay. Startup logs a warning for each, as
  `reserveAdminHost` does. `UpdateDomain` checks only a move, so such a row
  can be edited in place, to turn sign-in on for instance, or deleted.
- A preview's name is one label under its base domain, so a base domain
  other than the suffix is refused when it is saved
  (`handleSavePreviewConfig`). One saved earlier makes the preview fail when
  it is created, and `spawnFailedComment` gains a line for that.

### Collisions

Two rules already refuse a name in use, in the transaction that writes it:
another app's row on the same host and path (`DomainTakenError`), and the
admin host (`ErrHostReserved`). App names are unique, so two apps can't both
generate `<app>.<suffix>`. What remains is a generated name that a row typed
on another app already has, or the admin UI's own name.

Every creation path reports that with the host, and leaves nothing behind.
The new-app form and the dialog do today, and the template deploy does after
the change above. `coldCreate` has a message for the admin host and gains one
for a taken name: "an app named `x` would get `x.<suffix>`, which another app
serves; push under another name". `takenText` still decides who is told the
other app's name.

## R11: sign-in on new domains

### When sign-in can't work

`protectionAvailability` (`internal/server/protection.go`) says whether a row
can require sign-in: the public URL must be set, and visitors must reach it
over HTTPS or through a tunnel. `Validate`, `serve` and `Disable` hold both
true while the setting is on (see [Settings](#settings)). What is left is a
tunnel setting that can't be read, on a server without HTTPS. Then the row
isn't created, and the error says why.

With the setting on, `infraController.reconcile` treats protected routes as
present whatever the rows say, so Traefik always maps the gate's host. The
admin route needs that mapping already, so this recreates nothing.

### Every path that creates a row

Six places write a domain row. `AddDomain`, `CreateApp` and
`CreateAppWithDomains` have no other callers.

| Path | Today | With the setting on |
|---|---|---|
| Domain dialog: `handleAddDomain`, `domainform.tmpl` | the switch is off for a new row. The `protected` field decides | the switch is on for a new row, and turning it off opens the row. A request without the dialog's `protected_offered` field, from a script or an old page, is protected |
| New-app form: `handleCreateApp`, `appform.tmpl` | no switch. `CreateAppWithDomains` seeds a single-container app's row itself, open (`internal/store/apps.go`) | the form shows the switch, on, with the same `protected_offered` and `protected` fields and the same rule. The store call gains a way to seed that row protected. A Compose app's first row is passed in, protected |
| Template deploy: `handleDeployTemplate` | rows are open | rows are protected. The form gets no switch |
| First `git push`: `githook.coldCreate` | the row is open | the row is protected |
| Preview: `previewmgr.mintDomains` | each row copies its parent row's switch | a whole-host row, one with an empty path, is protected whatever its parent's. A path row copies its parent's (call 20) |
| Parent's domains saved: `Server.addPreviewRow` | adds protected rows only | unchanged |

With the setting off, none of these changes and the new-app form has no
switch. Editing a row (`handleUpdateDomain`) is unchanged either way.

A new protected row has the side effects `handleAddDomain` already handles:

- **Another project on the host.** `handleCreateApp` and
  `handleDeployTemplate` ask `sharedHostRefused` with `protecting` true.
  `coldCreate` has no request to ask it with, so it asks the store
  (`OtherProjectsOnHost`) and refuses the push if another project has any row
  on the host, whoever pushes.
- **Another app's old open route on the host.** The dialog warns, then
  redeploys that app (`othersToRedeploy`). The new-app form and the template
  deploy ask `leftoverRoutes` and refuse, saying to redeploy that app first
  (call 21). The first push checks rows only, as the preview manager does.
- **The gate's host.** The two handlers call `ensureGateHost` after the save,
  as the dialog does. If it fails they delete the app they made and report
  it. The first push relies on the mapping above.

`main.go` gives the preview manager and the bridge the setting as a flag. The
dialog's line "Existing previews stay protected. New ones will be open." must
say, with the setting on, that a new preview's own address requires sign-in.

### Making something public

Nothing new is needed. With the setting on, the docs and the dialog's hint say:

- **A whole host:** turn "Require sign-in" off on the host's row.
- **One path, such as a webhook:** add a row for that path with "Require
  sign-in" off, under the protected host ("Leaving a path open" in
  `docs/PROTECTED-ROUTES.md`).

A project member or above on the app's project may do either, as today. The
save redeploys a running app. A preview's row is opened the same way.

## Checked against real Docker and Compose so far

Docker 29.9.1 (API 1.56), Compose v5.6.0 and BuildKit v0.34.0 on Linux, with
throwaway containers and networks, since removed.

1. A new container gets the lowest free address. A stopped container with a
   fixed address gives it up: another took it, and the first then failed to
   start with `Address already in use`.
2. With a gateway given and the upper half as the range, containers got
   `.128`, then `.129`. A fixed `.2` wasn't handed out while its container
   was stopped or removed, and survived a recreate, a restart and a stop.
   With a range and no gateway, Docker took the gateway from the range.
3. A fixed address in use, or equal to the gateway, fails with `Address
   already in use`. One on a network whose subnet Docker chose is accepted,
   so "the address needs the subnet" is this design's rule, not Docker's.
4. A network that overlaps another isn't created: `Pool overlaps with other
   one on this address space`. `docker network inspect` gives the subnet,
   range, gateway and each attached container's address.
5. Moving: with its containers disconnected but running, a network was
   removed and created again under its name with a subnet. Connected again,
   the containers got addresses from the new range, found each other by name,
   and kept the attachment over a restart.
6. `docker build` steps ran on Docker's default bridge. `docker compose
   config --format json`, with and without `--no-env-resolution`, leaves
   `restart` out when the file has no key, keeps `"no"`, and shows
   `deploy.restart_policy` and each `depends_on` condition.
7. An override's `restart` replaced `no`, `always` and `on-failure:3`, and
   `up` recreated only the service the override named. With only
   `deploy.restart_policy` the container's policy was `on-failure`, and with
   both keys it stayed `on-failure`.
8. A run-once service given `unless-stopped` restarted without end, and
   `up --wait` still exited 0. When a service waited on it with
   `service_completed_successfully`, that service never started and
   `up --wait` failed: `timeout waiting for dependencies`. With the policy on
   the waiting service only, both behaved.

Not checked: the firewall rule and the metadata service; a fixed address
across a Docker daemon restart or a reboot; `unless-stopped` after a manual
stop across a daemon restart; any older Docker or Compose.

## Tests

- **`internal/config`:** each variable's bad values and combinations, named
  by `Validate`. The gateway and range derived for several subnets.
- **`internal/docker`:** with the `httptest` Engine API in `network_test.go`:
  the create request carries subnet, gateway and range, and with no subnet is
  today's; a matching network creates nothing; a different one returns the
  mismatch error; the container create carries the endpoint's address.
  `docker.Fake` records network specs and can hold an existing network.
- **`internal/traefik`, `internal/tunnel`, `main`:** the spec carries the
  address. The hash is unchanged with none and changes with one.
  `EnsureProxy` leaves the old container when another holds the address.
  Startup fails in the three cases, and not with no subnet. `Disable` refuses
  when it would leave neither HTTPS nor a tunnel.
- **`internal/compose`, `internal/deploy`:** the selection, for each row of
  the table, on JSON captured from real Compose. The override with the
  policy only, with both, and with no extras (today's bytes). With
  `compose.Fake`: a stack with no domain rows gets the override, the log
  names the services, and with the setting unset `Config` isn't called.
- **`internal/core`, `internal/catalog`, `internal/store`:** `OneLabelUnder`,
  including a two-label name, the bare suffix, case and a trailing dot. Each
  name in the table, with and without a suffix. Every template's rows are
  distinct under a suffix. The suffix rule on each writer, an outside row
  edited in place, and a primary row seeded protected.
- **`internal/server`, `internal/githook`, `internal/previewmgr`:** each
  creation path with the setting on and off, including a request without
  `protected_offered`, the refusals, and the undo. The template deploy leaves
  nothing behind when a name is taken. Names under the suffix on each form.
- **Installer:** the unit file orders itself after `docker.service`.

## Docs

- `README.md`: the six variables in the configuration table.
- `docs/SHARED-HOST.md`, new: the subnet and address, how the gateway and
  range follow, an example of the firewall rule and the table of what runs
  where, the steps to move an existing install (check 5 above), the restart
  setting and run-once services, the suffix and the "only" rule.
- `docs/PROTECTED-ROUTES.md`: a section on the setting, with "Making
  something public" above, and the Previews section.
- `ARCHITECTURE.md`: a short section per requirement. The template catalog's
  "zero-DNS shape" stays the default, with the suffix named as the exception.
- `AGENTS.md`: a "Decisions to keep" line each for the address range, the
  services left alone, and "a new row is never left open by default".

## Order of work

Each series stands alone and is one pull request.

- **R8:** the settings; the client (network spec, inspect, the endpoint
  address) and the fake; Traefik and the connector; startup and the log line.
- **R10:** the setting; the R6 design's reader commit, if it isn't there, and
  the selection; the override; the pipeline and its log lines; the unit-file
  test.
- **R9:** `core.Naming` in place of `serverIP`; the settings and
  `OneLabelUnder`; the names, with `${domain:<label>}` and the MinIO
  template; the template deploy in one transaction, and the collision
  messages; the "only" rule in the store and the forms.
- **R11:** the setting, the refusals and the gate host mapping; the protected
  primary row in the store; the dialog; the new-app form, template deploy and
  first push; previews.

Each ends with its docs. None needs R1 to R4, or another. R9 and R11 both
change `handleCreateApp`, `handleDeployTemplate`, `coldCreate` and the two
forms. R1 to R4 also change `hashConfig`, `ProxyConfig`, `Override`,
`handleCreateApp` and `mintDomains`. The second to land rebases.

## What only the real host can show

1. After install, `docker inspect outhaul-traefik` shows the fixed address,
   and `docker network inspect outhaul` the subnet, gateway and range. After
   `docker rm -f outhaul-traefik` and a restart of Outhaul, and after an
   update, Traefik has the same address.
2. After `sudo reboot`, and after `systemctl restart docker`: Traefik has its
   address, and every app, database and Compose service answers with nobody
   acting. A stack stopped in the UI beforehand is still stopped.
3. With the firewall rule in place, the metadata request in the brief times
   out from an app container, an unpublished Compose service, a database
   container and a `RUN` step of a build. Traefik still renews: force one
   with ACME staging. A Nixpacks app, a Dockerfile app, a Compose app, a
   template and a preview all still deploy.
4. With another subnet in the environment file, Outhaul refuses to start and
   the journal says why.
5. A new app is offered `<app>.<suffix>`, a template lands on
   `<name>.<suffix>` and a preview on `<app>-pr-<n>.<suffix>`, all served
   through the CDN with no new certificate. `a.b.<suffix>` is refused.
6. Each of those, and a first `git push`, starts with sign-in required. A
   path row opened for a webhook is reached without signing in.
7. The host's Docker and Compose versions, with the checks above run again
   on them.

## Left to the builder

- Go names, error texts and UI wording beyond those given.
- Where the gateway-and-range function lives, and whether `InspectNetwork`
  and R4's `NetworkGateway` are one call.
- The shape of "seed the primary row protected" on `CreateAppWithDomains`,
  and what `githook`'s `Store` interface needs for it.
- The script that fills the new-app form's domain, and how a Compose app's
  optional domain is offered there.
- How the network check reads a network with more than one IPAM entry.

## Calls made in this design

Each was this design's call. The maintainer approved the design, with these
calls, on 11 November 2026.

1. The upper half of the subnet is the dynamic range, always, and Traefik's
   address must be in the lower half. A range must be one CIDR, and half is
   the only split that doesn't leave one side tiny.
2. The gateway is the subnet's first address, passed explicitly.
3. The subnet may be set without the address. The address needs the subnet.
4. With a subnet set, Outhaul refuses to start when Docker can't be reached,
   not only when the network differs. Otherwise the check is skipped just
   when nothing can be verified.
5. Outhaul never recreates a network. Moving an install is manual.
6. No `outhaul info`. The values are the configured ones, and a log line
   repeats them with what is derived.
7. IPv4 only and `/28` at the smallest. `EnsureProxy` checks who holds the
   address before it replaces Traefik.
8. R10 has a setting, `OUTHAUL_COMPOSE_RESTART`, though the brief names none:
   it changes what a stack does at its next deploy.
9. `deploy.restart_policy` counts as an explicit value.
10. A service another waits on to complete is left alone. Every other service
    without a policy gets one, run-once or not.
11. The model comes from the R6 design's reader. The run-once exception is
    read from `Service.Raw`, which that design doesn't mention.
12. Outhaul starts no apps at boot, and existing stacks get the policy only
    when they are next deployed. The unit file doesn't change.
13. Template names lose their random part under a suffix, and
    `${domain:<label>}` is added for a second name.
14. The suffix rule is its own setting, `OUTHAUL_DOMAIN_SUFFIX_ONLY`, so an
    install can have default names without refusing other domains.
15. Under it a name outside the suffix is refused for everyone. The brief
    allowed a warning for system admins. The name wouldn't be routed.
16. The suffix is not derived from `OUTHAUL_TLS_WILDCARD`, which would change
    names on an install that set only the wildcard. Set together, they agree.
17. The suffix replaces the `sslip.io` option in the dialog.
18. The template deploy becomes one transaction, with the shared-host check.
19. `OUTHAUL_PROTECT_NEW_DOMAINS` refuses to start where sign-in can't work,
    and a row that can't be protected isn't created. The template form gets
    no switch, and a request that doesn't say is protected.
20. A preview's whole-host row starts protected even when its parent's is
    open. Its path rows copy the parent's, so a webhook path stays open.
21. The new-app form and the template deploy refuse when another app still
    serves an old open route on the host. They don't redeploy that app.
22. The optional system-admin-only setting is left out. An open row for the
    path `/` opens a host too, so it would be a guard rail only, and roles
    already are guard rails for trusted people. It can be added later.
