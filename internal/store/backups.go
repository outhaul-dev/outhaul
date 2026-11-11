package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/outhaul-dev/outhaul/internal/core"
)

// ErrDestinationInUse is returned by DeleteDestination while backups still
// reference the destination.
var ErrDestinationInUse = errors.New("store: destination still has backups")

// runHistoryCap bounds how many run rows are kept per backup; older rows are
// deleted when a new run is recorded.
const runHistoryCap = 20

// --- destinations ---

// CreateDestination inserts an S3 destination, encrypting the secret key at
// rest. The returned Destination keeps the plaintext secret. The Azure row
// belongs to the environment and is written by SyncAzureDestination.
func (s *Store) CreateDestination(ctx context.Context, d core.Destination) (core.Destination, error) {
	if d.Kind == core.DestinationAzure {
		return core.Destination{}, errors.New("store: the Azure destination comes from the environment")
	}
	if d.Kind == "" {
		d.Kind = core.DestinationS3
	}
	if s.box == nil {
		return core.Destination{}, fmt.Errorf("store: no secret box configured; cannot store destination credentials")
	}
	enc, err := s.box.Seal([]byte(d.SecretKey))
	if err != nil {
		return core.Destination{}, err
	}
	d.CreatedAt = time.Now().UTC()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO destinations (name, endpoint, region, bucket, access_key, secret_key, created_at, kind)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		d.Name, d.Endpoint, d.Region, d.Bucket, d.AccessKey, enc, fmtTime(d.CreatedAt), d.Kind)
	if err != nil {
		return core.Destination{}, err
	}
	d.ID, err = res.LastInsertId()
	if err != nil {
		return core.Destination{}, err
	}
	return d, nil
}

const (
	destinationCols      = `id, name, endpoint, region, bucket, access_key, secret_key, created_at, kind`
	azureDestinationName = "Azure Blob"
)

// SyncAzureDestination keeps the single kind=azure row in step with the
// environment. Empty account and container remove it unless a backup schedule
// uses it; a kept row has its endpoint and bucket cleared.
func (s *Store) SyncAzureDestination(ctx context.Context, account, container string) error {
	var id int64
	err := s.db.QueryRowContext(ctx,
		`SELECT id FROM destinations WHERE kind = ?`, core.DestinationAzure).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		if account == "" && container == "" {
			return nil
		}
		_, err := s.db.ExecContext(ctx,
			`INSERT INTO destinations (name, endpoint, region, bucket, access_key, secret_key, created_at, kind)
			 VALUES (?, ?, '', ?, '', '', ?, ?)`,
			azureDestinationName, azureEndpoint(account), container, fmtTime(time.Now().UTC()), core.DestinationAzure)
		return err
	}
	if err != nil {
		return err
	}
	if account == "" && container == "" {
		if err := s.DeleteDestination(ctx, id); err != nil {
			if !errors.Is(err, ErrDestinationInUse) {
				return err
			}
			_, err = s.db.ExecContext(ctx,
				`UPDATE destinations SET endpoint = '', bucket = '' WHERE id = ?`, id)
			return err
		}
		return nil
	}
	_, err = s.db.ExecContext(ctx,
		`UPDATE destinations SET name = ?, endpoint = ?, region = '', bucket = ?, access_key = '', secret_key = '', kind = ?
		 WHERE id = ?`,
		azureDestinationName, azureEndpoint(account), container, core.DestinationAzure, id)
	return err
}

func azureEndpoint(account string) string {
	return "https://" + account + ".blob.core.windows.net"
}

// stateBackupRunKey holds the last state-backup outcome in settings.
const stateBackupRunKey = "state_backup_last"

// StateBackupRun returns the most recent state-backup outcome. ok is false
// when no run has been recorded.
func (s *Store) StateBackupRun(ctx context.Context) (core.StateBackupRun, bool, error) {
	v, ok, err := s.GetSetting(ctx, stateBackupRunKey)
	if err != nil || !ok {
		return core.StateBackupRun{}, false, err
	}
	var r core.StateBackupRun
	if err := json.Unmarshal([]byte(v), &r); err != nil {
		return core.StateBackupRun{}, false, fmt.Errorf("store: decode %s: %w", stateBackupRunKey, err)
	}
	return r, true, nil
}

