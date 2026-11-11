package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/outhaul-dev/outhaul/internal/blobstore"
	"github.com/outhaul-dev/outhaul/internal/config"
	"github.com/outhaul-dev/outhaul/internal/core"
	"github.com/outhaul-dev/outhaul/internal/secret"
	"github.com/outhaul-dev/outhaul/internal/store"
)

type restoreSource struct {
	dbPath  string // the snapshot
	dbFile  string // the live database, closed and holding the session
	keyPath string
	key     []byte
}

func newRestoreSource(t *testing.T, sealed bool) *restoreSource {
	t.Helper()
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "key")
	box, err := secret.Load(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	dbFile := filepath.Join(dir, "source.db")
	st, err := store.Open(dbFile, box)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	u, err := st.CreateUser(ctx, "admin", "hash")
	if err != nil {
		t.Fatal(err)
	}
	app, err := st.CreateApp(ctx, core.App{Name: "web", RepoURL: "https://example.com/r.git", Domain: "web.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if sealed {
		if _, err := st.CreateDatabase(ctx, core.Database{Name: "db", Engine: core.EnginePostgres, Image: "img", Username: "u", Password: "pw", DBName: "db"}); err != nil {
			t.Fatal(err)
		}
		if err := st.SetEnv(ctx, app.ID, "K", "v", false); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.CreateSession(ctx, core.Session{
		Token: "super-secret-token", UserID: u.ID, ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(dir, "20261111T021500Z.db")
	if err := st.Snapshot(ctx, dbPath); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	return &restoreSource{dbPath: dbPath, dbFile: dbFile, keyPath: keyPath, key: raw}
}

func runRestoreArgs(t *testing.T, env map[string]string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	var out, errb bytes.Buffer
	code = runRestoreState(args, func(k string) string { return env[k] }, &out, &errb)
	return out.String(), errb.String(), code
}

func runRestore(t *testing.T, dir, from, keyFile string) (stdout, stderr string, code int) {
	t.Helper()
	args := []string{"--from", from}
	if keyFile != "" {
		args = append(args, "--key-file", keyFile)
	}
	return runRestoreArgs(t, map[string]string{"OUTHAUL_DATA_DIR": dir}, args...)
}

// azureEnv is a configuration that names the Azure destination and a prefix.
func azureEnv(dir string) map[string]string {
	return map[string]string{
		"OUTHAUL_DATA_DIR":               dir,
		"OUTHAUL_BACKUP_AZURE_ACCOUNT":   "examplebackups",
		"OUTHAUL_BACKUP_AZURE_CONTAINER": "outhaul",
		"OUTHAUL_STATE_BACKUP_SCHEDULE":  "15 2 * * *",
		"OUTHAUL_STATE_BACKUP_PREFIX":    "p",
	}
}

// stateBlob is an in-memory blobstore.Client for the Azure paths.
type stateBlob struct {
	objects map[string][]byte
}

func (b *stateBlob) Put(_ context.Context, key string, r io.Reader, _ int64) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	b.objects[key] = data
	return nil
}

func (b *stateBlob) Get(_ context.Context, key string) (io.ReadCloser, error) {
	data, ok := b.objects[key]
	if !ok {
		return nil, errors.New("404 Not Found")
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (b *stateBlob) List(_ context.Context, prefix string) ([]blobstore.Object, error) {
	var out []blobstore.Object
	for k, v := range b.objects {
		if strings.HasPrefix(k, prefix) {
			out = append(out, blobstore.Object{Key: k, Size: int64(len(v))})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

func (b *stateBlob) Delete(_ context.Context, key string) error {
	delete(b.objects, key)
	return nil
}

func useStateBlob(t *testing.T, b blobstore.Client) {
	t.Helper()
	old := openStateBlob
	openStateBlob = func(config.Config) (blobstore.Client, error) { return b, nil }
	t.Cleanup(func() { openStateBlob = old })
}

func copyForTest(t *testing.T, src, dst string) {
	t.Helper()
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func gzipForTest(t *testing.T, src string) string {
	t.Helper()
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	dst := src + ".gz"
	f, err := os.Create(dst)
	if err != nil {
		t.Fatal(err)
	}
	zw := gzip.NewWriter(f)
	if _, err := zw.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return dst
}

func openTarget(t *testing.T, dir, keyPath string) *store.Store {
	t.Helper()
	var box *secret.Box
	if keyPath != "" {
		raw, err := os.ReadFile(keyPath)
		if err != nil {
			t.Fatal(err)
		}
		box, err = secret.NewBox(raw)
		if err != nil {
			t.Fatal(err)
		}
	}
	st, err := store.Open(filepath.Join(dir, "outhaul.db"), box)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func globOne(t *testing.T, pattern string) string {
	t.Helper()
	matches, err := filepath.Glob(pattern)
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 {
		t.Fatalf("%s matched %v, want one", pattern, matches)
	}
	return matches[0]
}

func assertNoStaged(t *testing.T, dir string) {
	t.Helper()
	for _, p := range []string{"outhaul.db.restoring", "outhaul.db.restoring-wal", "outhaul.db.restoring-shm"} {
		if _, err := os.Stat(filepath.Join(dir, p)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s still exists after the refusal", p)
		}
	}
}

func TestParseRestoreOptions(t *testing.T) {
	opts, err := parseRestoreOptions([]string{"--from", "./copy.db.gz", "--key-file", "/k"})
	if err != nil || opts.From != "./copy.db.gz" || opts.KeyFile != "/k" {
		t.Fatalf("options = %+v, %v", opts, err)
	}
	if _, err := parseRestoreOptions(nil); err == nil {
		t.Error("no --from accepted, want an error")
	}
	if _, err := parseRestoreOptions([]string{"--from", "x", "extra"}); err == nil {
		t.Error("a positional argument accepted, want an error")
	}
	opts, err = parseRestoreOptions([]string{"--list"})
	if err != nil || !opts.List || opts.From != "" {
		t.Fatalf("--list options = %+v, %v", opts, err)
	}
	if _, err := parseRestoreOptions([]string{"--list", "--from", "x"}); err == nil {
		t.Error("--list with --from accepted, want an error")
	}
}

func TestRestoreStateList(t *testing.T) {
	src := newRestoreSource(t, true)
	gz, err := os.ReadFile(gzipForTest(t, src.dbPath))
	if err != nil {
		t.Fatal(err)
	}
	fake := &stateBlob{objects: map[string][]byte{
		"p/_outhaul-state/20261110T021500Z.db.gz": gz,
		"p/_outhaul-state/20261111T021500Z.db.gz": gz,
		"p/other/x.gz": {},
	}}
	useStateBlob(t, fake)
	out, errOut, code := runRestoreArgs(t, azureEnv(t.TempDir()), "--list")
	if code != 0 {
		t.Fatalf("--list = %d: %s", code, errOut)
	}
	if want := "20261110T021500Z\n20261111T021500Z\n"; out != want {
		t.Errorf("--list = %q, want %q", out, want)
	}
}

func TestRestoreStateLatest(t *testing.T) {
	src := newRestoreSource(t, true)
	gz, err := os.ReadFile(gzipForTest(t, src.dbPath))
	if err != nil {
		t.Fatal(err)
	}
	useStateBlob(t, &stateBlob{objects: map[string][]byte{
		"p/_outhaul-state/20261110T021500Z.db.gz": gz,
		"p/_outhaul-state/20261111T021500Z.db.gz": gz,
	}})
	dir := t.TempDir()
	_, errOut, code := runRestoreArgs(t, azureEnv(dir), "--from", "latest", "--key-file", src.keyPath)
	if code != 0 {
		t.Fatalf("restore latest = %d: %s", code, errOut)
	}
	st := openTarget(t, dir, src.keyPath)
	m, ok, err := st.RestoreMarker(context.Background())
	if err != nil || !ok || m.From != "20261111T021500Z" {
		t.Fatalf("marker = %+v, ok %v, err %v", m, ok, err)
	}
}

func TestRestoreStateStamp(t *testing.T) {
	src := newRestoreSource(t, true)
	gz, err := os.ReadFile(gzipForTest(t, src.dbPath))
	if err != nil {
		t.Fatal(err)
	}
	useStateBlob(t, &stateBlob{objects: map[string][]byte{
		"p/_outhaul-state/20261110T021500Z.db.gz": gz,
		"p/_outhaul-state/20261111T021500Z.db.gz": gz,
	}})
	dir := t.TempDir()
	_, errOut, code := runRestoreArgs(t, azureEnv(dir), "--from", "20261110T021500Z", "--key-file", src.keyPath)
	if code != 0 {
		t.Fatalf("restore stamp = %d: %s", code, errOut)
	}
	st := openTarget(t, dir, src.keyPath)
	m, ok, err := st.RestoreMarker(context.Background())
	if err != nil || !ok || m.From != "20261110T021500Z" {
		t.Fatalf("marker = %+v, ok %v, err %v", m, ok, err)
	}
}

func TestRestoreStateNeedsTheAzurePair(t *testing.T) {
	for _, args := range [][]string{{"--list"}, {"--from", "latest"}} {
		_, errOut, code := runRestoreArgs(t, map[string]string{"OUTHAUL_DATA_DIR": t.TempDir()}, args...)
		if code != 1 {
			t.Fatalf("%v = %d, want 1", args, code)
		}
		for _, want := range []string{"OUTHAUL_BACKUP_AZURE_ACCOUNT", "OUTHAUL_BACKUP_AZURE_CONTAINER", "pass a file"} {
			if !strings.Contains(errOut, want) {
				t.Errorf("%v error = %q, want %q", args, errOut, want)
			}
		}
	}
}

func TestRestoreStateNoCopies(t *testing.T) {
	useStateBlob(t, &stateBlob{objects: map[string][]byte{}})
	_, errOut, code := runRestoreArgs(t, azureEnv(t.TempDir()), "--from", "latest")
	if code != 1 || !strings.Contains(errOut, "no state copies") {
		t.Fatalf("empty container = %d, %q", code, errOut)
	}
}

func TestRestoreStateFromGzipAndSQLite(t *testing.T) {
	src := newRestoreSource(t, true)
	gz := gzipForTest(t, src.dbPath)
	for _, tc := range []struct{ name, from string }{
		{"gzip", gz},
		{"sqlite", src.dbPath},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			out, errOut, code := runRestore(t, dir, tc.from, src.keyPath)
			if code != 0 {
				t.Fatalf("restore = %d: %s", code, errOut)
			}
			if !strings.Contains(out, "20261111T021500Z") {
				t.Errorf("summary = %q, want the copy's stamp", out)
			}
			targetKey, err := os.ReadFile(filepath.Join(dir, "secret.key"))
			if err != nil || !bytes.Equal(targetKey, src.key) {
				t.Fatalf("target key = %x, %v; want the supplied key", targetKey, err)
			}
			st := openTarget(t, dir, src.keyPath)
			ctx := context.Background()
			if users, err := st.ListUsers(ctx); err != nil || len(users) != 1 {
				t.Errorf("users = %v (err %v), want 1", users, err)
			}
			if apps, err := st.ListApps(ctx); err != nil || len(apps) != 1 {
				t.Errorf("apps = %v (err %v), want 1", apps, err)
			}
			if _, err := st.GetSession(ctx, "super-secret-token"); !errors.Is(err, sql.ErrNoRows) {
				t.Errorf("session still present: %v", err)
			}
			m, ok, err := st.RestoreMarker(ctx)
			if err != nil || !ok || !m.Pending || m.From != "20261111T021500Z" {
				t.Errorf("marker = %+v, ok %v, err %v", m, ok, err)
			}
			after, err := os.ReadFile(src.keyPath)
			if err != nil || !bytes.Equal(after, src.key) {
				t.Errorf("the supplied key file changed")
			}
		})
	}
}

func TestRestoreStateStripsSessionsFromAPlainCopy(t *testing.T) {
	src := newRestoreSource(t, true)
	plain := filepath.Join(t.TempDir(), "plain.db")
	copyForTest(t, src.dbFile, plain)
	data, err := os.ReadFile(plain)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte("super-secret-token")) {
		t.Fatal("the plain copy has no session token to strip")
	}
	dir := t.TempDir()
	_, errOut, code := runRestore(t, dir, plain, src.keyPath)
	if code != 0 {
		t.Fatalf("restore = %d: %s", code, errOut)
	}
	st := openTarget(t, dir, src.keyPath)
	if _, err := st.GetSession(context.Background(), "super-secret-token"); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("session still present: %v", err)
	}
	target, err := os.ReadFile(filepath.Join(dir, "outhaul.db"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(target, []byte("super-secret-token")) {
		t.Error("the target still contains the session token's bytes")
	}
}

func TestRestoreStateRefusals(t *testing.T) {
	src := newRestoreSource(t, true)
	gz := gzipForTest(t, src.dbPath)

	text := filepath.Join(t.TempDir(), "text")
	if err := os.WriteFile(text, []byte("not a backup"), 0o600); err != nil {
		t.Fatal(err)
	}
	gzText := gzipForTest(t, text)

	noSchema := filepath.Join(t.TempDir(), "noschema.db")
	db, err := sql.Open("sqlite", "file:"+url.PathEscape(noSchema))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE t (x INTEGER)`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	newer := filepath.Join(t.TempDir(), "newer.db")
	copyForTest(t, src.dbPath, newer)
	db, err = sql.Open("sqlite", "file:"+url.PathEscape(newer))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO schema_migrations (name, applied_at) VALUES ('migrations/9999_future.sql', datetime('now'))`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	wrongKey := filepath.Join(t.TempDir(), "wrong.key")
	if err := os.WriteFile(wrongKey, bytes.Repeat([]byte{9}, 32), 0o600); err != nil {
		t.Fatal(err)
	}
	shortKey := filepath.Join(t.TempDir(), "short.key")
	if err := os.WriteFile(shortKey, []byte("short"), 0o600); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		from string
		key  string
		want string
	}{
		{"no data dir", src.dbPath, src.keyPath, "install Outhaul first"},
		{"not a backup", text, src.keyPath, "not an Outhaul state copy"},
		{"gzip of text", gzText, src.keyPath, "not an Outhaul state copy"},
		{"no schema", noSchema, src.keyPath, "not an Outhaul state copy"},
		{"newer", newer, src.keyPath, "newer Outhaul"},
		{"no key", gz, "", "pass the original with --key-file"},
		{"wrong key", gz, wrongKey, "The key the fresh install made for itself can't work"},
		{"short key", gz, shortKey, "The key the fresh install made for itself can't work"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.name == "no data dir" {
				dir = filepath.Join(dir, "missing")
			}
			_, errOut, code := runRestore(t, dir, tc.from, tc.key)
			if code == 0 {
				t.Fatal("restore succeeded, want a refusal")
			}
			if !strings.Contains(errOut, tc.want) {
				t.Errorf("error = %q, want %q", errOut, tc.want)
			}
			if tc.name != "no data dir" {
				assertNoStaged(t, dir)
			}
		})
	}
}

func TestRestoreStateRefusesARunningTarget(t *testing.T) {
	src := newRestoreSource(t, true)
	dir := t.TempDir()
	box, err := secret.Load(filepath.Join(dir, "secret.key"))
	if err != nil {
		t.Fatal(err)
	}
	target, err := store.Open(filepath.Join(dir, "outhaul.db"), box)
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	_, errOut, code := runRestore(t, dir, src.dbPath, src.keyPath)
	if code == 0 || !strings.Contains(errOut, "stop Outhaul first") {
		t.Fatalf("restore with an open target = %d, %q", code, errOut)
	}
	assertNoStaged(t, dir)
}

func TestRestoreStateRefusesANonEmptyTarget(t *testing.T) {
	src := newRestoreSource(t, true)
	dir := t.TempDir()
	box, err := secret.Load(filepath.Join(dir, "secret.key"))
	if err != nil {
		t.Fatal(err)
	}
	target, err := store.Open(filepath.Join(dir, "outhaul.db"), box)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := target.CreateUser(context.Background(), "admin", "hash"); err != nil {
		t.Fatal(err)
	}
	target.Close()
	_, errOut, code := runRestore(t, dir, src.dbPath, src.keyPath)
	if code == 0 || !strings.Contains(errOut, "fills an empty install only") {
		t.Fatalf("restore over an account = %d, %q", code, errOut)
	}
	assertNoStaged(t, dir)
}

func TestRestoreStateSwapsKeyAndDatabase(t *testing.T) {
	src := newRestoreSource(t, true)
	dir := t.TempDir()
	oldKeyPath := filepath.Join(dir, "secret.key")
	oldBox, err := secret.Load(oldKeyPath)
	if err != nil {
		t.Fatal(err)
	}
	oldRaw, err := os.ReadFile(oldKeyPath)
	if err != nil {
		t.Fatal(err)
	}
	old, err := store.Open(filepath.Join(dir, "outhaul.db"), oldBox)
	if err != nil {
		t.Fatal(err)
	}
	if err := old.SetSetting(context.Background(), "target", "yes"); err != nil {
		t.Fatal(err)
	}
	old.Close()

	_, errOut, code := runRestore(t, dir, src.dbPath, src.keyPath)
	if code != 0 {
		t.Fatalf("restore = %d: %s", code, errOut)
	}
	newKey, err := os.ReadFile(filepath.Join(dir, "secret.key"))
	if err != nil || !bytes.Equal(newKey, src.key) {
		t.Fatalf("new key = %x, %v; want the copy's", newKey, err)
	}
	movedKey := globOne(t, filepath.Join(dir, "secret.key.before-restore-*"))
	if moved, err := os.ReadFile(movedKey); err != nil || !bytes.Equal(moved, oldRaw) {
		t.Fatalf("moved key = %x, %v; want the old bytes", moved, err)
	}
	movedDB := globOne(t, filepath.Join(dir, "outhaul.db.before-restore-*"))
	moved, err := store.Open(movedDB, oldBox)
	if err != nil {
		t.Fatal(err)
	}
	defer moved.Close()
	if v, ok, err := moved.GetSetting(context.Background(), "target"); err != nil || !ok || v != "yes" {
		t.Fatalf("old database lost its row: %q, %v, %v", v, ok, err)
	}
	st := openTarget(t, dir, src.keyPath)
	if users, err := st.ListUsers(context.Background()); err != nil || len(users) != 1 {
		t.Fatalf("restored users = %v, %v", users, err)
	}
}

func TestRestoreStateMovesLeftoverWAL(t *testing.T) {
	src := newRestoreSource(t, true)
	dir := t.TempDir()
	box, err := secret.Load(filepath.Join(dir, "secret.key"))
	if err != nil {
		t.Fatal(err)
	}
	old, err := store.Open(filepath.Join(dir, "outhaul.db"), box)
	if err != nil {
		t.Fatal(err)
	}
	if err := old.SetSetting(context.Background(), "old", "yes"); err != nil {
		t.Fatal(err)
	}
	seed := t.TempDir()
	for _, side := range []string{"", "-wal", "-shm"} {
		srcPath := filepath.Join(dir, "outhaul.db"+side)
		if _, err := os.Stat(srcPath); err == nil {
			copyForTest(t, srcPath, filepath.Join(seed, "outhaul.db"+side))
		}
	}
	old.Close()
	for _, side := range []string{"", "-wal", "-shm"} {
		srcPath := filepath.Join(seed, "outhaul.db"+side)
		if _, err := os.Stat(srcPath); err == nil {
			copyForTest(t, srcPath, filepath.Join(dir, "outhaul.db"+side))
		}
	}

	_, errOut, code := runRestore(t, dir, src.dbPath, src.keyPath)
	if code != 0 {
		t.Fatalf("restore = %d: %s", code, errOut)
	}
	for _, side := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(filepath.Join(dir, "outhaul.db"+side)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s sits beside the new database", side)
		}
	}
	movedDB := globOne(t, filepath.Join(dir, "outhaul.db.before-restore-*"))
	moved, err := store.Open(movedDB, box)
	if err != nil {
		t.Fatal(err)
	}
	defer moved.Close()
	if v, ok, err := moved.GetSetting(context.Background(), "old"); err != nil || !ok || v != "yes" {
		t.Fatalf("old database lost its row: %q, %v, %v", v, ok, err)
	}
}

func TestRestoreStateClearsAStaleStagedWAL(t *testing.T) {
	src := newRestoreSource(t, true)
	dir := t.TempDir()
	box, err := secret.Load(filepath.Join(dir, "secret.key"))
	if err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(dir, "outhaul.db.restoring")
	st, err := store.Open(stale, box)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetSetting(context.Background(), "stale", "yes"); err != nil {
		t.Fatal(err)
	}
	seed := t.TempDir()
	for _, side := range []string{"", "-wal", "-shm"} {
		srcPath := stale + side
		if _, err := os.Stat(srcPath); err == nil {
			copyForTest(t, srcPath, filepath.Join(seed, "staged"+side))
		}
	}
	st.Close()
	for _, side := range []string{"", "-wal", "-shm"} {
		srcPath := filepath.Join(seed, "staged"+side)
		if _, err := os.Stat(srcPath); err == nil {
			copyForTest(t, srcPath, stale+side)
		}
	}

	_, errOut, code := runRestore(t, dir, src.dbPath, src.keyPath)
	if code != 0 {
		t.Fatalf("restore = %d: %s", code, errOut)
	}
	target := openTarget(t, dir, src.keyPath)
	if _, ok, err := target.GetSetting(context.Background(), "stale"); err != nil || ok {
		t.Fatalf("stale setting survived: ok %v, err %v", ok, err)
	}
	assertNoStaged(t, dir)
}

func TestRestoreStateRefusesASurvivingStagedWAL(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "staged.db")
	box, err := secret.Load(filepath.Join(dir, "key"))
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(path, box)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetSetting(context.Background(), "k", "v"); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+url.PathEscape(path)+"?_pragma=busy_timeout(0)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`INSERT INTO settings (key, value) VALUES ('held', 'yes')`); err != nil {
		t.Fatal(err)
	}
	err = closeStaged(st, path)
	if err == nil || !strings.Contains(err.Error(), "-wal") {
		t.Fatalf("closeStaged = %v, want an error naming the -wal", err)
	}
	tx.Rollback()
}

func TestRestoreStateMigratesAnOlderCopy(t *testing.T) {
	src := newRestoreSource(t, true)
	old := filepath.Join(t.TempDir(), "old.db")
	copyForTest(t, src.dbPath, old)
	db, err := sql.Open("sqlite", "file:"+url.PathEscape(old))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`ALTER TABLE destinations DROP COLUMN kind`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DELETE FROM schema_migrations WHERE name = 'migrations/0030_destination_kind.sql'`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	dir := t.TempDir()
	out, errOut, code := runRestore(t, dir, old, src.keyPath)
	if code != 0 {
		t.Fatalf("restore = %d: %s", code, errOut)
	}
	if !strings.Contains(out, "0030_destination_kind") {
		t.Errorf("summary = %q, want the migration name", out)
	}
	st := openTarget(t, dir, src.keyPath)
	rows, err := st.AppliedMigrations(context.Background())
	if err != nil || len(rows) == 0 || rows[len(rows)-1] != "migrations/0030_destination_kind.sql" {
		t.Fatalf("migrations = %v, %v", rows, err)
	}
}

func TestSwapInReportsWhatMoved(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Load(func(k string) string {
		if k == "OUTHAUL_DATA_DIR" {
			return dir
		}
		return ""
	})
	old := filepath.Join(dir, "outhaul.db")
	if err := os.WriteFile(old, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	moved, dbMoved, err := swapIn(cfg, filepath.Join(dir, "missing.db"), nil, "20261111T021500Z")
	if err == nil || !dbMoved {
		t.Fatalf("swapIn = moved %v, dbMoved %v, err %v; want a failure after moving the database", moved, dbMoved, err)
	}
	want := old + ".before-restore-20261111T021500Z"
	if len(moved) != 1 || moved[0] != want {
		t.Fatalf("moved = %v, want [%s]", moved, want)
	}
	msg := swapError(moved, dbMoved, err).Error()
	if !strings.Contains(msg, want) || !strings.Contains(msg, "the host has no outhaul.db") {
		t.Errorf("swapError = %q, want the moved path and the no-database warning", msg)
	}
}

func TestRestoreStateRefusesAFailedMigration(t *testing.T) {
	src := newRestoreSource(t, true)
	failed := filepath.Join(t.TempDir(), "failed.db")
	copyForTest(t, src.dbPath, failed)
	db, err := sql.Open("sqlite", "file:"+url.PathEscape(failed))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DELETE FROM schema_migrations WHERE name = 'migrations/0030_destination_kind.sql'`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	dir := t.TempDir()
	oldKeyPath := filepath.Join(dir, "secret.key")
	oldBox, err := secret.Load(oldKeyPath)
	if err != nil {
		t.Fatal(err)
	}
	oldRaw, err := os.ReadFile(oldKeyPath)
	if err != nil {
		t.Fatal(err)
	}
	old, err := store.Open(filepath.Join(dir, "outhaul.db"), oldBox)
	if err != nil {
		t.Fatal(err)
	}
	if err := old.SetSetting(context.Background(), "target", "yes"); err != nil {
		t.Fatal(err)
	}
	old.Close()

	_, errOut, code := runRestore(t, dir, failed, src.keyPath)
	if code == 0 || !strings.Contains(errOut, "duplicate column") {
		t.Fatalf("restore with a failing migration = %d, %q", code, errOut)
	}
	assertNoStaged(t, dir)
	if got, err := os.ReadFile(oldKeyPath); err != nil || !bytes.Equal(got, oldRaw) {
		t.Errorf("the target key changed: %x, %v", got, err)
	}
	target, err := store.Open(filepath.Join(dir, "outhaul.db"), oldBox)
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	if v, ok, err := target.GetSetting(context.Background(), "target"); err != nil || !ok || v != "yes" {
		t.Errorf("the target database changed: %q, %v, %v", v, ok, err)
	}
}

func TestRestoreStateAcceptsACopyWithNothingSealedWithoutAKey(t *testing.T) {
	src := newRestoreSource(t, false)
	t.Run("no key", func(t *testing.T) {
		dir := t.TempDir()
		out, errOut, code := runRestore(t, dir, src.dbPath, "")
		if code != 0 {
			t.Fatalf("restore = %d: %s", code, errOut)
		}
		if !strings.Contains(out, "sealed values opened: 0") {
			t.Errorf("summary = %q, want 0 sealed values", out)
		}
		if _, err := os.Stat(filepath.Join(dir, "secret.key")); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("a key was written for an unsealed copy")
		}
		st := openTarget(t, dir, "")
		if users, err := st.ListUsers(context.Background()); err != nil || len(users) != 1 {
			t.Fatalf("users = %v, %v", users, err)
		}
	})
	t.Run("wrong key", func(t *testing.T) {
		dir := t.TempDir()
		wrong := filepath.Join(t.TempDir(), "wrong.key")
		if err := os.WriteFile(wrong, bytes.Repeat([]byte{9}, 32), 0o600); err != nil {
			t.Fatal(err)
		}
		_, errOut, code := runRestore(t, dir, src.dbPath, wrong)
		if code != 0 {
			t.Fatalf("restore with an unused wrong key = %d: %s", code, errOut)
		}
		st := openTarget(t, dir, wrong)
		if users, err := st.ListUsers(context.Background()); err != nil || len(users) != 1 {
			t.Fatalf("users = %v, %v", users, err)
		}
	})
}
