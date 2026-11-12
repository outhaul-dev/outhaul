package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/outhaul-dev/outhaul/internal/config"
	"github.com/outhaul-dev/outhaul/internal/core"
	"github.com/outhaul-dev/outhaul/internal/docker"
	"github.com/outhaul-dev/outhaul/internal/server"
	"github.com/outhaul-dev/outhaul/internal/store"
	"github.com/outhaul-dev/outhaul/internal/traefik"
	"github.com/outhaul-dev/outhaul/internal/tunnel"
	"github.com/outhaul-dev/outhaul/internal/update"
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

// At startup the store learns the admin UI's host, so no writer can give it to
// an app. Rows already on it are named in a warning, one per row, and left as
// they are.
func TestStartupReservesTheAdminHost(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	old, err := st.CreateApp(ctx, core.App{Name: "old", RepoURL: "https://x/y.git", Domain: "outhaul.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddDomain(ctx, core.Domain{AppID: old.ID, Host: "Outhaul.example.com", Path: "/api", Port: 8080}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateApp(ctx, core.App{Name: "other", RepoURL: "https://x/y.git", Domain: "other.example.com"}); err != nil {
		t.Fatal(err)
	}

	var warnings []string
	logf := func(format string, args ...any) { warnings = append(warnings, fmt.Sprintf(format, args...)) }
	reserveAdminHost(ctx, st, config.Config{PublicURL: "https://Outhaul.Example.com:8443/"}.AdminHost(), logf)

	if len(warnings) != 2 {
		t.Fatalf("warnings = %q, want one for each of the old app's two rows", warnings)
	}
	for _, path := range []string{"", "/api"} {
		found := false
		for _, w := range warnings {
			found = found || strings.HasPrefix(w, "WARNING:") && strings.Contains(w, `"old"`) &&
				strings.Contains(w, "Outhaul's own address") && strings.Contains(strings.ToLower(w), "outhaul.example.com"+path+",")
		}
		if !found {
			t.Errorf("warnings = %q, want one naming the app and the route outhaul.example.com%s", warnings, path)
		}
	}
	if rows, _ := st.ListDomains(ctx, old.ID); len(rows) != 2 {
		t.Errorf("the old app's rows = %+v, want both kept", rows)
	}
	if _, err := st.AddDomain(ctx, core.Domain{AppID: old.ID, Host: "outhaul.example.com", Path: "/new", Port: 8080}); !errors.Is(err, store.ErrHostReserved) {
		t.Errorf("AddDomain on the admin host after startup: err = %v, want ErrHostReserved", err)
	}

	// No public URL: nothing is reserved, and nothing is said.
	warnings = nil
	reserveAdminHost(ctx, st, config.Config{}.AdminHost(), logf)
	if len(warnings) != 0 {
		t.Errorf("warnings with no public URL = %q", warnings)
	}
	if _, err := st.AddDomain(ctx, core.Domain{AppID: old.ID, Host: "outhaul.example.com", Path: "/new", Port: 8080}); err != nil {
		t.Errorf("AddDomain with nothing reserved: %v", err)
	}
}

// The first-boot hint is how an installer (or a person reading the journal)
// finds the setup page. When Traefik publishes the admin UI under the public
// URL, that is the address to open: the listen port itself is firewalled.
func TestSetupURLs(t *testing.T) {
	for _, tc := range []struct {
		name          string
		cfg           config.Config
		public, local string
	}{
		{"Let's Encrypt",
			config.Config{ListenAddr: ":8080", PublicURL: "https://paas.example.com", ACMEEmail: "a@example.com"},
			"https://paas.example.com/setup?token=t", "http://localhost:8080/setup?token=t"},
		{"local CA, path dropped (the admin route matches the host only)",
			config.Config{ListenAddr: ":9000", PublicURL: "https://paas.lan/admin/", LocalCA: true},
			"https://paas.lan/setup?token=t", "http://localhost:9000/setup?token=t"},
		{"public URL without TLS publishes no admin route",
			config.Config{ListenAddr: ":8080", PublicURL: "https://paas.example.com"},
			"", "http://localhost:8080/setup?token=t"},
		{"no public URL",
			config.Config{ListenAddr: "127.0.0.1:8080", ACMEEmail: "a@example.com"},
			"", "http://127.0.0.1:8080/setup?token=t"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			public, local := setupURLs(tc.cfg, "t")
			if public != tc.public || local != tc.local {
				t.Fatalf("setupURLs = %q, %q; want %q, %q", public, local, tc.public, tc.local)
			}
		})
	}
}

