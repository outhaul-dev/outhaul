// Command outhaul is a single-binary, self-hosted PaaS: git-push-to-deploy on
// one VPS, orchestrating Docker and Traefik. `outhaul serve` starts the admin
// UI and the background deploy worker.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/outhaul-dev/outhaul/internal/backup"
	"github.com/outhaul-dev/outhaul/internal/builder"
	"github.com/outhaul-dev/outhaul/internal/catalog"
	"github.com/outhaul-dev/outhaul/internal/compose"
	"github.com/outhaul-dev/outhaul/internal/config"
	"github.com/outhaul-dev/outhaul/internal/dbaas"
	"github.com/outhaul-dev/outhaul/internal/deploy"
	"github.com/outhaul-dev/outhaul/internal/docker"
	"github.com/outhaul-dev/outhaul/internal/githook"
	"github.com/outhaul-dev/outhaul/internal/github"
	"github.com/outhaul-dev/outhaul/internal/gitrepo"
	"github.com/outhaul-dev/outhaul/internal/gitsource"
	"github.com/outhaul-dev/outhaul/internal/gitssh"
	"github.com/outhaul-dev/outhaul/internal/localca"
	"github.com/outhaul-dev/outhaul/internal/logstream"
	"github.com/outhaul-dev/outhaul/internal/previewmgr"
	"github.com/outhaul-dev/outhaul/internal/prune"
	"github.com/outhaul-dev/outhaul/internal/secret"
	"github.com/outhaul-dev/outhaul/internal/server"
	"github.com/outhaul-dev/outhaul/internal/store"
	"github.com/outhaul-dev/outhaul/internal/traefik"
	"github.com/outhaul-dev/outhaul/internal/tunnel"
	"github.com/outhaul-dev/outhaul/internal/update"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("outhaul: ")

	if len(os.Args) >= 2 && os.Args[1] == "git-hook" {
		os.Exit(runGitHook(os.Args[2:]))
	}
	if len(os.Args) >= 2 && os.Args[1] == "ca" {
		os.Exit(runCA(os.Args[2:]))
	}
	if len(os.Args) >= 2 && os.Args[1] == "version" {
		os.Exit(runVersion(os.Stdout, update.Current()))
	}
	if len(os.Args) >= 2 && os.Args[1] == "images" {
		os.Exit(runImages(os.Stdout, config.Load(os.Getenv)))
	}
	if len(os.Args) < 2 || os.Args[1] != "serve" {
		usage()
		os.Exit(2)
	}
	if err := serve(); err != nil {
		log.Fatalf("fatal: %v", err)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "Usage: outhaul serve")
	fmt.Fprintln(os.Stderr, "       outhaul ca root [--path]")
	fmt.Fprintln(os.Stderr, "       outhaul version")
	fmt.Fprintln(os.Stderr, "       outhaul images")
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "Starts the Outhaul admin UI and deploy worker.")
	fmt.Fprintln(os.Stderr, "Configuration via OUTHAUL_* environment variables (see ARCHITECTURE.md).")
}

// runGitHook is the post-receive relay: `outhaul git-hook <app> <socket>`. It
// reads ref updates from stdin and streams the deploy back to stdout, exiting
// with the deploy's status.
func runGitHook(args []string) int {
	if len(args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: outhaul git-hook <app> <socket>")
		return 2
	}
	code, err := githook.RunHook(args[1], args[0], os.Stdin, os.Stdout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "outhaul: %v\n", err)
	}
	return code
}

// runVersion implements `outhaul version`: the build stamp as key=value lines,
// which the updater parses.
func runVersion(w io.Writer, b update.Build) int {
	fmt.Fprintf(w, "commit=%s\nrepo=%s\nlocal=%t\nversion=%s\n", b.Commit, b.Repo, b.Local, b.Version)
	return 0
}

// runImages implements `outhaul images`: the container images Outhaul manages,
// one `<name> <image>` per line, so the updater can pre-pull them.
func runImages(w io.Writer, cfg config.Config) int {
	fmt.Fprintf(w, "traefik %s\ncloudflared %s\n", cfg.TraefikImage, cfg.CloudflaredImage)
	return 0
}

// runCA implements `outhaul ca root [--path]`: prints the local CA root
// certificate (or its path) so it can be installed on LAN devices.
func runCA(args []string) int {
	if len(args) < 1 || args[0] != "root" {
		fmt.Fprintln(os.Stderr, "usage: outhaul ca root [--path]")
		return 2
	}
	cfg := config.Load(os.Getenv)
	path := localca.RootPath(cfg.CADir())
	if len(args) >= 2 && args[1] == "--path" {
		fmt.Println(path)
		return 0
	}
	pemBytes, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "outhaul: local CA not initialised (%v)\n", err)
		fmt.Fprintln(os.Stderr, "Set OUTHAUL_LOCAL_CA=true and start `outhaul serve` once to create it.")
		return 1
	}
	os.Stdout.Write(pemBytes)
	return 0
}

