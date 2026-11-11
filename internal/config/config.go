// Package config resolves Outhaul's runtime configuration. There are no config
// files (a locked decision): defaults plus OUTHAUL_* environment overrides,
// all rooted at a single data directory.
package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/outhaul-dev/outhaul/internal/cron"
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

	ACMEEmail       string        // Let's Encrypt account email; empty disables TLS
	ACMEStaging     bool          // use the LE staging CA (avoid rate limits)
	LocalCA         bool          // serve HTTPS from the built-in local CA (LAN installs; excludes ACME)
	HTTPSPort       string        // host port for the websecure entrypoint
	HealthTimeout   time.Duration // deploy health-check deadline
	ImageKeep       int           // built images kept per app; 0 disables pruning
	PruneBuildCache bool          // nightly prune of the daemon-wide build cache; off for a shared host

	PublicURL string // externally reachable base URL of the admin UI (for GitHub callbacks/webhooks); empty disables GitHub App setup
	ServerIP  string // public IP used in generated sslip.io template domains; empty means auto-detect

	BackupAzureAccount   string // OUTHAUL_BACKUP_AZURE_ACCOUNT; the storage account for the Azure destination
	BackupAzureContainer string // OUTHAUL_BACKUP_AZURE_CONTAINER; its private container
	AzureClientID        string // AZURE_CLIENT_ID; the managed identity, when the VM has more than one

	StateBackupSchedule    string // OUTHAUL_STATE_BACKUP_SCHEDULE; unset disables state backups
	StateBackupDestination string // OUTHAUL_STATE_BACKUP_DESTINATION; "" means the Azure row
	StateBackupKeep        int    // OUTHAUL_STATE_BACKUP_KEEP; 0 keeps every copy
	StateBackupPrefix      string // OUTHAUL_STATE_BACKUP_PREFIX; keeps hosts sharing a container apart

	stateBackupKeepSet bool    // OUTHAUL_STATE_BACKUP_KEEP was present
	problems           []error // malformed OUTHAUL_* values; reported by Validate
}

// Getenv matches os.Getenv; injected so Load is testable without touching the
// process environment.
type Getenv func(string) string

