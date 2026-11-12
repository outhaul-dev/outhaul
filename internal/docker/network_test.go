package docker

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	dockernetwork "github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
)

// newCreateRecordingDaemon answers every network inspect with status and body,
// and records the bodies of any create requests.
func newCreateRecordingDaemon(t *testing.T, status int, body string) (*real, *[]dockernetwork.CreateOptions) {
	t.Helper()
	var creates []dockernetwork.CreateOptions
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/networks/"):
			w.WriteHeader(status)
			w.Write([]byte(body))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/networks/create"):
			var opts dockernetwork.CreateOptions
			if err := json.NewDecoder(r.Body).Decode(&opts); err != nil {
				t.Errorf("decode create body: %v", err)
			}
			creates = append(creates, opts)
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"Id":"n1"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ts.Close)
	cli, err := client.NewClientWithOpts(client.WithHost("tcp://"+ts.Listener.Addr().String()), client.WithVersion("1.47"))
	if err != nil {
		t.Fatal(err)
	}
	return &real{cli: cli}, &creates
}

// createdNetworkOptions runs EnsureNetwork against a daemon that has no
// networks yet, and returns the driver options of the network it creates.
func createdNetworkOptions(t *testing.T, name string) map[string]string {
	t.Helper()
	r, creates := newCreateRecordingDaemon(t, http.StatusNotFound, `{"message":"not found"}`)
	if err := r.EnsureNetwork(context.Background(), NetworkSpec{Name: name}); err != nil {
		t.Fatalf("EnsureNetwork: %v", err)
	}
	if len(*creates) != 1 {
		t.Fatalf("create requests = %d, want 1", len(*creates))
	}
	return (*creates)[0].Options
}

// A firewall rule has to name the interface Traefik's traffic to the admin
// UI arrives on. Docker's default br-<id> changes whenever the network is
// recreated, so the bridge is named after the network.
func TestEnsureNetworkNamesTheBridge(t *testing.T) {
	got := createdNetworkOptions(t, "outhaul")["com.docker.network.bridge.name"]
	if got != "outhaul" {
		t.Fatalf("bridge name = %q, want %q", got, "outhaul")
	}
}

// Linux interface names are at most 15 bytes; a longer network name keeps
// Docker's default bridge name rather than failing to create.
func TestEnsureNetworkLeavesLongNamesToDocker(t *testing.T) {
	if got := createdNetworkOptions(t, "outhaul-network")["com.docker.network.bridge.name"]; got != "outhaul-network" {
		t.Fatalf("15-byte name: bridge name = %q, want %q", got, "outhaul-network")
	}
	if got, ok := createdNetworkOptions(t, "outhaul-networks")["com.docker.network.bridge.name"]; ok {
		t.Fatalf("16-byte name: bridge name = %q, want Docker's default", got)
	}
}

func TestEnsureNetworkCreatesWithSubnet(t *testing.T) {
	r, creates := newCreateRecordingDaemon(t, http.StatusNotFound, `{"message":"not found"}`)
	spec := NetworkSpec{
		Name: "outhaul", Subnet: "172.30.0.0/24",
		Gateway: "172.30.0.1", IPRange: "172.30.0.128/25",
	}
	if err := r.EnsureNetwork(context.Background(), spec); err != nil {
		t.Fatalf("EnsureNetwork: %v", err)
	}
	if len(*creates) != 1 {
		t.Fatalf("create requests = %d, want 1", len(*creates))
	}
	got := (*creates)[0]
	if got.Driver != "bridge" {
		t.Errorf("driver = %q, want %q", got.Driver, "bridge")
	}
	if opts := got.Options["com.docker.network.bridge.name"]; opts != "outhaul" {
		t.Errorf("bridge name = %q, want %q", opts, "outhaul")
	}
	if got.IPAM == nil {
		t.Fatal("IPAM = nil, want the subnet, gateway and range")
	}
	if got.IPAM.Driver != "default" {
		t.Errorf("IPAM driver = %q, want %q", got.IPAM.Driver, "default")
	}
	if len(got.IPAM.Config) != 1 {
		t.Fatalf("IPAM configs = %d, want 1", len(got.IPAM.Config))
	}
	cfg := got.IPAM.Config[0]
	if cfg.Subnet != spec.Subnet || cfg.Gateway != spec.Gateway || cfg.IPRange != spec.IPRange {
		t.Errorf("IPAM config = %+v, want subnet %q, gateway %q, range %q",
			cfg, spec.Subnet, spec.Gateway, spec.IPRange)
	}
}

