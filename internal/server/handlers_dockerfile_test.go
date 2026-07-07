package server

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/outhaul-dev/outhaul/internal/core"
)

// dockerfileForm builds a valid create-app submission for a dockerfile app.
func dockerfileForm(name string) url.Values {
	return url.Values{
		"name": {name}, "kind": {"dockerfile"},
		"source": {"public"}, "repo_url": {"https://github.com/o/" + name + ".git"},
		"branch": {"main"}, "domain": {name + ".example.com"},
	}
}

func TestCreateDockerfileApp(t *testing.T) {
	e := newTestEnv(t)
	e.completeSetup(t)

	resp := e.postForm(t, "/apps", dockerfileForm("api"))
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("create = %d, want 303; body=%s", resp.StatusCode, body(t, resp))
	}
	resp.Body.Close()

	app, err := e.store.GetAppByName(context.Background(), "api")
	if err != nil {
		t.Fatal(err)
	}
	if app.Kind != core.KindDockerfile {
		t.Errorf("Kind = %q, want dockerfile", app.Kind)
	}
	if app.DockerfilePath != "Dockerfile" {
		t.Errorf("DockerfilePath = %q, want the Dockerfile default", app.DockerfilePath)
	}
	if app.Domain != "api.example.com" {
		t.Errorf("Domain = %q, want the form's domain (single-container apps keep App.Domain)", app.Domain)
	}
}

func TestCreateDockerfileAppValidation(t *testing.T) {
	e := newTestEnv(t)
	e.completeSetup(t)

	// A custom path is kept.
	form := dockerfileForm("api")
	form.Set("dockerfile_path", "deploy/Dockerfile.prod")
	e.postForm(t, "/apps", form).Body.Close()
	app, _ := e.store.GetAppByName(context.Background(), "api")
	if app.DockerfilePath != "deploy/Dockerfile.prod" {
		t.Errorf("DockerfilePath = %q, want the custom path", app.DockerfilePath)
	}

	// Path escaping the repo.
	form = dockerfileForm("api2")
	form.Set("dockerfile_path", "../Dockerfile")
	resp := e.postForm(t, "/apps", form)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("escaping path = %d, want 400", resp.StatusCode)
	}
	if !strings.Contains(body(t, resp), "relative path") {
		t.Error("expected the dockerfile-path error")
	}

	// Dockerfile apps require a domain, like nixpacks apps.
	form = dockerfileForm("api3")
	form.Set("domain", "")
	resp = e.postForm(t, "/apps", form)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("dockerfile app without domain = %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestDockerfileAppSettingsUpdate(t *testing.T) {
	e := newTestEnv(t)
	e.completeSetup(t)
	e.postForm(t, "/apps", dockerfileForm("api")).Body.Close()
	app, _ := e.store.GetAppByName(context.Background(), "api")

	resp := e.postForm(t, "/apps/"+itoa(app.ID)+"/settings", url.Values{
		"branch": {"main"}, "dockerfile_path": {"docker/Dockerfile"},
	})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("settings = %d, want 303; body=%s", resp.StatusCode, body(t, resp))
	}
	resp.Body.Close()
	got, _ := e.store.GetApp(context.Background(), app.ID)
	if got.DockerfilePath != "docker/Dockerfile" {
		t.Errorf("DockerfilePath = %q, want docker/Dockerfile", got.DockerfilePath)
	}

	// An escaping path is rejected and the stored value survives.
	resp = e.postForm(t, "/apps/"+itoa(app.ID)+"/settings", url.Values{
		"branch": {"main"}, "dockerfile_path": {"../../etc/Dockerfile"},
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("escaping path = %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()
	got, _ = e.store.GetApp(context.Background(), app.ID)
	if got.DockerfilePath != "docker/Dockerfile" {
		t.Errorf("DockerfilePath after rejected update = %q, want unchanged", got.DockerfilePath)
	}

	// The app page offers the Dockerfile field and badge.
	page := body(t, e.get(t, "/apps/"+itoa(app.ID)))
	if !strings.Contains(page, `name="dockerfile_path"`) {
		t.Error("app page missing the dockerfile-path settings field")
	}
	if !strings.Contains(page, "docker/Dockerfile") {
		t.Error("app page missing the stored Dockerfile path")
	}
}
