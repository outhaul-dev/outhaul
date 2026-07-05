// Package docker abstracts the container operations Outhaul needs behind a
// small Client interface. A real SDK-backed implementation drives the local
// Docker daemon; an in-memory Fake backs unit tests. No test touches a real
// daemon.
package docker

import (
	"context"
	"io"
	"time"
)

// PortMapping publishes a container port on the host (used for Traefik's :80).
type PortMapping struct {
	HostPort      string // e.g. "80"
	ContainerPort string // e.g. "80"
	Proto         string // "tcp" (default) or "udp"
}

// Mount attaches storage to a container: a host bind mount by default (used
// to give Traefik the Docker socket and databases their data dirs), or a
// named Docker volume when Volume is set (used by backup helper containers).
type Mount struct {
	Source   string // host path, or the volume name when Volume is true
	Target   string
	ReadOnly bool
	Volume   bool
}

// ContainerSpec describes a container to create.
type ContainerSpec struct {
	Name          string
	Image         string
	Labels        map[string]string
	Env           []string
	Networks      []string
	Ports         []PortMapping
	Mounts        []Mount
	Cmd           []string
	RestartPolicy string // e.g. "unless-stopped"; empty means Docker default
}

// Stats is a point-in-time sample of a running container's resource usage,
// with the same semantics as the docker stats CLI.
type Stats struct {
	CPUPercent float64   // 100 = one core fully busy (can exceed 100 on multi-core)
	MemUsage   uint64    // bytes, reclaimable page cache excluded
	MemLimit   uint64    // bytes; the host total when the container is unlimited
	NetRx      uint64    // cumulative bytes received across interfaces
	NetTx      uint64    // cumulative bytes sent
	StartedAt  time.Time // when the container last started (zero if unknown)
}

// Container is a minimal view of an existing container.
type Container struct {
	ID     string
	Name   string
	Image  string
	State  string // "running", "exited", "created", ...
	Labels map[string]string
}

// Running reports whether the container is currently running.
func (c Container) Running() bool { return c.State == "running" }

// Client is the container-runtime surface Outhaul depends on.
type Client interface {
	// Ping verifies the daemon is reachable.
	Ping(ctx context.Context) error

	// PullImage pulls ref, streaming progress to out (may be nil).
	PullImage(ctx context.Context, ref string, out io.Writer) error

	// EnsureNetwork creates the named bridge network if it does not exist.
	EnsureNetwork(ctx context.Context, name string) error

	// FindContainer returns the container with the given name, or nil if none.
	FindContainer(ctx context.Context, name string) (*Container, error)

	// ListContainers returns all containers (running or not) whose labels
	// include every key=value in match. Used to enumerate a compose stack via
	// its com.docker.compose.project label.
	ListContainers(ctx context.Context, match map[string]string) ([]Container, error)

	// ContainerIP returns the container's IPv4 address on the named network, or
	// "" if it is not attached to that network.
	ContainerIP(ctx context.Context, id, network string) (string, error)

	// ContainerLogs returns a stream of the container's stdout+stderr, starting
	// tail lines back and following until the container stops or the reader is
	// closed. The stream is plain text (Docker's multiplexing framing is
	// stripped).
	ContainerLogs(ctx context.Context, id string, tail int) (io.ReadCloser, error)

	// ContainerStats samples the container's live resource usage once. The
	// daemon primes CPU% with two internal readings, so a call takes about a
	// second.
	ContainerStats(ctx context.Context, id string) (Stats, error)

	// CreateContainer creates (does not start) a container and returns its ID.
	CreateContainer(ctx context.Context, spec ContainerSpec) (string, error)

	// StartContainer starts a created container.
	StartContainer(ctx context.Context, id string) error

	// StopContainer stops a running container, waiting up to timeout.
	StopContainer(ctx context.Context, id string, timeout time.Duration) error

	// RemoveContainer removes a container (force removes even if running).
	RemoveContainer(ctx context.Context, id string, force bool) error

	// ExecContainer runs cmd inside a running container with extra env vars,
	// feeding stdin to the command when non-nil (closed once the reader
	// drains) and streaming the command's stdout and stderr to the given
	// writers (either may be nil), and returns its exit code. Used to run the
	// dump and restore tools that ship inside database images (pg_dump,
	// pg_restore, mysqldump, mysql).
	ExecContainer(ctx context.Context, id string, cmd, env []string, stdin io.Reader, stdout, stderr io.Writer) (int, error)

	// ListVolumes returns the names of Docker volumes whose labels include
	// every key=value in match. Used to enumerate a compose stack's named
	// volumes via its com.docker.compose.project label.
	ListVolumes(ctx context.Context, match map[string]string) ([]string, error)

	// RunContainer runs a one-shot container to completion: create from spec,
	// stream its stdout/stderr to the writers (either may be nil) while it
	// runs, wait for exit, remove it, and return the exit code. Used for
	// backup helper containers that tar a volume to stdout.
	RunContainer(ctx context.Context, spec ContainerSpec, stdout, stderr io.Writer) (int, error)

	// ListImages returns the local image tags matching a reference pattern
	// (e.g. "outhaul/*"), one entry per matching tag. Used by the image
	// pruner to reconcile the outhaul/* namespace against the database.
	ListImages(ctx context.Context, refPattern string) ([]string, error)

	// RemoveImage untags ref and deletes the underlying image when that was
	// its last tag. A ref that does not exist is success (the desired state);
	// an image in use by a container is an error — the pruner never forces.
	RemoveImage(ctx context.Context, ref string) error

	// PruneImages removes dangling (untagged) images only — never tagged
	// ones — and returns the bytes reclaimed.
	PruneImages(ctx context.Context) (uint64, error)

	// PruneBuildCache removes build-cache entries unused for longer than
	// olderThan and returns the bytes reclaimed.
	PruneBuildCache(ctx context.Context, olderThan time.Duration) (uint64, error)

	// Close releases any client resources.
	Close() error
}
