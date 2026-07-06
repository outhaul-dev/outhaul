package hostmetrics

import (
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// noFiles is a readFile stub that fails every read; individual tests override
// the paths they care about.
func noFiles(string) ([]byte, error) { return nil, os.ErrNotExist }

func noStatfs(string) (uint64, uint64, error) { return 0, 0, os.ErrNotExist }

func TestHostCPUDelta(t *testing.T) {
	s := NewSampler("/")
	s.statfs = noStatfs
	// Fields: user nice system idle iowait ...
	// idle = idle + iowait = 700 + 100 = 800; total = 1000; busy = 200.
	// The nonzero iowait exercises the i==4 idle term.
	stat := "cpu  100 0 100 700 100 0 0 0\n"
	s.readFile = func(p string) ([]byte, error) {
		if p == "/proc/stat" {
			return []byte(stat), nil
		}
		return noFiles(p)
	}

	if h, _ := s.Sample(); h.CPUPercent != 0 {
		t.Fatalf("first sample CPU = %v, want 0 (no predecessor)", h.CPUPercent)
	}
	// Advance including iowait: idle = 800 + 100 = 900, total = 1400.
	// delta total = 400, delta idle = 100, delta busy = 300 => 75%.
	stat = "cpu  300 0 200 800 100 0 0 0\n"
	if h, _ := s.Sample(); h.CPUPercent != 75 {
		t.Fatalf("CPU = %v, want 75", h.CPUPercent)
	}
}

func TestHostMem(t *testing.T) {
	s := NewSampler("/")
	s.statfs = noStatfs
	s.readFile = func(p string) ([]byte, error) {
		if p == "/proc/meminfo" {
			return []byte("MemTotal:       4000 kB\nMemFree:  500 kB\nMemAvailable:    1500 kB\n"), nil
		}
		return noFiles(p)
	}
	h, _ := s.Sample()
	if h.MemTotal != 4000*1024 || h.MemUsed != (4000-1500)*1024 {
		t.Fatalf("mem = %d/%d, want %d/%d", h.MemUsed, h.MemTotal, (4000-1500)*1024, 4000*1024)
	}
}

func TestHostDisk(t *testing.T) {
	s := NewSampler("/")
	s.readFile = noFiles
	const gib = 1024 * 1024 * 1024
	s.statfs = func(p string) (uint64, uint64, error) {
		if p != "/" {
			t.Fatalf("statfs path = %q, want /", p)
		}
		return 40 * gib, 26 * gib, nil // 40 total, 26 free => 14 used
	}
	h, _ := s.Sample()
	if h.DiskTotal != 40*gib || h.DiskUsed != 14*gib {
		t.Fatalf("disk = %d/%d, want %d/%d", h.DiskUsed, h.DiskTotal, 14*gib, 40*gib)
	}
}

func TestHostLoad(t *testing.T) {
	s := NewSampler("/")
	s.statfs = noStatfs
	s.readFile = func(p string) ([]byte, error) {
		if p == "/proc/loadavg" {
			return []byte("0.42 0.31 0.28 1/234 5678\n"), nil
		}
		return noFiles(p)
	}
	h, _ := s.Sample()
	if h.Load1 != 0.42 || h.Load5 != 0.31 || h.Load15 != 0.28 {
		t.Fatalf("load = %v/%v/%v, want 0.42/0.31/0.28", h.Load1, h.Load5, h.Load15)
	}
}

func TestSelfCPUDelta(t *testing.T) {
	s := NewSampler("/")
	s.statfs = noStatfs
	// comm deliberately contains a space and parens to exercise the last-')'
	// parse. utime=100 stime=100 => 200 ticks.
	stat := "123 (out (haul)) S 1 1 1 0 -1 0 0 0 0 0 100 100 " + strings.Repeat("0 ", 30) + "\n"
	tm := time.Unix(1000, 0)
	s.now = func() time.Time { return tm }
	s.readFile = func(p string) ([]byte, error) {
		if p == "/proc/self/stat" {
			return []byte(stat), nil
		}
		return noFiles(p)
	}

	s.Sample() // prime at 200 ticks, t=1000
	// +1s wall, +50 ticks (0.5 CPU-seconds) => 50%.
	stat = "123 (out (haul)) S 1 1 1 0 -1 0 0 0 0 0 125 125 " + strings.Repeat("0 ", 30) + "\n"
	tm = time.Unix(1001, 0)
	if _, self := s.Sample(); self.CPUPercent != 50 {
		t.Fatalf("self CPU = %v, want 50", self.CPUPercent)
	}
}

func TestSelfRSSAndRuntime(t *testing.T) {
	s := NewSampler("/")
	s.statfs = noStatfs
	s.readFile = func(p string) ([]byte, error) {
		if p == "/proc/self/status" {
			return []byte("Name:\touthaul\nVmRSS:\t   18432 kB\nThreads:\t8\n"), nil
		}
		return noFiles(p)
	}
	_, self := s.Sample()
	if self.RSS != 18432*1024 {
		t.Fatalf("RSS = %d, want %d", self.RSS, 18432*1024)
	}
	if self.Goroutines < 1 {
		t.Fatalf("goroutines = %d, want >= 1", self.Goroutines)
	}
	if self.HeapAlloc == 0 {
		t.Fatalf("heap alloc = 0, want > 0")
	}
}

func TestMissingFilesTolerated(t *testing.T) {
	s := NewSampler("/")
	s.readFile = noFiles
	s.statfs = noStatfs
	h, self := s.Sample() // must not panic
	if h.MemTotal != 0 || h.CPUPercent != 0 || h.DiskTotal != 0 || self.RSS != 0 {
		t.Fatalf("expected zeroed metrics, got host=%+v self=%+v", h, self)
	}
}

// TestMemNoAvailable checks the fallback when MemAvailable is absent: used is 0
// and total still reflects MemTotal.
func TestMemNoAvailable(t *testing.T) {
	s := NewSampler("/")
	s.statfs = noStatfs
	s.readFile = func(p string) ([]byte, error) {
		if p == "/proc/meminfo" {
			return []byte("MemTotal:       4000 kB\nMemFree:  500 kB\n"), nil
		}
		return noFiles(p)
	}
	h, _ := s.Sample()
	if h.MemUsed != 0 || h.MemTotal != 4000*1024 {
		t.Fatalf("mem = %d/%d, want 0/%d", h.MemUsed, h.MemTotal, 4000*1024)
	}
}

// TestDiskFreeExceedsTotal checks that a free > total reading is clamped so
// DiskUsed does not underflow.
func TestDiskFreeExceedsTotal(t *testing.T) {
	s := NewSampler("/")
	s.readFile = noFiles
	const gib = 1024 * 1024 * 1024
	s.statfs = func(string) (uint64, uint64, error) {
		return 40 * gib, 50 * gib, nil // free > total
	}
	h, _ := s.Sample()
	if h.DiskUsed != 0 || h.DiskTotal != 40*gib {
		t.Fatalf("disk = %d/%d, want 0/%d", h.DiskUsed, h.DiskTotal, 40*gib)
	}
}

// TestSampleConcurrent exercises the "safe for concurrent use" claim; run under
// -race it must not panic or trip the race detector.
func TestSampleConcurrent(t *testing.T) {
	s := NewSampler("/")
	s.statfs = func(string) (uint64, uint64, error) { return 100, 40, nil }
	s.readFile = func(p string) ([]byte, error) {
		switch p {
		case "/proc/stat":
			return []byte("cpu  100 0 100 700 100 0 0 0\n"), nil
		case "/proc/meminfo":
			return []byte("MemTotal: 4000 kB\nMemAvailable: 1500 kB\n"), nil
		case "/proc/loadavg":
			return []byte("0.1 0.2 0.3 1/2 3\n"), nil
		case "/proc/self/stat":
			return []byte("1 (x) S 1 1 1 0 -1 0 0 0 0 0 5 5 " + strings.Repeat("0 ", 30) + "\n"), nil
		case "/proc/self/status":
			return []byte("VmRSS:\t1024 kB\n"), nil
		}
		return noFiles(p)
	}

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				s.Sample()
			}
		}()
	}
	wg.Wait()
}
