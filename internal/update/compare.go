package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// Comparison statuses.
const (
	CompareBehind   = "behind"            // main has commits the running build lacks
	CompareUpToDate = "up_to_date"        // the running build is main
	CompareAhead    = "ahead_or_diverged" // the running build has commits main lacks, or is unknown to GitHub
)

const (
	defaultAPIBase = "https://api.github.com"
	maxCommits     = 50
	maxCompareBody = 64 << 20
)

var (
	// ErrNotGitHub means the build's repository is not on github.com.
	ErrNotGitHub = errors.New("update: repository is not on GitHub")
	// ErrUnknownBuild means the binary does not know which commit it is.
	ErrUnknownBuild = errors.New("update: this build does not know its commit")
)

// Commit is one commit on main that the running build lacks.
type Commit struct {
	SHA     string
	Subject string
	Date    time.Time
}

// Comparison is how far the running build is behind main.
type Comparison struct {
	Status           string
	Behind           int
	Commits          []Commit // newest first, at most 50
	URL              string
	InstallerChanged bool // deploy/bootstrap.sh differs
	TraefikChanged   bool // the pinned Traefik image or its static config differs
}

// Comparer asks GitHub's compare API how a commit relates to main. It sends
// no credentials.
type Comparer struct {
	Client  *http.Client
	APIBase string // default https://api.github.com
}

var repoPattern = regexp.MustCompile(`^(?:https://github\.com/|git@github\.com:)([A-Za-z0-9_.-]+)/([A-Za-z0-9_.-]+?)(?:\.git)?/?$`)

func parseRepo(u string) (owner, name string, err error) {
	m := repoPattern.FindStringSubmatch(strings.TrimSpace(u))
	if m == nil {
		return "", "", ErrNotGitHub
	}
	return m[1], m[2], nil
}

type compareReply struct {
	Status  string `json:"status"`
	AheadBy int    `json:"ahead_by"`
	HTMLURL string `json:"html_url"`
	Commits []struct {
		SHA    string `json:"sha"`
		Commit struct {
			Message   string `json:"message"`
			Committer struct {
				Date time.Time `json:"date"`
			} `json:"committer"`
		} `json:"commit"`
	} `json:"commits"`
	Files []struct {
		Filename string `json:"filename"`
		Patch    string `json:"patch"`
	} `json:"files"`
}

// Compare reports how main relates to the commit base.
func (c *Comparer) Compare(ctx context.Context, repoURL, base string) (Comparison, error) {
	owner, name, err := parseRepo(repoURL)
	if err != nil {
		return Comparison{}, err
	}
	apiBase := strings.TrimRight(c.APIBase, "/")
	if apiBase == "" {
		apiBase = defaultAPIBase
	}
	client := c.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/repos/%s/%s/compare/%s...main", apiBase, owner, name, base), nil)
	if err != nil {
		return Comparison{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := client.Do(req)
	if err != nil {
		return Comparison{}, err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		// GitHub does not know the commit: a local build, or a force-pushed one.
		return Comparison{Status: CompareAhead}, nil
	case resp.StatusCode != http.StatusOK:
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return Comparison{}, fmt.Errorf("github compare: %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	var r compareReply
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxCompareBody)).Decode(&r); err != nil {
		return Comparison{}, fmt.Errorf("github compare: %w", err)
	}

	out := Comparison{URL: r.HTMLURL}
	switch r.Status {
	case "identical":
		out.Status = CompareUpToDate
	case "ahead":
		out.Status = CompareBehind
		out.Behind = r.AheadBy
	case "behind", "diverged":
		out.Status = CompareAhead
	default:
		return Comparison{}, fmt.Errorf("github compare: unexpected status %q", r.Status)
	}
	if out.Status != CompareBehind {
		return out, nil
	}
	for i := len(r.Commits) - 1; i >= 0 && len(out.Commits) < maxCommits; i-- {
		rc := r.Commits[i]
		subject, _, _ := strings.Cut(rc.Commit.Message, "\n")
		out.Commits = append(out.Commits, Commit{SHA: rc.SHA, Subject: strings.TrimSpace(subject), Date: rc.Commit.Committer.Date})
	}
	for _, f := range r.Files {
		switch f.Filename {
		case "deploy/bootstrap.sh":
			out.InstallerChanged = true
		case "internal/traefik/proxy.go":
			out.TraefikChanged = true
		case "internal/config/config.go":
			if patchChangesLine(f.Patch, "DefaultTraefikImage") {
				out.TraefikChanged = true
			}
		}
	}
	return out, nil
}

// patchChangesLine reports whether an added or removed line of the patch
// contains s.
func patchChangesLine(patch, s string) bool {
	for _, line := range strings.Split(patch, "\n") {
		if (strings.HasPrefix(line, "+") || strings.HasPrefix(line, "-")) && strings.Contains(line, s) {
			return true
		}
	}
	return false
}
