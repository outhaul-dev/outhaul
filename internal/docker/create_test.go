package docker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
)

// createdHostConfig runs CreateContainer against a daemon that records the
// HostConfig of the create request.
func createdHostConfig(t *testing.T, spec ContainerSpec) container.HostConfig {
	t.Helper()
	var got container.CreateRequest
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/containers/create") {
			http.NotFound(w, r)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode create body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"Id":"c1"}`))
	}))
	defer ts.Close()
	cli, err := client.NewClientWithOpts(client.WithHost("tcp://"+ts.Listener.Addr().String()), client.WithVersion("1.47"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (&real{cli: cli}).CreateContainer(context.Background(), spec); err != nil {
		t.Fatalf("CreateContainer: %v", err)
	}
	if got.HostConfig == nil {
		t.Fatal("create request carried no HostConfig")
	}
	return *got.HostConfig
}

func TestCreateContainerSendsResources(t *testing.T) {
	host := createdHostConfig(t, ContainerSpec{
		Name:      "web",
		Image:     "img",
		NanoCPUs:  500_000_000,
		Memory:    1 << 30,
		PidsLimit: 512,
	})
	if host.Resources.NanoCPUs != 500_000_000 {
		t.Errorf("NanoCPUs = %d, want 500000000", host.Resources.NanoCPUs)
	}
	if host.Resources.Memory != 1<<30 {
		t.Errorf("Memory = %d, want %d", host.Resources.Memory, 1<<30)
	}
	if host.Resources.PidsLimit == nil || *host.Resources.PidsLimit != 512 {
		t.Errorf("PidsLimit = %v, want 512", host.Resources.PidsLimit)
	}
	if host.Resources.MemorySwap != 0 || host.Resources.CPUPeriod != 0 || host.Resources.CPUQuota != 0 {
		t.Errorf("unasked-for resources set: swap %d, period %d, quota %d", host.Resources.MemorySwap, host.Resources.CPUPeriod, host.Resources.CPUQuota)
	}
}

func TestCreateContainerWithoutLimitsSendsNoResources(t *testing.T) {
	host := createdHostConfig(t, ContainerSpec{Name: "web", Image: "img"})
	r := host.Resources
	if r.NanoCPUs != 0 || r.Memory != 0 || r.PidsLimit != nil {
		t.Errorf("resources = %+v, want none", r)
	}
}
