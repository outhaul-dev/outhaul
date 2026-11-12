# A shared host

These settings suit a host running many people's apps. They are off unless set:
an ordinary install lets Docker pick the shared network's subnet and Traefik's
address, and a Compose service comes back after a reboot only if its own file
says so.

## A fixed subnet and Traefik address

| Variable | Values | Default | Needs |
|---|---|---|---|
| `OUTHAUL_NETWORK_SUBNET` | an IPv4 network in CIDR form, `/28` or larger, such as `172.30.0.0/24` | unset: Docker picks | nothing |
| `OUTHAUL_TRAEFIK_IP` | an IPv4 address in the lower half of the subnet, after its first two addresses | unset: Docker picks | `OUTHAUL_NETWORK_SUBNET` |

```sh
OUTHAUL_NETWORK_SUBNET=172.30.0.0/24
OUTHAUL_TRAEFIK_IP=172.30.0.2
```

The gateway and the address ranges follow from the subnet:

| | For `172.30.0.0/24` | Rule |
|---|---|---|
| Gateway | `172.30.0.1` | the first address, given to Docker explicitly |
| Fixed addresses | `172.30.0.2` to `172.30.0.127` | the rest of the lower half. Docker never hands these out |
| Dynamic range | `172.30.0.128/25` | the upper half, passed as the network's `IPRange` |

The dynamic range is the upper half, so the subnet's size decides how many
containers can hold an address. A `/28` leaves about eight, shared by
cloudflared, every single-container app, every database, every deploy's
health-check container and every published Compose service. Size the subnet for
the host: the `/24` above leaves about 126.

### Why a fixed address

Every container on the host can reach the cloud metadata service at
`169.254.169.254`, which answers with an access token for the host's identity to
whoever asks. On a host running other people's apps, that is the host's
credentials in any container. The infrastructure's answer is a firewall rule
that drops traffic to that address from every source but Traefik
("The firewall rule" below). Such a rule names a source address, and today
Docker picks the shared network's subnet and every address on it, Traefik's
included, so there is no address to name. These two settings fix both.

Traefik needs the metadata service only when its DNS-01 provider takes the
host's identity from it; Azure's managed identity is one such provider.

### Starting up

With `OUTHAUL_NETWORK_SUBNET` set, `outhaul serve` checks the shared network
before it does anything else with Docker, and refuses to start in three cases:

| The problem | What the error says |
|---|---|
| Docker can't be reached | that the network can't be checked, and why |
| the network exists with another subnet, gateway or range | both sets of values, that Outhaul won't change a network containers are on, and where the steps to move are |
| Docker refuses to create it, for example because it overlaps another network on the host | the variable, and Docker's own message |

systemd shows the reason and tries again, so the same text is in
`journalctl -u outhaul`.

Once the network is ready, `serve` logs one line with the values set and the
ones derived from them:

```
network "outhaul": subnet 172.30.0.0/24, gateway 172.30.0.1, dynamic range 172.30.0.128/25, Traefik at 172.30.0.2, host bridge "outhaul"
```

With no `OUTHAUL_NETWORK_SUBNET`, startup is as before: an unreachable Docker is
a warning and the admin UI still starts.

### Moving an existing install

An install that already has its network can't be changed in place: other
containers are on it, and Outhaul never removes or reconfigures one. Move it by
hand, with the containers disconnected but still running.

First pick a subnet that no existing network uses and that lies outside Docker's
default pool. The example `172.30.0.0/24` above is inside that pool
(`172.17.0.0/16` to `172.31.0.0/16`, then `192.168.0.0/16`), so on a host that
already runs stacks one of their networks may hold it, and Docker then refuses
to create Outhaul's. A `10.x` block is a safer choice. Make sure the host's own
LAN doesn't overlap either, and check what the host uses:

```sh
docker network ls
docker network inspect $(docker network ls -q) --format '{{.Name}} {{range .IPAM.Config}}{{.Subnet}} {{end}}'
```

