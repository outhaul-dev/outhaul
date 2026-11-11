package docker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/docker/docker/client"
)

// createdNetworkOptions runs EnsureNetwork against a daemon that has no
// networks yet, and returns the driver options of the network it creates.
func createdNetworkOptions(t *testing.T, name string) map[string]string {
	t.Helper()
	var opts map[string]string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/networks"):
			w.Write([]byte(`[]`))
		case strings.HasSuffix(r.URL.Path, "/networks/create"):
			var body struct{ Options map[string]string }
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode create body: %v", err)
			}
			opts = body.Options
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"Id":"n1"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()
	cli, err := client.NewClientWithOpts(client.WithHost("tcp://"+ts.Listener.Addr().String()), client.WithVersion("1.47"))
	if err != nil {
		t.Fatal(err)
	}
	if err := (&real{cli: cli}).EnsureNetwork(context.Background(), name); err != nil {
		t.Fatalf("EnsureNetwork: %v", err)
	}
	return opts
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
