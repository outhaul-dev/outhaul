// Package compose runs docker compose stacks for compose-kind apps. Like the
// Nixpacks builder, the real implementation shells out to a binary on the
// host (`docker compose`, the v2 plugin) behind a small interface with an
// in-memory fake, so no test needs a Docker daemon.
package compose

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"time"
)

// ProjectName is the compose project (-p) for an app's stack. The prefix
// keeps stacks clearly Outhaul-owned and unable to collide with the
// "outhaul-app-<name>" containers of nixpacks apps. App names are validated
// on creation to satisfy compose project-name rules.
func ProjectName(appName string) string {
	return "outhaul-" + appName
}

// Runner executes compose operations for one stack. Build and Up need the
// checked-out repo (dir + compose files); the lifecycle verbs deliberately
// take only the project name — compose rediscovers the stack from container
// labels, so stop/restart/down work long after the deploy's work dir is gone.
type Runner interface {
	// Build runs `docker compose build` for the stack in dir.
	Build(ctx context.Context, dir string, files []string, project string, out io.Writer) error

	// Up runs `docker compose up -d --wait`, blocking until every service is
	// running (and healthy, where a healthcheck is defined) or waitTimeout
	// elapses. A non-zero exit — build error, crash loop, health timeout — is
	// returned as an error.
	Up(ctx context.Context, dir string, files []string, project string, waitTimeout time.Duration, out io.Writer) error

	Stop(ctx context.Context, project string, out io.Writer) error
	Restart(ctx context.Context, project string, out io.Writer) error

	// Down removes the stack's containers and networks. Named volumes are
	// deliberately kept (they are data); orphaned containers are removed.
	Down(ctx context.Context, project string, out io.Writer) error
}

// Docker is the real Runner: it shells out to `docker compose`.
type Docker struct {
	// Bin is the docker executable; defaults to "docker" (looked up on PATH).
	Bin string
}

// NewDocker returns a Runner using the docker binary on PATH.
func NewDocker() *Docker { return &Docker{Bin: "docker"} }

func (d *Docker) Build(ctx context.Context, dir string, files []string, project string, out io.Writer) error {
	return d.run(ctx, dir, out, buildArgs(files, project))
}

func (d *Docker) Up(ctx context.Context, dir string, files []string, project string, waitTimeout time.Duration, out io.Writer) error {
	return d.run(ctx, dir, out, upArgs(files, project, waitTimeout))
}

func (d *Docker) Stop(ctx context.Context, project string, out io.Writer) error {
	return d.run(ctx, "", out, []string{"compose", "-p", project, "stop"})
}

func (d *Docker) Restart(ctx context.Context, project string, out io.Writer) error {
	return d.run(ctx, "", out, []string{"compose", "-p", project, "restart"})
}

func (d *Docker) Down(ctx context.Context, project string, out io.Writer) error {
	return d.run(ctx, "", out, []string{"compose", "-p", project, "down", "--remove-orphans"})
}

// buildArgs and upArgs are split out so the exact command lines are testable.
// files are paths relative to the work dir; the first one's directory becomes
// the compose project directory (where compose reads .env), matching where
// the pipeline writes it.
func buildArgs(files []string, project string) []string {
	return append(fileArgs(files, project), "build")
}

func upArgs(files []string, project string, waitTimeout time.Duration) []string {
	secs := int(waitTimeout.Seconds())
	if secs < 1 {
		secs = 1
	}
	return append(fileArgs(files, project),
		"up", "-d", "--wait", "--wait-timeout", strconv.Itoa(secs), "--remove-orphans")
}

func fileArgs(files []string, project string) []string {
	args := []string{"compose", "-p", project}
	for _, f := range files {
		args = append(args, "-f", f)
	}
	return args
}

// run executes `docker <args...>` in dir, streaming combined output to out. A
// missing binary is reported as a clear, actionable error rather than a crash.
func (d *Docker) run(ctx context.Context, dir string, out io.Writer, args []string) error {
	bin := d.Bin
	if bin == "" {
		bin = "docker"
	}
	if _, err := exec.LookPath(bin); err != nil {
		return fmt.Errorf("docker binary %q not found on PATH; compose apps need the docker CLI with the compose v2 plugin: %w", bin, err)
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	cmd.Stdout = out
	cmd.Stderr = out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker %s failed: %w", args[0]+" "+lastVerb(args), err)
	}
	return nil
}

// lastVerb names the compose verb for error messages (build, up, stop, ...).
func lastVerb(args []string) string {
	for i := len(args) - 1; i >= 0; i-- {
		switch args[i] {
		case "build", "up", "stop", "restart", "down":
			return args[i]
		}
	}
	return ""
}
