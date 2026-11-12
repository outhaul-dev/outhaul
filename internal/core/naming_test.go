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

func TestNamesUnderASuffix(t *testing.T) {
	n := Naming{ServerIP: "203.0.113.7", Suffix: "shared.demo.example.com"}
	if got := n.HostUnderSuffix("web-api"); got != "web-api.shared.demo.example.com" {
		t.Errorf("HostUnderSuffix = %q", got)
	}
	if got := n.PreviewHost(PreviewConfig{}, "web", 42, ""); got != "web-pr-42.shared.demo.example.com" {
		t.Errorf("preview host = %q", got)
	}
}

func TestOneLabelUnder(t *testing.T) {
	const suffix = "shared.demo.example.com"
	cases := []struct {
		host string
		want bool
	}{
		{"web.shared.demo.example.com", true},
		{"WEB.Shared.Demo.Example.COM", true},
		{"web.shared.demo.example.com.", true},
		{"shared.demo.example.com", false},
		{"a.b.shared.demo.example.com", false},
		{"xshared.demo.example.com", false},
		{"", false},
	}
	for _, c := range cases {
		if got := OneLabelUnder(c.host, suffix); got != c.want {
			t.Errorf("OneLabelUnder(%q) = %v, want %v", c.host, got, c.want)
		}
	}
	if OneLabelUnder("web.example.com", "") {
		t.Error("an empty suffix covers nothing")
	}
}
