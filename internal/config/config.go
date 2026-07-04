// Package config resolves Outhaul's runtime configuration. There are no config
// files (a locked decision): defaults plus OUTHAUL_* environment overrides,
// all rooted at a single data directory.
package config

import (
	"path/filepath"
	"strconv"
	"time"
)

// DefaultTraefikImage is the Traefik image Outhaul pulls and manages. Pinned to
// a major version so upgrades are deliberate.
const DefaultTraefikImage = "traefik:v3.3"

// Config holds resolved runtime settings.
type Config struct {
	DataDir      string // root data dir; holds the SQLite DB and work dirs
	ListenAddr   string // address the admin UI/API listens on
	DockerHost   string // Docker endpoint; empty means "use the SDK's env default"
	TraefikImage string // image used for the managed Traefik container
	Network      string // shared Docker network app containers + Traefik join

	ACMEEmail     string        // Let's Encrypt account email; empty disables TLS
	ACMEStaging   bool          // use the LE staging CA (avoid rate limits)
	HTTPSPort     string        // host port for the websecure entrypoint
	HealthTimeout time.Duration // deploy health-check deadline
	ImageKeep     int           // built images kept per app; 0 disables pruning

	PublicURL string // externally reachable base URL of the admin UI (for GitHub callbacks/webhooks); empty disables GitHub App setup
}

// Getenv matches os.Getenv; injected so Load is testable without touching the
// process environment.
type Getenv func(string) string

// Load resolves configuration from defaults overlaid with OUTHAUL_* env vars.
func Load(getenv Getenv) Config {
	return Config{
		DataDir:      or(getenv("OUTHAUL_DATA_DIR"), "/var/lib/outhaul"),
		ListenAddr:   or(getenv("OUTHAUL_LISTEN_ADDR"), ":8080"),
		DockerHost:   getenv("OUTHAUL_DOCKER_HOST"), // empty is a valid value: defer to SDK
		TraefikImage: or(getenv("OUTHAUL_TRAEFIK_IMAGE"), DefaultTraefikImage),
		Network:      or(getenv("OUTHAUL_NETWORK"), "outhaul"),

		ACMEEmail:     getenv("OUTHAUL_ACME_EMAIL"),
		ACMEStaging:   truthy(getenv("OUTHAUL_ACME_STAGING")),
		HTTPSPort:     or(getenv("OUTHAUL_HTTPS_PORT"), "443"),
		HealthTimeout: durationOr(getenv("OUTHAUL_HEALTH_TIMEOUT"), 60*time.Second),
		ImageKeep:     intOr(getenv("OUTHAUL_IMAGE_KEEP"), 5),

		PublicURL: getenv("OUTHAUL_PUBLIC_URL"),
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

// DatabasesDir is the root under which each managed database gets a
// per-database data directory, bind-mounted into its container.
func (c Config) DatabasesDir() string { return filepath.Join(c.DataDir, "databases") }

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
