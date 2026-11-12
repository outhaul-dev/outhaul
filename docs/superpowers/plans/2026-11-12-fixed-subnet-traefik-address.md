# A Fixed Subnet and Traefik Address Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give Outhaul's Docker network a fixed subnet and Traefik a fixed, never-reassigned address on it, so a firewall rule can admit only Traefik to the host's metadata service.

**Architecture:** `docker.PlanSubnet` derives the gateway and the upper-half dynamic range from an IPv4 CIDR. `EnsureNetwork` creates the network with those three values or refuses an existing one that differs; `InspectNetwork` reports who holds an address, and `ContainerSpec.IPv4` pins Traefik's endpoint. With `OUTHAUL_NETWORK_SUBNET` set, `serve` checks the network before anything else; the address enters Traefik's config hash only when configured, so an install that sets neither sees no change.

**Tech Stack:** Go 1.26, Docker Engine API (the real client is tested with the `httptest` Engine API as in `internal/docker/network_test.go`), Traefik v3.7.6, POSIX `sh` for the documented move, `iptables` for the example firewall rule.

**Spec:** `docs/superpowers/specs/2026-11-11-shared-host-defaults-design.md`, section "R8: a fixed subnet and Traefik address" and its "Settings", "Tests" and "Docs" sections. Read it first; this plan argues from it.

## Global Constraints

- An install that sets neither variable sees no change: the same network-create request (`IPAM` absent, the same bridge options), the same Traefik config hash and golden files, the same startup warnings, the same pages.
- Settings are `OUTHAUL_*` variables read through the `parser` in `internal/config/config.go`; `Validate` names the variable in every error and `serve` refuses to start on a bad value.
- New variables, exactly: `OUTHAUL_NETWORK_SUBNET` (an IPv4 CIDR, `/28` or larger, no host bits) and `OUTHAUL_TRAEFIK_IP` (an IPv4 address; it needs the subnet).
- For `172.30.0.0/24`: gateway `172.30.0.1`, fixed addresses `172.30.0.2` to `172.30.0.127`, dynamic range `172.30.0.128/25`. The gateway is the subnet's first address, given to Docker explicitly. The dynamic range is the upper half, always.
- Outhaul never removes, recreates or reconfigures a network that already exists: a mismatch is an error naming both sets of values.
- With `OUTHAUL_NETWORK_SUBNET` set, `serve` fails on an unreachable Docker, a mismatching network or a create failure, so systemd shows the reason and retries. With it unset, startup behaves as today (warn and carry on).
- Traefik's fixed address goes on the first network of its `ContainerSpec`. `hashConfig` covers the address only when it is non-empty, so an upgrade without it recreates nothing.
- `EnsureProxy` checks who holds the address before it stops the old Traefik, and ignores the old Traefik itself.
- Test first: write the failing test, watch it fail for the stated reason, then implement. Unit tests use `docker.Fake`; Docker API behaviour is pinned with `httptest`.
- This repository is public: `example.com` names only, in code, tests and docs.
- Commits look like `feat(scope): summary` or `fix(scope): summary`; the body says what a user saw go wrong and what changed.
- Docs travel with the code. Task 2 adds the `README.md` rows and the first section of `docs/SHARED-HOST.md`; Task 7 finishes that document, `ARCHITECTURE.md` and `AGENTS.md`.
- After each task: `go build ./... && go vet ./... && go test ./...`. Before calling the plan done: `go test -race -timeout 30m ./...`.

## Review Focus

The inputs most likely to bite that the requirement doesn't spell out. Each has its test in the task named.

1. An existing network with no IPv4 IPAM, such as an IPv6-only one: with a subnet set it is a mismatch that says what Docker has, not a panic or a silent adoption; with no subnet it is adopted as today (Task 3).
2. A fixed address already held by another container: refused before the old Traefik is stopped, with the holder's name, rather than surfacing later as Docker's `Address already in use` (Task 4).
3. An address equal to the network address or the gateway, in the upper half, or outside the subnet: refused by `Validate` with the allowed range, not at container-create time (Task 2).
4. A `/28`, the smallest allowed subnet: six fixed addresses, and the dynamic range is a `/29`, not a range that swallows the fixed addresses (Tasks 1 and 2).
5. Docker unreachable at startup while a subnet is set: `serve` fails instead of starting the admin UI, and the message says the network can't be checked and why (Task 6).

---

## File map