func TestEnsureNetworkWithoutSubnetIsTodaysRequest(t *testing.T) {
	r, creates := newCreateRecordingDaemon(t, http.StatusNotFound, `{"message":"not found"}`)
	if err := r.EnsureNetwork(context.Background(), NetworkSpec{Name: "outhaul"}); err != nil {
		t.Fatalf("EnsureNetwork: %v", err)
	}
	if len(*creates) != 1 {
		t.Fatalf("create requests = %d, want 1", len(*creates))
	}
	got := (*creates)[0]
	if got.IPAM != nil {
		t.Errorf("IPAM = %+v, want nil so Docker chooses the subnet", got.IPAM)
	}
	if opts := got.Options["com.docker.network.bridge.name"]; opts != "outhaul" {
		t.Errorf("bridge name = %q, want %q", opts, "outhaul")
	}
}

func TestEnsureNetworkAdoptsMatching(t *testing.T) {
	r, creates := newCreateRecordingDaemon(t, http.StatusOK,
		`{"Name":"outhaul","IPAM":{"Config":[{"Subnet":"172.30.0.0/24","Gateway":"172.30.0.1","IPRange":"172.30.0.128/25"}]}}`)
	spec := NetworkSpec{
		Name: "outhaul", Subnet: "172.30.0.0/24",
		Gateway: "172.30.0.1", IPRange: "172.30.0.128/25",
	}
	if err := r.EnsureNetwork(context.Background(), spec); err != nil {
		t.Fatalf("EnsureNetwork: %v", err)
	}
	if len(*creates) != 0 {
		t.Fatalf("create requests = %d, want none for a matching network", len(*creates))
	}
}

func TestEnsureNetworkWithoutSubnetAdoptsAnything(t *testing.T) {
	r, creates := newCreateRecordingDaemon(t, http.StatusOK,
		`{"Name":"outhaul","IPAM":{"Config":[{"Subnet":"10.0.0.0/24","Gateway":"10.0.0.1"}]}}`)
	if err := r.EnsureNetwork(context.Background(), NetworkSpec{Name: "outhaul"}); err != nil {
		t.Fatalf("EnsureNetwork: %v", err)
	}
	if len(*creates) != 0 {
		t.Fatalf("create requests = %d, want none for an existing network", len(*creates))
	}
}

func TestEnsureNetworkRefusesADifferentNetwork(t *testing.T) {
	want := NetworkSpec{
		Name: "outhaul", Subnet: "172.30.0.0/24",
		Gateway: "172.30.0.1", IPRange: "172.30.0.128/25",
	}
	r, creates := newCreateRecordingDaemon(t, http.StatusOK,
		`{"Name":"outhaul","IPAM":{"Config":[{"Subnet":"10.0.0.0/24","Gateway":"10.0.0.1","IPRange":"10.0.0.128/25"}]}}`)
	err := r.EnsureNetwork(context.Background(), want)
	var mismatch *NetworkMismatchError
	if !errors.As(err, &mismatch) {
		t.Fatalf("EnsureNetwork error = %v, want a *NetworkMismatchError", err)
	}
	msg := err.Error()
	for _, part := range []string{
		"10.0.0.0/24", "172.30.0.0/24",
		"10.0.0.1", "172.30.0.1",
		"10.0.0.128/25", "172.30.0.128/25",
	} {
		if !strings.Contains(msg, part) {
			t.Errorf("error = %q, want it to contain %q", msg, part)
		}
	}
	if len(*creates) != 0 {
		t.Errorf("create requests = %d, want none for a mismatching network", len(*creates))
	}

	empty, _ := newCreateRecordingDaemon(t, http.StatusOK, `{"Name":"outhaul","IPAM":{"Config":[]}}`)
	err = empty.EnsureNetwork(context.Background(), want)
	if !errors.As(err, &mismatch) {
		t.Fatalf("empty IPAM: EnsureNetwork error = %v, want a *NetworkMismatchError", err)
	}
	if !strings.Contains(err.Error(), "Docker chose") {
		t.Errorf("empty IPAM: error = %q, want it to say Docker chose the subnet", err.Error())
	}
}

