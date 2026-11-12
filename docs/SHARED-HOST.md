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
