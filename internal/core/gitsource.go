package core

import "time"

// GitSourceGithubApp is the only git source kind today: a GitHub App plus the
// single installation it was created for. Kind selects the gitsource.Provider
// that can list a source's repos, mint its credentials, and verify its webhooks.
const GitSourceGithubApp = "github_app"

// GitSource is one connected account on a Git host. Outhaul creates private
// GitHub Apps, and GitHub only installs a private App on the account that owns
// it — so one source is one App is one account.
type GitSource struct {
	ID           int64
	Kind         string
	AccountLogin string // "" until the installation is bound
	AccountType  string // "User" | "Organization"
	CreatedAt    time.Time

	// GithubApp carries the credentials when Kind == GitSourceGithubApp.
	GithubApp GithubAppCreds
}

// GithubAppCreds are a GitHub App's credentials. Secret fields hold plaintext
// in memory (decrypted); the store seals them at rest.
type GithubAppCreds struct {
	AppID          int64
	Slug           string
	PrivateKey     string // PEM
	WebhookSecret  string
	ClientID       string
	ClientSecret   string
	InstallationID int64
}

// Installed reports whether the source finished installation and can mint
// credentials. An uninstalled source exists on GitHub but grants nothing.
func (s GitSource) Installed() bool {
	switch s.Kind {
	case GitSourceGithubApp:
		return s.GithubApp.InstallationID != 0
	default:
		return false
	}
}

// Display is the name to show in the UI: the account login once GitHub has
// told us, the App slug while it has not, and a marker if neither is known.
func (s GitSource) Display() string {
	if s.AccountLogin != "" {
		return s.AccountLogin
	}
	if s.GithubApp.Slug != "" {
		return s.GithubApp.Slug
	}
	return "(pending)"
}

// AccountKind renders AccountType for the UI; "" when the account is unknown.
func (s GitSource) AccountKind() string {
	switch s.AccountType {
	case "Organization":
		return "org"
	case "User":
		return "personal"
	}
	return ""
}
