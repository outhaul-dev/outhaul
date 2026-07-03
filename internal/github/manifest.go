// Package github integrates with GitHub Apps: manifest creation, the App JWT,
// installation tokens, and repository listing. The Client interface has a real
// HTTP implementation and a fake so handlers and the pipeline test offline.
package github

import (
	"encoding/json"
	"strings"
)

// ManifestParams are the inputs to a GitHub App manifest.
type ManifestParams struct {
	Name      string // globally-unique GitHub App name
	PublicURL string // Slipway's externally reachable base URL
}

// BuildManifest returns the GitHub App manifest JSON POSTed to
// github.com/settings/apps/new. Permissions are read-only contents + metadata;
// the App subscribes to push events only.
func BuildManifest(p ManifestParams) (string, error) {
	base := strings.TrimRight(p.PublicURL, "/")
	m := map[string]any{
		"name":         p.Name,
		"url":          base,
		"redirect_url": base + "/github/callback",
		"hook_attributes": map[string]any{
			"url": base + "/webhooks/github",
		},
		"public": false,
		"default_permissions": map[string]any{
			"contents": "read",
			"metadata": "read",
		},
		"default_events": []string{"push"},
	}
	b, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
