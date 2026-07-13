// Command outhaul is a single-binary, self-hosted PaaS: git-push-to-deploy on
// one VPS, orchestrating Docker and Traefik. `outhaul serve` starts the admin
// UI and the background deploy worker.
package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
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
	"github.com/outhaul-dev/outhaul/internal/gitssh"
	"github.com/outhaul-dev/outhaul/internal/logstream"
	"github.com/outhaul-dev/outhaul/internal/previewmgr"
	"github.com/outhaul-dev/outhaul/internal/prune"
	"github.com/outhaul-dev/outhaul/internal/secret"
	"github.com/outhaul-dev/outhaul/internal/server"
	"github.com/outhaul-dev/outhaul/internal/store"
	"github.com/outhaul-dev/outhaul/internal/traefik"
	"github.com/outhaul-dev/outhaul/internal/tunnel"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("outhaul: ")

	if len(os.Args) >= 2 && os.Args[1] == "git-hook" {
		os.Exit(runGitHook(os.Args[2:]))
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

func serve() error {
	cfg := config.Load(os.Getenv)

	// Data directories.
	if err := os.MkdirAll(cfg.WorkDir(), 0o755); err != nil {
		return fmt.Errorf("create data dir %s: %w", cfg.DataDir, err)
	}
	if err := os.MkdirAll(cfg.AcmeDir(), 0o700); err != nil {
		return fmt.Errorf("create acme dir: %w", err)
	}
	box, err := secret.Load(cfg.SecretKeyPath())
	if err != nil {
		return fmt.Errorf("load secret key: %w", err)
	}

	// Store + crash recovery.
	st, err := store.Open(cfg.DBPath(), box)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer st.Close()

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

	infra := ensureInfra(dc, cfg, st)

	ghClient := github.New()

	// Background worker.
	broker := logstream.New()
	worker := deploy.NewWorker(st, dc,
		deploy.Builders{Nixpacks: builder.NewNixpacks(), Dockerfile: builder.NewDocker()},
		compose.NewDocker(), deploy.NewGit(), broker, ghClient, cfg)

	// Image pruner: after-deploy retention hook + daily sweep.
	pruner := prune.New(st, dc, cfg.ImageKeep, cfg.WorkDir())
	worker.SetPruner(pruner)

	workerCtx, stopWorker := context.WithCancel(context.Background())
	workerDone := make(chan struct{})
	// worker.Run is launched below, after the preview manager is wired in as the
	// after-deploy hook (SetDeployHook must be called before Run).

	// Database manager (databases-as-a-service).
	dbm := dbaas.NewManager(st, dc, cfg.Network, cfg.DatabasesDir())

	// Backup scheduler (dumps + volume tarballs to S3-compatible storage).
	backups := backup.NewManager(st, dc, cfg.WorkDir())
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

	// Preview environments: per-PR ephemeral child apps. The token source mints
	// an installation token (via the App JWT) so the manager can post PR comments.
	tokenSource := func(ctx context.Context) (string, bool, error) {
		ga, ok, err := st.GithubApp(ctx)
		if err != nil {
			return "", false, err
		}
		if !ok {
			return "", false, nil
		}
		jwt, err := github.AppJWT(ga.PrivateKey, ga.AppID, time.Now())
		if err != nil {
			return "", false, err
		}
		tok, err := ghClient.InstallationToken(ctx, jwt, ga.InstallationID)
		return tok, err == nil, err
	}
	previews := previewmgr.New(st, worker,
		&previewDBProvisioner{st: st, dbm: dbm},
		&previewDocker{st: st, runtime: dc, compose: compose.NewDocker()},
		ghClient, tokenSource, serverIP)
	worker.SetDeployHook(previews.OnDeployFinished) // update a preview's PR comment + status on deploy completion
	go func() {
		worker.Run(workerCtx)
		close(workerDone)
	}()
	go previews.Run(workerCtx)

	setupToken := server.NewToken()
	srv, err := server.New(st, worker, dc, compose.NewDocker(), dbm, backups, broker, ghClient,
		previews,
		cfg.PublicURL, serverIP, cfg.TLSEnabled(), setupToken)
	if err != nil {
		stopWorker()
		return fmt.Errorf("build server: %w", err)
	}
	srv.SetRepos(repos)
	if sshSrv != nil {
		srv.SetSSHControl(sshSrv)
	}
	srv.SetTunnelControl(infra)
	printSetupHint(srv, cfg, setupToken)

	httpServer := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	serverErr := make(chan error, 1)
	go func() {
		log.Printf("listening on %s", cfg.ListenAddr)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			serverErr <- err
		}
	}()

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

	return shutdown(httpServer, stopWorker, workerDone)
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
}

// ensureInfra brings up the shared network, Traefik, and (if the tunnel is
// enabled) the cloudflared connector. Failures are logged, not fatal, so the
// admin UI still starts (e.g. when Docker is down). It returns a controller the
// server uses to toggle the tunnel at runtime.
func ensureInfra(dc docker.Client, cfg config.Config, st *store.Store) *infraController {
	ic := &infraController{dc: dc, st: st, cfg: cfg}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := dc.Ping(ctx); err != nil {
		log.Printf("WARNING: Docker not reachable (%v); deploys will fail until it is available", err)
		return ic
	}
	if err := ic.reconcile(ctx); err != nil {
		log.Printf("WARNING: could not ensure infrastructure: %v", err)
		return ic
	}
	ic.logStatus(ctx)
	return ic
}