// SetStateBackupRun records the most recent state-backup outcome.
func (s *Store) SetStateBackupRun(ctx context.Context, r core.StateBackupRun) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return s.SetSetting(ctx, stateBackupRunKey, string(b))
}

func (s *Store) GetDestination(ctx context.Context, id int64) (core.Destination, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+destinationCols+` FROM destinations WHERE id = ?`, id)
	return s.scanDestination(row)
}

// ListDestinations returns all destinations (secrets decrypted), by name.
func (s *Store) ListDestinations(ctx context.Context) ([]core.Destination, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+destinationCols+` FROM destinations ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ds []core.Destination
	for rows.Next() {
		d, err := s.scanDestination(rows)
		if err != nil {
			return nil, err
		}
		ds = append(ds, d)
	}
	return ds, rows.Err()
}

// DeleteDestination removes an unused destination; one still referenced by
// backups is refused with ErrDestinationInUse.
func (s *Store) DeleteDestination(ctx context.Context, id int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var n int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM backups WHERE destination_id = ?`, id).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return ErrDestinationInUse
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM destinations WHERE id = ?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) scanDestination(row scanner) (core.Destination, error) {
	var (
		d         core.Destination
		enc       string
		createdAt string
	)
	if err := row.Scan(&d.ID, &d.Name, &d.Endpoint, &d.Region, &d.Bucket, &d.AccessKey, &enc, &createdAt, &d.Kind); err != nil {
		return core.Destination{}, err
	}
	if enc != "" {
		if s.box == nil {
			return core.Destination{}, fmt.Errorf("store: no secret box configured; cannot read destination credentials")
		}
		plain, err := s.box.Open(enc)
		if err != nil {
			return core.Destination{}, fmt.Errorf("decrypt destination %q secret: %w", d.Name, err)
		}
		d.SecretKey = string(plain)
	}
	t, err := parseTime(createdAt)
	if err != nil {
		return core.Destination{}, err
	}
	d.CreatedAt = t
	return d, nil
}

// --- backups ---

const backupCols = `id, target_kind, target_id, destination_id, schedule, prefix, retention, enabled, created_at`

// CreateBackup inserts a backup schedule.
func (s *Store) CreateBackup(ctx context.Context, b core.Backup) (core.Backup, error) {
	b.Enabled = true
	b.CreatedAt = time.Now().UTC()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO backups (target_kind, target_id, destination_id, schedule, prefix, retention, enabled, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, 1, ?)`,
		b.TargetKind, b.TargetID, b.DestinationID, b.Schedule, b.Prefix, b.Retention, fmtTime(b.CreatedAt))
	if err != nil {
		return core.Backup{}, err
	}
	b.ID, err = res.LastInsertId()
	if err != nil {
		return core.Backup{}, err
	}
	return b, nil
}

func (s *Store) GetBackup(ctx context.Context, id int64) (core.Backup, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+backupCols+` FROM backups WHERE id = ?`, id)
	return scanBackup(row)
}

// ListBackupsForTarget returns a target's backups, oldest first.
func (s *Store) ListBackupsForTarget(ctx context.Context, kind string, targetID int64) ([]core.Backup, error) {
	return s.listBackups(ctx,
		`SELECT `+backupCols+` FROM backups WHERE target_kind = ? AND target_id = ? ORDER BY id`, kind, targetID)
}

// ListEnabledBackups returns every enabled backup (the scheduler's worklist).
func (s *Store) ListEnabledBackups(ctx context.Context) ([]core.Backup, error) {
	return s.listBackups(ctx,
		`SELECT `+backupCols+` FROM backups WHERE enabled = 1 ORDER BY id`)
}

func (s *Store) listBackups(ctx context.Context, query string, args ...any) ([]core.Backup, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var bs []core.Backup
	for rows.Next() {
		b, err := scanBackup(rows)
		if err != nil {
			return nil, err
		}
		bs = append(bs, b)
	}
	return bs, rows.Err()
}

// SetBackupEnabled toggles a schedule without deleting its history.
func (s *Store) SetBackupEnabled(ctx context.Context, id int64, enabled bool) error {
	_, err := s.db.ExecContext(ctx, `UPDATE backups SET enabled = ? WHERE id = ?`, boolToInt(enabled), id)
	return err
}

// DeleteBackup removes a schedule and its run history.
func (s *Store) DeleteBackup(ctx context.Context, id int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM backup_runs WHERE backup_id = ?`, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM backups WHERE id = ?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// deleteBackupsForTargetTx removes a target's schedules and their history
// inside the caller's transaction — used by DeleteApp and DeleteDatabase so a
// deleted target never leaves orphaned schedules behind.
func deleteBackupsForTargetTx(ctx context.Context, tx *sql.Tx, kind string, targetID int64) error {
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM backup_runs WHERE backup_id IN (SELECT id FROM backups WHERE target_kind = ? AND target_id = ?)`,
		kind, targetID); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx,
		`DELETE FROM backups WHERE target_kind = ? AND target_id = ?`, kind, targetID)
	return err
}

func scanBackup(row scanner) (core.Backup, error) {
	var (
		b         core.Backup
		enabled   int
		createdAt string
	)
	if err := row.Scan(&b.ID, &b.TargetKind, &b.TargetID, &b.DestinationID,
		&b.Schedule, &b.Prefix, &b.Retention, &enabled, &createdAt); err != nil {
		return core.Backup{}, err
	}
	b.Enabled = enabled != 0
	t, err := parseTime(createdAt)
	if err != nil {
		return core.Backup{}, err
	}
	b.CreatedAt = t
	return b, nil
}

// --- runs ---

// StartBackupRun records a run (kind backup or restore) in status running and
// prunes history beyond the cap.
func (s *Store) StartBackupRun(ctx context.Context, backupID int64, kind string) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO backup_runs (backup_id, kind, status, started_at) VALUES (?, ?, ?, ?)`,
		backupID, kind, core.RunRunning, fmtTime(time.Now().UTC()))
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	_, err = s.db.ExecContext(ctx,
		`DELETE FROM backup_runs WHERE backup_id = ? AND id NOT IN
		   (SELECT id FROM backup_runs WHERE backup_id = ? ORDER BY id DESC LIMIT ?)`,
		backupID, backupID, runHistoryCap)
	return id, err
}

