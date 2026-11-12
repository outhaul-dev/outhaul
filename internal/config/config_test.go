package config

import (
	"path/filepath"
	"strings"
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

// Traefik reaches the gate on the host through the host gateway, on the port
// the admin UI listens on.
func TestGateAddress(t *testing.T) {
	c := Load(func(k string) string {
		if k == "OUTHAUL_LISTEN_ADDR" {
			return ":9000"
		}
		return ""
	})
	if got, want := c.GateAddress(), "http://host.docker.internal:9000/gate/check"; got != want {
		t.Errorf("GateAddress() = %q, want %q", got, want)
	}
	def := Load(func(string) string { return "" })
	if got, want := def.GateAddress(), "http://host.docker.internal:8080/gate/check"; got != want {
		t.Errorf("default GateAddress() = %q, want %q", got, want)
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

// Bad values used to fall back to defaults silently: OUTHAUL_HEALTH_TIMEOUT=120
// (no unit) meant 60s, "0" failed every deploy at once, a typo'd boolean read
// as off. Load still falls back (so `outhaul ca root` works), but Validate,
// which serve runs first, names each bad variable.
func TestValidateRejectsMalformedValues(t *testing.T) {
	cases := []struct{ key, val string }{
		{"OUTHAUL_HEALTH_TIMEOUT", "120"},
		{"OUTHAUL_HEALTH_TIMEOUT", "0"},
		{"OUTHAUL_HEALTH_TIMEOUT", "-5s"},
		{"OUTHAUL_IMAGE_KEEP", "lots"},
		{"OUTHAUL_IMAGE_KEEP", "-3"},
		{"OUTHAUL_ACME_STAGING", "maybe"},
		{"OUTHAUL_PRUNE_BUILD_CACHE", "maybe"},
		{"OUTHAUL_LOCAL_CA", "enabled"},
		{"OUTHAUL_HTTPS_PORT", "https"},
		{"OUTHAUL_HTTPS_PORT", "70000"},
		{"OUTHAUL_LISTEN_ADDR", "8080"},
		{"OUTHAUL_SSH_ADDR", "2222"},
		{"OUTHAUL_LISTEN_ADDR", "off"},
		{"OUTHAUL_PUBLIC_URL", "paas.example.com"},
		{"OUTHAUL_PUBLIC_URL", "ftp://paas.example.com"},
		{"OUTHAUL_SERVER_IP", "my-server"},
	}
	for _, tc := range cases {
		cfg := Load(func(k string) string {
			if k == tc.key {
				return tc.val
			}
			return ""
		})
		err := cfg.Validate()
		if err == nil {
			t.Errorf("%s=%q: Validate accepted it", tc.key, tc.val)
			continue
		}
		if !strings.Contains(err.Error(), tc.key) {
			t.Errorf("%s=%q: error %q does not name the variable", tc.key, tc.val, err)
		}
	}
}

func TestSSHAddrOff(t *testing.T) {
	for _, v := range []string{"off", "OFF", "Off"} {
		c := Load(func(k string) string {
			if k == "OUTHAUL_SSH_ADDR" {
				return v
			}
			return ""
		})
		if !c.SSHOff() {
			t.Errorf("OUTHAUL_SSH_ADDR=%q: SSHOff() = false", v)
		}
		if err := c.Validate(); err != nil {
			t.Errorf("OUTHAUL_SSH_ADDR=%q: Validate: %v", v, err)
		}
	}
	c := Load(func(k string) string {
		if k == "OUTHAUL_SSH_ADDR" {
			return ":2200"
		}
		return ""
	})
	if c.SSHOff() {
		t.Error(":2200 read as off")
	}
}

func TestUpdatesSetting(t *testing.T) {
	for _, tc := range []struct {
		in, want string
		off      bool
	}{
		{"", "main", false}, {"main", "main", false}, {"MAIN", "main", false},
		{"off", "off", true}, {"OFF", "off", true}, {" off ", "off", true},
	} {
		c := Load(func(k string) string {
			if k == "OUTHAUL_UPDATES" {
				return tc.in
			}
			return ""
		})
		if c.Updates != tc.want || c.UpdatesOff() != tc.off {
			t.Errorf("OUTHAUL_UPDATES=%q -> %q/%v, want %q/%v", tc.in, c.Updates, c.UpdatesOff(), tc.want, tc.off)
		}
	}
	for _, v := range []string{"weekly", "releases"} {
		c := Load(func(k string) string {
			if k == "OUTHAUL_UPDATES" {
				return v
			}
			return ""
		})
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "want main or off") {
			t.Errorf("OUTHAUL_UPDATES=%q: Validate = %v, want 'want main or off'", v, err)
		}
	}
}

func TestValidateAcceptsWellFormedValues(t *testing.T) {
	env := map[string]string{
		"OUTHAUL_HEALTH_TIMEOUT": "2m",
		"OUTHAUL_IMAGE_KEEP":     "0",
		"OUTHAUL_ACME_STAGING":   "Yes",
		"OUTHAUL_LOCAL_CA":       "off",
		"OUTHAUL_HTTPS_PORT":     "8443",
		"OUTHAUL_LISTEN_ADDR":    "127.0.0.1:9090",
		"OUTHAUL_SSH_ADDR":       ":2222",
		"OUTHAUL_PUBLIC_URL":     "https://paas.example.com",
		"OUTHAUL_SERVER_IP":      "203.0.113.7",
	}
	cfg := Load(func(k string) string { return env[k] })
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if !cfg.ACMEStaging || cfg.LocalCA {
		t.Errorf("ACMEStaging=%v LocalCA=%v, want true,false", cfg.ACMEStaging, cfg.LocalCA)
	}
	if err := Load(func(string) string { return "" }).Validate(); err != nil {
		t.Errorf("defaults: Validate: %v", err)
	}
}

func TestAzureDestinationSettings(t *testing.T) {
	pair := func(account, container string) map[string]string {
		m := map[string]string{}
		if account != "" {
			m["OUTHAUL_BACKUP_AZURE_ACCOUNT"] = account
		}
		if container != "" {
			m["OUTHAUL_BACKUP_AZURE_CONTAINER"] = container
		}
		return m
	}
	cases := []struct {
		name    string
		env     map[string]string
		wantVar string
	}{
		{"valid", pair("examplebackups", "outhaul"), ""},
		{"account upper", pair("Example", "outhaul"), "OUTHAUL_BACKUP_AZURE_ACCOUNT"},
		{"account short", pair("ab", "outhaul"), "OUTHAUL_BACKUP_AZURE_ACCOUNT"},
		{"account underscore", pair("a_b", "outhaul"), "OUTHAUL_BACKUP_AZURE_ACCOUNT"},
		{"container space", pair("examplebackups", "out haul"), "OUTHAUL_BACKUP_AZURE_CONTAINER"},
		{"container leading hyphen", pair("examplebackups", "-outhaul"), "OUTHAUL_BACKUP_AZURE_CONTAINER"},
		{"container trailing hyphen", pair("examplebackups", "outhaul-"), "OUTHAUL_BACKUP_AZURE_CONTAINER"},
		{"container double hyphen", pair("examplebackups", "a--b"), "OUTHAUL_BACKUP_AZURE_CONTAINER"},
		{"container short", pair("examplebackups", "ab"), "OUTHAUL_BACKUP_AZURE_CONTAINER"},
		{"account only", pair("examplebackups", ""), "OUTHAUL_BACKUP_AZURE_CONTAINER"},
		{"container only", pair("", "outhaul"), "OUTHAUL_BACKUP_AZURE_ACCOUNT"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := Load(func(k string) string { return tc.env[k] })
			err := c.Validate()
			if tc.wantVar == "" {
				if err != nil {
					t.Fatalf("Validate = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantVar) {
				t.Fatalf("Validate = %v, want an error naming %s", err, tc.wantVar)
			}
		})
	}
}

func TestAzureClientIDSetting(t *testing.T) {
	env := func(account, container, clientID string) map[string]string {
		m := map[string]string{
			"OUTHAUL_BACKUP_AZURE_ACCOUNT":   account,
			"OUTHAUL_BACKUP_AZURE_CONTAINER": container,
			"AZURE_CLIENT_ID":                clientID,
		}
		return m
	}
	c := Load(func(k string) string {
		return env("examplebackups", "outhaul", "8f7c1a2e-0000-4000-8000-000000000000")[k]
	})
	if c.BackupAzureAccount != "examplebackups" || c.BackupAzureContainer != "outhaul" || c.AzureClientID == "" {
		t.Fatalf("Load = %+v", c)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate with a GUID = %v, want nil", err)
	}
	c = Load(func(k string) string { return env("examplebackups", "outhaul", "not-a-guid")[k] })
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "AZURE_CLIENT_ID") {
		t.Fatalf("Validate with a bad client ID = %v, want an error naming AZURE_CLIENT_ID", err)
	}
	c = Load(func(k string) string { return env("", "", "not-a-guid")[k] })
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate without the pair = %v, want the client ID ignored", err)
	}
}

func TestStateBackupSettings(t *testing.T) {
	cases := []struct {
		name     string
		withPair bool
		env      map[string]string
		wantVar  string
		wantKeep int
	}{
		{"azure pair", true, map[string]string{"OUTHAUL_STATE_BACKUP_SCHEDULE": "15 2 * * *"}, "", 30},
		{"destination", false, map[string]string{
			"OUTHAUL_STATE_BACKUP_SCHEDULE": "15 2 * * *", "OUTHAUL_STATE_BACKUP_DESTINATION": "minio",
		}, "", 30},
		{"schedule alone", false, map[string]string{"OUTHAUL_STATE_BACKUP_SCHEDULE": "15 2 * * *"}, "OUTHAUL_STATE_BACKUP_SCHEDULE", 30},
		{"bad schedule", false, map[string]string{
			"OUTHAUL_STATE_BACKUP_SCHEDULE": "15 2 * *", "OUTHAUL_STATE_BACKUP_DESTINATION": "minio",
		}, "OUTHAUL_STATE_BACKUP_SCHEDULE", 30},
		{"destination without schedule", false, map[string]string{"OUTHAUL_STATE_BACKUP_DESTINATION": "minio"}, "OUTHAUL_STATE_BACKUP_DESTINATION", 30},
		{"prefix without schedule", false, map[string]string{"OUTHAUL_STATE_BACKUP_PREFIX": "prod/state"}, "OUTHAUL_STATE_BACKUP_PREFIX", 30},
		{"keep without schedule", false, map[string]string{"OUTHAUL_STATE_BACKUP_KEEP": "7"}, "OUTHAUL_STATE_BACKUP_KEEP", 7},
		{"prefix valid", false, map[string]string{
			"OUTHAUL_STATE_BACKUP_SCHEDULE": "15 2 * * *", "OUTHAUL_STATE_BACKUP_DESTINATION": "minio",
			"OUTHAUL_STATE_BACKUP_PREFIX": "prod/state",
		}, "", 30},
		{"prefix double slash", false, map[string]string{
			"OUTHAUL_STATE_BACKUP_SCHEDULE": "15 2 * * *", "OUTHAUL_STATE_BACKUP_DESTINATION": "minio",
			"OUTHAUL_STATE_BACKUP_PREFIX": "prod//state",
		}, "OUTHAUL_STATE_BACKUP_PREFIX", 30},
		{"prefix leading slash", false, map[string]string{
			"OUTHAUL_STATE_BACKUP_SCHEDULE": "15 2 * * *", "OUTHAUL_STATE_BACKUP_DESTINATION": "minio",
			"OUTHAUL_STATE_BACKUP_PREFIX": "/prod",
		}, "OUTHAUL_STATE_BACKUP_PREFIX", 30},
		{"prefix trailing slash", false, map[string]string{
			"OUTHAUL_STATE_BACKUP_SCHEDULE": "15 2 * * *", "OUTHAUL_STATE_BACKUP_DESTINATION": "minio",
			"OUTHAUL_STATE_BACKUP_PREFIX": "prod/",
		}, "OUTHAUL_STATE_BACKUP_PREFIX", 30},
		{"keep negative", false, map[string]string{
			"OUTHAUL_STATE_BACKUP_SCHEDULE": "15 2 * * *", "OUTHAUL_STATE_BACKUP_DESTINATION": "minio",
			"OUTHAUL_STATE_BACKUP_KEEP": "-1",
		}, "OUTHAUL_STATE_BACKUP_KEEP", 30},
		{"keep garbage", false, map[string]string{
			"OUTHAUL_STATE_BACKUP_SCHEDULE": "15 2 * * *", "OUTHAUL_STATE_BACKUP_DESTINATION": "minio",
			"OUTHAUL_STATE_BACKUP_KEEP": "many",
		}, "OUTHAUL_STATE_BACKUP_KEEP", 30},
		{"keep unset", false, map[string]string{
			"OUTHAUL_STATE_BACKUP_SCHEDULE": "15 2 * * *", "OUTHAUL_STATE_BACKUP_DESTINATION": "minio",
		}, "", 30},
		{"keep zero", false, map[string]string{
			"OUTHAUL_STATE_BACKUP_SCHEDULE": "15 2 * * *", "OUTHAUL_STATE_BACKUP_DESTINATION": "minio",
			"OUTHAUL_STATE_BACKUP_KEEP": "0",
		}, "", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := map[string]string{}
			if tc.withPair {
				env["OUTHAUL_BACKUP_AZURE_ACCOUNT"] = "examplebackups"
				env["OUTHAUL_BACKUP_AZURE_CONTAINER"] = "outhaul"
			}
			for k, v := range tc.env {
				env[k] = v
			}
			c := Load(func(k string) string { return env[k] })
			err := c.Validate()
			if tc.wantVar == "" {
				if err != nil {
					t.Fatalf("Validate = %v, want nil", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.wantVar) {
				t.Fatalf("Validate = %v, want an error naming %s", err, tc.wantVar)
			}
			if c.StateBackupKeep != tc.wantKeep {
				t.Errorf("StateBackupKeep = %d, want %d", c.StateBackupKeep, tc.wantKeep)
			}
		})
	}
}

func TestPruneBuildCacheDefaultsOn(t *testing.T) {
	cfg := Load(func(string) string { return "" })
	if !cfg.PruneBuildCache {
		t.Error("PruneBuildCache should default to true")
	}
	cfg = Load(func(k string) string {
		if k == "OUTHAUL_PRUNE_BUILD_CACHE" {
			return "No"
		}
		return ""
	})
	if cfg.PruneBuildCache {
		t.Error("OUTHAUL_PRUNE_BUILD_CACHE=No should turn the build-cache prune off")
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

func TestHTTPPortSetting(t *testing.T) {
	cases := []struct {
		val     string
		localCA bool
		wantOff bool
		wantErr bool
	}{
		{"", false, false, false},
		{"80", false, false, false},
		{"off", true, true, false},
		{"OFF", true, true, false},
		{"8080", false, false, true},
		{"on", false, false, true},
		{"no", false, false, true},
	}
	for _, tc := range cases {
		cfg := Load(func(k string) string {
			switch k {
			case "OUTHAUL_HTTP_PORT":
				return tc.val
			case "OUTHAUL_LOCAL_CA":
				if tc.localCA {
					return "true"
				}
			}
			return ""
		})
		if cfg.HTTPOff != tc.wantOff {
			t.Errorf("OUTHAUL_HTTP_PORT=%q -> HTTPOff %v, want %v", tc.val, cfg.HTTPOff, tc.wantOff)
		}
		err := cfg.Validate()
		if tc.wantErr {
			if err == nil {
				t.Errorf("OUTHAUL_HTTP_PORT=%q: Validate accepted it", tc.val)
				continue
			}
			for _, want := range []string{"OUTHAUL_HTTP_PORT", "80 or off"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("OUTHAUL_HTTP_PORT=%q: error %q does not contain %q", tc.val, err, want)
				}
			}
		} else if err != nil {
			t.Errorf("OUTHAUL_HTTP_PORT=%q: Validate: %v", tc.val, err)
		}
	}
}

func TestHTTPOffNeedsHTTPS(t *testing.T) {
	off := Load(func(k string) string {
		if k == "OUTHAUL_HTTP_PORT" {
			return "off"
		}
		return ""
	})
	err := off.Validate()
	if err == nil {
		t.Fatal("HTTP off without HTTPS: Validate accepted it")
	}
	for _, want := range []string{"OUTHAUL_HTTP_PORT", "HTTPS"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}

	acme := Load(func(k string) string {
		switch k {
		case "OUTHAUL_HTTP_PORT":
			return "off"
		case "OUTHAUL_ACME_EMAIL":
			return "ops@example.com"
		}
		return ""
	})
	err = acme.Validate()
	if err == nil {
		t.Fatal("HTTP off with the HTTP challenge: Validate accepted it")
	}
	for _, want := range []string{"OUTHAUL_HTTP_PORT", "port 80"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
}

func TestDNSProviderSetting(t *testing.T) {
	ok := Load(func(k string) string {
		switch k {
		case "OUTHAUL_ACME_DNS_PROVIDER":
			return "azuredns"
		case "OUTHAUL_ACME_EMAIL":
			return "ops@example.com"
		}
		return ""
	})
	if ok.ACMEDNSProvider != "azuredns" {
		t.Errorf("ACMEDNSProvider = %q, want azuredns", ok.ACMEDNSProvider)
	}
	if err := ok.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}

	noEmail := Load(func(k string) string {
		if k == "OUTHAUL_ACME_DNS_PROVIDER" {
			return "azuredns"
		}
		return ""
	})
	err := noEmail.Validate()
	if err == nil {
		t.Fatal("a DNS provider without an ACME email: Validate accepted it")
	}
	for _, want := range []string{"OUTHAUL_ACME_DNS_PROVIDER", "OUTHAUL_ACME_EMAIL"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}

	for _, v := range []string{"Azure DNS", "azure_dns", "azure;rm"} {
		cfg := Load(func(k string) string {
			switch k {
			case "OUTHAUL_ACME_DNS_PROVIDER":
				return v
			case "OUTHAUL_ACME_EMAIL":
				return "ops@example.com"
			}
			return ""
		})
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), "OUTHAUL_ACME_DNS_PROVIDER") {
			t.Errorf("OUTHAUL_ACME_DNS_PROVIDER=%q: error %v does not name the variable", v, err)
		}
	}
}

func TestDNSEnvSetting(t *testing.T) {
	cases := []struct {
		env     string
		want    []string
		wantErr []string
	}{
		{"A,B", []string{"A=1", "B=2"}, nil},
		{" A , B ", []string{"A=1", "B=2"}, nil},
	}
	for _, tc := range cases {
		cfg := Load(func(k string) string {
			switch k {
			case "OUTHAUL_ACME_DNS_PROVIDER":
				return "azuredns"
			case "OUTHAUL_ACME_EMAIL":
				return "ops@example.com"
			case "OUTHAUL_ACME_DNS_ENV":
				return tc.env
			case "A":
				return "1"
			case "B":
				return "2"
			case "EMPTY":
				return ""
			}
			return ""
		})
		if err := cfg.Validate(); err != nil {
			t.Fatalf("OUTHAUL_ACME_DNS_ENV=%q: Validate: %v", tc.env, err)
		}
		if strings.Join(cfg.ACMEDNSEnv, ",") != strings.Join(tc.want, ",") {
			t.Errorf("OUTHAUL_ACME_DNS_ENV=%q -> %v, want %v", tc.env, cfg.ACMEDNSEnv, tc.want)
		}
	}

	bad := []struct {
		env  string
		said []string
	}{
		{"A,", []string{"OUTHAUL_ACME_DNS_ENV", "empty"}},
		{"A,,B", []string{"OUTHAUL_ACME_DNS_ENV", "empty"}},
		{"A,A", []string{"OUTHAUL_ACME_DNS_ENV", "A"}},
		{"A B", []string{"OUTHAUL_ACME_DNS_ENV", "A B"}},
		{"1A", []string{"OUTHAUL_ACME_DNS_ENV", "1A"}},
		{"A,MISSING", []string{"OUTHAUL_ACME_DNS_ENV", "MISSING"}},
		{"A,EMPTY", []string{"OUTHAUL_ACME_DNS_ENV", "EMPTY"}},
	}
	for _, tc := range bad {
		cfg := Load(func(k string) string {
			switch k {
			case "OUTHAUL_ACME_DNS_PROVIDER":
				return "azuredns"
			case "OUTHAUL_ACME_EMAIL":
				return "ops@example.com"
			case "OUTHAUL_ACME_DNS_ENV":
				return tc.env
			case "A":
				return "1"
			case "B":
				return "2"
			case "EMPTY":
				return ""
			}
			return ""
		})
		err := cfg.Validate()
		if err == nil {
			t.Errorf("OUTHAUL_ACME_DNS_ENV=%q: Validate accepted it", tc.env)
			continue
		}
		for _, want := range tc.said {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("OUTHAUL_ACME_DNS_ENV=%q: error %q does not contain %q", tc.env, err, want)
			}
		}
		if strings.Contains(err.Error(), "=1") || strings.Contains(err.Error(), "=2") {
			t.Errorf("OUTHAUL_ACME_DNS_ENV=%q: error leaks a value: %q", tc.env, err)
		}
	}

	noProvider := Load(func(k string) string {
		switch k {
		case "OUTHAUL_ACME_DNS_ENV":
			return "A"
		case "A":
			return "1"
		}
		return ""
	})
	err := noProvider.Validate()
	if err == nil || !strings.Contains(err.Error(), "OUTHAUL_ACME_DNS_PROVIDER") {
		t.Errorf("env without a provider: error %v does not name OUTHAUL_ACME_DNS_PROVIDER", err)
	}
}

