package deploy

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/outhaul-dev/outhaul/internal/core"
	"github.com/outhaul-dev/outhaul/internal/dbaas"
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

// TestInjectAttachmentsAppendsWhenAbsent is the common case: the attachment's
// key is not already present, so the injected var is appended (not replacing
// anything) and the untouched var is preserved.
func TestInjectAttachmentsAppendsWhenAbsent(t *testing.T) {
	base := []core.EnvVar{{Key: "OTHER", Value: "x"}}
	db := core.Database{ID: 7, Name: "web-db", Engine: core.EnginePostgres, Username: "u", Password: "p", DBName: "web", Status: core.DBRunning}
	atts := []core.Attachment{{ID: 1, AppID: 1, DatabaseID: 7, EnvVar: "DATABASE_URL"}}

	out, err := injectAttachments(base, atts, func(id int64) (core.Database, error) { return db, nil })
	if err != nil {
		t.Fatal(err)
	}
	var injected, other *core.EnvVar
	for i := range out {
		switch out[i].Key {
		case "DATABASE_URL":
			injected = &out[i]
		case "OTHER":
			other = &out[i]
		}
	}
	if injected == nil {
		t.Fatal("DATABASE_URL not appended")
	}
	if injected.Value != dbaas.InternalURL(db) {
		t.Errorf("value = %q, want injected DSN %q", injected.Value, dbaas.InternalURL(db))
	}
	if !injected.IsSecret {
		t.Error("injected var must be secret")
	}
	if other == nil || other.Value != "x" {
		t.Errorf("OTHER not preserved: %+v", other)
	}
	if len(out) != 2 {
		t.Errorf("expected 2 vars, got %d: %v", len(out), out)
	}
}

// TestInjectAttachmentsMultiple: every attachment contributes its own injected
// secret var.
func TestInjectAttachmentsMultiple(t *testing.T) {
	dbs := map[int64]core.Database{
		7: {ID: 7, Name: "web-db", Engine: core.EnginePostgres, Username: "u", Password: "p", DBName: "web", Status: core.DBRunning},
		8: {ID: 8, Name: "cache", Engine: core.EngineRedis, Password: "r", Status: core.DBRunning},
	}
	atts := []core.Attachment{
		{ID: 1, AppID: 1, DatabaseID: 7, EnvVar: "DATABASE_URL"},
		{ID: 2, AppID: 1, DatabaseID: 8, EnvVar: "REDIS_URL"},
	}

	out, err := injectAttachments(nil, atts, func(id int64) (core.Database, error) { return dbs[id], nil })
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 {
		t.Fatalf("expected 2 injected vars, got %d: %v", len(out), out)
	}
	want := map[string]string{
		"DATABASE_URL": dbaas.InternalURL(dbs[7]),
		"REDIS_URL":    dbaas.InternalURL(dbs[8]),
	}
	for _, v := range out {
		if v.Value != want[v.Key] {
			t.Errorf("%s = %q, want %q", v.Key, v.Value, want[v.Key])
		}
		if !v.IsSecret {
			t.Errorf("%s must be secret", v.Key)
		}
		delete(want, v.Key)
	}
	if len(want) != 0 {
		t.Errorf("missing injected vars: %v", want)
	}
}

// TestInjectAttachmentsLoadError: a failure fetching the database is a hard
// error, wrapped so the deploy reason names the offending env var.
func TestInjectAttachmentsLoadError(t *testing.T) {
	_, err := injectAttachments(nil, []core.Attachment{{DatabaseID: 7, EnvVar: "DATABASE_URL"}}, func(id int64) (core.Database, error) {
		return core.Database{}, errors.New("boom")
	})
	if err == nil {
		t.Fatal("expected error when load fails")
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

// TestPipelineInjectsAttachedDatabaseURL is the end-to-end proof that
// loadEnv -> injectAttachments is wired: an attached, running database's
// connection URL lands in the container's RUNTIME env as a secret, and being
// secret it is excluded from the builder's BUILD env.
func TestPipelineInjectsAttachedDatabaseURL(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	app := h.app(t, "web")

	db, err := h.store.CreateDatabase(ctx, core.Database{
		Name: "web-db", Engine: core.EnginePostgres, Image: "postgres:17",
		Username: "u", Password: "p", DBName: "web",
	})
	if err != nil {
		t.Fatalf("CreateDatabase: %v", err)
	}
	// CreateDatabase always records DBCreating; a deploy requires it running.
	if err := h.store.SetDatabaseStatus(ctx, db.ID, core.DBRunning, ""); err != nil {
		t.Fatalf("SetDatabaseStatus: %v", err)
	}
	if _, err := h.store.AttachDatabase(ctx, app.ID, db.ID, "DATABASE_URL"); err != nil {
		t.Fatalf("AttachDatabase: %v", err)
	}
	// Re-read the row the pipeline will see, to compute the expected DSN.
	running, err := h.store.GetDatabase(ctx, db.ID)
	if err != nil {
		t.Fatalf("GetDatabase: %v", err)
	}
	wantURL := dbaas.InternalURL(running)

	dep := h.claimedDeployment(t, app.ID)
	h.worker.runPipeline(ctx, dep)

	if got := h.status(t, dep.ID); got.Status != core.StatusRunning {
		t.Fatalf("status = %q (%q), want running", got.Status, got.Reason)
	}
	spec := lastCreatedNamed(t, h, "outhaul-app-web")
	if !contains(spec.Env, "DATABASE_URL="+wantURL) {
		t.Errorf("runtime env missing injected DATABASE_URL=%s: %v", wantURL, spec.Env)
	}
	if _, ok := h.builder.lastReq.Env["DATABASE_URL"]; ok {
		t.Error("injected attachment URL (secret) leaked into the build env")
	}
}

// TestPipelineFailsOnStoppedAttachedDatabase: attaching a non-running database
// fails the deploy with a reason naming it, instead of shipping a dead URL that
// would fail opaquely at runtime.
func TestPipelineFailsOnStoppedAttachedDatabase(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	app := h.app(t, "web")

	db, err := h.store.CreateDatabase(ctx, core.Database{
		Name: "web-db", Engine: core.EnginePostgres, Image: "postgres:17",
		Username: "u", Password: "p", DBName: "web",
	})
	if err != nil {
		t.Fatalf("CreateDatabase: %v", err)
	}
	if err := h.store.SetDatabaseStatus(ctx, db.ID, core.DBStopped, ""); err != nil {
		t.Fatalf("SetDatabaseStatus: %v", err)
	}
	if _, err := h.store.AttachDatabase(ctx, app.ID, db.ID, "DATABASE_URL"); err != nil {
		t.Fatalf("AttachDatabase: %v", err)
	}

	dep := h.claimedDeployment(t, app.ID)
	h.worker.runPipeline(ctx, dep)

	got := h.status(t, dep.ID)
	if got.Status != core.StatusFailed {
		t.Fatalf("status = %q, want failed", got.Status)
	}
	if !strings.Contains(got.Reason, "web-db") || !strings.Contains(got.Reason, "running") {
		t.Errorf("reason = %q, want it to name the database and its non-running state", got.Reason)
	}
	if len(h.docker.Created) != 0 {
		t.Errorf("no container should be created for a stopped attached DB: %v", h.docker.Created)
	}
}
