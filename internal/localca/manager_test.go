package localca

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/outhaul-dev/outhaul/internal/core"
)

type fakeLister struct{ rows []core.DomainListing }

func (f *fakeLister) ListAllDomains(context.Context) ([]core.DomainListing, error) {
	return f.rows, nil
}

func domainRow(host string, tls bool) core.DomainListing {
	return core.DomainListing{Domain: core.Domain{Host: host, TLS: tls}}
}

func testManager(t *testing.T, lister DomainLister) (*Manager, string, string) {
	t.Helper()
	base := t.TempDir()
	certsDir := filepath.Join(base, "certs")
	dynDir := filepath.Join(base, "dynamic")
	m := NewManager(testCA(t), certsDir, dynDir, "gateway.local", lister)
	m.hostIPs = func() []net.IP { return []net.IP{net.ParseIP("192.168.1.10")} }
	return m, certsDir, dynDir
}

func TestSyncMintsLeafsAndWritesDynamicConfig(t *testing.T) {
	lister := &fakeLister{rows: []core.DomainListing{domainRow("app.local", true), domainRow("plain.local", false)}}
	m, certsDir, dynDir := testManager(t, lister)
	if err := m.Sync(context.Background()); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	for _, f := range []string{"gateway.local.pem", "gateway.local.key", "app.local.pem", "app.local.key"} {
		if _, err := os.Stat(filepath.Join(certsDir, f)); err != nil {
			t.Errorf("missing %s: %v", f, err)
		}
	}
	if _, err := os.Stat(filepath.Join(certsDir, "plain.local.pem")); err == nil {
		t.Error("TLS-off domain must not get a cert")
	}
	body, err := os.ReadFile(filepath.Join(dynDir, "outhaul-local-certs.yml"))
	if err != nil {
		t.Fatalf("dynamic config: %v", err)
	}
	for _, want := range []string{
		"/etc/traefik/certs/app.local.pem",
		"/etc/traefik/certs/gateway.local.key",
		"defaultCertificate:",
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("dynamic config missing %q:\n%s", want, body)
		}
	}
}

func TestSyncPrunesRemovedDomains(t *testing.T) {
	lister := &fakeLister{rows: []core.DomainListing{domainRow("app.local", true)}}
	m, certsDir, _ := testManager(t, lister)
	if err := m.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	lister.rows = nil
	if err := m.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(certsDir, "app.local.pem")); err == nil {
		t.Error("removed domain's cert should be pruned")
	}
	if _, err := os.Stat(filepath.Join(certsDir, "gateway.local.pem")); err != nil {
		t.Error("default-host cert must survive pruning")
	}
}

func TestSyncRotatesExpiringLeaf(t *testing.T) {
	lister := &fakeLister{rows: []core.DomainListing{domainRow("app.local", true)}}
	m, certsDir, _ := testManager(t, lister)
	if err := m.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(certsDir, "app.local.pem"))
	m.now = func() time.Time { return time.Now().Add(800 * 24 * time.Hour) }
	if err := m.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(filepath.Join(certsDir, "app.local.pem"))
	if string(before) == string(after) {
		t.Error("leaf inside renewal window was not rotated")
	}
}

func TestSyncAtomicWritePreservesOldPairOnFailure(t *testing.T) {
	lister := &fakeLister{rows: []core.DomainListing{domainRow("app.local", true)}}
	m, certsDir, dynDir := testManager(t, lister)
	// First sync: create initial cert/key pair
	if err := m.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	oldPem, _ := os.ReadFile(filepath.Join(certsDir, "app.local.pem"))
	oldKey, _ := os.ReadFile(filepath.Join(certsDir, "app.local.key"))

	// Make certsDir read-only to force the cert write to fail during remint
	if err := os.Chmod(certsDir, 0o555); err != nil {
		t.Fatalf("chmod certsDir: %v", err)
	}
	defer os.Chmod(certsDir, 0o755) // restore for cleanup

	// Second sync: try to remint (we're in renewal window)
	m.now = func() time.Time { return time.Now().Add(800 * 24 * time.Hour) }
	if err := m.Sync(context.Background()); err != nil {
		// Sync may fail because certsDir is read-only; that's expected
	}

	// Restore permissions to verify the old pair is intact
	if err := os.Chmod(certsDir, 0o755); err != nil {
		t.Fatalf("chmod certsDir: %v", err)
	}

	// Verify old cert/key pair is unchanged
	newPem, err := os.ReadFile(filepath.Join(certsDir, "app.local.pem"))
	if err != nil {
		t.Fatalf("read cert after failed remint: %v", err)
	}
	newKey, err := os.ReadFile(filepath.Join(certsDir, "app.local.key"))
	if err != nil {
		t.Fatalf("read key after failed remint: %v", err)
	}
	if string(oldPem) != string(newPem) {
		t.Error("cert was modified despite remint failure")
	}
	if string(oldKey) != string(newKey) {
		t.Error("key was modified despite remint failure")
	}
	// Verify app.local is still listed in the YAML (serving old cert)
	body, err := os.ReadFile(filepath.Join(dynDir, "outhaul-local-certs.yml"))
	if err != nil {
		t.Fatalf("dynamic config: %v", err)
	}
	if !strings.Contains(string(body), "/etc/traefik/certs/app.local.pem") {
		t.Error("app.local should still be listed in config despite remint failure")
	}
}
