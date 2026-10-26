package tagdeploy

import (
	"fmt"
	"strings"

	"github.com/outhaul-dev/outhaul/internal/core"
	"github.com/outhaul-dev/outhaul/internal/webhook"
)

// Policy is an app's tag-deploy settings: which tags it follows, and whether
// pre-releases count.
type Policy struct {
	Pattern     string // glob a tag must match, e.g. "v*" or "api/v*"
	Prereleases bool   // also deploy pre-release versions
}

// PolicyFor returns the policy configured on app.
func PolicyFor(app core.App) Policy {
	return Policy{Pattern: app.TagPattern, Prereleases: app.TagPrereleases}
}

// Decision is the outcome of Decide. An empty Tag means nothing deploys, and
// Reason says why.
type Decision struct {
	Tag    string
	Reason string
}

// Version parses tag as a version after removing the pattern's literal
// prefix, the text before its first "*", "?" or "[". With pattern "api/v*",
// "api/v1.2.0" parses as 1.2.0. It reports false when the tag lacks the
// prefix or the rest isn't a version.
func (p Policy) Version(tag string) (Version, bool) {
	prefix := strings.TrimPrefix(p.Pattern, "/") // read the pattern as MatchGlob does
	if i := strings.IndexAny(prefix, "*?["); i >= 0 {
		prefix = prefix[:i]
	}
	rest, ok := strings.CutPrefix(tag, prefix)
	if !ok {
		return Version{}, false
	}
	return ParseVersion(rest)
}

// Eligible reports whether tag matches the pattern and, unless pre-releases
// are on, isn't a pre-release. A tag that isn't version-like is never a
// pre-release.
func (p Policy) Eligible(tag string) bool {
	if !webhook.MatchGlob(p.Pattern, tag) {
		return false
	}
	if v, ok := p.Version(tag); ok && v.IsPrerelease() && !p.Prereleases {
		return false
	}
	return true
}

// Decide picks the tag to deploy from those a push created. inPlay holds the
// tags already live or on their way (live, queued, building or deploying);
// the highest of them is the high-water mark, and a pushed tag deploys only
// if it is newer. Of several eligible tags it takes the highest version. If
// none is version-like it takes the last one, so a pattern such as
// "nightly-*" still deploys.
func (p Policy) Decide(pushed, inPlay []string) Decision {
	if len(pushed) == 0 {
		return Decision{Reason: "no tag was pushed"}
	}
	var eligible []string
	for _, t := range pushed {
		if p.Eligible(t) {
			eligible = append(eligible, t)
		}
	}
	if len(eligible) == 0 {
		// Explain the first tag that matched the pattern (it was held back as
		// a pre-release); `git push --tags` order may put non-matching first.
		for _, t := range pushed {
			if webhook.MatchGlob(p.Pattern, t) {
				return Decision{Reason: p.rejection(t)}
			}
		}
		return Decision{Reason: p.rejection(pushed[0])}
	}

	best, bestV, found := "", Version{}, false
	for _, t := range eligible {
		v, ok := p.Version(t)
		if ok && (!found || v.Compare(bestV) > 0) {
			best, bestV, found = t, v, true
		}
	}
	if !found {
		return Decision{Tag: eligible[len(eligible)-1]}
	}

	mark, markV, have := "", Version{}, false
	for _, t := range inPlay {
		if !webhook.MatchGlob(p.Pattern, t) {
			continue
		}
		v, ok := p.Version(t)
		if ok && (!have || v.Compare(markV) > 0) {
			mark, markV, have = t, v, true
		}
	}
	if have && bestV.Compare(markV) <= 0 {
		return Decision{Reason: fmt.Sprintf("%s is not newer than %s, which is live or already on its way", best, mark)}
	}
	return Decision{Tag: best}
}

// rejection explains why an ineligible tag was passed over.
func (p Policy) rejection(tag string) string {
	if webhook.MatchGlob(p.Pattern, tag) {
		return fmt.Sprintf("%s is a pre-release, and this app doesn't deploy pre-releases", tag)
	}
	return fmt.Sprintf("no pushed tag matches %q", p.Pattern)
}

// Newest returns the highest version-like eligible tag, or false if there is
// none.
func (p Policy) Newest(tags []string) (string, bool) {
	best, bestV, found := "", Version{}, false
	for _, t := range tags {
		if !p.Eligible(t) {
			continue
		}
		v, ok := p.Version(t)
		if ok && (!found || v.Compare(bestV) > 0) {
			best, bestV, found = t, v, true
		}
	}
	return best, found
}
