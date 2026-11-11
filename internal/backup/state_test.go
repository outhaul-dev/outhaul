package backup

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/outhaul-dev/outhaul/internal/blobstore"
	"github.com/outhaul-dev/outhaul/internal/core"
	"github.com/outhaul-dev/outhaul/internal/store"
)

const statePrefix = "prod/_outhaul-state/"

func stateHarness(t *testing.T, withUser bool) *harness {
	t.Helper()
	h := newHarness(t)
	if withUser {
		if _, err := h.store.CreateUser(context.Background(), "admin", "hash"); err != nil {
			t.Fatal(err)
		}
	}
	return h
}

func TestStateBackupRun(t *testing.T) {
	h := stateHarness(t, true)
	ctx := context.Background()
	app, err := h.store.CreateApp(ctx, core.App{Name: "web", RepoURL: "https://example.com/r.git", Domain: "web.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	u, err := h.store.GetUserByName(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.store.CreateSession(ctx, core.Session{
		Token: "super-secret-token", UserID: u.ID, ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	h.destination(t)
	h.m.SetStateBackup(StateBackup{Schedule: "15 2 * * *", Destination: "minio", Keep: 2, Prefix: "prod"})
	h.m.RunStateNow()
	h.waitRun(t)

	keys := h.blob.keys()
	if len(keys) != 1 || !strings.HasPrefix(keys[0], statePrefix) || !strings.HasSuffix(keys[0], ".db.gz") {
		t.Fatalf("keys = %v, want one %s<stamp>.db.gz", keys, statePrefix)
	}
	data := h.blob.objects[keys[0]]
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	plain, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "state.db")
	if err := os.WriteFile(path, plain, 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(path, h.box)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	users, err := st.ListUsers(ctx)
	if err != nil || len(users) != 1 {
		t.Fatalf("users = %v (err %v), want 1", users, err)
	}
	apps, err := st.ListApps(ctx)
	if err != nil || len(apps) != 1 || apps[0].Name != app.Name {
		t.Fatalf("apps = %v (err %v), want web", apps, err)
	}
	if _, err := st.GetSession(ctx, "super-secret-token"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("session still in the copy: %v", err)
	}

	outcome, ok, err := h.store.StateBackupRun(ctx)
	if err != nil || !ok {
		t.Fatalf("outcome = ok %v, err %v", ok, err)
	}
	if !outcome.OK || outcome.Key != keys[0] || outcome.SizeBytes != int64(len(data)) {
		t.Errorf("outcome = %+v, want ok, key %s, size %d", outcome, keys[0], len(data))
	}
	if matches, _ := filepath.Glob(filepath.Join(h.m.workDir, "backup-state-*")); len(matches) != 0 {
		t.Errorf("snapshot files left in workDir: %v", matches)
	}
}

func TestStateBackupSkipsWithoutAnAccount(t *testing.T) {
	h := stateHarness(t, false)
	var logs bytes.Buffer
	old := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(old)

	h.m.SetStateBackup(StateBackup{Schedule: "15 2 * * *", Destination: "minio", Keep: 2, Prefix: "prod"})
	h.m.RunStateNow()
	h.waitRun(t)

	if keys := h.blob.keys(); len(keys) != 0 {
		t.Errorf("uploaded %v, want nothing", keys)
	}
	if _, ok, err := h.store.StateBackupRun(context.Background()); ok || err != nil {
		t.Errorf("outcome = ok %v, err %v, want none", ok, err)
	}
	if !strings.Contains(logs.String(), "skipped: no account yet") {
		t.Errorf("log = %q, want the skip line", logs.String())
	}
}

func TestStateBackupRetention(t *testing.T) {
	h := stateHarness(t, true)
	for _, stamp := range []string{"20260101T021500Z", "20260102T021500Z", "20260103T021500Z"} {
		h.blob.objects[statePrefix+stamp+".db.gz"] = []byte("old")
	}
	h.destination(t)
	h.m.SetStateBackup(StateBackup{Schedule: "15 2 * * *", Destination: "minio", Keep: 1, Prefix: "prod"})
	h.m.RunStateNow()
	h.waitRun(t)

	keys := h.blob.keys()
	if len(keys) != 1 || !strings.HasPrefix(keys[0], statePrefix) {
		t.Fatalf("keys = %v, want only the new copy", keys)
	}
	for k := range h.blob.objects {
		if strings.Contains(k, "2026010") {
			t.Errorf("old copy %q survived retention 1", k)
		}
	}
}

func TestStateBackupUsesTheAzureRow(t *testing.T) {
	h := stateHarness(t, true)
	ctx := context.Background()
	if err := h.store.SyncAzureDestination(ctx, "examplebackups", "outhaul"); err != nil {
		t.Fatal(err)
	}
	var kinds []string
	h.m.dial = func(d core.Destination) (blobstore.Client, error) {
		kinds = append(kinds, d.Kind)
		return h.blob, nil
	}
	h.m.SetStateBackup(StateBackup{Schedule: "15 2 * * *", Keep: 1, Prefix: "prod"})
	h.m.RunStateNow()
	h.waitRun(t)

	if len(kinds) != 1 || kinds[0] != core.DestinationAzure {
		t.Fatalf("dial saw %v, want azure", kinds)
	}
}

func TestStateBackupMissingDestination(t *testing.T) {
	h := stateHarness(t, true)
	h.m.SetStateBackup(StateBackup{Schedule: "15 2 * * *", Destination: "nope", Keep: 1, Prefix: "prod"})
	h.m.RunStateNow()
	h.waitRun(t)

	outcome, ok, err := h.store.StateBackupRun(context.Background())
	if err != nil || !ok {
		t.Fatalf("outcome = ok %v, err %v", ok, err)
	}
	if outcome.OK || !strings.Contains(outcome.Reason, "nope") {
		t.Errorf("outcome = %+v, want a failure naming nope", outcome)
	}
	if keys := h.blob.keys(); len(keys) != 0 {
		t.Errorf("uploaded %v, want nothing", keys)
	}
}

func TestStateBackupTickHonoursHold(t *testing.T) {
	h := stateHarness(t, true)
	h.destination(t)
	h.m.SetStateBackup(StateBackup{Schedule: "15 2 * * *", Destination: "minio", Keep: 1, Prefix: "prod"})
	h.m.SetHold(func() bool { return true })
	h.m.Tick(context.Background(), time.Date(2026, 11, 11, 2, 15, 0, 0, time.Local))

	select {
	case <-h.m.runDone:
		t.Fatal("a held tick started a state backup")
	case <-time.After(50 * time.Millisecond):
	}
	if keys := h.blob.keys(); len(keys) != 0 {
		t.Errorf("uploaded %v, want nothing", keys)
	}
}

func TestStateBackupStopCancels(t *testing.T) {
	h := stateHarness(t, true)
	h.destination(t)
	b := &blockingBlob{started: make(chan struct{})}
	h.m.dial = func(core.Destination) (blobstore.Client, error) { return b, nil }
	h.m.SetStateBackup(StateBackup{Schedule: "15 2 * * *", Destination: "minio", Keep: 1, Prefix: "prod"})
	h.m.RunStateNow()
	select {
	case <-b.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the run never reached the upload")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.m.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	outcome, ok, err := h.store.StateBackupRun(context.Background())
	if err != nil || !ok {
		t.Fatalf("outcome = ok %v, err %v", ok, err)
	}
	if outcome.OK || !strings.Contains(outcome.Reason, "interrupted") {
		t.Errorf("outcome = %+v, want an interrupted failure", outcome)
	}
}

func TestStateBackupDoesNotOverlap(t *testing.T) {
	h := stateHarness(t, true)
	h.destination(t)
	b := &gateBlob{fakeBlob: h.blob, started: make(chan struct{}), release: make(chan struct{})}
	h.m.dial = func(core.Destination) (blobstore.Client, error) { return b, nil }
	h.m.SetStateBackup(StateBackup{Schedule: "15 2 * * *", Destination: "minio", Keep: 1, Prefix: "prod"})

	h.m.RunStateNow()
	select {
	case <-b.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the first run never reached the upload")
	}
	h.m.RunStateNow()
	time.Sleep(50 * time.Millisecond)
	if got := atomic.LoadInt32(&b.calls); got != 1 {
		t.Errorf("Put calls = %d, want 1 while the first is in flight", got)
	}
	close(b.release)
	h.waitRun(t)
	if got := atomic.LoadInt32(&b.calls); got != 1 {
		t.Errorf("Put calls = %d, want 1", got)
	}
}

type blockingBlob struct {
	started chan struct{}
}

func (b *blockingBlob) Put(ctx context.Context, key string, r io.Reader, size int64) error {
	close(b.started)
	<-ctx.Done()
	return ctx.Err()
}
func (b *blockingBlob) Get(context.Context, string) (io.ReadCloser, error) {
	return nil, errors.New("not implemented")
}
func (b *blockingBlob) List(context.Context, string) ([]blobstore.Object, error) {
	return nil, errors.New("not implemented")
}
func (b *blockingBlob) Delete(context.Context, string) error { return errors.New("not implemented") }

type gateBlob struct {
	*fakeBlob
	started chan struct{}
	release chan struct{}
	calls   int32
}

func (b *gateBlob) Put(ctx context.Context, key string, r io.Reader, size int64) error {
	if atomic.AddInt32(&b.calls, 1) == 1 {
		close(b.started)
	}
	<-b.release
	return b.fakeBlob.Put(ctx, key, r, size)
}
