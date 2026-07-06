package core

import "time"

// SourcePush is an app source whose repo Outhaul hosts itself: there is no
// remote RepoURL; deploys build from a bare repo pushed to over SSH.
const SourcePush = "push"

// PushKey is a global account SSH public key authorized to push to any
// push-source app. Keys are matched on Fingerprint at SSH auth time.
type PushKey struct {
	ID          int64
	Label       string // human name, e.g. "james-laptop"
	Fingerprint string // ssh.FingerprintSHA256, e.g. "SHA256:abc…"
	PublicKey   string // authorized_keys line
	CreatedAt   time.Time
	LastUsedAt  *time.Time // nil until first use
}