// reconcile makes Traefik's posture and the connector match the stored tunnel
// state: tunnel on -> plain-HTTP Traefik + running connector; tunnel off ->
// ACME/ports Traefik + no connector.
func (ic *infraController) reconcile(ctx context.Context) error {
	ic.mu.Lock()
	defer ic.mu.Unlock()

	token, tunnelOn, err := ic.st.CloudflareToken(ctx)
	if err != nil {
		return fmt.Errorf("read tunnel token: %w", err)
	}
	if tunnelOn {
		// Bring the tunnel up before flipping Traefik to its portless posture,
		// so the current Traefik keeps serving until the connector is live.
		// cloudflared reaches Traefik's :80 web entrypoint in either posture.
		cc := tunnel.ConnectorConfig{Image: ic.cfg.CloudflaredImage, Network: ic.cfg.Network, Token: token}
		if err := tunnel.EnsureConnector(ctx, ic.dc, cc, os.Stdout); err != nil {
			return fmt.Errorf("ensure connector: %w", err)
		}
		if err := traefik.EnsureProxy(ctx, ic.dc, ic.proxyConfig(ctx, true), os.Stdout); err != nil {
			return fmt.Errorf("ensure traefik: %w", err)
		}
		return nil
	}
	// Disabling: restore the ACME/ports Traefik before removing the connector,
	// so ingress is back before the tunnel goes away.
	if err := traefik.EnsureProxy(ctx, ic.dc, ic.proxyConfig(ctx, false), os.Stdout); err != nil {
		return fmt.Errorf("ensure traefik: %w", err)
	}
	if err := tunnel.RemoveConnector(ctx, ic.dc); err != nil {
		return fmt.Errorf("remove connector: %w", err)
	}
	return nil
}

// proxyConfig builds the Traefik config for the current ingress posture. Tunnel
// mode forces plain HTTP (Cloudflare does TLS); otherwise ACME/ports apply.
func (ic *infraController) proxyConfig(ctx context.Context, tunnelOn bool) traefik.ProxyConfig {
	pc := traefik.ProxyConfig{
		ContainerName:    "outhaul-traefik",
		Image:            ic.cfg.TraefikImage,
		Network:          ic.cfg.Network,
		HTTPPort:         "80",
		DockerAPIVersion: ic.dc.ServerAPIVersion(ctx),
		AdminHost:        ic.cfg.AdminHost(),
		AdminPort:        ic.cfg.AdminPort(),
		DynamicDir:       ic.cfg.DynamicDir(),
	}
	if tunnelOn {
		pc.TunnelMode = true
		return pc
	}
	pc.TLSEnabled = ic.cfg.TLSEnabled()
	pc.ACMEEmail = ic.cfg.ACMEEmail
	pc.ACMEStaging = ic.cfg.ACMEStaging
	pc.HTTPSPort = ic.cfg.HTTPSPort
	pc.ACMEStorageDir = ic.cfg.AcmeDir()
	return pc
}

// logStatus prints a one-line summary of the current ingress posture.
func (ic *infraController) logStatus(ctx context.Context) {
	if on, _ := ic.st.TunnelEnabled(ctx); on {
		log.Printf("Cloudflare Tunnel is the ingress: Traefik serves plain HTTP behind cloudflared on network %q (point Cloudflare hostnames at http://outhaul-traefik:80)", ic.cfg.Network)
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

// Enable persists the connector token and brings the tunnel up live. The
// reconcile is run on a detached context, not the caller's request context:
// it recreates Traefik, which can drop the very connection driving the request
// (when the admin UI is proxied through Traefik). A cancelled reconcile could
// strand the server with no ingress, so it must run to completion.
func (ic *infraController) Enable(_ context.Context, token string) error {
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

// Disable clears the token and returns Traefik to its ACME/ports posture. As
// with Enable, this runs on a detached context (see Enable) so the reconcile
// that recreates Traefik isn't cancelled by the request that triggered it.
func (ic *infraController) Disable(_ context.Context) error {
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

// shutdown performs graceful shutdown: stop the worker first so in-flight
// pipelines abort (and are marked failed) and their log streams close, which
// lets long-lived SSE connections end, then drain the HTTP server.
func shutdown(httpServer *http.Server, stopWorker context.CancelFunc, workerDone <-chan struct{}) error {
	stopWorker()
	select {
	case <-workerDone:
	case <-time.After(20 * time.Second):
		log.Print("WARNING: worker did not drain within 20s")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(ctx); err != nil {
		return fmt.Errorf("http shutdown: %w", err)
	}
	log.Print("stopped cleanly")
	return nil
}

// startGitPush brings up the deploy bridge (unix socket) and the git-push SSH
// server. Every failure is non-fatal and logged; it returns the SSH server (for
// live port rebinds from the UI) or nil if git-push could not start.
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
	hostKey, err := gitssh.LoadOrCreateHostKey(cfg.SSHHostKeyPath())
	if err != nil {
		log.Printf("WARNING: git-push SSH server disabled (host key): %v", err)
		return nil
	}
	sshSrv := gitssh.New(hostKey, st, repos)
	addr := resolveSSHAddr(st, cfg)
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

// resolveSSHAddr returns the stored ssh_addr setting if set, else the configured
// default (OUTHAUL_SSH_ADDR or :2222).
func resolveSSHAddr(st *store.Store, cfg config.Config) string {
	if v, ok, err := st.GetSetting(context.Background(), "ssh_addr"); err == nil && ok && v != "" {
		return v
	}
	return cfg.SSHAddr
}

// printSetupHint prints the one-time setup URL on first boot.
func printSetupHint(srv *server.Server, cfg config.Config, token string) {
	needs, err := srv.NeedsSetup(context.Background())
	if err != nil || !needs {
		return
	}
	host := "localhost"
	addr := cfg.ListenAddr
	if len(addr) > 0 && addr[0] == ':' {
		addr = host + addr
	}
	log.Printf("first-boot setup required")
	log.Printf("  open: http://%s/setup?token=%s", addr, token)
}
