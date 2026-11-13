package gitrepo

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
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

// hookFile is the hook's path in app's repository under m.
func hookFile(t *testing.T, m *Manager, app string) string {
	t.Helper()
	dir, err := m.Path(app)
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, "hooks", "post-receive")
}

// wantHook is the script Init writes for app under m.
func wantHook(m *Manager, app string) string {
	return "#!/bin/sh\nexec " + shellQuote(m.binPath) + " git-hook " + shellQuote(app) + " " + shellQuote(m.sockPath) + "\n"
}

// checkHook fails unless app's hook holds exactly its script, is executable,
// and is alone in hooks/ but for git's samples.
func checkHook(t *testing.T, m *Manager, app string) {
	t.Helper()
	hook := hookFile(t, m, app)
	data, err := os.ReadFile(hook)
	if err != nil {
		t.Fatalf("read hook: %v", err)
	}
	if string(data) != wantHook(m, app) {
		t.Errorf("hook = %q, want %q", data, wantHook(m, app))
	}
	fi, err := os.Stat(hook)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o755 {
		t.Errorf("hook mode = %v, want 0755", fi.Mode().Perm())
	}
	entries, err := os.ReadDir(filepath.Dir(hook))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "post-receive.") {
			t.Errorf("hooks/ holds %s, left from a write", e.Name())
		}
	}
}

func TestInitRewritesAStaleHook(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	root := t.TempDir()
	old := New(root, "/opt/old/outhaul", "/var/lib/outhaul/old.sock")
	if err := old.Init("api"); err != nil {
		t.Fatalf("Init: %v", err)
	}
	m := New(root, "/usr/local/bin/outhaul", "/var/lib/outhaul/git-hook.sock")
	if err := m.Init("api"); err != nil {
		t.Fatalf("Init with a moved binary: %v", err)
	}
	checkHook(t, m, "api")

	// The right script, but not executable.
	if err := os.Chmod(hookFile(t, m, "api"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := m.Init("api"); err != nil {
		t.Fatalf("Init over a hook that isn't executable: %v", err)
	}
	checkHook(t, m, "api")
}

// A hook that already holds its script is left alone, so most requests write
// nothing.
func TestInitLeavesAGoodHookAlone(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	m := New(t.TempDir(), "/usr/local/bin/outhaul", "/var/lib/outhaul/git-hook.sock")
	if err := m.Init("api"); err != nil {
		t.Fatalf("Init: %v", err)
	}
	hook := hookFile(t, m, "api")
	then := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := os.Chtimes(hook, then, then); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(hook)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Init("api"); err != nil {
		t.Fatalf("second Init: %v", err)
	}
	after, err := os.Stat(hook)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(then) {
		t.Errorf("the hook's modification time moved from %v to %v: it was written again", then, after.ModTime())
	}
	if !os.SameFile(before, after) {
		t.Error("the hook is another file: it was replaced")
	}
	checkHook(t, m, "api")
}

// A push whose hook starts while Init rewrites it must never run half a
// script: a reader sees the whole old script or the whole new one.
func TestInitNeverShowsHalfAHook(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	root := t.TempDir()
	// Two binaries' scripts, so every Init has something to write.
	a := New(root, "/usr/local/bin/outhaul", "/var/lib/outhaul/git-hook.sock")
	b := New(root, "/opt/outhaul/"+strings.Repeat("b", 200)+"/outhaul", "/run/outhaul/hook.sock")
	if err := a.Init("api"); err != nil {
		t.Fatalf("Init: %v", err)
	}
	hook := hookFile(t, a, "api")
	whole := map[string]bool{wantHook(a, "api"): true, wantHook(b, "api"): true}

	var stop atomic.Bool
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; !stop.Load(); i++ {
			m := a
			if i%2 == 1 {
				m = b
			}
			if err := m.Init("api"); err != nil {
				t.Errorf("Init: %v", err)
				return
			}
		}
	}()
	deadline := time.Now().Add(time.Second)
	var bad string
	reads := 0
	for time.Now().Before(deadline) && bad == "" {
		data, err := os.ReadFile(hook)
		if err != nil {
			bad = "an error: " + err.Error()
			break
		}
		reads++
		if !whole[string(data)] {
			bad = strconv.Quote(string(data))
		}
	}
	stop.Store(true)
	wg.Wait()
	if bad != "" {
		t.Fatalf("after %d whole reads, a read of the hook gave %s", reads, bad)
	}
	if err := b.Init("api"); err != nil {
		t.Fatal(err)
	}
	checkHook(t, b, "api")
}

// isolateGit keeps git in this test off the developer's own configuration.
func isolateGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(home, ".gitconfig"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
}

// bare runs git on the bare repository dir and returns its trimmed output.
func bare(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"--git-dir", dir, "-c", "user.name=Test", "-c", "user.email=test@example.com"}, args...)...)
	cmd.Stdin = strings.NewReader("")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// commitOn makes a commit on ref in dir and returns its id.
