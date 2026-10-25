package update

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
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
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newChecker(f *fakeGitHub) (*Checker, *fakeClock) {
	clk := &fakeClock{t: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}
	b := Build{Commit: "abc", Repo: repoURL}
	return NewChecker(b, f.comparer(), clk.now), clk
}

func TestCheckerCachesTenMinutes(t *testing.T) {
	f := newFakeGitHub(t, ghReply{Status: "identical"})
	k, clk := newChecker(f)
	ctx := context.Background()
	k.Get(ctx, false)
	clk.advance(9 * time.Minute)
	got := k.Get(ctx, false)
	if len(f.paths) != 1 {
		t.Fatalf("requests = %d, want 1", len(f.paths))
	}
	if got.Err != nil || got.Comparison.Status != CompareUpToDate || got.Build.Commit != "abc" {
		t.Fatalf("got %+v", got)
	}
	clk.advance(2 * time.Minute)
	k.Get(ctx, false)
	if len(f.paths) != 2 {
		t.Fatalf("requests = %d, want 2", len(f.paths))
	}
	k.Get(ctx, true)
	k.Get(ctx, true)
	if len(f.paths) != 4 {
		t.Fatalf("requests = %d, want 4", len(f.paths))
	}
}

func TestCheckerCachesErrors(t *testing.T) {
	f := newFakeGitHub(t, ghReply{})
	f.code = http.StatusForbidden
	k, clk := newChecker(f)
	ctx := context.Background()
	if got := k.Get(ctx, false); got.Err == nil {
		t.Fatal("want an error")
	}
	clk.advance(5 * time.Minute)
	if got := k.Get(ctx, false); got.Err == nil {
		t.Fatal("want the cached error")
	}
	if len(f.paths) != 1 {
		t.Fatalf("requests = %d, want 1", len(f.paths))
	}
}

func TestCheckerDoesNotCacheACancelledCheck(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	var mu sync.Mutex
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		first := calls == 1
		mu.Unlock()
		if first {
			started <- struct{}{}
			select {
			case <-release:
			case <-r.Context().Done():
			}
			return
		}
		_, _ = w.Write([]byte(`{"status":"identical"}`))
	}))
	defer srv.Close()
	defer close(release)

	clk := &fakeClock{t: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}
	k := NewChecker(Build{Commit: "abc", Repo: repoURL}, &Comparer{APIBase: srv.URL}, clk.now)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan Check)
	go func() { done <- k.Get(ctx, false) }()
	<-started
	cancel()
	if got := <-done; got.Err == nil {
		t.Fatal("the cancelled caller should see an error")
	}

	got := k.Get(context.Background(), false)
	if got.Err != nil || got.Comparison.Status != CompareUpToDate {
		t.Fatalf("a later caller got %+v", got)
	}
}

func TestCheckerUnknownBuild(t *testing.T) {
	f := newFakeGitHub(t, ghReply{Status: "identical"})
	k := NewChecker(Build{}, f.comparer(), time.Now)
	if got := k.Get(context.Background(), false); !errors.Is(got.Err, ErrUnknownBuild) {
		t.Fatalf("err = %v", got.Err)
	}
	if len(f.paths) != 0 {
		t.Fatalf("requests = %v", f.paths)
	}
}

func TestUpdateAvailable(t *testing.T) {
	f := newFakeGitHub(t, ghReply{Status: "ahead", AheadBy: 1})
	k, _ := newChecker(f)
	if k.UpdateAvailable() {
		t.Fatal("true before any check")
	}
	k.Get(context.Background(), false)
	if !k.UpdateAvailable() {
		t.Fatal("want true after behind")
	}
	f.reply = ghReply{Status: "identical"}
	k.Get(context.Background(), true)
	if k.UpdateAvailable() {
		t.Fatal("want false after up to date")
	}
}

func TestCheckerRunChecksAtStart(t *testing.T) {
	f := newFakeGitHub(t, ghReply{Status: "ahead", AheadBy: 1})
	k, _ := newChecker(f)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { k.Run(ctx); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for !k.UpdateAvailable() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	if !k.UpdateAvailable() {
		t.Fatal("Run did not check at start")
	}
}