func TestTLSWildcardSetting(t *testing.T) {
	valid := func(v string) Config {
		return Load(func(k string) string {
			switch k {
			case "OUTHAUL_TLS_WILDCARD":
				return v
			case "OUTHAUL_ACME_DNS_PROVIDER":
				return "azuredns"
			case "OUTHAUL_ACME_EMAIL":
				return "ops@example.com"
			}
			return ""
		})
	}
	if got := valid("*.shared.demo.example.com").TLSWildcard; got != "*.shared.demo.example.com" {
		t.Errorf("TLSWildcard = %q", got)
	}
	if got := valid("*.Shared.Demo.Example.com").TLSWildcard; got != "*.shared.demo.example.com" {
		t.Errorf("TLSWildcard = %q, want it lower-cased", got)
	}
	for _, v := range []string{
		"shared.demo.example.com", "*.com", "*.*.example.com", "*.exa mple.com", "*.example.com.",
	} {
		cfg := valid(v)
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), "OUTHAUL_TLS_WILDCARD") {
			t.Errorf("OUTHAUL_TLS_WILDCARD=%q: error %v does not name the variable", v, err)
		}
	}
	noProvider := Load(func(k string) string {
		if k == "OUTHAUL_TLS_WILDCARD" {
			return "*.shared.demo.example.com"
		}
		return ""
	})
	err := noProvider.Validate()
	if err == nil || !strings.Contains(err.Error(), "OUTHAUL_ACME_DNS_PROVIDER") {
		t.Errorf("wildcard without a provider: error %v does not name OUTHAUL_ACME_DNS_PROVIDER", err)
	}
}

