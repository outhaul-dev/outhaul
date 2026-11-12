package previewmgr

import (
	"errors"
	"fmt"

	"github.com/outhaul-dev/outhaul/internal/store"
)

func buildingComment(sha string) string {
	return fmt.Sprintf("⏳ Preview building… (commit `%s`)", short(sha))
}

// ReadyComment is rendered by the deploy-completion hook once the preview is
// live; kept here so all comment copy lives together. An empty sha omits the
// commit line entirely (rather than showing a bogus label).
func ReadyComment(urls []string, sha string) string {
	body := "✅ **Preview ready**\n\n"
	for _, u := range urls {
		body += fmt.Sprintf("- %s\n", u)
	}
	if sha == "" {
		return body
	}
	return body + fmt.Sprintf("\nUpdated for commit `%s`.", short(sha))
}

func FailedComment(sha string) string {
	if sha == "" {
		return "✗ Preview build failed."
	}
	return fmt.Sprintf("✗ Preview build failed for commit `%s`.", short(sha))
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// spawnFailedComment tells the PR a preview never came up. The comment is
// public, so the cause is named only when it is safe and actionable (a host
// clash, with another app or with Outhaul's own address, or a name outside the
// server's domain suffix); anything else points at the server logs.
func spawnFailedComment(err error) string {
	if errors.Is(err, store.ErrDomainTaken) || errors.Is(err, store.ErrHostReserved) {
		return "✗ Preview could not be created: its hostname is already used by another app or by Outhaul itself."
	}
	if errors.Is(err, store.ErrHostOutsideSuffix) {
		return "✗ Preview could not be created: its name is outside this server's domain suffix, so nothing would route it."
	}
	return "✗ Preview could not be created. See the Outhaul server logs for details."
}
