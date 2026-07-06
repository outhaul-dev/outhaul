package github

import (
	"context"
	"fmt"
)

// Fake is an in-memory Client for tests. Set the *Result fields to control
// return values; *Err fields to force errors. Calls are recorded.
type Fake struct {
	ManifestResult ManifestResult
	ManifestErr    error
	Token          string
	TokenErr       error
	Repos          []Repo
	ReposErr       error

	LastCode           string
	LastJWT            string
	LastInstallationID int64
	LastToken          string

	Comments map[string]string
}

func (f *Fake) ExchangeManifest(ctx context.Context, code string) (ManifestResult, error) {
	f.LastCode = code
	return f.ManifestResult, f.ManifestErr
}

func (f *Fake) InstallationToken(ctx context.Context, appJWT string, installationID int64) (string, error) {
	f.LastJWT = appJWT
	f.LastInstallationID = installationID
	return f.Token, f.TokenErr
}

func (f *Fake) ListRepos(ctx context.Context, token string) ([]Repo, error) {
	f.LastToken = token
	return f.Repos, f.ReposErr
}

func (f *Fake) UpsertPRComment(ctx context.Context, token, repoFullName string, pr int, body string) error {
	if f.Comments == nil {
		f.Comments = map[string]string{}
	}
	f.Comments[fmt.Sprintf("%s#%d", repoFullName, pr)] = body
	return nil
}

// LastComment returns the latest upserted comment body for repo + pr.
func (f *Fake) LastComment(repo string, pr int) string {
	return f.Comments[fmt.Sprintf("%s#%d", repo, pr)]
}