func TestInspectNetwork(t *testing.T) {
	r := newNetworkDaemon(t, "/networks/outhaul", http.StatusOK,
		`{"Name":"outhaul","IPAM":{"Config":[
			{"Subnet":"fd00::/64","Gateway":"fd00::1"},
			{"Subnet":"172.30.0.0/24","Gateway":"172.30.0.1","IPRange":"172.30.0.128/25"}
		]},"Containers":{"id1":{"Name":"outhaul-traefik","IPv4Address":"172.30.0.2/24"},"id2":{"Name":"ipv6-only","IPv4Address":""}}}`)
	info, err := r.InspectNetwork(context.Background(), "outhaul")
	if err != nil {
		t.Fatalf("InspectNetwork: %v", err)
	}
	if info.Subnet != "172.30.0.0/24" || info.Gateway != "172.30.0.1" || info.IPRange != "172.30.0.128/25" {
		t.Errorf("InspectNetwork = %+v, want the IPv4 subnet, gateway and range", info)
	}
	if got := info.IPv4s["outhaul-traefik"]; got != "172.30.0.2" {
		t.Errorf("IPv4s[outhaul-traefik] = %q, want %q", got, "172.30.0.2")
	}
	if _, present := info.IPv4s["ipv6-only"]; present {
		t.Errorf("IPv4s = %v, want a container with no IPv4 address left out", info.IPv4s)
	}
	if len(info.IPv4s) != 1 {
		t.Errorf("IPv4s = %v, want exactly the one attached container", info.IPv4s)
	}
}

