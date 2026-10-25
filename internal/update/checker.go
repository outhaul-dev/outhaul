package update

import (
	"context"
	"sync"
	"time"
)

const (
	checkCacheTTL = 10 * time.Minute
	checkRunEvery = 6 * time.Hour
)

// Check is one result of comparing the running build with main.
type Check struct {
	Build      Build
	Comparison Comparison
	Err        error
	At         time.Time
}

// Checker caches the comparison of the running build with main. It is safe
// for concurrent use.
type Checker struct {
	build Build
	cmp   *Comparer
	now   func() time.Time

	fetch sync.Mutex // one request at a time; later callers see its result
	mu    sync.Mutex // guards last
	last  *Check
}

// NewChecker returns a Checker for build b.
func NewChecker(b Build, c *Comparer, now func() time.Time) *Checker {
	return &Checker{build: b, cmp: c, now: now}
}

func (k *Checker) cached() (Check, bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.last == nil {
		return Check{}, false
	}
	return *k.last, true
}

// Get returns the cached check if it is under ten minutes old, otherwise (or
// when force is set) asks GitHub again. Errors are cached like results.
func (k *Checker) Get(ctx context.Context, force bool) Check {
	if !force {
		if c, ok := k.cached(); ok && k.now().Sub(c.At) < checkCacheTTL {
			return c
		}
	}
	k.fetch.Lock()
	defer k.fetch.Unlock()
	started := k.now()
	if !force {
		// Another caller may have refreshed while we waited.
		if c, ok := k.cached(); ok && started.Sub(c.At) < checkCacheTTL {
			return c
		}
	}
	c := Check{Build: k.build, At: started}
	if !k.build.Known() {
		c.Err = ErrUnknownBuild
	} else {
		c.Comparison, c.Err = k.cmp.Compare(ctx, k.build.Repo, k.build.Commit)
	}
	if ctx.Err() != nil {
		// The caller gave up; that says nothing about GitHub, so don't let it
		// stand in for everyone else's answer.
		return c
	}
	k.mu.Lock()
	k.last = &c
	k.mu.Unlock()
	return c
}

// Run checks at once and then every six hours, until ctx is done.
func (k *Checker) Run(ctx context.Context) {
	k.Get(ctx, true)
	t := time.NewTicker(checkRunEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			k.Get(ctx, true)
		}
	}
}

// UpdateAvailable reports whether the last check found main ahead.
func (k *Checker) UpdateAvailable() bool {
	c, ok := k.cached()
	return ok && c.Err == nil && c.Comparison.Status == CompareBehind
}
