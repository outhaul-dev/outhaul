package backup

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/outhaul-dev/outhaul/internal/core"
	"github.com/outhaul-dev/outhaul/internal/docker"
)

// gzipped compresses s the way the backup path stores database dumps.
func gzipped(t *testing.T, s string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := io.WriteString(w, s); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestPostgresRestoreStreamsDumpIntoContainer(t *testing.T) {
	h := newHarness(t)
	dest := h.destination(t)
	db := h.seedRunningDatabase(t, "shop", core.EnginePostgres)
	b := h.backup(t, core.BackupTargetDatabase, db.ID, dest.ID, 0)

	const key = "prod/shop/20260101T000000Z.dump.gz"
	h.blob.objects[key] = gzipped(t, "PGDMP-restore-bytes")

	h.m.RestoreNow(b, key)
	h.waitRun(t)

	run := h.lastRun(t, b.ID)
	if run.Status != core.RunOK {
		t.Fatalf("run = %q (%s), want ok", run.Status, run.Reason)
	}
	if run.Kind != core.RunKindRestore || run.ObjectKey != key {
		t.Errorf("run record = kind %q key %q", run.Kind, run.ObjectKey)
	}
	if run.SizeBytes != int64(len(h.blob.objects[key])) {
		t.Errorf("size = %d, want the downloaded archive's %d bytes", run.SizeBytes, len(h.blob.objects[key]))
	}

	if len(h.docker.Execs) != 1 {
		t.Fatalf("execs = %d, want 1", len(h.docker.Execs))
	}
	exec := h.docker.Execs[0]
	wantCmd := "pg_restore -U shop -d shop -O --clean --if-exists"
	if strings.Join(exec.Cmd, " ") != wantCmd {
		t.Errorf("cmd = %q, want %q", strings.Join(exec.Cmd, " "), wantCmd)
	}
	if len(exec.Env) != 1 || exec.Env[0] != "PGPASSWORD=pw" {
		t.Errorf("env = %v, want PGPASSWORD", exec.Env)
	}
	if string(exec.Stdin) != "PGDMP-restore-bytes" {
		t.Errorf("stdin = %q, want the gunzipped dump", exec.Stdin)
	}
}

func TestMySQLRestoreCommand(t *testing.T) {
	h := newHarness(t)
	dest := h.destination(t)
	db := h.seedRunningDatabase(t, "erp", core.EngineMySQL)
	b := h.backup(t, core.BackupTargetDatabase, db.ID, dest.ID, 0)

	const key = "prod/erp/20260101T000000Z.sql.gz"
	h.blob.objects[key] = gzipped(t, "-- MySQL dump")

	h.m.RestoreNow(b, key)
	h.waitRun(t)

	if run := h.lastRun(t, b.ID); run.Status != core.RunOK {
		t.Fatalf("run = %q (%s)", run.Status, run.Reason)
	}
	exec := h.docker.Execs[0]
	if strings.Join(exec.Cmd, " ") != "mysql -uroot erp" {
		t.Errorf("cmd = %v", exec.Cmd)
	}
	if len(exec.Env) != 1 || exec.Env[0] != "MYSQL_PWD=pw" {
		t.Errorf("env = %v, want MYSQL_PWD", exec.Env)
	}
	if string(exec.Stdin) != "-- MySQL dump" {
		t.Errorf("stdin = %q", exec.Stdin)
	}
}

func TestRestoreRejectsKeyOutsideBackupDirectory(t *testing.T) {
	h := newHarness(t)
	dest := h.destination(t)
	db := h.seedRunningDatabase(t, "shop", core.EnginePostgres)
	b := h.backup(t, core.BackupTargetDatabase, db.ID, dest.ID, 0)
	h.blob.objects["other/secret.dump.gz"] = gzipped(t, "x")

	h.m.RestoreNow(b, "other/secret.dump.gz")
	h.waitRun(t)

	run := h.lastRun(t, b.ID)
	if run.Status != core.RunFailed || !strings.Contains(run.Reason, "not under") {
		t.Errorf("run = %q (%q), want failed/not under this backup's directory", run.Status, run.Reason)
	}
	if len(h.docker.Execs) != 0 {
		t.Error("a rejected key must never reach the container")
	}
}

func TestRestoreDownloadFailureTouchesNothing(t *testing.T) {
	h := newHarness(t)
	dest := h.destination(t)
	db := h.seedRunningDatabase(t, "shop", core.EnginePostgres)
	b := h.backup(t, core.BackupTargetDatabase, db.ID, dest.ID, 0)
	h.blob.getErr = errors.New("connection reset by peer")

	h.m.RestoreNow(b, "prod/shop/20260101T000000Z.dump.gz")
	h.waitRun(t)

	run := h.lastRun(t, b.ID)
	if run.Status != core.RunFailed || !strings.Contains(run.Reason, "connection reset") {
		t.Errorf("run = %q (%q)", run.Status, run.Reason)
	}
	if len(h.docker.Execs) != 0 {
		t.Error("a failed download must never reach the container")
	}
}

func TestRestoreToolFailureRecordsStderr(t *testing.T) {
	h := newHarness(t)
	dest := h.destination(t)
	db := h.seedRunningDatabase(t, "shop", core.EnginePostgres)
	b := h.backup(t, core.BackupTargetDatabase, db.ID, dest.ID, 0)
	const key = "prod/shop/20260101T000000Z.dump.gz"
	h.blob.objects[key] = gzipped(t, "PGDMP")
	h.docker.OnExec = func(string, []string, []string) (string, string, int, error) {
		return "", "relation is busy", 1, nil
	}

	h.m.RestoreNow(b, key)
	h.waitRun(t)

	run := h.lastRun(t, b.ID)
	if run.Status != core.RunFailed ||
		!strings.Contains(run.Reason, "pg_restore exited 1") ||
		!strings.Contains(run.Reason, "relation is busy") {
		t.Errorf("reason = %q, want tool exit and stderr", run.Reason)
	}
}

// seedComposeWithVolume creates a compose app, its named volume, one running
// container, and one that is not running.
func (h *harness) seedComposeWithVolume(t *testing.T) core.App {
	t.Helper()
	app, err := h.store.CreateApp(context.Background(), core.App{
		Name: "wiki", RepoURL: "https://example.com/r.git", Kind: core.KindCompose,
	})
	if err != nil {
		t.Fatal(err)
	}
	h.docker.Volumes["outhaul-wiki_data"] = map[string]string{"com.docker.compose.project": "outhaul-wiki"}
	labels := map[string]string{"com.docker.compose.project": "outhaul-wiki"}
	web, err := h.docker.CreateContainer(context.Background(), docker.ContainerSpec{Name: "wiki-web", Image: "img", Labels: labels})
	if err != nil {
		t.Fatal(err)
	}
	h.docker.StartContainer(context.Background(), web)
	if _, err := h.docker.CreateContainer(context.Background(), docker.ContainerSpec{Name: "wiki-job", Image: "img", Labels: labels}); err != nil {
		t.Fatal(err)
	}
	return app
}

func TestVolumeRestoreStopsUntarsAndRestarts(t *testing.T) {
	h := newHarness(t)
	dest := h.destination(t)
	app := h.seedComposeWithVolume(t)
	b := h.backup(t, core.BackupTargetApp, app.ID, dest.ID, 0)

	const key = "prod/wiki/outhaul-wiki_data/20260101T000000Z.tar.gz"
	h.blob.objects[key] = []byte("tarball")

	var stateDuringUntar string
	h.docker.OnRun = func(spec docker.ContainerSpec) (string, int, error) {
		c, _ := h.docker.FindContainer(context.Background(), "wiki-web")
		stateDuringUntar = c.State
		return "", 0, nil
	}

	h.m.RestoreNow(b, key)
	h.waitRun(t)

	run := h.lastRun(t, b.ID)
	if run.Status != core.RunOK {
		t.Fatalf("run = %q (%s)", run.Status, run.Reason)
	}
	if run.Kind != core.RunKindRestore {
		t.Errorf("kind = %q", run.Kind)
	}
	if stateDuringUntar != "exited" {
		t.Errorf("web container state during untar = %q, want exited (stack stopped first)", stateDuringUntar)
	}
	if c, _ := h.docker.FindContainer(context.Background(), "wiki-web"); !c.Running() {
		t.Error("previously running container was not restarted")
	}
	if c, _ := h.docker.FindContainer(context.Background(), "wiki-job"); c.Running() {
		t.Error("container that was not running got started by the restore")
	}

	if len(h.docker.Runs) != 1 {
		t.Fatalf("helper runs = %d, want 1", len(h.docker.Runs))
	}
	spec := h.docker.Runs[0]
	if spec.Image != helperImage {
		t.Errorf("helper image = %q", spec.Image)
	}
	if got := strings.Join(spec.Cmd, " "); !strings.Contains(got, "find /dst -mindepth 1 -delete") || !strings.Contains(got, "tar xzf /restore.tar.gz -C /dst") {
		t.Errorf("helper cmd = %q, want empty-then-untar", got)
	}
	if len(spec.Mounts) != 2 {
		t.Fatalf("mounts = %+v", spec.Mounts)
	}
	vol, archive := spec.Mounts[0], spec.Mounts[1]
	if vol.Source != "outhaul-wiki_data" || vol.Target != "/dst" || !vol.Volume || vol.ReadOnly {
		t.Errorf("volume mount = %+v, want the named volume rw at /dst", vol)
	}
	if archive.Target != "/restore.tar.gz" || !archive.ReadOnly || archive.Volume || archive.Source == "" {
		t.Errorf("archive mount = %+v, want the staged file ro", archive)
	}
}

func TestVolumeRestoreRestartsStackEvenWhenUntarFails(t *testing.T) {
	h := newHarness(t)
	dest := h.destination(t)
	app := h.seedComposeWithVolume(t)
	b := h.backup(t, core.BackupTargetApp, app.ID, dest.ID, 0)
	const key = "prod/wiki/outhaul-wiki_data/20260101T000000Z.tar.gz"
	h.blob.objects[key] = []byte("tarball")
	h.docker.OnRun = func(docker.ContainerSpec) (string, int, error) { return "", 1, nil }

	h.m.RestoreNow(b, key)
	h.waitRun(t)

	if run := h.lastRun(t, b.ID); run.Status != core.RunFailed || !strings.Contains(run.Reason, "exited 1") {
		t.Errorf("run = %q (%q)", run.Status, run.Reason)
	}
	if c, _ := h.docker.FindContainer(context.Background(), "wiki-web"); !c.Running() {
		t.Error("stack must be restarted even when the untar failed")
	}
}

func TestVolumeRestoreRejectsUnknownVolume(t *testing.T) {
	h := newHarness(t)
	dest := h.destination(t)
	app := h.seedComposeWithVolume(t)
	b := h.backup(t, core.BackupTargetApp, app.ID, dest.ID, 0)
	const key = "prod/wiki/ghost-volume/20260101T000000Z.tar.gz"
	h.blob.objects[key] = []byte("tarball")

	h.m.RestoreNow(b, key)
	h.waitRun(t)

	if run := h.lastRun(t, b.ID); run.Status != core.RunFailed || !strings.Contains(run.Reason, "does not exist") {
		t.Errorf("run = %q (%q), want failed/volume does not exist", run.Status, run.Reason)
	}
	if len(h.docker.Runs) != 0 {
		t.Error("no helper should run for an unknown volume")
	}
	if c, _ := h.docker.FindContainer(context.Background(), "wiki-web"); !c.Running() {
		t.Error("stack must not be left stopped")
	}
}

// seedSingleContainerWithVolume creates a nixpacks app, its named volume
// (labeled the way a real deploy would), and its running canonical container.
func (h *harness) seedSingleContainerWithVolume(t *testing.T) core.App {
	t.Helper()
	app, err := h.store.CreateApp(context.Background(), core.App{
		Name: "web", RepoURL: "https://example.com/web.git", Kind: core.KindNixpacks,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.AddVolume(context.Background(), app.ID, "/data"); err != nil {
		t.Fatal(err)
	}
	h.docker.Volumes["outhaul-web-data"] = core.VolumeLabels("web")
	c, err := h.docker.CreateContainer(context.Background(), docker.ContainerSpec{Name: "outhaul-app-web", Image: "web:1"})
	if err != nil {
		t.Fatal(err)
	}
	h.docker.StartContainer(context.Background(), c)
	return app
}

func TestRestoreVolumeSingleContainer(t *testing.T) {
	h := newHarness(t)
	dest := h.destination(t)
	app := h.seedSingleContainerWithVolume(t)
	b := h.backup(t, core.BackupTargetApp, app.ID, dest.ID, 0)

	const key = "prod/web/outhaul-web-data/20260101T000000Z.tar.gz"
	h.blob.objects[key] = []byte("tarball")

	var stateDuringUntar string
	h.docker.OnRun = func(spec docker.ContainerSpec) (string, int, error) {
		c, _ := h.docker.FindContainer(context.Background(), "outhaul-app-web")
		stateDuringUntar = c.State
		return "", 0, nil
	}

	h.m.RestoreNow(b, key)
	h.waitRun(t)

	run := h.lastRun(t, b.ID)
	if run.Status != core.RunOK {
		t.Fatalf("run = %q (%s)", run.Status, run.Reason)
	}
	if stateDuringUntar != "exited" {
		t.Errorf("app container state during untar = %q, want exited (stopped first)", stateDuringUntar)
	}
	if c, _ := h.docker.FindContainer(context.Background(), "outhaul-app-web"); c == nil || !c.Running() {
		t.Error("canonical container should be running again after restore")
	}
	if len(h.docker.Runs) != 1 {
		t.Fatalf("helper runs = %d, want 1", len(h.docker.Runs))
	}
	vol := h.docker.Runs[0].Mounts[0]
	if vol.Source != "outhaul-web-data" || vol.Target != "/dst" || !vol.Volume || vol.ReadOnly {
		t.Errorf("volume mount = %+v, want the named volume rw at /dst", vol)
	}
}

func TestRestoreSharesInFlightGuardWithBackups(t *testing.T) {
	h := newHarness(t)
	dest := h.destination(t)
	db := h.seedRunningDatabase(t, "shop", core.EnginePostgres)
	b := h.backup(t, core.BackupTargetDatabase, db.ID, dest.ID, 0)
	const key = "prod/shop/20260101T000000Z.dump.gz"
	h.blob.objects[key] = gzipped(t, "PGDMP")

	block := make(chan struct{})
	h.docker.OnExec = func(string, []string, []string) (string, string, int, error) {
		<-block
		return "x", "", 0, nil
	}
	h.m.RunNow(b)          // backup in flight...
	h.m.RestoreNow(b, key) // ...restore of the same schedule must be skipped
	close(block)
	h.waitRun(t)

	select {
	case <-h.m.runDone:
		t.Fatal("overlapping restore executed")
	case <-time.After(100 * time.Millisecond):
	}
	if runs, _ := h.store.ListBackupRuns(context.Background(), b.ID, 10); len(runs) != 1 {
		t.Errorf("runs = %d, want just the backup", len(runs))
	}
}

func TestListRestoreObjectsNewestFirstOwnDirectoryOnly(t *testing.T) {
	h := newHarness(t)
	dest := h.destination(t)
	db := h.seedRunningDatabase(t, "shop", core.EnginePostgres)
	b := h.backup(t, core.BackupTargetDatabase, db.ID, dest.ID, 0)

	h.blob.objects["prod/shop/20250101T000000Z.dump.gz"] = []byte("a")
	h.blob.objects["prod/shop/20260101T000000Z.dump.gz"] = []byte("b")
	h.blob.objects["prod/other/20270101T000000Z.dump.gz"] = []byte("c")

	objs, err := h.m.ListRestoreObjects(context.Background(), b)
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 2 ||
		objs[0].Key != "prod/shop/20260101T000000Z.dump.gz" ||
		objs[1].Key != "prod/shop/20250101T000000Z.dump.gz" {
		t.Errorf("objects = %+v, want shop's archives newest first", objs)
	}

	dir, err := h.m.RestoreDir(context.Background(), b)
	if err != nil || dir != "prod/shop" {
		t.Errorf("RestoreDir = %q (%v), want prod/shop", dir, err)
	}
}
