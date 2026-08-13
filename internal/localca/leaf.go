package localca

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"sort"
	"time"
)

const (
	// leafValidity is Apple's ceiling for trusted server certs (mkcert's
	// choice too); longer and iOS/macOS reject the cert outright.
	leafValidity = 825 * 24 * time.Hour
	// renewBefore is how close to expiry a leaf gets before it is re-minted.
	renewBefore = 30 * 24 * time.Hour
)

// MintLeaf issues a server certificate for host (plus optional IP SANs),
// returning the PEM cert and key. now is injected so rotation is testable.
func (ca *CA) MintLeaf(host string, ips []net.IP, now time.Time) (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: host, Organization: []string{"Outhaul"}},
		NotBefore:    now.Add(-time.Hour), // tolerate device clock skew
		NotAfter:     now.Add(leafValidity),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{host},
		IPAddresses:  ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		return nil, nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM, nil
}

// NeedsRemint reports whether certPEM should be replaced for host+ips:
// unparseable, expiring within renewBefore, or SANs no longer matching.
func NeedsRemint(certPEM []byte, host string, ips []net.IP, now time.Time) bool {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return true
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return true
	}
	if now.Add(renewBefore).After(cert.NotAfter) {
		return true
	}
	if len(cert.DNSNames) != 1 || cert.DNSNames[0] != host {
		return true
	}
	return !sameIPs(cert.IPAddresses, ips)
}

func sameIPs(a, b []net.IP) bool {
	if len(a) != len(b) {
		return false
	}
	as, bs := ipStrings(a), ipStrings(b)
	for i := range as {
		if as[i] != bs[i] {
			return false
		}
	}
	return true
}

func ipStrings(ips []net.IP) []string {
	out := make([]string, len(ips))
	for i, ip := range ips {
		out[i] = ip.String()
	}
	sort.Strings(out)
	return out
}
