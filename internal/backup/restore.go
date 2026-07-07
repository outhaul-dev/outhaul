package backup

import (
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"log"
	"slices"
	"strings"
	"time"

	"github.com/outhaul-dev/outhaul/internal/blobstore"
	"github.com/outhaul-dev/outhaul/internal/compose"
	"github.com/outhaul-dev/outhaul/internal/core"
	"github.com/outhaul-dev/outhaul/internal/dbaas"
	"github.com/outhaul-dev/outhaul/internal/docker"
)

// stopTimeout bounds each container stop while a volume is restored.
const stopTimeout = 30 * time.Second

// RestoreNow restores the archive at objectKey back into b's target in the
// background. It shares the backup's per-schedule in-flight guard, so a
// restore never overlaps the same schedule's backup or another restore.
func (m *Manager) RestoreNow(b core.Backup, objectKey string) {
	m.launch(b.ID, func(ctx context.Context) { m.restoreRun(ctx, b, objectKey) })
}

// ListRestoreObjects lists the archives schedule b can restore from — the
// objects under its own directory — newest first.
func (m *Manager) ListRestoreObjects(ctx context.Context, b core.Backup) ([]blobstore.Object, error) {
	dest, err := m.store.GetDestination(ctx, b.DestinationID)
	if err != nil {
		return nil, fmt.Errorf("load destination: %w", err)
	}
	blob, err := m.dial(dest)
	if err != nil {
		return nil, err
	}
	dir, err := m.RestoreDir(ctx, b)
	if err != nil {
		return nil, err
	}
	objs, err := blob.List(ctx, dir+"/")
	if err != nil {
		return nil, err
	}
	// Keys sort chronologically (the stamp guarantees it); newest first reads
	// better on a restore page.
	slices.Reverse(objs)
	return objs, nil
}

// RestoreDir is the object directory schedule b writes into —
// <prefix>/<target-name> — and therefore the only place it restores from.
func (m *Manager) RestoreDir(ctx context.Context, b core.Backup) (string, error) {
	switch b.TargetKind {
	case core.BackupTargetDatabase:
		db, err := m.store.GetDatabase(ctx, b.TargetID)
		if err != nil {
			return "", fmt.Errorf("load database: %w", err)
		}
		return joinKey(b.Prefix, db.Name), nil
	case core.BackupTargetApp:
		app, err := m.store.GetApp(ctx, b.TargetID)
		if err != nil {
			return "", fmt.Errorf("load app: %w", err)
		}
		return joinKey(b.Prefix, app.Name), nil
	default:
		return "", fmt.Errorf("unknown backup target kind %q", b.TargetKind)
	}
}

// restoreRun executes one restore attempt and records its outcome in the
// shared run history.
func (m *Manager) restoreRun(ctx context.Context, b core.Backup, key string) {
	runID, err := m.store.StartBackupRun(ctx, b.ID, core.RunKindRestore)
	if err != nil {
		log.Printf("restore %d: record run: %v", b.ID, err)
		return
	}
	size, err := m.restore(ctx, b, key)
	if err != nil {
		if ferr := m.store.FinishBackupRun(ctx, runID, core.RunFailed, err.Error(), key, size); ferr != nil {
			log.Printf("restore %d: record failure: %v (original: %v)", b.ID, ferr, err)
		}
		return
	}
	if err := m.store.FinishBackupRun(ctx, runID, core.RunOK, "", key, size); err != nil {
		log.Printf("restore %d: record success: %v", b.ID, err)
	}
}

// restore brings the archive at key back into b's target, returning the bytes
// downloaded.
func (m *Manager) restore(ctx context.Context, b core.Backup, key string) (int64, error) {
	dir, err := m.RestoreDir(ctx, b)
	if err != nil {
		return 0, err
	}
	// The server validates this too; re-check here so RestoreNow can never be
	// pointed outside the schedule's own directory.
	rel, ok := strings.CutPrefix(key, dir+"/")
	if !ok || rel == "" {
		return 0, fmt.Errorf("archive %q is not under this backup's directory %s/", key, dir)
	}
	dest, err := m.store.GetDestination(ctx, b.DestinationID)
	if err != nil {
		return 0, fmt.Errorf("load destination: %w", err)
	}
	blob, err := m.dial(dest)
	if err != nil {
		return 0, err
	}
	switch b.TargetKind {
	case core.BackupTargetDatabase:
		return m.restoreDatabase(ctx, b, blob, key)
	case core.BackupTargetApp:
		return m.restoreVolume(ctx, b, blob, key, rel)
	default:
		return 0, fmt.Errorf("unknown backup target kind %q", b.TargetKind)
	}
}

// download stages the object at key to a local temp file, so a network
// failure aborts the restore before anything is touched.
func (m *Manager) download(ctx context.Context, blob blobstore.Client, key string) (*staged, error) {
	rc, err := blob.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return m.stage(false, func(w io.Writer) error {
		_, err := io.Copy(w, rc)
		return err
	})
}

