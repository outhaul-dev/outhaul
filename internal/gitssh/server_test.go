package gitssh

import (
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/james-smart/outhaul/internal/core"
	"github.com/james-smart/outhaul/internal/gitrepo"
	"github.com/james-smart/outhaul/internal/sshkey"
	"github.com/james-smart/outhaul/internal/store"
	"golang.org/x/crypto/ssh"
)

// testServer spins up a Server on 127.0.0.1:0 backed by a real store and a
// gitrepo.Manager whose hooks are a no-op (/bin/true). It returns the store and
// git root so callers can register keys and inspect bare repos.
func testServer(t *testing.T) (*Server, *store.Store, string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skip("ssh not on PATH")
	}

	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), nil)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	gitRoot := filepath.Join(t.TempDir(), "git")
	repos := gitrepo.New(gitRoot, "/bin/true", filepath.Join(t.TempDir(), "s.sock"))

	priv, _, err := sshkey.Generate()
	if err != nil {
		t.Fatalf("host key: %v", err)
	}
	signer, err := ssh.ParsePrivateKey([]byte(priv))
	if err != nil {
		t.Fatalf("parse host key: %v", err)
	}

	srv := New(signer, st, repos)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = srv.Serve(ctx, "127.0.0.1:0") }()
	waitAddr(t, srv)
	return srv, st, gitRoot
}

// waitAddr polls until the server reports a listen address.
func waitAddr(t *testing.T, srv *Server) string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if a := srv.Addr(); a != "" {
			return a
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("server never reported an address")
	return ""
}

// registerKey generates a client keypair, registers its public key in the store,
// and returns a path to the private key file (mode 0600).
func registerKey(t *testing.T, st *store.Store) string {
	t.Helper()
	cpriv, cpub, err := sshkey.Generate()
	if err != nil {
		t.Fatalf("client key: %v", err)
	}
	pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(cpub))
	if err != nil {
		t.Fatalf("parse client pub: %v", err)
	}
	fp := ssh.FingerprintSHA256(pub)
	if _, err := st.AddPushKey(context.Background(), core.PushKey{
		Label: "t", PublicKey: cpub, Fingerprint: fp,
	}); err != nil {
		t.Fatalf("AddPushKey: %v", err)
	}
	return writeKeyFile(t, cpriv)
}

// unregisteredKey generates a client keypair but does NOT register it.
func unregisteredKey(t *testing.T) string {
	t.Helper()
	cpriv, _, err := sshkey.Generate()
	if err != nil {
		t.Fatalf("client key: %v", err)
	}
	return writeKeyFile(t, cpriv)
}

func writeKeyFile(t *testing.T, pem string) string {
	t.Helper()
	f := filepath.Join(t.TempDir(), "id_ed25519")
	if err := os.WriteFile(f, []byte(pem), 0o600); err != nil {
		t.Fatalf("write key file: %v", err)
	}
	return f
}

// gitEnv returns an environment slice pointing GIT_SSH_COMMAND at the given key
// file and server port, disabling host-key checks for the throwaway host key.
func gitEnv(keyFile, addr string) []string {
	_, port, _ := net.SplitHostPort(addr)
	sshCmd := "ssh -i " + keyFile +
		" -o IdentitiesOnly=yes -o StrictHostKeyChecking=no" +
		" -o UserKnownHostsFile=/dev/null -p " + port
	return append(os.Environ(),
		"GIT_SSH_COMMAND="+sshCmd,
		"GIT_TERMINAL_PROMPT=0",
	)
}

func remoteURL(addr, app string) string {
	host, port, _ := net.SplitHostPort(addr)
	return "ssh://git@" + host + ":" + port + "/" + app
}

