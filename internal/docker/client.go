// Package docker abstracts the container operations Outhaul needs behind a
// small Client interface. A real SDK-backed implementation drives the local
// Docker daemon; an in-memory Fake backs unit tests. No test touches a real
// daemon.
package docker

import (
	"context"
	"fmt"
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

// NetworkSpec describes the network EnsureNetwork creates when it is missing.
type NetworkSpec struct {
	Name    string // network name
	Subnet  string // IPv4 CIDR; "" lets Docker choose
	Gateway string // the bridge's address; "" unless Subnet is set
	IPRange string // the addresses Docker hands out; "" unless Subnet is set
}

// NetworkInfo describes an existing network's IPv4 configuration and the
// containers attached to it.
type NetworkInfo struct {
	Subnet  string            // first IPv4 IPAM subnet, "" when there is none
	Gateway string            // its gateway
	IPRange string            // its dynamic range
	IPv4s   map[string]string // attached container name -> IPv4 address
}

// NetworkMismatchError reports an existing network whose subnet, gateway or
// dynamic range differs from what the settings ask for. Outhaul never
// reconfigures a network that already exists, so this stops startup instead.
type NetworkMismatchError struct {
	Name string
	Have NetworkInfo
	Want NetworkSpec
}

// Error names both sets of values, saying when Docker chose the subnet.
func (e *NetworkMismatchError) Error() string {
	return fmt.Sprintf("network %q has %s; the settings ask for %s",
		e.Name,
		describeNetwork(e.Have.Subnet, e.Have.Gateway, e.Have.IPRange),
		describeNetwork(e.Want.Subnet, e.Want.Gateway, e.Want.IPRange))
}

func describeNetwork(subnet, gateway, ipRange string) string {
	switch {
	case subnet == "":
		return "a subnet Docker chose"
	case ipRange == "":
		return fmt.Sprintf("subnet %s and gateway %s, with no dynamic range", subnet, gateway)
	default:
		return fmt.Sprintf("subnet %s, gateway %s and range %s", subnet, gateway, ipRange)
	}
}

// ContainerSpec describes a container to create.
type ContainerSpec struct {
	Name          string
	Image         string
	Labels        map[string]string
	Env           []string
	Networks      []string
	IPv4          string // fixed address on the spec's first network
	Ports         []PortMapping
	Mounts        []Mount
	Cmd           []string
	RestartPolicy string   // e.g. "unless-stopped"; empty means Docker default
	ExtraHosts    []string // extra /etc/hosts entries, e.g. "host.docker.internal:host-gateway"
	NanoCPUs      int64    // billionths of a CPU (1 CPU = 1e9); 0 = none
	Memory        int64    // bytes; 0 = none
	PidsLimit     int64    // processes; 0 = none
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
	CPULimit   float64   // HostConfig.NanoCPUs/1e9, else CPUQuota/CPUPeriod; 0 = none
	MemLimited bool      // HostConfig.Memory > 0: MemLimit is the container's own
	Pids       uint64    // pids_stats.current
	PidsLimit  uint64    // HostConfig.PidsLimit when above 0; 0 = none
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

// VolumeInfo is a named volume with its labels, for inventory/attribution.
type VolumeInfo struct {
	Name   string
	Labels map[string]string
}

// Client is the container-runtime surface Outhaul depends on.
// ImageLabel marks every image Outhaul builds, so the dangling-image prune
// can tell its own leftovers from other images on a shared daemon. Docker
// copies image labels onto containers, so containers carry it too.
const ImageLabel = "outhaul.image=true"

type Client interface {
	// Ping verifies the daemon is reachable.
	Ping(ctx context.Context) error

	// ServerAPIVersion reports the daemon's supported Docker API version
	// (e.g. "1.44"). Empty if it cannot be determined. Used to pin
	// DOCKER_API_VERSION for the managed Traefik container, whose embedded
	// client otherwise defaults to a version modern daemons reject.
	ServerAPIVersion(ctx context.Context) string

	// NetworkGateway returns the IPv4 gateway of the named network: the host's
	// address on its bridge.
	NetworkGateway(ctx context.Context, name string) (string, error)

	// PullImage pulls ref, streaming progress to out (may be nil).
	PullImage(ctx context.Context, ref string, out io.Writer) error

	// EnsureNetwork creates the network described by spec if it does not
	// exist. A network it creates gets a host bridge interface of the same
	// name. When spec.Subnet is set, an existing network whose subnet,
	// gateway or dynamic range differs is a *NetworkMismatchError.
	EnsureNetwork(ctx context.Context, spec NetworkSpec) error

	// InspectNetwork returns the named network's first IPv4 subnet, gateway
	// and dynamic range, and the IPv4 addresses of its attached containers.
	InspectNetwork(ctx context.Context, name string) (NetworkInfo, error)

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

	// RenameContainer gives a container a new name. A running container may be
	// renamed. Used when a reprovisioned database's replacement takes over the
	// canonical name once the old container is gone.
	RenameContainer(ctx context.Context, id, name string) error

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

	// CreateVolume idempotently creates a named volume with the given labels.
	// Creating a volume that already exists is not an error.
	CreateVolume(ctx context.Context, name string, labels map[string]string) error

	// RemoveVolume removes a named volume. force removes it even if it is
	// referenced; a volume that does not exist is treated as success.
	RemoveVolume(ctx context.Context, name string, force bool) error

	// ListVolumesFull is ListVolumes with labels returned. A match value of ""
	// matches the presence of that label key with any value.
	ListVolumesFull(ctx context.Context, match map[string]string) ([]VolumeInfo, error)

	// RunContainer runs a one-shot container to completion: create from spec,
	// stream its stdout/stderr to the writers (either may be nil) while it
	// runs, wait for exit, remove it, and return the exit code. Used for
	// backup helper containers that tar a volume to stdout.
	RunContainer(ctx context.Context, spec ContainerSpec, stdout, stderr io.Writer) (int, error)

	// ListImages returns the local image tags matching a reference pattern
	// (e.g. "outhaul/*"), one entry per matching tag. Used by the image
	// pruner to reconcile the outhaul/* namespace against the database.
	ListImages(ctx context.Context, refPattern string) ([]string, error)

	// ImageLabels returns the labels baked into a local image's config.
	ImageLabels(ctx context.Context, ref string) (map[string]string, error)
	// RemoveImage untags ref and deletes the underlying image when that was
	// its last tag. A ref that does not exist is success (the desired state);
	// an image in use by a container is an error — the pruner never forces.
	RemoveImage(ctx context.Context, ref string) error

	// PruneImages removes dangling (untagged) images that Outhaul built
	// (see ImageLabel) — never tagged ones, and never another user's — and
	// returns the bytes reclaimed.
	PruneImages(ctx context.Context) (uint64, error)

	// PruneBuildCache removes build-cache entries unused for longer than
	// olderThan and returns the bytes reclaimed.
	PruneBuildCache(ctx context.Context, olderThan time.Duration) (uint64, error)

	// Close releases any client resources.
	Close() error
}
