package gitsource

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/outhaul-dev/outhaul/internal/core"
	"github.com/outhaul-dev/outhaul/internal/github"
	"github.com/outhaul-dev/outhaul/internal/webhook"
)

// GithubApp serves sources of kind core.GitSourceGithubApp: a GitHub App and
// the one installation it was created for.
type GithubApp struct{ gh github.Client }

// NewGithubApp wraps a GitHub API client as a Provider.
func NewGithubApp(c github.Client) *GithubApp { return &GithubApp{gh: c} }

func (p *GithubApp) Kind() string { return core.GitSourceGithubApp }

// Token mints a fresh installation access token. Tokens are short-lived and
// minted per use; caching them is a deliberate seam, not an oversight.
func (p *GithubApp) Token(ctx context.Context, src core.GitSource) (string, error) {
	jwt, err := p.appJWT(src)
	if err != nil {
		return "", err
	}
	tok, err := p.gh.InstallationToken(ctx, jwt, src.GithubApp.InstallationID)
	if err != nil {
		return "", fmt.Errorf("mint installation token for %s: %w", src.Display(), err)
	}
	return tok, nil
}

func (p *GithubApp) Repos(ctx context.Context, src core.GitSource) ([]Repo, error) {
	tok, err := p.Token(ctx, src)
	if err != nil {
		return nil, err
	}
	ghRepos, err := p.gh.ListRepos(ctx, tok)
	if err != nil {
		return nil, fmt.Errorf("list repos for %s: %w", src.Display(), err)
	}
	repos := make([]Repo, 0, len(ghRepos))
	for _, r := range ghRepos {
		repos = append(repos, Repo{FullName: r.FullName, DefaultBranch: r.DefaultBranch})
	}
	return repos, nil
}

func (p *GithubApp) VerifyWebhook(src core.GitSource, h http.Header, body []byte) bool {
	// Guard against a non-GitHub-App source ever reaching this provider: without
	// it, src.GithubApp.WebhookSecret would be the empty HMAC key and every
	// signature would verify. Unreachable today (Registry only routes
	// core.GitSourceGithubApp here), but appJWT below carries the same guard —
	// stay consistent rather than relying on the caller alone.
	if src.Kind != core.GitSourceGithubApp {
		return false
	}
	return webhook.VerifyGitHub(src.GithubApp.WebhookSecret, h.Get("X-Hub-Signature-256"), body)
}

// appJWT builds the App-authenticating JWT, refusing a source that cannot mint
// anything yet. The messages surface in deploy logs, so they name the source.
func (p *GithubApp) appJWT(src core.GitSource) (string, error) {
	if src.Kind != core.GitSourceGithubApp {
		return "", fmt.Errorf("gitsource: %q is not a GitHub App source", src.Kind)
	}
	if !src.Installed() {
		return "", fmt.Errorf("git source %s is not installed on GitHub", src.Display())
	}
	jwt, err := github.AppJWT(src.GithubApp.PrivateKey, src.GithubApp.AppID, time.Now())
	if err != nil {
		return "", fmt.Errorf("build app jwt for %s: %w", src.Display(), err)
	}
	return jwt, nil
}
