// Package backup runs scheduled backups to S3-compatible storage: database
// dumps (pg_dump/mysqldump executed inside the database container) and
// compose-stack named volumes (tarred by a transient helper container). It is
// the backups counterpart of the deploy worker — a minute ticker evaluates
// each enabled backup's cron expression and due backups run in goroutines.
package backup

import (
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/james-smart/outhaul/internal/blobstore"
	"github.com/james-smart/outhaul/internal/compose"
	"github.com/james-smart/outhaul/internal/core"
	"github.com/james-smart/outhaul/internal/cron"
	"github.com/james-smart/outhaul/internal/dbaas"
	"github.com/james-smart/outhaul/internal/docker"
	"github.com/james-smart/outhaul/internal/store"
)

// helperImage tars volumes; busybox ships tar+gzip in ~2 MB.
const helperImage = "busybox:stable"

// runTimeout bounds one backup run end to end (dump + upload + prune).
const runTimeout = 2 * time.Hour

// Manager owns backup execution. All Docker and object-storage access for
// backups goes through it; the server talks to it through a small interface.
type Manager struct {
	store   *store.Store
	docker  docker.Client
	workDir string // scratch space for staged archives

	// dial opens a blobstore for a destination; a test seam defaulting to
	// blobstore.Open.
	dial func(core.Destination) (blobstore.Client, error)

	mu       sync.Mutex
	inFlight map[int64]bool // backup IDs currently running

	// runDone, when non-nil, is signalled after each run finishes (tests use
	// it to wait deterministically).
	runDone chan struct{}
}

// NewManager wires a Manager. workDir is config.WorkDir().
func NewManager(st *store.Store, dc docker.Client, workDir string) *Manager {
	return &Manager{
		store:    st,
		docker:   dc,
		workDir:  workDir,
		dial:     blobstore.Open,
		inFlight: map[int64]bool{},
	}
}

// Run ticks once a minute until ctx is cancelled, starting whichever enabled
// backups' schedules match the current minute. Minutes that pass while the
// binary is down are skipped — same contract as any cron.
func (m *Manager) Run(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			m.Tick(ctx, now)
		}
	}
}

// Tick starts every enabled backup whose schedule matches now (local time).
func (m *Manager) Tick(ctx context.Context, now time.Time) {
	backups, err := m.store.ListEnabledBackups(ctx)
	if err != nil {
		log.Printf("backup: list schedules: %v", err)
		return
	}
	for _, b := range backups {
		s, err := cron.Parse(b.Schedule)
		if err != nil {
			log.Printf("backup %d: bad schedule %q: %v", b.ID, b.Schedule, err)
			continue
		}
		if s.Matches(now) {
			m.RunNow(b)
		}
	}
}

// RunNow executes a backup in the background, skipping silently if the same
// backup is already in flight (a long run overlapping its next tick, or a
// restore from the same schedule).
func (m *Manager) RunNow(b core.Backup) {
	m.launch(b.ID, func(ctx context.Context) { m.run(ctx, b) })
}

// launch runs job in a goroutine under the per-schedule in-flight guard,
// bounded by runTimeout. Backups and restores of the same schedule share the
// guard so they can never overlap each other.
func (m *Manager) launch(backupID int64, job func(ctx context.Context)) {
	m.mu.Lock()
	if m.inFlight[backupID] {
		m.mu.Unlock()
		return
	}
	m.inFlight[backupID] = true
	m.mu.Unlock()

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), runTimeout)
		defer cancel()
		job(ctx)
		m.mu.Lock()
		delete(m.inFlight, backupID)
		m.mu.Unlock()
		if m.runDone != nil {
			m.runDone <- struct{}{}
		}
	}()
}

// TestDestination verifies a destination is writable (used by the settings
// page's Test button).
func (m *Manager) TestDestination(ctx context.Context, d core.Destination) error {
	c, err := m.dial(d)
	if err != nil {
		return err
	}
	return blobstore.Probe(ctx, c)
}

// run executes one backup attempt and records its outcome.
func (m *Manager) run(ctx context.Context, b core.Backup) {
	runID, err := m.store.StartBackupRun(ctx, b.ID, core.RunKindBackup)
	if err != nil {
		log.Printf("backup %d: record run: %v", b.ID, err)
		return
	}
	key, size, err := m.execute(ctx, b)
	if err != nil {
		if ferr := m.store.FinishBackupRun(ctx, runID, core.RunFailed, err.Error(), key, size); ferr != nil {
			log.Printf("backup %d: record failure: %v (original: %v)", b.ID, ferr, err)
		}
		return
	}
	if err := m.store.FinishBackupRun(ctx, runID, core.RunOK, "", key, size); err != nil {
		log.Printf("backup %d: record success: %v", b.ID, err)
	}
}

