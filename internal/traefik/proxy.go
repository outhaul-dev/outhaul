package traefik

import (
	"bytes"
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
	"github.com/outhaul-dev/outhaul/internal/localca"
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

// forwardedDynamicFile is the file-provider config (under DynamicDir) that
// defines the outhaul-forwarded middleware the entrypoints name while proxies
// are trusted.
const forwardedDynamicFile = "outhaul-forwarded.yml"

// proxyStopTimeout bounds how long we wait for a drifted Traefik container to
// stop gracefully before it is removed and recreated.
const proxyStopTimeout = 10 * time.Second

// ProxyConfig parameterises the managed Traefik container.
type ProxyConfig struct {
	ContainerName string // e.g. "outhaul-traefik"
	Image         string // e.g. "traefik:v3.3"
	Network       string // shared network app containers also join
	Subnet        string // OUTHAUL_NETWORK_SUBNET; "" lets Docker choose
	IPv4          string // Traefik's fixed address; "" lets Docker choose
	HTTPPort      string // host port for the web entrypoint, e.g. "80"
	DockerSocket  string // host docker socket path; defaults to DefaultDockerSocket

	ACME           bool // Let's Encrypt automation on (websecure + ACME resolver)
	ACMEEmail      string
	ACMEStaging    bool
	DNSProvider    string   // lego DNS-01 provider code; "" uses the HTTP challenge
	DNSEnv         []string // "NAME=value" variables passed to Traefik for the provider
	HTTPSPort      string   // host port for :443
	ACMEStorageDir string   // host dir bind-mounted for acme.json

	// LocalCA serves HTTPS from Outhaul-minted certs instead of ACME: the
	// websecure entrypoint and redirect are on, no ACME flags are emitted,
	// and CertsDir is mounted read-only for the file-provider tls config
	// (outhaul-local-certs.yml under DynamicDir) to reference.
	LocalCA  bool
	CertsDir string

	// DockerAPIVersion pins DOCKER_API_VERSION for Traefik's docker client.
	// Empty falls back to fallbackDockerAPIVersion.
	DockerAPIVersion string

	// Admin-UI routing: when AdminHost is set and TLS is enabled, Traefik's
	// file provider publishes an HTTPS router for the admin UI, reaching the
	// host process at host.docker.internal:AdminPort via the host gateway.
	AdminHost  string // hostname the admin UI is served under (from PublicURL)
	AdminPort  string // host port the admin UI listens on, e.g. "8080"
	DynamicDir string // host dir bind-mounted as Traefik's file provider

	// ProtectedRoutes is true while any domain row is protected, or any
	// managed container still carries a gate (HasGateLabel). The gate is
	// host.docker.internal on AdminPort, so Traefik maps that name to the host
	// gateway even when the admin route is off; otherwise the name would be
	// looked up on the Docker network.
	ProtectedRoutes bool

	// TunnelMode makes the tunnel the server-wide ingress: Traefik serves plain
	// HTTP on :80 for cloudflared to reach over the Docker network, publishes no
	// host ports, and runs no ACME/redirect (Cloudflare terminates TLS at its
	// edge). The admin route, if any, is published over plain HTTP.
	TunnelMode bool

	// Routing carries the server-wide routing settings routers are written
	// from. proxySpec reads HTTPOff here; later settings (a wildcard, a
	// required header) join it. ACME, LocalCA and TunnelMode stay ProxyConfig's
	// own fields.
	Routing Routing

	// GateHost is the address host.docker.internal maps to; "" uses Docker's
	// host-gateway. The admin port is closed to every other address with
	// OUTHAUL_ADMIN_DIRECT=off, so Traefik must reach this one.
	GateHost string

	// TrustedProxies and TrustAllProxies name the proxies in front of Traefik
	// whose forwarded headers it believes. They are not part of Routing: no
	// router is written from them, only Traefik's own flags and one file.
	TrustedProxies  []string
	TrustAllProxies bool
}

// trustsProxy reports whether Traefik believes any proxy's forwarded headers.
func (pc ProxyConfig) trustsProxy() bool {
	return pc.TrustAllProxies || len(pc.TrustedProxies) > 0
}

// adminRoutingEnabled reports whether the admin UI should be published
// through Traefik: over HTTPS (websecure + Let's Encrypt cert) when ACME is
// enabled, over HTTPS (websecure + file-provider cert) when the local CA is
// enabled, or over plain HTTP (web entrypoint) in tunnel mode where
// Cloudflare terminates TLS. Either way it needs an admin host and a dir to
// write the dynamic config into.
func (pc ProxyConfig) adminRoutingEnabled() bool {
	return (pc.ACME || pc.LocalCA || pc.TunnelMode) && pc.AdminHost != "" && pc.DynamicDir != ""
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
	// A fixed address is reserved by the network's fixed range, which only
	// exists with a fixed subnet. Docker would accept the address on a network
	// whose subnet it chose, and Traefik would come up outside the range the
	// firewall names. Validate refuses this too; a ProxyConfig built elsewhere
	// must not slip past.
	if pc.IPv4 != "" && pc.Subnet == "" {
		return fmt.Errorf("OUTHAUL_TRAEFIK_IP=%s needs OUTHAUL_NETWORK_SUBNET, which fixes the addresses Traefik may use", pc.IPv4)
	}
	if pc.DockerSocket == "" {
		pc.DockerSocket = DefaultDockerSocket
	}
	if pc.trustsProxy() && pc.DynamicDir == "" {
		return fmt.Errorf("OUTHAUL_TRUSTED_PROXIES needs a directory for Traefik's file provider, and none is configured")
	}
	if err := writeAdminDynamicConfig(pc); err != nil {
		return fmt.Errorf("write admin dynamic config: %w", err)
	}
	if err := removeStaleLocalCertsConfig(pc); err != nil {
		return fmt.Errorf("remove stale local-certs config: %w", err)
	}
	// While trust is on, the file must exist before Traefik is created, so a
	// replacement finds the middleware its flags name. When trust is off it
	// goes only once nothing running still names it.
	if err := writeForwardedDynamicConfig(pc); err != nil {
		return fmt.Errorf("write forwarded-headers config: %w", err)
	}
	networkSpec, err := docker.NetworkFor(pc.Network, pc.Subnet)
	if err != nil {
		return fmt.Errorf("network settings: %w", err)
	}
	if err := dc.EnsureNetwork(ctx, networkSpec); err != nil {
		return fmt.Errorf("ensure network %q: %w", pc.Network, err)
	}

	spec := proxySpec(pc)

	existing, err := dc.FindContainer(ctx, pc.ContainerName)
	if err != nil {
		return fmt.Errorf("find traefik container: %w", err)
	}
	finish := func() error {
		if pc.trustsProxy() {
			return nil
		}
		if err := removeForwardedDynamicConfig(pc); err != nil {
			return fmt.Errorf("remove forwarded-headers config: %w", err)
		}
		return nil
	}
	// Adopt only if its config matches; otherwise recreate so new flags
	// (e.g. newly-enabled TLS) actually take effect.
	if existing != nil && existing.Labels["outhaul.config-hash"] == spec.Labels["outhaul.config-hash"] {
		if !existing.Running() {
			if err := dc.StartContainer(ctx, existing.ID); err != nil {
				return err
			}
		}
		return finish()
	}

	// A fixed address is only usable if nothing else on the network holds it.
	// Check before the pull, so a taken address leaves the old Traefik running
	// rather than stopped or half-replaced.
	if pc.IPv4 != "" {
		info, err := dc.InspectNetwork(ctx, pc.Network)
		if err != nil {
			return fmt.Errorf("check who holds Traefik's address %s: %w", pc.IPv4, err)
		}
		for name, addr := range info.IPv4s {
			if addr != pc.IPv4 || name == pc.ContainerName {
				continue
			}
			return fmt.Errorf("Traefik's address %s on network %s is held by container %s; stop or remove it, or set OUTHAUL_TRAEFIK_IP to another address", pc.IPv4, pc.Network, name)
		}
	}

	// (Re)create needed. Pull FIRST so a pull failure never leaves us with no proxy.
	if err := dc.PullImage(ctx, pc.Image, logOut); err != nil {
		if pc.Routing.Constraint() == "" {
			return fmt.Errorf("pull traefik image: %w", err)
		}
		// A header is required: the old Traefik has no constraint, so it must
		// not keep serving. Use the local image when it is there; a change of
		// flags alone never needs a pull. Otherwise stop the old one.
		local, lerr := dc.ListImages(ctx, pc.Image)
		if lerr != nil || len(local) == 0 {
			if existing != nil && existing.Running() {
				if stopErr := dc.StopContainer(ctx, existing.ID, proxyStopTimeout); stopErr != nil {
					return fmt.Errorf("pull traefik image: %w; stopping the old Traefik, which has no constraint for the required header, failed: %v", err, stopErr)
				}
			}
			return fmt.Errorf("pull traefik image: %w", err)
		}
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
	return finish()
}

// ProxyMapsGateHost reports whether the running Traefik maps the gate's host
// (host.docker.internal) to the host gateway, and whether that is known: its
// config hash must be the one pc gives with ProtectedRoutes on or off.
// Otherwise (no Traefik, a lookup that fails, a config that differs in
// something else) it is unknown.
func ProxyMapsGateHost(ctx context.Context, dc docker.Client, pc ProxyConfig) (mapped, known bool) {
	if pc.DockerSocket == "" {
		pc.DockerSocket = DefaultDockerSocket
	}
	existing, err := dc.FindContainer(ctx, pc.ContainerName)
	if err != nil || existing == nil {
		return false, false
	}
	have := existing.Labels["outhaul.config-hash"]
	pc.ProtectedRoutes = true
	on := proxySpec(pc)
	if have == on.Labels["outhaul.config-hash"] {
		return mapsGateHost(on.ExtraHosts, pc.GateHost), true
	}
	pc.ProtectedRoutes = false
	if have == proxySpec(pc).Labels["outhaul.config-hash"] {
		return false, true
	}
	return false, false
}

// mapsGateHost reports whether extra hosts maps host.docker.internal, either
// to the host gateway or to the configured gateway address.
func mapsGateHost(extraHosts []string, gateHost string) bool {
	for _, h := range extraHosts {
		if h == "host.docker.internal:host-gateway" {
			return true
		}
		if gateHost != "" && h == "host.docker.internal:"+gateHost {
			return true
		}
	}
	return false
}

// ProxyFlags returns the flags and environment proxySpec gives the managed
// Traefik container, for the real-Traefik scripts so they run the code Outhaul
// runs rather than a copy of it.
func ProxyFlags(pc ProxyConfig) (cmd, env []string) {
	spec := proxySpec(pc)
	return spec.Cmd, spec.Env
}

// proxySpec builds the desired Traefik container spec (deterministic), stamping a
// config hash so drift can be detected on the next boot.
func proxySpec(pc ProxyConfig) docker.ContainerSpec {
	cmd := []string{
		"--providers.docker=true",
		"--providers.docker.exposedbydefault=false",
	}
	if constraint := pc.Routing.Constraint(); constraint != "" {
		cmd = append(cmd, "--providers.docker.constraints="+constraint)
	}
	httpOff := pc.Routing.HTTPOff && !pc.TunnelMode
	var entrypoints []string
	if !httpOff {
		cmd = append(cmd, "--entrypoints.web.address=:80")
		entrypoints = append(entrypoints, "web")
	}
	var ports []docker.PortMapping
	if !pc.TunnelMode && !httpOff {
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

	// The file provider carries the admin-UI router, the local-CA cert list
	// and, while a proxy is trusted, the forwarded-headers middleware;
	// host-gateway is needed for the admin route and for the gate of
	// any protected route. Without either, the spec is what it was before
	// protected routes, so upgrading doesn't recreate Traefik.
	if pc.adminRoutingEnabled() || (pc.LocalCA && !pc.TunnelMode) || pc.trustsProxy() {
		cmd = append(cmd,
			"--providers.file.directory=/etc/traefik/dynamic",
			"--providers.file.watch=true",
		)
		mounts = append(mounts, docker.Mount{Source: pc.DynamicDir, Target: "/etc/traefik/dynamic", ReadOnly: true})
	}
	if pc.adminRoutingEnabled() || pc.ProtectedRoutes {
		host := "host.docker.internal:host-gateway"
		if pc.GateHost != "" {
			host = "host.docker.internal:" + pc.GateHost
		}
		extraHosts = append(extraHosts, host)
	}

	if (pc.ACME || pc.LocalCA) && !pc.TunnelMode {
		cmd = append(cmd, "--entrypoints.websecure.address=:443")
		entrypoints = append(entrypoints, "websecure")
		if pc.ACME && pc.Routing.Wildcard != "" {
			cmd = append(cmd,
				"--entrypoints.websecure.http.tls.certresolver=le",
				"--entrypoints.websecure.http.tls.domains[0].main="+pc.Routing.Wildcard,
			)
		}
		if !httpOff {
			cmd = append(cmd,
				"--entrypoints.web.http.redirections.entrypoint.to=websecure",
				"--entrypoints.web.http.redirections.entrypoint.scheme=https",
			)
		}
		ports = append(ports, docker.PortMapping{HostPort: pc.HTTPSPort, ContainerPort: "443", Proto: "tcp"})
	}
	if pc.trustsProxy() {
		for _, ep := range entrypoints {
			if pc.TrustAllProxies {
				cmd = append(cmd, "--entrypoints."+ep+".forwardedheaders.insecure=true")
			} else {
				cmd = append(cmd, "--entrypoints."+ep+".forwardedheaders.trustedips="+strings.Join(pc.TrustedProxies, ","))
			}
			cmd = append(cmd,
				"--entrypoints."+ep+".http.middlewares=outhaul-forwarded@file",
				"--entrypoints."+ep+".http.underscoreheadersstrategy=delete",
			)
		}
	}
	if pc.ACME && !pc.TunnelMode {
		if pc.DNSProvider != "" {
			cmd = append(cmd,
				"--certificatesresolvers.le.acme.dnschallenge=true",
				"--certificatesresolvers.le.acme.dnschallenge.provider="+pc.DNSProvider,
			)
		} else {
			cmd = append(cmd,
				"--certificatesresolvers.le.acme.httpchallenge=true",
				"--certificatesresolvers.le.acme.httpchallenge.entrypoint=web",
			)
		}
		cmd = append(cmd,
			"--certificatesresolvers.le.acme.email="+pc.ACMEEmail,
			"--certificatesresolvers.le.acme.storage=/etc/traefik/acme/acme.json",
		)
		if pc.ACMEStaging {
			cmd = append(cmd, "--certificatesresolvers.le.acme.caserver=https://acme-staging-v02.api.letsencrypt.org/directory")
		}
		mounts = append(mounts, docker.Mount{Source: pc.ACMEStorageDir, Target: "/etc/traefik/acme", ReadOnly: false})
	}
	if pc.ACME && !pc.TunnelMode {
		env = append(env, pc.DNSEnv...)
	}
	if pc.LocalCA && !pc.TunnelMode {
		mounts = append(mounts, docker.Mount{Source: pc.CertsDir, Target: localca.ContainerCertsDir, ReadOnly: true})
	}

	labels := map[string]string{
		"outhaul.managed":     "true",
		"outhaul.role":        "proxy",
		"outhaul.config-hash": hashConfig(pc.Image, cmd, ports, mounts, env, extraHosts, pc.TunnelMode, pc.IPv4),
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
		IPv4:          pc.IPv4,
		RestartPolicy: "unless-stopped",
		Labels:        labels,
	}
}

// hashConfig fingerprints the fields that define the desired Traefik container.
// IMPORTANT: include every ProxyConfig-derived field that should trigger a
// recreate when it changes (image, cmd, ports, mounts, env, extra hosts, and
// the fixed address). Add new fields here. The admin router's host/port live
// in the file-provider config instead, which Traefik hot-reloads — so they
// need not be hashed.
func hashConfig(image string, cmd []string, ports []docker.PortMapping, mounts []docker.Mount, env, extraHosts []string, tunnelMode bool, ipv4 string) string {
	var sb strings.Builder
	sb.WriteString(image)
	sb.WriteByte('\n')
	fmt.Fprintf(&sb, "tunnel=%v\n", tunnelMode)
	// Only an address changes the container; without one the hash is exactly
	// what it was before fixed addresses existed, so upgrades don't recreate.
	if ipv4 != "" {
		fmt.Fprintf(&sb, "|ip4=%s", ipv4)
	}
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
	entrypoint, tlsBlock := "websecure", "\n      tls:\n        certResolver: le"
	switch {
	case pc.TunnelMode:
		entrypoint, tlsBlock = "web", ""
	case pc.LocalCA:
		// Cert comes from the file-provider tls store, not a resolver.
		entrypoint, tlsBlock = "websecure", "\n      tls: {}"
	case WildcardCovers(pc.Routing.Wildcard, pc.AdminHost):
		// The entrypoint's wildcard covers this host: it inherits the
		// resolver and certificate, so it must name no tls setting.
		tlsBlock = ""
	}
	return os.WriteFile(path, []byte(adminDynamicConfig(pc, entrypoint, tlsBlock)), 0o644)
}

// ForwardedDynamicConfig is the file-provider YAML EnsureProxy writes while
// proxies are trusted: a middleware that blanks the headers a proxy has no
// reason to send, so a visitor can't forge them.
func ForwardedDynamicConfig() string {
	return `# Managed by Outhaul — removes the headers a proxy has no reason to send.
# Do not edit by hand.
http:
  middlewares:
    outhaul-forwarded:
      headers:
        customRequestHeaders:
          X-Real-Ip: ""
          X-Forwarded-Uri: ""
          X-Forwarded-Method: ""
          X-Forwarded-Prefix: ""
          X-Forwarded-Tls-Client-Cert: ""
          X-Forwarded-Tls-Client-Cert-Info: ""
`
}

// writeForwardedDynamicConfig writes the forwarded-headers middleware file
// when proxies are trusted, and does nothing otherwise.
func writeForwardedDynamicConfig(pc ProxyConfig) error {
	if pc.DynamicDir == "" || !pc.trustsProxy() {
		return nil
	}
	if err := os.MkdirAll(pc.DynamicDir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(pc.DynamicDir, forwardedDynamicFile)
	want := []byte(ForwardedDynamicConfig())
	// A file that is missing, empty or half written closes every route on
	// the entrypoint, so leave a file that is already right alone, and
	// replace any other by rename, never by truncating it.
	if have, err := os.ReadFile(path); err == nil && bytes.Equal(have, want) {
		return nil
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, want, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// removeForwardedDynamicConfig removes the middleware file, ignoring its
// absence. Call it only once no running Traefik names the middleware.
func removeForwardedDynamicConfig(pc ProxyConfig) error {
	if pc.DynamicDir == "" {
		return nil
	}
	if err := os.Remove(filepath.Join(pc.DynamicDir, forwardedDynamicFile)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// removeStaleLocalCertsConfig clears localca's file-provider cert list when
// LocalCA is off. localca.Manager only writes/updates this file while the
// local CA is running; without this, switching LocalCA -> ACME (or off)
// leaves Traefik's file provider referencing cert paths under a dir that is
// no longer mounted.
func removeStaleLocalCertsConfig(pc ProxyConfig) error {
	if pc.DynamicDir == "" || pc.LocalCA {
		return nil
	}
	path := filepath.Join(pc.DynamicDir, localca.DynamicCertsFile)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// adminDynamicConfig renders the file-provider YAML that routes host to the
// admin UI on the host. In tunnel mode Cloudflare terminates TLS, so the router
// sits on the plain-HTTP web entrypoint; under LocalCA it uses the
// file-provider tls store (tls: {}); otherwise it uses websecure with an
// on-demand Let's Encrypt cert.
func adminDynamicConfig(pc ProxyConfig, entrypoint, tlsBlock string) string {
	port := pc.AdminPort
	if port == "" {
		port = "8080"
	}
	rule := "Host(`" + pc.AdminHost + "`)" + pc.Routing.headerTerm()
	return fmt.Sprintf(`# Managed by Outhaul — routes the admin UI. Do not edit by hand.
http:
  routers:
    outhaul-admin:
      rule: "%s"
      entryPoints:
        - %s
      service: outhaul-admin%s
  services:
    outhaul-admin:
      loadBalancer:
        servers:
          - url: "http://host.docker.internal:%s"
`, rule, entrypoint, tlsBlock, port)
}
