package core

import "strings"

// Naming is where generated names get their host from: one value carries the
// server's public IP and the shared domain suffix, so every constructor that
// mints a name takes the same argument.
type Naming struct {
	ServerIP   string // the host's public IPv4, for sslip.io names; may be empty
	Suffix     string // OUTHAUL_DOMAIN_SUFFIX, lower-cased; "" keeps sslip.io names
	SuffixOnly bool   // OUTHAUL_DOMAIN_SUFFIX_ONLY: refuse hosts outside Suffix
}

// HostUnderSuffix is label under the suffix, or "" when no suffix is set.
func (n Naming) HostUnderSuffix(label string) string {
	if n.Suffix == "" {
		return ""
	}
	return label + "." + n.Suffix
}

// OneLabelUnder reports whether host is exactly one non-empty label under
// suffix, the name a certificate for *.<suffix> covers. Case and one trailing
// dot on either are ignored; the bare suffix and a deeper name are not.
func OneLabelUnder(host, suffix string) bool {
	suffix = strings.TrimSuffix(strings.ToLower(suffix), ".")
	if suffix == "" {
		return false
	}
	label, ok := strings.CutSuffix(strings.TrimSuffix(strings.ToLower(host), "."), "."+suffix)
	return ok && label != "" && !strings.Contains(label, ".")
}

// AppHost is the host an app's first row gets, and whether one can be made.
// The suffix wins over the IP; with neither, no name is generated.
func (n Naming) AppHost(appName string) (host string, ok bool) {
	if h := n.HostUnderSuffix(appName); h != "" {
		return h, true
	}
	if n.ServerIP == "" {
		return "", false
	}
	return appName + "." + n.ServerIP + ".sslip.io", true
}
