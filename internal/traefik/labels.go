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
// over HTTPS only when its own TLS flag and globalTLS are both true.
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
		for k, v := range RouteLabels(RouterName(app.Name, d.ID), d.Host, port, d.Path, d.InternalPath, d.TLS && globalTLS, certResolver, d.Protected, gateAddress) {
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
// Every router's first middleware is about identity. A protected route calls
// the gate at gateAddress (forward auth), which answers with IdentityHeader
// for a signed-in visitor; an open route removes any IdentityHeader the client
// sent, so an app that trusts the header can't be fooled on a route with no
// gate. Both are defined in these labels, never in the file provider: Traefik
// drops a router that references an undefined middleware, and the request
// then falls through to an open router. The identity middleware runs before
// any path rewrite, so the gate sees the URL the visitor asked for.
func RouteLabels(router, host string, port int, urlPath, internalPath string, tlsEnabled bool, certResolver string, protected bool, gateAddress string) map[string]string {
	rule := fmt.Sprintf("Host(`%s`)", host)
	if urlPath != "" {
		rule += fmt.Sprintf(" && PathPrefix(`%s`)", urlPath)
	}
	// The router names its service explicitly: a container with several
	// domains defines several services, and Traefik's Docker provider drops
	// any router it cannot link to exactly one.
	labels := map[string]string{
		"traefik.http.routers." + router + ".rule":                      rule,
		"traefik.http.routers." + router + ".entrypoints":               "web",
		"traefik.http.routers." + router + ".service":                   router,
		"traefik.http.services." + router + ".loadbalancer.server.port": strconv.Itoa(port),
	}
	mws := []string{identityMiddleware(router, protected, gateAddress, labels)}
	mws = append(mws, rewriteMiddlewares(router, urlPath, internalPath, labels)...)
	labels["traefik.http.routers."+router+".middlewares"] = strings.Join(mws, ",")
	if tlsEnabled {
		tls := router + "-tls"
		labels["traefik.http.routers."+tls+".rule"] = rule
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

// identityMiddleware defines the router's identity middleware in dst and
// returns its name: "<router>-gate", a forward auth to gateAddress that copies
// IdentityHeader from the gate's answer, for a protected route; otherwise
// "<router>-noident", a headers middleware whose empty custom request header
// value makes Traefik delete IdentityHeader from the request.
//
// The gate leaves trustForwardHeader unset, so Traefik sets X-Forwarded-Host,
// -Uri, -Method and -Proto itself and a client can't choose what the gate
// checks, even through an entrypoint that trusts forwarded headers
// (deploy/test/integration_gate.sh checks this on a real Traefik).
func identityMiddleware(router string, protected bool, gateAddress string, dst map[string]string) string {
	if protected {
		gate := router + "-gate"
		dst["traefik.http.middlewares."+gate+".forwardauth.address"] = gateAddress
		dst["traefik.http.middlewares."+gate+".forwardauth.authresponseheaders"] = IdentityHeader
		return gate
	}
	strip := router + "-noident"
	dst["traefik.http.middlewares."+strip+".headers.customrequestheaders."+IdentityHeader] = ""
	return strip
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
