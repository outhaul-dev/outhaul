package server

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/james-smart/outhaul/internal/core"
)

func seedDestination(t *testing.T, e *testEnv, name string) core.Destination {
	t.Helper()
	d, err := e.store.CreateDestination(context.Background(), core.Destination{
		Name: name, Endpoint: "https://s3.example.com", Bucket: "b",
		AccessKey: "AK", SecretKey: "SK",
	})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func destinationForm(name string) url.Values {
	return url.Values{
		"name": {name}, "endpoint": {"https://s3.example.com"}, "region": {"eu-west-2"},
		"bucket": {"outhaul"}, "access_key": {"AK"}, "secret_key": {"SK"},
	}
}

func TestCreateDestinationAndListOnSettings(t *testing.T) {
	e := newTestEnv(t)
	e.login(t)

	resp := e.postForm(t, "/settings/destinations", destinationForm("minio"))
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", resp.StatusCode)
	}
	dests, err := e.store.ListDestinations(context.Background())
	if err != nil || len(dests) != 1 {
		t.Fatalf("destinations = %v (err %v)", dests, err)
	}
	if dests[0].SecretKey != "SK" {
		t.Errorf("secret did not round-trip: %q", dests[0].SecretKey)
	}

	page := body(t, e.get(t, "/settings"))
	if !strings.Contains(page, "minio") || !strings.Contains(page, "/settings/destinations/"+itoa(dests[0].ID)+"/test") {
		t.Error("settings page missing the destination row or Test action")
	}
}

func TestCreateDestinationValidation(t *testing.T) {
	e := newTestEnv(t)
	e.login(t)

	bad := destinationForm("minio")
	bad.Set("endpoint", "not-a-url")
	if resp := e.postForm(t, "/settings/destinations", bad); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("bad endpoint status = %d, want 400", resp.StatusCode)
	}
	missing := destinationForm("minio")
	missing.Set("secret_key", "")
	if resp := e.postForm(t, "/settings/destinations", missing); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("missing secret status = %d, want 400", resp.StatusCode)
	}
}

func TestTestDestinationProbes(t *testing.T) {
	e := newTestEnv(t)
	e.login(t)
	d := seedDestination(t, e, "minio")

	resp := e.postForm(t, "/settings/destinations/"+itoa(d.ID)+"/test", url.Values{})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", resp.StatusCode)
	}
	if len(e.backups.tested) != 1 || e.backups.tested[0] != "minio" {
		t.Errorf("tested = %v", e.backups.tested)
	}

	e.backups.testErr = errors.New("AccessDenied")
	page := body(t, e.postForm(t, "/settings/destinations/"+itoa(d.ID)+"/test", url.Values{}))
	if !strings.Contains(page, "AccessDenied") {
		t.Error("probe failure not surfaced on the page")
	}
}

func TestDeleteDestinationGuardedWhileReferenced(t *testing.T) {
	e := newTestEnv(t)
	e.login(t)
	d := seedDestination(t, e, "minio")
	db := seedDatabase(t, e, "shop", core.EnginePostgres)
	if _, err := e.store.CreateBackup(context.Background(), core.Backup{
		TargetKind: core.BackupTargetDatabase, TargetID: db.ID, DestinationID: d.ID, Schedule: "0 3 * * *",
	}); err != nil {
		t.Fatal(err)
	}

	resp := e.postForm(t, "/settings/destinations/"+itoa(d.ID)+"/delete", url.Values{})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	if got := body(t, resp); !strings.Contains(got, "backup schedules") {
		t.Error("conflict page should explain the guard")
	}
}

func backupForm(kind string, targetID, destID int64) url.Values {
	return url.Values{
		"target_kind": {kind}, "target_id": {itoa(targetID)},
		"destination_id": {itoa(destID)}, "schedule": {"0 3 * * *"},
		"prefix": {"prod"}, "retention": {"7"},
	}
}

func TestCreateBackupForDatabase(t *testing.T) {
	e := newTestEnv(t)
	e.login(t)
	d := seedDestination(t, e, "minio")
	db := seedDatabase(t, e, "shop", core.EnginePostgres)

	resp := e.postForm(t, "/backups", backupForm(core.BackupTargetDatabase, db.ID, d.ID))
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/databases/"+itoa(db.ID) {
		t.Errorf("redirect = %q, want the database page", loc)
	}
	bs, err := e.store.ListBackupsForTarget(context.Background(), core.BackupTargetDatabase, db.ID)
	if err != nil || len(bs) != 1 {
		t.Fatalf("backups = %v (err %v)", bs, err)
	}
	b := bs[0]
	if b.Schedule != "0 3 * * *" || b.Prefix != "prod" || b.Retention != 7 || !b.Enabled {
		t.Errorf("backup = %+v", b)
	}
}

