package gitrepo

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestInitIsIdempotentAndWritesHook(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	root := t.TempDir()
	m := New(root, "/usr/local/bin/outhaul", "/var/lib/outhaul/git-hook.sock")

	if err := m.Init("api"); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := m.Init("api"); err != nil {
		t.Fatalf("second Init: %v", err)
	}

	repo := filepath.Join(root, "api.git")
	if fi, err := os.Stat(filepath.Join(repo, "HEAD")); err != nil || fi.IsDir() {
		t.Fatalf("bare repo HEAD missing: %v", err)
	}
	hook := filepath.Join(repo, "hooks", "post-receive")
	data, err := os.ReadFile(hook)
	if err != nil {
		t.Fatalf("read hook: %v", err)
	}
	if !strings.Contains(string(data), "git-hook") || !strings.Contains(string(data), "api") {
		t.Fatalf("hook missing subcommand/app: %s", data)
	}
	if fi, _ := os.Stat(hook); fi.Mode()&0o111 == 0 {
		t.Fatalf("hook not executable: %v", fi.Mode())
	}
}

func TestShellQuote(t *testing.T) {
	cases := map[string]string{
		"api":     `'api'`,
		"a b":     `'a b'`,
		"":        `''`,
		"foo'bar": `'foo'\''bar'`,
	}
	for in, want := range cases {
		if got := shellQuote(in); got != want {
			t.Fatalf("shellQuote(%q) = %q; want %q", in, got, want)
		}
	}
}

func TestPathRejectsTraversal(t *testing.T) {
	m := New(t.TempDir(), "/bin/outhaul", "/tmp/s.sock")
	for _, bad := range []string{"../etc", "a/b", "/abs", "..", "", "a/../b"} {
		if _, err := m.Path(bad); err == nil {
			t.Fatalf("Path(%q) should reject", bad)
		}
	}
	if _, err := m.Path("api"); err != nil {
		t.Fatalf("Path(api) should be ok: %v", err)
	}
}

func TestRemove(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	root := t.TempDir()
	m := New(root, "/bin/outhaul", "/tmp/s.sock")
	if err := m.Init("gone"); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := m.Remove("gone"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "gone.git")); !os.IsNotExist(err) {
		t.Fatalf("repo dir should be gone, stat err = %v", err)
	}
	if err := m.Remove("gone"); err != nil {
		t.Fatalf("Remove absent: %v", err)
	}
}
