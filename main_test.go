package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
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
		PublicURL: "https://outhaul.example.com", ACMEEmail: "ops@example.com",
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
	gated := traefik.AppLabels(app, []core.Domain{{ID: 1, Host: "wiki.example.com", Protected: true}}, 8080, true, "le", "http://host.docker.internal:8080/gate/check")
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
		PublicURL: "https://outhaul.example.com", ACMEEmail: "ops@example.com",
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
		PublicURL: "https://outhaul.example.com", ACMEEmail: "ops@example.com",
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
