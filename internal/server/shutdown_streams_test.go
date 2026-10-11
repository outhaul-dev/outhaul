package server

import (
	"bufio"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/outhaul-dev/outhaul/internal/core"
)

// readUntil reads SSE lines from br until one contains want.
func readUntil(t *testing.T, br *bufio.Reader, want string) {
	t.Helper()
	for {
		line, err := br.ReadString('\n')
		if strings.Contains(line, want) {
			return
		}
		if err != nil {
			t.Fatalf("stream ended before %q: %v", want, err)
		}
	}
}

// A runtime-log stream follows the container until it stops, so an open app
// page used to keep http.Server.Shutdown waiting out its whole timeout and
// the process exited 1 on every restart. CloseStreams ends it.
func TestCloseStreamsEndsRuntimeLogStream(t *testing.T) {
	e := newTestEnv(t)
	e.login(t)
	app := seedRunningApp(t, e, "")
	pr, pw := io.Pipe()
	e.runtime.follow = pr
	go pw.Write([]byte("listening on :3000\n")) // then the container keeps running

	resp, err := e.getWithin(t, "/apps/"+itoa(app.ID)+"/logs", 3*time.Second)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	br := bufio.NewReader(resp.Body)
	readUntil(t, br, "listening on :3000")

	e.srv.CloseStreams()
	if _, err := io.ReadAll(br); err != nil {
		t.Fatalf("runtime log stream did not end after CloseStreams: %v", err)
	}
}

// A queued deployment's log stream waits for a pipeline that will not run
// once the worker has stopped; CloseStreams ends it too.
func TestCloseStreamsEndsBuildLogStream(t *testing.T) {
	e := newTestEnv(t)
	e.completeSetup(t)
	ctx := context.Background()
	app, _ := e.store.CreateApp(ctx, core.App{Name: "web", RepoURL: "https://x/y.git", Domain: "web.test"})
	dep, _ := e.store.CreateDeployment(ctx, app.ID)
	e.broker.Publish(dep.ID, "waiting for a build slot")

	resp, err := e.getWithin(t, "/deployments/"+itoa(dep.ID)+"/logs", 3*time.Second)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	br := bufio.NewReader(resp.Body)
	readUntil(t, br, "waiting for a build slot")

	e.srv.CloseStreams()
	if _, err := io.ReadAll(br); err != nil {
		t.Fatalf("build log stream did not end after CloseStreams: %v", err)
	}
}
