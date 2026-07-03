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
