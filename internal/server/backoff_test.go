package server

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func newTestLimiter() (*signInLimiter, *time.Time) {
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	return newSignInLimiter(func() time.Time { return now }), &now
}

func TestSignInLimiterLockTable(t *testing.T) {
	for _, tc := range []struct {
		failures int
		wait     time.Duration
	}{
		{0, 0}, {1, 0}, {2, 0}, {3, 0}, {4, 0},
		{5, 30 * time.Second},
		{6, time.Minute},
		{7, 2 * time.Minute},
		{8, 4 * time.Minute},
		{9, 8 * time.Minute},
		{10, 15 * time.Minute},
		{15, 15 * time.Minute},
	} {
		l, now := newTestLimiter()
		for i := 0; i < tc.failures; i++ {
			*now = now.Add(lockFor(i))
			wait, ok, _ := l.allow("admin", true)
			if !ok {
				t.Fatalf("%d failures: attempt %d was refused for %v", tc.failures, i+1, wait)
			}
		}
		wait, ok, _ := l.allow("admin", true)
		if tc.wait == 0 {
			if !ok {
				t.Errorf("%d failures: the next attempt was refused; want it checked", tc.failures)
			}
		} else if ok || wait != tc.wait {
			t.Errorf("%d failures: next = (%v, %v), want refused for %v", tc.failures, wait, ok, tc.wait)
		}
	}
}

func TestSignInLimiterHourClearsAName(t *testing.T) {
	l, now := newTestLimiter()
	for i := 0; i < 4; i++ {
		l.allow("admin", true)
	}
	*now = now.Add(time.Hour + time.Second)
	if _, ok, _ := l.allow("admin", true); !ok {
		t.Error("the first attempt after an hour was refused; the count should start again")
	}
	if f := l.known[sha256.Sum256([]byte("admin"))]; f == nil || f.count != 1 {
		t.Errorf("after an hour the record is %+v, want one fresh failure", f)
	}
}

func TestSignInLimiterSuccessClearsAName(t *testing.T) {
	l, _ := newTestLimiter()
	for i := 0; i < 5; i++ {
		l.allow("admin", true)
	}
	l.clear("admin")
	if wait, ok, _ := l.allow("admin", true); !ok {
		t.Errorf("after a success the next attempt was refused for %v", wait)
	}
}

func TestSignInLimiterGiveBack(t *testing.T) {
	l, _ := newTestLimiter()
	for i := 0; i < 4; i++ {
		l.allow("admin", true)
	}
	if wait, ok, _ := l.allow("admin", true); !ok || wait != 0 {
		t.Fatalf("the fifth attempt = (%v, %v), want it counted and checked", wait, ok)
	}
	l.giveBack("admin", true)
	if wait, ok, _ := l.allow("admin", true); !ok {
		t.Errorf("after a give-back the next attempt was refused for %v; the attempt that never ran was still counted", wait)
	}
}

func TestRefusedAttemptsDoNotExtendTheLock(t *testing.T) {
	l, now := newTestLimiter()
	for i := 0; i < 5; i++ {
		l.allow("admin", true)
	}
	for i := 0; i < 20; i++ {
		if _, ok, _ := l.allow("admin", true); ok {
			t.Fatal("a refused attempt was allowed")
		}
	}
	*now = now.Add(31 * time.Second)
	if wait, ok, _ := l.allow("admin", true); !ok {
		t.Errorf("after the 30-second wait a burst of refused attempts still locked the name for %v", wait)
	}
}

func TestConcurrentAttemptsAfterFourFailuresCheckOnlyOne(t *testing.T) {
	l, _ := newTestLimiter()
	for i := 0; i < 4; i++ {
		l.allow("admin", true)
	}
	var wg sync.WaitGroup
	allowed := make(chan struct{}, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, ok, _ := l.allow("admin", true); ok {
				allowed <- struct{}{}
			}
		}()
	}
	wg.Wait()
	close(allowed)
	if n := len(allowed); n != 1 {
		t.Errorf("%d simultaneous attempts were checked after 4 failures, want exactly 1", n)
	}
}

func TestUnknownNamesAreCappedAndTheOldestGoes(t *testing.T) {
	l, now := newTestLimiter()
	for i := 0; i < maxUnknownSignInNames; i++ {
		l.allow(fmt.Sprintf("name-%d", i), false)
		*now = now.Add(time.Millisecond)
	}
	if len(l.unknown) != maxUnknownSignInNames {
		t.Fatalf("unknown map holds %d, want %d", len(l.unknown), maxUnknownSignInNames)
	}
	l.allow("one-more", false)
	if len(l.unknown) != maxUnknownSignInNames {
		t.Errorf("unknown map holds %d after the flood, want %d", len(l.unknown), maxUnknownSignInNames)
	}
	if l.unknown[sha256.Sum256([]byte("name-0"))] != nil {
		t.Error("the oldest unknown name survived the cap")
	}
}

func TestAFloodOfUnknownNamesLeavesARealAccountLocked(t *testing.T) {
	l, _ := newTestLimiter()
	for i := 0; i < 5; i++ {
		l.allow("admin", true)
	}
	for i := 0; i < maxUnknownSignInNames+1; i++ {
		l.allow(fmt.Sprintf("name-%d", i), false)
	}
	if wait, ok, _ := l.allow("admin", true); ok {
		t.Errorf("a flood of made-up names unlocked a real account (wait %v)", wait)
	}
}

func TestALongNameCostsOneHashedKey(t *testing.T) {
	l, _ := newTestLimiter()
	l.allow(strings.Repeat("x", 1<<20), false)
	if len(l.unknown) != 1 {
		t.Errorf("unknown map holds %d entries for one name", len(l.unknown))
	}
}

func TestSignInLockedErrorCopy(t *testing.T) {
	for _, tc := range []struct {
		wait  time.Duration
		copy  string
		retry int
	}{
		{30 * time.Second, "Too many failed sign-ins for this username. Try again in less than a minute.", 30},
		{time.Minute, "Too many failed sign-ins for this username. Try again in 1 minute.", 60},
		{4 * time.Minute, "Too many failed sign-ins for this username. Try again in 4 minutes.", 240},
	} {
		e := &signInLockedError{wait: tc.wait}
		if got := e.Error(); got != tc.copy {
			t.Errorf("wait %v: Error() = %q, want %q", tc.wait, got, tc.copy)
		}
		if got := e.retryAfter(); got != tc.retry {
			t.Errorf("wait %v: retryAfter() = %d, want %d", tc.wait, got, tc.retry)
		}
	}
}

func TestSignInLimiterReportsWhenAnAttemptSetsAWait(t *testing.T) {
	l, now := newTestLimiter()
	for i := 1; i <= 4; i++ {
		if _, ok, lockedNow := l.allow("admin", true); !ok || lockedNow {
			t.Fatalf("attempt %d = ok %v, lockedNow %v; want allowed and no wait", i, ok, lockedNow)
		}
	}
	if _, ok, lockedNow := l.allow("admin", true); !ok || !lockedNow {
		t.Fatalf("attempt 5 = ok %v, lockedNow %v; want allowed and a wait set", ok, lockedNow)
	}
	if _, ok, lockedNow := l.allow("admin", true); ok || lockedNow {
		t.Errorf("a refused attempt = ok %v, lockedNow %v; want refused and not lockedNow", ok, lockedNow)
	}
	*now = now.Add(31 * time.Second)
	if _, ok, lockedNow := l.allow("admin", true); !ok || !lockedNow {
		t.Errorf("attempt 6 = ok %v, lockedNow %v; want allowed and a wait set", ok, lockedNow)
	}
}
