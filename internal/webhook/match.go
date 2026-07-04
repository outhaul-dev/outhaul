package webhook

import "strings"

// MatchAny reports whether any file matches any watch-path pattern. It backs
// the watch-paths auto-deploy gate: patterns are the globs an operator
// configured on an app, files are the paths a push changed.
func MatchAny(patterns, files []string) bool {
	for _, p := range patterns {
		for _, f := range files {
			if matchGlob(p, f) {
				return true
			}
		}
	}
	return false
}

// matchGlob matches a slash-separated path against a glob pattern with the
// usual watch-path semantics (the micromatch subset Dokploy documents):
//
//	"*"      any run of characters within one path segment
//	         ("src/*" matches "src/a.js" but not "src/lib/a.js")
//	"**"     any run of whole segments, including none
//	         ("src/**" matches "src/a.js" and "src/lib/deep/a.js")
//	"?"      exactly one character within a segment
//	"[seq]"  one character in seq; ranges ("a-z"), negation ("[^x]"/"[!x]")
//
// Both sides are compared segment-wise, so "*" and "?" never cross a "/".
// An invalid pattern (unclosed "[") matches nothing rather than erroring.
func matchGlob(pattern, path string) bool {
	pattern = strings.TrimPrefix(pattern, "/") // payload paths are repo-relative
	return matchSegments(strings.Split(pattern, "/"), strings.Split(path, "/"))
}

func matchSegments(pat, path []string) bool {
	if len(pat) == 0 {
		return len(path) == 0
	}
	if pat[0] == "**" {
		// "**" may swallow zero or more whole segments.
		for skip := 0; skip <= len(path); skip++ {
			if matchSegments(pat[1:], path[skip:]) {
				return true
			}
		}
		return false
	}
	if len(path) == 0 {
		return false
	}
	return matchSegment(pat[0], path[0]) && matchSegments(pat[1:], path[1:])
}

// matchSegment matches one path segment against one pattern segment.
func matchSegment(pat, s string) bool {
	if pat == "" {
		return s == ""
	}
	switch pat[0] {
	case '*':
		for i := 0; i <= len(s); i++ {
			if matchSegment(pat[1:], s[i:]) {
				return true
			}
		}
		return false
	case '?':
		return s != "" && matchSegment(pat[1:], s[1:])
	case '[':
		end := strings.IndexByte(pat[1:], ']')
		if end < 0 {
			return false // unclosed class: invalid pattern matches nothing
		}
		end++ // index into pat
		if s == "" {
			return false
		}
		set := pat[1:end]
		negate := false
		if strings.HasPrefix(set, "^") || strings.HasPrefix(set, "!") {
			negate, set = true, set[1:]
		}
		if inClass(set, s[0]) == negate {
			return false
		}
		return matchSegment(pat[end+1:], s[1:])
	default:
		return s != "" && pat[0] == s[0] && matchSegment(pat[1:], s[1:])
	}
}

// inClass reports whether c is in a character class body like "a-z_0-9".
func inClass(set string, c byte) bool {
	for i := 0; i < len(set); i++ {
		if i+2 < len(set) && set[i+1] == '-' {
			if c >= set[i] && c <= set[i+2] {
				return true
			}
			i += 2
			continue
		}
		if set[i] == c {
			return true
		}
	}
	return false
}
