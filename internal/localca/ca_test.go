package localca

import (
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadOrCreateCreatesThenReloads(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ca")
	ca1, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	block, _ := pem.Decode(ca1.CertPEM())
	if block == nil {
		t.Fatal("CertPEM did not decode")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !cert.IsCA || cert.KeyUsage&x509.KeyUsageCertSign == 0 {
		t.Errorf("not a signing CA: IsCA=%v usage=%v", cert.IsCA, cert.KeyUsage)
	}
	if fi, _ := os.Stat(filepath.Join(dir, "rootCA.key")); fi.Mode().Perm() != 0o600 {
		t.Errorf("key mode = %v; want 0600", fi.Mode().Perm())
	}
	ca2, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if string(ca1.CertPEM()) != string(ca2.CertPEM()) {
		t.Error("reload regenerated the CA; must reuse the existing one")
	}
}

func TestLoadOrCreateRefusesCorruptCA(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ca")
	if _, err := LoadOrCreate(dir); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "rootCA.key"), []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreate(dir); err == nil {
		t.Error("corrupt key must be a hard error, not a regenerate")
	}
}

func TestLoadOrCreateRefusesPartialCA(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ca")
	if _, err := LoadOrCreate(dir); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := os.Remove(filepath.Join(dir, "rootCA.key")); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreate(dir); err == nil {
		t.Error("cert-without-key must be a hard error")
	}
}
