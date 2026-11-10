// Package traefik generates the container labels that configure Traefik's
// Docker provider, and manages the Traefik proxy container itself. Label
// generation is a pure function so it can be tested without a running daemon.
package traefik

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/outhaul-dev/outhaul/internal/core"
)

// AppLabels builds the full Docker-provider label set for a single-container
// app (nixpacks/dockerfile) from its domain rows: the ownership markers once,
// plus one router per domain against the container's port. globalTLS is the
// server-wide switch (ACME configured or local CA active); a row is served
// over HTTPS only when globalTLS is true and the row wants TLS: its own TLS
// flag is on, or it is protected (core.Domain.WantsTLS).
// certResolver is "le" under ACME and empty under the local CA, where the
// file provider supplies certs by SNI instead of a resolver. gateAddress is
// the forward-auth address protected rows call (config.GateAddress). With no
// domains the app is internal-only (traefik.enable=false).
func AppLabels(app core.App, domains []core.Domain, port int, globalTLS bool, certResolver, gateAddress string) map[string]string {
	labels := map[string]string{
		"traefik.enable":  "true",
		"outhaul.managed": "true",
		"outhaul.app":     app.Name,
	}
	if len(domains) == 0 {
		labels["traefik.enable"] = "false"
		return labels
	}
	for _, d := range domains {
		for k, v := range RouteLabels(RouterName(app.Name, d.ID), d.Host, port, d.Path, d.InternalPath, d.WantsTLS() && globalTLS, certResolver, d.Protected, gateAddress) {
			labels[k] = v
		}
	}
	return labels
}

// RouteLabels returns the router, service, and middleware labels for one
// host[/path]→port route. Router names must be unique across everything Traefik
// sees; callers derive them via RouterName. A container carrying several routes
// sets traefik.enable and the ownership labels once (see AppLabels / the compose
// override). certResolver is "le" under ACME, which asks Traefik to obtain a
// cert for the router; an empty resolver leaves the TLS router without one,
// which is correct under the local CA, where the file provider serves a
// pre-issued cert by SNI instead.
//
// Every router's middlewares start with identity. A protected route calls the
// gate at gateAddress (forward auth) first, which answers with IdentityHeader
// for a signed-in visitor, then removes the header's other spellings
// (identityAliases); an open route removes the header and its other
// spellings, so an app that trusts the header can't be fooled on a route with
// no gate. All of them are defined in these labels, never in the file
// provider: Traefik drops a router that references an undefined middleware,
// and the request then falls through to an open router. They run before any
// path rewrite, so the gate sees the URL the visitor asked for.
func RouteLabels(router, host string, port int, urlPath, internalPath string, tlsEnabled bool, certResolver string, protected bool, gateAddress string) map[string]string {
	rule := Rule(host, urlPath)
	// Traefik ranks routers by the length of their rule unless one names a
	// priority. The traversal term lengthens every rule, so each router names
	// the length of its rule without it: the rank a container deployed before
	// the term has, so that one and a redeployed one on the same host still
	// order by path as they did.
	priority := strconv.Itoa(len(addressRule(host, urlPath)))
	// The router names its service explicitly: a container with several
	// domains defines several services, and Traefik's Docker provider drops
	// any router it cannot link to exactly one.
	labels := map[string]string{
		"traefik.http.routers." + router + ".rule":                      rule,
		"traefik.http.routers." + router + ".priority":                  priority,
		"traefik.http.routers." + router + ".entrypoints":               "web",
		"traefik.http.routers." + router + ".service":                   router,
		"traefik.http.services." + router + ".loadbalancer.server.port": strconv.Itoa(port),
	}
	mws := identityMiddlewares(router, protected, gateAddress, labels)
	mws = append(mws, rewriteMiddlewares(router, urlPath, internalPath, labels)...)
	labels["traefik.http.routers."+router+".middlewares"] = strings.Join(mws, ",")
	if tlsEnabled {
		tls := router + "-tls"
		labels["traefik.http.routers."+tls+".rule"] = rule
		labels["traefik.http.routers."+tls+".priority"] = priority
		labels["traefik.http.routers."+tls+".entrypoints"] = "websecure"
		labels["traefik.http.routers."+tls+".tls"] = "true"
		if certResolver != "" {
			labels["traefik.http.routers."+tls+".tls.certresolver"] = certResolver
		}
		labels["traefik.http.routers."+tls+".service"] = router
		labels["traefik.http.routers."+tls+".middlewares"] = strings.Join(mws, ",")
	}
	return labels
}

