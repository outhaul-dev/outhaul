package webhook

import "testing"

// TestMatchGlob pins the watch-path glob semantics: * and ? stay inside one
// path segment, ** crosses segments, [seq] supports ranges and negation.
func TestMatchGlob(t *testing.T) {
	tests := []struct {
		pattern, path string
		want          bool
	}{
		// literal
		{"docker-compose.yml", "docker-compose.yml", true},
		{"docker-compose.yml", "deploy/docker-compose.yml", false},

		// * within one segment
		{"src/*", "src/app.js", true},
		{"src/*", "src/lib/app.js", false},
		{"*.md", "README.md", true},
		{"*.md", "docs/README.md", false},
		{"src/*.js", "src/app.ts", false},

		// ** across segments (including zero)
		{"src/**", "src/app.js", true},
		{"src/**", "src/lib/deep/app.js", true},
		{"src/**", "srclib/app.js", false},
		{"**/*.sql", "internal/store/migrations/0001_init.sql", true},
		{"**/*.sql", "0001_init.sql", true},
		{"api/**/handlers/*.go", "api/v2/handlers/user.go", true},
		{"api/**/handlers/*.go", "api/v2/models/user.go", false},

		// ?
		{"src/?.js", "src/a.js", true},
		{"src/?.js", "src/ab.js", false},
		{"src/?.js", "src/.js", false},

		// [seq]
		{"src/[a-c].js", "src/b.js", true},
		{"src/[a-c].js", "src/d.js", false},
		{"src/[!a-c].js", "src/d.js", true},
		{"src/[^a-c].js", "src/a.js", false},

		// leading slash tolerated on the pattern
		{"/src/*", "src/app.js", true},

		// invalid pattern matches nothing (never errors)
		{"src/[unclosed", "src/u", false},
	}
	for _, tt := range tests {
		if got := matchGlob(tt.pattern, tt.path); got != tt.want {
			t.Errorf("matchGlob(%q, %q) = %v, want %v", tt.pattern, tt.path, got, tt.want)
		}
	}
}

func TestMatchAny(t *testing.T) {
	patterns := []string{"src/**", "package.json"}
	if !MatchAny(patterns, []string{"docs/x.md", "src/a.js"}) {
		t.Error("expected a match via src/**")
	}
	if MatchAny(patterns, []string{"docs/x.md", "README.md"}) {
		t.Error("expected no match for docs-only changes")
	}
	if MatchAny(nil, []string{"src/a.js"}) {
		t.Error("no patterns must never match (callers treat empty patterns as deploy-always before matching)")
	}
	if MatchAny(patterns, nil) {
		t.Error("no files must never match")
	}
}
