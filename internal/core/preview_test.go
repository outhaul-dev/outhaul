package core

import "testing"

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
