// Package hostmetrics samples host and self resource usage from the Linux
// /proc filesystem and statfs, with no external dependencies (CGO stays off).
// All file access goes through injectable function fields so tests never touch
// real /proc. CPU is delta-based: the Sampler remembers the previous reading
// and reports utilization since the last call. The first call has no
// predecessor and reports 0% CPU, matching the convention the docker stats
// code already uses.
package hostmetrics

import (
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Host is a point-in-time sample of whole-machine resource usage.
type Host struct {
	CPUPercent           float64 // 0..100 across all cores, since the previous sample
	MemUsed, MemTotal    uint64  // bytes
	DiskUsed, DiskTotal  uint64  // bytes, the filesystem holding diskPath
	Load1, Load5, Load15 float64
}

// Self is a point-in-time sample of the Outhaul process's own usage.
type Self struct {
	CPUPercent float64 // 100 = one core, since the previous sample
	RSS        uint64  // bytes, resident set size
	Goroutines int
	HeapAlloc  uint64 // bytes, Go heap in use
}

// clockTick is USER_HZ on Linux; /proc/<pid>/stat CPU times are in these ticks.
const clockTick = 100

type cpuTimes struct{ busy, total uint64 }

// Sampler reads host and self metrics. It is safe for concurrent use.
type Sampler struct {
	diskPath string

	// Injectable dependencies — real implementations by default, overridden in tests.
	readFile func(string) ([]byte, error)
	statfs   func(string) (total, free uint64, err error)
	now      func() time.Time

	mu         sync.Mutex
	prevHost   cpuTimes
	hostPrimed bool
	prevSelf   uint64 // previous utime+stime, in ticks
	selfPrimed bool
	prevTime   time.Time
}

// NewSampler returns a Sampler reporting disk usage for the filesystem holding
// diskPath (use "/" for the host root).
func NewSampler(diskPath string) *Sampler {
	return &Sampler{
		diskPath: diskPath,
		readFile: os.ReadFile,
		statfs:   realStatfs,
		now:      time.Now,
	}
}

func realStatfs(path string) (total, free uint64, err error) {
	var st syscall.Statfs_t
	if err = syscall.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	bs := uint64(st.Bsize)
	// Bfree (not Bavail) is intentional: for host monitoring we want physical
	// usage including the root-reserved blocks, so this reports more used space
	// than `df` (which subtracts Bavail). Do not "fix" this to Bavail.
	return st.Blocks * bs, st.Bfree * bs, nil
}

// Sample reads one host and self snapshot. It never returns an error:
// unreadable inputs yield zero-valued fields for the affected metric.
func (s *Sampler) Sample() (Host, Self) {
	s.mu.Lock()

	now := s.now()
	var h Host
	var self Self

	if ct, ok := s.readHostCPU(); ok {
		if s.hostPrimed && ct.total > s.prevHost.total && ct.busy >= s.prevHost.busy {
			dTotal := float64(ct.total - s.prevHost.total)
			dBusy := float64(ct.busy - s.prevHost.busy)
			h.CPUPercent = clamp(dBusy / dTotal * 100)
		}
		s.prevHost = ct
		s.hostPrimed = true
	}

	h.MemUsed, h.MemTotal = s.readMem()
	h.DiskUsed, h.DiskTotal = s.readDisk()
	h.Load1, h.Load5, h.Load15 = s.readLoad()

	if ticks, ok := s.readSelfCPU(); ok {
		if s.selfPrimed && ticks >= s.prevSelf {
			if wall := now.Sub(s.prevTime).Seconds(); wall > 0 {
				secs := float64(ticks-s.prevSelf) / clockTick
				self.CPUPercent = secs / wall * 100
			}
		}
		s.prevSelf = ticks
		s.prevTime = now
		s.selfPrimed = true
	}

	self.RSS = s.readSelfRSS()

	// runtime.ReadMemStats stops the world and touches no shared sampler state,
	// so release s.mu before calling it rather than holding the lock across it.
	s.mu.Unlock()
	self.Goroutines = runtime.NumGoroutine()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	self.HeapAlloc = ms.HeapAlloc

	return h, self
}

// readHostCPU parses the aggregate "cpu" line of /proc/stat into busy/total
// jiffies (idle = idle + iowait, the 4th and 5th fields).
func (s *Sampler) readHostCPU() (cpuTimes, bool) {
	b, err := s.readFile("/proc/stat")
	if err != nil {
		return cpuTimes{}, false
	}
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(line, "cpu ") {
			continue
		}
		fields := strings.Fields(line)[1:] // user nice system idle iowait irq ...
		var total, idle uint64
		for i, f := range fields {
			v, err := strconv.ParseUint(f, 10, 64)
			if err != nil {
				continue
			}
			total += v
			if i == 3 || i == 4 { // idle, iowait
				idle += v
			}
		}
		return cpuTimes{busy: total - idle, total: total}, true
	}
	return cpuTimes{}, false
}

