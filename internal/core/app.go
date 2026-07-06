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

// Domain publishes one app route: Traefik matches Host (and optionally a Path
// prefix) and forwards to a container Port, optionally rewriting the path to
// InternalPath and terminating TLS. A nixpacks/dockerfile app's rows target its
// single container on AppPort with an empty Service; a compose app's rows name
// the stack Service to route to. Zero rows means the app is internal-only.
type Domain struct {
	ID           int64
	AppID        int64
	Host         string // bare hostname Traefik matches
	Service      string // compose service; "" for nixpacks/dockerfile
	Port         int    // container port; 8080 for nixpacks/dockerfile
	Path         string // external PathPrefix, leading slash; "" = whole host
	InternalPath string // path forwarded to the container; "" = unchanged
	TLS          bool   // automate HTTPS for this route (needs global ACME)
	CreatedAt    time.Time
}

// DomainListing is a Domain plus its app's identity, for the global Domains tab.
type DomainListing struct {
	Domain
	AppName string
	AppKind string
}
