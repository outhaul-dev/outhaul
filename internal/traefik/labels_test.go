package traefik

import (
	"testing"

	"github.com/james-smart/outhaul/internal/core"
)

func TestLabels(t *testing.T) {
	app := core.App{Name: "web", Domain: "web.example.com"}

	got := Labels(app, 8080, false)

	want := map[string]string{
		"traefik.enable":                               "true",
		"outhaul.managed":                              "true",
		"outhaul.app":                                  "web",
		"traefik.http.routers.outhaul-web.rule":        "Host(`web.example.com`)",
		"traefik.http.routers.outhaul-web.entrypoints": "web",
		"traefik.http.services.outhaul-web.loadbalancer.server.port": "8080",
	}

	if len(got) != len(want) {
		t.Fatalf("got %d labels, want %d\n got=%v", len(got), len(want), got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("label %q = %q, want %q", k, got[k], v)
		}
	}
}

func TestLabelsRouterNameIsNamespacedPerApp(t *testing.T) {
	// Two apps must not collide on router/service names.
	a := Labels(core.App{Name: "api", Domain: "api.test"}, 3000, false)
	b := Labels(core.App{Name: "web", Domain: "web.test"}, 3000, false)

	if a["traefik.http.routers.outhaul-api.rule"] == "" {
		t.Error("api router label missing")
	}
	if b["traefik.http.routers.outhaul-web.rule"] == "" {
		t.Error("web router label missing")
	}
	// The api app must not define a web router (no cross-talk).
	if _, ok := a["traefik.http.routers.outhaul-web.rule"]; ok {
		t.Error("api labels leaked a web router")
	}
}

func TestLabelsPortRendered(t *testing.T) {
	got := Labels(core.App{Name: "svc", Domain: "svc.test"}, 5000, false)
	if got["traefik.http.services.outhaul-svc.loadbalancer.server.port"] != "5000" {
		t.Errorf("port label = %q", got["traefik.http.services.outhaul-svc.loadbalancer.server.port"])
	}
}

func TestLabelsTLSAddsWebsecureRouter(t *testing.T) {
	got := Labels(core.App{Name: "web", Domain: "web.example.com"}, 8080, true)

	if got["traefik.http.routers.outhaul-web-tls.entrypoints"] != "websecure" {
		t.Errorf("missing websecure entrypoint: %v", got)
	}
	if got["traefik.http.routers.outhaul-web-tls.tls"] != "true" {
		t.Errorf("tls not enabled on the secure router: %v", got)
	}
	if got["traefik.http.routers.outhaul-web-tls.tls.certresolver"] != "le" {
		t.Errorf("certresolver not set: %v", got)
	}
	if got["traefik.http.routers.outhaul-web-tls.rule"] != "Host(`web.example.com`)" {
		t.Errorf("secure router rule wrong: %v", got)
	}
	if got["traefik.http.routers.outhaul-web-tls.service"] != "outhaul-web" {
		t.Errorf("secure router should reuse the app service: %v", got)
	}
	http := Labels(core.App{Name: "web", Domain: "web.example.com"}, 8080, false)
	if _, ok := http["traefik.http.routers.outhaul-web-tls.tls"]; ok {
		t.Error("HTTP-only labels leaked a TLS router")
	}
}
