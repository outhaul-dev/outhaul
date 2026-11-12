package core

import "testing"

func TestAppHost(t *testing.T) {
	ip := Naming{ServerIP: "203.0.113.7"}
	if h, ok := ip.AppHost("web"); !ok || h != "web.203.0.113.7.sslip.io" {
		t.Errorf("AppHost with an IP = %q, %v", h, ok)
	}
	if h, ok := (Naming{}).AppHost("web"); ok || h != "" {
		t.Errorf("AppHost with neither = %q, %v; want no host", h, ok)
	}
	shared := Naming{ServerIP: "203.0.113.7", Suffix: "shared.demo.example.com"}
	if h, ok := shared.AppHost("web"); !ok || h != "web.shared.demo.example.com" {
		t.Errorf("AppHost under a suffix = %q, %v; the suffix wins over the IP", h, ok)
	}
}

func TestHostUnderSuffix(t *testing.T) {
	if got := (Naming{}).HostUnderSuffix("web"); got != "" {
		t.Errorf("no suffix = %q, want empty", got)
	}
	if got := (Naming{Suffix: "shared.demo.example.com"}).HostUnderSuffix("web-api"); got != "web-api.shared.demo.example.com" {
		t.Errorf("HostUnderSuffix = %q", got)
	}
}
