package core

import "time"

// App source kinds: how Slipway obtains and authenticates the repo.
const (
	SourcePublic = "public" // public http(s) clone, no credentials
	SourceSSH    = "ssh"    // clone over SSH with a generated deploy key
	SourceGithub = "github" // clone over HTTPS with a GitHub App installation token
)

// App deploy kinds: the strategy that turns the repo into running containers.
const (
	KindNixpacks = "nixpacks" // one Nixpacks-built container, blue-green cutover
	KindCompose  = "compose"  // docker compose stack, recreated in place
)

// App is a deployable unit: a Git repo served on a domain.
type App struct {
	ID        int64
	ProjectID int64  // workspace this app belongs to (DefaultProjectID if unset)
	Name      string // unique, human/URL friendly; also used to name containers
	RepoURL   string // Git URL to clone (https or ssh, per Source)
	Domain    string // host Traefik routes to this app
	CreatedAt time.Time

	Branch        string // branch to clone and match webhooks against ("main" default)
	AutoDeploy    bool   // webhook pushes to Branch trigger a deploy
	Source        string // one of SourcePublic | SourceSSH | SourceGithub
	WebhookSecret string // per-app generic-webhook token (identifies + verifies)
	GithubRepo    string // "owner/name" when Source == SourceGithub
	SSHPublicKey  string // authorized_keys line to add as a deploy key (Source == SourceSSH)

	Kind           string   // KindNixpacks | KindCompose — deploy strategy
	ComposePath    string   // compose file, relative to the repo root (Kind == KindCompose)
	ComposeService string   // service exposed on Domain (Kind == KindCompose; empty = none)
	ComposePort    int      // container port ComposeService listens on
	WatchPaths     []string // glob patterns gating auto-deploy; empty = every push deploys

	// SSHPrivateKey is write-only: set by the caller on create (stored
	// encrypted) and NEVER populated on reads, so it cannot leak via templates.
	SSHPrivateKey string
}