| File | Responsibility | Tasks |
|---|---|---|
| `internal/docker/subnet.go` (new) | `SubnetPlan`, `PlanSubnet`, `Fixed`, `NetworkFor` | 1, 3 |
| `internal/docker/client.go` | `NetworkSpec`, `NetworkInfo`, `NetworkMismatchError`, the two client calls, `ContainerSpec.IPv4` | 3 |
| `internal/docker/real.go` | network inspect/create, fixed endpoint address | 3 |
| `internal/docker/fake.go` | recorded specs, existing network details, fixed attachments | 3 |
| `internal/docker/network_test.go`, `subnet_test.go` (new) | the Engine API and pure-subnet tests | 1, 3 |
| `internal/config/config.go`, `config_test.go` | the two settings and their validation | 2 |
| `internal/traefik/proxy.go`, `proxy_test.go` | `Subnet`/`IPv4`, the network spec, the holder check, the hash | 4 |
| `internal/tunnel/connector.go`, `connector_test.go` | the connector's `Subnet` | 5 |
| `main.go`, `main_test.go` | startup check, the log line, wiring | 6 |
| `README.md` | the two configuration rows | 2 |
| `docs/SHARED-HOST.md` (new) | the settings, the derived layout, the move, the rule, the table | 2, 7 |
| `ARCHITECTURE.md`, `AGENTS.md` | the decision, in a short section and a bullet | 7 |

---

## Task 1: Derive a subnet's gateway and address range

Pure arithmetic, no Docker yet. This is the one rule every later task uses.

**Files:**
- Create: `internal/docker/subnet.go`, `internal/docker/subnet_test.go`
- Test: `internal/docker/subnet_test.go`

**Interfaces:**
- Produces:

```go
// internal/docker/subnet.go
type SubnetPlan struct {
	Subnet  string // canonical CIDR, e.g. "172.30.0.0/24"
	Gateway string // the first address, e.g. "172.30.0.1"
	IPRange string // the upper half, e.g. "172.30.0.128/25"
	First   string // first fixed address, e.g. "172.30.0.2"
	Last    string // last fixed address, e.g. "172.30.0.127"
}
func PlanSubnet(cidr string) (SubnetPlan, error)
func (p SubnetPlan) Fixed(ip string) bool
```

- [ ] **Step 1: Write the failing tests**

```go
// TestPlanSubnet: for each case, PlanSubnet(cidr) equals the whole struct.
//   "172.30.0.0/24" -> {Subnet: "172.30.0.0/24", Gateway: "172.30.0.1",
//                       IPRange: "172.30.0.128/25", First: "172.30.0.2", Last: "172.30.0.127"}
//   "172.30.0.0/28" -> {Subnet: "172.30.0.0/28", Gateway: "172.30.0.1",
//                       IPRange: "172.30.0.8/29", First: "172.30.0.2", Last: "172.30.0.7"}
//   "172.30.0.0/27" -> IPRange "172.30.0.16/28", Last "172.30.0.15"
//   "172.30.0.0/23" -> IPRange "172.30.1.0/24", Last "172.30.0.255"
//   "172.30.0.0/16" -> IPRange "172.30.128.0/17", Last "172.30.127.255"
// TestPlanSubnetRejects: each error names what is wrong.
//   "fd00::/64"      -> contains "IPv4"
//   "172.30.0.0/29"  -> contains "/28"
//   "172.30.0.5/24"  -> contains "host bits" and "172.30.0.0/24"
//   "172.30.0.0"     -> an error
//   "172.30.0.0/33"  -> an error
// TestSubnetPlanFixed, on 172.30.0.0/24:
//   "172.30.0.2", "172.30.0.127" -> true
//   "172.30.0.0", "172.30.0.1", "172.30.0.128", "172.30.0.255" -> false
//   "", "fd00::2", "not-an-ip" -> false
```

- [ ] **Step 2: Run the tests to see them fail**

Run: `go test ./internal/docker/ -run 'PlanSubnet|SubnetPlanFixed'`
Expected: FAIL (undefined `PlanSubnet`, `SubnetPlan`).

- [ ] **Step 3: Implement `internal/docker/subnet.go`**

`net.ParseCIDR`, then convert the four-byte address with `encoding/binary.BigEndian.Uint32`. Compute `size := uint64(1) << (32 - prefix)` and `half := size / 2` (uint64 avoids the shift overflow at `/0`); gateway is network+1, `First` network+2, `Last` network+half-1, `IPRange` network+half with prefix `prefix+1`. Reject, in this order: a parse error; `ip.To4() == nil` (IPv6); `prefix > 28`; `!ip.Equal(ipnet.IP)` (host bits, with the canonical CIDR in the message). `Fixed` parses the address, returns false unless it is IPv4, and compares its uint32 with `First` and `Last` inclusive.

