package traefik

import (
	"testing"

	"github.com/slipwaydev/slipway/internal/core"
)

func TestLabels(t *testing.T) {
	app := core.App{Name: "web", Domain: "web.example.com"}

	got := Labels(app, 8080)

	want := map[string]string{
		"traefik.enable": "true",
		"slipway.managed": "true",
		"slipway.app":     "web",
		"traefik.http.routers.slipway-web.rule":                      "Host(`web.example.com`)",
		"traefik.http.routers.slipway-web.entrypoints":               "web",
		"traefik.http.services.slipway-web.loadbalancer.server.port": "8080",
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
	a := Labels(core.App{Name: "api", Domain: "api.test"}, 3000)
	b := Labels(core.App{Name: "web", Domain: "web.test"}, 3000)

	if a["traefik.http.routers.slipway-api.rule"] == "" {
		t.Error("api router label missing")
	}
	if b["traefik.http.routers.slipway-web.rule"] == "" {
		t.Error("web router label missing")
	}
	// The api app must not define a web router (no cross-talk).
	if _, ok := a["traefik.http.routers.slipway-web.rule"]; ok {
		t.Error("api labels leaked a web router")
	}
}

func TestLabelsPortRendered(t *testing.T) {
	got := Labels(core.App{Name: "svc", Domain: "svc.test"}, 5000)
	if got["traefik.http.services.slipway-svc.loadbalancer.server.port"] != "5000" {
		t.Errorf("port label = %q", got["traefik.http.services.slipway-svc.loadbalancer.server.port"])
	}
}
