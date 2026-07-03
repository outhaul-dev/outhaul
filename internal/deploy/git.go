package deploy

import (
	"context"
	"fmt"
	"io"
	"os/exec"
)

// Cloner checks out a repository into a directory. Behind an interface so the
// pipeline can be tested with a fake that writes a fixture tree.
type Cloner interface {
	Clone(ctx context.Context, repoURL, dir string, out io.Writer) error
}

// Git clones public repositories by shelling out to `git`.
type Git struct {
	// Bin is the git executable; defaults to "git".
	Bin string
}

// NewGit returns a Git cloner using the binary on PATH.
func NewGit() *Git { return &Git{Bin: "git"} }

// Clone performs a shallow single-branch clone of repoURL into dir.
func (g *Git) Clone(ctx context.Context, repoURL, dir string, out io.Writer) error {
	bin := g.Bin
	if bin == "" {
		bin = "git"
	}
	if _, err := exec.LookPath(bin); err != nil {
		return fmt.Errorf("git binary %q not found on PATH: %w", bin, err)
	}
	cmd := exec.CommandContext(ctx, bin, cloneArgs(repoURL, dir)...)
	cmd.Stdout = out
	cmd.Stderr = out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git clone failed: %w", err)
	}
	return nil
}

// cloneArgs builds a shallow, single-branch clone command. M1 only supports
// public repos; credentials are a later seam.
func cloneArgs(repoURL, dir string) []string {
	return []string{"clone", "--depth", "1", "--single-branch", repoURL, dir}
}
