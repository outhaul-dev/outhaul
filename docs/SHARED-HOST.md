# A shared host

These settings suit a host running many people's apps. They are off unless set:
an ordinary install lets Docker pick the shared network's subnet and Traefik's
address.

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
hand, with the containers disconnected but still running:

```sh
systemctl stop outhaul
docker rm -f outhaul-traefik
docker network inspect outhaul --format '{{range .Containers}}{{.Name}}{{"\n"}}{{end}}'
docker network disconnect outhaul <name>     # once for each name the line above lists
docker network rm outhaul
```

Then set both variables, in `/etc/outhaul.env` or the installer's drop-in (the
paths are in `docs/MANUAL-DEPLOY.md`), and start Outhaul, which creates the
network with the subnet:

```sh
systemctl start outhaul
docker network connect outhaul <name>        # once for each name, as before
```

A disconnected container keeps running; it only loses the shared network, so it
can't be reached through Traefik until it is connected again. Traefik must be
recreated, not moved with the others: while it is attached, Docker won't remove
the network, and once it is disconnected Outhaul finds a container whose
settings haven't changed and adopts it, leaving it off the shared network. A
Compose stack's published services can instead be reattached with
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
| Traefik | container | the shared one, at the fixed address | yes, for DNS-01 with the host's identity |
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
