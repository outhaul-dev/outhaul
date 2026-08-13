package localca

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/outhaul-dev/outhaul/internal/core"
)

// dynamicCertsFile is the Traefik file-provider config (under the dynamic
// dir, next to outhaul-admin.yml) listing every local-CA leaf.
const dynamicCertsFile = "outhaul-local-certs.yml"

// syncInterval paces the rotation ticker; leafs renew 30 days early, so a
// twice-daily check has weeks of slack.
const syncInterval = 12 * time.Hour

// DomainLister is the slice of store.Store the manager needs.
type DomainLister interface {
	ListAllDomains(ctx context.Context) ([]core.DomainListing, error)
}

// Manager keeps leaf certs and the Traefik file-provider config in step with
// the domain table: at boot, after domain changes (server calls Sync), and on
// the Run ticker for rotation.
type Manager struct {
	ca          *CA
	certsDir    string
	dynamicDir  string
	defaultHost string // admin host, or the machine hostname when PublicURL is unset
	domains     DomainLister

	mu      sync.Mutex
	hostIPs func() []net.IP  // injected for tests
	now     func() time.Time // injected for tests
}

// NewManager wires a manager; defaultHost also becomes Traefik's default
// (non-SNI-match) certificate and carries the host's LAN IP SANs.
func NewManager(ca *CA, certsDir, dynamicDir, defaultHost string, domains DomainLister) *Manager {
	return &Manager{
		ca: ca, certsDir: certsDir, dynamicDir: dynamicDir,
		defaultHost: defaultHost, domains: domains,
		hostIPs: lanIPs, now: time.Now,
	}
}

// Sync makes disk match the domain table: mint missing/expiring/drifted
// leafs, prune leafs for removed domains, and rewrite the file-provider
// config. A single failed mint logs and continues — certs must never block
// startup or deploys.
func (m *Manager) Sync(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	rows, err := m.domains.ListAllDomains(ctx)
	if err != nil {
		return fmt.Errorf("list domains: %w", err)
	}
	// Which host gets which IP SANs: only the default host carries them.
	wanted := map[string][]net.IP{m.defaultHost: m.hostIPs()}
	for _, r := range rows {
		if r.TLS && r.Host != m.defaultHost {
			wanted[r.Host] = nil
		}
	}

	if err := os.MkdirAll(m.certsDir, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(m.dynamicDir, 0o700); err != nil {
		return err
	}

	now := m.now()
	var served []string
	for _, host := range sortedKeys(wanted) {
		pemPath := filepath.Join(m.certsDir, host+".pem")
		cur, readErr := os.ReadFile(pemPath)
		if readErr == nil && !NeedsRemint(cur, host, wanted[host], now) {
			served = append(served, host)
			continue
		}
		certPEM, keyPEM, err := m.ca.MintLeaf(host, wanted[host], now)
		if err != nil {
			log.Printf("localca: mint %s: %v", host, err)
			if readErr == nil {
				served = append(served, host) // keep serving the old cert
			}
			continue
		}
		if err := os.WriteFile(filepath.Join(m.certsDir, host+".key"), keyPEM, 0o600); err != nil {
			log.Printf("localca: write key for %s: %v", host, err)
			continue
		}
		if err := os.WriteFile(pemPath, certPEM, 0o644); err != nil {
			log.Printf("localca: write cert for %s: %v", host, err)
			continue
		}
		served = append(served, host)
	}
	m.prune(wanted)
	return os.WriteFile(filepath.Join(m.dynamicDir, dynamicCertsFile), renderCertsConfig(served, m.defaultHost), 0o644)
}

// Run re-syncs on a fixed interval so leafs rotate long before expiry.
func (m *Manager) Run(ctx context.Context) {
	t := time.NewTicker(syncInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := m.Sync(ctx); err != nil {
				log.Printf("localca: sync: %v", err)
			}
		}
	}
}

// prune removes leaf files whose host no longer needs a cert.
func (m *Manager) prune(wanted map[string][]net.IP) {
	entries, err := os.ReadDir(m.certsDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		host := strings.TrimSuffix(strings.TrimSuffix(name, ".pem"), ".key")
		if host == name { // neither suffix matched
			continue
		}
		if _, ok := wanted[host]; !ok {
			_ = os.Remove(filepath.Join(m.certsDir, name))
		}
	}
}

// renderCertsConfig emits the file-provider YAML: every served leaf, plus the
// default host as the default (non-SNI-match) certificate so Traefik never
// falls back to its self-generated one. Hosts are validated hostnames; they
// contain no YAML metacharacters.
func renderCertsConfig(served []string, defaultHost string) []byte {
	sort.Strings(served)
	var b strings.Builder
	b.WriteString("# Managed by Outhaul — local CA certificates. Do not edit by hand.\n")
	b.WriteString("tls:\n")
	hasDefault := false
	for _, h := range served {
		if h == defaultHost {
			hasDefault = true
		}
	}
	if hasDefault {
		b.WriteString("  stores:\n    default:\n      defaultCertificate:\n")
		fmt.Fprintf(&b, "        certFile: %s/%s.pem\n", ContainerCertsDir, defaultHost)
		fmt.Fprintf(&b, "        keyFile: %s/%s.key\n", ContainerCertsDir, defaultHost)
	}
	if len(served) > 0 {
		b.WriteString("  certificates:\n")
		for _, h := range served {
			fmt.Fprintf(&b, "    - certFile: %s/%s.pem\n", ContainerCertsDir, h)
			fmt.Fprintf(&b, "      keyFile: %s/%s.key\n", ContainerCertsDir, h)
		}
	}
	return []byte(b.String())
}

func sortedKeys(m map[string][]net.IP) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// lanIPs enumerates the host's non-loopback private IPv4 addresses, so
// https://192.168.x.x works without a warning once the root is trusted.
func lanIPs() []net.IP {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	var out []net.IP
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip := ipn.IP.To4()
		if ip != nil && ip.IsPrivate() {
			out = append(out, ip)
		}
	}
	return out
}
