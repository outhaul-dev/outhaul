package server

import (
	"encoding/json"
	"testing"

	"github.com/james-smart/outhaul/internal/docker"
	"github.com/james-smart/outhaul/internal/hostmetrics"
)

// stubSampler returns fixed host/self values so the endpoint test is
// deterministic (the real sampler reads live /proc).
type stubSampler struct {
	host hostmetrics.Host
	self hostmetrics.Self
}

func (s stubSampler) Sample() (hostmetrics.Host, hostmetrics.Self) { return s.host, s.self }

func TestMetricsSampleJSON(t *testing.T) {
	const gib = 1024 * 1024 * 1024
	const mib = 1024 * 1024
	e := newTestEnv(t)
	e.completeSetup(t)

	e.srv.metrics = stubSampler{
		host: hostmetrics.Host{
			CPUPercent: 12.5,
			MemUsed:    2 * gib, MemTotal: 4 * gib,
			DiskUsed: 10 * gib, DiskTotal: 40 * gib,
			Load1: 0.4, Load5: 0.3, Load15: 0.2,
		},
		self: hostmetrics.Self{CPUPercent: 0.3, RSS: 18 * mib, Goroutines: 24, HeapAlloc: 6 * mib},
	}

	// Two managed containers + one foreign (must be filtered out). The managed
	// pair is listed OUT of order (db before app) so the assertions below only
	// hold if containerStats actually sorts by name.
	e.runtime.stack = []docker.Container{
		{ID: "b", Name: "outhaul-db-shop", State: "running"},
		{ID: "a", Name: "outhaul-app-web", State: "running"},
		{ID: "c", Name: "some-foreign", State: "running"},
	}
	e.runtime.stats = map[string]docker.Stats{
		"a": {CPUPercent: 2.0, MemUsage: 42 * mib, NetRx: 1024, NetTx: 512},
		"b": {CPUPercent: 1.0, MemUsage: 90 * mib},
		"c": {CPUPercent: 9.0},
	}

	resp := e.get(t, "/metrics/sample")
	if resp.StatusCode != 200 {
		t.Fatalf("GET /metrics/sample = %d, want 200", resp.StatusCode)
	}
	var m metricsSample
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if m.Host.CPU != "12.5" || m.Host.CPUPct != 12.5 {
		t.Errorf("host cpu = %q/%v", m.Host.CPU, m.Host.CPUPct)
	}
	if m.Host.MemSub != "of 4.0 GiB" || m.Host.MemPct != 50 {
		t.Errorf("host mem = %q sub=%q pct=%v", m.Host.Mem, m.Host.MemSub, m.Host.MemPct)
	}
	if m.Host.Load != "0.40 · 0.30 · 0.20" {
		t.Errorf("host load = %q", m.Host.Load)
	}
	if m.Self.Mem != "18 MiB" || m.Self.Goroutines != 24 || m.Self.Heap != "6.0 MiB" {
		t.Errorf("self = %+v", m.Self)
	}
	if len(m.Containers) != 2 {
		t.Fatalf("containers = %d, want 2 (foreign filtered out)", len(m.Containers))
	}
	// Sorted by name: outhaul-app-web then outhaul-db-shop.
	if m.Containers[0].Name != "outhaul-app-web" || m.Containers[0].Kind != "app" || m.Containers[0].Mem != "42 MiB" {
		t.Errorf("row0 = %+v", m.Containers[0])
	}
	if m.Containers[0].Net != "1.0 KiB ↓ · 512 B ↑" {
		t.Errorf("row0 net = %q", m.Containers[0].Net)
	}
	if m.Containers[1].Name != "outhaul-db-shop" || m.Containers[1].Kind != "database" {
		t.Errorf("row1 = %+v", m.Containers[1])
	}
}

// TestMetricsSampleSkipsUnsampledContainer exercises the out/ok two-slice
// design: a container that fails to sample (stopped mid-poll → ContainerStats
// error) is dropped without misaligning the surviving rows.
func TestMetricsSampleSkipsUnsampledContainer(t *testing.T) {
	const mib = 1024 * 1024
	e := newTestEnv(t)
	e.completeSetup(t)

	e.srv.metrics = stubSampler{}

	// Three running managed containers; the MIDDLE one (by ID) has no stats
	// entry, so ContainerStats returns an error for it.
	e.runtime.stack = []docker.Container{
		{ID: "a", Name: "outhaul-app-web", State: "running"},
		{ID: "b", Name: "outhaul-app-mid", State: "running"},
		{ID: "c", Name: "outhaul-db-shop", State: "running"},
	}
	e.runtime.stats = map[string]docker.Stats{
		"a": {CPUPercent: 2.0, MemUsage: 42 * mib},
		"c": {CPUPercent: 1.0, MemUsage: 90 * mib},
		// "b" omitted → ContainerStats("b") errors → row skipped.
	}

	resp := e.get(t, "/metrics/sample")
	if resp.StatusCode != 200 {
		t.Fatalf("GET /metrics/sample = %d, want 200", resp.StatusCode)
	}
	var m metricsSample
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if len(m.Containers) != 2 {
		t.Fatalf("containers = %d, want 2 (unsampled one dropped)", len(m.Containers))
	}
	// Sorted by name: outhaul-app-web then outhaul-db-shop; outhaul-app-mid gone.
	if m.Containers[0].Name != "outhaul-app-web" || m.Containers[0].Mem != "42 MiB" {
		t.Errorf("row0 = %+v", m.Containers[0])
	}
	if m.Containers[1].Name != "outhaul-db-shop" || m.Containers[1].Mem != "90 MiB" {
		t.Errorf("row1 = %+v", m.Containers[1])
	}
	for _, c := range m.Containers {
		if c.Name == "outhaul-app-mid" {
			t.Errorf("unsampled container outhaul-app-mid must be absent")
		}
	}
}

// TestMetricsSampleNoContainers checks the empty case: no managed containers
// still yields 200 with valid host/self JSON and an empty container list.
func TestMetricsSampleNoContainers(t *testing.T) {
	const gib = 1024 * 1024 * 1024
	e := newTestEnv(t)
	e.completeSetup(t)

	e.srv.metrics = stubSampler{
		host: hostmetrics.Host{CPUPercent: 5.0, MemUsed: 1 * gib, MemTotal: 2 * gib},
		self: hostmetrics.Self{Goroutines: 7},
	}
	// Only a foreign container → filtered out, leaving nothing to sample.
	e.runtime.stack = []docker.Container{
		{ID: "z", Name: "some-foreign", State: "running"},
	}

	resp := e.get(t, "/metrics/sample")
	if resp.StatusCode != 200 {
		t.Fatalf("GET /metrics/sample = %d, want 200", resp.StatusCode)
	}
	var m metricsSample
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(m.Containers) != 0 {
		t.Fatalf("containers = %d, want 0", len(m.Containers))
	}
	if m.Host.CPU != "5.0" || m.Host.MemPct != 50 {
		t.Errorf("host = %+v", m.Host)
	}
	if m.Self.Goroutines != 7 {
		t.Errorf("self = %+v", m.Self)
	}
}
