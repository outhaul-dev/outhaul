package core

import "time"

// SourcePush is an app source whose repo Outhaul hosts itself: there is no
// remote RepoURL; deploys build from a bare repo pushed to over SSH.
const SourcePush = "push"

// PushKey is an account's SSH public key, authorized to push to the
// push-source apps that account can deploy. Keys are matched on Fingerprint at SSH auth time.
type PushKey struct {
	ID          int64
	UserID      int64  // owning account; 0 means no owner
	Label       string // human name, e.g. "james-laptop"
	Fingerprint string // ssh.FingerprintSHA256, e.g. "SHA256:abc…"
	PublicKey   string // authorized_keys line
	CreatedAt   time.Time
	LastUsedAt  *time.Time // nil until first use
}
