package localca

import (
	"crypto/x509"
	"encoding/pem"
	"net"
	"path/filepath"
	"testing"
	"time"
)

func testCA(t *testing.T) *CA {
	t.Helper()
	ca, err := LoadOrCreate(filepath.Join(t.TempDir(), "ca"))
	if err != nil {
		t.Fatal(err)
	}
	return ca
}

func TestMintLeafSANsAndValidity(t *testing.T) {
	ca := testCA(t)
	now := time.Now()
	ip := net.ParseIP("192.168.1.10")
	certPEM, keyPEM, err := ca.MintLeaf("app.local", []net.IP{ip}, now)
	if err != nil {
		t.Fatalf("MintLeaf: %v", err)
	}
	if len(keyPEM) == 0 {
		t.Fatal("no key")
	}
	block, _ := pem.Decode(certPEM)
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if len(cert.DNSNames) != 1 || cert.DNSNames[0] != "app.local" {
		t.Errorf("DNSNames = %v", cert.DNSNames)
	}
	if len(cert.IPAddresses) != 1 || !cert.IPAddresses[0].Equal(ip) {
		t.Errorf("IPAddresses = %v", cert.IPAddresses)
	}
	wantExpiry := now.Add(825 * 24 * time.Hour)
	if cert.NotAfter.Before(wantExpiry.Add(-time.Hour)) || cert.NotAfter.After(wantExpiry.Add(time.Hour)) {
		t.Errorf("NotAfter = %v; want ~%v", cert.NotAfter, wantExpiry)
	}
	// Chain must verify against the root.
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca.CertPEM()) {
		t.Fatal("bad root PEM")
	}
	if _, err := cert.Verify(x509.VerifyOptions{Roots: roots, DNSName: "app.local"}); err != nil {
		t.Errorf("leaf does not verify against CA: %v", err)
	}
}

func TestNeedsRemint(t *testing.T) {
	ca := testCA(t)
	now := time.Now()
	certPEM, _, err := ca.MintLeaf("app.local", nil, now)
	if err != nil {
		t.Fatal(err)
	}
	if NeedsRemint(certPEM, "app.local", nil, now) {
		t.Error("fresh cert should not need remint")
	}
	if !NeedsRemint(certPEM, "app.local", nil, now.Add(800*24*time.Hour)) {
		t.Error("cert inside the 30-day renewal window must remint")
	}
	if !NeedsRemint(certPEM, "other.local", nil, now) {
		t.Error("host change must remint")
	}
	if !NeedsRemint(certPEM, "app.local", []net.IP{net.ParseIP("10.0.0.1")}, now) {
		t.Error("IP SAN drift must remint")
	}
	if !NeedsRemint([]byte("garbage"), "app.local", nil, now) {
		t.Error("unparseable cert must remint")
	}
}
