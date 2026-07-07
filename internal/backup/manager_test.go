package backup

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/outhaul-dev/outhaul/internal/blobstore"
	"github.com/outhaul-dev/outhaul/internal/core"
	"github.com/outhaul-dev/outhaul/internal/docker"
	"github.com/outhaul-dev/outhaul/internal/secret"
	"github.com/outhaul-dev/outhaul/internal/store"
)

// fakeBlob is an in-memory blobstore.Client.
type fakeBlob struct {
	mu      sync.Mutex
	objects map[string][]byte
	putErr  error
	getErr  error
}

func (f *fakeBlob) Put(_ context.Context, key string, r io.Reader, size int64) error {
	if f.putErr != nil {
		return f.putErr
	}
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	if int64(len(b)) != size {
		return errors.New("size mismatch")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects[key] = b
	return nil
}

func (f *fakeBlob) Get(_ context.Context, key string) (io.ReadCloser, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.objects[key]
	if !ok {
		return nil, errors.New("404 Not Found: NoSuchKey")
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

func (f *fakeBlob) List(_ context.Context, prefix string) ([]blobstore.Object, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []blobstore.Object
	for k, v := range f.objects {
		if strings.HasPrefix(k, prefix) {
			out = append(out, blobstore.Object{Key: k, Size: int64(len(v))})
		}
	}
	// sorted ascending, as the real client returns
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j].Key < out[i].Key {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out, nil
}

func (f *fakeBlob) Delete(_ context.Context, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.objects, key)
	return nil
}

func (f *fakeBlob) keys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var ks []string
	for k := range f.objects {
		ks = append(ks, k)
	}
	return ks
}

type harness struct {
	m      *Manager
	store  *store.Store
	docker *docker.Fake
	blob   *fakeBlob
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	box, err := secret.Load(filepath.Join(dir, "key"))
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dir, "test.db"), box)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	fake := docker.NewFake()
	blob := &fakeBlob{objects: map[string][]byte{}}
	m := NewManager(st, fake, dir)
	m.dial = func(core.Destination) (blobstore.Client, error) { return blob, nil }
	m.runDone = make(chan struct{}, 8)
	return &harness{m: m, store: st, docker: fake, blob: blob}
}

