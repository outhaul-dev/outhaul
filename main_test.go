package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/outhaul-dev/outhaul/internal/config"
)

// An SSE handler only returns when its stream ends, so http.Server.Shutdown
// must end the streams itself or it waits out its whole timeout (and serve
// exits 1). newHTTPServer runs onShutdown as Shutdown begins.
func TestHTTPServerShutdownEndsStreams(t *testing.T) {
	streamsClosed := make(chan struct{})
	entered := make(chan struct{})
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		close(entered)
		<-streamsClosed // a log stream that only ends when told to
	})
	srv := newHTTPServer("127.0.0.1:0", h, func() { close(streamsClosed) })

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln)
	resp, err := http.Get("http://" + ln.Addr().String() + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	<-entered

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown with an open stream: %v", err)
	}
}

// shutdown stops the backup manager (cancelling in-flight runs so each records
// its outcome) within a bounded wait, alongside the deploy worker.
func TestShutdownStopsBackups(t *testing.T) {
	workerDone := make(chan struct{})
	close(workerDone)
	var stopCtx context.Context
	stopBackups := func(ctx context.Context) error {
		stopCtx = ctx
		return nil
	}
	srv := newHTTPServer("127.0.0.1:0", http.NotFoundHandler(), func() {})

	if err := shutdown(srv, func() {}, workerDone, stopBackups); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if stopCtx == nil {
		t.Fatal("shutdown did not stop the backup manager")
	}
	if _, ok := stopCtx.Deadline(); !ok {
		t.Error("backups were stopped without a deadline: a hung run would block shutdown")
	}
}

// A missing secret.key used to be regenerated silently, leaving every sealed
// value (env vars, database passwords, GitHub App keys) undecryptable.
func TestOpenStoreRefusesNewKeyOverSealedValues(t *testing.T) {
	cfg := config.Config{DataDir: t.TempDir()}
	st, err := openStore(cfg)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	if err := st.SetCloudflareToken(context.Background(), "tok"); err != nil {
		t.Fatal(err)
	}
	st.Close()
	if err := os.Remove(cfg.SecretKeyPath()); err != nil {
		t.Fatal(err)
	}

	st, err = openStore(cfg)
	if err == nil {
		st.Close()
		t.Fatal("opened the store with a new key over values sealed with the lost one")
	}
	if !strings.Contains(err.Error(), cfg.SecretKeyPath()) {
		t.Errorf("error = %q, want it to name the missing key file", err)
	}
	if _, serr := os.Stat(cfg.SecretKeyPath()); !errors.Is(serr, os.ErrNotExist) {
		t.Errorf("a replacement key file was written (stat err = %v)", serr)
	}
}

// Nothing sealed yet (fresh install, or the key went before any secret was
// stored): generating a key is safe.
func TestOpenStoreGeneratesKeyWhenNothingIsSealed(t *testing.T) {
	cfg := config.Config{DataDir: t.TempDir()}
	st, err := openStore(cfg)
	if err != nil {
		t.Fatalf("fresh open: %v", err)
	}
	st.Close()
	if err := os.Remove(cfg.SecretKeyPath()); err != nil {
		t.Fatal(err)
	}
	st, err = openStore(cfg)
	if err != nil {
		t.Fatalf("reopen with no sealed values: %v", err)
	}
	st.Close()
	if _, err := os.Stat(cfg.SecretKeyPath()); err != nil {
		t.Errorf("no key file after open: %v", err)
	}
}
