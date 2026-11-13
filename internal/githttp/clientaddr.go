// Package githttp serves git over HTTPS.
package githttp

import (
	"context"
	"fmt"
	"net/http"
	"net/netip"
	"strings"
)

// Proxies is what Outhaul knows about the proxies in front of it.
type Proxies struct {
	Trusted  []netip.Prefix // OUTHAUL_TRUSTED_PROXIES; an address is a /32 or /128
	TrustAll bool           // OUTHAUL_TRUSTED_PROXIES=all
	// Traefik is Outhaul's own Traefik on the Docker network; ok is false
	// while that isn't known.
	Traefik func(ctx context.Context) (addr netip.Addr, ok bool)
}

// ParseTrusted turns OUTHAUL_TRUSTED_PROXIES entries into prefixes. An entry
// is an address (a /32 or /128) or a prefix.
func ParseTrusted(entries []string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, e := range entries {
		e = strings.TrimSpace(e)
		if strings.Contains(e, "/") {
			p, err := netip.ParsePrefix(e)
			if err != nil {
				return nil, fmt.Errorf("trusted proxy %q: %w", e, err)
			}
			out = append(out, p)
			continue
		}
		a, err := netip.ParseAddr(e)
		if err != nil {
			return nil, fmt.Errorf("trusted proxy %q: %w", e, err)
		}
		out = append(out, netip.PrefixFrom(a, a.BitLen()))
	}
	return out, nil
}

// ClientAddr is the address a request is counted and recorded under. Only a
// connection from Outhaul's own Traefik has its X-Forwarded-For read, so an
// app container on the Docker network can't choose the address recorded for
// it. Any other peer is taken at its own address. A request whose peer can't
// be read gives the zero Addr.
func (p Proxies) ClientAddr(r *http.Request) netip.Addr {
	peer := parseHostAddr(r.RemoteAddr)
	if p.Traefik == nil || !peer.IsValid() {
		return peer
	}
	tf, ok := p.Traefik(r.Context())
	if !ok || peer != normalize(tf) {
		return peer
	}
	chain := forwardedChain(r.Header.Values("X-Forwarded-For"))
	if len(chain) == 0 {
		return peer
	}
	// chain is the usable entries, rightmost first.
	switch {
	case p.TrustAll:
		if len(chain) >= 2 {
			return chain[1]
		}
		return chain[0]
	case len(p.Trusted) > 0:
		for _, a := range chain {
			if !p.trusts(a) {
				return a
			}
		}
		return chain[len(chain)-1]
	default:
		return chain[0]
	}
}

func (p Proxies) trusts(a netip.Addr) bool {
	for _, pre := range p.Trusted {
		if pre.Contains(a) {
			return true
		}
	}
	return false
}

// forwardedChain reads the header lines as one list, in order, and returns
// its entries from the right while they parse. The walk ends at the first
// entry that isn't an address.
func forwardedChain(lines []string) []netip.Addr {
	var chain []netip.Addr
	for i := len(lines) - 1; i >= 0; i-- {
		line := lines[i]
		for line != "" {
			var entry string
			if j := strings.LastIndexByte(line, ','); j >= 0 {
				entry, line = line[j+1:], line[:j]
			} else {
				entry, line = line, ""
			}
			a := parseHostAddr(entry)
			if !a.IsValid() {
				return chain
			}
			chain = append(chain, a)
		}
	}
	return chain
}

// parseHostAddr reads an address, an address with a port, or a bracketed
// address with or without one. The result is unmapped and has no zone; an
// entry that is none of those gives the zero Addr.
func parseHostAddr(s string) netip.Addr {
	s = strings.TrimSpace(s)
	if ap, err := netip.ParseAddrPort(s); err == nil {
		return normalize(ap.Addr())
	}
	if len(s) > 2 && s[0] == '[' && s[len(s)-1] == ']' {
		s = s[1 : len(s)-1]
	}
	a, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Addr{}
	}
	return normalize(a)
}

func normalize(a netip.Addr) netip.Addr {
	return a.WithZone("").Unmap()
}