- [ ] **Step 4: Run the tests to see them pass**

Run: `go test ./internal/docker/ -run 'PlanSubnet|SubnetPlanFixed'`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/docker/subnet.go internal/docker/subnet_test.go
git commit -m "feat(docker): derive a subnet's gateway and fixed address range"
```

---

## Task 2: The two settings and their validation

**Files:**
- Create: `docs/SHARED-HOST.md`
- Modify: `internal/config/config.go:30-120,198-264`, `internal/config/config_test.go`, `README.md` (configuration table)
- Test: `internal/config/config_test.go`

**Interfaces:**
- Consumes: `docker.PlanSubnet`, `SubnetPlan.Fixed` from Task 1.
- Produces:

```go
// config.Config
NetworkSubnet string // OUTHAUL_NETWORK_SUBNET; "" lets Docker pick
TraefikIP     string // OUTHAUL_TRAEFIK_IP; "" lets Docker pick

// config.parser, unexported
func (p *parser) subnet(key string) string
func (p *parser) ipv4(key string) string
```

- [ ] **Step 1: Write the failing tests**

```go
// TestNetworkSubnetSetting (Load with just the variable, then Validate):
//   unset            -> NetworkSubnet "", Validate nil
//   "172.30.0.0/24"  -> stored as given, Validate nil
//   "172.30.0.0/28"  -> stored as given, Validate nil
//   "fd00::/64", "172.30.0.0/29", "172.30.0.5/24", "172.30.0.0"
//        -> NetworkSubnet "", error contains "OUTHAUL_NETWORK_SUBNET"
// TestTraefikIPSetting (with OUTHAUL_NETWORK_SUBNET=172.30.0.0/24):
//   "172.30.0.2", "172.30.0.127"              -> stored, Validate nil
//   "172.30.0.0", "172.30.0.1", "172.30.0.128", "172.30.0.200", "192.168.1.2"
//        -> error contains "OUTHAUL_TRAEFIK_IP", "172.30.0.2" and "172.30.0.127"
//   "fd00::2", "172.30.0"                     -> error contains "OUTHAUL_TRAEFIK_IP"
// TestTraefikIPNeedsTheSubnet: "172.30.0.2" alone -> error contains both variable names
```

- [ ] **Step 2: Run them to see them fail**

Run: `go test ./internal/config/ -run 'NetworkSubnet|TraefikIP'`
Expected: FAIL (unknown fields, missing parser methods).

- [ ] **Step 3: Implement**

- Add the two fields beside `Network` and load them with `p.subnet` and `p.ipv4`.
- `subnet`: empty stays empty; otherwise `docker.PlanSubnet(v)`, and on error append `fmt.Errorf("%s=%q: %v", key, v, err)` as the other parser helpers do, returning `""`.
- `ipv4`: empty stays empty; `net.ParseIP(v)` must parse and `To4()` must be non-nil, else `p.reject(key, v, "an IPv4 address such as 172.30.0.2")`.
- `Validate`: when `TraefikIP != ""` and `NetworkSubnet == ""`, add `OUTHAUL_TRAEFIK_IP=<ip> needs OUTHAUL_NETWORK_SUBNET, which fixes the addresses Traefik may use`. When both are set, `plan, err := docker.PlanSubnet(c.NetworkSubnet)`; if `err == nil && !plan.Fixed(c.TraefikIP)`, add `OUTHAUL_TRAEFIK_IP=<ip>: want an address from <plan.First> to <plan.Last> in OUTHAUL_NETWORK_SUBNET=<plan.Subnet> (the lower half, after the gateway)`.
- `README.md`: two rows after `OUTHAUL_NETWORK`, both linking `docs/SHARED-HOST.md`; the subnet default is "(unset: Docker picks)" and the address default likewise.
- `docs/SHARED-HOST.md`: the title, one paragraph saying these settings suit a host running many people's apps and are off unless set, then "A fixed subnet and Traefik address" with the two-row settings table, a two-line example (`OUTHAUL_NETWORK_SUBNET=172.30.0.0/24`, `OUTHAUL_TRAEFIK_IP=172.30.0.2`), and the derived table for `172.30.0.0/24` (gateway, fixed addresses, dynamic range). Task 7 completes the page.

- [ ] **Step 4: Run everything**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git commit -am "feat(config): a fixed network subnet and Traefik address"
```

