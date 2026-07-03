package traefik

import (
	"context"
	"fmt"
	"io"

	"github.com/slipwaydev/slipway/internal/docker"
)

// DefaultDockerSocket is the host path to the Docker socket that Traefik's
// Docker provider watches.
const DefaultDockerSocket = "/var/run/docker.sock"

// ProxyConfig parameterises the managed Traefik container.
type ProxyConfig struct {
	ContainerName string // e.g. "slipway-traefik"
	Image         string // e.g. "traefik:v3.3"
	Network       string // shared network app containers also join
	HTTPPort      string // host port for the web entrypoint, e.g. "80"
	DockerSocket  string // host docker socket path; defaults to DefaultDockerSocket
}

// EnsureProxy makes the Traefik proxy present and running: it creates the shared
// network if missing, adopts an existing Traefik container (starting it if
// stopped), or pulls the image and creates one configured to use the Docker
// provider. It is idempotent and safe to call on every boot.
func EnsureProxy(ctx context.Context, dc docker.Client, pc ProxyConfig, logOut io.Writer) error {
	if pc.DockerSocket == "" {
		pc.DockerSocket = DefaultDockerSocket
	}

	if err := dc.EnsureNetwork(ctx, pc.Network); err != nil {
		return fmt.Errorf("ensure network %q: %w", pc.Network, err)
	}

	existing, err := dc.FindContainer(ctx, pc.ContainerName)
	if err != nil {
		return fmt.Errorf("find traefik container: %w", err)
	}
	if existing != nil {
		if existing.Running() {
			return nil // already up; adopt it
		}
		if err := dc.StartContainer(ctx, existing.ID); err != nil {
			return fmt.Errorf("start existing traefik: %w", err)
		}
		return nil
	}

	if err := dc.PullImage(ctx, pc.Image, logOut); err != nil {
		return fmt.Errorf("pull traefik image: %w", err)
	}

	spec := docker.ContainerSpec{
		Name:  pc.ContainerName,
		Image: pc.Image,
		Cmd: []string{
			"--providers.docker=true",
			"--providers.docker.exposedbydefault=false",
			"--entrypoints.web.address=:80",
		},
		Ports: []docker.PortMapping{
			{HostPort: pc.HTTPPort, ContainerPort: "80", Proto: "tcp"},
		},
		Mounts: []docker.Mount{
			{Source: pc.DockerSocket, Target: "/var/run/docker.sock", ReadOnly: true},
		},
		Networks:      []string{pc.Network},
		RestartPolicy: "unless-stopped",
		Labels: map[string]string{
			"slipway.managed": "true",
			"slipway.role":    "proxy",
		},
	}

	id, err := dc.CreateContainer(ctx, spec)
	if err != nil {
		return fmt.Errorf("create traefik: %w", err)
	}
	if err := dc.StartContainer(ctx, id); err != nil {
		return fmt.Errorf("start traefik: %w", err)
	}
	return nil
}
