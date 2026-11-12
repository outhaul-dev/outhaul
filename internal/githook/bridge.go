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
	"github.com/outhaul-dev/outhaul/internal/store"
	"github.com/outhaul-dev/outhaul/internal/tagdeploy"
)

// Store is the slice of the store the bridge needs.
type Store interface {
	GetAppByName(ctx context.Context, name string) (core.App, error)
	CreateApp(ctx context.Context, app core.App) (core.App, error)
	CreateAppProtected(ctx context.Context, app core.App) (core.App, error)
	OtherProjectsOnHost(ctx context.Context, host string, projectID int64) (anyRow, protected bool, err error)
	CreateDeployment(ctx context.Context, appID int64) (core.Deployment, error)
	EnqueueTagDeployment(ctx context.Context, appID int64, choose func(inPlay []string) (tag string, ok bool)) (core.Deployment, bool, error)
	GetDeployment(ctx context.Context, id int64) (core.Deployment, error)
	GetPushToken(ctx context.Context, id int64) (core.PushToken, error)
	GetUser(ctx context.Context, id int64) (core.User, error)
}

// Notifier wakes the deploy worker (the worker's Notify satisfies it).
type Notifier interface{ Notify() }

// Bridge turns a relayed push into an enqueued, streamed, gated deployment.
type Bridge struct {
	store      Store
	broker     *logstream.Broker
	notifier   Notifier
	repos      *gitrepo.Manager
	naming     core.Naming   // for cold-push generated hosts; a zero value disables them
	protectNew bool          // OUTHAUL_PROTECT_NEW_DOMAINS; see coldCreate
	timeout    time.Duration // overall per-push deadline
	publicURL  string        // for an HTTPS push's build-log line; "" prints none
}

// NewBridge wires the bridge. timeout bounds a single push→deploy.
func NewBridge(st Store, br *logstream.Broker, n Notifier, repos *gitrepo.Manager, naming core.Naming, protectNew bool, timeout time.Duration) *Bridge {
	return &Bridge{store: st, broker: br, notifier: n, repos: repos, naming: naming, protectNew: protectNew, timeout: timeout}
}

// SetPublicURL sets the address an HTTPS push's build-log line points at. Call
// it before Serve; "" prints no such line.
func (b *Bridge) SetPublicURL(u string) { b.publicURL = strings.TrimRight(u, "/") }

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

	if req.Origin.HTTPS() {
		b.logHTTPSPush(ctx, app, req)
	}

	if app.DeployTrigger == core.TriggerTag {
		return b.serveTagPush(ctx, conn, app, req)
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
	dep, err := b.store.CreateDeployment(ctx, app.ID)
	if err != nil {
		fmt.Fprintf(conn, "✗ enqueue failed: %v\n", err)
		return 1
	}
	return b.runDeploy(ctx, conn, app, dep, req.Origin)
}

// logHTTPSPush records who pushed over HTTPS: the account, the token's id and
// label (never its value), the client address, the app and each ref's new
// commit. A token or account deleted since the push began is named as removed.
func (b *Bridge) logHTTPSPush(ctx context.Context, app core.App, req request) {
	id := req.Origin.TokenID
	who, token := "an account that has been removed", fmt.Sprintf("token %d (removed)", id)
	if tok, err := b.store.GetPushToken(ctx, id); err == nil {
		token = fmt.Sprintf("token %d %q", id, tok.Label)
		if u, err := b.store.GetUser(ctx, tok.UserID); err == nil {
			who = fmt.Sprintf("%q (user %d)", u.Display(), u.ID)
		}
	}
	addr := req.Origin.Addr
	if addr == "" {
		addr = "an unknown address"
	}
	refs := make([]string, 0, len(req.Refs))
	for _, u := range req.Refs {
		refs = append(refs, fmt.Sprintf("%q", u.Ref+" "+u.New))
	}
	log.Printf("git push over HTTPS: %s, %s, from %s, to app %s: %s", who, token, addr, app.Name, strings.Join(refs, ", "))
}