---

## Task 3: The network spec, inspection, and a container's fixed address

**Files:**
- Modify: `internal/docker/client.go:30-42,79-99`, `internal/docker/real.go:121-145,306-330`, `internal/docker/fake.go:17-126,220-260`, `internal/docker/subnet.go`, `internal/docker/network_test.go`, `internal/docker/subnet_test.go`, and every `EnsureNetwork` caller: `internal/traefik/proxy.go:123`, `internal/tunnel/connector.go:36`, `main.go:544`, `main_test.go:583-591`
- Test: `internal/docker/network_test.go`, `internal/docker/subnet_test.go`

**Interfaces:**
- Consumes: `PlanSubnet` from Task 1.
- Produces:

```go
// internal/docker/client.go
type NetworkSpec struct {
	Name    string // network name
	Subnet  string // IPv4 CIDR; "" lets Docker choose
	Gateway string // the bridge's address; "" unless Subnet is set
	IPRange string // the addresses Docker hands out; "" unless Subnet is set
}
type NetworkInfo struct {
	Subnet  string            // first IPv4 IPAM subnet, "" when there is none
	Gateway string            // its gateway
	IPRange string            // its dynamic range
	IPv4s   map[string]string // attached container name -> IPv4 address
}
type NetworkMismatchError struct {
	Name string
	Have NetworkInfo
	Want NetworkSpec
}
func (e *NetworkMismatchError) Error() string

// Client interface
EnsureNetwork(ctx context.Context, spec NetworkSpec) error
InspectNetwork(ctx context.Context, name string) (NetworkInfo, error)

// ContainerSpec
IPv4 string // fixed address on the spec's first network

// internal/docker/subnet.go
func NetworkFor(name, subnet string) (NetworkSpec, error)

// docker.Fake, test-settable
NetworkSpecs   []NetworkSpec          // specs passed to EnsureNetwork, in order
NetworkDetails map[string]NetworkInfo // existing networks, by name
```

- [ ] **Step 1: Write the failing tests**

```go
// TestEnsureNetworkCreatesWithSubnet: httptest daemon answers GET /networks/outhaul 404 and records
// POST /networks/create. The body has Driver "bridge", Options["com.docker.network.bridge.name"] "outhaul",
// and IPAM.Config[0] == {Subnet "172.30.0.0/24", Gateway "172.30.0.1", IPRange "172.30.0.128/25"}.
// TestEnsureNetworkWithoutSubnetIsTodaysRequest: same, with Subnet ""; the body's IPAM is nil and the
//   bridge options are unchanged.
// TestEnsureNetworkAdoptsMatching: GET answers 200 with that subnet, gateway and range; no create request.
// TestEnsureNetworkWithoutSubnetAdoptsAnything: GET answers 200 with 10.0.0.0/24; nil, no create request.
// TestEnsureNetworkRefusesADifferentNetwork: GET answers 200 with 10.0.0.0/24, gateway 10.0.0.1 and
//   range 10.0.0.128/25; errors.As(*NetworkMismatchError) is true, and the message contains both subnets,
//   both gateways and both ranges. A second case answers 200 with an empty IPAM.Config: the message says
//   Docker chose the subnet.
// TestInspectNetwork: GET answers 200 with IPAM.Config = [IPv6, IPv4] and
//   Containers = {"id1": {"Name": "outhaul-traefik", "IPv4Address": "172.30.0.2/24"}}.
//   -> Subnet/Gateway/IPRange from the IPv4 entry; IPv4s == {"outhaul-traefik": "172.30.0.2"}.
// TestCreateContainerFixedAddress: POST /containers/create records
//   NetworkingConfig.EndpointsConfig["outhaul"].IPAMConfig.IPv4Address == "172.30.0.2";
//   with IPv4 "" the endpoint's IPAMConfig is nil.
// TestFakeRecordsNetworkSpecs: NewFake().NetworkDetails["outhaul"] = {Subnet "10.0.0.0/24", ...};
//   EnsureNetwork(spec for 172.30.0.0/24) returns a *NetworkMismatchError; the same call with Subnet ""
//   returns nil; a fresh name records the spec in NetworkSpecs and sets Networks[name].
// TestNetworkFor: name only for ""; for "172.30.0.0/24" the canonical subnet, gateway and range;
//   an error for "fd00::/64".
```

