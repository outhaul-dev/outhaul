package deploy

import (
	"context"
	"os"
	"strings"
	"testing"
)

// recordingGit runs the real arg/env construction but captures instead of exec.
func TestCloneArgsWithBranch(t *testing.T) {
	args := cloneArgs(CloneSpec{URL: "https://github.com/o/r.git", Branch: "dev"}, "/work")
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--branch dev") {
		t.Errorf("args missing branch: %v", args)
	}
	if !strings.Contains(joined, "--depth 1") || !strings.Contains(joined, "--single-branch") {
		t.Errorf("args missing shallow flags: %v", args)
	}
	if args[len(args)-2] != "https://github.com/o/r.git" || args[len(args)-1] != "/work" {
		t.Errorf("url/dir not last: %v", args)
	}
}

func TestTokenURL(t *testing.T) {
	got := tokenURL("https://github.com/o/r.git", "ghs_abc")
	want := "https://x-access-token:ghs_abc@github.com/o/r.git"
	if got != want {
		t.Errorf("tokenURL = %q, want %q", got, want)
	}
	// Non-github or non-https URLs are returned unchanged (defensive).
	if u := tokenURL("git@github.com:o/r.git", "t"); u != "git@github.com:o/r.git" {
		t.Errorf("tokenURL mutated ssh url: %q", u)
	}
}

func TestSSHEnvWritesKeyFile(t *testing.T) {
	env, cleanup, err := sshEnv("PRIVATE-KEY-DATA")
	if err != nil {
		t.Fatalf("sshEnv: %v", err)
	}
	defer cleanup()

	var cmd string
	for _, e := range env {
		if strings.HasPrefix(e, "GIT_SSH_COMMAND=") {
			cmd = strings.TrimPrefix(e, "GIT_SSH_COMMAND=")
		}
	}
	if cmd == "" {
		t.Fatal("GIT_SSH_COMMAND not set")
	}
	if !strings.Contains(cmd, "IdentitiesOnly=yes") || !strings.Contains(cmd, "StrictHostKeyChecking=accept-new") {
		t.Errorf("ssh command missing options: %q", cmd)
	}
	// The key file exists, is 0600, and holds the key.
	fields := strings.Fields(cmd)
	var keyPath string
	for i, f := range fields {
		if f == "-i" && i+1 < len(fields) {
			keyPath = fields[i+1]
		}
	}
	data, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("read key file: %v", err)
	}
	if string(data) != "PRIVATE-KEY-DATA" {
		t.Errorf("key file content = %q", data)
	}
	info, _ := os.Stat(keyPath)
	if info.Mode().Perm() != 0o600 {
		t.Errorf("key file perm = %v, want 0600", info.Mode().Perm())
	}

	cleanup()
	if _, err := os.Stat(keyPath); !os.IsNotExist(err) {
		t.Error("key file not removed by cleanup")
	}
}

var _ = context.Background
