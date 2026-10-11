package docker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/docker/docker/client"
)

// fakeDaemon answers just enough of the Engine API for RunContainer and
// ExecContainer. Its hijacked attach/exec streams send one line of output
// once the work has started, then stay silent, like a long dump or untar.
type fakeDaemon struct {
	started chan struct{} // closed when the container is started
	release chan struct{} // closed at test end to free hijacked conns

	mu      sync.Mutex
	removed []string
}

// frame is one stdout record in Docker's multiplexed stream format.
func frame(s string) []byte {
	return append([]byte{1, 0, 0, 0, 0, 0, 0, byte(len(s))}, s...)
}

func (d *fakeDaemon) hijack(w http.ResponseWriter, ready <-chan struct{}) {
	conn, _, err := w.(http.Hijacker).Hijack()
	if err != nil {
		return
	}
	defer conn.Close()
	conn.Write([]byte("HTTP/1.1 101 UPGRADED\r\nContent-Type: application/vnd.docker.raw-stream\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n"))
	select {
	case <-ready:
	case <-d.release:
		return
	}
	conn.Write(frame("working...\n"))
	<-d.release
}

func (d *fakeDaemon) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	switch {
	case strings.HasSuffix(p, "/containers/create"):
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"Id":"c1","Warnings":[]}`))
	case strings.HasSuffix(p, "/containers/c1/exec"):
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"Id":"e1"}`))
	case strings.HasSuffix(p, "/containers/c1/attach"):
		d.hijack(w, d.started)
	case strings.HasSuffix(p, "/exec/e1/start"):
		now := make(chan struct{})
		close(now)
		d.hijack(w, now)
	case strings.HasSuffix(p, "/containers/c1/wait"):
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		select { // the container never exits on its own
		case <-r.Context().Done():
		case <-d.release:
		}
	case strings.HasSuffix(p, "/containers/c1/start"):
		w.WriteHeader(http.StatusNoContent)
		close(d.started)
	case r.Method == http.MethodDelete && strings.HasSuffix(p, "/containers/c1"):
		d.mu.Lock()
		d.removed = append(d.removed, "c1")
		d.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	default:
		http.NotFound(w, r)
	}
}

func newFakeDaemon(t *testing.T) (*fakeDaemon, *real) {
	t.Helper()
	d := &fakeDaemon{started: make(chan struct{}), release: make(chan struct{})}
	ts := httptest.NewServer(d)
	t.Cleanup(func() { close(d.release); ts.Close() })
	cli, err := client.NewClientWithOpts(client.WithHost("tcp://"+ts.Listener.Addr().String()), client.WithVersion("1.47"))
	if err != nil {
		t.Fatal(err)
	}
	return d, &real{cli: cli}
}

// signalWriter closes got on its first write: the caller is reading the
// stream, so the work is under way.
type signalWriter struct {
	once sync.Once
	got  chan struct{}
}

func (s *signalWriter) Write(p []byte) (int, error) {
	s.once.Do(func() { close(s.got) })
	return len(p), nil
}

// returnsAfterCancel runs call, cancels its context once output is flowing,
// and fails unless call returns promptly with an error.
func returnsAfterCancel(t *testing.T, call func(ctx context.Context, stdout *signalWriter) error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := &signalWriter{got: make(chan struct{})}
	errc := make(chan error, 1)
	go func() { errc <- call(ctx, out) }()

	select {
	case <-out.got:
	case err := <-errc:
		t.Fatalf("call returned before any output: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("no output arrived")
	}
	cancel()
	select {
	case err := <-errc:
		if err == nil {
			t.Fatal("cancelled call returned no error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("call ignored its cancelled context: a hijacked stream does not watch ctx")
	}
}

// A backup helper container (or restore untar) that hangs used to hold its
// goroutine forever: the 2h run timeout and shutdown cancellation never
// reached the hijacked attach stream, so the schedule never ran again.
func TestRunContainerHonoursCancel(t *testing.T) {
	d, r := newFakeDaemon(t)
	returnsAfterCancel(t, func(ctx context.Context, stdout *signalWriter) error {
		_, err := r.RunContainer(ctx, ContainerSpec{Name: "outhaul-backup-1", Image: "busybox:stable"}, stdout, nil)
		return err
	})
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.removed) != 1 {
		t.Errorf("helper container removals = %v, want the cancelled helper force-removed", d.removed)
	}
}

func TestExecContainerHonoursCancel(t *testing.T) {
	_, r := newFakeDaemon(t)
	returnsAfterCancel(t, func(ctx context.Context, stdout *signalWriter) error {
		_, err := r.ExecContainer(ctx, "c1", []string{"pg_dump"}, nil, nil, stdout, nil)
		return err
	})
}
