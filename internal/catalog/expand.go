package catalog

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"math/big"
	"regexp"
	"strconv"
	"strings"
)

// The variable engine is Dokploy's (packages/server/src/templates/
// processors.ts), reduced to the helpers the curated catalog needs:
//
//	${domain}       generated <app>-<hash>-<ip-dashed>.sslip.io name
//	${password[:n]} random lowercase-alphanumeric password (default 16)
//	${base64[:n]}   base64 of n random bytes (default 32)
//	${hash[:n]}     random hex string (default 8)
//	${uuid}         random UUID v4
//	${email}        random admin email (some apps demand one at first boot)
//	${username}     random lowercase username
//
// Anything else in ${...} resolves against the template's variables; unknown
// names are left verbatim so compose-level ${VAR} interpolation (which reads
// the .env the pipeline writes) keeps working.

var placeholderRe = regexp.MustCompile(`\$\{([^}]+)\}`)

// Rendered is a template made concrete: every helper generated and every
// variable reference resolved, ready to be written onto a new app.
type Rendered struct {
	Domains []DomainSpec // hosts filled in
	Env     []EnvSpec    // values filled in
}

// Render expands a template's variables, domains, and env for a new app.
// appName seeds generated domains; serverIP (may be empty) makes them
// resolve via sslip.io without DNS setup.
func Render(t Template, appName, serverIP string) (Rendered, error) {
	// First pass: generate helper values inside variable definitions.
	// Second pass: resolve variables referencing other variables — one level,
	// like Dokploy, which is all a catalog without cycles needs.
	vars := make(map[string]string, len(t.Variables))
	for name, value := range t.Variables {
		vars[name] = expand(value, nil, appName, serverIP)
	}
	for name, value := range vars {
		vars[name] = expand(value, vars, appName, serverIP)
	}

	var r Rendered
	for _, d := range t.Domains {
		host := d.Host
		if host == "" {
			host = "${domain}"
		}
		d.Host = expand(host, vars, appName, serverIP)
		if strings.Contains(d.Host, "${") {
			return Rendered{}, fmt.Errorf("catalog %s: domain host %q did not fully resolve", t.ID, d.Host)
		}
		r.Domains = append(r.Domains, d)
	}
	for _, e := range t.Env {
		e.Value = expand(e.Value, vars, appName, serverIP)
		r.Env = append(r.Env, e)
	}
	return r, nil
}

// expand replaces ${helper} and ${var} placeholders in one string.
func expand(s string, vars map[string]string, appName, serverIP string) string {
	return placeholderRe.ReplaceAllStringFunc(s, func(match string) string {
		name := match[2 : len(match)-1]
		if v, ok := generate(name, appName, serverIP); ok {
			return v
		}
		if v, ok := vars[name]; ok {
			return v
		}
		return match
	})
}

// generate produces a value for a helper placeholder; ok is false when the
// name is not a helper (so it falls through to variable lookup).
func generate(name, appName, serverIP string) (string, bool) {
	helper, arg, _ := strings.Cut(name, ":")
	n, _ := strconv.Atoi(arg)
	switch helper {
	case "domain":
		return sslipDomain(appName, serverIP), true
	case "password":
		if n <= 0 {
			n = 16
		}
		return randomString(n, "abcdefghijklmnopqrstuvwxyz0123456789"), true
	case "base64":
		if n <= 0 {
			n = 32
		}
		return base64.StdEncoding.EncodeToString(randomBytes(n)), true
	case "hash":
		if n <= 0 {
			n = 8
		}
		return hex.EncodeToString(randomBytes((n + 1) / 2))[:n], true
	case "uuid":
		b := randomBytes(16)
		b[6] = (b[6] & 0x0f) | 0x40 // version 4
		b[8] = (b[8] & 0x3f) | 0x80 // RFC 4122 variant
		h := hex.EncodeToString(b)
		return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:], true
	case "email":
		return randomString(8, "abcdefghijklmnopqrstuvwxyz") + "@" + sslipHost(appName, serverIP), true
	case "username":
		return randomString(10, "abcdefghijklmnopqrstuvwxyz"), true
	}
	return "", false
}

// sslipDomain builds Dokploy's zero-DNS domain shape: sslip.io resolves any
// name ending in a dash-separated IP, so <app>-<hash>-<ip-dashed>.sslip.io
// works the moment the stack is up. Without a server IP the name still
// parses as a domain — the user edits it on the app page.
func sslipDomain(appName, serverIP string) string {
	return appName + "-" + hex.EncodeToString(randomBytes(3)) + sslipSuffix(serverIP) + ".sslip.io"
}

// sslipHost is the stable (hash-free) flavor used inside generated emails.
func sslipHost(appName, serverIP string) string {
	return appName + sslipSuffix(serverIP) + ".sslip.io"
}

func sslipSuffix(serverIP string) string {
	if serverIP == "" {
		return ""
	}
	slug := strings.NewReplacer(".", "-", ":", "-").Replace(serverIP)
	return "-" + slug
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("catalog: crypto/rand unavailable: " + err.Error()) // no sane fallback for secrets
	}
	return b
}

func randomString(n int, alphabet string) string {
	var b strings.Builder
	max := big.NewInt(int64(len(alphabet)))
	for range n {
		i, err := rand.Int(rand.Reader, max)
		if err != nil {
			panic("catalog: crypto/rand unavailable: " + err.Error())
		}
		b.WriteByte(alphabet[i.Int64()])
	}
	return b.String()
}
