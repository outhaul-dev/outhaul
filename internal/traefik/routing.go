package traefik

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/outhaul-dev/outhaul/internal/config"
	"github.com/outhaul-dev/outhaul/internal/core"
)

// IngressLabel carries a short stamp of the routing settings on every routed
// container. While a request header is required, Traefik's Docker provider is
// constrained to the current stamp, so a container labelled under other
// settings has no routes at all.
const IngressLabel = "outhaul.ingress"

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

// Stamp is a short hash of the settings that change what a container's labels
// mean. It is "" when none of them is set, so an ordinary install writes no
// stamp label. The header name is compared without case, as Traefik compares
// header names.
func (rt Routing) Stamp() string {
	if rt.HeaderName == "" && !rt.HTTPOff && rt.Wildcard == "" {
		return ""
	}
	material := fmt.Sprintf("http-off=%t\nwildcard=%s\nheader-name=%s\nheader-value=%s\n",
		rt.HTTPOff, strings.ToLower(rt.Wildcard), strings.ToLower(rt.HeaderName), rt.HeaderValue)
	sum := sha256.Sum256([]byte(material))
	return hex.EncodeToString(sum[:])[:12]
}

// Constraint is the Docker-provider constraint that makes Traefik ignore
// containers whose stamp differs. It is "" unless a header is required.
func (rt Routing) Constraint() string {
	if !rt.HeaderRequired() {
		return ""
	}
	return "Label(`" + IngressLabel + "`,`" + rt.Stamp() + "`)"
}

// Stale reports whether a container Traefik is asked to route (traefik.enable
// on) was labelled under other settings: its stamp, "" when absent, isn't the
// current one.
func (rt Routing) Stale(labels map[string]string) bool {
	if labels["traefik.enable"] != "true" {
		return false
	}
	return labels[IngressLabel] != rt.Stamp()
}

// Ignores reports whether Traefik, running with rt's constraint, ignores a
// container with these labels: a header is required and the container is
// stale.
func (rt Routing) Ignores(labels map[string]string) bool {
	return rt.HeaderRequired() && rt.Stale(labels)
}

// Unserved reports whether Traefik cannot serve a container with these
// labels: either its stamp is stale under a required header, or it names the
// web entrypoint while the server publishes no port 80.
func (rt Routing) Unserved(labels map[string]string) bool {
	if rt.Ignores(labels) {
		return true
	}
	if !rt.HTTPOff {
		return false
	}
	for k, v := range labels {
		if !strings.HasPrefix(k, "traefik.http.routers.") || !strings.HasSuffix(k, ".entrypoints") {
			continue
		}
		for _, ep := range strings.Split(v, ",") {
			if strings.TrimSpace(ep) == "web" {
				return true
			}
		}
	}
	return false
}

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
