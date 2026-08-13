# Local CA TLS Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Local-only Outhaul installs get valid LAN HTTPS from a built-in mkcert-style CA: Outhaul mints per-domain leaf certs automatically and serves them through Traefik's file provider; the user installs the CA root on their devices once.

**Architecture:** A new `internal/localca` package owns CA creation, leaf minting, and a `Manager` that syncs certs + a Traefik file-provider YAML on boot, after domain changes, and on a 12-hour rotation ticker. Traefik's `ProxyConfig` grows a `LocalCA` posture (websecure + redirect, no ACME flags, read-only certs mount). Route labels take a `certResolver` parameter so the same TLS routers work with ACME (`"le"`) or file-provider certs (`""`). Spec: `docs/superpowers/specs/2026-08-13-local-ca-tls-design.md`.

**Tech Stack:** Go stdlib only (`crypto/x509`, `crypto/ecdsa`, `crypto/rand`, `encoding/pem`) — no new dependencies. POSIX sh for the installer.

## Global Constraints

- No new Go module dependencies.
- CA: ECDSA P-256, 10-year validity, CN `Outhaul Local CA (<hostname>)`, files `$DataDir/ca/rootCA.pem` + `rootCA.key` (dir 0700, key 0600). Never silently regenerate an existing CA — corrupt/partial CA state is a startup error.
- Leafs: ECDSA P-256, **825-day** validity, re-minted when **< 30 days** remain or SANs drift. Files `$DataDir/traefik/certs/<host>.pem`/`.key` (key 0600). A failed mint logs and continues; it never blocks startup or deploys.
- `OUTHAUL_LOCAL_CA` is mutually exclusive with `OUTHAUL_ACME_EMAIL` (startup error) and with an enabled Cloudflare Tunnel (startup error + refusal in the Enable handler).
- Container-side certs path: `/etc/traefik/certs` (constant `localca.ContainerCertsDir`).
- Each commit must build (`go build ./...`) and pass `go test ./...`. Match surrounding comment style: comments explain contracts/why, never narrate the diff.
- `README.md` and `docs/MANUAL-DEPLOY.md` carry **pre-existing uncommitted user edits**. Task 10 edits them but must NOT commit them; commit only files this plan created/changed elsewhere.

---

### Task 1: Config — local-CA knobs and the ACME/TLS split

**Files:**
- Modify: `internal/config/config.go`
- Test: `internal/config/config_test.go` (exists — append)

**Interfaces:**
- Produces: `Config.LocalCA bool` field; methods `LocalCAEnabled() bool`, `ACMEEnabled() bool`, `TLSEnabled() bool` (now `ACMEEnabled || LocalCAEnabled`), `CertResolver() string` (`"le"` when ACME, else `""`), `CADir() string` (`$DataDir/ca`), `CertsDir() string` (`$DataDir/traefik/certs`), `Validate() error`.

