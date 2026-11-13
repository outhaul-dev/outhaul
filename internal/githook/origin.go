package githook

import (
	"net/netip"
	"strconv"
)

// The HTTPS transport sets these on `git receive-pack`; git passes its
// environment to the post-receive hook.
const (
	EnvTokenID    = "OUTHAUL_PUSH_TOKEN_ID"    // the push_tokens row id, never the token
	EnvClientAddr = "OUTHAUL_PUSH_CLIENT_ADDR" // the client's IP address
)

// Origin is what the HTTPS transport knew about a push. The zero value is a
// push over SSH.
type Origin struct {
	TokenID int64
	Addr    string
}

// HTTPS reports whether the push came over HTTPS.
func (o Origin) HTTPS() bool { return o.TokenID != 0 }

// OriginFromEnv reads the two variables. An id that isn't a positive whole
// number gives the zero Origin; an Addr that netip.ParseAddr refuses is dropped.
func OriginFromEnv(getenv func(string) string) Origin {
	id, err := strconv.ParseInt(getenv(EnvTokenID), 10, 64)
	if err != nil || id <= 0 {
		return Origin{}
	}
	o := Origin{TokenID: id}
	if a, ok := parseClientAddr(getenv(EnvClientAddr)); ok {
		o.Addr = a
	}
	return o
}

// parseClientAddr is the one place an address from the hook is accepted. A
// zone (fe80::1%eth0) is refused: a TCP peer's address never needs one, and
// the zone is free text that would reach the log. What it returns is always
// the canonical form of a zoneless address.
func parseClientAddr(s string) (string, bool) {
	a, err := netip.ParseAddr(s)
	if err != nil || a.Zone() != "" {
		return "", false
	}
	return a.String(), true
}
