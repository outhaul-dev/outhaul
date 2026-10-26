package deploy

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

// tagRepo makes a repository whose tag v1.0.0 (annotated) points at a commit
// where v.txt is "one", while main has moved on to "two".
func tagRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	src := t.TempDir()
	gitRun(t, src, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(src, "v.txt"), []byte("one"), 0o644)
	gitRun(t, src, "add", ".")
	gitRun(t, src, "commit", "-q", "-m", "a")
	gitRun(t, src, "tag", "-a", "v1.0.0", "-m", "r")
	os.WriteFile(filepath.Join(src, "v.txt"), []byte("two"), 0o644)
	gitRun(t, src, "commit", "-q", "-am", "b")
	return src
}

func cloneTag(t *testing.T, url string) {
	t.Helper()
	dst := filepath.Join(t.TempDir(), "work")
	spec := CloneSpec{URL: url, Branch: "refs/tags/v1.0.0"}
	if err := NewGit().Clone(context.Background(), spec, dst, io.Discard); err != nil {
		t.Fatalf("Clone: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dst, "v.txt"))
	if err != nil || string(got) != "one" {
		t.Fatalf("v.txt = %q (err %v), want the tag's content", got, err)
	}
}

func TestCloneChecksOutAnnotatedTag(t *testing.T) {
	src := tagRepo(t)
	cloneTag(t, "file://"+src)
}

// A branch sharing the tag's name must not be what a tag deploy builds.
func TestCloneTagNotSameNamedBranch(t *testing.T) {
	src := tagRepo(t)
	gitRun(t, src, "branch", "v1.0.0")
	cloneTag(t, "file://"+src)
}

func TestCloneTagFromBareRepo(t *testing.T) {
	src := tagRepo(t)
	bare := filepath.Join(t.TempDir(), "bare.git")
	gitRun(t, src, "clone", "-q", "--bare", src, bare)
	cloneTag(t, bare)
}

func TestListTags(t *testing.T) {
	src := tagRepo(t)
	gitRun(t, src, "tag", "v1.1.0")
	got, err := NewGit().ListTags(context.Background(), CloneSpec{URL: "file://" + src})
	if err != nil {
		t.Fatalf("ListTags: %v", err)
	}
	sort.Strings(got)
	if want := []string{"v1.0.0", "v1.1.0"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("tags = %v, want %v", got, want)
	}
}
