package store

import (
	"context"
	"errors"
	"testing"

	"github.com/outhaul-dev/outhaul/internal/core"
)

func TestDefaultProjectExistsAfterMigration(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	p, err := s.GetProject(ctx, core.DefaultProjectID)
	if err != nil {
		t.Fatalf("GetProject(default): %v", err)
	}
	if p.Name != "default" {
		t.Errorf("default project name = %q, want %q", p.Name, "default")
	}
	if p.CreatedAt.IsZero() {
		t.Error("default project has zero CreatedAt")
	}
}

func TestAppsLandInDefaultProject(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	app := mustApp(t, s, "web")
	if app.ProjectID != core.DefaultProjectID {
		t.Errorf("CreateApp ProjectID = %d, want %d", app.ProjectID, core.DefaultProjectID)
	}
	got, err := s.GetApp(ctx, app.ID)
	if err != nil {
		t.Fatalf("GetApp: %v", err)
	}
	if got.ProjectID != core.DefaultProjectID {
		t.Errorf("GetApp ProjectID = %d, want %d", got.ProjectID, core.DefaultProjectID)
	}
}

func TestProjectCRUD(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	p, err := s.CreateProject(ctx, core.Project{Name: "acme", Description: "Acme Corp"})
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if p.ID == 0 || p.CreatedAt.IsZero() {
		t.Fatalf("CreateProject did not populate ID/CreatedAt: %+v", p)
	}

	if _, err := s.CreateProject(ctx, core.Project{Name: "acme"}); err == nil {
		t.Error("duplicate project name should fail")
	}

	if err := s.UpdateProject(ctx, p.ID, "acme-prod", "renamed"); err != nil {
		t.Fatalf("UpdateProject: %v", err)
	}
	got, err := s.GetProject(ctx, p.ID)
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if got.Name != "acme-prod" || got.Description != "renamed" {
		t.Errorf("after update = %+v", got)
	}

	projects, err := s.ListProjects(ctx)
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	if len(projects) != 2 { // default + acme-prod
		t.Fatalf("ListProjects len = %d, want 2", len(projects))
	}
	if projects[0].Name != "acme-prod" || projects[1].Name != "default" {
		t.Errorf("ListProjects order = %q, %q; want name order", projects[0].Name, projects[1].Name)
	}
}

func TestDeleteProjectGuardedByApps(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	p, err := s.CreateProject(ctx, core.Project{Name: "acme"})
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	app, err := s.CreateApp(ctx, core.App{
		Name: "web", RepoURL: "https://example.com/r.git", Domain: "web.test", ProjectID: p.ID,
	})
	if err != nil {
		t.Fatalf("CreateApp: %v", err)
	}

	if err := s.DeleteProject(ctx, p.ID); !errors.Is(err, ErrProjectNotEmpty) {
		t.Fatalf("DeleteProject with apps = %v, want ErrProjectNotEmpty", err)
	}
	if _, err := s.GetProject(ctx, p.ID); err != nil {
		t.Fatalf("project should survive a refused delete: %v", err)
	}

	if err := s.DeleteApp(ctx, app.ID); err != nil {
		t.Fatalf("DeleteApp: %v", err)
	}
	if err := s.DeleteProject(ctx, p.ID); err != nil {
		t.Fatalf("DeleteProject after emptying: %v", err)
	}
	if _, err := s.GetProject(ctx, p.ID); err == nil {
		t.Error("project should be gone after delete")
	}
}

func TestCountAppsByProject(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	p, err := s.CreateProject(ctx, core.Project{Name: "acme"})
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	mustApp(t, s, "web") // default project
	for _, name := range []string{"api", "worker"} {
		if _, err := s.CreateApp(ctx, core.App{
			Name: name, RepoURL: "https://example.com/r.git", Domain: name + ".test", ProjectID: p.ID,
		}); err != nil {
			t.Fatalf("CreateApp(%s): %v", name, err)
		}
	}

	counts, err := s.CountAppsByProject(ctx)
	if err != nil {
		t.Fatalf("CountAppsByProject: %v", err)
	}
	if counts[core.DefaultProjectID] != 1 || counts[p.ID] != 2 {
		t.Errorf("counts = %v, want default:1 %d:2", counts, p.ID)
	}

	apps, err := s.ListAppsByProject(ctx, p.ID)
	if err != nil {
		t.Fatalf("ListAppsByProject: %v", err)
	}
	if len(apps) != 2 || apps[0].Name != "api" || apps[1].Name != "worker" {
		t.Errorf("ListAppsByProject = %+v", apps)
	}
}