- [ ] **Step 1: Write the failing tests** — append to `internal/config/config_test.go` (match the file's existing style for constructing `Config`/`Load` with a fake getenv; adapt the helper name if one exists):

```go
func TestLocalCAConfig(t *testing.T) {
	env := map[string]string{"OUTHAUL_LOCAL_CA": "true"}
	cfg := Load(func(k string) string { return env[k] })
	if !cfg.LocalCAEnabled() || cfg.ACMEEnabled() {
		t.Errorf("LocalCAEnabled=%v ACMEEnabled=%v; want true,false", cfg.LocalCAEnabled(), cfg.ACMEEnabled())
	}
	if !cfg.TLSEnabled() {
		t.Error("TLSEnabled should be true with local CA on")
	}
	if got := cfg.CertResolver(); got != "" {
		t.Errorf("CertResolver = %q; want empty for local CA", got)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

func TestACMECertResolver(t *testing.T) {
	env := map[string]string{"OUTHAUL_ACME_EMAIL": "ops@example.com"}
	cfg := Load(func(k string) string { return env[k] })
	if got := cfg.CertResolver(); got != "le" {
		t.Errorf("CertResolver = %q; want le", got)
	}
}

func TestValidateRejectsLocalCAPlusACME(t *testing.T) {
	env := map[string]string{"OUTHAUL_LOCAL_CA": "1", "OUTHAUL_ACME_EMAIL": "ops@example.com"}
	cfg := Load(func(k string) string { return env[k] })
	if err := cfg.Validate(); err == nil {
		t.Error("Validate should reject LOCAL_CA together with ACME_EMAIL")
	}
}

func TestCADirs(t *testing.T) {
	cfg := Load(func(string) string { return "" })
	if got, want := cfg.CADir(), filepath.Join(cfg.DataDir, "ca"); got != want {
		t.Errorf("CADir = %q; want %q", got, want)
	}
	if got, want := cfg.CertsDir(), filepath.Join(cfg.DataDir, "traefik", "certs"); got != want {
		t.Errorf("CertsDir = %q; want %q", got, want)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/config/`
Expected: FAIL — `cfg.LocalCAEnabled undefined` etc.

- [ ] **Step 3: Implement.** In `internal/config/config.go`:

Add to the `Config` struct after `ACMEStaging`:

```go
	LocalCA bool // serve HTTPS from the built-in local CA (LAN installs; excludes ACME)
```

Add to `Load` after the `ACMEStaging:` line:

```go
		LocalCA:     truthy(getenv("OUTHAUL_LOCAL_CA")),
```

Replace the existing `TLSEnabled` method (line ~107) with:

```go
// ACMEEnabled reports whether Let's Encrypt automation is configured.
func (c Config) ACMEEnabled() bool { return c.ACMEEmail != "" }

// LocalCAEnabled reports whether the built-in local CA serves HTTPS instead.
func (c Config) LocalCAEnabled() bool { return c.LocalCA }

// TLSEnabled reports whether HTTPS is available by either mechanism; it gates
// the websecure entrypoint, redirects, and the per-domain TLS toggle.
func (c Config) TLSEnabled() bool { return c.ACMEEnabled() || c.LocalCAEnabled() }

// CertResolver is the Traefik certresolver TLS routers should reference;
// empty when certs come from the file provider (local CA) instead of ACME.
func (c Config) CertResolver() string {
	if c.ACMEEnabled() {
		return "le"
	}
	return ""
}

// Validate rejects impossible combinations before any infrastructure moves.
func (c Config) Validate() error {
	if c.LocalCAEnabled() && c.ACMEEnabled() {
		return errors.New("OUTHAUL_LOCAL_CA and OUTHAUL_ACME_EMAIL are mutually exclusive: choose local-CA or Let's Encrypt HTTPS")
	}
	return nil
}
```

Add next to `AcmeDir`/`DynamicDir`:

```go
// CADir holds the local CA's root certificate and key.
func (c Config) CADir() string { return filepath.Join(c.DataDir, "ca") }

// CertsDir is the host directory of local-CA leaf certs, bind-mounted
// read-only into Traefik at localca.ContainerCertsDir.
func (c Config) CertsDir() string { return filepath.Join(c.DataDir, "traefik", "certs") }
```

Add `"errors"` to the imports.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/config/ && go build ./...`
Expected: PASS (nothing else calls the removed method signature — `TLSEnabled()` keeps its name).

- [ ] **Step 5: Commit**

```bash
git add internal/config/config.go internal/config/config_test.go
git commit -m "feat(config): OUTHAUL_LOCAL_CA knob and ACME/local-CA TLS split"
```

---

### Task 2: localca — CA creation and loading

**Files:**
- Create: `internal/localca/ca.go`
- Test: `internal/localca/ca_test.go`

**Interfaces:**
- Produces: `localca.LoadOrCreate(dir string) (*CA, error)`; `localca.RootPath(dir string) string`; `(*CA).CertPEM() []byte`. `CA` has unexported `cert *x509.Certificate`, `key *ecdsa.PrivateKey`. Constant `ContainerCertsDir = "/etc/traefik/certs"` also lives in this file (Task 5 imports it).

- [ ] **Step 1: Write the failing tests** — `internal/localca/ca_test.go`:

```go
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
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/localca/`
Expected: FAIL — package does not exist yet.

- [ ] **Step 3: Implement** — `internal/localca/ca.go`:

```go
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
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/localca/`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/localca/ca.go internal/localca/ca_test.go
git commit -m "feat(localca): mkcert-style root CA, created once and never regenerated"
```

---

### Task 3: localca — leaf minting and renewal checks

**Files:**
- Create: `internal/localca/leaf.go`
- Test: `internal/localca/leaf_test.go`

**Interfaces:**
- Consumes: `*CA` from Task 2.
- Produces: `(*CA).MintLeaf(host string, ips []net.IP, now time.Time) (certPEM, keyPEM []byte, err error)`; `NeedsRemint(certPEM []byte, host string, ips []net.IP, now time.Time) bool`.

- [ ] **Step 1: Write the failing tests** — `internal/localca/leaf_test.go`:

```go
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
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/localca/`
Expected: FAIL — `ca.MintLeaf undefined`.

- [ ] **Step 3: Implement** — `internal/localca/leaf.go`:

```go
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
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/localca/`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/localca/leaf.go internal/localca/leaf_test.go
git commit -m "feat(localca): leaf minting with SAN/expiry-driven remint checks"
```

---

### Task 4: localca — Manager: sync certs + Traefik file-provider YAML

**Files:**
- Create: `internal/localca/manager.go`
- Test: `internal/localca/manager_test.go`

**Interfaces:**
- Consumes: `*CA`, `MintLeaf`, `NeedsRemint` (Tasks 2–3); `core.DomainListing` (`.Host`, `.TLS` fields); `(*store.Store).ListAllDomains` satisfies the interface below.
- Produces:
  - `type DomainLister interface { ListAllDomains(ctx context.Context) ([]core.DomainListing, error) }`
  - `NewManager(ca *CA, certsDir, dynamicDir, defaultHost string, domains DomainLister) *Manager`
  - `(*Manager).Sync(ctx context.Context) error` — Task 7 (boot) and Task 8 (`server.CertSyncer`) call this.
  - `(*Manager).Run(ctx context.Context)` — 12h rotation ticker, Task 7 starts it.
  - Manager test seams: unexported fields `hostIPs func() []net.IP`, `now func() time.Time`.

- [ ] **Step 1: Write the failing tests** — `internal/localca/manager_test.go`:

```go
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
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/localca/`
Expected: FAIL — `NewManager undefined`.

- [ ] **Step 3: Implement** — `internal/localca/manager.go`:

```go
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
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/localca/`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/localca/manager.go internal/localca/manager_test.go
git commit -m "feat(localca): manager syncs leafs and Traefik file-provider config"
```

---

### Task 5: Traefik proxy — LocalCA posture

**Files:**
- Modify: `internal/traefik/proxy.go`
- Modify: `main.go` (only `proxyConfig`, to keep the build green after the field rename)
- Test: `internal/traefik/proxy_test.go` (exists — rename field uses + add tests)

**Interfaces:**
- Consumes: `localca.ContainerCertsDir` (Task 2).
- Produces: `ProxyConfig` field `TLSEnabled` **renamed to `ACME`**; new fields `LocalCA bool`, `CertsDir string`. `adminRoutingEnabled` and `proxySpec`/`writeAdminDynamicConfig` honor the LocalCA posture. `adminDynamicConfig(host, port, entrypoint, tlsBlock string) string` (signature change).

- [ ] **Step 1: Write the failing tests** — append to `internal/traefik/proxy_test.go` (reuse `recordingFake` and the helper style already in the file; add a helper):

```go
func localCAProxyConfig(t *testing.T) ProxyConfig {
	pc := testProxyConfig()
	pc.LocalCA = true
	pc.HTTPSPort = "443"
	pc.CertsDir = t.TempDir()
	pc.DynamicDir = t.TempDir()
	return pc
}

func TestEnsureProxyLocalCA(t *testing.T) {
	ctx := context.Background()
	rec := &recordingFake{Fake: docker.NewFake()}
	if err := EnsureProxy(ctx, rec, localCAProxyConfig(t), nil); err != nil {
		t.Fatalf("EnsureProxy: %v", err)
	}
	joined := strings.Join(rec.created.Cmd, " ")
	for _, want := range []string{
		"--entrypoints.websecure.address=:443",
		"--entrypoints.web.http.redirections.entrypoint.to=websecure",
		"--providers.file.directory=/etc/traefik/dynamic",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("cmd missing %q; got %v", want, rec.created.Cmd)
		}
	}
	if strings.Contains(joined, "acme") {
		t.Errorf("local CA posture must carry no ACME flags: %v", rec.created.Cmd)
	}
	foundCerts := false
	for _, m := range rec.created.Mounts {
		if m.Target == "/etc/traefik/certs" && m.ReadOnly {
			foundCerts = true
		}
	}
	if !foundCerts {
		t.Errorf("certs dir not mounted read-only: %v", rec.created.Mounts)
	}
	found443 := false
	for _, p := range rec.created.Ports {
		if p.HostPort == "443" {
			found443 = true
		}
	}
	if !found443 {
		t.Errorf("should publish :443, ports=%v", rec.created.Ports)
	}
}

func TestAdminDynamicConfigLocalCA(t *testing.T) {
	pc := localCAProxyConfig(t)
	pc.AdminHost = "gateway.local"
	pc.AdminPort = "8080"
	if err := writeAdminDynamicConfig(pc); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(pc.DynamicDir, adminDynamicFile))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "tls: {}") {
		t.Errorf("admin router should use tls: {} (file-provider cert), got:\n%s", body)
	}
	if strings.Contains(string(body), "certResolver") {
		t.Errorf("local CA admin router must not reference a certresolver:\n%s", body)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/traefik/`
Expected: FAIL — `pc.LocalCA undefined`.

- [ ] **Step 3: Implement.** In `internal/traefik/proxy.go`:

Rename the `TLSEnabled` field to `ACME` and add the LocalCA fields (the TLS block of `ProxyConfig`, currently lines 43–47):

```go
	ACME           bool // Let's Encrypt automation on (websecure + ACME resolver)
	ACMEEmail      string
	ACMEStaging    bool
	HTTPSPort      string // host port for :443
	ACMEStorageDir string // host dir bind-mounted for acme.json

	// LocalCA serves HTTPS from Outhaul-minted certs instead of ACME: the
	// websecure entrypoint and redirect are on, no ACME flags are emitted,
	// and CertsDir is mounted read-only for the file-provider tls config
	// (outhaul-local-certs.yml under DynamicDir) to reference.
	LocalCA  bool
	CertsDir string
```

Update `adminRoutingEnabled`:

```go
func (pc ProxyConfig) adminRoutingEnabled() bool {
	return (pc.ACME || pc.LocalCA || pc.TunnelMode) && pc.AdminHost != "" && pc.DynamicDir != ""
}
```

Replace the file-provider and TLS sections of `proxySpec` (currently lines 154–180) with:

```go
	// The file provider carries the admin-UI router and/or the local-CA cert
	// list; host-gateway is only needed for the admin route.
	if pc.adminRoutingEnabled() || (pc.LocalCA && !pc.TunnelMode) {
		cmd = append(cmd,
			"--providers.file.directory=/etc/traefik/dynamic",
			"--providers.file.watch=true",
		)
		mounts = append(mounts, docker.Mount{Source: pc.DynamicDir, Target: "/etc/traefik/dynamic", ReadOnly: true})
	}
	if pc.adminRoutingEnabled() {
		extraHosts = append(extraHosts, "host.docker.internal:host-gateway")
	}

	if (pc.ACME || pc.LocalCA) && !pc.TunnelMode {
		cmd = append(cmd,
			"--entrypoints.websecure.address=:443",
			"--entrypoints.web.http.redirections.entrypoint.to=websecure",
			"--entrypoints.web.http.redirections.entrypoint.scheme=https",
		)
		ports = append(ports, docker.PortMapping{HostPort: pc.HTTPSPort, ContainerPort: "443", Proto: "tcp"})
	}
	if pc.ACME && !pc.TunnelMode {
		cmd = append(cmd,
			"--certificatesresolvers.le.acme.httpchallenge=true",
			"--certificatesresolvers.le.acme.httpchallenge.entrypoint=web",
			"--certificatesresolvers.le.acme.email="+pc.ACMEEmail,
			"--certificatesresolvers.le.acme.storage=/etc/traefik/acme/acme.json",
		)
		if pc.ACMEStaging {
			cmd = append(cmd, "--certificatesresolvers.le.acme.caserver=https://acme-staging-v02.api.letsencrypt.org/directory")
		}
		mounts = append(mounts, docker.Mount{Source: pc.ACMEStorageDir, Target: "/etc/traefik/acme", ReadOnly: false})
	}
	if pc.LocalCA && !pc.TunnelMode {
		mounts = append(mounts, docker.Mount{Source: pc.CertsDir, Target: localca.ContainerCertsDir, ReadOnly: true})
	}
```

Add the import `"github.com/outhaul-dev/outhaul/internal/localca"`. No `hashConfig` change is needed: the new cmd flags and mounts already flow into the fingerprint, so toggling LocalCA recreates the container.

In `writeAdminDynamicConfig`, compute the router posture (replacing the two-branch logic inside `adminDynamicConfig`):

```go
	entrypoint, tlsBlock := "websecure", "\n      tls:\n        certResolver: le"
	switch {
	case pc.TunnelMode:
		entrypoint, tlsBlock = "web", ""
	case pc.LocalCA:
		// Cert comes from the file-provider tls store, not a resolver.
		entrypoint, tlsBlock = "websecure", "\n      tls: {}"
	}
	return os.WriteFile(path, []byte(adminDynamicConfig(pc.AdminHost, port, entrypoint, tlsBlock)), 0o644)
```

And change `adminDynamicConfig` to accept them (drop its `tunnelMode` parameter and internal branch):

```go
func adminDynamicConfig(host, port, entrypoint, tlsBlock string) string {
```

Fix the rename fallout: `grep -rn "TLSEnabled" internal/traefik/ main.go` — update `tlsProxyConfig()` in `proxy_test.go`, any other test uses, and in `main.go`'s `proxyConfig` (line ~324) replace the TLS block with:

```go
	pc.ACME = ic.cfg.ACMEEnabled()
	pc.ACMEEmail = ic.cfg.ACMEEmail
	pc.ACMEStaging = ic.cfg.ACMEStaging
	pc.HTTPSPort = ic.cfg.HTTPSPort
	pc.ACMEStorageDir = ic.cfg.AcmeDir()
	pc.LocalCA = ic.cfg.LocalCAEnabled()
	pc.CertsDir = ic.cfg.CertsDir()
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/traefik/ && go build ./...`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/traefik/proxy.go internal/traefik/proxy_test.go main.go
git commit -m "feat(traefik): LocalCA proxy posture — websecure + file-provider certs, no ACME"
```

---

### Task 6: Route labels — certresolver becomes a parameter

**Files:**
- Modify: `internal/traefik/labels.go`, `internal/compose/override.go`, `internal/deploy/pipeline.go:354`, `internal/deploy/pipeline_compose.go:66`
- Test: `internal/traefik/labels_test.go`, `internal/compose/compose_test.go` (update call sites), plus any deploy tests the compiler flags

**Interfaces:**
- Consumes: `Config.CertResolver()` (Task 1); worker field `w.cfg` (exists).
- Produces: `AppLabels(app core.App, domains []core.Domain, port int, globalTLS bool, certResolver string) map[string]string`; `RouteLabels(router, host string, port int, urlPath, internalPath string, tlsEnabled bool, certResolver string) map[string]string`; `compose.Override(app core.App, domains []core.Domain, network string, tlsEnabled bool, certResolver string) []byte`. Empty resolver ⇒ `tls=true` router with **no** `certresolver` label (file-provider cert via SNI).

- [ ] **Step 1: Write the failing test** — append to `internal/traefik/labels_test.go`:

```go
func TestRouteLabelsLocalCANoResolver(t *testing.T) {
	got := RouteLabels("r", "web.local", 8080, "", "", true, "")
	if got["traefik.http.routers.r-tls.tls"] != "true" {
		t.Errorf("tls router missing: %v", got)
	}
	if _, ok := got["traefik.http.routers.r-tls.tls.certresolver"]; ok {
		t.Error("empty resolver must omit the certresolver label")
	}
}

func TestRouteLabelsACMEResolver(t *testing.T) {
	got := RouteLabels("r", "web.example.com", 8080, "", "", true, "le")
	if got["traefik.http.routers.r-tls.tls.certresolver"] != "le" {
		t.Errorf("certresolver = %q; want le", got["traefik.http.routers.r-tls.tls.certresolver"])
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/traefik/`
Expected: FAIL — too many arguments to `RouteLabels`.

- [ ] **Step 3: Implement.** In `internal/traefik/labels.go`:

```go
func AppLabels(app core.App, domains []core.Domain, port int, globalTLS bool, certResolver string) map[string]string {
```
(and pass `certResolver` through in its `RouteLabels` call)

```go
func RouteLabels(router, host string, port int, urlPath, internalPath string, tlsEnabled bool, certResolver string) map[string]string {
```

In the `if tlsEnabled` block, replace the hardcoded resolver line with:

```go
		labels["traefik.http.routers."+tls+".tls"] = "true"
		if certResolver != "" {
			labels["traefik.http.routers."+tls+".tls.certresolver"] = certResolver
		}
```

Update the doc comments: the resolver is `"le"` under ACME and empty under the local CA, where the file provider supplies certs by SNI.

In `internal/compose/override.go`, extend `Override`:

```go
func Override(app core.App, domains []core.Domain, network string, tlsEnabled bool, certResolver string) []byte {
```
and pass `certResolver` in its `traefik.RouteLabels(...)` call.

Update the callers:
- `internal/deploy/pipeline.go:354`: `labels = traefik.AppLabels(app, domains, AppPort, w.effectiveTLS(ctx), w.cfg.CertResolver())`
- `internal/deploy/pipeline_compose.go:66`: `ov := compose.Override(app, domains, w.cfg.Network, w.effectiveTLS(ctx), w.cfg.CertResolver())`

Then `go build ./... 2>&1` and fix every remaining caller the compiler lists (tests in `internal/traefik`, `internal/compose`, `internal/deploy`): existing TLS-on call sites gain `"le"` as the resolver argument, TLS-off sites gain `""`.

- [ ] **Step 4: Run tests**

Run: `go test ./...`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add -u
git commit -m "feat(traefik): route labels take a certresolver, empty for file-provider certs"
```

---

### Task 7: main.go wiring + `outhaul ca root` CLI

**Files:**
- Modify: `main.go`

**Interfaces:**
- Consumes: `config.Validate/LocalCAEnabled/CADir/CertsDir/DynamicDir/AdminHost/ACMEEnabled` (Task 1); `localca.LoadOrCreate/NewManager/RootPath`, `(*Manager).Sync/Run` (Tasks 2, 4).
- Produces: `outhaul ca root [--path]` subcommand; startup validation; local-CA boot sequence (`certMgr` variable in `serve()` — Task 8 wires it into the server); tunnel guards.

- [ ] **Step 1: Add the CLI branch and validation.** In `main()` (line ~44), before the `serve` check:

```go
	if len(os.Args) >= 2 && os.Args[1] == "ca" {
		os.Exit(runCA(os.Args[2:]))
	}
```

Add alongside `runGitHook`:

```go
// runCA implements `outhaul ca root [--path]`: prints the local CA root
// certificate (or its path) so it can be installed on LAN devices.
func runCA(args []string) int {
	if len(args) < 1 || args[0] != "root" {
		fmt.Fprintln(os.Stderr, "usage: outhaul ca root [--path]")
		return 2
	}
	cfg := config.Load(os.Getenv)
	path := localca.RootPath(cfg.CADir())
	if len(args) >= 2 && args[1] == "--path" {
		fmt.Println(path)
		return 0
	}
	pemBytes, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "outhaul: local CA not initialised (%v)\n", err)
		fmt.Fprintln(os.Stderr, "Set OUTHAUL_LOCAL_CA=true and start `outhaul serve` once to create it.")
		return 1
	}
	os.Stdout.Write(pemBytes)
	return 0
}
```

Extend `usage()`:

```go
	fmt.Fprintln(os.Stderr, "Usage: outhaul serve")
	fmt.Fprintln(os.Stderr, "       outhaul ca root [--path]")
```

In `serve()`, right after `cfg := config.Load(os.Getenv)`:

```go
	if err := cfg.Validate(); err != nil {
		return err
	}
```

- [ ] **Step 2: Boot the CA and manager.** In `serve()`, after the crash-recovery block and before `ensureInfra` (line ~119), add:

```go
	// Local CA: refuse to run against an enabled tunnel (mutually exclusive
	// ingress postures), then mint certs before Traefik comes up so the file
	// provider finds them on first watch.
	var certMgr *localca.Manager
	if cfg.LocalCAEnabled() {
		if on, err := st.TunnelEnabled(context.Background()); err == nil && on {
			return fmt.Errorf("OUTHAUL_LOCAL_CA is set but a Cloudflare Tunnel is enabled; disable the tunnel in Settings first (or unset OUTHAUL_LOCAL_CA)")
		}
		ca, err := localca.LoadOrCreate(cfg.CADir())
		if err != nil {
			return fmt.Errorf("local CA: %w", err)
		}
		defaultHost := cfg.AdminHost()
		if defaultHost == "" {
			if hn, err := os.Hostname(); err == nil && hn != "" {
				defaultHost = hn
			} else {
				defaultHost = "outhaul.local"
			}
		}
		certMgr = localca.NewManager(ca, cfg.CertsDir(), cfg.DynamicDir(), defaultHost, st)
		if err := certMgr.Sync(context.Background()); err != nil {
			log.Printf("WARNING: local CA cert sync: %v", err)
		}
	}
```

After `workerCtx` is created (line ~133), start rotation:

```go
	if certMgr != nil {
		go certMgr.Run(workerCtx)
	}
```

Add `"github.com/outhaul-dev/outhaul/internal/localca"` to imports. (The `certMgr` → server wiring lands in Task 8, which adds the setters; until then `certMgr` is used by Sync/Run only.)

- [ ] **Step 3: Tunnel guard + status line.** In `infraController.Enable` (line ~353), first thing in the function body:

```go
	if ic.cfg.LocalCAEnabled() {
		return fmt.Errorf("cannot enable a Cloudflare Tunnel while OUTHAUL_LOCAL_CA is set; unset it and restart first")
	}
```

In `logStatus` (line ~333), before the `ic.cfg.TLSEnabled()` branch:

```go
	if ic.cfg.LocalCAEnabled() {
		log.Printf("Traefik proxy ready on :80 and :%s (TLS via built-in local CA; install the root with `outhaul ca root`) on network %q", ic.cfg.HTTPSPort, ic.cfg.Network)
		return
	}
```

- [ ] **Step 4: Verify**

Run: `go build ./... && go test ./... && go run . ca 2>&1 | head -2`
Expected: build + tests pass; the `ca` invocation prints `usage: outhaul ca root [--path]`.

- [ ] **Step 5: Commit**

```bash
git add main.go
git commit -m 'feat: boot local CA with cert sync + rotation, add "outhaul ca root" CLI'
```

---

### Task 8: Server — cert re-sync on domain changes, /ca.pem, settings card

**Files:**
- Modify: `internal/server/server.go`, `internal/server/handlers.go`, `internal/server/settings.go`, `internal/server/templates/settings.tmpl`, `main.go` (wire `certMgr` into the server)
- Test: `internal/server/localca_test.go` (create)

**Interfaces:**
- Consumes: `(*localca.Manager).Sync` satisfies `CertSyncer`.
- Produces: `type CertSyncer interface { Sync(context.Context) error }`; `(*Server).SetCertSync(CertSyncer)`; `(*Server).SetLocalCAFile(path string)`; unauthenticated route `GET /ca.pem`; settings-page download card. Task 7 calls both setters.

- [ ] **Step 1: Write the failing test** — `internal/server/localca_test.go` (follow the `newTestEnv` harness used by `domains_test.go`; check `handlers_test.go` for the env's GET helper — if only `postForm` exists, use `e.client.Get(e.base + "/ca.pem")` in the same shape other tests fetch pages):

```go
package server

import (
	"context"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/outhaul-dev/outhaul/internal/core"
)

func TestCARootDownload(t *testing.T) {
	e := newTestEnv(t)
	caFile := filepath.Join(t.TempDir(), "rootCA.pem")
	if err := os.WriteFile(caFile, []byte("-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	e.srv.SetLocalCAFile(caFile)

	resp := e.get(t, "/ca.pem") // adapt to the harness's GET helper
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/x-pem-file" {
		t.Errorf("Content-Type = %q", ct)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) == "" || string(body)[:10] != "-----BEGIN" {
		t.Errorf("body = %q", body)
	}
}

func TestCARootNotFoundWhenDisabled(t *testing.T) {
	e := newTestEnv(t)
	resp := e.get(t, "/ca.pem")
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Errorf("status %d; want 404 when local CA is off", resp.StatusCode)
	}
}

type recordingSyncer struct{ n int }

func (r *recordingSyncer) Sync(context.Context) error { r.n++; return nil }

func TestDomainChangeTriggersCertSync(t *testing.T) {
	e := newTestEnv(t)
	e.completeSetup(t)
	rs := &recordingSyncer{}
	e.srv.SetCertSync(rs)
	app, _ := e.store.CreateApp(context.Background(), core.App{Name: "web", RepoURL: "https://x/y.git", Domain: "web.test"})
	form := url.Values{"host_kind": {"custom"}, "host": {"alias.test"}, "tls": {"on"}}
	resp := e.postForm(t, "/apps/"+itoa(app.ID)+"/domains", form)
	resp.Body.Close()
	if rs.n == 0 {
		t.Error("adding a domain must trigger a cert sync")
	}
}
```

If the harness exposes the server under a different field than `e.srv`, adapt (read `handlers_test.go`'s `newTestEnv` first).

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/server/ -run 'CARoot|CertSync'`
Expected: FAIL — `SetLocalCAFile undefined`.

- [ ] **Step 3: Implement.**

In `internal/server/server.go`, add fields to the `Server` struct (near `tunnel`): `certSync CertSyncer` and `caFile string`; add near the other setters (`SetTunnelControl`):

```go
// CertSyncer re-mints local-CA certificates; satisfied by *localca.Manager.
type CertSyncer interface{ Sync(context.Context) error }

// SetCertSync installs the hook that refreshes local-CA certs after domain
// changes. Call before Handler.
func (s *Server) SetCertSync(cs CertSyncer) { s.certSync = cs }

// SetLocalCAFile publishes the CA root certificate at GET /ca.pem.
func (s *Server) SetLocalCAFile(path string) { s.caFile = path }
```

Register the route in `Handler()` next to `GET /healthz` (line ~161) — unauthenticated by design; the root certificate is public material and devices need it before they can trust anything:

```go
	mux.HandleFunc("GET /ca.pem", s.handleCARoot)
```

Add the handler (e.g. in `settings.go`):

```go
// handleCARoot serves the local CA root certificate for device trust setup.
// Unauthenticated: the root cert is public material, and LAN devices need to
// fetch it before HTTPS to anything (including the login page) is trusted.
func (s *Server) handleCARoot(w http.ResponseWriter, r *http.Request) {
	if s.caFile == "" {
		http.NotFound(w, r)
		return
	}
	pemBytes, err := os.ReadFile(s.caFile)
	if err != nil {
		http.Error(w, "local CA not initialised", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/x-pem-file")
	w.Header().Set("Content-Disposition", `attachment; filename="outhaul-ca.pem"`)
	w.Write(pemBytes)
}
```

Add the sync hook helper (also `server.go`):

```go
// syncCerts refreshes local-CA certs after a domain change; best-effort — a
// route reaches Traefik on the next deploy anyway, minting can lag a beat.
func (s *Server) syncCerts(ctx context.Context) {
	if s.certSync == nil {
		return
	}
	if err := s.certSync.Sync(ctx); err != nil {
		log.Printf("local CA sync: %v", err)
	}
}
```

Call `s.syncCerts(r.Context())` immediately after each successful store write in `internal/server/handlers.go`:
- `handleAddDomain` (after the `AddDomain` call, ~line 687)
- `handleUpdateDomain` (after `UpdateDomain`, ~line 725)
- `handleDeleteDomain` (after `DeleteDomain`, ~line 743)
- `handleCreateApp` (after its seeding `AddDomain`, ~line 284)

In `internal/server/settings.go` `renderSettings`, add before `s.render(...)`:

```go
	data["LocalCA"] = s.caFile != ""
```

In `main.go` `serve()`, after `srv.SetTunnelControl(infra)` (line ~205), wire the manager from Task 7:

```go
	if certMgr != nil {
		srv.SetCertSync(certMgr)
		srv.SetLocalCAFile(localca.RootPath(cfg.CADir()))
	}
```

In `internal/server/templates/settings.tmpl`, add a panel (copy the exact section/heading/button classes from the adjacent Cloudflare Tunnel card at lines ~94–112):

```html
{{if .LocalCA}}
<section class="panel">
  <h2 class="panel-title">Local certificate authority</h2>
  <p class="muted env-note">HTTPS here is signed by Outhaul's built-in CA. Install this root certificate on each device once and every app domain shows as valid — new apps included.</p>
  <p><a href="/ca.pem" download>Download CA certificate</a></p>
</section>
{{end}}
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/server/`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/server/server.go internal/server/handlers.go internal/server/settings.go internal/server/templates/settings.tmpl internal/server/localca_test.go main.go
git commit -m "feat(server): /ca.pem download, settings card, cert re-sync on domain changes"
```

---

### Task 9: Installer — local-CA prompt, env var, firewall

**Files:**
- Modify: `deploy/bootstrap.sh` (`choose_ingress`, `write_env_file`, `derive_firewall_ports`, `completion_screen`, plus their call sites in `main()`)
- Test: `deploy/test/test_env.sh`, `deploy/test/test_firewall.sh` (extend); Create: `deploy/test/test_local_ca.sh`

- [ ] **Step 1: Write the failing shell tests.**

Append to `deploy/test/test_env.sh` (before the final `[ "${TESTS_FAIL:-0}" -eq 0 ]` line):

```sh
# write_env_file dest mode url email sshaddr localca
write_env_file "$dest2" c '' '' :2222 1
body=$(cat "$dest2")
assert_contains "mode c + local CA writes flag" "$body" "OUTHAUL_LOCAL_CA=true"
write_env_file "$dest2" c '' '' :2222 0
body=$(cat "$dest2")
case "$body" in *OUTHAUL_LOCAL_CA*) echo "  FAIL local CA off leaked flag" >&2; TESTS_FAIL=$((TESTS_FAIL+1));; *) echo "  ok   local CA off writes no flag";; esac
write_env_file "$dest2" a https://paas.dev me@x.com :2222 0
body=$(cat "$dest2")
case "$body" in *OUTHAUL_LOCAL_CA*) echo "  FAIL mode a leaked local CA flag" >&2; TESTS_FAIL=$((TESTS_FAIL+1));; *) echo "  ok   mode a has no local CA flag";; esac
rm -f "$dest2"
```
(with `dest2=$(mktemp)` declared above the block; keep the existing calls untouched — the new sixth argument is optional).

Append to `deploy/test/test_firewall.sh` (match its existing assertion style):

```sh
assert_eq "mode c + local CA opens web ports" "22 80 443" "$(derive_firewall_ports c '' 1)"
assert_eq "mode c without local CA stays closed" "22" "$(derive_firewall_ports c '' 0)"
assert_eq "mode a ignores localca arg" "22 80 443" "$(derive_firewall_ports a '' 1)"
```

Create `deploy/test/test_local_ca.sh` (fd-3 prompt pattern from `test_prompt.sh`):

```sh
#!/usr/bin/env sh
# choose_ingress mode 3: the local-CA prompt sets LOCAL_CA.
here=$(cd "$(dirname "$0")" && pwd)
. "$here/assert.sh"

in=$(mktemp)

printf '3\ny\n' > "$in"
out=$(OUTHAUL_INSTALLER_LIB=1 sh -c '. '"$here"'/../bootstrap.sh; choose_ingress; echo "MODE=$MODE LOCAL_CA=$LOCAL_CA"' 3<"$in" 2>/dev/null | tail -1)
assert_contains "mode 3 + yes enables local CA" "$out" "MODE=c LOCAL_CA=1"

printf '3\nn\n' > "$in"
out=$(OUTHAUL_INSTALLER_LIB=1 sh -c '. '"$here"'/../bootstrap.sh; choose_ingress; echo "MODE=$MODE LOCAL_CA=$LOCAL_CA"' 3<"$in" 2>/dev/null | tail -1)
assert_contains "mode 3 + no disables local CA" "$out" "MODE=c LOCAL_CA=0"

printf '2\n' > "$in"
out=$(OUTHAUL_INSTALLER_LIB=1 sh -c '. '"$here"'/../bootstrap.sh; choose_ingress; echo "LOCAL_CA=$LOCAL_CA"' 3<"$in" 2>/dev/null | tail -1)
assert_contains "tunnel mode leaves local CA off" "$out" "LOCAL_CA=0"

rm -f "$in"
[ "${TESTS_FAIL:-0}" -eq 0 ]
```

- [ ] **Step 2: Run to verify failure**

Run: `sh deploy/test/run.sh`
Expected: FAIL — new assertions red.

- [ ] **Step 3: Implement in `deploy/bootstrap.sh`.**

`choose_ingress` (line ~548) — initialise and prompt:

```sh
# Ingress-mode menu. Sets MODE (a/b/c), PUBLIC_URL, ACME_EMAIL, LOCAL_CA (0/1).
choose_ingress() {
	LOCAL_CA=0
	printf '\n  How should apps be reachable?\n'
	printf '    1) Public domain + automatic HTTPS (Let'\''s Encrypt)\n'
	printf '    2) Cloudflare Tunnel (no public IP / ports needed)\n'
	printf '    3) Local only (optional HTTPS from a built-in local CA)\n'
	choice=$(ask_value "  Choose 1/2/3:" 1)
	case "$choice" in
		2) MODE=b; PUBLIC_URL=''; ACME_EMAIL='';;
		3) MODE=c; PUBLIC_URL=''; ACME_EMAIL=''
		   ask_yes_no "  Enable HTTPS with a local certificate authority?" y && LOCAL_CA=1;;
		*) MODE=a
		   PUBLIC_URL=$(ask_value "  Public URL (e.g. https://paas.example.com):" '')
		   dom=${PUBLIC_URL#*://}; dom=${dom%%/*}
		   acme_preflight "$dom"
		   ACME_EMAIL=$(ask_value "  Email for Let's Encrypt:" '');;
	esac
}
```

`write_env_file` — add the sixth parameter and emit the flag for mode c:

```sh
write_env_file() { # dest mode url email sshaddr localca
	dest=$1 mode=$2 url=$3 email=$4 sshaddr=$5 localca=${6:-0}
	{
		printf '# Outhaul configuration — generated by the installer.\n'
		printf '# OUTHAUL_* overrides, one per line. Comments on their OWN line.\n'
		printf '# Edit, then: systemctl restart outhaul\n\n'
		if [ "$mode" = a ]; then
			printf 'OUTHAUL_PUBLIC_URL=%s\n' "$url"
			printf 'OUTHAUL_ACME_EMAIL=%s\n' "$email"
		fi
		if [ "$mode" = c ] && [ "$localca" = 1 ]; then
			printf 'OUTHAUL_LOCAL_CA=true\n'
		fi
		[ -n "$sshaddr" ] && printf 'OUTHAUL_SSH_ADDR=%s\n' "$sshaddr"
	} > "$dest"
	chmod 0600 "$dest"
}
```

`derive_firewall_ports` — capture the third arg before `set --` clobbers `$3`:

```sh
# Prints the space-separated, sorted, de-duplicated port set to open.
# SSH (22) is ALWAYS included. Mode a adds 80/443, as does mode c with the
# local CA (HTTPS redirect needs :80 too). A non-empty git port is added.
derive_firewall_ports() { # mode gitport localca
	mode=$1; gitport=${2:-}; localca=${3:-0}
	set -- 22
	[ "$mode" = a ] && set -- "$@" 80 443
	[ "$mode" = c ] && [ "$localca" = 1 ] && set -- "$@" 80 443
	[ -n "$gitport" ] && set -- "$@" "$gitport"
	printf '%s\n' "$@" | sort -n -u | paste -sd' ' -
}
```

`completion_screen` — sixth parameter, richer mode-c note:

```sh
completion_screen() { # mode url ports setup_url healthy(0/1) localca(0/1)
	mode=$1; url=$2; ports=$3; setup=$4; healthy=$5; localca=${6:-0}
	...
	case "$mode" in
		a) note "HTTPS: Let's Encrypt configured for $url";;
		b) _c '1;33'; printf '  → Finish Cloudflare Tunnel: log in, then paste your connector token in Settings → Tunnel\n'; _c 0;;
		c) if [ "$localca" = 1 ]; then
			note "HTTPS: built-in local CA — install the root on your devices: outhaul ca root > outhaul-ca.pem (or download /ca.pem from the admin UI)"
		   else
			note "admin UI on :8080 — set OUTHAUL_PUBLIC_URL + OUTHAUL_ACME_EMAIL later for HTTPS"
		   fi;;
	esac
	...
}
```
(only the `case "$mode"` block and the signature line change; keep the rest verbatim.)

Call sites in `main()` (find with `grep -n 'write_env_file \|derive_firewall_ports \|completion_screen ' deploy/bootstrap.sh`): append `"$LOCAL_CA"` (or `"${LOCAL_CA:-0}"`) as the new final argument to each of the three calls.

- [ ] **Step 4: Run tests**

Run: `sh deploy/test/run.sh`
Expected: all green, including pre-existing tests.

- [ ] **Step 5: Commit**

```bash
git add deploy/bootstrap.sh deploy/test/test_env.sh deploy/test/test_firewall.sh deploy/test/test_local_ca.sh
git commit -m "feat(installer): local-CA option for local-only installs"
```

---

### Task 10: Documentation + final verification

**Files:**
- Create: `docs/LOCAL-CA.md`
- Modify: `ARCHITECTURE.md` (ingress-posture section, ~lines 66–76)
- Modify (DO NOT COMMIT — pre-existing user edits): `README.md` (config table, ~lines 148–160), `docs/MANUAL-DEPLOY.md` (§4 env block)

- [ ] **Step 1: Write `docs/LOCAL-CA.md`:**

```markdown
# HTTPS on a local network — the built-in CA

Local-only installs can't get Let's Encrypt certificates (the challenge
requires a host reachable from the internet). Instead, Outhaul ships an
mkcert-style certificate authority: set `OUTHAUL_LOCAL_CA=true` (the
installer's local-only mode offers this) and Outhaul creates a CA on first
boot, then automatically mints and rotates a certificate for every app
domain with TLS enabled. Nothing to renew by hand — you install the CA root
on each of your devices once.

## Install the root certificate

Get the root, either way:

- from the server: `outhaul ca root > outhaul-ca.pem`
- from any browser: `http://<server>:8080/ca.pem` (also linked from
  Settings)

Then trust it:

- **Debian/Ubuntu**: `sudo cp outhaul-ca.pem /usr/local/share/ca-certificates/outhaul-ca.crt && sudo update-ca-certificates`
- **Fedora/Arch**: `sudo trust anchor outhaul-ca.pem`
- **macOS**: open Keychain Access → System → import the file → double-click
  it → Trust → "Always Trust". Or:
  `sudo security add-trusted-cert -d -k /Library/Keychains/System.keychain outhaul-ca.pem`
- **Windows**: double-click the file → Install Certificate → Local Machine →
  "Trusted Root Certification Authorities".
- **iOS**: AirDrop/mail the file, install the profile under Settings →
  General → VPN & Device Management, then enable full trust under Settings →
  General → About → Certificate Trust Settings.
- **Android**: Settings → Security → More → Encryption & credentials →
  Install a certificate → CA certificate (exact path varies by vendor).
- **Firefox** (uses its own store): Settings → Privacy & Security →
  Certificates → View Certificates → Authorities → Import.

Until a device trusts the root it will show certificate warnings — Outhaul
redirects HTTP to HTTPS whenever TLS is on, same as in Let's Encrypt mode.

## Details

- Root: `$DataDir/ca/rootCA.pem` (public) + `rootCA.key` (0600, never leaves
  the server). ECDSA P-256, 10-year validity.
- Leafs: 825-day validity (Apple's trust ceiling), re-minted automatically
  30 days before expiry and whenever a domain's SANs change.
- The admin-host certificate also carries the server's private IPv4
  addresses, so `https://192.168.x.x` is valid too.
- Back up `$DataDir/ca/` with the rest of the data dir: a lost CA means
  re-trusting a new root on every device. The CA is never silently
  regenerated — if its files are corrupt, Outhaul refuses to start and says
  so.
- `OUTHAUL_LOCAL_CA` is mutually exclusive with `OUTHAUL_ACME_EMAIL` and
  with a Cloudflare Tunnel.
```

- [ ] **Step 2: ARCHITECTURE.md.** In the ingress-posture section (~lines 66–76), add a third posture paragraph alongside ACME and tunnel mode:

> **Local CA** (`OUTHAUL_LOCAL_CA=true`): Traefik keeps the websecure entrypoint and HTTP→HTTPS redirect, but no ACME resolver runs. `internal/localca` mints leaf certs from a host-local root CA into `$DataDir/traefik/certs` (mounted read-only) and lists them in a file-provider config (`outhaul-local-certs.yml`) that Traefik hot-reloads; routes select certs by SNI, and the admin-host cert (with LAN-IP SANs) is the default certificate. Certs re-sync at boot, on domain changes, and on a 12-hour rotation ticker. Mutually exclusive with ACME and tunnel mode. See `docs/LOCAL-CA.md`.

Adapt wording/formatting to the surrounding section's style.

- [ ] **Step 3: README.md and docs/MANUAL-DEPLOY.md** (edit, do not commit):
- README config table: add the row `| OUTHAUL_LOCAL_CA | false | serve HTTPS from the built-in local CA (LAN installs; excludes OUTHAUL_ACME_EMAIL) — see docs/LOCAL-CA.md |` matching the table's exact column format.
- MANUAL-DEPLOY §4: below the existing HTTPS env example, add a "local network HTTPS" variant showing `OUTHAUL_LOCAL_CA=true` (no `OUTHAUL_ACME_EMAIL`), a pointer to `docs/LOCAL-CA.md`, and note §6 firewall needs 80/443 open in this mode too.

- [ ] **Step 4: Full verification**

Run: `gofmt -l . && go vet ./... && go test ./... && sh deploy/test/run.sh && go build ./...`
Expected: gofmt lists nothing; everything passes.

- [ ] **Step 5: Commit (excluding the two user-edited files)**

```bash
git add docs/LOCAL-CA.md ARCHITECTURE.md
git commit -m "docs: local CA trust guide + architecture posture"
git status --short   # README.md / docs/MANUAL-DEPLOY.md intentionally left uncommitted
```

Report to the user that README.md and docs/MANUAL-DEPLOY.md carry both their pre-existing edits and this feature's additions, uncommitted.
