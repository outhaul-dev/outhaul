package previewmgr

import "fmt"

func buildingComment(sha string) string {
	return fmt.Sprintf("⏳ Preview building… (commit `%s`)", short(sha))
}

// ReadyComment is rendered by the deploy-completion hook (a later task) once the
// preview is live; kept here so all comment copy lives together.
func ReadyComment(urls []string, sha string) string {
	body := "✅ **Preview ready**\n\n"
	for _, u := range urls {
		body += fmt.Sprintf("- %s\n", u)
	}
	return body + fmt.Sprintf("\nUpdated for commit `%s`.", short(sha))
}

func FailedComment(sha string) string {
	return fmt.Sprintf("✗ Preview build failed for commit `%s`.", short(sha))
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
