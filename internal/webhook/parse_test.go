package webhook

import "testing"

func TestParsePush(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		wantRepo string
		wantBran string
	}{
		{
			name:     "github push",
			body:     `{"ref":"refs/heads/main","repository":{"full_name":"owner/repo"}}`,
			wantRepo: "owner/repo",
			wantBran: "main",
		},
		{
			name:     "gitlab push",
			body:     `{"ref":"refs/heads/dev","project":{"path_with_namespace":"grp/proj"}}`,
			wantRepo: "grp/proj",
			wantBran: "dev",
		},
		{
			name:     "tag push has no branch",
			body:     `{"ref":"refs/tags/v1.0.0","repository":{"full_name":"owner/repo"}}`,
			wantRepo: "owner/repo",
			wantBran: "",
		},
		{
			name:     "branch with slashes",
			body:     `{"ref":"refs/heads/feature/x","repository":{"full_name":"owner/repo"}}`,
			wantRepo: "owner/repo",
			wantBran: "feature/x",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ev, err := ParsePush([]byte(tt.body))
			if err != nil {
				t.Fatalf("ParsePush: %v", err)
			}
			if ev.RepoFullName != tt.wantRepo {
				t.Errorf("repo = %q, want %q", ev.RepoFullName, tt.wantRepo)
			}
			if ev.Branch != tt.wantBran {
				t.Errorf("branch = %q, want %q", ev.Branch, tt.wantBran)
			}
		})
	}
}

func TestParsePushMalformed(t *testing.T) {
	if _, err := ParsePush([]byte("not json")); err == nil {
		t.Error("expected error for malformed body")
	}
}

// TestParsePushChangedFiles pins the changed-file extraction feeding watch
// paths: the union of added/modified/removed across commits, deduplicated.
func TestParsePushChangedFiles(t *testing.T) {
	body := `{
		"ref": "refs/heads/main",
		"repository": {"full_name": "owner/repo"},
		"commits": [
			{"added": ["src/new.js"], "modified": ["README.md"], "removed": []},
			{"added": [], "modified": ["README.md", "src/app.js"], "removed": ["old.txt"]}
		]
	}`
	ev, err := ParsePush([]byte(body))
	if err != nil {
		t.Fatalf("ParsePush: %v", err)
	}
	want := []string{"src/new.js", "README.md", "src/app.js", "old.txt"}
	if len(ev.Changed) != len(want) {
		t.Fatalf("Changed = %v, want %v", ev.Changed, want)
	}
	for i, f := range want {
		if ev.Changed[i] != f {
			t.Errorf("Changed[%d] = %q, want %q", i, ev.Changed[i], f)
		}
	}
}

// Commits with missing file arrays (Dokploy issue #4081 crashes there) must
// parse cleanly to an empty Changed, which the deploy gate fails open on.
func TestParsePushThinPayloadHasNoChangedFiles(t *testing.T) {
	body := `{"ref":"refs/heads/main","repository":{"full_name":"o/r"},"commits":[{"id":"abc"}]}`
	ev, err := ParsePush([]byte(body))
	if err != nil {
		t.Fatalf("ParsePush: %v", err)
	}
	if len(ev.Changed) != 0 {
		t.Errorf("Changed = %v, want empty", ev.Changed)
	}
}
