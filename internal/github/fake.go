package github

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
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

	// InstallationsByApp maps App id -> the installations that App owns.
	InstallationsByApp map[int64][]Installation
	InstallationErr    error

	LastCode           string
	LastJWT            string
	LastInstallationID int64
	LastToken          string

	ReposCalls int // number of ListRepos invocations, for cache tests

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
	f.ReposCalls++
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

func (f *Fake) Installation(ctx context.Context, appJWT string, installationID int64) (Installation, error) {
	f.LastJWT = appJWT
	if f.InstallationErr != nil {
		return Installation{}, f.InstallationErr
	}
	for _, inst := range f.InstallationsByApp[issFromJWT(appJWT)] {
		if inst.ID == installationID {
			return inst, nil
		}
	}
	return Installation{}, fmt.Errorf("github: installation %d not found for this app", installationID)
}

// issFromJWT reads the App id from an unverified JWT payload. The fake needs it
// to scope installations to the calling App the way GitHub's API does; nothing
// in production trusts a JWT this way.
func issFromJWT(token string) int64 {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return 0
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return 0
	}
	var claims struct {
		Iss int64 `json:"iss"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return 0
	}
	return claims.Iss
}
