package deploy

import (
	"testing"

	"github.com/james-smart/outhaul/internal/core"
	"github.com/james-smart/outhaul/internal/dbaas"
)

func TestInjectAttachmentsWinsAndIsSecret(t *testing.T) {
	base := []core.EnvVar{{Key: "DATABASE_URL", Value: "manual", IsSecret: false}, {Key: "OTHER", Value: "x"}}
	db := core.Database{ID: 7, Name: "web-db", Engine: core.EnginePostgres, Username: "u", Password: "p", DBName: "web", Status: core.DBRunning}
	atts := []core.Attachment{{ID: 1, AppID: 1, DatabaseID: 7, EnvVar: "DATABASE_URL"}}

	out, err := injectAttachments(base, atts, func(id int64) (core.Database, error) { return db, nil })
	if err != nil {
		t.Fatal(err)
	}
	var found *core.EnvVar
	for i := range out {
		if out[i].Key == "DATABASE_URL" {
			found = &out[i]
		}
	}
	if found == nil {
		t.Fatal("DATABASE_URL missing")
	}
	if found.Value != dbaas.InternalURL(db) {
		t.Errorf("value = %q, want injected DSN %q", found.Value, dbaas.InternalURL(db))
	}
	if !found.IsSecret {
		t.Error("injected var must be secret")
	}
	count := 0
	for _, v := range out {
		if v.Key == "DATABASE_URL" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected 1 DATABASE_URL, got %d", count)
	}
}

func TestInjectAttachmentsRejectsStoppedDatabase(t *testing.T) {
	_, err := injectAttachments(nil, []core.Attachment{{DatabaseID: 7, EnvVar: "DATABASE_URL"}}, func(id int64) (core.Database, error) {
		return core.Database{ID: 7, Name: "web-db", Status: core.DBStopped}, nil
	})
	if err == nil {
		t.Fatal("expected error for non-running database")
	}
}
