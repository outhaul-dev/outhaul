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

	if c.DataDir != "/var/lib/outhaul" {
		t.Errorf("DataDir = %q, want /var/lib/outhaul", c.DataDir)
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
	if c.Network != "outhaul" {
		t.Errorf("Network = %q, want outhaul", c.Network)
	}
	if c.ImageKeep != 5 {
		t.Errorf("ImageKeep = %d, want 5", c.ImageKeep)
	}
}

func TestLoadImageKeep(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 5},     // default
		{"0", 0},    // explicit disable
		{"12", 12},  // override
		{"-3", 5},   // negative falls back
		{"lots", 5}, // garbage falls back
	}
	for _, tc := range cases {
		c := Load(func(k string) string {
			if k == "OUTHAUL_IMAGE_KEEP" {
				return tc.in
			}
			return ""
		})
		if c.ImageKeep != tc.want {
			t.Errorf("OUTHAUL_IMAGE_KEEP=%q -> %d, want %d", tc.in, c.ImageKeep, tc.want)
		}
	}
}

func TestLoadEnvOverrides(t *testing.T) {
	env := map[string]string{
		"OUTHAUL_DATA_DIR":      "/data/slip",
		"OUTHAUL_LISTEN_ADDR":   ":9000",
		"OUTHAUL_DOCKER_HOST":   "tcp://10.0.0.1:2375",
		"OUTHAUL_TRAEFIK_IMAGE": "traefik:v9.9",
		"OUTHAUL_NETWORK":       "mynet",
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
		if k == "OUTHAUL_DATA_DIR" {
			return "/data/slip"
		}
		return ""
	})
	want := filepath.Join("/data/slip", "outhaul.db")
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
		"OUTHAUL_ACME_EMAIL":     "ops@example.com",
		"OUTHAUL_ACME_STAGING":   "true",
		"OUTHAUL_HEALTH_TIMEOUT": "90s",
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

func TestPublicURL(t *testing.T) {
	c := Load(func(k string) string {
		if k == "OUTHAUL_PUBLIC_URL" {
			return "https://slip.example.com"
		}
		return ""
	})
	if c.PublicURL != "https://slip.example.com" {
		t.Errorf("PublicURL = %q", c.PublicURL)
	}
	if !c.PublicURLSet() {
		t.Error("PublicURLSet() = false, want true")
	}

	empty := Load(func(string) string { return "" })
	if empty.PublicURLSet() {
		t.Error("PublicURLSet() = true for empty, want false")
	}
}

func TestAdminHostAndPort(t *testing.T) {
	c := Load(func(k string) string {
		switch k {
		case "OUTHAUL_PUBLIC_URL":
			return "https://outhaul.example.com"
		case "OUTHAUL_LISTEN_ADDR":
			return ":9090"
		}
		return ""
	})
	if got := c.AdminHost(); got != "outhaul.example.com" {
		t.Errorf("AdminHost() = %q, want outhaul.example.com", got)
	}
	if got := c.AdminPort(); got != "9090" {
		t.Errorf("AdminPort() = %q, want 9090", got)
	}

	def := Load(func(string) string { return "" })
	if got := def.AdminHost(); got != "" {
		t.Errorf("AdminHost() = %q, want empty when PublicURL unset", got)
	}
	if got := def.AdminPort(); got != "8080" {
		t.Errorf("AdminPort() = %q, want default 8080", got)
	}
}

func TestCloudflaredImageDefaultAndOverride(t *testing.T) {
	def := Load(func(string) string { return "" })
	if def.CloudflaredImage != DefaultCloudflaredImage {
		t.Errorf("default CloudflaredImage = %q, want %q", def.CloudflaredImage, DefaultCloudflaredImage)
	}
	over := Load(func(k string) string {
		if k == "OUTHAUL_CLOUDFLARED_IMAGE" {
			return "cloudflare/cloudflared:pinned"
		}
		return ""
	})
	if over.CloudflaredImage != "cloudflare/cloudflared:pinned" {
		t.Errorf("override CloudflaredImage = %q", over.CloudflaredImage)
	}
}

// A leaked inline comment in /etc/outhaul.env (systemd keeps it as part of the
// value) must not corrupt the ACME email — otherwise TLS silently never issues.
func TestACMEEmailStripsInlineComment(t *testing.T) {
	c := Load(func(k string) string {
		if k == "OUTHAUL_ACME_EMAIL" {
			return "ops@example.com     # set to enable automatic HTTPS"
		}
		return ""
	})
	if c.ACMEEmail != "ops@example.com" {
		t.Errorf("ACMEEmail = %q, want ops@example.com", c.ACMEEmail)
	}
	if !c.TLSEnabled() {
		t.Error("TLSEnabled() = false, want true")
	}
}

func TestLocalCAConfig(t *testing.T) {
	env := map[string]string{"OUTHAUL_LOCAL_CA": "true"}
	cfg := Load(func(k string) string { return env[k] })
	if !cfg.LocalCAEnabled() || cfg.ACMEEnabled() {
		t.Errorf("LocalCAEnabled=%v ACMEEnabled=%v; want true,false", cfg.LocalCAEnabled(), cfg.ACMEEnabled())
	}
	if !cfg.TLSEnabled() {
		t.Error("TLSEnabled should be true with local CA on")
	}
	if got := cfg.CertResolver(); got != "" {
		t.Errorf("CertResolver = %q; want empty for local CA", got)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

func TestACMECertResolver(t *testing.T) {
	env := map[string]string{"OUTHAUL_ACME_EMAIL": "ops@example.com"}
	cfg := Load(func(k string) string { return env[k] })
	if got := cfg.CertResolver(); got != "le" {
		t.Errorf("CertResolver = %q; want le", got)
	}
}

func TestValidateRejectsLocalCAPlusACME(t *testing.T) {
	env := map[string]string{"OUTHAUL_LOCAL_CA": "1", "OUTHAUL_ACME_EMAIL": "ops@example.com"}
	cfg := Load(func(k string) string { return env[k] })
	if err := cfg.Validate(); err == nil {
		t.Error("Validate should reject LOCAL_CA together with ACME_EMAIL")
	}
}

func TestCADirs(t *testing.T) {
	cfg := Load(func(string) string { return "" })
	if got, want := cfg.CADir(), filepath.Join(cfg.DataDir, "ca"); got != want {
		t.Errorf("CADir = %q; want %q", got, want)
	}
	if got, want := cfg.CertsDir(), filepath.Join(cfg.DataDir, "traefik", "certs"); got != want {
		t.Errorf("CertsDir = %q; want %q", got, want)
	}
}
