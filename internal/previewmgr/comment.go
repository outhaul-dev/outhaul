package previewmgr

import "fmt"

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
