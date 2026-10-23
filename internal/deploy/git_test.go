package deploy

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

func TestTokenEnvScopesBasicAuthHeaderToGitHub(t *testing.T) {
	env := tokenEnv("https://github.com/o/r.git", "ghs_abc")
	want := []string{
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http.https://github.com/.extraheader",
		"GIT_CONFIG_VALUE_0=Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:ghs_abc")),
	}
	if strings.Join(env, "\n") != strings.Join(want, "\n") {
		t.Errorf("tokenEnv = %q, want %q", env, want)
	}
	// Non-github or non-https URLs get no credentials (defensive).
	if env := tokenEnv("git@github.com:o/r.git", "t"); len(env) != 0 {
		t.Errorf("tokenEnv for an ssh url = %q, want none", env)
	}
}

// The installation token must never be part of the clone URL: git writes the
// URL into the work tree's .git/config, the work tree is the build context,
// and a plain COPY . . would bake an installation-wide token into the image.
func TestCloneKeepsTokenOutOfURL(t *testing.T) {
	dir := t.TempDir()
	record := filepath.Join(dir, "record")
	fake := filepath.Join(dir, "git")
	script := "#!/bin/sh\n{ printf 'ARGS:%s\\n' \"$*\"; env | grep '^GIT_CONFIG_'; } > " + record + "\nexit 0\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	g := &Git{Bin: fake}
	spec := CloneSpec{URL: "https://github.com/o/r.git", Branch: "main", Auth: Auth{Kind: AuthToken, Token: "ghs_secret"}}
	if err := g.Clone(context.Background(), spec, filepath.Join(dir, "work"), io.Discard); err != nil {
		t.Fatalf("Clone: %v", err)
	}
	got, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.SplitN(string(got), "\n", 2)[0]
	if strings.Contains(args, "ghs_secret") || !strings.Contains(args, "https://github.com/o/r.git") {
		t.Errorf("clone args = %q; want the plain URL and no token", args)
	}
	if !strings.Contains(string(got), "GIT_CONFIG_KEY_0=http.https://github.com/.extraheader") {
		t.Errorf("auth header config not passed via env:\n%s", got)
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

// GitHub answers a credential-less clone of a private or missing repository
// with 401. Run by systemd, git then failed asking for a username with no
// terminal ("could not read Username ...: No such device or address"); from
// a terminal it would wait for one. The clone must fail at once and say why.
func TestCloneReportsCredentialsRequired(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate", `Basic realm="GitHub"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	// No credential helper or askpass program on this machine may answer.
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_ASKPASS", "")
	t.Setenv("SSH_ASKPASS", "")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var out strings.Builder
	err := NewGit().Clone(ctx, CloneSpec{URL: srv.URL + "/o/r.git"}, filepath.Join(t.TempDir(), "work"), &out)
	if !errors.Is(err, ErrCredentialsRequired) {
		t.Fatalf("Clone = %v, want ErrCredentialsRequired; output:\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "fatal: could not read Username") {
		t.Errorf("git's own error should still reach the log:\n%s", out.String())
	}
}
