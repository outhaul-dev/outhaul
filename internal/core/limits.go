package core

import (
	"errors"
	"regexp"
	"strconv"
	"strings"
)

// MinMemory is Docker's minimum memory limit, 6 MiB.
const MinMemory int64 = 6 << 20

// MaxPipelines is how many deployments run at once: the pipeline semaphore's
// size, and the upper bound of OUTHAUL_MAX_CONCURRENT_BUILDS.
const MaxPipelines = 4

// The "want" phrases the settings parser and the per-app form both show.
const (
	wantCPUs   = "a number of CPUs with up to three decimals, 0.01 or more"
	wantMemory = "a memory size such as 512m or 1g, 6m or more"
)

// Limits is a container's resource ceiling. A zero field means no limit.
type Limits struct {
	MilliCPUs int64 // thousandths of a CPU; 0 = none
	Memory    int64 // bytes; 0 = none
	Pids      int64 // processes; 0 = none
}

var cpusRe = regexp.MustCompile(`^[0-9]+(\.[0-9]{1,3})?$`)

// ParseCPUs returns thousandths of a CPU. It accepts digits with up to three
// decimals, 0.01 or more.
func ParseCPUs(s string) (int64, error) {
	if !cpusRe.MatchString(s) {
		return 0, errors.New(wantCPUs)
	}
	whole, frac, _ := strings.Cut(s, ".")
	w, err := strconv.ParseInt(whole, 10, 64)
	if err != nil || w > (1<<62)/1000 {
		return 0, errors.New(wantCPUs)
	}
	milli := w * 1000
	if frac != "" {
		f, err := strconv.ParseInt(frac, 10, 64)
		if err != nil {
			return 0, errors.New(wantCPUs)
		}
		for i := len(frac); i < 3; i++ {
			f *= 10
		}
		milli += f
	}
	if milli < 10 {
		return 0, errors.New(wantCPUs)
	}
	return milli, nil
}

var memoryRe = regexp.MustCompile(`^([0-9]+)(?:\.([0-9]+))?$`)

// ParseMemory returns bytes. It accepts digits, an optional decimal part, and
// an optional k, m or g in either case, with or without b. Units are powers of
// 1024, and the minimum is MinMemory.
func ParseMemory(s string) (int64, error) {
	v := strings.ToLower(s)
	v = strings.TrimSuffix(v, "b")
	unit := int64(1)
	switch {
	case strings.HasSuffix(v, "k"):
		unit = 1 << 10
		v = strings.TrimSuffix(v, "k")
	case strings.HasSuffix(v, "m"):
		unit = 1 << 20
		v = strings.TrimSuffix(v, "m")
	case strings.HasSuffix(v, "g"):
		unit = 1 << 30
		v = strings.TrimSuffix(v, "g")
	}
	m := memoryRe.FindStringSubmatch(v)
	if m == nil {
		return 0, errors.New(wantMemory)
	}
	whole, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return 0, errors.New(wantMemory)
	}
	frac := m[2]
	var f, den int64 = 0, 1
	for i := 0; i < len(frac); i++ {
		if den > (1<<62)/10 {
			return 0, errors.New(wantMemory)
		}
		den *= 10
		f = f*10 + int64(frac[i]-'0')
	}
	if whole > (1<<62)/unit/den {
		return 0, errors.New(wantMemory)
	}
	bytes := (whole*den + f) * unit / den
	if bytes < MinMemory {
		return 0, errors.New(wantMemory)
	}
	return bytes, nil
}

// Or returns l with each zero field taken from fallback.
func (l Limits) Or(fallback Limits) Limits {
	if l.MilliCPUs == 0 {
		l.MilliCPUs = fallback.MilliCPUs
	}
	if l.Memory == 0 {
		l.Memory = fallback.Memory
	}
	if l.Pids == 0 {
		l.Pids = fallback.Pids
	}
	return l
}
