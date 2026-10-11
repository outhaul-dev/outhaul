package main

import (
	"context"
	"net"
	"net/http"
	"testing"
	"time"
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