Keep `TestEnsureNetworkNamesTheBridge` and `TestEnsureNetworkLeavesLongNamesToDocker`; adapt `createdNetworkOptions` to answer `GET /networks/<name>` with 404.

- [ ] **Step 2: Run them to see them fail**

Run: `go test ./internal/docker/ -run 'EnsureNetwork|InspectNetwork|FixedAddress|FakeRecords|NetworkFor'`
Expected: FAIL (unknown types; the interface no longer matches its callers).

- [ ] **Step 3: Implement**

- `real.EnsureNetwork`: `InspectNetwork` the name. On success: with no `spec.Subnet` return nil; when subnet, gateway and range all equal, return nil; otherwise return `&NetworkMismatchError{Name: spec.Name, Have: info, Want: spec}`. On `client.IsErrNotFound(err)`, create: `network.CreateOptions{Driver: "bridge", Options: bridgeOptions(name)}`, and when a subnet is set add `IPAM: &network.IPAM{Driver: "default", Config: []network.IPAMConfig{{Subnet, Gateway, IPRange}}}`. Any other error is returned.
- `real.InspectNetwork`: `NetworkInspect`; take the first IPAM config whose `Subnet` is non-empty and has no `:`; fill `IPv4s` from `Containers`, keyed by `Name`, stripping the `/prefix` from `IPv4Address`.
- Rewrite `real.NetworkGateway` to call `InspectNetwork` and return `info.Gateway`, keeping today's error for a missing IPv4 gateway. Its tests must pass unchanged.
- `real.CreateContainer`: for the first network only, when `spec.IPv4 != ""`, set `EndpointSettings.IPAMConfig = &network.EndpointIPAMConfig{IPv4Address: spec.IPv4}`.
- `NetworkMismatchError.Error`: `network %q has %s; the settings ask for %s`, where a helper says `a subnet Docker chose` for an empty subnet, `subnet %s and gateway %s, with no dynamic range` when the range is empty, and `subnet %s, gateway %s and range %s` otherwise.
- `Fake.EnsureNetwork`: append to `NetworkSpecs`; treat a name in `Networks` or `NetworkDetails` as existing. With no subnet, mark it existing and return nil. With a subnet, compare the three values and return a `*NetworkMismatchError` on any difference. A missing name is recorded in both `Networks` and `NetworkDetails`, with the spec's three values. Add `NetworkDetails: map[string]NetworkInfo{}` to `NewFake`.
- `Fake.InspectNetwork`: an error when the name is in neither map; otherwise the `NetworkDetails` entry. `Fake.CreateContainer`: when the first network exists and `spec.IPv4 != ""`, record it in that network's `IPv4s`. `Fake.NetworkGateway` treats either map as existing, so a test that seeds only `NetworkDetails` still reads its gateway.
- `NetworkFor`: name only for an empty subnet; otherwise `PlanSubnet` and its canonical subnet, gateway and range.
- Update the four callers to pass `docker.NetworkSpec{Name: ...}` with no other change.

- [ ] **Step 4: Run everything**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: PASS, golden files unmodified.

- [ ] **Step 5: Commit**

```bash
git commit -am "feat(docker): create and inspect a fixed-subnet network, and pin a container's address"
```

---

## Task 4: Traefik at a fixed address

**Files:**
- Modify: `internal/traefik/proxy.go:36-93,113-176,226-361`, `internal/traefik/proxy_test.go`
- Test: `internal/traefik/proxy_test.go`

**Interfaces:**
- Consumes: `docker.NetworkFor`, `docker.InspectNetwork`, `docker.NetworkMismatchError` from Task 3.
- Produces:

```go
// traefik.ProxyConfig gains
Subnet string // OUTHAUL_NETWORK_SUBNET; "" lets Docker choose
IPv4   string // Traefik's fixed address; "" lets Docker choose
```

- [ ] **Step 1: Write the failing tests**

