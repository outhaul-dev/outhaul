package core

import "fmt"

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

// PreviewAppName is the child app's unique name for a parent + PR number.
func PreviewAppName(parentName string, pr int) string {
	return fmt.Sprintf("%s-pr-%d", parentName, pr)
}

// PreviewHost builds a preview's public host for one service. service is "" for
// single-container apps. With a BaseDomain it produces "[service-]pr-<n>.<base>";
// otherwise it falls back to sslip.io "<name[-service]>-pr-<n>.<ip>.sslip.io".
func PreviewHost(cfg PreviewConfig, appName string, pr int, service, serverIP string) string {
	if cfg.BaseDomain != "" {
		if service != "" {
			return fmt.Sprintf("%s-pr-%d.%s", service, pr, cfg.BaseDomain)
		}
		return fmt.Sprintf("pr-%d.%s", pr, cfg.BaseDomain)
	}
	label := PreviewAppName(appName, pr)
	if service != "" {
		label = fmt.Sprintf("%s-%s-pr-%d", service, appName, pr)
	}
	return fmt.Sprintf("%s.%s.sslip.io", label, serverIP)
}
