package traefik

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/outhaul-dev/outhaul/internal/docker"
)

// DefaultDockerSocket is the host path to the Docker socket that Traefik's
// Docker provider watches.
const DefaultDockerSocket = "/var/run/docker.sock"

// fallbackDockerAPIVersion pins the Docker API version for Traefik's embedded
// client when the daemon's version can't be detected. Traefik otherwise
// defaults to an old version (1.24) that modern daemons (min 1.40) reject,
// which silently breaks its Docker provider — and therefore all routing.
const fallbackDockerAPIVersion = "1.44"

// adminDynamicFile is the file-provider config (under DynamicDir) that routes
// the admin UI over HTTPS. host.docker.internal resolves to the host gateway.
const adminDynamicFile = "outhaul-admin.yml"

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

	// DockerAPIVersion pins DOCKER_API_VERSION for Traefik's docker client.
	// Empty falls back to fallbackDockerAPIVersion.
	DockerAPIVersion string

	// Admin-UI routing: when AdminHost is set and TLS is enabled, Traefik's
	// file provider publishes an HTTPS router for the admin UI, reaching the
	// host process at host.docker.internal:AdminPort via the host gateway.
	AdminHost  string // hostname the admin UI is served under (from PublicURL)
	AdminPort  string // host port the admin UI listens on, e.g. "8080"
	DynamicDir string // host dir bind-mounted as Traefik's file provider

	// TunnelMode makes the tunnel the server-wide ingress: Traefik serves plain
	// HTTP on :80 for cloudflared to reach over the Docker network, publishes no
	// host ports, and runs no ACME/redirect (Cloudflare terminates TLS at its
	// edge). The admin route, if any, is published over plain HTTP.
	TunnelMode bool
}

// adminRoutingEnabled reports whether the admin UI should be published over
// HTTPS through Traefik. It needs TLS (for the cert), a hostname, and a dir
// to write the dynamic config into.
func (pc ProxyConfig) adminRoutingEnabled() bool {
	return (pc.TLSEnabled || pc.TunnelMode) && pc.AdminHost != "" && pc.DynamicDir != ""
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
	if err := writeAdminDynamicConfig(pc); err != nil {
		return fmt.Errorf("write admin dynamic config: %w", err)
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
	var ports []docker.PortMapping
	if !pc.TunnelMode {
		// In tunnel mode the only way in is the tunnel, so publish nothing.
		ports = []docker.PortMapping{{HostPort: pc.HTTPPort, ContainerPort: "80", Proto: "tcp"}}
	}
	mounts := []docker.Mount{{Source: pc.DockerSocket, Target: "/var/run/docker.sock", ReadOnly: true}}

	apiVersion := pc.DockerAPIVersion
	if apiVersion == "" {
		apiVersion = fallbackDockerAPIVersion
	}
	env := []string{"DOCKER_API_VERSION=" + apiVersion}
	var extraHosts []string

	if pc.adminRoutingEnabled() {
		// File provider carries the admin-UI router; host-gateway lets the
		// container reach the admin process listening on the host.
		cmd = append(cmd,
			"--providers.file.directory=/etc/traefik/dynamic",
			"--providers.file.watch=true",
		)
		mounts = append(mounts, docker.Mount{Source: pc.DynamicDir, Target: "/etc/traefik/dynamic", ReadOnly: true})
		extraHosts = append(extraHosts, "host.docker.internal:host-gateway")
	}

	if pc.TLSEnabled && !pc.TunnelMode {
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
		"outhaul.config-hash": hashConfig(pc.Image, cmd, ports, mounts, env, extraHosts, pc.TunnelMode),
	}
	return docker.ContainerSpec{
		Name:          pc.ContainerName,
		Image:         pc.Image,
		Cmd:           cmd,
		Env:           env,
		ExtraHosts:    extraHosts,
		Ports:         ports,
		Mounts:        mounts,
		Networks:      []string{pc.Network},
		RestartPolicy: "unless-stopped",
		Labels:        labels,
	}
}

// hashConfig fingerprints the fields that define the desired Traefik container.
// IMPORTANT: include every ProxyConfig-derived field that should trigger a
// recreate when it changes (image, cmd, ports, mounts, env, extra hosts). Add
// new fields here. The admin router's host/port live in the file-provider
// config instead, which Traefik hot-reloads — so they need not be hashed.
func hashConfig(image string, cmd []string, ports []docker.PortMapping, mounts []docker.Mount, env, extraHosts []string, tunnelMode bool) string {
	var sb strings.Builder
	sb.WriteString(image)
	sb.WriteByte('\n')
	fmt.Fprintf(&sb, "tunnel=%v\n", tunnelMode)
	sb.WriteString(strings.Join(cmd, " "))
	for _, p := range ports {
		fmt.Fprintf(&sb, "|%s:%s/%s", p.HostPort, p.ContainerPort, p.Proto)
	}
	for _, m := range mounts {
		fmt.Fprintf(&sb, "|%s:%s:%v", m.Source, m.Target, m.ReadOnly)
	}
	for _, e := range env {
		fmt.Fprintf(&sb, "|env=%s", e)
	}
	for _, h := range extraHosts {
		fmt.Fprintf(&sb, "|host=%s", h)
	}
	sum := sha256.Sum256([]byte(sb.String()))
	return hex.EncodeToString(sum[:])[:12]
}

// writeAdminDynamicConfig writes (or removes) the Traefik file-provider config
// that publishes the admin UI over HTTPS. When routing is disabled it clears
// any stale file so Traefik drops the route on its next watch cycle.
func writeAdminDynamicConfig(pc ProxyConfig) error {
	if pc.DynamicDir == "" {
		return nil
	}
	path := filepath.Join(pc.DynamicDir, adminDynamicFile)
	if !pc.adminRoutingEnabled() {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	if err := os.MkdirAll(pc.DynamicDir, 0o700); err != nil {
		return err
	}
	port := pc.AdminPort
	if port == "" {
		port = "8080"
	}
	return os.WriteFile(path, []byte(adminDynamicConfig(pc.AdminHost, port, pc.TunnelMode)), 0o644)
}

// adminDynamicConfig renders the file-provider YAML that routes host to the
// admin UI on the host. In tunnel mode Cloudflare terminates TLS, so the router
// sits on the plain-HTTP web entrypoint; otherwise it uses websecure with an
// on-demand Let's Encrypt cert.
func adminDynamicConfig(host, port string, tunnelMode bool) string {
	entrypoint, tlsBlock := "websecure", "\n      tls:\n        certResolver: le"
	if tunnelMode {
		entrypoint, tlsBlock = "web", ""
	}
	return fmt.Sprintf(`# Managed by Outhaul — routes the admin UI. Do not edit by hand.
http:
  routers:
    outhaul-admin:
      rule: "Host(`+"`%s`"+`)"
      entryPoints:
        - %s
      service: outhaul-admin%s
  services:
    outhaul-admin:
      loadBalancer:
        servers:
          - url: "http://host.docker.internal:%s"
`, host, entrypoint, tlsBlock, port)
}