func serve() error {
	cfg := config.Load(os.Getenv)
	if err := cfg.Validate(); err != nil {
		return err
	}

	// A strict container policy hides Outhaul's own environment from Compose
	// files: every `docker compose` call gets only a small keep-list, so a
	// file's ${NAME} cannot read OUTHAUL_* settings or secrets. Off, the slice
	// is nil and every run inherits the environment exactly as before. The
	// stack's own .env still supplies the app's variables.
	var composeEnv []string
	if cfg.StrictContainerPolicy() {
		composeEnv = compose.RestrictedEnv(os.Environ())
	}

	// Data directories.
	if err := os.MkdirAll(cfg.WorkDir(), 0o755); err != nil {
		return fmt.Errorf("create data dir %s: %w", cfg.DataDir, err)
	}
	if err := os.MkdirAll(cfg.AcmeDir(), 0o700); err != nil {
		return fmt.Errorf("create acme dir: %w", err)
	}
	// Store (with its secret key) + crash recovery.
	st, err := openStore(cfg)
	if err != nil {
		return err
	}
	defer st.Close()
	reserveAdminHost(context.Background(), st, cfg.AdminHost(), log.Printf)

	if n, err := st.RecoverActive(context.Background(), "interrupted by restart"); err != nil {
		return fmt.Errorf("crash recovery: %w", err)
	} else if n > 0 {
		log.Printf("recovered %d interrupted deployment(s) as failed", n)
	}
	if n, err := st.RecoverCreatingDatabases(context.Background(), "interrupted by restart"); err != nil {
		return fmt.Errorf("crash recovery: %w", err)
	} else if n > 0 {
		log.Printf("recovered %d interrupted database provision(s) as failed", n)
	}

	// Docker client. Not fatal if unreachable: the admin UI (and first-boot
	// setup) should still come up so the operator can see what's wrong.
	dc, err := docker.New(cfg.DockerHost)
	if err != nil {
		return fmt.Errorf("docker client: %w", err)
	}
	defer dc.Close()

	// Local CA: refuse to run against an enabled tunnel (mutually exclusive
	// ingress postures), then mint certs before Traefik comes up so the file
	// provider finds them on first watch.
	var certMgr *localca.Manager
	if on, err := st.TunnelEnabled(context.Background()); err == nil {
		if err := httpOffTunnelError(cfg, on); err != nil {
			return err
		}
		if err := adminPublishError(cfg, on); err != nil {
			return err
		}
	}
	if warning := adminPortWarning(cfg); warning != "" {
		log.Print(warning)
	}
	if cfg.LocalCAEnabled() {
		if on, err := st.TunnelEnabled(context.Background()); err == nil && on {
			return fmt.Errorf("OUTHAUL_LOCAL_CA is set but a Cloudflare Tunnel is enabled; disable the tunnel in Settings first (or unset OUTHAUL_LOCAL_CA)")
		}
		ca, err := localca.LoadOrCreate(cfg.CADir())
		if err != nil {
			return fmt.Errorf("local CA: %w", err)
		}
		defaultHost := cfg.AdminHost()
		if defaultHost == "" {
			if hn, err := os.Hostname(); err == nil && hn != "" {
				defaultHost = hn
			} else {
				defaultHost = "outhaul.local"
			}
		}
		certMgr = localca.NewManager(ca, cfg.CertsDir(), cfg.DynamicDir(), defaultHost, st)
		if err := certMgr.Sync(context.Background()); err != nil {
			log.Printf("WARNING: local CA cert sync: %v", err)
		}
	}

	workerCtx, stopWorker := context.WithCancel(context.Background())
	infra := ensureInfra(workerCtx, dc, cfg, st)

	ghClient := github.New()
	// One registry, shared by the deploy worker, the preview manager, and the
	// HTTP layer: every consumer resolves a source's provider the same way.
	sources := gitsource.NewRegistry(gitsource.NewGithubApp(ghClient))

	// Background worker.
	broker := logstream.New()
	worker := deploy.NewWorker(st, dc,
		deploy.Builders{Nixpacks: builder.NewNixpacks(), Dockerfile: builder.NewDocker()},
		newComposeRunner(composeEnv), deploy.NewGit(), broker, sources, cfg)

	// Image pruner: after-deploy retention hook + daily sweep.
	pruner := prune.New(st, dc, cfg.ImageKeep, cfg.WorkDir(), cfg.PruneBuildCache)
	worker.SetPruner(pruner)

	// An update stops Outhaul, so while one is requested or running the worker
	// and the backup scheduler start nothing new. With updates off neither is
	// held, and no check ever leaves the server.
	updates, checker, updatesOff := newUpdates(cfg)
	if !updatesOff {
		worker.SetHold(updates.InProgress)
	}

	workerDone := make(chan struct{})
	// worker.Run is launched below, after the preview manager is wired in as the
	// after-deploy hook (SetDeployHook must be called before Run).

	if certMgr != nil {
		go certMgr.Run(workerCtx)
	}

	// Database manager (databases-as-a-service).
	dbm := dbaas.NewManager(st, dc, cfg.Network, cfg.DatabasesDir())
	dbm.SetPolicy(cfg.StrictContainerPolicy())
	warnPublishedDatabasePorts(context.Background(), st, cfg.StrictContainerPolicy(), log.Printf)

	// Backup scheduler (dumps + volume tarballs to S3-compatible storage).
	// Runs a dead process left "running" are failed before any can start.
	backups := backup.NewManager(st, dc, cfg.WorkDir())
	if n, err := backups.RecoverInterrupted(context.Background()); err != nil {
		stopWorker()
		return fmt.Errorf("crash recovery: %w", err)
	} else if n > 0 {
		log.Printf("recovered %d interrupted backup/restore run(s) as failed", n)
	}
	if !updatesOff {
		backups.SetHold(updates.InProgress)
		go checker.Run(workerCtx)
	}
	go backups.Run(workerCtx)

	// Daily disk-cleanup sweep (image retention, dangling images, build cache).
	go pruner.Run(workerCtx)

	// HTTP server. Generated template domains embed the server's public IP
	// (sslip.io), configurable for hosts whose outbound and public IPs differ.
	serverIP := cfg.ServerIP
	if serverIP == "" {
		serverIP = catalog.DetectServerIP()
	}
	// git-push-to-deploy: bare-repo manager, deploy bridge, and SSH server. All
	// startup failures are non-fatal — the admin UI still comes up, and git-push
	// is simply unavailable until the cause is fixed.
	self, exErr := os.Executable()
	if exErr != nil {
		log.Printf("WARNING: cannot resolve binary path; git-push disabled: %v", exErr)
		self = ""
	}
	repos := gitrepo.New(cfg.GitDir(), self, cfg.GitHookSocketPath())
	sshSrv := startGitPush(workerCtx, cfg, st, worker, broker, serverIP, repos, self)

	// Preview environments: per-PR ephemeral child apps. They post PR comments
	// as the app's own connected account.
	tokenSource := func(ctx context.Context, sourceID int64) (string, bool, error) {
		src, ok, err := st.GetGitSource(ctx, sourceID)
		if err != nil || !ok {
			return "", false, err
		}
		tok, err := sources.TokenFor(ctx, src)
		return tok, err == nil, err
	}
	previews := previewmgr.New(st, worker,
		&previewDBProvisioner{st: st, dbm: dbm},
		&previewDocker{st: st, runtime: dc, compose: newComposeRunner(composeEnv)},
		ghClient, tokenSource, serverIP)
	worker.SetDeployHook(previews.OnDeployFinished) // update a preview's PR comment + status on deploy completion
	// A preview of a route that requires sign-in requires it too: the spawn
	// reconciles the proxy, as turning sign-in on does, and links it over
	// HTTPS when protected rows are served that way (as the pages do).
	previews.SetProxyControl(infra)
	previews.SetHTTPSOnly(cfg.HTTPOff)
	previews.SetProtectedHTTPS(func(ctx context.Context) bool {
		if cfg.TLSEnabled() {
			return true
		}
		tunnel, err := st.TunnelEnabled(ctx)
		if err != nil {
			log.Printf("previews: read the tunnel setting: %v", err)
			return false
		}
		return tunnel
	})
	go func() {
		worker.Run(workerCtx)
		close(workerDone)
	}()
	go previews.Run(workerCtx)

	setupToken := server.NewToken()
	srv, err := server.New(st, worker, dc, newComposeRunner(composeEnv), dbm, backups, broker, ghClient,
		sources, previews,
		cfg.PublicURL, serverIP, cfg.TLSEnabled(), setupToken)
	if err != nil {
		stopWorker()
		return fmt.Errorf("build server: %w", err)
	}
	srv.SetRepos(repos)
	srv.SetRouting(traefik.RoutingFrom(cfg, false))
	go func() {
		for {
			if _, err := srv.RedeployStale(workerCtx); err == nil {
				return
			} else {
				log.Printf("ingress: redeploying apps for the routing settings failed; retrying in a minute: %v", err)
			}
			select {
			case <-workerCtx.Done():
				return
			case <-time.After(time.Minute):
			}
		}
	}()
	if sshSrv != nil {
		srv.SetSSHControl(sshSrv)
	}
	srv.SetTunnelControl(infra)
	srv.SetProxyControl(infra)
	srv.SetHTTPSPort(cfg.HTTPSPort)
	srv.SetContainerPolicy(cfg.StrictContainerPolicy())
	srv.SetBuild(update.Current())
	if updatesOff {
		srv.SetUpdatesOff()
	} else {
		srv.SetUpdates(updates, checker)
	}
	if certMgr != nil {
		srv.SetCertSync(certMgr)
		srv.SetLocalCAFile(localca.RootPath(cfg.CADir()))
	}
	printSetupHint(srv, cfg, setupToken)

	httpServer := newHTTPServer(cfg.ListenAddr, srv.Handler(), srv.CloseStreams)

	serverErr := make(chan error, 1)
	log.Printf("listening on %s", strings.Join(adminAddrs(cfg, infra.Gateway()), ", "))
	serveAdmin(workerCtx, httpServer, cfg, func(ctx context.Context) (string, error) {
		return dc.NetworkGateway(ctx, cfg.Network)
	}, 5*time.Second, serverErr)

	// Wait for a signal or a fatal server error.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-serverErr:
		stopWorker()
		return fmt.Errorf("http server: %w", err)
	case s := <-sig:
		log.Printf("received %s, shutting down", s)
	}

	return shutdown(httpServer, stopWorker, workerDone, backups.Stop)
}

