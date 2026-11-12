package server

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/outhaul-dev/outhaul/internal/compose"
	"github.com/outhaul-dev/outhaul/internal/core"
	"github.com/outhaul-dev/outhaul/internal/docker"
)

// appStatsResponse is the live-metrics snapshot the app page polls. Values are
// pre-formatted display strings so the formatting lives in testable Go and the
// template script stays a dumb poller.
type appStatsResponse struct {
	Running    bool   `json:"running"`
	CPU        string `json:"cpu,omitempty"`     // percent, 100 = one core
	CPUSub     string `json:"cpuSub,omitempty"`  // "limit 1 CPU" or "limits differ"
	Mem        string `json:"mem,omitempty"`     // e.g. "118 MiB"
	MemSub     string `json:"memSub,omitempty"`  // e.g. "of 1.9 GiB"
	Pids       string `json:"pids,omitempty"`    // e.g. "12"
	PidsSub    string `json:"pidsSub,omitempty"` // "of 512" or "limits differ"
	Net        string `json:"net,omitempty"`     // cumulative "rx ↓ · tx ↑"
	Uptime     string `json:"uptime,omitempty"`  // e.g. "2d 4h"
	Containers int    `json:"containers,omitempty"`
}

// handleAppStats returns one aggregated docker-stats sample for the app's
// running containers. There is no history or persistence — the page polls
// this while open, mirroring Dokploy's refresh-rate model without its
// metrics store.
func (s *Server) handleAppStats(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	app, err := s.store.GetApp(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache")
	json.NewEncoder(w).Encode(s.appStats(r.Context(), app))
}

// appStats samples and aggregates stats across the app's running containers.
// Containers that fail to sample (e.g. stopped mid-poll) are skipped; if
// nothing samples, the app reports as not running.
func (s *Server) appStats(ctx context.Context, app core.App) appStatsResponse {
	containers, err := s.runningContainers(ctx, app)
	if err != nil || len(containers) == 0 {
		return appStatsResponse{Running: false}
	}

	samples := make([]*docker.Stats, len(containers))
	var wg sync.WaitGroup
	for i, c := range containers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if st, err := s.runtime.ContainerStats(ctx, c.ID); err == nil {
				samples[i] = &st
			}
		}()
	}
	wg.Wait()

	var agg docker.Stats
	var cpuLimits, memLimits, pidLimits []int64
	var totalPids uint64
	sampled := 0
	for _, st := range samples {
		if st == nil {
			continue
		}
		sampled++
		agg.CPUPercent += st.CPUPercent
		agg.MemUsage += st.MemUsage
		agg.NetRx += st.NetRx
		agg.NetTx += st.NetTx
		// Limit is the host total for unlimited containers; summing would
		// double-count it, so take the max.
		if st.MemLimit > agg.MemLimit {
			agg.MemLimit = st.MemLimit
		}
		// Uptime is the longest-running container's.
		if !st.StartedAt.IsZero() && (agg.StartedAt.IsZero() || st.StartedAt.Before(agg.StartedAt)) {
			agg.StartedAt = st.StartedAt
		}
		if st.CPULimit > 0 {
			cpuLimits = append(cpuLimits, int64(math.Round(st.CPULimit*1000)))
		}
		if st.MemLimited {
			memLimits = append(memLimits, int64(st.MemLimit))
		}
		totalPids += st.Pids
		if st.PidsLimit > 0 {
			pidLimits = append(pidLimits, int64(st.PidsLimit))
		}
	}
	if sampled == 0 {
		return appStatsResponse{Running: false}
	}

	resp := appStatsResponse{
		Running:    true,
		CPU:        fmt.Sprintf("%.1f", agg.CPUPercent),
		Mem:        fmtBytes(agg.MemUsage),
		Net:        fmtBytes(agg.NetRx) + " ↓ · " + fmtBytes(agg.NetTx) + " ↑",
		Containers: sampled,
	}
	if v, same, any := commonLimit(cpuLimits); any {
		if same && len(cpuLimits) == sampled {
			resp.CPUSub = "limit " + fmtCPU(v) + " CPU"
		} else {
			resp.CPUSub = "limits differ"
		}
	}
	switch {
	case len(memLimits) == 0:
		// No container has its own limit: today's host-total text.
		if agg.MemLimit > 0 {
			resp.MemSub = "of " + fmtBytes(agg.MemLimit)
		}
	case len(memLimits) == sampled && allSame(memLimits):
		resp.MemSub = "of " + fmtBytes(uint64(memLimits[0]))
	default:
		resp.MemSub = "limits differ"
	}
	if len(pidLimits) > 0 {
		resp.Pids = strconv.FormatUint(totalPids, 10)
		if v, same, _ := commonLimit(pidLimits); same && len(pidLimits) == sampled {
			resp.PidsSub = "of " + strconv.FormatInt(v, 10)
		} else {
			resp.PidsSub = "limits differ"
		}
	}
	if !agg.StartedAt.IsZero() {
		resp.Uptime = fmtUptime(time.Since(agg.StartedAt))
	}
	return resp
}

// runningContainers resolves the app's currently running containers: the one
// outhaul-app-<name> container for nixpacks apps, the labelled stack for
// compose apps.
func (s *Server) runningContainers(ctx context.Context, app core.App) ([]docker.Container, error) {
	if app.Kind == core.KindCompose {
		cs, err := s.runtime.ListContainers(ctx,
			map[string]string{"com.docker.compose.project": compose.ProjectName(app.Name)})
		if err != nil {
			return nil, err
		}
		running := cs[:0]
		for _, c := range cs {
			if c.Running() {
				running = append(running, c)
			}
		}
		return running, nil
	}
	c, err := s.runtime.FindContainer(ctx, appContainerPrefix+app.Name)
	if err != nil || c == nil || !c.Running() {
		return nil, err
	}
	return []docker.Container{*c}, nil
}

// commonLimit reports the value every entry has, whether they all agree, and
// whether there was one at all.
func commonLimit(values []int64) (value int64, same, any bool) {
	if len(values) == 0 {
		return 0, false, false
	}
	return values[0], allSame(values), true
}

// allSame reports whether every entry equals the first.
func allSame(values []int64) bool {
	for _, v := range values[1:] {
		if v != values[0] {
			return false
		}
	}
	return true
}

// fmtCPU renders thousandths of a CPU as Compose and the panel write it: 1,
// 0.5, 1.5.
func fmtCPU(milli int64) string {
	whole, frac := milli/1000, milli%1000
	if frac == 0 {
		return strconv.FormatInt(whole, 10)
	}
	return strconv.FormatInt(whole, 10) + "." + strings.TrimRight(fmt.Sprintf("%03d", frac), "0")
}

// fmtBytes renders a byte count in IEC units the way docker stats does.
func fmtBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	v, exp := float64(n), 0
	for v >= unit && exp < 4 {
		v /= unit
		exp++
	}
	suffix := []string{"KiB", "MiB", "GiB", "TiB"}[exp-1]
	if v < 10 {
		return fmt.Sprintf("%.1f %s", v, suffix)
	}
	return fmt.Sprintf("%.0f %s", v, suffix)
}

// fmtUptime renders a duration at two units of precision: "42s", "12m 3s",
// "3h 4m", "2d 5h".
func fmtUptime(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm %ds", int(d.Minutes()), int(d.Seconds())%60)
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		days := int(d.Hours()) / 24
		return fmt.Sprintf("%dd %dh", days, int(d.Hours())%24)
	}
}
