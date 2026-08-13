package localca

import (
	"context"
	"errors"
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

func TestSyncKeepsOldPairWhenCertRenameFails(t *testing.T) {
	lister := &fakeLister{rows: []core.DomainListing{domainRow("app.local", true)}}
	m, certsDir, dynDir := testManager(t, lister)
	if err := m.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	oldPem, _ := os.ReadFile(filepath.Join(certsDir, "app.local.pem"))
	oldKey, _ := os.ReadFile(filepath.Join(certsDir, "app.local.key"))
	m.now = func() time.Time { return time.Now().Add(800 * 24 * time.Hour) }
	m.rename = func(oldpath, newpath string) error {
		if strings.HasSuffix(newpath, ".pem") {
			return errors.New("boom")
		}
		return os.Rename(oldpath, newpath)
	}
	if err := m.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	pem2, _ := os.ReadFile(filepath.Join(certsDir, "app.local.pem"))
	key2, _ := os.ReadFile(filepath.Join(certsDir, "app.local.key"))
	if string(pem2) != string(oldPem) || string(key2) != string(oldKey) {
		t.Error("failed cert rename must leave the old pair untouched")
	}
	body, _ := os.ReadFile(filepath.Join(dynDir, "outhaul-local-certs.yml"))
	if !strings.Contains(string(body), "app.local.pem") {
		t.Error("host with intact old pair must stay in the YAML")
	}
}

func TestSyncKeepsOldPairWhenKeyRenameFails(t *testing.T) {
	lister := &fakeLister{rows: []core.DomainListing{domainRow("app.local", true)}}
	m, certsDir, dynDir := testManager(t, lister)
	if err := m.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	oldPem, _ := os.ReadFile(filepath.Join(certsDir, "app.local.pem"))
	oldKey, _ := os.ReadFile(filepath.Join(certsDir, "app.local.key"))
	m.now = func() time.Time { return time.Now().Add(800 * 24 * time.Hour) }
	m.rename = func(oldpath, newpath string) error {
		// Only fail the keyTmp→keyPath rename, not the backup restore
		if strings.HasSuffix(newpath, ".key") && strings.HasSuffix(oldpath, ".key.tmp") {
			return errors.New("boom")
		}
		return os.Rename(oldpath, newpath)
	}
	if err := m.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	pem2, _ := os.ReadFile(filepath.Join(certsDir, "app.local.pem"))
	key2, _ := os.ReadFile(filepath.Join(certsDir, "app.local.key"))
	if string(pem2) != string(oldPem) || string(key2) != string(oldKey) {
		t.Error("failed key rename must leave the old pair untouched")
	}
	body, _ := os.ReadFile(filepath.Join(dynDir, "outhaul-local-certs.yml"))
	if !strings.Contains(string(body), "app.local.pem") {
		t.Error("host with intact old pair must stay in the YAML")
	}
}
