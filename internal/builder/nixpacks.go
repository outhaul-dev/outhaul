package builder

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"sort"
)

// Nixpacks builds images by shelling out to the `nixpacks` binary. Nixpacks
// detects the app's language/framework and produces a Docker image without a
// Dockerfile.
type Nixpacks struct {
	// Bin is the nixpacks executable; defaults to "nixpacks" (looked up on PATH).
	Bin string
}

// NewNixpacks returns a Nixpacks builder using the binary on PATH.
func NewNixpacks() *Nixpacks { return &Nixpacks{Bin: "nixpacks"} }

func (n *Nixpacks) Name() string { return "nixpacks" }

// Build runs `nixpacks build <ctx> --name <tag> [--env K=V ...]`, streaming
// output to out. A missing binary is reported as a clear, actionable error
// rather than a crash.
func (n *Nixpacks) Build(ctx context.Context, req BuildRequest, out io.Writer) error {
	bin := n.Bin
	if bin == "" {
		bin = "nixpacks"
	}
	if _, err := exec.LookPath(bin); err != nil {
		return fmt.Errorf("nixpacks binary %q not found on PATH; install it from https://nixpacks.com: %w", bin, err)
	}

	cmd := exec.CommandContext(ctx, bin, buildArgs(req)...)
	cmd.Stdout = out
	cmd.Stderr = out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("nixpacks build failed: %w", err)
	}
	return nil
}

// buildArgs assembles the nixpacks CLI arguments. Env keys are sorted so the
// command line is deterministic (and testable).
func buildArgs(req BuildRequest) []string {
	args := []string{"build", req.ContextDir, "--name", req.ImageTag}

	keys := make([]string, 0, len(req.Env))
	for k := range req.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, "--env", k+"="+req.Env[k])
	}
	return args
}
