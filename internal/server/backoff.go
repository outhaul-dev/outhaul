package server

import (
	"crypto/sha256"
	"fmt"
	"sync"
	"time"
)

// maxUnknownSignInNames bounds the map for names that are not password
// accounts, so a flood of made-up names cannot grow memory without limit.
const maxUnknownSignInNames = 10_000

// signInFailures is one name's failed attempts. count is the failures in a
// row, last the most recent, and until when the name may next be tried.
type signInFailures struct {
	count int
	last  time.Time
	until time.Time
}

// signInLimiter counts failed password sign-ins per typed username. Names are
// kept as their SHA-256, so a name as long as the request cap allows still
// costs 32 bytes. The zero value is not ready; use newSignInLimiter.
type signInLimiter struct {
	mu      sync.Mutex
	known   map[[32]byte]*signInFailures // names GetUserByName finds
	unknown map[[32]byte]*signInFailures // every other name, capped
	now     func() time.Time
}

func newSignInLimiter(now func() time.Time) *signInLimiter {
	return &signInLimiter{
		known:   map[[32]byte]*signInFailures{},
		unknown: map[[32]byte]*signInFailures{},
		now:     now,
	}
}

// lockFor is how long a name is refused after failures failed sign-ins:
// 1 to 4 are not refused, 5 for 30 seconds, 6 to 9 for 1, 2, 4 and 8 minutes,
// and 10 or more for 15 minutes.
func lockFor(failures int) time.Duration {
	switch {
	case failures < 5:
		return 0
	case failures == 5:
		return 30 * time.Second
	case failures == 6:
		return time.Minute
	case failures == 7:
		return 2 * time.Minute
	case failures == 8:
		return 4 * time.Minute
	case failures == 9:
		return 8 * time.Minute
	default:
		return 15 * time.Minute
	}
}

// allow counts one attempt for name. ok false means it is refused for wait and
// is not counted. An allowed attempt is counted before its password is
// checked, and sets any wait the table gives it.
func (l *signInLimiter) allow(name string, known bool) (wait time.Duration, ok bool) {
	key := sha256.Sum256([]byte(name))
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()

	m := l.unknown
	if known {
		m = l.known
	}
	if f, exists := m[key]; exists {
		switch {
		case now.Sub(f.last) >= time.Hour:
			delete(m, key)
		case now.Before(f.until):
			return f.until.Sub(now), false
		}
	}
	if !known && len(l.unknown) >= maxUnknownSignInNames {
		if _, exists := l.unknown[key]; !exists {
			l.evictOldestUnknown()
		}
	}
	f := m[key]
	if f == nil {
		f = &signInFailures{}
		m[key] = f
	}
	f.count++
	f.last = now
	f.until = now.Add(lockFor(f.count))
	return 0, true
}

// clear forgets name, after a successful sign-in or a completed setup link.
func (l *signInLimiter) clear(name string) {
	key := sha256.Sum256([]byte(name))
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.known, key)
	delete(l.unknown, key)
}

// giveBack undoes an allowed attempt whose password was never checked because
// no hash slot came free.
func (l *signInLimiter) giveBack(name string, known bool) {
	key := sha256.Sum256([]byte(name))
	l.mu.Lock()
	defer l.mu.Unlock()
	m := l.unknown
	if known {
		m = l.known
	}
	f := m[key]
	if f == nil {
		return
	}
	f.count--
	if f.count <= 0 {
		delete(m, key)
		return
	}
	f.until = f.last.Add(lockFor(f.count))
}

// evictOldestUnknown removes the unknown name whose last failure is oldest.
// Ties are arbitrary; all such names are equally valueless.
func (l *signInLimiter) evictOldestUnknown() {
	var (
		oldestKey [32]byte
		oldest    time.Time
		first     = true
	)
	for key, f := range l.unknown {
		if first || f.last.Before(oldest) {
			oldestKey, oldest, first = key, f.last, false
		}
	}
	delete(l.unknown, oldestKey)
}

// signInLockedError is a refused sign-in: the name is locked and wait remains.
type signInLockedError struct{ wait time.Duration }

func (e *signInLockedError) Error() string {
	seconds := e.retryAfter()
	if seconds < 60 {
		return "Too many failed sign-ins for this username. Try again in less than a minute."
	}
	minutes := (seconds + 59) / 60
	if minutes == 1 {
		return "Too many failed sign-ins for this username. Try again in 1 minute."
	}
	return fmt.Sprintf("Too many failed sign-ins for this username. Try again in %d minutes.", minutes)
}

// retryAfter is the wait in whole seconds, rounded up for Retry-After.
func (e *signInLockedError) retryAfter() int {
	return int((e.wait + time.Second - 1) / time.Second)
}