func runGit(env []string, dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Env = env
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestAuthRejectsUnknownKeyAcceptsKnown(t *testing.T) {
	srv, st, _ := testServer(t)
	addr := srv.Addr()

	goodKey := registerKey(t, st)
	badKey := unregisteredKey(t)

	// Unregistered key: ls-remote must fail (permission denied).
	if out, err := runGit(gitEnv(badKey, addr), "", "ls-remote", remoteURL(addr, "api")); err == nil {
		t.Fatalf("ls-remote with unregistered key should fail; output:\n%s", out)
	}

	// Registered key: ls-remote against the (lazily created) empty repo succeeds.
	if out, err := runGit(gitEnv(goodKey, addr), "", "ls-remote", remoteURL(addr, "api")); err != nil {
		t.Fatalf("ls-remote with registered key failed: %v\n%s", err, out)
	}
}

func TestPushTransfersObjects(t *testing.T) {
	srv, st, gitRoot := testServer(t)
	addr := srv.Addr()
	key := registerKey(t, st)
	env := gitEnv(key, addr)

	// Build a small local repo with one commit on main.
	local := filepath.Join(t.TempDir(), "local")
	if err := os.MkdirAll(local, 0o755); err != nil {
		t.Fatal(err)
	}
	seed := [][]string{
		{"init", "-q", "-b", "main"},
		{"config", "user.email", "t@example.com"},
		{"config", "user.name", "t"},
		{"config", "commit.gpgsign", "false"},
	}
	for _, a := range seed {
		if out, err := runGit(env, local, a...); err != nil {
			t.Fatalf("git %v: %v\n%s", a, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(local, "README"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, a := range [][]string{{"add", "-A"}, {"commit", "-q", "-m", "init"}} {
		if out, err := runGit(env, local, a...); err != nil {
			t.Fatalf("git %v: %v\n%s", a, err, out)
		}
	}

	// Push main to the server; the no-op hook lets it exit 0.
	if out, err := runGit(env, local, "push", remoteURL(addr, "api"), "main"); err != nil {
		t.Fatalf("git push failed: %v\n%s", err, out)
	}

	// The bare repo must now resolve refs/heads/main.
	bare := filepath.Join(gitRoot, "api.git")
	out, err := runGit(os.Environ(), "", "-C", bare, "rev-parse", "refs/heads/main")
	if err != nil {
		t.Fatalf("bare repo missing main: %v\n%s", err, out)
	}
	if strings.TrimSpace(out) == "" {
		t.Fatal("empty rev-parse output")
	}
}

func TestRebindMovesListener(t *testing.T) {
	srv, st, _ := testServer(t)
	key := registerKey(t, st)

	// Rebind to a fresh ephemeral port; the new address must serve.
	if err := srv.Rebind("127.0.0.1:0"); err != nil {
		t.Fatalf("Rebind to fresh port: %v", err)
	}
	newAddr := srv.Addr()
	if out, err := runGit(gitEnv(key, newAddr), "", "ls-remote", remoteURL(newAddr, "api")); err != nil {
		t.Fatalf("ls-remote after rebind failed: %v\n%s", err, out)
	}

	// Rebind to an address already in use must error and leave the old listener.
	blocker, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("blocker listen: %v", err)
	}
	defer blocker.Close()
	if err := srv.Rebind(blocker.Addr().String()); err == nil {
		t.Fatal("Rebind to in-use address should error")
	}

	// The current (still newAddr) listener must keep working.
	if srv.Addr() != newAddr {
		t.Fatalf("Addr changed after failed rebind: got %q want %q", srv.Addr(), newAddr)
	}
	if out, err := runGit(gitEnv(key, newAddr), "", "ls-remote", remoteURL(newAddr, "api")); err != nil {
		t.Fatalf("ls-remote after failed rebind failed: %v\n%s", err, out)
	}
}

// fakeChannel is a minimal ssh.Channel for exercising runGit at the unit level.
// Only Stderr() is meaningful; the git subprocess is never started in the paths
// that use it here.
type fakeChannel struct{ stderr bytes.Buffer }

func (c *fakeChannel) Read([]byte) (int, error)    { return 0, io.EOF }
func (c *fakeChannel) Write(p []byte) (int, error) { return len(p), nil }
func (c *fakeChannel) Close() error                { return nil }
func (c *fakeChannel) CloseWrite() error           { return nil }
func (c *fakeChannel) SendRequest(string, bool, []byte) (bool, error) {
	return false, nil
}
func (c *fakeChannel) Stderr() io.ReadWriter { return &c.stderr }

// TestRunGitRejectsNonPushApp verifies that an existing app whose Source is not
// "push" is refused before any git subprocess runs, and that its bare repo is
// never created.
func TestRunGitRejectsNonPushApp(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	ctx := context.Background()

	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), nil)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()
	if _, err := st.CreateApp(ctx, core.App{Name: "pub", Source: core.SourcePublic}); err != nil {
		t.Fatalf("CreateApp: %v", err)
	}

	gitRoot := filepath.Join(t.TempDir(), "git")
	repos := gitrepo.New(gitRoot, "/bin/true", filepath.Join(t.TempDir(), "s.sock"))
	priv, _, _ := sshkey.Generate()
	signer, _ := ssh.ParsePrivateKey([]byte(priv))
	srv := New(signer, st, repos)

	ch := &fakeChannel{}
	code := srv.runGit(ctx, ch, "git-receive-pack 'pub'", "SHA256:test")
	if code != 1 {
		t.Fatalf("runGit returned %d; want 1", code)
	}
	if !strings.Contains(ch.stderr.String(), "not push-deployable") {
		t.Fatalf("stderr = %q; want 'not push-deployable'", ch.stderr.String())
	}
	// The rejected app must not have had a bare repo created.
	if _, err := os.Stat(filepath.Join(gitRoot, "pub.git")); !os.IsNotExist(err) {
		t.Fatalf("bare repo should not exist for rejected app; stat err = %v", err)
	}
}
