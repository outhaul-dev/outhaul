package core

import "testing"

func TestPreviewHost(t *testing.T) {
	cfg := PreviewConfig{} // no base domain -> sslip.io
	got := PreviewHost(cfg, "web", 42, "", "1.2.3.4")
	if got != "web-pr-42.1.2.3.4.sslip.io" {
		t.Errorf("sslip host = %q", got)
	}
	cfg.BaseDomain = "preview.example.com"
	got = PreviewHost(cfg, "web", 42, "", "1.2.3.4")
	if got != "pr-42.preview.example.com" {
		t.Errorf("wildcard host = %q", got)
	}
	got = PreviewHost(cfg, "web", 42, "api", "1.2.3.4")
	if got != "api-pr-42.preview.example.com" {
		t.Errorf("wildcard service host = %q", got)
	}
}

func TestPreviewAppName(t *testing.T) {
	if got := PreviewAppName("web", 42); got != "web-pr-42" {
		t.Errorf("PreviewAppName = %q", got)
	}
}
