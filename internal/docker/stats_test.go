package docker

import (
	"context"
	"testing"

	"github.com/docker/docker/api/types/container"
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