// FinishBackupRun records a run's outcome.
func (s *Store) FinishBackupRun(ctx context.Context, runID int64, status, reason, objectKey string, sizeBytes int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE backup_runs SET status = ?, reason = ?, object_key = ?, size_bytes = ?, finished_at = ? WHERE id = ?`,
		status, reason, objectKey, sizeBytes, fmtTime(time.Now().UTC()), runID)
	return err
}

// FailRunningBackupRuns marks every run of kind still in status running as
// failed with reason, returning how many it changed. Boot recovery: the
// process that would have finished them is gone.
func (s *Store) FailRunningBackupRuns(ctx context.Context, kind, reason string) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE backup_runs SET status = ?, reason = ?, finished_at = ? WHERE status = ? AND kind = ?`,
		core.RunFailed, reason, fmtTime(time.Now().UTC()), core.RunRunning, kind)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// CountRunningBackupRuns returns how many runs, backups and restores alike,
// are in status running.
func (s *Store) CountRunningBackupRuns(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM backup_runs WHERE status = ?`, core.RunRunning).Scan(&n)
	return n, err
}

// ListBackupRuns returns a backup's newest runs, most recent first.
func (s *Store) ListBackupRuns(ctx context.Context, backupID int64, limit int) ([]core.BackupRun, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, backup_id, kind, status, reason, size_bytes, object_key, started_at, finished_at
		 FROM backup_runs WHERE backup_id = ? ORDER BY id DESC LIMIT ?`, backupID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var runs []core.BackupRun
	for rows.Next() {
		var (
			r          core.BackupRun
			startedAt  string
			finishedAt sql.NullString
		)
		if err := rows.Scan(&r.ID, &r.BackupID, &r.Kind, &r.Status, &r.Reason, &r.SizeBytes, &r.ObjectKey, &startedAt, &finishedAt); err != nil {
			return nil, err
		}
		t, err := parseTime(startedAt)
		if err != nil {
			return nil, err
		}
		r.StartedAt = t
		if r.FinishedAt, err = scanTime(finishedAt); err != nil {
			return nil, err
		}
		runs = append(runs, r)
	}
	return runs, rows.Err()
}
