package server

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

func TestOverviewShowsCounts(t *testing.T) {
	e := newTestEnv(t)
	e.login(t)

	e.postForm(t, "/apps", appForm("web", "web.example.com")).Body.Close()
	app, err := e.store.GetAppByName(context.Background(), "web")
	if err != nil {
		t.Fatal(err)
	}
	dep, err := e.store.CreateDeployment(context.Background(), app.ID)
	if err != nil {
		t.Fatal(err)
	}

	page := body(t, e.get(t, "/"))
	if !strings.Contains(page, "Overview") {
		t.Error("overview page missing heading")
	}
	if !strings.Contains(page, "Deployments") {
		t.Error("overview missing deployments stat")
	}
	if !strings.Contains(page, "web") {
		t.Error("overview missing recent-deployment app name")
	}
	if !strings.Contains(page, "/deployments/"+itoa(dep.ID)) {
		t.Error("overview missing recent-deployment link")
	}
}

func TestAppsListMovedToApps(t *testing.T) {
	e := newTestEnv(t)
	e.login(t)
	resp := e.get(t, "/apps")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /apps = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(body(t, resp), "New app") {
		t.Error("/apps should render the apps list with the create form")
	}
}