func TestRunVersionPrintsTheBuild(t *testing.T) {
	var buf strings.Builder
	if code := runVersion(&buf, update.Build{Commit: "abc", Repo: "https://github.com/o/r.git", Version: "v1.2.3"}); code != 0 {
		t.Fatalf("exit %d, want 0", code)
	}
	want := "commit=abc\nrepo=https://github.com/o/r.git\nlocal=false\nversion=v1.2.3\n"
	if buf.String() != want {
		t.Errorf("output = %q, want %q", buf.String(), want)
	}

	// A build with no version (a branch or a full commit was installed) still
	// prints the line, empty: the updater parses key=value lines.
	buf.Reset()
	if code := runVersion(&buf, update.Build{Commit: "abc", Repo: "https://github.com/o/r.git"}); code != 0 {
		t.Fatalf("exit %d, want 0", code)
	}
	want = "commit=abc\nrepo=https://github.com/o/r.git\nlocal=false\nversion=\n"
	if buf.String() != want {
		t.Errorf("output = %q, want %q", buf.String(), want)
	}
}

func TestRunImagesListsManagedImages(t *testing.T) {
	var buf strings.Builder
	cfg := config.Load(func(k string) string { return map[string]string{"OUTHAUL_TRAEFIK_IMAGE": "traefik:v9"}[k] })
	if code := runImages(&buf, cfg); code != 0 {
		t.Fatalf("exit %d, want 0", code)
	}
	want := "traefik traefik:v9\ncloudflared " + config.DefaultCloudflaredImage + "\n"
	if buf.String() != want {
		t.Errorf("output = %q, want %q", buf.String(), want)
	}
}