func TestHTTPOffWithDNSChallenge(t *testing.T) {
	cfg := Load(func(k string) string {
		switch k {
		case "OUTHAUL_HTTP_PORT":
			return "off"
		case "OUTHAUL_ACME_EMAIL":
			return "ops@example.com"
		case "OUTHAUL_ACME_DNS_PROVIDER":
			return "azuredns"
		}
		return ""
	})
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

func TestRequireHeaderSetting(t *testing.T) {
	load := func(env map[string]string) Config {
		return Load(func(k string) string { return env[k] })
	}
	ok := load(map[string]string{
		"OUTHAUL_REQUIRE_HEADER_NAME":  "X-Azure-FDID",
		"OUTHAUL_REQUIRE_HEADER_VALUE": "8f7c1a2e-0000-4000-8000-000000000000",
	})
	if err := ok.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
	if !ok.HeaderRequired() || ok.RequireHeaderName != "X-Azure-FDID" || ok.RequireHeaderValue != "8f7c1a2e-0000-4000-8000-000000000000" {
		t.Errorf("header settings = %q, %q, required %v", ok.RequireHeaderName, ok.RequireHeaderValue, ok.HeaderRequired())
	}
	if load(nil).HeaderRequired() {
		t.Error("no header set should not require one")
	}

	for env, want := range map[string]string{
		"OUTHAUL_REQUIRE_HEADER_NAME=X-Azure-FDID": "OUTHAUL_REQUIRE_HEADER_VALUE",
		"OUTHAUL_REQUIRE_HEADER_VALUE=abc-123":     "OUTHAUL_REQUIRE_HEADER_NAME",
	} {
		var e map[string]string
		for _, kv := range []string{env} {
			k, v, _ := strings.Cut(kv, "=")
			e = map[string]string{k: v}
		}
		err := load(e).Validate()
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: error %v does not name %s", env, err, want)
		}
	}

	for _, name := range []string{"X Azure", "X_Azure", "X:Azure"} {
		err := load(map[string]string{
			"OUTHAUL_REQUIRE_HEADER_NAME":  name,
			"OUTHAUL_REQUIRE_HEADER_VALUE": "abc-123",
		}).Validate()
		if err == nil || !strings.Contains(err.Error(), "OUTHAUL_REQUIRE_HEADER_NAME") {
			t.Errorf("name %q: error %v does not name OUTHAUL_REQUIRE_HEADER_NAME", name, err)
		}
	}
	err := load(map[string]string{"OUTHAUL_REQUIRE_HEADER_VALUE": "abc-123"}).Validate()
	if err == nil || !strings.Contains(err.Error(), "OUTHAUL_REQUIRE_HEADER_NAME") {
		t.Errorf("empty name with a value: error %v does not name OUTHAUL_REQUIRE_HEADER_NAME", err)
	}
	for _, value := range []string{"a`b", `a"b`, "a b", "a$b", "a\nb", `a\b`, strings.Repeat("a", 257)} {
		err := load(map[string]string{
			"OUTHAUL_REQUIRE_HEADER_NAME":  "X-Azure-FDID",
			"OUTHAUL_REQUIRE_HEADER_VALUE": value,
		}).Validate()
		if err == nil || !strings.Contains(err.Error(), "OUTHAUL_REQUIRE_HEADER_VALUE") {
			t.Errorf("value %q: error %v does not name OUTHAUL_REQUIRE_HEADER_VALUE", value, err)
		}
		if err != nil && strings.Contains(err.Error(), value) && len(value) > 3 {
			t.Errorf("value %q: error leaks the value: %q", value, err)
		}
	}
	okValue := load(map[string]string{
		"OUTHAUL_REQUIRE_HEADER_NAME":  "X-Azure-FDID",
		"OUTHAUL_REQUIRE_HEADER_VALUE": "Ab0-._~+/=:",
	})
	if err := okValue.Validate(); err != nil {
		t.Errorf("a valid value: %v", err)
	}
}

