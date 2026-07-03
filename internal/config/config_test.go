package config

import (
	"path/filepath"
	"testing"
	"time"
)

func TestLoadDefaults(t *testing.T) {
	// With no env set, Load returns the documented defaults.
	get := func(string) string { return "" }
	c := Load(get)

	if c.DataDir != "/var/lib/slipway" {
		t.Errorf("DataDir = %q, want /var/lib/slipway", c.DataDir)
	}
	if c.ListenAddr != ":8080" {
		t.Errorf("ListenAddr = %q, want :8080", c.ListenAddr)
	}
	if c.DockerHost != "" {
		t.Errorf("DockerHost = %q, want empty (SDK default from env)", c.DockerHost)
	}
	if c.TraefikImage == "" {
		t.Error("TraefikImage should have a non-empty default")
	}
	if c.Network != "slipway" {
		t.Errorf("Network = %q, want slipway", c.Network)
	}
}

func TestLoadEnvOverrides(t *testing.T) {
	env := map[string]string{
		"SLIPWAY_DATA_DIR":      "/data/slip",
		"SLIPWAY_LISTEN_ADDR":   ":9000",
		"SLIPWAY_DOCKER_HOST":   "tcp://10.0.0.1:2375",
		"SLIPWAY_TRAEFIK_IMAGE": "traefik:v9.9",
		"SLIPWAY_NETWORK":       "mynet",
	}
	c := Load(func(k string) string { return env[k] })

	if c.DataDir != "/data/slip" {
		t.Errorf("DataDir = %q", c.DataDir)
	}
	if c.ListenAddr != ":9000" {
		t.Errorf("ListenAddr = %q", c.ListenAddr)
	}
	if c.DockerHost != "tcp://10.0.0.1:2375" {
		t.Errorf("DockerHost = %q", c.DockerHost)
	}
	if c.TraefikImage != "traefik:v9.9" {
		t.Errorf("TraefikImage = %q", c.TraefikImage)
	}
	if c.Network != "mynet" {
		t.Errorf("Network = %q", c.Network)
	}
}

func TestDBPathDerivesFromDataDir(t *testing.T) {
	c := Load(func(k string) string {
		if k == "SLIPWAY_DATA_DIR" {
			return "/data/slip"
		}
		return ""
	})
	want := filepath.Join("/data/slip", "slipway.db")
	if got := c.DBPath(); got != want {
		t.Errorf("DBPath() = %q, want %q", got, want)
	}
}

func TestLoadTLSAndHealthDefaults(t *testing.T) {
	c := Load(func(string) string { return "" })
	if c.TLSEnabled() {
		t.Error("TLS should be disabled with no ACME email")
	}
	if c.HTTPSPort != "443" {
		t.Errorf("HTTPSPort default = %q, want 443", c.HTTPSPort)
	}
	if c.HealthTimeout != 60*time.Second {
		t.Errorf("HealthTimeout default = %v, want 60s", c.HealthTimeout)
	}
}

func TestLoadTLSEnabledWhenEmailSet(t *testing.T) {
	env := map[string]string{
		"SLIPWAY_ACME_EMAIL":     "ops@example.com",
		"SLIPWAY_ACME_STAGING":   "true",
		"SLIPWAY_HEALTH_TIMEOUT": "90s",
	}
	c := Load(func(k string) string { return env[k] })
	if !c.TLSEnabled() {
		t.Error("TLS should be enabled when ACME email is set")
	}
	if !c.ACMEStaging {
		t.Error("ACMEStaging should be true")
	}
	if c.HealthTimeout != 90*time.Second {
		t.Errorf("HealthTimeout = %v, want 90s", c.HealthTimeout)
	}
}
