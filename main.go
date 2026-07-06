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
	"syscall"
	"time"

	"github.com/james-smart/outhaul/internal/backup"
	"github.com/james-smart/outhaul/internal/builder"
	"github.com/james-smart/outhaul/internal/catalog"
	"github.com/james-smart/outhaul/internal/compose"
	"github.com/james-smart/outhaul/internal/config"
	"github.com/james-smart/outhaul/internal/dbaas"
	"github.com/james-smart/outhaul/internal/deploy"
	"github.com/james-smart/outhaul/internal/docker"
	"github.com/james-smart/outhaul/internal/githook"
	"github.com/james-smart/outhaul/internal/github"
	"github.com/james-smart/outhaul/internal/gitrepo"
	"github.com/james-smart/outhaul/internal/gitssh"
	"github.com/james-smart/outhaul/internal/logstream"
	"github.com/james-smart/outhaul/internal/prune"
	"github.com/james-smart/outhaul/internal/secret"
	"github.com/james-smart/outhaul/internal/server"
	"github.com/james-smart/outhaul/internal/store"
	"github.com/james-smart/outhaul/internal/traefik"
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

	ensureInfra(dc, cfg)

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
	go func() {
		worker.Run(workerCtx)
		close(workerDone)
	}()

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

	setupToken := server.NewToken()
	srv, err := server.New(st, worker, dc, compose.NewDocker(), dbm, backups, broker, ghClient, cfg.PublicURL, serverIP, cfg.TLSEnabled(), setupToken)
	if err != nil {
		stopWorker()
		return fmt.Errorf("build server: %w", err)
	}
	srv.SetRepos(repos)
	if sshSrv != nil {
		srv.SetSSHControl(sshSrv)
	}
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

// ensureInfra brings up the shared network and the Traefik proxy. Failures are
// logged, not fatal, so the admin UI still starts (e.g. when Docker is down).
func ensureInfra(dc docker.Client, cfg config.Config) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := dc.Ping(ctx); err != nil {
		log.Printf("WARNING: Docker not reachable (%v); deploys will fail until it is available", err)
		return
	}
	pc := traefik.ProxyConfig{
		ContainerName:    "outhaul-traefik",
		Image:            cfg.TraefikImage,
		Network:          cfg.Network,
		HTTPPort:         "80",
		TLSEnabled:       cfg.TLSEnabled(),
		ACMEEmail:        cfg.ACMEEmail,
		ACMEStaging:      cfg.ACMEStaging,
		HTTPSPort:        cfg.HTTPSPort,
		ACMEStorageDir:   cfg.AcmeDir(),
		DockerAPIVersion: dc.ServerAPIVersion(ctx),
		AdminHost:        cfg.AdminHost(),
		AdminPort:        cfg.AdminPort(),
		DynamicDir:       cfg.DynamicDir(),
	}
	if err := traefik.EnsureProxy(ctx, dc, pc, os.Stdout); err != nil {
		log.Printf("WARNING: could not ensure Traefik proxy: %v", err)
		return
	}
	if cfg.TLSEnabled() {
		log.Printf("Traefik proxy ready on :80 and :%s (TLS via Let's Encrypt) on network %q", cfg.HTTPSPort, cfg.Network)
		if cfg.AdminHost() != "" {
			log.Printf("admin UI published over HTTPS at https://%s (Traefik will obtain a cert on first request)", cfg.AdminHost())
		}
	} else {
		log.Printf("Traefik proxy ready on :80 (network %q; set OUTHAUL_ACME_EMAIL to enable HTTPS)", cfg.Network)
	}
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