// restoreDatabase streams a staged dump into the engine's restore tool inside
// the running database container — Dokploy's `docker exec -i` pipeline
// without the rclone.
func (m *Manager) restoreDatabase(ctx context.Context, b core.Backup, blob blobstore.Client, key string) (int64, error) {
	db, err := m.store.GetDatabase(ctx, b.TargetID)
	if err != nil {
		return 0, fmt.Errorf("load database: %w", err)
	}
	cmd, env, err := restoreCommand(db)
	if err != nil {
		return 0, err
	}
	c, err := m.docker.FindContainer(ctx, dbaas.ContainerName(db.Name))
	if err != nil {
		return 0, err
	}
	if c == nil || !c.Running() {
		return 0, fmt.Errorf("database container is not running")
	}

	f, err := m.download(ctx, blob, key)
	if err != nil {
		return 0, err
	}
	defer f.close()
	gz, err := gzip.NewReader(f.file)
	if err != nil {
		return f.size, fmt.Errorf("archive is not gzip: %w", err)
	}

	var stderr strings.Builder
	exit, err := m.docker.ExecContainer(ctx, c.ID, cmd, env, gz, nil, limitWriter(&stderr))
	if err != nil {
		return f.size, fmt.Errorf("%s: %w", cmd[0], err)
	}
	if exit != 0 {
		return f.size, fmt.Errorf("%s exited %d: %s", cmd[0], exit, strings.TrimSpace(stderr.String()))
	}
	return f.size, nil
}

// restoreCommand picks the engine's restore tool (Dokploy's commands, with
// the password in env rather than argv). The dump formats are exactly what
// the paired backup wrote: pg_dump -Fc custom format, plain mysqldump SQL
// without USE statements (so it always lands in the named database).
func restoreCommand(db core.Database) (cmd, env []string, err error) {
	switch db.Engine {
	case core.EnginePostgres:
		return []string{"pg_restore", "-U", db.Username, "-d", db.DBName, "-O", "--clean", "--if-exists"},
			[]string{"PGPASSWORD=" + db.Password}, nil
	case core.EngineMySQL:
		return []string{"mysql", "-uroot", db.DBName},
			[]string{"MYSQL_PWD=" + db.Password}, nil
	default:
		return nil, nil, fmt.Errorf("engine %q has no restore tool (redis was never backed up)", db.Engine)
	}
}

// restoreVolume unpacks one volume archive back into the app's named
// volume: stop the app's running container(s), empty the volume, untar the
// staged archive into it with the helper container, restart what was
// running. rel is the key relative to the schedule's directory —
// <volume>/<stamp>.tar.gz — naming the target volume.
func (m *Manager) restoreVolume(ctx context.Context, b core.Backup, blob blobstore.Client, key, rel string) (int64, error) {
	app, err := m.store.GetApp(ctx, b.TargetID)
	if err != nil {
		return 0, fmt.Errorf("load app: %w", err)
	}
	vol, _, ok := strings.Cut(rel, "/")
	if !ok || vol == "" {
		return 0, fmt.Errorf("archive %q does not name a volume (expected …/%s/<volume>/<stamp>.tar.gz)", key, app.Name)
	}

	// Discover the target volume and the containers to quiesce, per app kind.
	var (
		vols       []string
		containers []docker.Container
	)
	if app.Kind == core.KindCompose {
		project := compose.ProjectName(app.Name)
		vols, err = m.docker.ListVolumes(ctx, map[string]string{"com.docker.compose.project": project})
		if err != nil {
			return 0, err
		}
		containers, err = m.docker.ListContainers(ctx, map[string]string{"com.docker.compose.project": project})
		if err != nil {
			return 0, err
		}
	} else {
		vols, err = m.docker.ListVolumes(ctx, core.VolumeLabels(app.Name))
		if err != nil {
			return 0, err
		}
		// Single-container app: its one canonical container, named via
		// core.AppContainerName, holds the volume. The shared const keeps
		// restore in step with deploy without importing the deploy package.
		if c, err := m.docker.FindContainer(ctx, core.AppContainerName(app.Name)); err != nil {
			return 0, err
		} else if c != nil {
			containers = []docker.Container{*c}
		}
	}
	if !slices.Contains(vols, vol) {
		return 0, fmt.Errorf("volume %q does not exist for this app — deploy it once first; restore never creates volumes", vol)
	}

	f, err := m.download(ctx, blob, key)
	if err != nil {
		return 0, err
	}
	defer f.close()
	if err := m.docker.PullImage(ctx, helperImage, io.Discard); err != nil {
		return f.size, fmt.Errorf("pull %s: %w", helperImage, err)
	}

	// Stop whatever is running while the data is replaced; restart is
	// best-effort even when the untar failed — a broken volume with the app
	// down is strictly worse than a broken volume with it up, and the run row
	// carries the failure either way.
	var stopped []string
	defer func() {
		rctx := context.WithoutCancel(ctx)
		for _, id := range stopped {
			if err := m.docker.StartContainer(rctx, id); err != nil {
				log.Printf("restore %d: restart container %s: %v", b.ID, id, err)
			}
		}
	}()
	for _, c := range containers {
		if !c.Running() {
			continue
		}
		if err := m.docker.StopContainer(ctx, c.ID, stopTimeout); err != nil {
			return f.size, fmt.Errorf("stop %s: %w", c.Name, err)
		}
		stopped = append(stopped, c.ID)
	}

	var stderr strings.Builder
	exit, err := m.docker.RunContainer(ctx, docker.ContainerSpec{
		Name:  fmt.Sprintf("outhaul-restore-%d", b.ID),
		Image: helperImage,
		Cmd:   []string{"sh", "-c", "find /dst -mindepth 1 -delete && tar xzf /restore.tar.gz -C /dst"},
		Labels: map[string]string{
			"outhaul.managed": "true",
			"outhaul.role":    "restore",
		},
		Mounts: []docker.Mount{
			{Source: vol, Target: "/dst", Volume: true},
			{Source: f.file.Name(), Target: "/restore.tar.gz", ReadOnly: true},
		},
	}, nil, limitWriter(&stderr))
	if err != nil {
		return f.size, fmt.Errorf("untar into %s: %w", vol, err)
	}
	if exit != 0 {
		return f.size, fmt.Errorf("untar into %s exited %d: %s", vol, exit, strings.TrimSpace(stderr.String()))
	}
	return f.size, nil
}
