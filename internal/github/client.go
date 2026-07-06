package github

import "context"

// Repo is a repository the installation can access.
type Repo struct {
	FullName      string // "owner/name"
	DefaultBranch string // the repo's default branch, e.g. "main" or "master"
}

// ManifestResult is the App's credentials returned by the manifest conversion.
type ManifestResult struct {
	AppID         int64
	Slug          string
	PEM           string
	WebhookSecret string
	ClientID      string
	ClientSecret  string
}

// Client talks to the GitHub App API. Implemented by *HTTPClient and *Fake.
type Client interface {
	ExchangeManifest(ctx context.Context, code string) (ManifestResult, error)
	InstallationToken(ctx context.Context, appJWT string, installationID int64) (string, error)
	ListRepos(ctx context.Context, token string) ([]Repo, error)
	// UpsertPRComment creates or updates the single Outhaul preview comment on a
	// PR, identified by a hidden marker in the body.
	UpsertPRComment(ctx context.Context, token, repoFullName string, pr int, body string) error
}
