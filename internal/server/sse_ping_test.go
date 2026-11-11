package server

import (
	"bufio"
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/outhaul-dev/outhaul/internal/core"
)

// shortPing makes every stream ping quickly for one test.
func shortPing(t *testing.T) {
	t.Helper()
	old := ssePingEvery
	ssePingEvery = 10 * time.Millisecond
	t.Cleanup(func() { ssePingEvery = old })
}

// readSSEUntil reads lines from br until they contain want. The request's
// deadline ends the read, so a stream that never sends want fails the test
// instead of hanging it.
func readSSEUntil(t *testing.T, br *bufio.Reader, want string) string {
	t.Helper()
	var got strings.Builder
	for {
		line, err := br.ReadString('\n')
		got.WriteString(line)
		if strings.Contains(got.String(), want) {
			return got.String()
		}
		if err != nil {
			t.Fatalf("stream ended before %q: %v\n%s", want, err, got.String())
		}
	}
}

func TestWriteSSEComment(t *testing.T) {
	rec := httptest.NewRecorder()
	writeSSEComment(rec, "ping")
	if got := rec.Body.String(); got != ": ping\n\n" {
		t.Errorf("body = %q, want %q", got, ": ping\n\n")
	}
}

func TestBuildLogStreamPingsWhileQuiet(t *testing.T) {
	e := newTestEnv(t)
	e.login(t)
	app, err := e.store.CreateApp(context.Background(), core.App{Name: "web", RepoURL: "https://x/y.git", Domain: "web.test"})
	if err != nil {
		t.Fatal(err)
	}
	dep, err := e.store.CreateDeployment(context.Background(), app.ID)
	if err != nil {
		t.Fatal(err)
	}
	shortPing(t)

	resp, err := e.getWithin(t, "/deployments/"+itoa(dep.ID)+"/logs", 2*time.Second)
	if err != nil {
		t.Fatalf("the log stream did not open while quiet: %v", err)
	}
	defer resp.Body.Close()
	br := bufio.NewReader(resp.Body)
	if got := readSSEUntil(t, br, ": ping\n\n"); !strings.Contains(got, ": ping\n\n") {
		t.Fatalf("no ping before the deadline:\n%s", got)
	}
}