// execute resolves the target and produces + uploads its archive(s),
// returning the (last) object key and total bytes uploaded.
func (m *Manager) execute(ctx context.Context, b core.Backup) (string, int64, error) {
	dest, err := m.store.GetDestination(ctx, b.DestinationID)
	if err != nil {
		return "", 0, fmt.Errorf("load destination: %w", err)
	}
	blob, err := m.dial(dest)
	if err != nil {
		return "", 0, err
	}
	switch b.TargetKind {
	case core.BackupTargetDatabase:
		return m.backupDatabase(ctx, b, blob)
	case core.BackupTargetApp:
		return m.backupAppVolumes(ctx, b, blob)
	default:
		return "", 0, fmt.Errorf("unknown backup target kind %q", b.TargetKind)
	}
}

// stamp names archives; UTC and filename-safe, and lexicographic order is
// chronological order (retention pruning relies on that).
func stamp(t time.Time) string { return t.UTC().Format("20060102T150405Z") }

// joinKey builds an object key from a user prefix and parts.
func joinKey(prefix string, parts ...string) string {
	return path.Join(append([]string{strings.Trim(prefix, "/")}, parts...)...)
}

// backupDatabase dumps a managed database with its engine's native tool,
// executed inside the database container (the tools ship in the official
// images), gzipped and staged locally, then uploaded.
func (m *Manager) backupDatabase(ctx context.Context, b core.Backup, blob blobstore.Client) (string, int64, error) {
	db, err := m.store.GetDatabase(ctx, b.TargetID)
	if err != nil {
		return "", 0, fmt.Errorf("load database: %w", err)
	}
	cmd, env, ext, err := dumpCommand(db)
	if err != nil {
		return "", 0, err
	}
	c, err := m.docker.FindContainer(ctx, dbaas.ContainerName(db.Name))
	if err != nil {
		return "", 0, err
	}
	if c == nil || !c.Running() {
		return "", 0, fmt.Errorf("database container is not running")
	}

	f, err := m.stage(true, func(w io.Writer) error {
		var stderr strings.Builder
		exit, err := m.docker.ExecContainer(ctx, c.ID, cmd, env, nil, w, limitWriter(&stderr))
		if err != nil {
			return fmt.Errorf("%s: %w", cmd[0], err)
		}
		if exit != 0 {
			return fmt.Errorf("%s exited %d: %s", cmd[0], exit, strings.TrimSpace(stderr.String()))
		}
		return nil
	})
	if err != nil {
		return "", 0, err
	}
	defer f.close()

	key := joinKey(b.Prefix, db.Name, stamp(time.Now())+ext)
	if err := blob.Put(ctx, key, f.file, f.size); err != nil {
		return key, 0, err
	}
	if err := m.prune(ctx, blob, joinKey(b.Prefix, db.Name)+"/", b.Retention); err != nil {
		return key, f.size, fmt.Errorf("uploaded, but pruning old backups failed: %w", err)
	}
	return key, f.size, nil
}

// dumpCommand picks the engine's dump tool (Dokploy's commands, run in-place).
func dumpCommand(db core.Database) (cmd, env []string, ext string, err error) {
	switch db.Engine {
	case core.EnginePostgres:
		return []string{"pg_dump", "-Fc", "--no-acl", "--no-owner", "-U", db.Username, db.DBName},
			[]string{"PGPASSWORD=" + db.Password}, ".dump.gz", nil
	case core.EngineMySQL:
		return []string{"mysqldump", "--single-transaction", "--routines", "-uroot", db.DBName},
			[]string{"MYSQL_PWD=" + db.Password}, ".sql.gz", nil
	default:
		return nil, nil, "", fmt.Errorf("engine %q has no dump tool (redis is cache-shaped; back up what feeds it)", db.Engine)
	}
}

