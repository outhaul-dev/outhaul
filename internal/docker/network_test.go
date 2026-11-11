package docker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/docker/docker/client"
)

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
