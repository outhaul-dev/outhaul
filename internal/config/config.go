// Package config resolves Outhaul's runtime configuration. There are no config
// files (a locked decision): defaults plus OUTHAUL_* environment overrides,
// all rooted at a single data directory.
package config

import (
	"net"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// DefaultTraefikImage is the Traefik image Outhaul pulls and manages. Pinned to
// a specific version so upgrades are deliberate.
const DefaultTraefikImage = "traefik:v3.7.6"

// DefaultCloudflaredImage is the cloudflared image Outhaul runs for a Cloudflare
// Tunnel. Pinned so upgrades are deliberate; override with
// OUTHAUL_CLOUDFLARED_IMAGE.
const DefaultCloudflaredImage = "cloudflare/cloudflared:2026.7.0"

// Config holds resolved runtime settings.
type Config struct {
	DataDir          string // root data dir; holds the SQLite DB and work dirs
	ListenAddr       string // address the admin UI/API listens on
	SSHAddr          string // default listen address for the git-push SSH server
	DockerHost       string // Docker endpoint; empty means "use the SDK's env default"
	TraefikImage     string // image used for the managed Traefik container
	CloudflaredImage string // image used for the managed cloudflared connector
	Network          string // shared Docker network app containers + Traefik join

	ACMEEmail     string        // Let's Encrypt account email; empty disables TLS
	ACMEStaging   bool          // use the LE staging CA (avoid rate limits)
	HTTPSPort     string        // host port for the websecure entrypoint
	HealthTimeout time.Duration // deploy health-check deadline
	ImageKeep     int           // built images kept per app; 0 disables pruning

	PublicURL string // externally reachable base URL of the admin UI (for GitHub callbacks/webhooks); empty disables GitHub App setup
	ServerIP  string // public IP used in generated sslip.io template domains; empty means auto-detect
}

// Getenv matches os.Getenv; injected so Load is testable without touching the
// process environment.
type Getenv func(string) string

// Load resolves configuration from defaults overlaid with OUTHAUL_* env vars.
func Load(getenv Getenv) Config {
	return Config{
		DataDir:          or(getenv("OUTHAUL_DATA_DIR"), "/var/lib/outhaul"),
		ListenAddr:       or(getenv("OUTHAUL_LISTEN_ADDR"), ":8080"),
		SSHAddr:          or(getenv("OUTHAUL_SSH_ADDR"), ":2222"),
		DockerHost:       getenv("OUTHAUL_DOCKER_HOST"), // empty is a valid value: defer to SDK
		TraefikImage:     or(getenv("OUTHAUL_TRAEFIK_IMAGE"), DefaultTraefikImage),
		CloudflaredImage: or(getenv("OUTHAUL_CLOUDFLARED_IMAGE"), DefaultCloudflaredImage),
		Network:          or(getenv("OUTHAUL_NETWORK"), "outhaul"),

		ACMEEmail:     firstField(getenv("OUTHAUL_ACME_EMAIL")),
		ACMEStaging:   truthy(getenv("OUTHAUL_ACME_STAGING")),
		HTTPSPort:     or(getenv("OUTHAUL_HTTPS_PORT"), "443"),
		HealthTimeout: durationOr(getenv("OUTHAUL_HEALTH_TIMEOUT"), 60*time.Second),
		ImageKeep:     intOr(getenv("OUTHAUL_IMAGE_KEEP"), 5),

		PublicURL: getenv("OUTHAUL_PUBLIC_URL"),
		ServerIP:  getenv("OUTHAUL_SERVER_IP"),
	}
}

// DBPath is the SQLite database file path, derived from DataDir.
func (c Config) DBPath() string {
	return filepath.Join(c.DataDir, "outhaul.db")
}

// WorkDir is where a deployment's repo is cloned and built.
func (c Config) WorkDir() string {
	return filepath.Join(c.DataDir, "work")
}

// GitDir is the root holding per-app bare repos for git-push deploys.
func (c Config) GitDir() string { return filepath.Join(c.DataDir, "git") }

// GitRepoDir is the bare repo path for a push-source app. It does not sanitize
// app; callers must pass an already-validated app name (see gitrepo.Manager.Path).
func (c Config) GitRepoDir(app string) string {
	return filepath.Join(c.GitDir(), app+".git")
}

// SSHHostKeyPath is the persistent host key for the git-push SSH server.
func (c Config) SSHHostKeyPath() string {
	return filepath.Join(c.DataDir, "ssh_host_ed25519_key")
}

// GitHookSocketPath is the unix socket the post-receive hook relays through.
func (c Config) GitHookSocketPath() string {
	return filepath.Join(c.DataDir, "git-hook.sock")
}

func or(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

// TLSEnabled reports whether automatic HTTPS is configured (an ACME email set).
func (c Config) TLSEnabled() bool { return c.ACMEEmail != "" }

// PublicURLSet reports whether an externally reachable base URL is configured.
func (c Config) PublicURLSet() bool { return c.PublicURL != "" }

// SecretKeyPath is the env-encryption key file.
func (c Config) SecretKeyPath() string { return filepath.Join(c.DataDir, "secret.key") }

// AcmeDir is the host directory bind-mounted into Traefik for acme.json.
func (c Config) AcmeDir() string { return filepath.Join(c.DataDir, "traefik", "acme") }

// DynamicDir is the host directory bind-mounted into Traefik as its file
// provider, holding the dynamic config that routes the admin UI over HTTPS.
func (c Config) DynamicDir() string { return filepath.Join(c.DataDir, "traefik", "dynamic") }

// AdminHost is the hostname the admin UI should be served under, parsed from
// PublicURL (e.g. "outhaul.example.com" from "https://outhaul.example.com").
// Empty when PublicURL is unset or unparseable — no admin route is published.
func (c Config) AdminHost() string {
	if c.PublicURL == "" {
		return ""
	}
	u, err := url.Parse(c.PublicURL)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// AdminPort is the TCP port the admin UI listens on, derived from ListenAddr
// (":8080" -> "8080"). Traefik reaches it on the host via host-gateway.
func (c Config) AdminPort() string {
	_, port, err := net.SplitHostPort(c.ListenAddr)
	if err != nil || port == "" {
		return "8080"
	}
	return port
}

// DatabasesDir is the root under which each managed database gets a
// per-database data directory, bind-mounted into its container.
func (c Config) DatabasesDir() string { return filepath.Join(c.DataDir, "databases") }

// firstField returns the first whitespace-delimited token of v, also dropping
// anything from a '#' onward. systemd's EnvironmentFile keeps inline "# ..."
// comments as part of the value, which would otherwise corrupt a setting like
// the ACME email (which never contains spaces or '#').
func firstField(v string) string {
	if i := strings.IndexByte(v, '#'); i >= 0 {
		v = v[:i]
	}
	if f := strings.Fields(v); len(f) > 0 {
		return f[0]
	}
	return ""
}

func truthy(v string) bool {
	switch v {
	case "1", "true", "TRUE", "yes", "on":
		return true
	}
	return false
}

// intOr parses v as a non-negative integer, falling back on empty, invalid,
// or negative input.
func intOr(v string, fallback int) int {
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return fallback
	}
	return n
}

func durationOr(v string, fallback time.Duration) time.Duration {
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return fallback
	}
	return d
}
