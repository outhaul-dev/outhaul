package docker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
)

func TestStatsFromAPICPU(t *testing.T) {
	raw := container.StatsResponse{
		CPUStats: container.CPUStats{
			CPUUsage:    container.CPUUsage{TotalUsage: 400},
			SystemUsage: 2000,
			OnlineCPUs:  4,
		},
		PreCPUStats: container.CPUStats{
			CPUUsage:    container.CPUUsage{TotalUsage: 200},
			SystemUsage: 1000,
		},
	}
	// delta 200 of system delta 1000, scaled by 4 cores: 80%.
	if got := statsFromAPI(raw).CPUPercent; got != 80 {
		t.Errorf("CPUPercent = %v, want 80", got)
	}

	// No OnlineCPUs (older daemons): fall back to the per-CPU slice length.
	raw.CPUStats.OnlineCPUs = 0
	raw.CPUStats.CPUUsage.PercpuUsage = []uint64{1, 2}
	if got := statsFromAPI(raw).CPUPercent; got != 40 {
		t.Errorf("CPUPercent with percpu fallback = %v, want 40", got)
	}

	// A first-ever sample has zero deltas; that must read 0, not NaN.
	if got := statsFromAPI(container.StatsResponse{}).CPUPercent; got != 0 {
		t.Errorf("CPUPercent with zero deltas = %v, want 0", got)
	}
}

func TestStatsFromAPIMemory(t *testing.T) {
	// cgroup v1 exposes total_inactive_file; docker stats subtracts it.
	raw := container.StatsResponse{MemoryStats: container.MemoryStats{
		Usage: 1000, Limit: 4000,
		Stats: map[string]uint64{"total_inactive_file": 300},
	}}
	s := statsFromAPI(raw)
	if s.MemUsage != 700 || s.MemLimit != 4000 {
		t.Errorf("mem = %d/%d, want 700/4000", s.MemUsage, s.MemLimit)
	}

	// cgroup v2 names it inactive_file.
	raw.MemoryStats.Stats = map[string]uint64{"inactive_file": 250}
	if got := statsFromAPI(raw).MemUsage; got != 750 {
		t.Errorf("mem (cgroup v2) = %d, want 750", got)
	}

	// A cache counter larger than usage must not underflow.
	raw.MemoryStats.Stats = map[string]uint64{"inactive_file": 5000}
	if got := statsFromAPI(raw).MemUsage; got != 1000 {
		t.Errorf("mem (oversized cache counter) = %d, want 1000", got)
	}
}

func TestStatsFromAPINetworkSums(t *testing.T) {
	raw := container.StatsResponse{Networks: map[string]container.NetworkStats{
		"eth0": {RxBytes: 100, TxBytes: 10},
		"eth1": {RxBytes: 200, TxBytes: 20},
	}}
	s := statsFromAPI(raw)
	if s.NetRx != 300 || s.NetTx != 30 {
		t.Errorf("net = %d/%d, want 300/30", s.NetRx, s.NetTx)
	}
}

func TestFakeContainerStats(t *testing.T) {
	ctx := context.Background()
	f := NewFake()
	id, _ := f.CreateContainer(ctx, ContainerSpec{Name: "web", Image: "img"})
	f.Stats[id] = Stats{CPUPercent: 12.5, MemUsage: 42}

	s, err := f.ContainerStats(ctx, id)
	if err != nil {
		t.Fatalf("ContainerStats: %v", err)
	}
	if s.CPUPercent != 12.5 || s.MemUsage != 42 {
		t.Errorf("stats = %+v", s)
	}
	if _, err := f.ContainerStats(ctx, "missing"); err == nil {
		t.Error("expected error for unknown container, mirroring the daemon")
	}
}

// statsFromDaemon runs ContainerStats against a fake daemon answering one
// stats sample and one inspect, as the real client asks for both.
func statsFromDaemon(t *testing.T, inspect, sample string) Stats {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/stats"):
			w.Write([]byte(sample))
		case strings.HasSuffix(r.URL.Path, "/json"):
			w.Write([]byte(inspect))
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()
	cli, err := client.NewClientWithOpts(client.WithHost("tcp://"+ts.Listener.Addr().String()), client.WithVersion("1.47"))
	if err != nil {
		t.Fatal(err)
	}
	s, err := (&real{cli: cli}).ContainerStats(context.Background(), "c1")
	if err != nil {
		t.Fatalf("ContainerStats: %v", err)
	}
	return s
}

func TestContainerStatsReadsLimits(t *testing.T) {
	s := statsFromDaemon(t,
		`{"State":{"StartedAt":"2026-11-11T00:00:00Z"},"HostConfig":{"NanoCpus":500000000,"Memory":1073741824,"PidsLimit":512}}`,
		`{"pids_stats":{"current":12}}`)
	if s.CPULimit != 0.5 {
		t.Errorf("CPULimit = %v, want 0.5", s.CPULimit)
	}
	if !s.MemLimited {
		t.Error("MemLimited = false, want true")
	}
	if s.Pids != 12 {
		t.Errorf("Pids = %d, want 12", s.Pids)
	}
	if s.PidsLimit != 512 {
		t.Errorf("PidsLimit = %d, want 512", s.PidsLimit)
	}
}

func TestContainerStatsReadsLegacyCPUQuota(t *testing.T) {
	s := statsFromDaemon(t,
		`{"State":{"StartedAt":"2026-11-11T00:00:00Z"},"HostConfig":{"CpuQuota":50000,"CpuPeriod":100000}}`,
		`{}`)
	if s.CPULimit != 0.5 {
		t.Errorf("CPULimit from quota/period = %v, want 0.5", s.CPULimit)
	}
	if s.MemLimited {
		t.Error("MemLimited = true with no memory limit")
	}
	if s.PidsLimit != 0 {
		t.Errorf("PidsLimit = %d, want 0", s.PidsLimit)
	}
}
