package githttp

import (
	"fmt"
	"net/netip"
	"sync"
	"testing"
	"time"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newTestLimiter() (*Limiter, *fakeClock) {
	c := &fakeClock{t: time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)}
	return NewLimiter(c.now), c
}

func addr(s string) netip.Addr { return netip.MustParseAddr(s) }

func failN(l *Limiter, a string, n int) (limited int) {
	for i := 0; i < n; i++ {
		if _, lim := l.Fail(addr(a)); lim {
			limited++
		}
	}
	return limited
}

func TestLimiterTenThenLimited(t *testing.T) {
	l, _ := newTestLimiter()
	if n := failN(l, "203.0.113.9", 10); n != 0 {
		t.Fatalf("%d of the first ten were limited", n)
	}
	wait, limited := l.Fail(addr("203.0.113.9"))
	if !limited {
		t.Fatal("the eleventh was not limited")
	}
	if wait < 59*time.Second || wait > 60*time.Second {
		t.Fatalf("wait = %v; want 59s to 60s", wait)
	}
}

func TestLimiterRefillsOnePerMinute(t *testing.T) {
	l, c := newTestLimiter()
	failN(l, "203.0.113.9", 10)
	if _, lim := l.Fail(addr("203.0.113.9")); !lim {
		t.Fatal("not limited at eleven")
	}
	c.advance(time.Minute)
	if _, lim := l.Fail(addr("203.0.113.9")); lim {
		t.Fatal("limited one minute later")
	}
	if _, lim := l.Fail(addr("203.0.113.9")); !lim {
		t.Fatal("two attempts a minute after ten")
	}
	c.advance(11 * time.Minute)
	if n := failN(l, "203.0.113.9", 10); n != 0 {
		t.Fatalf("after quiet, %d of ten were limited", n)
	}
	if _, lim := l.Fail(addr("203.0.113.9")); !lim {
		t.Fatal("more than ten allowed after a long quiet")
	}
}

func TestLimiterLimitedConsumesNothing(t *testing.T) {
	l, c := newTestLimiter()
	failN(l, "203.0.113.9", 10)
	failN(l, "203.0.113.9", 50) // all limited
	c.advance(time.Minute)
	if _, lim := l.Fail(addr("203.0.113.9")); lim {
		t.Fatal("refused attempts were counted against the address")
	}
}

func TestLimiterV6IsCountedByItsSlash64(t *testing.T) {
	l, _ := newTestLimiter()
	failN(l, "2001:db8:1:2::1", 10)
	if _, lim := l.Fail(addr("2001:db8:1:2:ffff::9")); !lim {
		t.Fatal("same /64 did not share an allowance")
	}
	if _, lim := l.Fail(addr("2001:db8:1:3::1")); lim {
		t.Fatal("another /64 was limited")
	}
}

func TestLimiterMappedV4SharesWithV4(t *testing.T) {
	l, _ := newTestLimiter()
	failN(l, "203.0.113.9", 10)
	if _, lim := l.Fail(addr("::ffff:203.0.113.9")); !lim {
		t.Fatal("mapped form has its own allowance")
	}
}

func TestLimiterOtherAddressUnaffected(t *testing.T) {
	l, _ := newTestLimiter()
	failN(l, "203.0.113.9", 11)
	if _, lim := l.Fail(addr("203.0.113.10")); lim {
		t.Fatal("another address was limited")
	}
}

func TestLimiterBoundsTheMapAndEvictsTheOldest(t *testing.T) {
	l, c := newTestLimiter()
	first := netip.MustParseAddr("10.0.0.0")
	a := first
	for i := 0; i < 10_001; i++ {
		l.Fail(a)
		c.advance(time.Millisecond)
		a = a.Next()
	}
	if len(l.buckets) > 10_000 {
		t.Fatalf("map holds %d", len(l.buckets))
	}
	if _, ok := l.buckets[first]; ok {
		t.Fatal("the address touched longest ago is still held")
	}
	if _, ok := l.buckets[first.Next()]; !ok {
		t.Fatal("a newer address was evicted")
	}
}

func TestLimiterDropsAddressesBackAtTenWhenFull(t *testing.T) {
	l, c := newTestLimiter()
	a := netip.MustParseAddr("10.0.0.0")
	for i := 0; i < 10_000; i++ {
		l.Fail(a)
		a = a.Next()
	}
	c.advance(time.Minute) // every one of them has its ten back
	l.Fail(a)
	if len(l.buckets) != 1 {
		t.Fatalf("map holds %d; want only the new address", len(l.buckets))
	}
}

func TestLimiterConcurrent(t *testing.T) {
	l, _ := newTestLimiter()
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				l.Fail(addr(fmt.Sprintf("198.51.100.%d", i%20)))
			}
		}()
	}
	wg.Wait()
}