// Load resolves configuration from defaults overlaid with OUTHAUL_* env vars.
// A malformed value falls back to its default here, so commands that only
// need paths keep working; Validate reports it, and serve refuses to start.
func Load(getenv Getenv) Config {
	p := parser{getenv: getenv}
	c := Config{
		DataDir:          or(getenv("OUTHAUL_DATA_DIR"), "/var/lib/outhaul"),
		ListenAddr:       p.addr("OUTHAUL_LISTEN_ADDR", ":8080"),
		SSHAddr:          p.addr("OUTHAUL_SSH_ADDR", ":2222"),
		DockerHost:       getenv("OUTHAUL_DOCKER_HOST"), // empty is a valid value: defer to SDK
		TraefikImage:     or(getenv("OUTHAUL_TRAEFIK_IMAGE"), DefaultTraefikImage),
		CloudflaredImage: or(getenv("OUTHAUL_CLOUDFLARED_IMAGE"), DefaultCloudflaredImage),
		Network:          or(getenv("OUTHAUL_NETWORK"), "outhaul"),

		ACMEEmail:       firstField(getenv("OUTHAUL_ACME_EMAIL")),
		ACMEStaging:     p.bool("OUTHAUL_ACME_STAGING"),
		LocalCA:         p.bool("OUTHAUL_LOCAL_CA"),
		HTTPSPort:       p.port("OUTHAUL_HTTPS_PORT", "443"),
		HealthTimeout:   p.duration("OUTHAUL_HEALTH_TIMEOUT", 60*time.Second),
		ImageKeep:       p.count("OUTHAUL_IMAGE_KEEP", 5),
		PruneBuildCache: p.boolDefault("OUTHAUL_PRUNE_BUILD_CACHE", true),

		PublicURL: p.url("OUTHAUL_PUBLIC_URL"),
		ServerIP:  p.ip("OUTHAUL_SERVER_IP"),

		BackupAzureAccount:   getenv("OUTHAUL_BACKUP_AZURE_ACCOUNT"),
		BackupAzureContainer: getenv("OUTHAUL_BACKUP_AZURE_CONTAINER"),
		AzureClientID:        getenv("AZURE_CLIENT_ID"),

		StateBackupSchedule:    p.cron("OUTHAUL_STATE_BACKUP_SCHEDULE"),
		StateBackupDestination: getenv("OUTHAUL_STATE_BACKUP_DESTINATION"),
		StateBackupKeep:        p.count("OUTHAUL_STATE_BACKUP_KEEP", 30),
		StateBackupPrefix:      p.keyPrefix("OUTHAUL_STATE_BACKUP_PREFIX"),
	}
	c.stateBackupKeepSet = getenv("OUTHAUL_STATE_BACKUP_KEEP") != ""
	c.problems = p.problems
	return c
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

// ACMEEnabled reports whether Let's Encrypt automation is configured.
func (c Config) ACMEEnabled() bool { return c.ACMEEmail != "" }

// LocalCAEnabled reports whether the built-in local CA serves HTTPS instead.
func (c Config) LocalCAEnabled() bool { return c.LocalCA }

// TLSEnabled reports whether HTTPS is available by either mechanism; it gates
// the websecure entrypoint, redirects, and the per-domain TLS toggle.
func (c Config) TLSEnabled() bool { return c.ACMEEnabled() || c.LocalCAEnabled() }

// CertResolver is the Traefik certresolver TLS routers should reference;
// empty when certs come from the file provider (local CA) instead of ACME.
func (c Config) CertResolver() string {
	if c.ACMEEnabled() {
		return "le"
	}
	return ""
}

// Validate rejects malformed values and impossible combinations before any
// infrastructure moves.
// Azure destination shapes, checked before startup so a typo names its
// variable instead of failing the first backup.
var (
	azureAccountRe   = regexp.MustCompile(`^[a-z0-9]{3,24}$`)
	azureContainerRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$`)
	azureClientIDRe  = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
)

func (c Config) Validate() error {
	errs := append([]error(nil), c.problems...)
	if c.LocalCAEnabled() && c.ACMEEnabled() {
		errs = append(errs, errors.New("OUTHAUL_LOCAL_CA and OUTHAUL_ACME_EMAIL are mutually exclusive: choose local-CA or Let's Encrypt HTTPS"))
	}
	switch {
	case c.BackupAzureAccount == "" && c.BackupAzureContainer != "":
		errs = append(errs, errors.New("OUTHAUL_BACKUP_AZURE_ACCOUNT is required when OUTHAUL_BACKUP_AZURE_CONTAINER is set"))
	case c.BackupAzureAccount != "" && c.BackupAzureContainer == "":
		errs = append(errs, errors.New("OUTHAUL_BACKUP_AZURE_CONTAINER is required when OUTHAUL_BACKUP_AZURE_ACCOUNT is set"))
	case c.BackupAzureAccount != "":
		if !azureAccountRe.MatchString(c.BackupAzureAccount) {
			errs = append(errs, fmt.Errorf("OUTHAUL_BACKUP_AZURE_ACCOUNT=%q: want 3 to 24 lowercase letters and digits", c.BackupAzureAccount))
		}
		if !azureContainerRe.MatchString(c.BackupAzureContainer) || strings.Contains(c.BackupAzureContainer, "--") {
			errs = append(errs, fmt.Errorf("OUTHAUL_BACKUP_AZURE_CONTAINER=%q: want 3 to 63 lowercase letters, digits and single hyphens, starting and ending with a letter or digit", c.BackupAzureContainer))
		}
		if c.AzureClientID != "" && !azureClientIDRe.MatchString(c.AzureClientID) {
			errs = append(errs, fmt.Errorf("AZURE_CLIENT_ID=%q: want a GUID such as 8f7c1a2e-0000-4000-8000-000000000000", c.AzureClientID))
		}
	}
	if c.StateBackupSchedule == "" {
		if c.StateBackupDestination != "" {
			errs = append(errs, fmt.Errorf("OUTHAUL_STATE_BACKUP_DESTINATION=%q: needs OUTHAUL_STATE_BACKUP_SCHEDULE", c.StateBackupDestination))
		}
		if c.StateBackupPrefix != "" {
			errs = append(errs, fmt.Errorf("OUTHAUL_STATE_BACKUP_PREFIX=%q: needs OUTHAUL_STATE_BACKUP_SCHEDULE", c.StateBackupPrefix))
		}
		if c.stateBackupKeepSet {
			errs = append(errs, errors.New("OUTHAUL_STATE_BACKUP_KEEP: needs OUTHAUL_STATE_BACKUP_SCHEDULE"))
		}
	} else if c.StateBackupDestination == "" && (c.BackupAzureAccount == "" || c.BackupAzureContainer == "") {
		errs = append(errs, errors.New("OUTHAUL_STATE_BACKUP_SCHEDULE needs a destination: set OUTHAUL_STATE_BACKUP_DESTINATION, or OUTHAUL_BACKUP_AZURE_ACCOUNT and OUTHAUL_BACKUP_AZURE_CONTAINER"))
	}
	return errors.Join(errs...)
}

// PublicURLSet reports whether an externally reachable base URL is configured.
func (c Config) PublicURLSet() bool { return c.PublicURL != "" }

// SecretKeyPath is the env-encryption key file.
func (c Config) SecretKeyPath() string { return filepath.Join(c.DataDir, "secret.key") }

// AcmeDir is the host directory bind-mounted into Traefik for acme.json.
func (c Config) AcmeDir() string { return filepath.Join(c.DataDir, "traefik", "acme") }

// DynamicDir is the host directory bind-mounted into Traefik as its file
// provider, holding the dynamic config that routes the admin UI over HTTPS.
func (c Config) DynamicDir() string { return filepath.Join(c.DataDir, "traefik", "dynamic") }

// CADir holds the local CA's root certificate and key.
func (c Config) CADir() string { return filepath.Join(c.DataDir, "ca") }

// CertsDir is the host directory of local-CA leaf certs, bind-mounted
// read-only into Traefik at localca.ContainerCertsDir.
func (c Config) CertsDir() string { return filepath.Join(c.DataDir, "traefik", "certs") }

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

// GateAddress is the forward-auth address a protected route's Traefik
// middleware calls: the gate-check endpoint on the host, reached through the
// host gateway on the admin port, as the admin route's file-provider config is.
func (c Config) GateAddress() string {
	return "http://host.docker.internal:" + c.AdminPort() + "/gate/check"
}

// UpdateDir is where Outhaul and the root updater exchange the update request,
// status and log files.
func (c Config) UpdateDir() string { return filepath.Join(c.DataDir, "update") }

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

// parser reads OUTHAUL_* values, keeping a default for each malformed one
// and recording why it was rejected.
type parser struct {
	getenv   Getenv
	problems []error
}

func (p *parser) reject(key, v, want string) {
	p.problems = append(p.problems, fmt.Errorf("%s=%q: want %s", key, v, want))
}

// bool accepts 1/0, true/false, yes/no and on/off in any case; empty is false.
func (p *parser) bool(key string) bool { return p.boolDefault(key, false) }

// boolDefault is bool with an explicit value for an unset variable.
func (p *parser) boolDefault(key string, fallback bool) bool {
	v := p.getenv(key)
	switch strings.ToLower(v) {
	case "":
		return fallback
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	p.reject(key, v, "true or false")
	return fallback
}

// count parses a non-negative integer.
func (p *parser) count(key string, fallback int) int {
	v := p.getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		p.reject(key, v, "a whole number, 0 or more")
		return fallback
	}
	return n
}

// duration parses a positive Go duration such as 90s or 2m.
func (p *parser) duration(key string, fallback time.Duration) time.Duration {
	v := p.getenv(key)
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		p.reject(key, v, "a positive duration with a unit, such as 90s or 2m")
		return fallback
	}
	return d
}

// port parses a TCP port number.
func (p *parser) port(key, fallback string) string {
	v := p.getenv(key)
	if v == "" {
		return fallback
	}
	if n, err := strconv.Atoi(v); err != nil || n < 1 || n > 65535 {
		p.reject(key, v, "a port number from 1 to 65535")
		return fallback
	}
	return v
}

// addr parses a listen address, host:port with the host optional (":8080").
func (p *parser) addr(key, fallback string) string {
	v := p.getenv(key)
	if v == "" {
		return fallback
	}
	_, port, err := net.SplitHostPort(v)
	if n, perr := strconv.Atoi(port); err != nil || perr != nil || n < 1 || n > 65535 {
		p.reject(key, v, "host:port, such as :8080 or 127.0.0.1:8080")
		return fallback
	}
	return v
}

// url parses an absolute http(s) URL; empty stays empty.
func (p *parser) url(key string) string {
	v := p.getenv(key)
	if v == "" {
		return ""
	}
	u, err := url.Parse(v)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		p.reject(key, v, "an absolute URL such as https://paas.example.com")
		return ""
	}
	return v
}

// stateBackupPrefixRe is a key prefix: segments of letters, digits, '.', '_'
// and '-', joined by single slashes.
var stateBackupPrefixRe = regexp.MustCompile(`^[A-Za-z0-9._-]+(/[A-Za-z0-9._-]+)*$`)

// cron parses a five-field cron expression; empty stays empty.
func (p *parser) cron(key string) string {
	v := p.getenv(key)
	if v == "" {
		return ""
	}
	if _, err := cron.Parse(v); err != nil {
		p.reject(key, v, "a 5-field cron expression such as 15 2 * * *")
	}
	return v
}

// keyPrefix parses a slash-separated object-key prefix; empty stays empty.
func (p *parser) keyPrefix(key string) string {
	v := p.getenv(key)
	if v == "" {
		return ""
	}
	if !stateBackupPrefixRe.MatchString(v) {
		p.reject(key, v, "letters, digits, '-', '_', '.' and '/', with no '/' at either end")
	}
	return v
}

// ip parses an IP address; empty stays empty.
func (p *parser) ip(key string) string {
	v := p.getenv(key)
	if v != "" && net.ParseIP(v) == nil {
		p.reject(key, v, "an IP address")
		return ""
	}
	return v
}