// serveTagPush handles a push to a tag-mode app: branches are only stored, and
// the highest eligible pushed tag is deployed when it is newer than what is
// live or already queued.
func (b *Bridge) serveTagPush(ctx context.Context, conn io.ReadWriter, app core.App, req request) int {
	tags := tagRefs(req.Refs)
	if len(tags) == 0 {
		fmt.Fprintf(conn, "→ stored %d ref(s); this app deploys tags matching %q, and no tag was pushed, so no deploy\n",
			len(req.Refs), app.TagPattern)
		return 0
	}
	var decision tagdeploy.Decision
	dep, ok, err := b.store.EnqueueTagDeployment(ctx, app.ID, func(inPlay []string) (string, bool) {
		decision = tagdeploy.PolicyFor(app).Decide(tags, inPlay)
		return decision.Tag, decision.Tag != ""
	})
	if err != nil {
		fmt.Fprintf(conn, "✗ enqueue failed: %v\n", err)
		return 1
	}
	if !ok {
		fmt.Fprintf(conn, "→ stored %d ref(s); %s, so no deploy\n", len(req.Refs), decision.Reason)
		return 0
	}
	fmt.Fprintf(conn, "→ deploying tag %s\n", dep.Tag)
	return b.runDeploy(ctx, conn, app, dep, req.Origin)
}

// tagRefs returns the names of the refs/tags/* updates in push order,
// skipping deletions (an all-zero new object id).
func tagRefs(refs []RefUpdate) []string {
	var tags []string
	for _, u := range refs {
		name, ok := strings.CutPrefix(u.Ref, "refs/tags/")
		if !ok || name == "" || strings.Trim(u.New, "0") == "" {
			continue
		}
		tags = append(tags, name)
	}
	return tags
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
	host, hasHost := b.naming.AppHost(req.App)
	if kind == core.KindCompose {
		host, hasHost = "", false
	}
	if b.protectNew && hasHost {
		// A sign-in holds for a whole host, so only a system admin may bring
		// two projects together there. A push has no request to ask with, so
		// ask the store: a cold push is always on the default project.
		anyRow, _, err := b.store.OtherProjectsOnHost(ctx, host, core.DefaultProjectID)
		if err != nil {
			return core.App{}, fmt.Errorf("check who uses %s: %w", host, err)
		}
		if anyRow {
			return core.App{}, fmt.Errorf("an app named '%s' would get %s, which another project already uses; ask a system admin to set it up on the app page, or push under another name",
				req.App, host)
		}
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
	if hasHost {
		app.Domain = host
	}
	// No suffix check here: the only host this mints comes from
	// naming.AppHost, which is one label under the suffix by construction, and
	// the store refuses it if the suffix is ever wrong.
	// The app and its route are created together, so a refused route leaves
	// no app behind. This checks rows only, not what a container still serves:
	// leftoverRoutes reads containers, which the bridge has no reason to list.
	var created core.App
	if b.protectNew && hasHost {
		created, err = b.store.CreateAppProtected(ctx, app)
	} else {
		created, err = b.store.CreateApp(ctx, app)
	}
	if errors.Is(err, store.ErrHostReserved) {
		return core.App{}, fmt.Errorf("an app named '%s' would get %s, which is Outhaul's own address; push under another name",
			req.App, app.Domain)
	}
	var taken *store.DomainTakenError
	if errors.As(err, &taken) {
		return core.App{}, fmt.Errorf("an app named '%s' would get %s, which another app serves; push under another name",
			req.App, app.Domain)
	}
	if err != nil {
		return core.App{}, fmt.Errorf("create app: %w", err)
	}
	return created, nil
}

// runDeploy streams the enqueued deployment's log to conn and returns the
// gated exit code. An HTTPS push first gets the address of its build log.
func (b *Bridge) runDeploy(ctx context.Context, conn io.ReadWriter, app core.App, dep core.Deployment, o Origin) int {
	history, lines, cancel := b.broker.Subscribe(dep.ID)
	defer cancel()
	b.notifier.Notify()

	if o.HTTPS() && b.publicURL != "" {
		fmt.Fprintf(conn, "→ build log: %s/deployments/%d\n", b.publicURL, dep.ID)
	}
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
	if skip, ok := strings.CutPrefix(reason, "skipped: "); ok && dep.Status == core.StatusCancelled {
		// A claim-time skip (an older tag, say) deploys nothing but isn't a failure.
		fmt.Fprintf(conn, "→ %s, so no deploy\n", skip)
		return 0
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