// A protected route's gate is http://host.docker.internal:<admin port>. While
// a row is protected, Traefik keeps that name mapped to the host gateway,
// whether or not the admin route is published (and in tunnel mode too), so it
// is never looked up on the Docker network instead. reconcile runs at boot and
// when the tunnel is turned on or off.
func TestReconcileMapsTheGateHostWhileARouteIsProtected(t *testing.T) {
	ctx := context.Background()
	adminOn := config.Config{
		DataDir: t.TempDir(), Network: "outhaul", TraefikImage: config.DefaultTraefikImage,
		CloudflaredImage: config.DefaultCloudflaredImage, HTTPSPort: "443", ListenAddr: ":8080",
		PublicURL: "https://outhaul.example.com", ACMEEmail: "ops@example.com", AdminDirect: true,
	}
	adminOff := adminOn
	adminOff.PublicURL = "" // no admin host, so no admin route
	st, err := openStore(adminOn)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	dc := docker.NewFake()
	ic := &infraController{dc: dc, st: st}
	mapped := func(cfg config.Config) bool {
		t.Helper()
		ic.cfg = cfg
		if err := ic.reconcile(ctx); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		var spec *docker.ContainerSpec
		for i := range dc.Created {
			if dc.Created[i].Name == "outhaul-traefik" {
				spec = &dc.Created[i]
			}
		}
		if spec == nil {
			t.Fatal("no Traefik container was created")
		}
		for _, h := range spec.ExtraHosts {
			if h == "host.docker.internal:host-gateway" {
				return true
			}
		}
		return false
	}

	if mapped(adminOff) {
		t.Error("no protected route and no admin route: the gate host should not be mapped")
	}
	app, err := st.CreateApp(ctx, core.App{Name: "wiki", RepoURL: "https://x/y.git", Domain: "wiki.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := st.ListDomains(ctx, app.ID)
	if err != nil || len(rows) != 1 {
		t.Fatalf("ListDomains = %v (%v)", rows, err)
	}
	if err := st.SetDomainProtected(ctx, app.ID, rows[0].ID, true); err != nil {
		t.Fatal(err)
	}
	if !mapped(adminOn) {
		t.Error("admin route on: the gate host should be mapped")
	}
	if !mapped(adminOff) {
		t.Error("admin route turned off while a route is protected: the gate host should stay mapped")
	}
	if err := st.SetCloudflareToken(ctx, "tok"); err != nil {
		t.Fatal(err)
	}
	if !mapped(adminOff) {
		t.Error("tunnel on, no admin route, a route protected: the gate host should be mapped")
	}
	if err := st.ClearCloudflareToken(ctx); err != nil {
		t.Fatal(err)
	}
	if err := st.SetDomainProtected(ctx, app.ID, rows[0].ID, false); err != nil {
		t.Fatal(err)
	}
	if mapped(adminOff) {
		t.Error("no protected route left and no admin route: the mapping should go")
	}
}

// listFails is a Docker whose container listing fails.
type listFails struct{ *docker.Fake }

func (listFails) ListContainers(context.Context, map[string]string) ([]docker.Container, error) {
	return nil, errors.New("docker unreachable")
}

// nothingReads is a Docker that can neither list containers nor look one up.
type nothingReads struct{ listFails }

func (nothingReads) FindContainer(context.Context, string) (*docker.Container, error) {
	return nil, errors.New("docker unreachable")
}

// A container deployed while a row was protected keeps calling the gate until
// it is replaced, even after the row is turned off or deleted (or when the
// redeploy that would replace it fails). Until then the gate's host stays
// mapped. When the containers can't be listed, Traefik is left as it is: its
// mapping is kept, and so is its lack of one; when that can't be read either,
// the host is mapped.
func TestReconcileKeepsTheGateHostWhileAContainerIsGated(t *testing.T) {
	ctx := context.Background()
	cfg := config.Config{
		DataDir: t.TempDir(), Network: "outhaul", TraefikImage: config.DefaultTraefikImage,
		CloudflaredImage: config.DefaultCloudflaredImage, HTTPSPort: "443", ListenAddr: ":8080",
		AdminDirect: true,
	} // no public URL: no admin route
	st, err := openStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	fake := docker.NewFake()
	ic := &infraController{dc: fake, st: st, cfg: cfg}
	mapped := func() bool {
		t.Helper()
		if err := ic.reconcile(ctx); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		c, _ := fake.FindContainer(ctx, "outhaul-traefik")
		if c == nil {
			t.Fatal("no Traefik container")
		}
		var spec docker.ContainerSpec
		for _, s := range fake.Created {
			if s.Name == "outhaul-traefik" {
				spec = s
			}
		}
		for _, h := range spec.ExtraHosts {
			if h == "host.docker.internal:host-gateway" {
				return true
			}
		}
		return false
	}
	if mapped() {
		t.Fatal("nothing protected and nothing gated: the gate host should not be mapped")
	}

	app := core.App{Name: "wiki"}
	gated := traefik.AppLabels(app, []core.Domain{{ID: 1, Host: "wiki.example.com", Protected: true}}, 8080, traefik.Routing{TLS: true, CertResolver: "le", GateAddress: "http://host.docker.internal:8080/gate/check"})
	id, err := fake.CreateContainer(ctx, docker.ContainerSpec{Name: "outhaul-app-wiki", Image: "img:1", Labels: gated})
	if err != nil {
		t.Fatal(err)
	}
	if !mapped() {
		t.Error("no row is protected, but a container still carries a gate: the gate host should stay mapped")
	}
	if err := fake.RemoveContainer(ctx, id, true); err != nil {
		t.Fatal(err)
	}
	if mapped() {
		t.Error("the gated container is gone: the mapping should go")
	}

	// The containers can't be listed: the mapping stays as the running
	// Traefik has it. Here it has none, so Traefik isn't recreated.
	created := len(fake.Created)
	ic.dc = listFails{fake}
	if mapped() || len(fake.Created) != created {
		t.Error("containers can't be listed, and Traefik doesn't map the gate host: it should be left as it is")
	}
	// It has one: it keeps it.
	ic.dc = fake
	if id, err = fake.CreateContainer(ctx, docker.ContainerSpec{Name: "outhaul-app-wiki", Image: "img:1", Labels: gated}); err != nil {
		t.Fatal(err)
	}
	if !mapped() {
		t.Fatal("a gated container: the gate host should be mapped")
	}
	created = len(fake.Created)
	ic.dc = listFails{fake}
	if !mapped() || len(fake.Created) != created {
		t.Error("containers can't be listed, and Traefik maps the gate host: it should be left as it is")
	}

	// From here Traefik has no mapping, so wherever the answer is read from
	// it, it is no: a yes can only be the answer for what can't be read.
	ic.dc = fake
	if err := fake.RemoveContainer(ctx, id, true); err != nil {
		t.Fatal(err)
	}
	if mapped() {
		t.Fatal("the gated container is gone: the mapping should go")
	}
	ic.dc = listFails{fake}
	if ic.gatedContainersRemain(ctx, false) {
		t.Fatal("containers can't be listed, and Traefik doesn't map the gate host: gatedContainersRemain should say no")
	}
	// Nor can Traefik be looked up: the host is mapped, as before.
	ic.dc = nothingReads{listFails{fake}}
	if !ic.gatedContainersRemain(ctx, false) {
		t.Error("nothing can be read: gatedContainersRemain should say yes")
	}
	// Traefik is found, but its config differs in something else: the same.
	ic.dc = listFails{fake}
	for _, c := range fake.Containers {
		if c.Name == "outhaul-traefik" {
			c.Labels["outhaul.config-hash"] = "drifted"
		}
	}
	if !ic.gatedContainersRemain(ctx, false) {
		t.Error("Traefik's config drifted otherwise: gatedContainersRemain should say yes")
	}
}

// ReconcileProxy's error says what Outhaul couldn't do, in words the domain
// dialog can show after "Outhaul couldn't": the tunnel connector, or Traefik.
func TestReconcileProxyNamesWhatFailed(t *testing.T) {
	cfg := config.Config{
		DataDir: t.TempDir(), Network: "outhaul", TraefikImage: config.DefaultTraefikImage,
		CloudflaredImage: config.DefaultCloudflaredImage, HTTPSPort: "443", ListenAddr: ":8080",
		PublicURL: "https://outhaul.example.com", ACMEEmail: "ops@example.com", AdminDirect: true,
	}
	st, err := openStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	for _, c := range []struct {
		name, failing, want string
		tunnel              bool
	}{
		{"the connector", tunnel.ContainerName, "start the tunnel connector: ", true},
		{"Traefik", "outhaul-traefik", "update the proxy (Traefik): ", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.tunnel {
				if err := st.SetCloudflareToken(ctx, "tok"); err != nil {
					t.Fatal(err)
				}
			} else if err := st.ClearCloudflareToken(ctx); err != nil {
				t.Fatal(err)
			}
			fake := docker.NewFake()
			fake.FailCreate = func(spec docker.ContainerSpec) error {
				if spec.Name == c.failing {
					return errors.New("no such image")
				}
				return nil
			}
			ic := &infraController{dc: fake, st: st, cfg: cfg}
			err := ic.ReconcileProxy(ctx)
			if err == nil || !strings.HasPrefix(err.Error(), c.want) || !strings.HasSuffix(err.Error(), "no such image") {
				t.Errorf("ReconcileProxy = %v, want %q followed by the cause", err, c.want)
			}
		})
	}
}

// The server asks for a reconcile before it turns sign-in on. The request may
// travel through the Traefik that the reconcile recreates, so a cancelled
// request must not stop it half-way.
func TestReconcileProxyOutlivesTheRequest(t *testing.T) {
	cfg := config.Config{
		DataDir: t.TempDir(), Network: "outhaul", TraefikImage: config.DefaultTraefikImage,
		CloudflaredImage: config.DefaultCloudflaredImage, HTTPSPort: "443", ListenAddr: ":8080",
		PublicURL: "https://outhaul.example.com", ACMEEmail: "ops@example.com", AdminDirect: true,
	}
	st, err := openStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	fake := docker.NewFake()
	ic := &infraController{dc: fake, st: st, cfg: cfg}
	var _ server.ProxyControl = ic

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := ic.ReconcileProxy(ctx); err != nil {
		t.Fatalf("ReconcileProxy with the request gone: %v", err)
	}
	if c, _ := fake.FindContainer(context.Background(), "outhaul-traefik"); c == nil || !c.Running() {
		t.Errorf("Traefik after ReconcileProxy = %+v, want it running", c)
	}
}

func TestEnableTunnelRefusedWithHTTPOff(t *testing.T) {
	cfg := config.Config{DataDir: t.TempDir(), Network: "outhaul", ListenAddr: ":8080", HTTPOff: true}
	st, err := openStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ic := &infraController{dc: docker.NewFake(), st: st, cfg: cfg}
	err = ic.Enable(context.Background(), "tok")
	if err == nil || !strings.Contains(err.Error(), "OUTHAUL_HTTP_PORT") {
		t.Fatalf("Enable with HTTP off = %v, want an error naming OUTHAUL_HTTP_PORT", err)
	}
	if on, err := st.TunnelEnabled(context.Background()); err != nil || on {
		t.Errorf("TunnelEnabled = %v, %v; the token must not be stored", on, err)
	}
}

// OUTHAUL_SSH_ADDR=off turns the listener off even when the UI saved an
// address earlier: off means off, not "use the stored setting".
func TestSSHListenAddrOff(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.SetSetting(context.Background(), "ssh_addr", ":2200"); err != nil {
		t.Fatal(err)
	}

	if addr, on := sshListenAddr(st, config.Config{SSHAddr: "off"}); on || addr != "" {
		t.Errorf("off: got (%q, %v), want (\"\", false)", addr, on)
	}
	if addr, on := sshListenAddr(st, config.Config{SSHAddr: ":2222"}); !on || addr != ":2200" {
		t.Errorf("stored setting: got (%q, %v), want (\":2200\", true)", addr, on)
	}
}

func TestHTTPOffRefusesAnEnabledTunnel(t *testing.T) {
	cfg := config.Config{HTTPOff: true}
	err := httpOffTunnelError(cfg, true)
	if err == nil || !strings.Contains(err.Error(), "OUTHAUL_HTTP_PORT") {
		t.Fatalf("httpOffTunnelError(off, tunnel) = %v, want an error naming OUTHAUL_HTTP_PORT", err)
	}
	if err := httpOffTunnelError(cfg, false); err != nil {
		t.Errorf("httpOffTunnelError(off, no tunnel) = %v, want nil", err)
	}
}

// countingNetwork fails EnsureNetwork and counts the attempts, so a retry is
// visible without waiting for it to succeed.
type countingNetwork struct {
	*docker.Fake
	calls atomic.Int64
}

func (c *countingNetwork) EnsureNetwork(context.Context, string) error {
	c.calls.Add(1)
	return errors.New("docker unreachable")
}

func TestReconcileIsRetriedWhileAHeaderIsRequired(t *testing.T) {
	cfg := config.Config{
		DataDir: t.TempDir(), Network: "outhaul", TraefikImage: config.DefaultTraefikImage,
		ListenAddr: ":8080", RequireHeaderName: "X-Azure-FDID", RequireHeaderValue: "abc-123",
	}
	st, err := openStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	f := docker.NewFake()
	pulls := 0
	f.FailPull = func(string) error {
		pulls++
		if pulls <= 2 {
			return errors.New("registry down")
		}
		return nil
	}
	ic := &infraController{dc: f, st: st, cfg: cfg}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ic.retryReconcile(ctx, time.Millisecond)
	c, _ := f.FindContainer(ctx, "outhaul-traefik")
	if c == nil || !c.Running() {
		t.Fatalf("retryReconcile left no running Traefik: %+v", c)
	}
	found := false
	for _, spec := range f.Created {
		for _, flag := range spec.Cmd {
			if strings.HasPrefix(flag, "--providers.docker.constraints=") {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("the retried Traefik has no constraint: %v", f.Created)
	}
	if pulls != 3 {
		t.Errorf("pulls = %d, want 3 (two failures, then success)", pulls)
	}
}

func TestStartupDoesNotRetryWithoutAHeader(t *testing.T) {
	cfg := config.Config{DataDir: t.TempDir(), Network: "outhaul", TraefikImage: config.DefaultTraefikImage, ListenAddr: ":8080", AdminDirect: true}
	st, err := openStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	f := &countingNetwork{Fake: docker.NewFake()}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ensureInfra(ctx, f, cfg, st)
	time.Sleep(50 * time.Millisecond)
	if f.calls.Load() != 1 {
		t.Errorf("EnsureNetwork calls = %d, want 1 (no retry without a header)", f.calls.Load())
	}
}

func TestAdminAddrs(t *testing.T) {
	on := config.Config{AdminDirect: true, ListenAddr: ":8080"}
	if got := adminAddrs(on, "172.30.0.1"); len(got) != 1 || got[0] != ":8080" {
		t.Errorf("admin direct on: %v, want [:8080]", got)
	}
	off := config.Config{AdminDirect: false, ListenAddr: ":8080"}
	if got := adminAddrs(off, "172.30.0.1"); strings.Join(got, ",") != "127.0.0.1:8080,172.30.0.1:8080" {
		t.Errorf("admin direct off: %v, want the loopback and gateway addresses", got)
	}
	noGateway := config.Config{AdminDirect: false, ListenAddr: ":9090"}
	if got := adminAddrs(noGateway, ""); len(got) != 1 || got[0] != "127.0.0.1:9090" {
		t.Errorf("no gateway yet: %v, want [127.0.0.1:9090]", got)
	}
}

func TestAdminListensOnLoopbackUntilDockerAnswers(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	cfg := config.Config{AdminDirect: false, ListenAddr: fmt.Sprintf(":%d", port)}
	httpServer := newHTTPServer(cfg.ListenAddr, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}), func() {})
	defer httpServer.Close()

	calls := 0
	gateway := func(context.Context) (string, error) {
		calls++
		if calls <= 2 {
			return "", errors.New("docker unreachable")
		}
		return "127.0.0.2", nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errs := make(chan error, 4)
	serveAdmin(ctx, httpServer, cfg, gateway, 10*time.Millisecond, errs)

	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/", port))
	if err != nil {
		t.Fatalf("loopback is not reachable at once: %v", err)
	}
	resp.Body.Close()

	deadline := time.Now().Add(2 * time.Second)
	for {
		resp, err := http.Get(fmt.Sprintf("http://127.0.0.2:%d/", port))
		if err == nil {
			resp.Body.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the gateway address never became reachable: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case err := <-errs:
		t.Fatalf("serveAdmin reported an error: %v", err)
	default:
	}
}

func TestProxyConfigUsesTheGateway(t *testing.T) {
	cfg := config.Config{
		DataDir: t.TempDir(), Network: "outhaul", TraefikImage: config.DefaultTraefikImage,
		ListenAddr: ":8080", AdminDirect: false, PublicURL: "https://outhaul.example.com",
		ACMEEmail: "ops@example.com",
	}
	st, err := openStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	f := docker.NewFake()
	if f.Gateways == nil {
		f.Gateways = map[string]string{}
	}
	f.Gateways["outhaul"] = "172.30.0.1"
	ic := &infraController{dc: f, st: st, cfg: cfg}
	if err := ic.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := ic.proxyConfig(context.Background(), false, false).GateHost; got != "172.30.0.1" {
		t.Errorf("GateHost = %q, want the fake's gateway", got)
	}
}

func TestAdminDirectOffNeedsSomethingPublishingIt(t *testing.T) {
	cfg := config.Config{AdminDirect: false}
	err := adminPublishError(cfg, false)
	if err == nil || !strings.Contains(err.Error(), "OUTHAUL_ADMIN_DIRECT") {
		t.Fatalf("adminPublishError = %v, want an error naming OUTHAUL_ADMIN_DIRECT", err)
	}
	https := cfg
	https.ACMEEmail = "ops@example.com"
	if err := adminPublishError(https, false); err != nil {
		t.Errorf("with HTTPS: %v", err)
	}
	if err := adminPublishError(cfg, true); err != nil {
		t.Errorf("with the tunnel: %v", err)
	}
}

func TestDisableTunnelRefusedWhenOnlyItPublishesTheAdminUI(t *testing.T) {
	cfg := config.Config{
		DataDir: t.TempDir(), Network: "outhaul", TraefikImage: config.DefaultTraefikImage,
		ListenAddr: ":8080", AdminDirect: false,
	}
	st, err := openStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.SetCloudflareToken(context.Background(), "tok"); err != nil {
		t.Fatal(err)
	}
	ic := &infraController{dc: docker.NewFake(), st: st, cfg: cfg}
	err = ic.Disable(context.Background())
	if err == nil || !strings.Contains(err.Error(), "OUTHAUL_ADMIN_DIRECT") {
		t.Fatalf("Disable = %v, want an error naming OUTHAUL_ADMIN_DIRECT", err)
	}
	if on, err := st.TunnelEnabled(context.Background()); err != nil || !on {
		t.Errorf("TunnelEnabled = %v, %v; the tunnel must stay enabled", on, err)
	}
}

func TestWarnsWhenTheHeaderDoesNotCoverTheAdminPort(t *testing.T) {
	cfg := config.Config{AdminDirect: true, RequireHeaderName: "X-Probe-Id", RequireHeaderValue: "abc-123"}
	warning := adminPortWarning(cfg)
	if warning == "" {
		t.Fatal("a required header with the admin port open should warn")
	}
	for _, secret := range []string{"X-Probe-Id", "abc-123"} {
		if strings.Contains(warning, secret) {
			t.Errorf("the warning contains %q: %s", secret, warning)
		}
	}
	cfg.AdminDirect = false
	if adminPortWarning(cfg) != "" {
		t.Error("admin direct off should not warn")
	}
	cfg = config.Config{AdminDirect: true}
	if adminPortWarning(cfg) != "" {
		t.Error("no header should not warn")
	}
}

func TestAdminGatewayBindFailureIsNotFatal(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	blocker, err := net.Listen("tcp", fmt.Sprintf("127.0.0.2:%d", port))
	if err != nil {
		t.Skipf("cannot bind 127.0.0.2: %v", err)
	}
	defer blocker.Close()

	cfg := config.Config{AdminDirect: false, ListenAddr: fmt.Sprintf(":%d", port)}
	httpServer := newHTTPServer(cfg.ListenAddr, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}), func() {})
	defer httpServer.Close()
	gateway := func(context.Context) (string, error) { return "127.0.0.2", nil }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errs := make(chan error, 4)
	serveAdmin(ctx, httpServer, cfg, gateway, 10*time.Millisecond, errs)

	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/", port))
	if err != nil {
		t.Fatalf("loopback is not reachable: %v", err)
	}
	resp.Body.Close()
	select {
	case err := <-errs:
		t.Fatalf("a gateway bind failure ended the server: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	blocker.Close()
	deadline := time.Now().Add(2 * time.Second)
	for {
		resp, err := http.Get(fmt.Sprintf("http://127.0.0.2:%d/", port))
		if err == nil {
			resp.Body.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the gateway listener never retried: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestStartupRetriesWhenAdminDirectIsOff(t *testing.T) {
	old := reconcileRetryInterval
	reconcileRetryInterval = time.Millisecond
	t.Cleanup(func() { reconcileRetryInterval = old })

	cfg := config.Config{
		DataDir: t.TempDir(), Network: "outhaul", TraefikImage: config.DefaultTraefikImage,
		ListenAddr: ":8080", AdminDirect: false, PublicURL: "https://outhaul.example.com",
		ACMEEmail: "ops@example.com",
	}
	st, err := openStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	f := &countingNetwork{Fake: docker.NewFake()}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ensureInfra(ctx, f, cfg, st)
	deadline := time.Now().Add(2 * time.Second)
	for f.calls.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if f.calls.Load() < 2 {
		t.Fatalf("EnsureNetwork calls = %d, want a retry while OUTHAUL_ADMIN_DIRECT=off", f.calls.Load())
	}
}

func TestNewUpdatesOff(t *testing.T) {
	d, k, off := newUpdates(config.Config{Updates: "off"})
	if !off || d != nil || k != nil {
		t.Fatalf("off: got (%v, %v, %v), want (nil, nil, true)", d, k, off)
	}
	d, k, off = newUpdates(config.Config{Updates: "main", DataDir: t.TempDir()})
	if off || d == nil || k == nil {
		t.Fatalf("main: got (%v, %v, %v), want a Dir and a Checker", d, k, off)
	}
}

// The strict container policy forbids a published database port, but a row
// that still has one keeps publishing it until its container is replaced.
// Startup names each such row and changes nothing.
func TestStrictPolicyWarnsAboutPublishedDatabasePorts(t *testing.T) {
	ctx := context.Background()
	st, err := openStore(config.Config{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, d := range []core.Database{
		{Name: "shop", Engine: core.EnginePostgres, Image: "postgres:17", Password: "pw", ExtPort: 5433},
		{Name: "stock", Engine: core.EnginePostgres, Image: "postgres:17", Password: "pw"},
	} {
		if _, err := st.CreateDatabase(ctx, d); err != nil {
			t.Fatal(err)
		}
	}

	var warnings []string
	logf := func(format string, args ...any) { warnings = append(warnings, fmt.Sprintf(format, args...)) }
	warnPublishedDatabasePorts(ctx, st, true, logf)

	if len(warnings) != 1 {
		t.Fatalf("warnings = %q, want one, for the database that publishes a port", warnings)
	}
	if !strings.Contains(warnings[0], `"shop"`) || !strings.Contains(warnings[0], "5433") {
		t.Errorf("warning = %q, want it to name the database and its port", warnings[0])
	}
	dbs, err := st.ListDatabases(ctx)
	if err != nil || len(dbs) != 2 {
		t.Fatalf("databases after the warning = %+v (err %v), want both rows", dbs, err)
	}
	for _, d := range dbs {
		if d.Status != core.DBCreating {
			t.Errorf("database %q status = %q, want it left as it was (nothing is provisioned at startup)", d.Name, d.Status)
		}
	}

	warnings = nil
	warnPublishedDatabasePorts(ctx, st, false, logf)
	if len(warnings) != 0 {
		t.Errorf("warnings with the strict policy off = %q", warnings)
	}
}

// Every Compose runner shares one environment. Strict mode, or a required
// request header on its own, reduces it to the keep-list: no OUTHAUL_* setting
// and not the header's value reaches a Compose file's ${NAME}. With both off
// it is nil, so runs inherit exactly as before.
func TestComposeEnvironmentFollowsStrictAndHeader(t *testing.T) {
	t.Setenv("OUTHAUL_CONTAINER_POLICY", "strict")
	t.Setenv("OUTHAUL_REQUIRE_HEADER_NAME", "X-Azure-FDID")
	t.Setenv("OUTHAUL_REQUIRE_HEADER_VALUE", "header-secret")
	t.Setenv("PATH", "/usr/local/bin:/usr/bin")

	cases := []struct {
		name    string
		cfg     config.Config
		reduced bool
	}{
		{"strict container policy", config.Config{ContainerPolicy: "strict"}, true},
		{"header required without strict mode", config.Config{
			RequireHeaderName: "X-Azure-FDID", RequireHeaderValue: "header-secret",
		}, true},
		{"neither", config.Config{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := composeEnvFor(tc.cfg)
			if !tc.reduced {
				if env != nil {
					t.Fatalf("composeEnvFor = %q, want nil (inherit)", env)
				}
				if r := newComposeRunner(env); r.Env != nil {
					t.Fatalf("runner Env = %q, want nil (inherit)", r.Env)
				}
				return
			}
			if env == nil {
				t.Fatal("composeEnvFor = nil, want the restricted environment")
			}
			if want := "PATH=" + os.Getenv("PATH"); !slices.Contains(env, want) {
				t.Errorf("composeEnvFor = %q, want it to contain %q", env, want)
			}
			for _, kv := range env {
				if strings.HasPrefix(kv, "OUTHAUL_") {
					t.Errorf("composeEnvFor = %q, want no OUTHAUL_* setting in it", env)
					break
				}
			}
		})
	}
}

func TestTrustedAllWarning(t *testing.T) {
	w := trustedAllWarning(config.Config{TrustAllProxies: true})
	for _, want := range []string{"OUTHAUL_TRUSTED_PROXIES", "OUTHAUL_REQUIRE_HEADER_NAME", "OUTHAUL_REQUIRE_HEADER_VALUE"} {
		if !strings.Contains(w, want) {
			t.Errorf("warning %q does not name %s", w, want)
		}
	}
	if got := trustedAllWarning(config.Config{
		TrustAllProxies:    true,
		RequireHeaderName:  "X-Probe-Id",
		RequireHeaderValue: "abc-123",
	}); got != "" {
		t.Errorf("warning = %q with a required header, want empty", got)
	}
	if got := trustedAllWarning(config.Config{}); got != "" {
		t.Errorf("warning = %q without trust-all, want empty", got)
	}
}

func TestProxyConfigCarriesTrustedProxies(t *testing.T) {
	cfg := config.Config{
		Network: "outhaul", TraefikImage: config.DefaultTraefikImage,
		TrustedProxies: []string{"192.0.2.0/24"},
	}
	ic := &infraController{dc: docker.NewFake(), cfg: cfg}
	for _, tunnelOn := range []bool{false, true} {
		pc := ic.proxyConfig(context.Background(), tunnelOn, false)
		if !slices.Equal(pc.TrustedProxies, cfg.TrustedProxies) || pc.TrustAllProxies {
			t.Errorf("tunnelOn=%v: TrustedProxies=%v TrustAllProxies=%v", tunnelOn, pc.TrustedProxies, pc.TrustAllProxies)
		}
	}
	ic.cfg = config.Config{Network: "outhaul", TrustAllProxies: true}
	for _, tunnelOn := range []bool{false, true} {
		pc := ic.proxyConfig(context.Background(), tunnelOn, false)
		if !pc.TrustAllProxies || len(pc.TrustedProxies) != 0 {
			t.Errorf("tunnelOn=%v: TrustedProxies=%v TrustAllProxies=%v", tunnelOn, pc.TrustedProxies, pc.TrustAllProxies)
		}
	}
}