func TestCreateBackupValidation(t *testing.T) {
	e := newTestEnv(t)
	e.login(t)
	dest := seedDestination(t, e, "minio")
	redis := seedDatabase(t, e, "cache", core.EngineRedis)
	pg := seedDatabase(t, e, "shop", core.EnginePostgres)

	nix, err := e.store.CreateApp(context.Background(), core.App{Name: "api", RepoURL: "r"})
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		form url.Values
		want string
	}{
		{"redis target", backupForm(core.BackupTargetDatabase, redis.ID, dest.ID), "redis"},
		{"nixpacks target", backupForm(core.BackupTargetApp, nix.ID, dest.ID), "stateless"},
		{"bad cron", withField(backupForm(core.BackupTargetDatabase, pg.ID, dest.ID), "schedule", "every day"), "schedule"},
		{"unknown destination", backupForm(core.BackupTargetDatabase, pg.ID, 999), "destination"},
		{"bad retention", withField(backupForm(core.BackupTargetDatabase, pg.ID, dest.ID), "retention", "-1"), "Retention"},
		{"bad prefix", withField(backupForm(core.BackupTargetDatabase, pg.ID, dest.ID), "prefix", "a b"), "Prefix"},
		{"bad kind", withField(backupForm(core.BackupTargetDatabase, pg.ID, dest.ID), "target_kind", "volume"), "kind"},
	}
	for _, tc := range cases {
		resp := e.postForm(t, "/backups", tc.form)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", tc.name, resp.StatusCode)
			continue
		}
		if got := body(t, resp); !strings.Contains(got, tc.want) {
			t.Errorf("%s: error %q does not mention %q", tc.name, got, tc.want)
		}
	}
}

func withField(v url.Values, key, val string) url.Values {
	v.Set(key, val)
	return v
}

func TestRunToggleDeleteBackup(t *testing.T) {
	e := newTestEnv(t)
	e.login(t)
	dest := seedDestination(t, e, "minio")
	db := seedDatabase(t, e, "shop", core.EnginePostgres)
	b, err := e.store.CreateBackup(context.Background(), core.Backup{
		TargetKind: core.BackupTargetDatabase, TargetID: db.ID, DestinationID: dest.ID, Schedule: "0 3 * * *",
	})
	if err != nil {
		t.Fatal(err)
	}

	if resp := e.postForm(t, "/backups/"+itoa(b.ID)+"/run", url.Values{}); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("run status = %d", resp.StatusCode)
	}
	if len(e.backups.ran) != 1 || e.backups.ran[0] != b.ID {
		t.Errorf("ran = %v, want the backup handed to the manager", e.backups.ran)
	}

	if resp := e.postForm(t, "/backups/"+itoa(b.ID)+"/toggle", url.Values{}); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("toggle status = %d", resp.StatusCode)
	}
	if got, _ := e.store.GetBackup(context.Background(), b.ID); got.Enabled {
		t.Error("toggle did not pause the backup")
	}

	if resp := e.postForm(t, "/backups/"+itoa(b.ID)+"/delete", url.Values{}); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("delete status = %d", resp.StatusCode)
	}
	if _, err := e.store.GetBackup(context.Background(), b.ID); err == nil {
		t.Error("backup survived delete")
	}
}

func TestBackupPanelsRender(t *testing.T) {
	e := newTestEnv(t)
	e.login(t)
	dest := seedDestination(t, e, "minio")

	// Database page: panel with the create form.
	db := seedDatabase(t, e, "shop", core.EnginePostgres)
	page := body(t, e.get(t, "/databases/"+itoa(db.ID)))
	if !strings.Contains(page, `action="/backups"`) || !strings.Contains(page, dest.Name) {
		t.Error("database page missing the backups form")
	}

	// Redis page: no panel.
	redis := seedDatabase(t, e, "cache", core.EngineRedis)
	page = body(t, e.get(t, "/databases/"+itoa(redis.ID)))
	if strings.Contains(page, `action="/backups"`) {
		t.Error("redis page should not offer backups")
	}

	// Compose app page: panel present, with a run row once one exists.
	app, err := e.store.CreateApp(context.Background(), core.App{Name: "wiki", RepoURL: "r", Kind: core.KindCompose})
	if err != nil {
		t.Fatal(err)
	}
	b, err := e.store.CreateBackup(context.Background(), core.Backup{
		TargetKind: core.BackupTargetApp, TargetID: app.ID, DestinationID: dest.ID,
		Schedule: "30 2 * * 0", Prefix: "wiki",
	})
	if err != nil {
		t.Fatal(err)
	}
	runID, err := e.store.StartBackupRun(context.Background(), b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.store.FinishBackupRun(context.Background(), runID, core.RunOK, "", "wiki/wiki/vol/x.tar.gz", 12345); err != nil {
		t.Fatal(err)
	}
	page = body(t, e.get(t, "/apps/"+itoa(app.ID)))
	if !strings.Contains(page, "30 2 * * 0") || !strings.Contains(page, "wiki/wiki/vol/x.tar.gz") {
		t.Error("app page missing the schedule or its run history")
	}
	if !strings.Contains(page, "/backups/"+itoa(b.ID)+"/run") {
		t.Error("app page missing the Run now action")
	}

	// Nixpacks app page: no panel.
	nix, err := e.store.CreateApp(context.Background(), core.App{Name: "api", RepoURL: "r"})
	if err != nil {
		t.Fatal(err)
	}
	page = body(t, e.get(t, "/apps/"+itoa(nix.ID)))
	if strings.Contains(page, `action="/backups"`) {
		t.Error("nixpacks page should not offer backups")
	}
}
