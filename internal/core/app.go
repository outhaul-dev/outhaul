package core

import "time"

// App source kinds: how Outhaul obtains and authenticates the repo.
const (
	SourcePublic   = "public"   // public http(s) clone, no credentials
	SourceSSH      = "ssh"      // clone over SSH with a generated deploy key
	SourceGithub   = "github"   // clone over HTTPS with a GitHub App installation token
	SourceTemplate = "template" // no repo: compose file snapshotted from the built-in catalog
)

// App deploy kinds: the strategy that turns the repo into running containers.
const (
	KindNixpacks   = "nixpacks"   // one Nixpacks-built container, blue-green cutover
	KindDockerfile = "dockerfile" // one container built from the repo's Dockerfile, same cutover
	KindCompose    = "compose"    // docker compose stack, recreated in place
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

	Kind           string   // KindNixpacks | KindDockerfile | KindCompose — deploy strategy
	ComposePath    string   // compose file, relative to the repo root (Kind == KindCompose)
	DockerfilePath string   // Dockerfile, relative to the repo root (Kind == KindDockerfile)
	WatchPaths     []string // glob patterns gating auto-deploy; empty = every push deploys

	// Template apps (Source == SourceTemplate) carry their compose file with
	// them instead of a repo: ComposeRaw is the snapshot taken from the
	// built-in catalog at create time and the pipeline deploys it without
	// cloning anything.
	TemplateID string // catalog id the app was created from
	ComposeRaw string // compose file contents (Source == SourceTemplate)

	// SSHPrivateKey is write-only: set by the caller on create (stored
	// encrypted) and NEVER populated on reads, so it cannot leak via templates.
	SSHPrivateKey string
}

// ComposeDomain publishes one compose-stack service on one host: Traefik
// routes Domain to the named Service's container Port. A compose app may have
// any number of these (Dokploy's model); zero means the stack is
// internal-only. For nixpacks apps the single App.Domain plays this role.
type ComposeDomain struct {
	ID      int64
	AppID   int64
	Domain  string // bare hostname Traefik matches
	Service string // compose service name inside the stack
	Port    int    // container port that service listens on
}
