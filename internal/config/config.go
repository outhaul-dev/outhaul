// Package config resolves Slipway's runtime configuration. There are no config
// files (a locked decision): defaults plus SLIPWAY_* environment overrides,
// all rooted at a single data directory.
package config

import "path/filepath"

// DefaultTraefikImage is the Traefik image Slipway pulls and manages. Pinned to
// a major version so upgrades are deliberate.
const DefaultTraefikImage = "traefik:v3.3"

// Config holds resolved runtime settings.
type Config struct {
	DataDir      string // root data dir; holds the SQLite DB and work dirs
	ListenAddr   string // address the admin UI/API listens on
	DockerHost   string // Docker endpoint; empty means "use the SDK's env default"
	TraefikImage string // image used for the managed Traefik container
	Network      string // shared Docker network app containers + Traefik join
}

// Getenv matches os.Getenv; injected so Load is testable without touching the
// process environment.
type Getenv func(string) string

// Load resolves configuration from defaults overlaid with SLIPWAY_* env vars.
func Load(getenv Getenv) Config {
	return Config{
		DataDir:      or(getenv("SLIPWAY_DATA_DIR"), "/var/lib/slipway"),
		ListenAddr:   or(getenv("SLIPWAY_LISTEN_ADDR"), ":8080"),
		DockerHost:   getenv("SLIPWAY_DOCKER_HOST"), // empty is a valid value: defer to SDK
		TraefikImage: or(getenv("SLIPWAY_TRAEFIK_IMAGE"), DefaultTraefikImage),
		Network:      or(getenv("SLIPWAY_NETWORK"), "slipway"),
	}
}

// DBPath is the SQLite database file path, derived from DataDir.
func (c Config) DBPath() string {
	return filepath.Join(c.DataDir, "slipway.db")
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
