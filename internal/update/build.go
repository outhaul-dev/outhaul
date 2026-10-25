// Package update knows which build of Outhaul is running and where newer
// ones come from.
package update

// The build stamp, set at link time with
//
//	-ldflags "-X github.com/outhaul-dev/outhaul/internal/update.commit=<sha>
//	          -X github.com/outhaul-dev/outhaul/internal/update.repo=<url>
//	          -X github.com/outhaul-dev/outhaul/internal/update.local=true"
//
// All three are empty in an unstamped build (go run, go test).
var commit, repo, local string

const defaultRepo = "https://github.com/outhaul-dev/outhaul.git"

// Build describes the running binary.
type Build struct {
	Commit string // full git SHA the binary was built from; empty if unstamped
	Repo   string // git URL updates are fetched from
	Local  bool   // built from a local checkout rather than the upstream repo
}

// Current returns the stamp this binary was built with.
func Current() Build {
	b := Build{Commit: commit, Repo: repo, Local: local == "true"}
	if b.Repo == "" {
		b.Repo = defaultRepo
	}
	return b
}

// Known reports whether the binary knows its commit.
func (b Build) Known() bool { return b.Commit != "" }

// Short is the first 7 characters of the commit, or "unknown".
func (b Build) Short() string {
	if !b.Known() {
		return "unknown"
	}
	if len(b.Commit) < 7 {
		return b.Commit
	}
	return b.Commit[:7]
}
