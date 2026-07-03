package core

import "time"

// App is a deployable unit: a public Git repo served on a domain. M1 keeps
// this deliberately small; env vars, private-repo auth, and build config are
// later seams.
type App struct {
	ID        int64
	Name      string // unique, human/URL friendly; also used to name containers
	RepoURL   string // public Git URL to clone
	Domain    string // host Traefik routes to this app
	CreatedAt time.Time
}