// newUpdates builds the Updates panel's directory, shared with the root
// updater, and the check of the running build against main. With updates off
// it builds neither and creates nothing: no checker means no request ever
// leaves the server, and no update can be requested.
func newUpdates(cfg config.Config) (d *update.Dir, k *update.Checker, off bool) {
	if cfg.UpdatesOff() {
		return nil, nil, true
	}
	d = update.NewDir(cfg.UpdateDir(), update.LogPath, time.Now)
	k = update.NewChecker(update.Current(),
		&update.Comparer{Client: &http.Client{Timeout: 15 * time.Second}}, time.Now)
	return d, k, false
}

// newComposeRunner returns the runner shared by the deploy worker, the preview
// manager and the HTTP server. env is nil unless the strict container policy
// is on, so a default install inherits the environment exactly as before.
func newComposeRunner(env []string) *compose.Docker {
	d := compose.NewDocker()
	d.Env = env
	return d
}

// infraController reconciles the managed Traefik proxy and cloudflared connector
// against the stored Cloudflare Tunnel state. It runs at boot and on live
// settings changes, and satisfies server.TunnelControl.
type infraController struct {
	dc  docker.Client
	st  *store.Store
	cfg config.Config

	// mu serializes reconcile so concurrent Enable/Disable calls (from HTTP
	// handlers) can't race and leave Traefik/cloudflared in a state that
	// doesn't match the last-persisted tunnel token.
	mu sync.Mutex

	// gateway is the host's address on Outhaul's Docker network, read while
	// OUTHAUL_ADMIN_DIRECT=off. Traefik maps host.docker.internal to it, and
	// the admin UI listens there.
	gateway string
}

