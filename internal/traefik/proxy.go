package traefik

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/james-smart/outhaul/internal/docker"
)

// DefaultDockerSocket is the host path to the Docker socket that Traefik's
// Docker provider watches.
const DefaultDockerSocket = "/var/run/docker.sock"

// proxyStopTimeout bounds how long we wait for a drifted Traefik container to
// stop gracefully before it is removed and recreated.
const proxyStopTimeout = 10 * time.Second

// ProxyConfig parameterises the managed Traefik container.
type ProxyConfig struct {
	ContainerName string // e.g. "outhaul-traefik"
	Image         string // e.g. "traefik:v3.3"
	Network       string // shared network app containers also join
	HTTPPort      string // host port for the web entrypoint, e.g. "80"
	DockerSocket  string // host docker socket path; defaults to DefaultDockerSocket

	TLSEnabled     bool
	ACMEEmail      string
	ACMEStaging    bool
	HTTPSPort      string // host port for :443
	ACMEStorageDir string // host dir bind-mounted for acme.json
}

// EnsureProxy makes the Traefik proxy present and running: it creates the shared
// network if missing, adopts an existing Traefik container (starting it if
// stopped), or pulls the image and creates one configured to use the Docker
// provider. It is idempotent and safe to call on every boot.
//
// If an existing container's configuration has drifted from the desired one
// (e.g. TLS was just enabled), it is stopped, removed, and recreated so the
// new flags actually take effect.
func EnsureProxy(ctx context.Context, dc docker.Client, pc ProxyConfig, logOut io.Writer) error {
	if pc.DockerSocket == "" {
		pc.DockerSocket = DefaultDockerSocket
	}
	if err := dc.EnsureNetwork(ctx, pc.Network); err != nil {
		return fmt.Errorf("ensure network %q: %w", pc.Network, err)
	}

	spec := proxySpec(pc)

	existing, err := dc.FindContainer(ctx, pc.ContainerName)
	if err != nil {
		return fmt.Errorf("find traefik container: %w", err)
	}
	// Adopt only if its config matches; otherwise recreate so new flags
	// (e.g. newly-enabled TLS) actually take effect.
	if existing != nil && existing.Labels["outhaul.config-hash"] == spec.Labels["outhaul.config-hash"] {
		if existing.Running() {
			return nil
		}
		return dc.StartContainer(ctx, existing.ID)
	}

	// (Re)create needed. Pull FIRST so a pull failure never leaves us with no proxy.
	if err := dc.PullImage(ctx, pc.Image, logOut); err != nil {
		return fmt.Errorf("pull traefik image: %w", err)
	}
	if existing != nil {
		if existing.Running() {
			_ = dc.StopContainer(ctx, existing.ID, proxyStopTimeout)
		}
		if err := dc.RemoveContainer(ctx, existing.ID, true); err != nil {
			return fmt.Errorf("remove drifted traefik: %w", err)
		}
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

// proxySpec builds the desired Traefik container spec (deterministic), stamping a
// config hash so drift can be detected on the next boot.
func proxySpec(pc ProxyConfig) docker.ContainerSpec {
	cmd := []string{
		"--providers.docker=true",
		"--providers.docker.exposedbydefault=false",
		"--entrypoints.web.address=:80",
	}
	ports := []docker.PortMapping{{HostPort: pc.HTTPPort, ContainerPort: "80", Proto: "tcp"}}
	mounts := []docker.Mount{{Source: pc.DockerSocket, Target: "/var/run/docker.sock", ReadOnly: true}}

	if pc.TLSEnabled {
		cmd = append(cmd,
			"--entrypoints.websecure.address=:443",
			"--entrypoints.web.http.redirections.entrypoint.to=websecure",
			"--entrypoints.web.http.redirections.entrypoint.scheme=https",
			"--certificatesresolvers.le.acme.httpchallenge=true",
			"--certificatesresolvers.le.acme.httpchallenge.entrypoint=web",
			"--certificatesresolvers.le.acme.email="+pc.ACMEEmail,
			"--certificatesresolvers.le.acme.storage=/etc/traefik/acme/acme.json",
		)
		if pc.ACMEStaging {
			cmd = append(cmd, "--certificatesresolvers.le.acme.caserver=https://acme-staging-v02.api.letsencrypt.org/directory")
		}
		ports = append(ports, docker.PortMapping{HostPort: pc.HTTPSPort, ContainerPort: "443", Proto: "tcp"})
		mounts = append(mounts, docker.Mount{Source: pc.ACMEStorageDir, Target: "/etc/traefik/acme", ReadOnly: false})
	}

	labels := map[string]string{
		"outhaul.managed":     "true",
		"outhaul.role":        "proxy",
		"outhaul.config-hash": hashConfig(pc.Image, cmd, ports, mounts),
	}
	return docker.ContainerSpec{
		Name:          pc.ContainerName,
		Image:         pc.Image,
		Cmd:           cmd,
		Ports:         ports,
		Mounts:        mounts,
		Networks:      []string{pc.Network},
		RestartPolicy: "unless-stopped",
		Labels:        labels,
	}
}

// hashConfig fingerprints the fields that define the desired Traefik container.
// IMPORTANT: include every ProxyConfig-derived field that should trigger a
// recreate when it changes (image, cmd, ports, mounts). Add new fields here.
func hashConfig(image string, cmd []string, ports []docker.PortMapping, mounts []docker.Mount) string {
	var sb strings.Builder
	sb.WriteString(image)
	sb.WriteByte('\n')
	sb.WriteString(strings.Join(cmd, " "))
	for _, p := range ports {
		fmt.Fprintf(&sb, "|%s:%s/%s", p.HostPort, p.ContainerPort, p.Proto)
	}
	for _, m := range mounts {
		fmt.Fprintf(&sb, "|%s:%s:%v", m.Source, m.Target, m.ReadOnly)
	}
	sum := sha256.Sum256([]byte(sb.String()))
	return hex.EncodeToString(sum[:])[:12]
}
