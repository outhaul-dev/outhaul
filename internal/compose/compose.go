// Package compose runs docker compose stacks for compose-kind apps. Like the
// Nixpacks builder, the real implementation shells out to a binary on the
// host (`docker compose`, the v2 plugin) behind a small interface with an
// in-memory fake, so no test needs a Docker daemon.
package compose

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
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
	// Config runs `docker compose -p <project> -f <files...> config
	// --format json --no-env-resolution` in dir and returns stdout. Compose's
	// warnings go to warn (the deploy log).
	Config(ctx context.Context, dir string, files []string, project string, warn io.Writer) ([]byte, error)

	// Build runs `docker compose build` for the stack in dir.
	Build(ctx context.Context, dir string, files []string, project string, out io.Writer) error

	// Pull fetches the images `up` will use that the server does not already
	// have (`pull --policy missing`). A service that only builds is skipped
	// (`--ignore-buildable`): its image comes from `build`, not a registry.
	Pull(ctx context.Context, dir string, files []string, project string, out io.Writer) error

	// Up runs `docker compose up -d --wait`, blocking until every service is
	// running (and healthy, where a healthcheck is defined) or waitTimeout
	// elapses. A non-zero exit — build error, crash loop, health timeout — is
	// returned as an error.
	Up(ctx context.Context, dir string, files []string, project string, waitTimeout time.Duration, out io.Writer) error

	Stop(ctx context.Context, project string, out io.Writer) error
	Restart(ctx context.Context, project string, out io.Writer) error

	// Down removes the stack's containers, networks, and the images compose
	// built for it (--rmi local never touches registry images other stacks
	// share). Named volumes are deliberately kept (they are data); orphaned
	// containers are removed. Only called when the app is deleted.
	Down(ctx context.Context, project string, out io.Writer) error
}

// Docker is the real Runner: it shells out to `docker compose`.
type Docker struct {
	// Bin is the docker executable; defaults to "docker" (looked up on PATH).
	Bin string
	// Env, when non-nil, is the whole environment `docker compose` runs with.
	// nil inherits Outhaul's own, which is what a default install does. Under
	// the strict container policy it is RestrictedEnv(os.Environ()), so a
	// Compose file's ${NAME} cannot read Outhaul's settings or secrets.
	Env []string
}

// NewDocker returns a Runner using the docker binary on PATH.
func NewDocker() *Docker { return &Docker{Bin: "docker"} }

func (d *Docker) Build(ctx context.Context, dir string, files []string, project string, out io.Writer) error {
	return d.run(ctx, dir, out, buildArgs(files, project))
}

func (d *Docker) Pull(ctx context.Context, dir string, files []string, project string, out io.Writer) error {
	return d.run(ctx, dir, out, pullArgs(files, project))
}

// Config returns the stack's normalised configuration as JSON. Compose has
// interpolated every value, so the model can carry secrets and must never be
// logged. --no-env-resolution leaves the contents (and paths) of service
// env_file references unresolved; it does not stop interpolation. Compose's
// warnings still stream to warn; stdout is returned, not written.
func (d *Docker) Config(ctx context.Context, dir string, files []string, project string, warn io.Writer) ([]byte, error) {
	stdout, stderr, err := d.output(ctx, dir, warn, configArgs(files, project))
	if err != nil {
		// --no-env-resolution exists only from Compose 2.35. Name the version
		// rather than leave the user with an obscure "unknown flag".
		if s := string(stderr); strings.Contains(s, "--no-env-resolution") &&
			(strings.Contains(s, "unknown flag") || strings.Contains(s, "unknown shorthand flag")) {
			return nil, fmt.Errorf("reading a Compose file needs Docker Compose 2.35 or newer: %w", err)
		}
		// Compose writes why it could not read the file to stderr, which the
		// deploy log already has. Carrying the last line into the error puts
		// it in the deployment's reason too. stdout and the model never
		// appear here.
		if reason := lastStderrLine(stderr); reason != "" {
			return nil, fmt.Errorf("%w: %s", err, reason)
		}
		return nil, err
	}
	return stdout, nil
}

// lastStderrLine returns the last non-empty line of stderr, trimmed and capped
// so a long Compose diagnostic cannot fill the deployment's reason. The full
// stderr stays in the deploy log.
func lastStderrLine(stderr []byte) string {
	lines := strings.Split(string(stderr), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		if r := []rune(line); len(r) > 300 {
			line = strings.TrimSpace(string(r[:300]))
		}
		return line
	}
	return ""
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
	return d.run(ctx, "", out, []string{"compose", "-p", project, "down", "--remove-orphans", "--rmi", "local"})
}

// buildArgs, pullArgs and upArgs are split out so the exact command lines are
// testable. files are paths relative to the work dir; the first one's
// directory becomes the compose project directory (where compose reads .env),
// matching where the pipeline writes it.
func buildArgs(files []string, project string) []string {
	return append(fileArgs(files, project), "build")
}

func pullArgs(files []string, project string) []string {
	return append(fileArgs(files, project), "pull", "--policy", "missing", "--ignore-buildable")
}

// configArgs asks compose for the merged stack as JSON. --no-env-resolution
// leaves the contents (and paths) of service env_file references unresolved
// rather than reading them; variable interpolation still happens, so the
// model must never be logged.
func configArgs(files []string, project string) []string {
	return append(fileArgs(files, project), "config", "--format", "json", "--no-env-resolution")
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
	bin, err := d.binary()
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	if d.Env != nil {
		cmd.Env = withPWD(d.Env, dir)
	}
	cmd.Stdout = out
	cmd.Stderr = out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker %s failed: %w", args[0]+" "+lastVerb(args), err)
	}
	return nil
}

// output executes `docker <args...>` in dir and returns stdout and stderr
// separately. stderr is streamed to warn as it is written, so the deploy log
// still shows every warning, and kept for error classification; stdout is only
// returned, never logged.
func (d *Docker) output(ctx context.Context, dir string, warn io.Writer, args []string) ([]byte, []byte, error) {
	bin, err := d.binary()
	if err != nil {
		return nil, nil, err
	}
	if warn == nil {
		warn = io.Discard
	}
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	if d.Env != nil {
		cmd.Env = withPWD(d.Env, dir)
	}
	cmd.Stdout = &stdout
	cmd.Stderr = io.MultiWriter(warn, &stderr)
	if err := cmd.Run(); err != nil {
		return nil, stderr.Bytes(), fmt.Errorf("docker %s failed: %w", args[0]+" "+lastVerb(args), err)
	}
	return stdout.Bytes(), stderr.Bytes(), nil
}

// binary resolves the docker executable, reporting a missing one clearly.
func (d *Docker) binary() (string, error) {
	bin := d.Bin
	if bin == "" {
		bin = "docker"
	}
	if _, err := exec.LookPath(bin); err != nil {
		return "", fmt.Errorf("docker binary %q not found on PATH; compose apps need the docker CLI with the compose v2 plugin: %w", bin, err)
	}
	return bin, nil
}

// lastVerb names the compose verb for error messages (build, up, stop, ...).
func lastVerb(args []string) string {
	for i := len(args) - 1; i >= 0; i-- {
		switch args[i] {
		case "build", "up", "config", "stop", "restart", "down", "pull":
			return args[i]
		}
	}
	return ""
}
