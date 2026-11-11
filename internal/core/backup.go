package core

import (
	"strings"
	"time"
)

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

// Destination kinds: which client and credentials a destination uses.
const (
	DestinationS3    = "s3"    // an S3-compatible endpoint with an access key and secret
	DestinationAzure = "azure" // Azure Blob Storage, authenticated by the host's managed identity
)

// Destination is a bucket backups upload to: an S3-compatible one (AWS, MinIO,
// R2, B2, Wasabi, …) or an Azure Blob container. S3 destinations are addressed
// path-style and store SecretKey encrypted; the Azure one comes from the
// environment and has no secret.
type Destination struct {
	ID        int64
	Name      string // unique, human-friendly
	Endpoint  string // e.g. https://s3.eu-west-2.amazonaws.com, http://minio:9000, or https://<account>.blob.core.windows.net
	Region    string // SigV4 region; "auto" or "us-east-1" for most compatibles
	Bucket    string
	AccessKey string
	SecretKey string
	CreatedAt time.Time
	Kind      string // DestinationS3 | DestinationAzure; "" is treated as s3 when read
}

// AzureAccount is the storage account in an Azure endpoint, or "" when the
// endpoint is empty or is not the Azure form.
func (d Destination) AzureAccount() string {
	e := strings.TrimSuffix(d.Endpoint, "/")
	for _, scheme := range []string{"https://", "http://"} {
		if strings.HasPrefix(e, scheme) {
			e = strings.TrimPrefix(e, scheme)
			break
		}
	}
	const suffix = ".blob.core.windows.net"
	if !strings.HasSuffix(e, suffix) {
		return ""
	}
	return strings.TrimSuffix(e, suffix)
}

// StateBackupRun is the outcome of the most recent copy of Outhaul's own
// state, shown on Settings.
type StateBackupRun struct {
	At        time.Time `json:"at"`
	OK        bool      `json:"ok"`
	Key       string    `json:"key"`
	SizeBytes int64     `json:"size_bytes"`
	Reason    string    `json:"reason"`
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