```sh
systemctl stop outhaul
docker rm -f outhaul-traefik
docker network inspect outhaul --format '{{range .Containers}}{{.Name}}{{"\n"}}{{end}}'
docker network disconnect outhaul <name>     # once for each name the line above lists
docker network rm outhaul
```

Then set both variables, in `/etc/outhaul.env` (the `OUTHAUL_*` file the
installer doesn't rewrite) or the operator's own `--env-file` or extra env file.
The `10-install.conf` drop-in is installer-owned and rewritten by an unattended
or `--env-file` run, so a setting put there is lost at the next such run (the
paths are in `docs/MANUAL-DEPLOY.md`). Start Outhaul, which creates the network
with the subnet:

```sh
systemctl start outhaul
docker network connect outhaul <name>        # once for each name, as before
```

If startup fails because the pool overlaps, correct `OUTHAUL_NETWORK_SUBNET` and
start Outhaul again: nothing but the shared network's attachment is lost, and
every container named above is still running.

A disconnected container keeps running; it only loses the shared network, so it
can't be reached through Traefik until it is connected again. A container that
resolved a peer or the proxy at start-up may need a restart once it is
reconnected. cloudflared is on the shared network too: reconnect or recreate it
as well, or the tunnel stays up but cannot reach Traefik.

Traefik must be recreated, not moved with the others: while it is attached,
Docker won't remove the network, and once it is disconnected Outhaul may adopt
it as it is. With only the subnet set, its settings haven't changed, so Outhaul
adopts the disconnected Traefik and leaves it off the shared network. With
`OUTHAUL_TRAEFIK_IP` set, the address changes its settings, so Outhaul recreates
it at the address anyway. `docker rm -f` above is the one step that works either
way. A Compose stack's published services can instead be reattached with
`docker compose up -d`.

### The firewall rule

This is an example for the host's infrastructure, not a rule Outhaul installs or
manages. Replace the address with the configured `OUTHAUL_TRAEFIK_IP`:

```
iptables -I DOCKER-USER -d 169.254.169.254 ! -s 172.30.0.2 -j DROP
```

It matches forwarded traffic to that address on every bridge, so only Traefik's
address reaches the metadata service, whatever network a container is on. It
matches a source address, so it assumes a container can't choose its own: one
that can set a source address is outside what the rule can tell apart.

### What runs where

| What | Runs as | Network | Needs the metadata service |
|---|---|---|---|
| Traefik | container | the shared one, at the fixed address when `OUTHAUL_TRAEFIK_IP` is set | yes, for DNS-01 with the host's identity |
| cloudflared | container | the shared one, at an address Docker picks | no: it dials out with its own token |
| Single-container apps, and each deploy's health-check container (`outhaul-deploy-<id>`) | container | the shared one, at an address Docker picks | no |
| Databases (`outhaul-db-<name>` and the `-new` replacement) | container | the shared one, at an address Docker picks | no |
| Compose stacks | containers | the stack's own network, on a subnet Docker picks; published services are also on the shared one | no |
| Previews | apps and databases | as above | no |
| Backup, restore and data-removal helpers | container | Docker's default bridge | no: they copy files |
| Image builds, Nixpacks or Dockerfile | build steps | Docker's default bridge | no: they need registries and package mirrors |
| Outhaul, `git clone`, the Nixpacks plan, blob-store uploads | host process | the host's own | not forwarded traffic, so the rule doesn't see it |

So a rule that drops forwarded traffic to the metadata address from everywhere
but Traefik's address leaves the rest of the host working.

### Checking the real host

These need the shared host; none is run by CI.

1. After install, `docker inspect outhaul-traefik` shows the fixed address, and
   `docker network inspect outhaul` the subnet, gateway and range. After
   `docker rm -f outhaul-traefik` and a restart of Outhaul, and after an update,
   Traefik has the same address.
2. After `sudo reboot`, and after `systemctl restart docker`: Traefik has its
   address, and every app, database and Compose service answers with nobody
   acting. A stack stopped in the UI beforehand is still stopped.
3. With another subnet in the environment file, Outhaul refuses to start, and
   the journal names the network it found and the one the settings ask for.
4. A Compose stack deployed with `OUTHAUL_COMPOSE_RESTART=unless-stopped` comes
   back after the reboot in item 2 with nobody acting.

## Compose services after a reboot

| Variable | Values | Default | Needs |
|---|---|---|---|
| `OUTHAUL_COMPOSE_RESTART` | `unless-stopped` | unset: only what the Compose file says | Docker Compose 2.35 or newer on the host: with the setting on, every Compose deploy reads the file's normalised model — see [GUARD-RAILS.md](GUARD-RAILS.md#docker-compose-235-or-newer) |

```sh
OUTHAUL_COMPOSE_RESTART=unless-stopped
```

A single-container app and a database come back after a reboot on their own,
because Outhaul creates them with `unless-stopped`. A Compose service comes back
only if its own file says so. With this setting every service of a stack gets
`unless-stopped` at its next deploy, so Docker brings it back the same way.

Three services are left as the file has them:

- one that sets `restart`, whatever the value, `"no"` and `always` alike;
- one that sets `deploy.restart_policy`, which Compose turns into the
  container's policy and which wins over `restart`;
- one that another service waits on with
  `condition: service_completed_successfully`: it is meant to run once, and
  restarted it never completes, so the deploy would time out.

The deploy log names every service of the stack, with the policy it gained or
why it kept its own. The policy is applied by deploying: a stack that exists
keeps its containers until its next deploy, and Outhaul redeploys nothing.

### Run-once services

A service that is meant to run once, and that nothing waits on, looks like a
server to Outhaul: it gets the policy and runs again each time it exits. That is
the case the file can't show, so give it `restart: "no"`:

```yaml
services:
  migrate:
    image: example.com/migrate:1
    restart: "no"
```

### What the policy does to a failed deploy

A service with no healthcheck that exits at start fails the deploy today
(`container … exited (1)`, and the same for exit 0). With the policy, `up`
exits 0 and the service restarts in a loop, so the stack is up with one service
that is not. A service that has a healthcheck still fails the deploy, with
`is unhealthy`.

The setting needs Docker Compose 2.35 or newer: with it on, every Compose deploy
reads the file's normalised model, and an older host fails the deploy before it
builds, with the message in
[GUARD-RAILS.md](GUARD-RAILS.md#docker-compose-235-or-newer).

Whether a stopped stack stays stopped across a reboot is under
[Checking the real host](#checking-the-real-host) above.

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
253-character name, and it must be at least two labels: a suffix of one label
would make `OUTHAUL_DOMAIN_SUFFIX_ONLY` accept any `*.com` host, which no
certificate covers.

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

## A new row is never left open

A host that hands out names to people who don't know each other shouldn't publish
an app because nobody said otherwise. `OUTHAUL_PROTECT_NEW_DOMAINS=yes` closes
that: every domain row Outhaul creates starts with sign-in required — a row added
in the dialog, the new-app form's first row, every row of a template deploy, a
first `git push`, and a preview's own address — and opening an address is a
deliberate choice: turn the switch off on the host's row, or add a path row with
it off, such as a webhook's.

The guide, with what a new row needs, the refusals and how to open one, is
[PROTECTED-ROUTES.md](PROTECTED-ROUTES.md#requiring-sign-in-on-every-new-domain).
Editing a row changes nothing on its own, so a host that turns the setting on
still decides about the rows it already has.

It is the last of the six settings a first shared host sets, the two naming ones
above among them; the table of all six, with what each needs, is in the
[design](superpowers/specs/2026-11-11-shared-host-defaults-design.md#settings),
and README has a row for each.