// Gateway returns the Docker network gateway last read by reconcile, or "".
func (ic *infraController) Gateway() string {
	ic.mu.Lock()
	defer ic.mu.Unlock()
	return ic.gateway
}

// ensureInfra brings up the shared network, Traefik, and (if the tunnel is
// enabled) the cloudflared connector. Failures are logged, not fatal, so the
// admin UI still starts (e.g. when Docker is down). It returns a controller the
// server uses to toggle the tunnel at runtime.
// reconcileRetryInterval is how often a failed startup reconcile is retried.
// It is a variable so tests can shorten it.
var reconcileRetryInterval = 30 * time.Second

func ensureInfra(ctx context.Context, dc docker.Client, cfg config.Config, st *store.Store) *infraController {
	ic := &infraController{dc: dc, st: st, cfg: cfg}
	pingCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := dc.Ping(pingCtx); err != nil {
		log.Printf("WARNING: Docker not reachable (%v); deploys will fail until it is available", err)
		if cfg.HeaderRequired() || !cfg.AdminDirect {
			go ic.retryReconcile(ctx, reconcileRetryInterval)
		}
		return ic
	}
	if err := ic.reconcile(pingCtx); err != nil {
		log.Printf("WARNING: could not ensure infrastructure: %v", err)
		if cfg.HeaderRequired() || !cfg.AdminDirect {
			go ic.retryReconcile(ctx, reconcileRetryInterval)
		}
		return ic
	}
	ic.logStatus(pingCtx)
	return ic
}