func commitOn(t *testing.T, dir, ref string) string {
	t.Helper()
	tree := bare(t, dir, "mktree")
	c := bare(t, dir, "commit-tree", tree, "-m", "on "+ref)
	bare(t, dir, "update-ref", ref, c)
	return c
}

// headOf is the ref HEAD names, or "detached".
func headOf(t *testing.T, dir string) string {
	t.Helper()
	out, err := exec.Command("git", "--git-dir", dir, "symbolic-ref", "-q", "HEAD").Output()
	if err != nil {
		return "detached"
	}
	return strings.TrimSpace(string(out))
}

func TestPointHead(t *testing.T) {
	isolateGit(t)
	cases := []struct {
		name   string
		branch string
		setup  func(t *testing.T, dir string) // makes refs and sets HEAD
		want   string                         // HEAD afterwards
	}{
		{"HEAD names a missing branch and the app's exists", "main", func(t *testing.T, dir string) {
			commitOn(t, dir, "refs/heads/main")
			bare(t, dir, "symbolic-ref", "HEAD", "refs/heads/nonesuch")
		}, "refs/heads/main"},
		{"a branch in a folder", "feature/x", func(t *testing.T, dir string) {
			commitOn(t, dir, "refs/heads/feature/x")
			bare(t, dir, "symbolic-ref", "HEAD", "refs/heads/nonesuch")
		}, "refs/heads/feature/x"},
		{"HEAD's branch exists", "main", func(t *testing.T, dir string) {
			commitOn(t, dir, "refs/heads/main")
			commitOn(t, dir, "refs/heads/dev")
			bare(t, dir, "symbolic-ref", "HEAD", "refs/heads/dev")
		}, "refs/heads/dev"},
		{"no branch", "", func(t *testing.T, dir string) {
			commitOn(t, dir, "refs/heads/main")
			bare(t, dir, "symbolic-ref", "HEAD", "refs/heads/nonesuch")
		}, "refs/heads/nonesuch"},
		{"the app's branch has no commits", "main", func(t *testing.T, dir string) {
			commitOn(t, dir, "refs/heads/dev")
			bare(t, dir, "symbolic-ref", "HEAD", "refs/heads/nonesuch")
		}, "refs/heads/nonesuch"},
		{"an empty repository", "main", func(t *testing.T, dir string) {
			bare(t, dir, "symbolic-ref", "HEAD", "refs/heads/nonesuch")
		}, "refs/heads/nonesuch"},
		{"HEAD detached", "main", func(t *testing.T, dir string) {
			c := commitOn(t, dir, "refs/heads/main")
			bare(t, dir, "update-ref", "--no-deref", "HEAD", c)
		}, "detached"},
		// git keeps a ref named refs/heads/-x, but -x isn't a branch name.
		{"a branch that starts with a dash", "-x", func(t *testing.T, dir string) {
			commitOn(t, dir, "refs/heads/-x")
			bare(t, dir, "symbolic-ref", "HEAD", "refs/heads/nonesuch")
		}, "refs/heads/nonesuch"},
		{"a branch git wouldn't name", "@{-1}", func(t *testing.T, dir string) {
			commitOn(t, dir, "refs/heads/main")
			bare(t, dir, "symbolic-ref", "HEAD", "refs/heads/nonesuch")
		}, "refs/heads/nonesuch"},
		{"a branch named HEAD", "HEAD", func(t *testing.T, dir string) {
			commitOn(t, dir, "refs/heads/HEAD")
			bare(t, dir, "symbolic-ref", "HEAD", "refs/heads/nonesuch")
		}, "refs/heads/nonesuch"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := New(t.TempDir(), "/usr/local/bin/outhaul", "/var/lib/outhaul/git-hook.sock")
			if err := m.Init("api"); err != nil {
				t.Fatalf("Init: %v", err)
			}
			dir, err := m.Path("api")
			if err != nil {
				t.Fatal(err)
			}
			c.setup(t, dir)
			if err := m.PointHead("api", c.branch); err != nil {
				t.Fatalf("PointHead(%q): %v", c.branch, err)
			}
			if got := headOf(t, dir); got != c.want {
				t.Errorf("HEAD = %s, want %s", got, c.want)
			}
		})
	}
}

func TestPointHeadRefusesABadName(t *testing.T) {
	m := New(t.TempDir(), "/bin/outhaul", "/tmp/s.sock")
	if err := m.PointHead("../etc", "main"); err == nil {
		t.Error("PointHead(../etc) = nil, want an error")
	}
}

func TestExists(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	m := New(t.TempDir(), "/usr/local/bin/outhaul", "/var/lib/outhaul/git-hook.sock")
	if m.Exists("api") {
		t.Fatal("Exists before Init = true")
	}
	if err := m.Init("api"); err != nil {
		t.Fatal(err)
	}
	if !m.Exists("api") {
		t.Fatal("Exists after Init = false")
	}
	if err := m.Remove("api"); err != nil {
		t.Fatal(err)
	}
	if m.Exists("api") {
		t.Fatal("Exists after Remove = true")
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
