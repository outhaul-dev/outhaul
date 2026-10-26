package deploy

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// AuthKind selects how a clone authenticates.
type AuthKind int

const (
	AuthNone  AuthKind = iota // public repo
	AuthSSH                   // SSH deploy key
	AuthToken                 // GitHub App installation token over HTTPS
)

// Auth carries clone credentials.
type Auth struct {
	Kind   AuthKind
	SSHKey string // decrypted OpenSSH private-key PEM (AuthSSH)
	Token  string // installation token (AuthToken)
}

// CloneSpec fully describes a clone.
type CloneSpec struct {
	URL    string
	Branch string
	Auth   Auth
}

// ErrCredentialsRequired reports that the git server asked for credentials
// the clone did not supply. Over https, GitHub answers that way both for a
// private repository and for one that does not exist.
var ErrCredentialsRequired = errors.New("the git server asked for credentials")

// Cloner checks out a repository into a directory. Behind an interface so the
// pipeline can be tested with a fake.
type Cloner interface {
	Clone(ctx context.Context, spec CloneSpec, dir string, out io.Writer) error
	// ListTags returns the repository's tag names, without refs/tags/.
	ListTags(ctx context.Context, spec CloneSpec) ([]string, error)
}

// Git clones by shelling out to `git`.
type Git struct {
	Bin string // git executable; defaults to "git"
}

// NewGit returns a Git cloner using the binary on PATH.
func NewGit() *Git { return &Git{Bin: "git"} }

// Clone performs a shallow single-branch clone per spec.
func (g *Git) Clone(ctx context.Context, spec CloneSpec, dir string, out io.Writer) error {
	bin, env, cleanup, err := g.gitEnv(spec)
	if err != nil {
		return err
	}
	defer cleanup()

	if out == nil {
		out = io.Discard
	}
	run := func(args ...string) error {
		cmd := exec.CommandContext(ctx, bin, args...)
		cmd.Env = env
		output := &tailWriter{w: out}
		cmd.Stdout = output
		cmd.Stderr = output
		if err := cmd.Run(); err != nil {
			if strings.Contains(string(output.tail), credentialsMissing) {
				return ErrCredentialsRequired
			}
			return err
		}
		return nil
	}
	if isRef(spec.Branch) {
		// `clone --branch` takes only branch and tag names. A full ref (a fork
		// PR's refs/pull/<n>/head) is fetched into an empty repo instead.
		for _, args := range refFetchArgs(spec, dir) {
			if err := run(args...); err != nil {
				return fmt.Errorf("git checkout of %s failed: %w", spec.Branch, err)
			}
		}
		return nil
	}
	if err := run(cloneArgs(spec, dir)...); err != nil {
		return fmt.Errorf("git clone failed: %w", err)
	}
	return nil
}

// gitEnv resolves the git binary and builds the environment for a git
// command that talks to spec.URL: no prompts, plus the token or SSH key.
// cleanup removes any temporary key file; it is always safe to call.
func (g *Git) gitEnv(spec CloneSpec) (bin string, env []string, cleanup func(), err error) {
	cleanup = func() {}
	bin = g.Bin
	if bin == "" {
		bin = "git"
	}
	if _, err := exec.LookPath(bin); err != nil {
		return "", nil, cleanup, fmt.Errorf("git binary %q not found on PATH: %w", bin, err)
	}

	// Nothing can answer a prompt here, so git must never ask: a server that
	// wants credentials then fails the clone at once, with an error line git
	// does not translate (see credentialsMissing).
	env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")

	switch spec.Auth.Kind {
	case AuthToken:
		env = append(env, tokenEnv(spec.URL, spec.Auth.Token)...)
	case AuthSSH:
		sshVars, c, err := sshEnv(spec.Auth.SSHKey)
		if err != nil {
			return "", nil, func() {}, fmt.Errorf("prepare ssh key: %w", err)
		}
		cleanup = c
		env = append(env, sshVars...)
	}
	return bin, env, cleanup, nil
}

