package gitssh

import "testing"

func TestParseGitCommand(t *testing.T) {
	ok := []struct{ in, verb, repo string }{
		{"git-receive-pack 'api'", "git-receive-pack", "api"},
		{"git-upload-pack 'api'", "git-upload-pack", "api"},
		{`git-receive-pack "my-app"`, "git-receive-pack", "my-app"},
		{"git-receive-pack '/api'", "git-receive-pack", "api"},
	}
	for _, c := range ok {
		verb, repo, err := parseGitCommand(c.in)
		if err != nil || verb != c.verb || repo != c.repo {
			t.Fatalf("parseGitCommand(%q) = %q,%q,%v; want %q,%q,nil", c.in, verb, repo, err, c.verb, c.repo)
		}
	}
	bad := []string{
		"", "bash", "git-receive-pack", "git-upload-pack 'a' 'b'",
		"scp -t /x", "git-receive-pack a; rm -rf /", "git-gc 'api'",
		"git-receive-pack ''", "git-receive-pack '../etc'",
	}
	for _, in := range bad {
		if _, _, err := parseGitCommand(in); err == nil {
			t.Fatalf("parseGitCommand(%q) should error", in)
		}
	}
}