// readMem returns used/total bytes from /proc/meminfo (used = MemTotal -
// MemAvailable). Values in the file are in kB.
func (s *Sampler) readMem() (used, total uint64) {
	b, err := s.readFile("/proc/meminfo")
	if err != nil {
		return 0, 0
	}
	var memTotal, memAvail uint64
	haveAvail := false
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		v, _ := strconv.ParseUint(f[1], 10, 64)
		switch f[0] {
		case "MemTotal:":
			memTotal = v * 1024
		case "MemAvailable:":
			memAvail = v * 1024
			haveAvail = true
		}
	}
	if !haveAvail || memAvail > memTotal {
		return 0, memTotal
	}
	return memTotal - memAvail, memTotal
}

// readDisk returns used/total bytes for the sampler's filesystem via statfs.
func (s *Sampler) readDisk() (used, total uint64) {
	total, free, err := s.statfs(s.diskPath)
	if err != nil || total == 0 {
		return 0, 0
	}
	if free > total {
		free = total
	}
	return total - free, total
}

// readLoad returns the 1/5/15-minute load averages from /proc/loadavg.
func (s *Sampler) readLoad() (l1, l5, l15 float64) {
	b, err := s.readFile("/proc/loadavg")
	if err != nil {
		return 0, 0, 0
	}
	f := strings.Fields(string(b))
	if len(f) < 3 {
		return 0, 0, 0
	}
	l1, _ = strconv.ParseFloat(f[0], 64)
	l5, _ = strconv.ParseFloat(f[1], 64)
	l15, _ = strconv.ParseFloat(f[2], 64)
	return l1, l5, l15
}

// readSelfCPU returns utime+stime in clock ticks from /proc/self/stat. comm
// (field 2) can contain spaces and parentheses, so everything after the last
// ')' is parsed: rest[0] is field 3 (state), so utime (field 14) and stime
// (field 15) are rest[11] and rest[12].
func (s *Sampler) readSelfCPU() (uint64, bool) {
	b, err := s.readFile("/proc/self/stat")
	if err != nil {
		return 0, false
	}
	str := string(b)
	i := strings.LastIndexByte(str, ')')
	if i < 0 {
		return 0, false
	}
	rest := strings.Fields(str[i+1:])
	if len(rest) < 13 {
		return 0, false
	}
	utime, _ := strconv.ParseUint(rest[11], 10, 64)
	stime, _ := strconv.ParseUint(rest[12], 10, 64)
	return utime + stime, true
}

// readSelfRSS returns the resident set size in bytes from /proc/self/status
// (VmRSS is reported in kB).
func (s *Sampler) readSelfRSS() uint64 {
	b, err := s.readFile("/proc/self/status")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "VmRSS:") {
			f := strings.Fields(line)
			if len(f) >= 2 {
				v, _ := strconv.ParseUint(f[1], 10, 64)
				return v * 1024
			}
		}
	}
	return 0
}

func clamp(v float64) float64 {
	switch {
	case v < 0:
		return 0
	case v > 100:
		return 100
	default:
		return v
	}
}