func TestAdminDirectSetting(t *testing.T) {
	load := func(env map[string]string) Config {
		return Load(func(k string) string { return env[k] })
	}
	for _, v := range []string{"", "on", "true"} {
		cfg := load(map[string]string{"OUTHAUL_ADMIN_DIRECT": v})
		if !cfg.AdminDirect {
			t.Errorf("OUTHAUL_ADMIN_DIRECT=%q -> AdminDirect false, want true", v)
		}
	}
	for _, v := range []string{"off", "OFF", "false"} {
		cfg := load(map[string]string{"OUTHAUL_ADMIN_DIRECT": v, "OUTHAUL_PUBLIC_URL": "https://outhaul.example.com", "OUTHAUL_ACME_EMAIL": "ops@example.com"})
		if cfg.AdminDirect {
			t.Errorf("OUTHAUL_ADMIN_DIRECT=%q -> AdminDirect true, want false", v)
		}
		if err := cfg.Validate(); err != nil {
			t.Errorf("OUTHAUL_ADMIN_DIRECT=%q with HTTPS and a public URL: %v", v, err)
		}
	}
	cfg := load(map[string]string{"OUTHAUL_ADMIN_DIRECT": "proxy"})
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "OUTHAUL_ADMIN_DIRECT") {
		t.Errorf("OUTHAUL_ADMIN_DIRECT=proxy: error %v does not name the variable", err)
	}

	noURL := load(map[string]string{"OUTHAUL_ADMIN_DIRECT": "off", "OUTHAUL_ACME_EMAIL": "ops@example.com"})
	err := noURL.Validate()
	if err == nil {
		t.Fatal("admin direct off without a public URL: Validate accepted it")
	}
	for _, want := range []string{"OUTHAUL_ADMIN_DIRECT", "OUTHAUL_PUBLIC_URL"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}

	withHost := load(map[string]string{
		"OUTHAUL_ADMIN_DIRECT": "off", "OUTHAUL_ACME_EMAIL": "ops@example.com",
		"OUTHAUL_PUBLIC_URL": "https://outhaul.example.com", "OUTHAUL_LISTEN_ADDR": "127.0.0.1:8080",
	})
	err = withHost.Validate()
	if err == nil || !strings.Contains(err.Error(), "OUTHAUL_LISTEN_ADDR") {
		t.Errorf("a listen host: error %v does not name OUTHAUL_LISTEN_ADDR", err)
	}
	portOnly := load(map[string]string{
		"OUTHAUL_ADMIN_DIRECT": "off", "OUTHAUL_ACME_EMAIL": "ops@example.com",
		"OUTHAUL_PUBLIC_URL": "https://outhaul.example.com", "OUTHAUL_LISTEN_ADDR": ":9090",
	})
	if err := portOnly.Validate(); err != nil {
		t.Errorf("a port-only listen address: %v", err)
	}
}

func TestLoadSignInBackoff(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"", false},
		{"on", true},
		{"off", false},
		{"TRUE", true},
		{"yes", true},
		{"no", false},
	} {
		c := Load(func(k string) string {
			if k == "OUTHAUL_SIGNIN_BACKOFF" {
				return tc.in
			}
			return ""
		})
		if c.SignInBackoff != tc.want {
			t.Errorf("OUTHAUL_SIGNIN_BACKOFF=%q -> %v, want %v", tc.in, c.SignInBackoff, tc.want)
		}
		if err := c.Validate(); err != nil {
			t.Errorf("OUTHAUL_SIGNIN_BACKOFF=%q: Validate: %v", tc.in, err)
		}
	}
}

func TestValidateRejectsMalformedSignInBackoff(t *testing.T) {
	c := Load(func(k string) string {
		if k == "OUTHAUL_SIGNIN_BACKOFF" {
			return "maybe"
		}
		return ""
	})
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "OUTHAUL_SIGNIN_BACKOFF") {
		t.Errorf("Validate = %v, want an error naming OUTHAUL_SIGNIN_BACKOFF", err)
	}
}