// backupAppVolumes tars each of an app's named volumes — a compose stack's
// project volumes or a single-container app's labeled data volumes — with a
// transient helper container and uploads one archive per volume.
func (m *Manager) backupAppVolumes(ctx context.Context, b core.Backup, blob blobstore.Client) (string, int64, error) {
	app, err := m.store.GetApp(ctx, b.TargetID)
	if err != nil {
		return "", 0, fmt.Errorf("load app: %w", err)
	}
	var vols []string
	if app.Kind == core.KindCompose {
		vols, err = m.docker.ListVolumes(ctx,
			map[string]string{"com.docker.compose.project": compose.ProjectName(app.Name)})
	} else {
		vols, err = m.docker.ListVolumes(ctx, core.VolumeLabels(app.Name))
	}
	if err != nil {
		return "", 0, err
	}
	if len(vols) == 0 {
		return "", 0, fmt.Errorf("app %q has no named volumes to back up", app.Name)
	}
	if err := m.docker.PullImage(ctx, helperImage, io.Discard); err != nil {
		return "", 0, fmt.Errorf("pull %s: %w", helperImage, err)
	}

	ts := stamp(time.Now())
	var lastKey string
	var total int64
	for i, vol := range vols {
		f, err := m.stage(false, func(w io.Writer) error {
			var stderr strings.Builder
			exit, err := m.docker.RunContainer(ctx, docker.ContainerSpec{
				Name:  fmt.Sprintf("outhaul-backup-%d-%d", b.ID, i),
				Image: helperImage,
				Cmd:   []string{"tar", "czf", "-", "-C", "/src", "."},
				Labels: map[string]string{
					"outhaul.managed": "true",
					"outhaul.role":    "backup",
				},
				Mounts: []docker.Mount{{Source: vol, Target: "/src", ReadOnly: true, Volume: true}},
			}, w, limitWriter(&stderr))
			if err != nil {
				return fmt.Errorf("tar %s: %w", vol, err)
			}
			if exit != 0 {
				return fmt.Errorf("tar %s exited %d: %s", vol, exit, strings.TrimSpace(stderr.String()))
			}
			return nil
		})
		if err != nil {
			return lastKey, total, err
		}
		key := joinKey(b.Prefix, app.Name, vol, ts+".tar.gz")
		err = blob.Put(ctx, key, f.file, f.size)
		size := f.size
		f.close()
		if err != nil {
			return key, total, err
		}
		lastKey, total = key, total+size
		if err := m.prune(ctx, blob, joinKey(b.Prefix, app.Name, vol)+"/", b.Retention); err != nil {
			return key, total, fmt.Errorf("uploaded, but pruning old backups failed: %w", err)
		}
	}
	return lastKey, total, nil
}

// prune keeps the newest retention objects under dir (keys sort
// chronologically because of stamp); 0 keeps everything.
func (m *Manager) prune(ctx context.Context, blob blobstore.Client, dir string, retention int) error {
	if retention <= 0 {
		return nil
	}
	objs, err := blob.List(ctx, dir)
	if err != nil {
		return err
	}
	for i := 0; i < len(objs)-retention; i++ {
		if err := blob.Delete(ctx, objs[i].Key); err != nil {
			return err
		}
	}
	return nil
}

// staged is a temp file holding one finished archive, ready to upload.
type staged struct {
	file *os.File
	size int64
}

func (s *staged) close() {
	s.file.Close()
	os.Remove(s.file.Name())
}

// stage writes an archive through produce into a temp file and rewinds it
// for upload. gzipOutput wraps the producer's writer in gzip — used for
// database dumps; the volume tar helper compresses itself (tar czf).
func (m *Manager) stage(gzipOutput bool, produce func(io.Writer) error) (*staged, error) {
	f, err := os.CreateTemp(m.workDir, "backup-*")
	if err != nil {
		return nil, fmt.Errorf("stage backup: %w", err)
	}
	cleanup := func() {
		f.Close()
		os.Remove(f.Name())
	}
	var w io.Writer = f
	var gz *gzip.Writer
	if gzipOutput {
		gz = gzip.NewWriter(f)
		w = gz
	}
	if err := produce(w); err != nil {
		cleanup()
		return nil, err
	}
	if gz != nil {
		if err := gz.Close(); err != nil {
			cleanup()
			return nil, err
		}
	}
	info, err := f.Stat()
	if err != nil {
		cleanup()
		return nil, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		cleanup()
		return nil, err
	}
	return &staged{file: f, size: info.Size()}, nil
}

// limitWriter caps captured stderr so a chatty tool can't balloon memory.
func limitWriter(b *strings.Builder) io.Writer { return &cappedWriter{b: b, remaining: 4096} }

type cappedWriter struct {
	b         *strings.Builder
	remaining int
}

func (c *cappedWriter) Write(p []byte) (int, error) {
	n := len(p)
	if c.remaining > 0 {
		take := min(len(p), c.remaining)
		c.b.Write(p[:take])
		c.remaining -= take
	}
	return n, nil
}
