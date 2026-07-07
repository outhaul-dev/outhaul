package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"sync"

	"github.com/outhaul-dev/outhaul/internal/hostmetrics"
)

// metricsSampler is the host/self sampling surface the Metrics page needs. The
// production implementation is *hostmetrics.Sampler; tests inject a stub for
// determinism.
type metricsSampler interface {
	Sample() (hostmetrics.Host, hostmetrics.Self)
}

// metricsSample is the JSON snapshot the Metrics page polls. Values are
// pre-formatted display strings (formatting lives in testable Go) except the
// *Pct fields, which drive meter-bar widths.
type metricsSample struct {
	Host       hostView        `json:"host"`
	Self       selfView        `json:"self"`
	Containers []containerStat `json:"containers"`
}

type hostView struct {
	CPU     string  `json:"cpu"`
	CPUPct  float64 `json:"cpuPct"`
	Mem     string  `json:"mem"`
	MemSub  string  `json:"memSub"`
	MemPct  float64 `json:"memPct"`
	Disk    string  `json:"disk"`
	DiskSub string  `json:"diskSub"`
	DiskPct float64 `json:"diskPct"`
	Load    string  `json:"load"`
}

type selfView struct {
	CPU        string `json:"cpu"`
	Mem        string `json:"mem"`
	Goroutines int    `json:"goroutines"`
	Heap       string `json:"heap"`
}

type containerStat struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
	CPU  string `json:"cpu"`
	Mem  string `json:"mem"`
	Net  string `json:"net"`
}

// handleMetrics renders the Metrics page shell; the numbers are filled by the
// inline poller hitting /metrics/sample.
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	s.render(w, http.StatusOK, "metrics", map[string]any{
		"Title":  "Metrics",
		"Active": "metrics",
	})
}

// handleMetricsSample returns one live host/self/container snapshot as JSON.
// There is no history — the page polls this while open, mirroring the per-app
// stats endpoint.
func (s *Server) handleMetricsSample(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache")
	json.NewEncoder(w).Encode(s.metricsSnapshot(r.Context()))
}

func (s *Server) metricsSnapshot(ctx context.Context) metricsSample {
	host, self := s.metrics.Sample()
	return metricsSample{
		Host:       hostViewOf(host),
		Self:       selfViewOf(self),
		Containers: s.containerStats(ctx),
	}
}

func hostViewOf(h hostmetrics.Host) hostView {
	v := hostView{
		CPU:    fmt.Sprintf("%.1f", h.CPUPercent),
		CPUPct: h.CPUPercent,
		Mem:    fmtBytes(h.MemUsed),
		Disk:   fmtBytes(h.DiskUsed),
		Load:   fmt.Sprintf("%.2f · %.2f · %.2f", h.Load1, h.Load5, h.Load15),
	}
	if h.MemTotal > 0 {
		v.MemSub = "of " + fmtBytes(h.MemTotal)
		v.MemPct = pct(h.MemUsed, h.MemTotal)
	}
	if h.DiskTotal > 0 {
		v.DiskSub = "of " + fmtBytes(h.DiskTotal)
		v.DiskPct = pct(h.DiskUsed, h.DiskTotal)
	}
	return v
}

func selfViewOf(s hostmetrics.Self) selfView {
	return selfView{
		CPU:        fmt.Sprintf("%.1f", s.CPUPercent),
		Mem:        fmtBytes(s.RSS),
		Goroutines: s.Goroutines,
		Heap:       fmtBytes(s.HeapAlloc),
	}
}

func pct(used, total uint64) float64 {
	if total == 0 {
		return 0
	}
	if p := float64(used) / float64(total) * 100; p < 100 {
		return p
	}
	return 100
}

// containerStats samples every running, Outhaul-managed container in parallel
// and returns rows sorted by name. Foreign, infra, and transient containers are
// dropped by containerKind; a container that fails to sample (stopped mid-poll)
// is skipped.
func (s *Server) containerStats(ctx context.Context) []containerStat {
	all, err := s.runtime.ListContainers(ctx, nil)
	if err != nil {
		return nil
	}
	type target struct {
		id, name, kind string
	}
	var targets []target
	for _, c := range all {
		if !c.Running() {
			continue
		}
		if kind := containerKind(c.Name); kind != "" {
			targets = append(targets, target{c.ID, c.Name, kind})
		}
	}

	out := make([]containerStat, len(targets))
	ok := make([]bool, len(targets))
	var wg sync.WaitGroup
	// One goroutine per running managed container, uncapped: fine at the current
	// single-admin / few-containers scale. If container counts ever grow large,
	// bound the fan-out with a worker pool / semaphore here.
	for i, tg := range targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			st, err := s.runtime.ContainerStats(ctx, tg.id)
			if err != nil {
				return
			}
			out[i] = containerStat{
				Name: tg.name,
				Kind: tg.kind,
				CPU:  fmt.Sprintf("%.1f", st.CPUPercent),
				Mem:  fmtBytes(st.MemUsage),
				Net:  fmtBytes(st.NetRx) + " ↓ · " + fmtBytes(st.NetTx) + " ↑",
			}
			ok[i] = true
		}()
	}
	wg.Wait()

	rows := make([]containerStat, 0, len(targets))
	for i := range out {
		if ok[i] {
			rows = append(rows, out[i])
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
	return rows
}