// IdentityHeader carries the signed-in visitor's identity to an app behind a
// protected route: the SSO email, or the username of a password account.
const IdentityHeader = "X-Outhaul-User"

// IdentityHeaderAlias is IdentityHeader spelled with underscores. Servers that
// turn headers into CGI-style variables read both as HTTP_X_OUTHAUL_USER, so a
// client must not be able to send it to any app either.
const IdentityHeaderAlias = "X_Outhaul_User"

// identityAliases are every spelling of IdentityHeader, other than itself, that
// a CGI-style server turns into the same variable: each separator a hyphen or
// an underscore. Traefik matches header names case-insensitively.
var identityAliases = []string{IdentityHeaderAlias, "X-Outhaul_User", "X_Outhaul-User"}

// identityMiddlewares defines the router's identity middlewares in dst and
// returns their names in order. A protected route gets "<router>-gate", a
// forward auth to gateAddress that replaces IdentityHeader with the gate's
// answer, then "<router>-noalias", which removes IdentityHeaderAlias and the
// other identityAliases. An open route gets "<router>-noident", which removes
// IdentityHeader and its aliases. A headers middleware removes a request
// header whose custom value is empty.
//
// The gate sets trustForwardHeader to false, so Traefik drops every
// X-Forwarded-* header a client sent and sets X-Forwarded-Host, -Uri, -Method
// and -Proto itself, even through an entrypoint that trusts forwarded headers;
// a client can't choose what the gate checks (deploy/test/integration_gate.sh
// checks this on a real Traefik).
func identityMiddlewares(router string, protected bool, gateAddress string, dst map[string]string) []string {
	strip := func(name string, headers ...string) {
		for _, h := range headers {
			dst["traefik.http.middlewares."+name+".headers.customrequestheaders."+h] = ""
		}
	}
	if protected {
		gate, noalias := router+"-gate", router+"-noalias"
		dst["traefik.http.middlewares."+gate+".forwardauth.address"] = gateAddress
		dst["traefik.http.middlewares."+gate+".forwardauth.authresponseheaders"] = IdentityHeader
		dst["traefik.http.middlewares."+gate+".forwardauth.trustforwardheader"] = "false"
		strip(noalias, identityAliases...)
		return []string{gate, noalias}
	}
	noident := router + "-noident"
	strip(noident, append([]string{IdentityHeader}, identityAliases...)...)
	return []string{noident}
}

// RowHasGate reports whether labels (a container's) put router behind its
// gate: the router, and its "-tls" twin when it has any label at all (Traefik
// gives a router with no rule label a default one), list "<router>-gate"
// first, and that middleware's forward-auth address is set. Anything less
// means some request on that route can reach the app without the gate.
func RowHasGate(labels map[string]string, router string) bool {
	gate := router + "-gate"
	if labels["traefik.http.middlewares."+gate+".forwardauth.address"] == "" {
		return false
	}
	gatedFirst := func(r string) bool {
		first, _, _ := strings.Cut(labels["traefik.http.routers."+r+".middlewares"], ",")
		return strings.TrimSpace(first) == gate
	}
	if !gatedFirst(router) {
		return false
	}
	tls := router + "-tls"
	tlsPrefix := "traefik.http.routers." + tls + "."
	for k := range labels {
		if strings.HasPrefix(k, tlsPrefix) {
			return gatedFirst(tls)
		}
	}
	return true
}

// HasGateLabel reports whether labels (a container's) define any route's gate:
// a forward-auth middleware named "<router>-gate", whatever its address. While
// a container carries one, Traefik may call the gate, so it must keep the
// gate's host mapped to the host gateway (ProxyConfig.ProtectedRoutes).
func HasGateLabel(labels map[string]string) bool {
	for k := range labels {
		if strings.HasPrefix(k, "traefik.http.middlewares.") && strings.HasSuffix(k, "-gate.forwardauth.address") {
			return true
		}
	}
	return false
}