func TestCreateContainerFixedAddress(t *testing.T) {
	type endpoint struct {
		IPAMConfig *struct{ IPv4Address string }
	}
	type createBody struct {
		NetworkingConfig struct {
			EndpointsConfig map[string]endpoint
		}
	}
	var bodies []createBody
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/containers/create") {
			http.NotFound(w, r)
			return
		}
		var body createBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode create body: %v", err)
		}
		bodies = append(bodies, body)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"Id":"c1"}`))
	}))
	t.Cleanup(ts.Close)
	cli, err := client.NewClientWithOpts(client.WithHost("tcp://"+ts.Listener.Addr().String()), client.WithVersion("1.47"))
	if err != nil {
		t.Fatal(err)
	}
	r := &real{cli: cli}

	spec := ContainerSpec{Name: "outhaul-traefik", Image: "traefik:example", Networks: []string{"outhaul"}, IPv4: "172.30.0.2"}
	if _, err := r.CreateContainer(context.Background(), spec); err != nil {
		t.Fatalf("CreateContainer: %v", err)
	}
	spec.IPv4 = ""
	if _, err := r.CreateContainer(context.Background(), spec); err != nil {
		t.Fatalf("CreateContainer: %v", err)
	}
	if len(bodies) != 2 {
		t.Fatalf("create requests = %d, want 2", len(bodies))
	}
	ep := bodies[0].NetworkingConfig.EndpointsConfig["outhaul"]
	if ep.IPAMConfig == nil || ep.IPAMConfig.IPv4Address != "172.30.0.2" {
		t.Errorf("fixed endpoint = %+v, want IPv4Address %q", ep, "172.30.0.2")
	}
	if ep := bodies[1].NetworkingConfig.EndpointsConfig["outhaul"]; ep.IPAMConfig != nil {
		t.Errorf("empty IPv4 endpoint = %+v, want no IPAMConfig", ep.IPAMConfig)
	}
}

func TestFakeRecordsNetworkSpecs(t *testing.T) {
	f := NewFake()
	f.NetworkDetails["outhaul"] = NetworkInfo{
		Subnet: "10.0.0.0/24", Gateway: "10.0.0.1", IPRange: "10.0.0.128/25",
	}
	want := NetworkSpec{
		Name: "outhaul", Subnet: "172.30.0.0/24",
		Gateway: "172.30.0.1", IPRange: "172.30.0.128/25",
	}
	err := f.EnsureNetwork(context.Background(), want)
	var mismatch *NetworkMismatchError
	if !errors.As(err, &mismatch) {
		t.Fatalf("EnsureNetwork error = %v, want a *NetworkMismatchError", err)
	}
	if err := f.EnsureNetwork(context.Background(), NetworkSpec{Name: "outhaul"}); err != nil {
		t.Fatalf("EnsureNetwork without a subnet = %v, want nil", err)
	}

	fresh := NetworkSpec{
		Name: "shared", Subnet: "172.31.0.0/24",
		Gateway: "172.31.0.1", IPRange: "172.31.0.128/25",
	}
	if err := f.EnsureNetwork(context.Background(), fresh); err != nil {
		t.Fatalf("EnsureNetwork for a missing network = %v, want nil", err)
	}
	if !f.Networks["shared"] {
		t.Error("Networks[shared] = false, want the created network recorded")
	}
	if len(f.NetworkSpecs) != 3 {
		t.Fatalf("NetworkSpecs = %+v, want all three calls in order", f.NetworkSpecs)
	}
	if f.NetworkSpecs[2] != fresh {
		t.Errorf("NetworkSpecs[2] = %+v, want %+v", f.NetworkSpecs[2], fresh)
	}
	details := f.NetworkDetails["shared"]
	if details.Subnet != fresh.Subnet || details.Gateway != fresh.Gateway || details.IPRange != fresh.IPRange {
		t.Errorf("NetworkDetails[shared] = %+v, want the spec's values", details)
	}
}

func TestNetworkFor(t *testing.T) {
	got, err := NetworkFor("outhaul", "")
	if err != nil {
		t.Fatalf("NetworkFor with no subnet: %v", err)
	}
	if got != (NetworkSpec{Name: "outhaul"}) {
		t.Errorf("NetworkFor with no subnet = %+v, want the name only", got)
	}

	got, err = NetworkFor("outhaul", "172.30.0.0/24")
	if err != nil {
		t.Fatalf("NetworkFor: %v", err)
	}
	want := NetworkSpec{
		Name: "outhaul", Subnet: "172.30.0.0/24",
		Gateway: "172.30.0.1", IPRange: "172.30.0.128/25",
	}
	if got != want {
		t.Errorf("NetworkFor = %+v, want %+v", got, want)
	}

	if _, err := NetworkFor("outhaul", "fd00::/64"); err == nil {
		t.Error("NetworkFor with an IPv6 subnet should be an error")
	}
}

func TestFakeNetworkDetailsAndFixedAddress(t *testing.T) {
	f := NewFake()
	if _, err := f.InspectNetwork(context.Background(), "missing"); err == nil {
		t.Error("InspectNetwork for an unknown network should be an error")
	}
	spec := NetworkSpec{
		Name: "shared", Subnet: "172.31.0.0/24",
		Gateway: "172.31.0.1", IPRange: "172.31.0.128/25",
	}
	if err := f.EnsureNetwork(context.Background(), spec); err != nil {
		t.Fatalf("EnsureNetwork: %v", err)
	}
	if _, err := f.CreateContainer(context.Background(), ContainerSpec{
		Name: "outhaul-traefik", Image: "traefik:example",
		Networks: []string{"shared"}, IPv4: "172.31.0.2",
	}); err != nil {
		t.Fatalf("CreateContainer: %v", err)
	}
	info, err := f.InspectNetwork(context.Background(), "shared")
	if err != nil {
		t.Fatalf("InspectNetwork: %v", err)
	}
	if got := info.IPv4s["outhaul-traefik"]; got != "172.31.0.2" {
		t.Errorf("IPv4s[outhaul-traefik] = %q, want %q", got, "172.31.0.2")
	}
	if gw, err := f.NetworkGateway(context.Background(), "shared"); err != nil || gw != "172.31.0.1" {
		t.Errorf("NetworkGateway = %q, %v; want the network's gateway", gw, err)
	}
}

// newNetworkDaemon answers one network's inspect call; status and body are the
// test's.
func newNetworkDaemon(t *testing.T, path string, status int, body string) *real {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, path) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			w.Write([]byte(body))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(ts.Close)
	cli, err := client.NewClientWithOpts(client.WithHost("tcp://"+ts.Listener.Addr().String()), client.WithVersion("1.47"))
	if err != nil {
		t.Fatal(err)
	}
	return &real{cli: cli}
}

func TestNetworkGateway(t *testing.T) {
	r := newNetworkDaemon(t, "/networks/outhaul", http.StatusOK,
		`{"Name":"outhaul","IPAM":{"Config":[{"Subnet":"172.30.0.0/16","Gateway":"172.30.0.1"}]}}`)
	got, err := r.NetworkGateway(context.Background(), "outhaul")
	if err != nil || got != "172.30.0.1" {
		t.Errorf("NetworkGateway = %q, %v; want 172.30.0.1, nil", got, err)
	}

	notFound := newNetworkDaemon(t, "/networks/missing", http.StatusNotFound, `{"message":"not found"}`)
	if _, err := notFound.NetworkGateway(context.Background(), "missing"); err == nil {
		t.Error("a missing network should be an error")
	}

	noIPv4 := newNetworkDaemon(t, "/networks/outhaul", http.StatusOK,
		`{"Name":"outhaul","IPAM":{"Config":[{"Subnet":"fd00::/64","Gateway":"fd00::1"}]}}`)
	if _, err := noIPv4.NetworkGateway(context.Background(), "outhaul"); err == nil {
		t.Error("a network with no IPv4 gateway should be an error")
	}
}
