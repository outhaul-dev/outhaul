package store

import (
	"context"
	"testing"

	"github.com/james-smart/outhaul/internal/core"
)

func TestAttachmentsCRUD(t *testing.T) {
	s := openWithBox(t) // CreateDatabase needs a secret box
	ctx := context.Background()

	app, err := s.CreateApp(ctx, core.App{Name: "web", Source: core.SourcePublic, Kind: core.KindNixpacks, Branch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	db, err := s.CreateDatabase(ctx, core.Database{Name: "web-db", Engine: core.EnginePostgres, Username: "u", Password: "p", DBName: "web"})
	if err != nil {
		t.Fatal(err)
	}

	att, err := s.AttachDatabase(ctx, app.ID, db.ID, "DATABASE_URL")
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	if att.ID == 0 || att.EnvVar != "DATABASE_URL" {
		t.Fatalf("unexpected attachment: %+v", att)
	}

	list, err := s.ListAttachments(ctx, app.ID)
	if err != nil || len(list) != 1 || list[0].DatabaseID != db.ID {
		t.Fatalf("list = %+v, err %v", list, err)
	}

	if _, err := s.AttachDatabase(ctx, app.ID, db.ID, "DATABASE_URL"); err == nil {
		t.Fatal("expected duplicate env var to be rejected")
	}
	if _, err := s.AttachDatabase(ctx, app.ID, db.ID, "bad var"); err == nil {
		t.Fatal("expected invalid env var to be rejected")
	}

	back, err := s.AttachmentsForDatabase(ctx, db.ID)
	if err != nil || len(back) != 1 {
		t.Fatalf("AttachmentsForDatabase = %+v, err %v", back, err)
	}

	if err := s.DetachDatabase(ctx, app.ID, att.ID); err != nil {
		t.Fatalf("detach: %v", err)
	}
	list, _ = s.ListAttachments(ctx, app.ID)
	if len(list) != 0 {
		t.Fatalf("expected 0 after detach, got %d", len(list))
	}
}

func TestAttachDatabaseCrossProjectRejected(t *testing.T) {
	s := openWithBox(t) // CreateDatabase needs a secret box
	ctx := context.Background()
	proj, err := s.CreateProject(ctx, core.Project{Name: "other"})
	if err != nil {
		t.Fatal(err)
	}
	app, err := s.CreateApp(ctx, core.App{Name: "web", Source: core.SourcePublic, Kind: core.KindNixpacks, Branch: "main"}) // default project
	if err != nil {
		t.Fatal(err)
	}
	db, err := s.CreateDatabase(ctx, core.Database{ProjectID: proj.ID, Name: "otherdb", Engine: core.EnginePostgres, Username: "u", Password: "p", DBName: "d"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AttachDatabase(ctx, app.ID, db.ID, "DATABASE_URL"); err == nil {
		t.Fatal("expected cross-project attach to be rejected")
	}
}
