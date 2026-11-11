package server

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/outhaul-dev/outhaul/internal/core"
	"github.com/outhaul-dev/outhaul/internal/docker"
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

func TestRuntimeLogStreamPingsWhileQuiet(t *testing.T) {
	e := newTestEnv(t)
	e.login(t)
	app := seedRunningApp(t, e, "")
	pr, pw := io.Pipe()
	e.runtime.follow = pr
	shortPing(t)
	defer pw.Close()

	resp, err := e.getWithin(t, "/apps/"+itoa(app.ID)+"/logs", 2*time.Second)
	if err != nil {
		t.Fatalf("headers did not arrive while the container was quiet: %v", err)
	}
	defer resp.Body.Close()
	br := bufio.NewReader(resp.Body)
	readSSEUntil(t, br, ": ping\n\n")
	pw.Close()
	if got := readSSEUntil(t, br, "event: done"); !strings.Contains(got, "event: done") {
		t.Fatalf("stream did not end with done:\n%s", got)
	}
}

func TestDatabaseLogStreamPingsWhileQuiet(t *testing.T) {
	e := newTestEnv(t)
	e.login(t)
	d := seedDatabase(t, e, "shop", core.EnginePostgres)
	e.runtime.container = &docker.Container{ID: "db1", Name: "outhaul-db-shop", State: "running"}
	pr, pw := io.Pipe()
	e.runtime.follow = pr
	shortPing(t)
	defer pw.Close()

	resp, err := e.getWithin(t, "/databases/"+itoa(d.ID)+"/logs", 2*time.Second)
	if err != nil {
		t.Fatalf("headers did not arrive while the container was quiet: %v", err)
	}
	defer resp.Body.Close()
	br := bufio.NewReader(resp.Body)
	readSSEUntil(t, br, ": ping\n\n")
	pw.Close()
	if got := readSSEUntil(t, br, "event: done"); !strings.Contains(got, "event: done") {
		t.Fatalf("stream did not end with done:\n%s", got)
	}
}

func TestRuntimeLogStreamEndsWhenStreamsClose(t *testing.T) {
	e := newTestEnv(t)
	e.login(t)
	app := seedRunningApp(t, e, "")
	pr, pw := io.Pipe()
	e.runtime.follow = pr
	shortPing(t)
	defer pw.Close()

	resp, err := e.getWithin(t, "/apps/"+itoa(app.ID)+"/logs", 2*time.Second)
	if err != nil {
		t.Fatalf("headers did not arrive while the container was quiet: %v", err)
	}
	defer resp.Body.Close()
	br := bufio.NewReader(resp.Body)
	readSSEUntil(t, br, ": ping\n\n")
	e.srv.CloseStreams()
	rest, err := io.ReadAll(br)
	if err != nil {
		t.Fatalf("reading after CloseStreams: %v", err)
	}
	if strings.Contains(string(rest), "event: done") {
		t.Errorf("stream ended with done after CloseStreams:\n%s", rest)
	}
}

// blockingLogs is a follow stream that blocks in Read until the handler closes
// it. The closed channel records that Close happened, so the test can see the
// scanner was released even though it cannot see the goroutine itself.
type blockingLogs struct {
	closed chan struct{}
	once   sync.Once
}

func (b *blockingLogs) Read([]byte) (int, error) {
	<-b.closed
	return 0, io.EOF
}

func (b *blockingLogs) Close() error {
	b.once.Do(func() { close(b.closed) })
	return nil
}

func TestRuntimeLogStreamCancelsTheScanOnClientDisconnect(t *testing.T) {
	e := newTestEnv(t)
	e.login(t)
	app := seedRunningApp(t, e, "")
	logs := &blockingLogs{closed: make(chan struct{})}
	e.runtime.follow = logs
	shortPing(t)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.http.URL+"/apps/"+itoa(app.ID)+"/logs", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatalf("headers did not arrive while the container was quiet: %v", err)
	}
	defer resp.Body.Close()
	br := bufio.NewReader(resp.Body)
	readSSEUntil(t, br, ": ping\n\n")

	cancel()
	select {
	case <-logs.closed:
	case <-time.After(2 * time.Second):
		t.Fatal("the handler did not close the log stream after the client went away")
	}
	rest, _ := io.ReadAll(br)
	if strings.Contains(string(rest), "event: done") {
		t.Errorf("stream ended with done after the client went away:\n%s", rest)
	}
}
