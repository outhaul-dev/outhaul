package deploy

import (
	"context"
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

// Cloner checks out a repository into a directory. Behind an interface so the
// pipeline can be tested with a fake.
type Cloner interface {
	Clone(ctx context.Context, spec CloneSpec, dir string, out io.Writer) error
}

// Git clones by shelling out to `git`.
type Git struct {
	Bin string // git executable; defaults to "git"
}

// NewGit returns a Git cloner using the binary on PATH.
func NewGit() *Git { return &Git{Bin: "git"} }

// Clone performs a shallow single-branch clone per spec.
func (g *Git) Clone(ctx context.Context, spec CloneSpec, dir string, out io.Writer) error {
	bin := g.Bin
	if bin == "" {
		bin = "git"
	}
	if _, err := exec.LookPath(bin); err != nil {
		return fmt.Errorf("git binary %q not found on PATH: %w", bin, err)
	}

	url := spec.URL
	env := os.Environ()

	switch spec.Auth.Kind {
	case AuthToken:
		url = tokenURL(url, spec.Auth.Token)
	case AuthSSH:
		sshVars, cleanup, err := sshEnv(spec.Auth.SSHKey)
		if err != nil {
			return fmt.Errorf("prepare ssh key: %w", err)
		}
		defer cleanup()
		env = append(env, sshVars...)
	}

	spec.URL = url
	cmd := exec.CommandContext(ctx, bin, cloneArgs(spec, dir)...)
	cmd.Env = env
	cmd.Stdout = out
	cmd.Stderr = out
	if err := cmd.Run(); err != nil {
		// Never echo the credentialed URL in errors.
		return fmt.Errorf("git clone failed: %w", err)
	}
	return nil
}

// cloneArgs builds a shallow, single-branch clone command for the given spec.
func cloneArgs(spec CloneSpec, dir string) []string {
	args := []string{"clone", "--depth", "1", "--single-branch"}
	if spec.Branch != "" {
		args = append(args, "--branch", spec.Branch)
	}
	return append(args, spec.URL, dir)
}

// tokenURL rewrites an https GitHub URL to embed an installation token. Other
// URLs are returned unchanged.
func tokenURL(url, token string) string {
	const p = "https://github.com/"
	if !strings.HasPrefix(url, p) {
		return url
	}
	return "https://x-access-token:" + token + "@github.com/" + strings.TrimPrefix(url, p)
}

// sshEnv writes the private key to a temp 0600 file and returns env vars setting
// GIT_SSH_COMMAND to use it. The returned cleanup removes the file.
func sshEnv(privateKey string) (env []string, cleanup func(), err error) {
	f, err := os.CreateTemp("", "slipway-deploy-key-*")
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