func (h *harness) destination(t *testing.T) core.Destination {
	t.Helper()
	d, err := h.store.CreateDestination(context.Background(), core.Destination{
		Name: "minio", Endpoint: "http://minio:9000", Bucket: "backups",
		AccessKey: "AK", SecretKey: "SK",
	})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// seedRunningDatabase creates a database row plus its running container.
func (h *harness) seedRunningDatabase(t *testing.T, name, engine string) core.Database {
	t.Helper()
	d := core.Database{Name: name, Engine: engine, Image: "img", Username: name, Password: "pw", DBName: name}
	d, err := h.store.CreateDatabase(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	id, err := h.docker.CreateContainer(context.Background(), docker.ContainerSpec{Name: "outhaul-db-" + name, Image: "img"})
	if err != nil {
		t.Fatal(err)
	}
	h.docker.StartContainer(context.Background(), id)
	return d
}

func (h *harness) backup(t *testing.T, kind string, targetID, destID int64, retention int) core.Backup {
	t.Helper()
	b, err := h.store.CreateBackup(context.Background(), core.Backup{
		TargetKind: kind, TargetID: targetID, DestinationID: destID,
		Schedule: "30 3 * * *", Prefix: "prod", Retention: retention,
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func (h *harness) waitRun(t *testing.T) core.BackupRun {
	t.Helper()
	select {
	case <-h.m.runDone:
	case <-time.After(10 * time.Second):
		t.Fatal("backup run did not finish")
	}
	return core.BackupRun{}
}

func (h *harness) lastRun(t *testing.T, backupID int64) core.BackupRun {
	t.Helper()
	runs, err := h.store.ListBackupRuns(context.Background(), backupID, 1)
	if err != nil || len(runs) == 0 {
		t.Fatalf("no runs recorded (err=%v)", err)
	}
	return runs[0]
}

func gunzip(t *testing.T, b []byte) string {
	t.Helper()
	r, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("not gzip: %v", err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func TestPostgresBackupUploadsDump(t *testing.T) {
	h := newHarness(t)
	dest := h.destination(t)
	db := h.seedRunningDatabase(t, "shop", core.EnginePostgres)
	b := h.backup(t, core.BackupTargetDatabase, db.ID, dest.ID, 0)

	h.docker.OnExec = func(id string, cmd, env []string) (string, string, int, error) {
		return "PGDMP-bytes", "", 0, nil
	}
	h.m.RunNow(b)
	h.waitRun(t)

	run := h.lastRun(t, b.ID)
	if run.Status != core.RunOK {
		t.Fatalf("run = %q (%s), want ok", run.Status, run.Reason)
	}
	keys := h.blob.keys()
	if len(keys) != 1 || !strings.HasPrefix(keys[0], "prod/shop/") || !strings.HasSuffix(keys[0], ".dump.gz") {
		t.Fatalf("keys = %v, want one prod/shop/<ts>.dump.gz", keys)
	}
	if got := gunzip(t, h.blob.objects[keys[0]]); got != "PGDMP-bytes" {
		t.Errorf("uploaded dump = %q", got)
	}
	if run.ObjectKey != keys[0] || run.SizeBytes == 0 {
		t.Errorf("run record = key %q size %d", run.ObjectKey, run.SizeBytes)
	}

	exec := h.docker.Execs[0]
	wantCmd := "pg_dump -Fc --no-acl --no-owner -U shop shop"
	if strings.Join(exec.Cmd, " ") != wantCmd {
		t.Errorf("cmd = %q, want %q", strings.Join(exec.Cmd, " "), wantCmd)
	}
	if len(exec.Env) != 1 || exec.Env[0] != "PGPASSWORD=pw" {
		t.Errorf("env = %v, want PGPASSWORD", exec.Env)
	}
}

func TestMySQLBackupCommand(t *testing.T) {
	h := newHarness(t)
	dest := h.destination(t)
	db := h.seedRunningDatabase(t, "erp", core.EngineMySQL)
	b := h.backup(t, core.BackupTargetDatabase, db.ID, dest.ID, 0)

	h.docker.OnExec = func(id string, cmd, env []string) (string, string, int, error) {
		return "-- MySQL dump", "", 0, nil
	}
	h.m.RunNow(b)
	h.waitRun(t)

	if run := h.lastRun(t, b.ID); run.Status != core.RunOK {
		t.Fatalf("run = %q (%s)", run.Status, run.Reason)
	}
	exec := h.docker.Execs[0]
	if strings.Join(exec.Cmd, " ") != "mysqldump --single-transaction --routines -uroot erp" {
		t.Errorf("cmd = %v", exec.Cmd)
	}
	if len(exec.Env) != 1 || exec.Env[0] != "MYSQL_PWD=pw" {
		t.Errorf("env = %v, want MYSQL_PWD", exec.Env)
	}
	if keys := h.blob.keys(); len(keys) != 1 || !strings.HasSuffix(keys[0], ".sql.gz") {
		t.Errorf("keys = %v, want one .sql.gz", keys)
	}
}

func TestDumpFailureRecordsStderr(t *testing.T) {
	h := newHarness(t)
	dest := h.destination(t)
	db := h.seedRunningDatabase(t, "shop", core.EnginePostgres)
	b := h.backup(t, core.BackupTargetDatabase, db.ID, dest.ID, 0)

	h.docker.OnExec = func(id string, cmd, env []string) (string, string, int, error) {
		return "", "connection to server failed", 1, nil
	}
	h.m.RunNow(b)
	h.waitRun(t)

	run := h.lastRun(t, b.ID)
	if run.Status != core.RunFailed {
		t.Fatalf("run = %q, want failed", run.Status)
	}
	if !strings.Contains(run.Reason, "exited 1") || !strings.Contains(run.Reason, "connection to server failed") {
		t.Errorf("reason = %q, want exit code and stderr", run.Reason)
	}
	if len(h.blob.keys()) != 0 {
		t.Errorf("failed dump still uploaded: %v", h.blob.keys())
	}
}

func TestRedisBackupRefused(t *testing.T) {
	h := newHarness(t)
	dest := h.destination(t)
	db := h.seedRunningDatabase(t, "cache", core.EngineRedis)
	b := h.backup(t, core.BackupTargetDatabase, db.ID, dest.ID, 0)

	h.m.RunNow(b)
	h.waitRun(t)
	if run := h.lastRun(t, b.ID); run.Status != core.RunFailed || !strings.Contains(run.Reason, "redis") {
		t.Errorf("run = %q (%q), want failed mentioning redis", run.Status, run.Reason)
	}
}

func TestStoppedDatabaseFails(t *testing.T) {
	h := newHarness(t)
	dest := h.destination(t)
	db := core.Database{Name: "shop", Engine: core.EnginePostgres, Image: "img", Username: "shop", Password: "pw", DBName: "shop"}
	db, err := h.store.CreateDatabase(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	b := h.backup(t, core.BackupTargetDatabase, db.ID, dest.ID, 0)

	h.m.RunNow(b)
	h.waitRun(t)
	if run := h.lastRun(t, b.ID); run.Status != core.RunFailed || !strings.Contains(run.Reason, "not running") {
		t.Errorf("run = %q (%q), want failed/not running", run.Status, run.Reason)
	}
}

func TestComposeVolumesUploadOnePerVolume(t *testing.T) {
	h := newHarness(t)
	dest := h.destination(t)
	app, err := h.store.CreateApp(context.Background(), core.App{
		Name: "wiki", RepoURL: "https://example.com/r.git", Kind: core.KindCompose,
	})
	if err != nil {
		t.Fatal(err)
	}
	h.docker.Volumes["outhaul-wiki_data"] = map[string]string{"com.docker.compose.project": "outhaul-wiki"}
	h.docker.Volumes["outhaul-wiki_uploads"] = map[string]string{"com.docker.compose.project": "outhaul-wiki"}
	h.docker.Volumes["unrelated"] = map[string]string{"com.docker.compose.project": "other"}
	h.docker.OnRun = func(spec docker.ContainerSpec) (string, int, error) {
		return "tarball-of-" + spec.Mounts[0].Source, 0, nil
	}
	b := h.backup(t, core.BackupTargetApp, app.ID, dest.ID, 0)

	h.m.RunNow(b)
	h.waitRun(t)

	run := h.lastRun(t, b.ID)
	if run.Status != core.RunOK {
		t.Fatalf("run = %q (%s)", run.Status, run.Reason)
	}
	keys := h.blob.keys()
	if len(keys) != 2 {
		t.Fatalf("keys = %v, want one per stack volume", keys)
	}
	for _, k := range keys {
		if !strings.HasPrefix(k, "prod/wiki/outhaul-wiki_") || !strings.HasSuffix(k, ".tar.gz") {
			t.Errorf("key %q not under prod/wiki/<volume>/", k)
		}
	}
	// The helper mounts the volume read-only and tars it; output is not
	// double-gzipped (RunContainer's stdout is uploaded as-is).
	spec := h.docker.Runs[0]
	if spec.Image != helperImage || !spec.Mounts[0].Volume || !spec.Mounts[0].ReadOnly {
		t.Errorf("helper spec = %+v", spec)
	}
	if strings.Join(spec.Cmd, " ") != "tar czf - -C /src ." {
		t.Errorf("helper cmd = %v", spec.Cmd)
	}
	var found bool
	for _, k := range keys {
		if strings.Contains(k, "_data") {
			found = string(h.blob.objects[k]) == "tarball-of-outhaul-wiki_data"
		}
	}
	if !found {
		t.Error("uploaded bytes are not the helper's stdout")
	}
	if len(h.docker.Pulled) == 0 || h.docker.Pulled[0] != helperImage {
		t.Errorf("helper image not pulled: %v", h.docker.Pulled)
	}
}

func TestSingleContainerVolumeBackedUp(t *testing.T) {
	h := newHarness(t)
	dest := h.destination(t)
	app, err := h.store.CreateApp(context.Background(), core.App{
		Name: "web", RepoURL: "https://example.com/web.git", Kind: core.KindNixpacks,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.AddVolume(context.Background(), app.ID, "/data"); err != nil {
		t.Fatalf("AddVolume: %v", err)
	}
	h.docker.Volumes["outhaul-web-data"] = core.VolumeLabels("web")
	h.docker.OnRun = func(spec docker.ContainerSpec) (string, int, error) {
		return "tarball-of-" + spec.Mounts[0].Source, 0, nil
	}
	b := h.backup(t, core.BackupTargetApp, app.ID, dest.ID, 0)

	h.m.RunNow(b)
	h.waitRun(t)

	run := h.lastRun(t, b.ID)
	if run.Status != core.RunOK {
		t.Fatalf("run = %q (%s)", run.Status, run.Reason)
	}
	keys := h.blob.keys()
	if len(keys) != 1 || !strings.HasPrefix(keys[0], "prod/web/outhaul-web-data/") || !strings.HasSuffix(keys[0], ".tar.gz") {
		t.Fatalf("keys = %v, want one prod/web/outhaul-web-data/<ts>.tar.gz", keys)
	}
	if got := string(h.blob.objects[keys[0]]); got != "tarball-of-outhaul-web-data" {
		t.Errorf("uploaded bytes = %q", got)
	}
	spec := h.docker.Runs[0]
	if spec.Image != helperImage || !spec.Mounts[0].Volume || !spec.Mounts[0].ReadOnly {
		t.Errorf("helper spec = %+v", spec)
	}
}

func TestAppBackupRejectsVolumelessApps(t *testing.T) {
	h := newHarness(t)
	dest := h.destination(t)

	// A nixpacks app with no volume (in the store or in Docker) still has
	// nothing to back up.
	nix, err := h.store.CreateApp(context.Background(), core.App{Name: "api", RepoURL: "r"})
	if err != nil {
		t.Fatal(err)
	}
	b := h.backup(t, core.BackupTargetApp, nix.ID, dest.ID, 0)
	h.m.RunNow(b)
	h.waitRun(t)
	if run := h.lastRun(t, b.ID); run.Status != core.RunFailed || !strings.Contains(run.Reason, "no named volumes") {
		t.Errorf("nixpacks run = %q (%q)", run.Status, run.Reason)
	}

	cmp, err := h.store.CreateApp(context.Background(), core.App{Name: "wiki", RepoURL: "r", Kind: core.KindCompose})
	if err != nil {
		t.Fatal(err)
	}
	b2 := h.backup(t, core.BackupTargetApp, cmp.ID, dest.ID, 0)
	h.m.RunNow(b2)
	h.waitRun(t)
	if run := h.lastRun(t, b2.ID); run.Status != core.RunFailed || !strings.Contains(run.Reason, "no named volumes") {
		t.Errorf("empty stack run = %q (%q)", run.Status, run.Reason)
	}
}

func TestRetentionPrunesOldest(t *testing.T) {
	h := newHarness(t)
	dest := h.destination(t)
	db := h.seedRunningDatabase(t, "shop", core.EnginePostgres)
	b := h.backup(t, core.BackupTargetDatabase, db.ID, dest.ID, 2)

	// Pre-existing older objects in the same directory.
	h.blob.objects["prod/shop/20200101T000000Z.dump.gz"] = []byte("old1")
	h.blob.objects["prod/shop/20210101T000000Z.dump.gz"] = []byte("old2")
	h.docker.OnExec = func(string, []string, []string) (string, string, int, error) { return "new", "", 0, nil }

	h.m.RunNow(b)
	h.waitRun(t)

	keys := h.blob.keys()
	if len(keys) != 2 {
		t.Fatalf("keys = %v, want newest 2 kept", keys)
	}
	for _, k := range keys {
		if k == "prod/shop/20200101T000000Z.dump.gz" {
			t.Error("oldest object survived pruning")
		}
	}
}

func TestTickRunsMatchingSchedulesOnly(t *testing.T) {
	h := newHarness(t)
	dest := h.destination(t)
	db := h.seedRunningDatabase(t, "shop", core.EnginePostgres)
	due := h.backup(t, core.BackupTargetDatabase, db.ID, dest.ID, 0) // 30 3 * * *
	notDue, err := h.store.CreateBackup(context.Background(), core.Backup{
		TargetKind: core.BackupTargetDatabase, TargetID: db.ID, DestinationID: dest.ID,
		Schedule: "0 12 * * *", Prefix: "other",
	})
	if err != nil {
		t.Fatal(err)
	}
	disabled, err := h.store.CreateBackup(context.Background(), core.Backup{
		TargetKind: core.BackupTargetDatabase, TargetID: db.ID, DestinationID: dest.ID,
		Schedule: "30 3 * * *", Prefix: "disabled",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.store.SetBackupEnabled(context.Background(), disabled.ID, false); err != nil {
		t.Fatal(err)
	}
	h.docker.OnExec = func(string, []string, []string) (string, string, int, error) { return "x", "", 0, nil }

	h.m.Tick(context.Background(), time.Date(2026, time.July, 4, 3, 30, 0, 0, time.Local))
	h.waitRun(t)

	if run := h.lastRun(t, due.ID); run.Status != core.RunOK {
		t.Errorf("due backup = %q (%s)", run.Status, run.Reason)
	}
	if runs, _ := h.store.ListBackupRuns(context.Background(), notDue.ID, 5); len(runs) != 0 {
		t.Error("not-due backup ran")
	}
	if runs, _ := h.store.ListBackupRuns(context.Background(), disabled.ID, 5); len(runs) != 0 {
		t.Error("disabled backup ran")
	}
}

func TestInFlightGuardSkipsOverlap(t *testing.T) {
	h := newHarness(t)
	dest := h.destination(t)
	db := h.seedRunningDatabase(t, "shop", core.EnginePostgres)
	b := h.backup(t, core.BackupTargetDatabase, db.ID, dest.ID, 0)

	block := make(chan struct{})
	h.docker.OnExec = func(string, []string, []string) (string, string, int, error) {
		<-block
		return "x", "", 0, nil
	}
	h.m.RunNow(b)
	h.m.RunNow(b) // overlaps; must be skipped
	close(block)
	h.waitRun(t)

	select {
	case <-h.m.runDone:
		t.Fatal("second overlapping run executed")
	case <-time.After(100 * time.Millisecond):
	}
	if runs, _ := h.store.ListBackupRuns(context.Background(), b.ID, 10); len(runs) != 1 {
		t.Errorf("runs = %d, want 1", len(runs))
	}
}
