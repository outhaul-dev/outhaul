package githttp

import (
	"net/netip"
	"sync"
	"time"
)

const (
	// failAllowance is how many failed attempts an address may make at once.
	failAllowance = 10
	// failRefill is how often an address earns one more, up to failAllowance.
	failRefill = time.Minute
	// maxLimiterAddresses bounds the map, so a flood of addresses can't grow
	// memory without limit.
	maxLimiterAddresses = 10_000
)

// Limiter counts failed attempts per client address: each may fail ten times
// at once and earns one more attempt each minute, up to ten. An IPv6 address
// is counted by its /64. It is in memory, so a restart forgets it.
type Limiter struct {
	mu      sync.Mutex
	buckets map[netip.Addr]*bucket
	now     func() time.Time
}

// bucket is one address's allowance. tokens counts the attempts left as of
// stamp; touched is the last time the address failed.
type bucket struct {
	tokens  int
	stamp   time.Time
	touched time.Time
}

// NewLimiter returns a limiter reading the time from now.
func NewLimiter(now func() time.Time) *Limiter {
	return &Limiter{buckets: map[netip.Addr]*bucket{}, now: now}
}

// limiterKey is what an address is counted under: IPv4 (and IPv4-mapped IPv6)
// as itself, IPv6 as its /64.
func limiterKey(a netip.Addr) netip.Addr {
	a = a.WithZone("").Unmap()
	if a.Is6() {
		p, err := a.Prefix(64)
		if err == nil {
			return p.Addr()
		}
	}
	return a
}

// refill brings b up to date at now.
func (b *bucket) refill(now time.Time) {
	if b.tokens >= failAllowance {
		b.tokens, b.stamp = failAllowance, now
		return
	}
	if n := int(now.Sub(b.stamp) / failRefill); n > 0 {
		b.tokens += n
		b.stamp = b.stamp.Add(time.Duration(n) * failRefill)
	}
	if b.tokens >= failAllowance {
		b.tokens, b.stamp = failAllowance, now
	}
}

// Fail records a failed attempt from addr. limited is true when the address
// had none left; wait is then how long until it has one. A limited call
// consumes nothing.
func (l *Limiter) Fail(addr netip.Addr) (wait time.Duration, limited bool) {
	key := limiterKey(addr)
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()

	b := l.buckets[key]
	if b == nil {
		if len(l.buckets) >= maxLimiterAddresses {
			l.makeRoom(now)
		}
		b = &bucket{tokens: failAllowance, stamp: now}
		l.buckets[key] = b
	}
	b.refill(now)
	b.touched = now
	if b.tokens == 0 {
		return b.stamp.Add(failRefill).Sub(now), true
	}
	b.tokens--
	return 0, false
}

// makeRoom drops every address back at its full allowance, and when that
// frees nothing, the one touched longest ago. Ties are arbitrary.
func (l *Limiter) makeRoom(now time.Time) {
	for key, b := range l.buckets {
		b.refill(now)
		if b.tokens >= failAllowance {
			delete(l.buckets, key)
		}
	}
	if len(l.buckets) < maxLimiterAddresses {
		return
	}
	var (
		oldestKey netip.Addr
		oldest    time.Time
		first     = true
	)
	for key, b := range l.buckets {
		if first || b.touched.Before(oldest) {
			oldestKey, oldest, first = key, b.touched, false
		}
	}
	delete(l.buckets, oldestKey)
}
