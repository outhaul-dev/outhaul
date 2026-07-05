package builder

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
)

// Docker builds images from the repo's own Dockerfile by shelling out to
// `docker build`, the same way the Nixpacks strategy shells out to nixpacks.
// The build context is always the repo root: --file points at the configured
// Dockerfile, so COPY paths keep working for the common repo-root layout and
// a monorepo Dockerfile can still reference any file in the clone.
type Docker struct {
	// Bin is the docker executable; defaults to "docker" (looked up on PATH).
	Bin string
}

// NewDocker returns a Dockerfile builder using the docker CLI on PATH.
func NewDocker() *Docker { return &Docker{Bin: "docker"} }

func (d *Docker) Name() string { return "dockerfile" }

// Build runs `docker build <ctx> --file <ctx>/<dockerfile> --tag <tag>
// [--build-arg K=V ...]`, streaming output to out. A missing Dockerfile (the
// common operator error, checked first) or a missing docker binary is
// reported as a clear, actionable error before Docker gets a chance to print
// its own noise.
func (d *Docker) Build(ctx context.Context, req BuildRequest, out io.Writer) error {
	dockerfile := req.Dockerfile
	if dockerfile == "" {
		dockerfile = "Dockerfile"
	}
	if _, err := os.Stat(filepath.Join(req.ContextDir, dockerfile)); err != nil {
		return fmt.Errorf("no Dockerfile at %q in the repository — set the right path in the app's deploy settings: %w", dockerfile, err)
	}
	bin := d.Bin
	if bin == "" {
		bin = "docker"
	}
	if _, err := exec.LookPath(bin); err != nil {
		return fmt.Errorf("docker binary %q not found on PATH; install Docker to build Dockerfile apps: %w", bin, err)
	}

	cmd := exec.CommandContext(ctx, bin, dockerBuildArgs(req)...)
	cmd.Stdout = out
	cmd.Stderr = out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker build failed: %w", err)
	}
	return nil
}

// dockerBuildArgs assembles the docker CLI arguments. Env is passed as
// --build-arg (consuming one is opt-in: the Dockerfile declares ARG), sorted
// so the command line is deterministic (and testable).
func dockerBuildArgs(req BuildRequest) []string {
	dockerfile := req.Dockerfile
	if dockerfile == "" {
		dockerfile = "Dockerfile"
	}
	args := []string{"build", req.ContextDir,
		"--file", filepath.Join(req.ContextDir, dockerfile),
		"--tag", req.ImageTag}

	keys := make([]string, 0, len(req.Env))
	for k := range req.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, "--build-arg", k+"="+req.Env[k])
	}
	return args
}
