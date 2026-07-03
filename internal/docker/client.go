// Package docker abstracts the container operations Slipway needs behind a
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

// Mount is a host bind mount (used to give Traefik the Docker socket).
type Mount struct {
	Source   string
	Target   string
	ReadOnly bool
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

// Client is the container-runtime surface Slipway depends on.
type Client interface {
	// Ping verifies the daemon is reachable.
	Ping(ctx context.Context) error

	// PullImage pulls ref, streaming progress to out (may be nil).
	PullImage(ctx context.Context, ref string, out io.Writer) error

	// EnsureNetwork creates the named bridge network if it does not exist.
	EnsureNetwork(ctx context.Context, name string) error

	// FindContainer returns the container with the given name, or nil if none.
	FindContainer(ctx context.Context, name string) (*Container, error)

	// ContainerIP returns the container's IPv4 address on the named network, or
	// "" if it is not attached to that network.
	ContainerIP(ctx context.Context, id, network string) (string, error)

	// CreateContainer creates (does not start) a container and returns its ID.
	CreateContainer(ctx context.Context, spec ContainerSpec) (string, error)

	// StartContainer starts a created container.
	StartContainer(ctx context.Context, id string) error

	// StopContainer stops a running container, waiting up to timeout.
	StopContainer(ctx context.Context, id string, timeout time.Duration) error

	// RemoveContainer removes a container (force removes even if running).
	RemoveContainer(ctx context.Context, id string, force bool) error

	// Close releases any client resources.
	Close() error
}