// retryReconcile calls ic.reconcile every interval until it succeeds once or
// ctx ends, logging each failure. With a request header required, a Traefik
// that couldn't be replaced must not be left serving old containers, so the
// attempt is not a one-shot at startup.
func (ic *infraController) retryReconcile(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := ic.reconcile(ctx); err == nil {
			return
		} else {
			log.Printf("reconcile: %v; retrying in %v", err, interval)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// reconcile makes Traefik's posture and the connector match the stored tunnel
// state: tunnel on -> plain-HTTP Traefik + running connector; tunnel off ->
// ACME/ports Traefik + no connector. An error starts with the step that
// failed, worded to follow "Outhaul couldn't" (ReconcileProxy).
func (ic *infraController) reconcile(ctx context.Context) error {
	ic.mu.Lock()
	defer ic.mu.Unlock()

	token, tunnelOn, err := ic.st.CloudflareToken(ctx)
	if err != nil {
		return fmt.Errorf("read the tunnel setting: %w", err)
	}
	// Read before anything changes: if it fails, Traefik is left as it is.
	protected, err := ic.st.AnyDomainProtected(ctx)
	if err != nil {
		return fmt.Errorf("read which routes require sign-in: %w", err)
	}
	if !protected {
		protected = ic.gatedContainersRemain(ctx, tunnelOn)
	}
	if !ic.cfg.AdminDirect {
		// The admin UI will listen only on loopback and this address, and
		// Traefik maps host.docker.internal to it instead of host-gateway.
		if err := ic.dc.EnsureNetwork(ctx, ic.cfg.Network); err != nil {
			return fmt.Errorf("ensure network %q: %w", ic.cfg.Network, err)
		}
		gw, err := ic.dc.NetworkGateway(ctx, ic.cfg.Network)
		if err != nil {
			return fmt.Errorf("read the gateway of network %q: %w", ic.cfg.Network, err)
		}
		ic.gateway = gw
	}
	if tunnelOn {
		// Bring the tunnel up before flipping Traefik to its portless posture,
		// so the current Traefik keeps serving until the connector is live.
		// cloudflared reaches Traefik's :80 web entrypoint in either posture.
		cc := tunnel.ConnectorConfig{Image: ic.cfg.CloudflaredImage, Network: ic.cfg.Network, Token: token}
		if err := tunnel.EnsureConnector(ctx, ic.dc, cc, os.Stdout); err != nil {
			return fmt.Errorf("start the tunnel connector: %w", err)
		}
		if err := traefik.EnsureProxy(ctx, ic.dc, ic.proxyConfig(ctx, true, protected), os.Stdout); err != nil {
			return fmt.Errorf("update the proxy (Traefik): %w", err)
		}
		return nil
	}
	// Disabling: restore the ACME/ports Traefik before removing the connector,
	// so ingress is back before the tunnel goes away.
	if err := traefik.EnsureProxy(ctx, ic.dc, ic.proxyConfig(ctx, false, protected), os.Stdout); err != nil {
		return fmt.Errorf("update the proxy (Traefik): %w", err)
	}
	if err := tunnel.RemoveConnector(ctx, ic.dc); err != nil {
		return fmt.Errorf("remove the tunnel connector: %w", err)
	}
	return nil
}

// gatedContainersRemain reports whether any managed container still carries a
// gate. One deployed while a row was protected calls the gate until it is
// replaced, even once no row is protected (the row was turned off or deleted,
// or the redeploy failed), so the gate's host stays mapped until then. When
// the containers can't be listed, the answer is what the running Traefik
// already has (traefik.ProxyMapsGateHost), so that alone doesn't recreate it;
// when that can't be read either, the answer is yes.
func (ic *infraController) gatedContainersRemain(ctx context.Context, tunnelOn bool) bool {
	cs, err := ic.dc.ListContainers(ctx, map[string]string{"outhaul.managed": "true"})
	if err != nil {
		mapped, known := traefik.ProxyMapsGateHost(ctx, ic.dc, ic.proxyConfig(ctx, tunnelOn, false))
		if !known {
			log.Printf("reconcile: could not list containers, so the gate's host stays mapped: %v", err)
			return true
		}
		log.Printf("reconcile: could not list containers, so the gate's host stays as the proxy has it (mapped: %v): %v", mapped, err)
		return mapped
	}
	for _, c := range cs {
		if traefik.HasGateLabel(c.Labels) {
			return true
		}
	}
	return false
}

// proxyConfig builds the Traefik config for the current ingress posture. Tunnel
// mode forces plain HTTP (Cloudflare does TLS); otherwise ACME/ports apply.
// protectedRoutes keeps the gate's host mapped to the host gateway.
func (ic *infraController) proxyConfig(ctx context.Context, tunnelOn, protectedRoutes bool) traefik.ProxyConfig {
	pc := traefik.ProxyConfig{
		ContainerName:    "outhaul-traefik",
		Image:            ic.cfg.TraefikImage,
		Network:          ic.cfg.Network,
		HTTPPort:         "80",
		DockerAPIVersion: ic.dc.ServerAPIVersion(ctx),
		AdminHost:        ic.cfg.AdminHost(),
		AdminPort:        ic.cfg.AdminPort(),
		DynamicDir:       ic.cfg.DynamicDir(),
		ProtectedRoutes:  protectedRoutes,
		Routing:          traefik.RoutingFrom(ic.cfg, false),
	}
	if !ic.cfg.AdminDirect {
		pc.GateHost = ic.gateway
	}
	if tunnelOn {
		pc.TunnelMode = true
		return pc
	}
	pc.ACME = ic.cfg.ACMEEnabled()
	pc.ACMEEmail = ic.cfg.ACMEEmail
	pc.ACMEStaging = ic.cfg.ACMEStaging
	pc.DNSProvider = ic.cfg.ACMEDNSProvider
	pc.DNSEnv = ic.cfg.ACMEDNSEnv
	pc.HTTPSPort = ic.cfg.HTTPSPort
	pc.ACMEStorageDir = ic.cfg.AcmeDir()
	pc.LocalCA = ic.cfg.LocalCAEnabled()
	pc.CertsDir = ic.cfg.CertsDir()
	return pc
}

// logStatus prints a one-line summary of the current ingress posture.
func (ic *infraController) logStatus(ctx context.Context) {
	if on, _ := ic.st.TunnelEnabled(ctx); on {
		log.Printf("Cloudflare Tunnel is the ingress: Traefik serves plain HTTP behind cloudflared on network %q (point Cloudflare hostnames at http://outhaul-traefik:80)", ic.cfg.Network)
		return
	}
	if ic.cfg.LocalCAEnabled() {
		log.Printf("Traefik proxy ready on :80 and :%s (TLS via built-in local CA; install the root with `outhaul ca root`) on network %q", ic.cfg.HTTPSPort, ic.cfg.Network)
		return
	}
	if ic.cfg.TLSEnabled() {
		log.Printf("Traefik proxy ready on :80 and :%s (TLS via Let's Encrypt) on network %q", ic.cfg.HTTPSPort, ic.cfg.Network)
		if ic.cfg.AdminHost() != "" {
			log.Printf("admin UI published over HTTPS at https://%s (Traefik will obtain a cert on first request)", ic.cfg.AdminHost())
		}
		return
	}
	log.Printf("Traefik proxy ready on :80 (network %q; set OUTHAUL_ACME_EMAIL to enable HTTPS)", ic.cfg.Network)
}

// httpOffTunnelError reports why an HTTPS-only server can't also have a
// Cloudflare Tunnel: cloudflared reaches Traefik on port 80, which
// OUTHAUL_HTTP_PORT=off does not publish.
func httpOffTunnelError(cfg config.Config, tunnelOn bool) error {
	if cfg.HTTPOff && tunnelOn {
		return fmt.Errorf("OUTHAUL_HTTP_PORT=off and a Cloudflare Tunnel can't be combined: cloudflared reaches Traefik on port 80; disable the tunnel in Settings first (or unset OUTHAUL_HTTP_PORT)")
	}
	return nil
}

// Enable persists the connector token and brings the tunnel up live. The
// reconcile is run on a detached context, not the caller's request context:
// it recreates Traefik, which can drop the very connection driving the request
// (when the admin UI is proxied through Traefik). A cancelled reconcile could
// strand the server with no ingress, so it must run to completion.
func (ic *infraController) Enable(_ context.Context, token string) error {
	if ic.cfg.HTTPOff {
		return fmt.Errorf("cannot enable a Cloudflare Tunnel while OUTHAUL_HTTP_PORT=off; unset it and restart first")
	}
	if ic.cfg.LocalCAEnabled() {
		return fmt.Errorf("cannot enable a Cloudflare Tunnel while OUTHAUL_LOCAL_CA is set; unset it and restart first")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	if err := ic.st.SetCloudflareToken(ctx, token); err != nil {
		return err
	}
	if err := ic.reconcile(ctx); err != nil {
		return err
	}
	ic.logStatus(ctx)
	return nil
}

// ReconcileProxy reconciles now, for the server to call before it turns
// sign-in on (server.ProxyControl), so Traefik maps the gate's host. Its error
// names the step that failed (reconcile). Like
// Enable, it runs on a detached context: when the mapping was missing it
// recreates Traefik, which can drop the request that asked for it, and a
// cancelled reconcile could leave no Traefik at all. EnsureProxy keeps a
// container whose config already matches, so normally this changes nothing.
func (ic *infraController) ReconcileProxy(_ context.Context) error {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	return ic.reconcile(ctx)
}

// Disable clears the token and returns Traefik to its ACME/ports posture. As
// with Enable, this runs on a detached context (see Enable) so the reconcile
// that recreates Traefik isn't cancelled by the request that triggered it.
func (ic *infraController) Disable(_ context.Context) error {
	if err := adminPublishError(ic.cfg, false); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	if err := ic.st.ClearCloudflareToken(ctx); err != nil {
		return err
	}
	if err := ic.reconcile(ctx); err != nil {
		return err
	}
	ic.logStatus(ctx)
	return nil
}

// openStore opens the database with its secret box. A missing key file is
// generated only when nothing in the database was sealed with the old one:
// a new key would leave every sealed value (env vars, database passwords,
// Git credentials) unreadable.
func openStore(cfg config.Config) (*store.Store, error) {
	box, err := secret.LoadExisting(cfg.SecretKeyPath())
	if err == nil {
		return openStoreWith(cfg, box)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	st, err := store.Open(cfg.DBPath(), nil)
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}
	sealed, err := st.HasSealedValues(context.Background())
	st.Close()
	if err != nil {
		return nil, fmt.Errorf("check for sealed values: %w", err)
	}
	if sealed {
		return nil, fmt.Errorf("secret key %s is missing, but %s holds values encrypted with it; "+
			"restore the key file from a backup (a new key would make those values unreadable)",
			cfg.SecretKeyPath(), cfg.DBPath())
	}
	if box, err = secret.Load(cfg.SecretKeyPath()); err != nil {
		return nil, err
	}
	return openStoreWith(cfg, box)
}

// reserveAdminHost tells the store the host the admin UI is served on (the
// public URL's), so no app can be routed there, whoever writes the route: an
// app on that host would share the admin UI's origin and cookies. Each route
// already on it gets a warning. Nothing is changed or deleted. "" reserves
// nothing.
func reserveAdminHost(ctx context.Context, st *store.Store, host string, logf func(format string, args ...any)) {
	st.SetReservedHost(host)
	if host == "" {
		return
	}
	rows, err := st.ReservedHostDomains(ctx)
	if err != nil {
		logf("WARNING: could not check for apps on Outhaul's own address: %v", err)
		return
	}
	for _, d := range rows {
		logf("WARNING: app %q has the domain %s%s, which is Outhaul's own address (OUTHAUL_PUBLIC_URL). "+
			"An app there shares the admin UI's origin and cookies. Delete that domain on the app's page; Outhaul leaves it in place.",
			d.AppName, d.Host, d.Path)
	}
}

// warnPublishedDatabasePorts names every database whose row still asks for a
// host port while the strict container policy forbids one. The mapping lasts
// until the container is next replaced, so nothing is restarted or
// reprovisioned at startup: the operator clears it from the database's page.
func warnPublishedDatabasePorts(ctx context.Context, st *store.Store, strict bool, logf func(string, ...any)) {
	if !strict {
		return
	}
	dbs, err := st.ListDatabases(ctx)
	if err != nil {
		logf("WARNING: could not check databases for published ports: %v", err)
		return
	}
	for _, d := range dbs {
		if d.ExtPort > 0 {
			logf("WARNING: database %q publishes port %d, which OUTHAUL_CONTAINER_POLICY=strict does not allow; open its page and stop publishing it", d.Name, d.ExtPort)
		}
	}
}

func openStoreWith(cfg config.Config, box *secret.Box) (*store.Store, error) {
	st, err := store.Open(cfg.DBPath(), box)
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}
	return st, nil
}

// newHTTPServer builds the admin UI's server. onShutdown runs as Shutdown
// begins: it ends the long-lived log streams, which never go idle on their own.
func newHTTPServer(addr string, h http.Handler, onShutdown func()) *http.Server {
	s := &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
	}
	s.RegisterOnShutdown(onShutdown)
	return s
}

// adminAddrs is where the admin UI listens. By default that is
// OUTHAUL_LISTEN_ADDR, on every interface. With OUTHAUL_ADMIN_DIRECT=off it is
// loopback (the host itself) and, once known, the IPv4 gateway of Outhaul's
// own Docker network, which is the host's address on that bridge and the only
// one Traefik can reach.
func adminAddrs(cfg config.Config, gateway string) []string {
	if cfg.AdminDirect {
		return []string{cfg.ListenAddr}
	}
	addrs := []string{net.JoinHostPort("127.0.0.1", cfg.AdminPort())}
	if gateway != "" {
		addrs = append(addrs, net.JoinHostPort(gateway, cfg.AdminPort()))
	}
	return addrs
}

// adminPublishError refuses an admin port that nothing can publish: with
// OUTHAUL_ADMIN_DIRECT=off the UI is on loopback and the Docker bridge, so it
// needs Traefik (HTTPS) or the tunnel to reach anyone else.
func adminPublishError(cfg config.Config, tunnelOn bool) error {
	if cfg.AdminDirect || cfg.TLSEnabled() || tunnelOn {
		return nil
	}
	return fmt.Errorf("OUTHAUL_ADMIN_DIRECT=off needs HTTPS or a Cloudflare Tunnel to publish the admin UI, or it is reachable on loopback only")
}

// adminPortWarning is the startup warning for the one posture the header does
// not cover: a required header with the admin port still open on every
// interface. It names neither the header nor its value.
func adminPortWarning(cfg config.Config) string {
	if !cfg.AdminDirect || !cfg.HeaderRequired() {
		return ""
	}
	return "WARNING: a request header is required on every route, but the admin port listens on every interface, where Traefik doesn't check it. Set OUTHAUL_ADMIN_DIRECT=off, or keep other machines from that port."
}

// serveAdmin serves s on adminAddrs. With OUTHAUL_ADMIN_DIRECT=off and no
// gateway yet (Docker wasn't reachable at boot), it listens on loopback at
// once, then looks the gateway up every interval and adds that listener once.
// Listen errors go to errs; a loopback listen failure is fatal to startup.
func serveAdmin(ctx context.Context, s *http.Server, cfg config.Config, gateway func(context.Context) (string, error), interval time.Duration, errs chan<- error) {
	listen := func(addr string) error {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			return err
		}
		go func() {
			if err := s.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errs <- fmt.Errorf("serve %s: %w", addr, err)
			}
		}()
		return nil
	}
	if cfg.AdminDirect {
		if err := listen(cfg.ListenAddr); err != nil {
			errs <- fmt.Errorf("listen %s: %w", cfg.ListenAddr, err)
		}
		return
	}
	// Loopback is the host's own address and must be there from the start; a
	// failure is fatal, as the ordinary ListenAndServe error was.
	loopback := net.JoinHostPort("127.0.0.1", cfg.AdminPort())
	if err := listen(loopback); err != nil {
		errs <- fmt.Errorf("listen %s: %w", loopback, err)
		return
	}
	// The Docker gateway may not exist yet (Docker was unreachable at boot) or
	// may not be bindable yet. Look it up and try again until it works; a
	// failure here never takes the loopback listener down.
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			gw, err := gateway(ctx)
			if err == nil && gw != "" {
				addr := net.JoinHostPort(gw, cfg.AdminPort())
				if err := listen(addr); err == nil {
					return
				} else {
					log.Printf("admin: could not listen on the Docker gateway %s: %v; retrying", addr, err)
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

// shutdown performs graceful shutdown: stop the worker first so in-flight
// pipelines abort (and are marked failed) and their log streams close, and
// stop the backup manager so in-flight backups and restores are cancelled
// and record their outcome; then drain the HTTP server, which ends any log
// streams still open (see newHTTPServer).
func shutdown(httpServer *http.Server, stopWorker context.CancelFunc, workerDone <-chan struct{}, stopBackups func(context.Context) error) error {
	stopWorker()
	drain, cancelDrain := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancelDrain()
	backupsStopped := make(chan error, 1)
	go func() { backupsStopped <- stopBackups(drain) }()

	select {
	case <-workerDone:
	case <-drain.Done():
		log.Print("WARNING: worker did not drain within 20s")
	}
	if err := <-backupsStopped; err != nil {
		log.Printf("WARNING: backups did not stop within 20s (%v); their runs are marked failed at next boot", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(ctx); err != nil {
		return fmt.Errorf("http shutdown: %w", err)
	}
	log.Print("stopped cleanly")
	return nil
}

// startGitPush brings up the deploy bridge (unix socket) and, unless
// OUTHAUL_SSH_ADDR=off, the git-push SSH server. Every failure is non-fatal and
// logged; it returns the SSH server (for live port rebinds from the UI) or nil
// if the SSH server is off or could not start. The hook socket and the bare
// repositories are set up either way.
func startGitPush(ctx context.Context, cfg config.Config, st *store.Store, worker *deploy.Worker, broker *logstream.Broker, serverIP string, repos *gitrepo.Manager, self string) *gitssh.Server {
	if self == "" {
		return nil
	}
	if err := os.MkdirAll(cfg.GitDir(), 0o700); err != nil {
		log.Printf("WARNING: git-push disabled (create git dir): %v", err)
		return nil
	}
	// Deploy bridge on the local unix socket the post-receive hook relays to.
	_ = os.Remove(cfg.GitHookSocketPath()) // clear a stale socket from a crash
	hookLn, err := net.Listen("unix", cfg.GitHookSocketPath())
	if err != nil {
		log.Printf("WARNING: git-push disabled (hook socket): %v", err)
		return nil
	}
	bridge := githook.NewBridge(st, broker, worker, repos, serverIP, cfg.HealthTimeout+5*time.Minute)
	go bridge.Serve(ctx, hookLn)

	// SSH server (public-key auth against push_keys).
	addr, on := sshListenAddr(st, cfg)
	if !on {
		log.Printf("git-push SSH server is off (OUTHAUL_SSH_ADDR=off)")
		return nil
	}
	hostKey, err := gitssh.LoadOrCreateHostKey(cfg.SSHHostKeyPath())
	if err != nil {
		log.Printf("WARNING: git-push SSH server disabled (host key): %v", err)
		return nil
	}
	sshSrv := gitssh.New(hostKey, st, repos)
	if err := sshSrv.Listen(addr); err != nil {
		log.Printf("WARNING: git-push SSH server disabled (bind %s): %v", addr, err)
		return nil
	}
	go func() {
		if err := sshSrv.Serve(ctx); err != nil {
			log.Printf("gitssh: %v", err)
		}
	}()
	log.Printf("git-push SSH server listening on %s", addr)
	return sshSrv
}

// sshListenAddr returns the address the git-push SSH server should listen on
// and whether it should listen at all. OUTHAUL_SSH_ADDR=off turns it off even
// when the UI stored an address; otherwise the stored ssh_addr setting, when
// set, beats the configured default (OUTHAUL_SSH_ADDR or :2222).
func sshListenAddr(st *store.Store, cfg config.Config) (addr string, on bool) {
	if cfg.SSHOff() {
		return "", false
	}
	if v, ok, err := st.GetSetting(context.Background(), "ssh_addr"); err == nil && ok && v != "" {
		return v, true
	}
	return cfg.SSHAddr, true
}

// printSetupHint prints the one-time setup URL on first boot. The first
// "open:" line is the one the installer shows.
func printSetupHint(srv *server.Server, cfg config.Config, token string) {
	needs, err := srv.NeedsSetup(context.Background())
	if err != nil || !needs {
		return
	}
	public, local := setupURLs(cfg, token)
	log.Printf("first-boot setup required")
	if public != "" {
		log.Printf("  open: %s", public)
		log.Printf("  or, over an SSH tunnel to port %s: %s", cfg.AdminPort(), local)
		return
	}
	log.Printf("  open: %s", local)
}

// setupURLs returns the setup page under the public URL, when Traefik
// publishes the admin UI there (HTTPS and an admin host), and on the listen
// address itself.
func setupURLs(cfg config.Config, token string) (public, local string) {
	addr := cfg.ListenAddr
	if len(addr) > 0 && addr[0] == ':' {
		addr = "localhost" + addr
	}
	local = "http://" + addr + "/setup?token=" + token
	if !cfg.TLSEnabled() || cfg.AdminHost() == "" {
		return "", local
	}
	u, err := url.Parse(cfg.PublicURL)
	if err != nil {
		return "", local
	}
	// The admin router matches the host only, so any path is dropped.
	return (&url.URL{Scheme: u.Scheme, Host: u.Host, Path: "/setup", RawQuery: "token=" + token}).String(), local
}
