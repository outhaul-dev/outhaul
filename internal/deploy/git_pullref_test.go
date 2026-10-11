package deploy

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// gitRun runs git in dir with a throwaway identity, failing the test on error.
func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// A fork PR's commits live in the fork; the base repo exposes them only as
// refs/pull/<n>/head. `git clone --branch` takes branch or tag names only, so
// a preview of a fork PR must fetch that ref instead.
func TestCloneChecksOutPullRequestRef(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	src := t.TempDir()
	gitRun(t, src, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(src, "main.txt"), []byte("base"), 0o644)
	gitRun(t, src, "add", ".")
	gitRun(t, src, "commit", "-q", "-m", "base")
	gitRun(t, src, "checkout", "-q", "-b", "fork-work")
	os.WriteFile(filepath.Join(src, "fork.txt"), []byte("from the fork"), 0o644)
	gitRun(t, src, "add", ".")
	gitRun(t, src, "commit", "-q", "-m", "fork change")
	gitRun(t, src, "update-ref", "refs/pull/7/head", "fork-work")
	gitRun(t, src, "checkout", "-q", "main")
	gitRun(t, src, "branch", "-q", "-D", "fork-work") // only refs/pull/7/head holds the commit now

	dst := filepath.Join(t.TempDir(), "work")
	spec := CloneSpec{URL: "file://" + src, Branch: "refs/pull/7/head"}
	if err := NewGit().Clone(context.Background(), spec, dst, io.Discard); err != nil {
		t.Fatalf("Clone: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dst, "fork.txt"))
	if err != nil || string(got) != "from the fork" {
		t.Fatalf("fork.txt = %q (err %v), want the PR head's content", got, err)
	}
}