```go
// TestProxySpecCarriesTheAddress: proxySpec(testProxyConfig() with Subnet "172.30.0.0/24" and
//   IPv4 "172.30.0.2").IPv4 == "172.30.0.2".
// TestProxyHashChangesWithTheAddress: pc with IPv4 "" and the same pc with "172.30.0.2" have different
//   outhaul.config-hash values; changing IPv4 changes it; Subnet alone does not.
//   TestProxySpecGolden and TestProxyUnchangedWithoutProtectedRoutes must pass with testdata unmodified,
//   which pins the no-address hash to today's.
// TestEnsureProxyUsesTheNetworkSpec: EnsureProxy with Subnet "172.30.0.0/24", IPv4 "172.30.0.2";
//   f.NetworkSpecs[0] == docker.NetworkSpec{Name: "outhaul", Subnet: "172.30.0.0/24",
//   Gateway: "172.30.0.1", IPRange: "172.30.0.128/25"}; the created Traefik's IPv4 is "172.30.0.2".
// TestEnsureProxyRefusesAnAddressAnotherContainerHolds: a fake whose "outhaul" network already
//   has the right subnet and range and whose IPv4s["other"] is "172.30.0.2"; EnsureProxy with IPv4
//   ".2" returns an error containing "172.30.0.2" and "other", and f.Created stays empty.
// TestEnsureProxyKeepsTheOldTraefikWhenTheAddressIsHeld: create Traefik with the subnet and no IPv4;
//   seed "other" at ".2"; add IPv4 ".2" and EnsureProxy; it errors; the old Traefik is still Running().
// TestEnsureProxyKeepsItsOwnAddress: create Traefik with Subnet and IPv4; change Image so the hash
//   differs; EnsureProxy succeeds (the old Traefik holds the address by the same name), and the new
//   Traefik's IPv4 is ".2".
```

- [ ] **Step 2: Run them to see them fail**

Run: `go test ./internal/traefik/ -run 'Address|NetworkSpec'`
Expected: FAIL (unknown fields).

- [ ] **Step 3: Implement**

- `proxySpec`: set `IPv4: pc.IPv4` on the returned spec.
- `hashConfig`: add a last `ipv4 string` parameter; write `|ip4=<address>` after the tunnel line only when it is non-empty. Pass `pc.IPv4` from `proxySpec`.
- `EnsureProxy`: replace the bare name with `docker.NetworkFor(pc.Network, pc.Subnet)`, wrapping an error as `network settings: %w`; call `EnsureNetwork` with it.
- After the adopt-or-recreate decision and before any pull, when `pc.IPv4 != ""`: `InspectNetwork`, and for each `IPv4s` entry whose address equals `pc.IPv4` and whose name is not `pc.ContainerName`, return `Traefik's address <ip> on network <name> is held by container <other>; stop or remove it, or set OUTHAUL_TRAEFIK_IP to another address`. An inspect failure is wrapped as `check who holds Traefik's address <ip>: %w`.

- [ ] **Step 4: Run everything**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: PASS, `internal/traefik/testdata` unmodified.

- [ ] **Step 5: Commit**

```bash
git commit -am "feat(traefik): put Traefik at a fixed address on the shared network"
```

---

## Task 5: The connector on the fixed network

**Files:**
- Modify: `internal/tunnel/connector.go:24-40`, `internal/tunnel/connector_test.go`
- Test: `internal/tunnel/connector_test.go`

**Interfaces:**
- Consumes: `docker.NetworkFor` from Task 3.
- Produces:

```go
// tunnel.ConnectorConfig gains
Subnet string // the shared network's fixed subnet; "" lets Docker choose
```

- [ ] **Step 1: Write the failing tests**

```go
// TestEnsureConnectorUsesTheNetworkSpec: cc.Subnet "172.30.0.0/24"; after EnsureConnector,
//   f.NetworkSpecs[0] == {Name: "outhaul", Subnet: "172.30.0.0/24", Gateway: "172.30.0.1",
//   IPRange: "172.30.0.128/25"}; the connector spec has no IPv4.
// TestEnsureConnectorFailsOnANetworkMismatch: f.NetworkDetails["outhaul"] holds 10.0.0.0/24;
//   EnsureConnector returns an error containing both subnets and creates no connector.
```

- [ ] **Step 2: Run them to see them fail**

Run: `go test ./internal/tunnel/ -run 'NetworkSpec|NetworkMismatch'`
Expected: FAIL (unknown field).

- [ ] **Step 3: Implement**

Replace `dc.EnsureNetwork(ctx, cc.Network)` with `docker.NetworkFor(cc.Network, cc.Subnet)` and the spec; wrap a derivation error as `network settings: %w` and an ensure error as today. Leave the connector's hash and spec otherwise unchanged.

- [ ] **Step 4: Run everything**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git commit -am "feat(tunnel): create the connector's network with the fixed subnet"
```

---

## Task 6: Fail before startup, and log the layout

**Files:**
- Modify: `main.go:191-198` (the call in `serve`), `main.go:525-575` (reconcile), `main.go:603-636` (`proxyConfig`), `main_test.go:583-591` (`countingNetwork`)
- Test: `main_test.go`

**Interfaces:**
- Consumes: everything above.
- Produces:

```go
// ensureFixedNetwork applies OUTHAUL_NETWORK_SUBNET before anything else runs.
// It returns nil at once when the setting is unset.
func ensureFixedNetwork(ctx context.Context, dc docker.Client, cfg config.Config) error

