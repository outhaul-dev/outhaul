// Package webhook parses and verifies inbound Git push webhooks. Pure: no I/O.
package webhook

import (
	"encoding/json"
	"strings"
)

// PushEvent is the subset of a push webhook Outhaul acts on.
type PushEvent struct {
	RepoFullName string   // "owner/name"
	Branch       string   // "main"; empty for non-branch refs (e.g. tags)
	Changed      []string // files touched across the push's commits; may be empty (thin payloads)
}

// ParsePush reads a GitHub/Gitea or GitLab push payload. GitHub/Gitea use
// repository.full_name; GitLab uses project.path_with_namespace. A ref that is
// not refs/heads/<branch> yields an empty Branch (so it never matches).
// Changed is the union of every commit's added/modified/removed lists — the
// same shape across all three providers — deduplicated, order preserved.
func ParsePush(body []byte) (PushEvent, error) {
	var p struct {
		Ref        string `json:"ref"`
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
		Project struct {
			PathWithNamespace string `json:"path_with_namespace"`
		} `json:"project"`
		Commits []struct {
			Added    []string `json:"added"`
			Modified []string `json:"modified"`
			Removed  []string `json:"removed"`
		} `json:"commits"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return PushEvent{}, err
	}
	full := p.Repository.FullName
	if full == "" {
		full = p.Project.PathWithNamespace
	}
	var branch string
	if strings.HasPrefix(p.Ref, "refs/heads/") {
		branch = strings.TrimPrefix(p.Ref, "refs/heads/")
	}
	var changed []string
	seen := map[string]bool{}
	for _, c := range p.Commits {
		for _, files := range [][]string{c.Added, c.Modified, c.Removed} {
			for _, f := range files {
				if f != "" && !seen[f] {
					seen[f] = true
					changed = append(changed, f)
				}
			}
		}
	}
	return PushEvent{RepoFullName: full, Branch: branch, Changed: changed}, nil
}
