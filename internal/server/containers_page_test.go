package server

import (
	"net/http"
	"strings"
	"testing"

	"github.com/outhaul-dev/outhaul/internal/docker"
)

func TestContainersPageListsManagedContainers(t *testing.T) {
	e := newTestEnv(t)
	e.login(t)
	e.runtime.stack = []docker.Container{
		{ID: "c1", Name: "outhaul-app-web", Image: "outhaul/web:1", State: "running"},
		{ID: "c2", Name: "outhaul-db-shop", Image: "postgres:16", State: "running"},
		{ID: "c3", Name: "outhaul-shop-worker-1", Image: "outhaul/worker:1", State: "running"},
		{ID: "c4", Name: "outhaul-traefik", Image: "traefik:v3.7.6", State: "running"},
		{ID: "c5", Name: "outhaul-deploy-42", Image: "busybox", State: "exited"},
		{ID: "c6", Name: "some-other-thing", Image: "nginx", State: "running"},
		{ID: "c7", Name: "outhaul-db-rm-shop", Image: "postgres:16", State: "exited"},
	}

	resp := e.get(t, "/containers")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /containers = %d, want 200", resp.StatusCode)
	}
	page := body(t, resp)

	for _, want := range []string{"outhaul-app-web", "outhaul-db-shop", "outhaul-shop-worker-1"} {
		if !strings.Contains(page, want) {
			t.Errorf("containers page missing managed container %q", want)
		}
	}
	for _, hidden := range []string{"outhaul-traefik", "outhaul-deploy-42", "some-other-thing", "outhaul-db-rm-shop"} {
		if strings.Contains(page, hidden) {
			t.Errorf("containers page should hide infra/transient/foreign container %q", hidden)
		}
	}
}

func TestContainersPageRequiresAuth(t *testing.T) {
	e := newTestEnv(t)
	resp := e.get(t, "/containers")
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login" {
		t.Fatalf("unauth /containers = %d -> %q, want 303 -> /login", resp.StatusCode, resp.Header.Get("Location"))
	}
}

func TestDeploymentsRedirectsToContainers(t *testing.T) {
	e := newTestEnv(t)
	e.login(t)
	resp := e.get(t, "/deployments")
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/containers" {
		t.Fatalf("GET /deployments = %d -> %q, want 302 -> /containers", resp.StatusCode, resp.Header.Get("Location"))
	}
}
