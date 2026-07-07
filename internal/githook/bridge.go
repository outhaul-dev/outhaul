package githook

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"strings"
	"time"

	"github.com/outhaul-dev/outhaul/internal/core"
	"github.com/outhaul-dev/outhaul/internal/gitrepo"
	"github.com/outhaul-dev/outhaul/internal/logstream"
)

// Store is the slice of the store the bridge needs.
type Store interface {
	GetAppByName(ctx context.Context, name string) (core.App, error)
	CreateApp(ctx context.Context, app core.App) (core.App, error)
	CreateDeployment(ctx context.Context, appID int64) (core.Deployment, error)
	GetDeployment(ctx context.Context, id int64) (core.Deployment, error)
}

// Notifier wakes the deploy worker (the worker's Notify satisfies it).
type Notifier interface{ Notify() }

// Bridge turns a relayed push into an enqueued, streamed, gated deployment.
type Bridge struct {
	store    Store
	broker   *logstream.Broker
	notifier Notifier
	repos    *gitrepo.Manager
	serverIP string        // for cold-push sslip.io domains; "" disables
	timeout  time.Duration // overall per-push deadline
}

// NewBridge wires the bridge. timeout bounds a single push→deploy.
func NewBridge(st Store, br *logstream.Broker, n Notifier, repos *gitrepo.Manager, serverIP string, timeout time.Duration) *Bridge {
	return &Bridge{store: st, broker: br, notifier: n, repos: repos, serverIP: serverIP, timeout: timeout}
}

// Handle serves one hook connection: read the request, run the deploy, stream
// output back, and write the trailing `exit <code>` line. The terminal line is
// NUL-prefixed so a build-log line that happens to start with "exit " cannot be
// mistaken for the sentinel by the client.
func (b *Bridge) Handle(ctx context.Context, conn io.ReadWriter) {
	ctx, cancel := context.WithTimeout(ctx, b.timeout)
	defer cancel()

	code := 1
	func() {
		defer func() {
			if r := recover(); r != nil {
				fmt.Fprintf(conn, "✗ internal error\n")
				log.Printf("githook: recovered panic handling push: %v", r)
				code = 1
			}
		}()
		code = b.serve(ctx, conn)
	}()
	fmt.Fprintf(conn, "\x00exit %d\n", code)
}

// serve runs the push and returns the exit code (0 = success).
func (b *Bridge) serve(ctx context.Context, conn io.ReadWriter) int {
	br := bufio.NewReader(conn)
	req, err := readRequest(br)
	if err != nil {
		fmt.Fprintf(conn, "✗ %v\n", err)
		return 1
	}

	app, err := b.resolveOrCreate(ctx, req)
	if err != nil {
		fmt.Fprintf(conn, "✗ %v\n", err)
		return 1
	}

	deployRef := "refs/heads/" + app.Branch
	pushedDeployBranch := false
	for _, u := range req.Refs {
		if u.Ref == deployRef {
			pushedDeployBranch = true
		}
	}
	if !pushedDeployBranch {
		fmt.Fprintf(conn, "→ stored %d ref(s); none is the deploy branch '%s', so no deploy\n",
			len(req.Refs), app.Branch)
		return 0
	}
	return b.runDeploy(ctx, conn, app)
}

// resolveOrCreate finds the push app by name, rejecting a non-push app, and
// cold-creates one (detecting kind from the pushed tip) when absent.
func (b *Bridge) resolveOrCreate(ctx context.Context, req request) (core.App, error) {
	app, err := b.store.GetAppByName(ctx, req.App)
	if err == nil {
		if app.Source != core.SourcePush {
			return core.App{}, fmt.Errorf("app '%s' is not push-deployable", req.App)
		}
		return app, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return core.App{}, err
	}
	return b.coldCreate(ctx, req)
}

// coldCreate builds a new push app from the first pushed branch.
func (b *Bridge) coldCreate(ctx context.Context, req request) (core.App, error) {
	if !core.ValidAppName(req.App) {
		return core.App{}, fmt.Errorf("invalid app name %q", req.App)
	}
	if len(req.Refs) == 0 {
		return core.App{}, fmt.Errorf("nothing pushed")
	}
	first := req.Refs[0]
	branch := strings.TrimPrefix(first.Ref, "refs/heads/")
	if branch == first.Ref {
		return core.App{}, fmt.Errorf("cold push must target a branch, got %q", first.Ref)
	}
	dir, err := b.repos.Path(req.App)
	if err != nil {
		return core.App{}, err
	}
	kind, composePath, err := gitrepo.DetectKind(ctx, dir, first.New)
	if err != nil {
		return core.App{}, fmt.Errorf("detect build kind: %w", err)
	}
	app := core.App{
		Name:   req.App,
		Source: core.SourcePush,
		Branch: branch,
		Kind:   kind,
	}
	switch kind {
	case core.KindCompose:
		app.ComposePath = composePath
	case core.KindDockerfile:
		app.DockerfilePath = "Dockerfile"
	}
	if b.serverIP != "" && kind != core.KindCompose {
		app.Domain = req.App + "." + b.serverIP + ".sslip.io"
	}
	created, err := b.store.CreateApp(ctx, app)
	if err != nil {
		return core.App{}, fmt.Errorf("create app: %w", err)
	}
	return created, nil
}

// runDeploy enqueues a deployment, streams its log to conn, and returns the
// gated exit code.
func (b *Bridge) runDeploy(ctx context.Context, conn io.ReadWriter, app core.App) int {
	dep, err := b.store.CreateDeployment(ctx, app.ID)
	if err != nil {
		fmt.Fprintf(conn, "✗ enqueue failed: %v\n", err)
		return 1
	}
	history, lines, cancel := b.broker.Subscribe(dep.ID)
	defer cancel()
	b.notifier.Notify()

	for _, l := range history {
		fmt.Fprintln(conn, l)
	}
	start := time.Now()
	for {
		select {
		case <-ctx.Done():
			fmt.Fprintf(conn, "✗ timed out after %s\n", time.Since(start).Round(time.Second))
			return 1
		case l, ok := <-lines:
			if !ok {
				return b.finish(ctx, conn, dep.ID, app, start)
			}
			fmt.Fprintln(conn, l)
		}
	}
}

// finish reads the terminal status and emits the success/failure summary line.
func (b *Bridge) finish(ctx context.Context, conn io.ReadWriter, depID int64, app core.App, start time.Time) int {
	dep, err := b.store.GetDeployment(ctx, depID)
	if err != nil {
		fmt.Fprintf(conn, "✗ could not read deployment status: %v\n", err)
		return 1
	}
	if dep.Status == core.StatusRunning {
		target := app.Domain
		if target == "" {
			target = app.Name
		}
		fmt.Fprintf(conn, "✓ deployed %s in %s\n", target, time.Since(start).Round(time.Second))
		return 0
	}
	reason := dep.Reason
	if reason == "" {
		reason = string(dep.Status)
	}
	fmt.Fprintf(conn, "✗ deploy failed: %s\n", reason)
	return 1
}

// Serve accepts hook connections on ln until ctx is cancelled.
func (b *Bridge) Serve(ctx context.Context, ln net.Listener) {
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	for {
		conn, err := ln.Accept()
		if err != nil {
			return // listener closed
		}
		go func() {
			defer conn.Close()
			b.Handle(ctx, conn)
		}()
	}
}
