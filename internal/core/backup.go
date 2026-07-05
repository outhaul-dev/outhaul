package core

import "time"

// Backup targets: what a backup snapshots.
const (
	BackupTargetDatabase = "database" // a managed database's dump
	BackupTargetApp      = "app"      // a compose stack's named volumes
)

// Backup run outcomes.
const (
	RunRunning = "running"
	RunOK      = "ok"
	RunFailed  = "failed"
)

// Backup run kinds: which direction the archive moved.
const (
	RunKindBackup  = "backup"
	RunKindRestore = "restore"
)

// Destination is an S3-compatible bucket backups upload to (AWS, MinIO, R2,
// B2, Wasabi, …). Addressed path-style; SecretKey is stored encrypted.
type Destination struct {
	ID        int64
	Name      string // unique, human-friendly
	Endpoint  string // e.g. https://s3.eu-west-2.amazonaws.com or http://minio:9000
	Region    string // SigV4 region; "auto" or "us-east-1" for most compatibles
	Bucket    string
	AccessKey string
	SecretKey string
	CreatedAt time.Time
}

// Backup is a schedule attaching one target to one destination: "dump this
// database (or tar this stack's volumes) on this cron, keep the newest N".
type Backup struct {
	ID            int64
	TargetKind    string // BackupTargetDatabase | BackupTargetApp
	TargetID      int64  // databases.id or apps.id
	DestinationID int64
	Schedule      string // 5-field cron expression, server-local time
	Prefix        string // object key prefix inside the bucket ("" = root)
	Retention     int    // newest objects kept per directory; 0 = keep all
	Enabled       bool
	CreatedAt     time.Time
}

// BackupRun is one execution of a Backup or a restore from one of its
// archives: what ran, what it produced, and how it ended. ObjectKey is the
// uploaded object (the last one, for multi-volume app backups) or the archive
// restored; Reason carries the failure detail.
type BackupRun struct {
	ID         int64
	BackupID   int64
	Kind       string // RunKindBackup | RunKindRestore
	Status     string // RunRunning | RunOK | RunFailed
	Reason     string
	SizeBytes  int64
	ObjectKey  string
	StartedAt  time.Time
	FinishedAt *time.Time
}
