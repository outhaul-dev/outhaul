package traefik

import (
	"strings"

	"github.com/outhaul-dev/outhaul/internal/config"
	"github.com/outhaul-dev/outhaul/internal/core"
)

// Routing carries the server-wide routing settings every writer of a Traefik
// router reads: the app labels, the Compose override and the admin route. One
// value keeps those writers in step as settings are added.
type Routing struct {
	TLS          bool   // HTTPS routers are written: Let's Encrypt or the local CA is on, and no tunnel is
	CertResolver string // "le" under Let's Encrypt, "" under the local CA
	GateAddress  string // the forward-auth address protected rows call
	HTTPOff      bool   // no web entrypoint: every row has one router, on websecure
	Wildcard     string // a wildcard certificate's host, when one is requested; "" = none
	HeaderName   string // a request header every router's rule requires; "" = none
	HeaderValue  string // the header's value; never logged or shown
}

// RoutingFrom builds the settings routers are written from. A Cloudflare
// Tunnel terminates TLS at its edge, so it turns HTTPS routers off even when
// ACME or the local CA is configured.
func RoutingFrom(cfg config.Config, tunnelOn bool) Routing {
	return Routing{
		TLS:          cfg.TLSEnabled() && !tunnelOn,
		CertResolver: cfg.CertResolver(),
		GateAddress:  cfg.GateAddress(),
		HTTPOff:      cfg.HTTPOff && !tunnelOn,
		Wildcard:     cfg.TLSWildcard,
		HeaderName:   cfg.RequireHeaderName,
		HeaderValue:  cfg.RequireHeaderValue,
	}
}

// Rule is the router rule RouteLabels writes for a route on host, under
// urlPath when it has one. It matches no path with a ".." segment in any
// spelling: an app that decodes such a path before resolving it would serve
// something outside the route, which may be a protected page, so the request
// isn't routed at all.
func (rt Routing) Rule(host, urlPath string) string {
	return addressRule(host, urlPath) + traversalTerm + rt.headerTerm()
}

// HeaderRequired reports whether every router must carry the request header.
func (rt Routing) HeaderRequired() bool { return rt.HeaderName != "" }

// headerTerm is the rule's last term, requiring the configured request header.
func (rt Routing) headerTerm() string {
	if rt.HeaderName == "" {
		return ""
	}
	return " && Header(`" + rt.HeaderName + "`, `" + rt.HeaderValue + "`)"
}

// routeWantsTLS reports whether a row gets its HTTPS router under rt: the
// server-wide switch is on and the row wants it (its own box, or protected).
func (rt Routing) routeWantsTLS(d core.Domain) bool {
	return rt.TLS && d.WantsTLS()
}

// WildcardCovers reports whether host is exactly one label under wildcard:
// the name a certificate for *.<domain> covers. Hosts are compared without
// case, and the bare domain, a deeper name, and a name that merely ends with
// the domain are not covered. An empty wildcard covers nothing.
func WildcardCovers(wildcard, host string) bool {
	if wildcard == "" {
		return false
	}
	domain, ok := strings.CutPrefix(strings.ToLower(wildcard), "*.")
	if !ok {
		return false
	}
	host = strings.ToLower(host)
	label, ok := strings.CutSuffix(host, "."+domain)
	return ok && label != "" && !strings.Contains(label, ".")
}