// ListTags runs `git ls-remote --tags --refs` against spec.URL. --refs drops
// the peeled "^{}" entries of annotated tags.
func (g *Git) ListTags(ctx context.Context, spec CloneSpec) ([]string, error) {
	bin, env, cleanup, err := g.gitEnv(spec)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	cmd := exec.CommandContext(ctx, bin, "ls-remote", "--tags", "--refs", "--", spec.URL)
	cmd.Env = env
	var stderr strings.Builder
	cmd.Stderr = &stderr
	stdout, err := cmd.Output()
	if err != nil {
		if strings.Contains(stderr.String(), credentialsMissing) {
			return nil, ErrCredentialsRequired
		}
		return nil, fmt.Errorf("git ls-remote failed: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	var tags []string
	for _, line := range strings.Split(string(stdout), "\n") {
		_, ref, ok := strings.Cut(strings.TrimSpace(line), "\t")
		if name, found := strings.CutPrefix(ref, "refs/tags/"); ok && found && name != "" {
			tags = append(tags, name)
		}
	}
	return tags, nil
}

// credentialsMissing ends git's error when, with GIT_TERMINAL_PROMPT=0, a
// server asks for credentials: "fatal: could not read Username for
// 'https://github.com': terminal prompts disabled".
const credentialsMissing = "terminal prompts disabled"

// tailWriter passes git's output on to the log, keeping the last of it to
// read the error line from.
type tailWriter struct {
	w    io.Writer
	tail []byte
}

func (t *tailWriter) Write(p []byte) (int, error) {
	t.tail = append(t.tail, p...)
	if over := len(t.tail) - 1024; over > 0 {
		t.tail = t.tail[over:]
	}
	return t.w.Write(p)
}

// isRef reports whether branch is a full ref (refs/...) rather than a branch name.
func isRef(branch string) bool { return strings.HasPrefix(branch, "refs/") }

// refFetchArgs checks out a full ref shallowly: init an empty repo, fetch just
// that ref, and detach onto it.
func refFetchArgs(spec CloneSpec, dir string) [][]string {
	return [][]string{
		{"init", "-q", "--", dir},
		{"-C", dir, "fetch", "-q", "--depth", "1", "--", spec.URL, spec.Branch},
		{"-C", dir, "checkout", "-q", "--detach", "FETCH_HEAD"},
	}
}

// cloneArgs builds a shallow, single-branch clone command for the given spec.
func cloneArgs(spec CloneSpec, dir string) []string {
	args := []string{"clone", "--depth", "1", "--single-branch"}
	if spec.Branch != "" {
		args = append(args, "--branch", spec.Branch)
	}
	return append(args, "--", spec.URL, dir)
}

// tokenEnv authenticates an https GitHub clone with an installation token,
// passed as an Authorization header through git's environment config
// (GIT_CONFIG_COUNT, git 2.31+) and scoped to github.com. Unlike a token in
// the URL it is never written to the work tree's .git/config — which sits in
// the build context and could be COPY'd into the image — nor shown in ps.
// Other URLs get no credentials.
func tokenEnv(url, token string) []string {
	if !strings.HasPrefix(url, "https://github.com/") {
		return nil
	}
	basic := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
	return []string{
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http.https://github.com/.extraheader",
		"GIT_CONFIG_VALUE_0=Authorization: Basic " + basic,
	}
}

// sshEnv writes the private key to a temp 0600 file and returns env vars setting
// GIT_SSH_COMMAND to use it. The returned cleanup removes the file.
func sshEnv(privateKey string) (env []string, cleanup func(), err error) {
	f, err := os.CreateTemp("", "outhaul-deploy-key-*")
	if err != nil {
		return nil, func() {}, err
	}
	name := f.Name()
	cleanup = func() { _ = os.Remove(name) }
	if err := os.Chmod(name, 0o600); err != nil {
		f.Close()
		cleanup()
		return nil, func() {}, err
	}
	if _, err := f.WriteString(privateKey); err != nil {
		f.Close()
		cleanup()
		return nil, func() {}, err
	}
	if err := f.Close(); err != nil {
		cleanup()
		return nil, func() {}, err
	}
	cmd := fmt.Sprintf("ssh -i %s -o IdentitiesOnly=yes -o StrictHostKeyChecking=accept-new", name)
	return []string{"GIT_SSH_COMMAND=" + cmd}, cleanup, nil
}
