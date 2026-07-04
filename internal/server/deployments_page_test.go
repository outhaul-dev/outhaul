package server

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

func TestDeploymentsPageListsAcrossApps(t *testing.T) {
	e := newTestEnv(t)
	e.login(t)

	e.postForm(t, "/apps", appForm("web", "web.example.com")).Body.Close()
	e.postForm(t, "/apps", appForm("api", "api.example.com")).Body.Close()
	web, _ := e.store.GetAppByName(context.Background(), "web")
	api, _ := e.store.GetAppByName(context.Background(), "api")
	e.store.CreateDeployment(context.Background(), web.ID)
	e.store.CreateDeployment(context.Background(), api.ID)

	resp := e.get(t, "/deployments")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /deployments = %d, want 200", resp.StatusCode)
	}
	page := body(t, resp)
	if !strings.Contains(page, "web") || !strings.Contains(page, "api") {
		t.Error("deployments page should list deployments from both apps")
	}
}

func TestDeploymentsPageRequiresAuth(t *testing.T) {
	e := newTestEnv(t)
	resp := e.get(t, "/deployments")
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login" {
		t.Fatalf("unauth /deployments = %d -> %q, want 303 -> /login", resp.StatusCode, resp.Header.Get("Location"))
	}
}