// fixedNetworkLine is the one-line summary serve logs after the network is
// ready: the configured values and what was derived from them.
func fixedNetworkLine(cfg config.Config, plan docker.SubnetPlan) string
```

- [ ] **Step 1: Write the failing tests**

```go
// TestStartupCreatesTheFixedNetwork: empty fake, cfg{Network: "outhaul",
//   NetworkSubnet: "172.30.0.0/24", TraefikIP: "172.30.0.2"}; ensureFixedNetwork returns nil;
//   f.NetworkSpecs == [{outhaul 172.30.0.0/24 172.30.0.1 172.30.0.128/25}]; f.Networks["outhaul"].
// TestStartupRefusesADifferentNetwork: f.Networks and f.NetworkDetails hold 10.0.0.0/24, gateway
//   10.0.0.1, range 10.0.0.128/25; the error contains "172.30.0.0/24", "10.0.0.0/24",
//   "Outhaul won't change a network" and "docs/SHARED-HOST.md".
// TestStartupFailsWhenDockerIsUnreachable: a pingFails wrapper; the error contains "can't be checked"
//   and the wrapper's message.
// TestStartupFailsWhenTheNetworkCannotBeCreated: a networkFails wrapper returning
//   "Pool overlaps with other one on this address space"; the error contains "OUTHAUL_NETWORK_SUBNET",
//   "172.30.0.0/24" and that message.
// TestNoSubnetLeavesTheNetworkToDocker: cfg with no subnet and a fake holding a different network;
//   nil, and f.NetworkSpecs is empty.
// TestFixedNetworkLine, plan from PlanSubnet("172.30.0.0/24"):
//   network "outhaul", TraefikIP "172.30.0.2" ->
//     `network "outhaul": subnet 172.30.0.0/24, gateway 172.30.0.1, dynamic range 172.30.0.128/25, Traefik at 172.30.0.2, host bridge "outhaul"`
//   no TraefikIP -> "Traefik at an address Docker picks" in place of "Traefik at 172.30.0.2"
//   network "outhaul-networks" -> "host bridge named by Docker (the network name is longer than 15 bytes)"
// TestProxyConfigCarriesTheFixedNetwork: ic.proxyConfig's Subnet and IPv4 equal the config's.
// TestReconcileCarriesTheFixedNetwork: a stored tunnel token; cfg with the subnet and address;
//   after reconcile every recorded NetworkSpec named "outhaul" has 172.30.0.1 / 172.30.0.128/25;
//   the Traefik spec's IPv4 is "172.30.0.2"; the connector's is "".
```

Add `pingFails` and `networkFails` wrappers beside `countingNetwork`, and change `countingNetwork.EnsureNetwork` to the new signature.

- [ ] **Step 2: Run them to see them fail**

Run: `go test . -run 'FixedNetwork|Startup|ReconcileCarries'`
Expected: FAIL (undefined `ensureFixedNetwork`, `fixedNetworkLine`).

- [ ] **Step 3: Implement**

- `ensureFixedNetwork`: return nil when `cfg.NetworkSubnet == ""`; `PlanSubnet` the value; `dc.Ping` with `OUTHAUL_NETWORK_SUBNET=<value>: network <name> can't be checked because Docker can't be reached: <err>`; construct `docker.NetworkSpec{Name: cfg.Network, Subnet: plan.Subnet, Gateway: plan.Gateway, IPRange: plan.IPRange}` and call `EnsureNetwork`. Wrap a mismatch as `<mismatch error>; Outhaul won't change a network while containers are on it: see docs/SHARED-HOST.md for the steps to move it` and any other ensure error as `OUTHAUL_NETWORK_SUBNET=<value>: ensure network <name>: <err>`. Log `fixedNetworkLine` on success and return nil.
- `fixedNetworkLine`: `network %q: subnet %s, gateway %s, dynamic range %s, %s, %s`; the fourth is `Traefik at <ip>` or `Traefik at an address Docker picks`; the fifth is `host bridge %q` when `len(cfg.Network) <= 15`, else `host bridge named by Docker (the network name is longer than 15 bytes)`.
- `serve`: right after `defer dc.Close()`, run `ensureFixedNetwork` with a 60-second `context.Background()` timeout and return its error.
- `proxyConfig`: set `Subnet: ic.cfg.NetworkSubnet`, `IPv4: ic.cfg.TraefikIP`.
- `reconcile`: the tunnel connector's config gets `Subnet: ic.cfg.NetworkSubnet`; the `!AdminDirect` block's `EnsureNetwork` uses `docker.NetworkFor(ic.cfg.Network, ic.cfg.NetworkSubnet)`.