// rewriteMiddlewares defines the strip/add-prefix middlewares that turn the
// external urlPath into the internalPath the container receives, writing their
// definition labels into dst and returning the router's middleware names in
// application order. No rewrite is needed when there is no path, the internal
// path is blank (forward unchanged), or it already equals the external path. An
// internal path only ever rewrites a matched external prefix, so it is ignored
// without one — input validation rejects that combination before it reaches here.
func rewriteMiddlewares(router, urlPath, internalPath string, dst map[string]string) []string {
	if urlPath == "" || internalPath == "" || internalPath == urlPath {
		return nil
	}
	strip := router + "-strip"
	dst["traefik.http.middlewares."+strip+".stripprefix.prefixes"] = urlPath
	mws := []string{strip}
	if internalPath != "/" {
		add := router + "-addpfx"
		dst["traefik.http.middlewares."+add+".addprefix.prefix"] = internalPath
		mws = append(mws, add)
	}
	return mws
}

// RouterName namespaces one domain row's Traefik router/service across apps.
func RouterName(appName string, domainID int64) string {
	return fmt.Sprintf("outhaul-%s-d%d", appName, domainID)
}

// traversalPattern matches a path with a ".." segment, in any mix of literal
// and percent-encoded spellings, in any case: a start or separator ("/",
// "%2f", "\", "%5c"), two dots ("." or "%2e"), then a separator, ";",
// "%3b", or the end. PathRegexp sees the path still escaped, after Traefik's
// own clean-up has resolved the plain "/../" forms (checked on traefik:v3.7.6
// by deploy/test/integration_gate.sh).
const traversalPattern = `(?i)(^|/|%2f|\\|%5c)(\.|%2e){2}(/|%2f|\\|%5c|;|%3b|$)`

// traversalTerm is the part of every Rule that refuses such a path.
const traversalTerm = " && !PathRegexp(`" + traversalPattern + "`)"

// addressRule is the part of a Rule that names its address: host, and
// urlPath when it has one. It is the whole rule an Outhaul before the
// traversal term wrote.
func addressRule(host, urlPath string) string {
	rule := fmt.Sprintf("Host(`%s`)", host)
	if urlPath != "" {
		rule += fmt.Sprintf(" && PathPrefix(`%s`)", urlPath)
	}
	return rule
}

// Rule is the router rule RouteLabels writes for a route on host, under
// urlPath when it has one. It matches no path with a ".." segment in any
// spelling: an app that decodes such a path before resolving it would serve
// something outside the route, which may be a protected page, so the request
// isn't routed at all.
func Rule(host, urlPath string) string {
	return addressRule(host, urlPath) + traversalTerm
}

// RuleAddress returns the host and path (or "") a router's rule names, when
// it is a rule RouteLabels writes, or one an older Outhaul wrote without the
// traversal term. Anything else has no address (ok is false). Two rules with
// the same address serve the same requests a row is about, so callers that
// match a container's router to a row compare addresses, not strings.
func RuleAddress(rule string) (host, urlPath string, ok bool) {
	rest, ok := strings.CutPrefix(rule, "Host(`")
	if !ok {
		return "", "", false
	}
	host, rest, ok = strings.Cut(rest, "`)")
	if !ok || host == "" || strings.Contains(host, "`") {
		return "", "", false
	}
	if p, found := strings.CutPrefix(rest, " && PathPrefix(`"); found {
		urlPath, rest, ok = strings.Cut(p, "`)")
		if !ok || urlPath == "" || strings.Contains(urlPath, "`") {
			return "", "", false
		}
	}
	if rest != "" && rest != traversalTerm {
		return "", "", false
	}
	return host, urlPath, true
}

// SameAddress reports whether rule (a router's rule label) names host and
// urlPath: its host equal ignoring case, as Traefik's Host matcher compares
// hosts, and its path exactly, as PathPrefix compares paths.
func SameAddress(rule, host, urlPath string) bool {
	h, p, ok := RuleAddress(rule)
	return ok && strings.EqualFold(h, host) && p == urlPath
}
