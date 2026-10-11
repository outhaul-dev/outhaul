package core

import (
	"strings"
	"testing"
)

func TestPreviewHost(t *testing.T) {
	cfg := PreviewConfig{} // no base domain -> sslip.io
	got := PreviewHost(cfg, "web", 42, "", "1.2.3.4")
	if got != "web-pr-42.1.2.3.4.sslip.io" {
		t.Errorf("sslip host = %q", got)
	}
	got = PreviewHost(cfg, "web", 42, "api", "1.2.3.4")
	if got != "api-web-pr-42.1.2.3.4.sslip.io" {
		t.Errorf("sslip service host = %q", got)
	}
	// The app name is part of the host under a base domain too: two apps sharing
	// one wildcard base must not both claim pr-42.<base>.
	cfg.BaseDomain = "preview.example.com"
	got = PreviewHost(cfg, "web", 42, "", "1.2.3.4")
	if got != "web-pr-42.preview.example.com" {
		t.Errorf("wildcard host = %q", got)
	}
	got = PreviewHost(cfg, "web", 42, "api", "1.2.3.4")
	if got != "api-web-pr-42.preview.example.com" {
		t.Errorf("wildcard service host = %q", got)
	}
	if a, b := PreviewHost(cfg, "web", 1, "", ""), PreviewHost(cfg, "shop", 1, "", ""); a == b {
		t.Errorf("apps web and shop share preview host %q", a)
	}
}

func TestPreviewAppName(t *testing.T) {
	if got := PreviewAppName("web", 42); got != "web-pr-42" {
		t.Errorf("PreviewAppName = %q", got)
	}
}

func TestDefaultPreviewConfig(t *testing.T) {
	cfg := DefaultPreviewConfig(7)
	if cfg.AppID != 7 {
		t.Errorf("AppID = %d", cfg.AppID)
	}
	if cfg.Enabled {
		t.Error("Enabled should be false")
	}
	if !cfg.PostPRComment {
		t.Error("PostPRComment should be true")
	}
	if cfg.AllowForkPRs {
		t.Error("AllowForkPRs should be false")
	}
	if cfg.IdleTTLDays != 7 {
		t.Errorf("IdleTTLDays = %d", cfg.IdleTTLDays)
	}
	if cfg.MaxConcurrent != 5 {
		t.Errorf("MaxConcurrent = %d", cfg.MaxConcurrent)
	}
	if cfg.BaseDomain != "" {
		t.Errorf("BaseDomain = %q", cfg.BaseDomain)
	}
}

// Compose service names may hold "_" and "." (and capitals), none of which are
// valid inside a single DNS label.
func TestPreviewHostSanitizesServiceName(t *testing.T) {
	got := PreviewHost(PreviewConfig{BaseDomain: "preview.example.com"}, "web", 3, "My_API.v2", "")
	if got != "my-api-v2-web-pr-3.preview.example.com" {
		t.Errorf("host = %q", got)
	}
}

// A DNS label is at most 63 characters. Long app + service names are cut and
// suffixed with a hash of the full label, so they stay valid and distinct.
func TestPreviewHostCapsLabelLength(t *testing.T) {
	cfg := PreviewConfig{BaseDomain: "preview.example.com"}
	app := "a-very-long-application-name-for-testing" // 40 chars, the maximum
	a := PreviewHost(cfg, app, 12345, "background-worker-service-one", "")
	b := PreviewHost(cfg, app, 12345, "background-worker-service-two", "")
	for _, h := range []string{a, b} {
		label := strings.SplitN(h, ".", 2)[0]
		if len(label) > 63 {
			t.Errorf("label %q is %d chars, want <= 63", label, len(label))
		}
		if !strings.HasSuffix(h, ".preview.example.com") {
			t.Errorf("host %q lost its base domain", h)
		}
	}
	if a == b {
		t.Errorf("distinct services share host %q", a)
	}
	if again := PreviewHost(cfg, app, 12345, "background-worker-service-one", ""); again != a {
		t.Errorf("host not deterministic: %q then %q", a, again)
	}
}
