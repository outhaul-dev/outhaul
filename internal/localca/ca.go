// Package localca implements Outhaul's built-in certificate authority for
// local-only installs: an mkcert-style root created once per host, plus leaf
// certificates minted per domain and served through Traefik's file provider.
// No ACME, no external dependencies, no human in the loop after the root is
// installed on each device.
package localca

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"time"
)

// ContainerCertsDir is where Traefik sees the leaf certs; the host side is
// config.CertsDir(), bind-mounted read-only.
const ContainerCertsDir = "/etc/traefik/certs"

const (
	rootCertFile = "rootCA.pem"
	rootKeyFile  = "rootCA.key"
	caValidity   = 10 * 365 * 24 * time.Hour
)

// CA is a loaded local certificate authority, able to mint leaf certs.
type CA struct {
	cert    *x509.Certificate
	key     *ecdsa.PrivateKey
	certPEM []byte
}

// RootPath is the CA certificate's path under dir, whether or not it exists.
func RootPath(dir string) string { return filepath.Join(dir, rootCertFile) }

// CertPEM returns the PEM-encoded root certificate (public material).
func (ca *CA) CertPEM() []byte { return ca.certPEM }

// LoadOrCreate loads the CA from dir, creating it on first use. Partial or
// corrupt state is a hard error: regenerating a CA that devices already trust
// would silently invalidate every leaf, so recovery is left to the operator.
func LoadOrCreate(dir string) (*CA, error) {
	certPath, keyPath := RootPath(dir), filepath.Join(dir, rootKeyFile)
	certPEM, certErr := os.ReadFile(certPath)
	keyPEM, keyErr := os.ReadFile(keyPath)
	switch {
	case certErr == nil && keyErr == nil:
		return load(certPEM, keyPEM)
	case os.IsNotExist(certErr) && os.IsNotExist(keyErr):
		return create(dir, certPath, keyPath)
	default:
		return nil, fmt.Errorf("local CA at %s is incomplete or unreadable (cert: %v; key: %v); restore both files or remove the directory to start a new CA", dir, certErr, keyErr)
	}
}

func load(certPEM, keyPEM []byte) (*CA, error) {
	cb, _ := pem.Decode(certPEM)
	kb, _ := pem.Decode(keyPEM)
	if cb == nil || kb == nil {
		return nil, fmt.Errorf("local CA files are not valid PEM")
	}
	cert, err := x509.ParseCertificate(cb.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse local CA certificate: %w", err)
	}
	key, err := x509.ParseECPrivateKey(kb.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse local CA key: %w", err)
	}
	if !key.PublicKey.Equal(cert.PublicKey) {
		return nil, fmt.Errorf("local CA key does not match its certificate")
	}
	return &CA{cert: cert, key: key, certPEM: certPEM}, nil
}

func create(dir, certPath, keyPath string) (*CA, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	hostname, _ := os.Hostname()
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: fmt.Sprintf("Outhaul Local CA (%s)", hostname), Organization: []string{"Outhaul"}},
		NotBefore:             now.Add(-time.Hour), // tolerate device clock skew
		NotAfter:              now.Add(caValidity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return nil, err
	}
	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		return nil, err
	}
	return load(certPEM, keyPEM)
}
