package server

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/james-smart/outhaul/internal/core"
)

func TestProjectEnvAddListAndMask(t *testing.T) {
	e := newTestEnv(t)
	e.login(t)
	p, err := e.store.CreateProject(context.Background(), core.Project{Name: "acme"})
	if err != nil {
		t.Fatal(err)
	}

	resp := e.postForm(t, "/projects/"+itoa(p.ID)+"/env", url.Values{
		"key": {"REGION"}, "value": {"eu-west-1"},
	})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("add env status = %d, want 303; body=%s", resp.StatusCode, body(t, resp))
	}
	resp.Body.Close()
	resp = e.postForm(t, "/projects/"+itoa(p.ID)+"/env", url.Values{
		"key": {"API_KEY"}, "value": {"s3cr3t"}, "secret": {"on"},
	})
	resp.Body.Close()

	page := body(t, e.get(t, "/projects/"+itoa(p.ID)))
	if !strings.Contains(page, "Shared variables") {
		t.Error("project page missing the Shared variables panel")
	}
	if !strings.Contains(page, "REGION") || !strings.Contains(page, "eu-west-1") {
		t.Error("normal shared var not shown")
	}
	if !strings.Contains(page, "API_KEY") {
		t.Error("secret key name should be shown")
	}
	if strings.Contains(page, "s3cr3t") {
		t.Error("secret VALUE leaked into the page")
	}
	if !strings.Contains(page, "${{project.KEY}}") {
		t.Error("panel should explain the ${{project.KEY}} reference syntax")
	}
}

func TestProjectEnvRejectsBadKey(t *testing.T) {
	e := newTestEnv(t)
	e.login(t)
	p, _ := e.store.CreateProject(context.Background(), core.Project{Name: "acme"})

	resp := e.postForm(t, "/projects/"+itoa(p.ID)+"/env", url.Values{"key": {"bad-key"}, "value": {"x"}})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	// The project page is re-rendered with the alert, not a bare error.
	if page := body(t, resp); !strings.Contains(page, "UPPER_SNAKE_CASE") {
		t.Error("400 should re-render the project page with the validation message")
	}
	vars, _ := e.store.ListProjectEnv(context.Background(), p.ID)
	if len(vars) != 0 {
		t.Errorf("invalid key stored anyway: %v", vars)
	}
}

func TestProjectEnvDelete(t *testing.T) {
	e := newTestEnv(t)
	e.login(t)
	p, _ := e.store.CreateProject(context.Background(), core.Project{Name: "acme"})
	e.store.SetProjectEnv(context.Background(), p.ID, "K", "v", false)

	resp := e.postForm(t, "/projects/"+itoa(p.ID)+"/env/delete", url.Values{"key": {"K"}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("delete status = %d, want 303", resp.StatusCode)
	}
	resp.Body.Close()
	vars, _ := e.store.ListProjectEnv(context.Background(), p.ID)
	if len(vars) != 0 {
		t.Errorf("env not deleted: %v", vars)
	}
}

func TestProjectEnvMissingProject404(t *testing.T) {
	e := newTestEnv(t)
	e.login(t)
	resp := e.postForm(t, "/projects/9999/env", url.Values{"key": {"K"}, "value": {"v"}})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("set status = %d, want 404", resp.StatusCode)
	}
	resp.Body.Close()
	resp = e.postForm(t, "/projects/9999/env/delete", url.Values{"key": {"K"}})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("delete status = %d, want 404", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestAppEnvPanelMentionsProjectReferences(t *testing.T) {
	e := newTestEnv(t)
	e.login(t)
	app, _ := e.store.CreateApp(context.Background(), core.App{Name: "web", RepoURL: "https://x/y.git", Domain: "web.test"})

	page := body(t, e.get(t, "/apps/"+itoa(app.ID)))
	if !strings.Contains(page, "${{project.KEY}}") {
		t.Error("app env panel should mention ${{project.KEY}} references")
	}
}
