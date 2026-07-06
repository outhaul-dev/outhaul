package store

import (
	"context"
	"strings"
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

func TestDetachDatabaseScopedToApp(t *testing.T) {
	s := openWithBox(t) // CreateDatabase needs a secret box
	ctx := context.Background()

	appA, err := s.CreateApp(ctx, core.App{Name: "web", Source: core.SourcePublic, Kind: core.KindNixpacks, Branch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	appB, err := s.CreateApp(ctx, core.App{Name: "api", Source: core.SourcePublic, Kind: core.KindNixpacks, Branch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	db, err := s.CreateDatabase(ctx, core.Database{Name: "shared-db", Engine: core.EnginePostgres, Username: "u", Password: "p", DBName: "shared"})
	if err != nil {
		t.Fatal(err)
	}

	att, err := s.AttachDatabase(ctx, appA.ID, db.ID, "DATABASE_URL")
	if err != nil {
		t.Fatalf("attach: %v", err)
	}

	// Wrong app id must not delete the attachment.
	if err := s.DetachDatabase(ctx, appB.ID, att.ID); err != nil {
		t.Fatalf("DetachDatabase (wrong app): %v", err)
	}
	if got, _ := s.ListAttachments(ctx, appA.ID); len(got) != 1 {
		t.Fatalf("attachment deleted through the wrong app: got %d, want 1", len(got))
	}

	// Correct app id removes it.
	if err := s.DetachDatabase(ctx, appA.ID, att.ID); err != nil {
		t.Fatalf("DetachDatabase: %v", err)
	}
	if got, _ := s.ListAttachments(ctx, appA.ID); len(got) != 0 {
		t.Fatalf("attachment not deleted: got %d, want 0", len(got))
	}
}

func TestDeleteDatabaseBlockedWhileAttached(t *testing.T) {
	s := openWithBox(t)
	ctx := context.Background()
	app, _ := s.CreateApp(ctx, core.App{Name: "web", Source: core.SourcePublic, Kind: core.KindNixpacks, Branch: "main"})
	db, _ := s.CreateDatabase(ctx, core.Database{Name: "web-db", Engine: core.EnginePostgres, Username: "u", Password: "p", DBName: "web"})
	att, _ := s.AttachDatabase(ctx, app.ID, db.ID, "DATABASE_URL")

	err := s.DeleteDatabase(ctx, db.ID)
	if err == nil {
		t.Fatal("expected delete to be blocked while attached")
	}
	if !strings.Contains(err.Error(), "detach it first") {
		t.Fatalf("expected guard message, got FK/other error: %v", err)
	}
	// After detaching, delete succeeds.
	if err := s.DetachDatabase(ctx, app.ID, att.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteDatabase(ctx, db.ID); err != nil {
		t.Fatalf("delete after detach: %v", err)
	}
}

func TestDeleteAppRemovesAttachments(t *testing.T) {
	s := openWithBox(t) // CreateDatabase needs a secret box
	ctx := context.Background()
	app, _ := s.CreateApp(ctx, core.App{Name: "web", Source: core.SourcePublic, Kind: core.KindNixpacks, Branch: "main"})
	db, _ := s.CreateDatabase(ctx, core.Database{Name: "web-db", Engine: core.EnginePostgres, Username: "u", Password: "p", DBName: "web"})
	if _, err := s.AttachDatabase(ctx, app.ID, db.ID, "DATABASE_URL"); err != nil {
		t.Fatalf("attach: %v", err)
	}

	if err := s.DeleteApp(ctx, app.ID); err != nil {
		t.Fatalf("DeleteApp: %v", err)
	}
	if got, err := s.ListAttachments(ctx, app.ID); err != nil || len(got) != 0 {
		t.Fatalf("ListAttachments after delete = %+v, err %v", got, err)
	}
}