- [ ] **Step 4: Run everything**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: PASS, no existing `main_test.go` assertion changed except the wrapper's signature.

- [ ] **Step 5: Commit**

```bash
git commit -am "feat: check the fixed network before startup, and log its layout"
```

---

## Task 7: The docs

**Files:**
- Modify: `docs/SHARED-HOST.md`, `ARCHITECTURE.md` (after the "Behind a TLS-terminating proxy" section, before "Package layout"), `AGENTS.md` ("Decisions to keep")

- [ ] **Step 1: Finish `docs/SHARED-HOST.md`**

Add, in the plain style of `docs/BEHIND-A-PROXY.md`:

- **Why a fixed address.** The metadata service at `169.254.169.254`, the token it hands out, and the firewall rule that will admit only Traefik. Docker picks the subnet and every address today.
- **Starting up.** With a subnet set, `serve` checks first and refuses in the three cases from the spec, naming the journal; with none set, nothing changes. Quote the log line as it appears.
- **Moving an existing install.** The tested path from the spec's check 5, as commands: `systemctl stop outhaul`; `docker rm -f outhaul-traefik`; list the network's containers with `docker network inspect outhaul --format '{{range .Containers}}{{.Name}}{{"\n"}}{{end}}'`; `docker network disconnect outhaul <name>` for each; `docker network rm outhaul`; set both variables in `/etc/outhaul.env` (or the installer's drop-in, see `docs/MANUAL-DEPLOY.md`); `systemctl start outhaul`; `docker network connect outhaul <name>` for each. Say that disconnected containers keep running, that Traefik must be recreated (it keeps its old attachment otherwise), and that a Compose stack's published services can instead be reattached by `docker compose up -d`.
- **The firewall rule.** An example, marked as the infrastructure's rule, not Outhaul's, and saying to replace the address with the configured `OUTHAUL_TRAEFIK_IP`:
  ```
  iptables -I DOCKER-USER -d 169.254.169.254 ! -s 172.30.0.2 -j DROP
  ```
  and a sentence that it matches forwarded traffic on every bridge, so only Traefik's address reaches the metadata service.
- **What runs where.** The spec's table: Traefik (shared, fixed), cloudflared (shared, dynamic), single-container apps and deploy health checks (shared, dynamic), databases (shared, dynamic), Compose stacks (their own network; published services also shared), previews (as above), backup/restore/data-removal helpers (default bridge), image builds (default bridge), and Outhaul itself (host, so the rule doesn't see it).
- **Checking the real host.** The spec's checks 1, 2 and 4: the fixed address after install, after `docker rm -f outhaul-traefik` and an Outhaul restart, and after an update; after `sudo reboot` and `systemctl restart docker`; and a second subnet in the environment file refusing startup with the journal naming it.

- [ ] **Step 2: Add the architecture section**

In `ARCHITECTURE.md`, `### A fixed network and Traefik address (opt-in)`: the two settings; `PlanSubnet`'s gateway/range/fixed rule; `EnsureNetwork` creating or refusing, never recreating; the address on the first network; the hash only when set; the holder check before replacing; and the startup failure only when a subnet is set.

- [ ] **Step 3: Add the AGENTS.md decision**

Under `### Decisions to keep`:

```markdown
- **The fixed network.** `OUTHAUL_NETWORK_SUBNET` gives Docker the gateway
  (the subnet's first address) and the upper half as its dynamic range;
  `OUTHAUL_TRAEFIK_IP` must be in the lower half, after the gateway. Outhaul
  never recreates a network that exists with another subnet: startup says so
  and the docs give the move.
```

- [ ] **Step 4: Verify the whole plan**

Run: `go build ./... && go vet ./... && git diff --check && go test -race -timeout 30m ./...`
Expected: PASS; the golden files under `internal/traefik/testdata` are unmodified.

- [ ] **Step 5: Commit**

```bash
git add docs/SHARED-HOST.md ARCHITECTURE.md AGENTS.md
git commit -m "docs: the fixed subnet and Traefik address on a shared host"
```
