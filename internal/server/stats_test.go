package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/james-smart/outhaul/internal/core"
	"github.com/james-smart/outhaul/internal/docker"
)

// getStats fetches and decodes the app's live-metrics snapshot.
func getStats(t *testing.T, e *testEnv, appID int64) appStatsResponse {
	t.Helper()
	resp := e.get(t, "/apps/"+itoa(appID)+"/stats")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var s appStatsResponse
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return s
}

func TestAppStatsNixpacks(t *testing.T) {
	e := newTestEnv(t)
	e.login(t)
	app := seedRunningApp(t, e, "")
	e.runtime.stats = map[string]docker.Stats{"c1": {
		CPUPercent: 3.14,
		MemUsage:   118 * 1024 * 1024,
		MemLimit:   2 * 1024 * 1024 * 1024,
		NetRx:      1536,
		NetTx:      512,
		StartedAt:  time.Now().Add(-(2*time.Hour + 30*time.Second)),
	}}

	s := getStats(t, e, app.ID)
	if !s.Running {
		t.Fatal("app with a running container should report running")
	}
	if s.CPU != "3.1" {
		t.Errorf("cpu = %q, want 3.1", s.CPU)
	}
	if s.Mem != "118 MiB" || s.MemSub != "of 2.0 GiB" {
		t.Errorf("mem = %q %q, want 118 MiB / of 2.0 GiB", s.Mem, s.MemSub)
	}
	if s.Net != "1.5 KiB ↓ · 512 B ↑" {
		t.Errorf("net = %q", s.Net)
	}
	if s.Uptime != "2h 0m" {
		t.Errorf("uptime = %q, want 2h 0m", s.Uptime)
	}
	if s.Containers != 1 {
		t.Errorf("containers = %d, want 1", s.Containers)
	}
}

func TestAppStatsNotRunning(t *testing.T) {
	e := newTestEnv(t)
	e.login(t)
	app, err := e.store.CreateApp(context.Background(),
		core.App{Name: "web", RepoURL: "https://x/y.git", Domain: "web.test"})
	if err != nil {
		t.Fatal(err)
	}

	// No container at all.
	if s := getStats(t, e, app.ID); s.Running {
		t.Error("app with no container should report not running")
	}

	// A container that exists but is stopped.
	e.runtime.container = &docker.Container{ID: "c1", Name: "outhaul-app-web", State: "exited"}
	if s := getStats(t, e, app.ID); s.Running {
		t.Error("app with a stopped container should report not running")
	}
}

func TestAppStatsComposeAggregates(t *testing.T) {
	e := newTestEnv(t)
	e.completeSetup(t)
	app := seedComposeStack(t, e)
	e.runtime.stats = map[string]docker.Stats{
		"c1": {CPUPercent: 1.5, MemUsage: 100 * 1024 * 1024, MemLimit: 2 * 1024 * 1024 * 1024,
			NetRx: 1024, NetTx: 100, StartedAt: time.Now().Add(-3 * time.Hour)},
		"c2": {CPUPercent: 2.0, MemUsage: 200 * 1024 * 1024, MemLimit: 2 * 1024 * 1024 * 1024,
			NetRx: 1024, NetTx: 100, StartedAt: time.Now().Add(-1 * time.Hour)},
	}

	s := getStats(t, e, app.ID)
	if s.CPU != "3.5" {
		t.Errorf("cpu = %q, want the stack's sum 3.5", s.CPU)
	}
	if s.Mem != "300 MiB" {
		t.Errorf("mem = %q, want the stack's sum 300 MiB", s.Mem)
	}
	// Both containers report the host total as their limit; the max (not the
	// sum, which would double-count the host) is shown.
	if s.MemSub != "of 2.0 GiB" {
		t.Errorf("memSub = %q, want of 2.0 GiB", s.MemSub)
	}
	if s.Net != "2.0 KiB ↓ · 200 B ↑" {
		t.Errorf("net = %q, want summed totals", s.Net)
	}
	// Uptime follows the longest-running container.
	if s.Uptime != "3h 0m" {
		t.Errorf("uptime = %q, want 3h 0m", s.Uptime)
	}
	if s.Containers != 2 {
		t.Errorf("containers = %d, want 2", s.Containers)
	}
}

func TestAppStatsUnknownApp404(t *testing.T) {
	e := newTestEnv(t)
	e.login(t)
	resp := e.get(t, "/apps/9999/stats")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func TestAppPageMetricsPanelIsLive(t *testing.T) {
	e := newTestEnv(t)
	e.login(t)
	app := seedRunningApp(t, e, "")

	page := body(t, e.get(t, "/apps/"+itoa(app.ID)))
	for _, id := range []string{`id="metric-cpu"`, `id="metric-mem"`, `id="metric-net"`, `id="metric-uptime"`, `id="metrics-indicator"`} {
		if !strings.Contains(page, id) {
			t.Errorf("app page missing %s", id)
		}
	}
	if strings.Contains(page, "coming soon") || strings.Contains(page, "not live") {
		t.Error("app page still carries the placeholder metrics markers")
	}
}

func TestFmtBytes(t *testing.T) {
	cases := []struct {
		n    uint64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1024, "1.0 KiB"},
		{1536, "1.5 KiB"},
		{10 * 1024, "10 KiB"},
		{118 * 1024 * 1024, "118 MiB"},
		{2 * 1024 * 1024 * 1024, "2.0 GiB"},
		{3 * 1024 * 1024 * 1024 * 1024, "3.0 TiB"},
	}
	for _, c := range cases {
		if got := fmtBytes(c.n); got != c.want {
			t.Errorf("fmtBytes(%d) = %q, want %q", c.n, got, c.want)
		}
	}
}

func TestFmtUptime(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{-time.Second, "0s"},
		{42 * time.Second, "42s"},
		{12*time.Minute + 3*time.Second, "12m 3s"},
		{3*time.Hour + 4*time.Minute, "3h 4m"},
		{2*24*time.Hour + 5*time.Hour, "2d 5h"},
	}
	for _, c := range cases {
		if got := fmtUptime(c.d); got != c.want {
			t.Errorf("fmtUptime(%v) = %q, want %q", c.d, got, c.want)
		}
	}
}
