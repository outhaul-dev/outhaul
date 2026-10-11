package core

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// Preview lifecycle states.
const (
	PreviewBuilding       = "building"
	PreviewReady          = "ready"
	PreviewFailed         = "failed"
	PreviewTeardownFailed = "teardown_failed"
)

// PreviewConfig is per-app configuration for GitHub-PR preview environments.
// Only settable on GitHub-connected apps. The zero value is "disabled".
type PreviewConfig struct {
	AppID         int64
	Enabled       bool
	BaseDomain    string // wildcard base, e.g. "preview.example.com"; "" -> sslip.io
	PostPRComment bool   // post/update a sticky PR comment
	AllowForkPRs  bool   // permit previews for PRs opened from forks
	IdleTTLDays   int    // auto-expire after N days with no new deployment
	MaxConcurrent int    // cap on simultaneous previews per app
}

// DefaultPreviewConfig is applied to a newly-enabled app.
func DefaultPreviewConfig(appID int64) PreviewConfig {
	return PreviewConfig{
		AppID:         appID,
		Enabled:       false,
		PostPRComment: true,
		AllowForkPRs:  false,
		IdleTTLDays:   7,
		MaxConcurrent: 5,
	}
}

// ForkPreviewRef is what a fork PR's preview builds: the base repo's view of
// the PR head, since the fork's commits are on none of the base repo's branches.
func ForkPreviewRef(pr int) string { return fmt.Sprintf("refs/pull/%d/head", pr) }

// IsForkPreview reports whether a is the preview of a pull request from a fork
// — untrusted code. Only fork previews build a refs/pull/ ref.
func (a App) IsForkPreview() bool {
	return a.Ephemeral && strings.HasPrefix(a.Branch, "refs/pull/")
}

// PreviewAppName is the child app's unique name for a parent + PR number.
func PreviewAppName(parentName string, pr int) string {
	return fmt.Sprintf("%s-pr-%d", parentName, pr)
}

// PreviewHost builds a preview's public host for one service. service is "" for
// single-container apps. The first label is "[service-]<name>-pr-<n>" — the app
// name keeps two apps sharing one base domain from claiming the same host. With
// a BaseDomain the label sits under it; otherwise it falls back to sslip.io.
// The label is made DNS-safe: the service name is lowercased with anything
// outside [a-z0-9-] turned into "-", and a label over 63 characters is cut and
// suffixed with a hash of the whole label so it stays valid and distinct.
func PreviewHost(cfg PreviewConfig, appName string, pr int, service, serverIP string) string {
	label := PreviewAppName(appName, pr)
	if service != "" {
		label = dnsSafe(service) + "-" + label
	}
	label = capLabel(label)
	if cfg.BaseDomain != "" {
		return label + "." + cfg.BaseDomain
	}
	return fmt.Sprintf("%s.%s.sslip.io", label, serverIP)
}

// maxLabel is the longest a single DNS label may be.
const maxLabel = 63

// dnsSafe lowercases s and replaces every character outside [a-z0-9-] with "-".
func dnsSafe(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			return r
		case r >= 'A' && r <= 'Z':
			return r + ('a' - 'A')
		}
		return '-'
	}, s)
}

// capLabel returns label unchanged if it fits in a DNS label, else its first
// characters plus "-" and 8 hex characters of its SHA-256.
func capLabel(label string) string {
	if len(label) <= maxLabel {
		return label
	}
	sum := sha256.Sum256([]byte(label))
	suffix := hex.EncodeToString(sum[:])[:8]
	head := strings.TrimRight(label[:maxLabel-len(suffix)-1], "-")
	return head + "-" + suffix
}
