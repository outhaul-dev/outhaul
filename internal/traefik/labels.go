// Package traefik generates the container labels that configure Traefik's
// Docker provider, and manages the Traefik proxy container itself. Label
// generation is a pure function so it can be tested without a running daemon.
package traefik

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/james-smart/outhaul/internal/core"
)

// AppLabels builds the full Docker-provider label set for a single-container
// app (nixpacks/dockerfile) from its domain rows: the ownership markers once,
// plus one router per domain against the container's port. globalTLS is the
// server-wide switch (ACME configured); a row is served over HTTPS only when
// its own TLS flag and globalTLS are both true. With no domains the app is
// internal-only (traefik.enable=false).
func AppLabels(app core.App, domains []core.Domain, port int, globalTLS bool) map[string]string {
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
		for k, v := range RouteLabels(RouterName(app.Name, d.ID), d.Host, port, d.Path, d.InternalPath, d.TLS && globalTLS) {
			labels[k] = v
		}
	}
	return labels
}

// RouteLabels returns the router, service, and middleware labels for one
// host[/path]→port route. Router names must be unique across everything Traefik
// sees; callers derive them via RouterName. A container carrying several routes
// sets traefik.enable and the ownership labels once (see AppLabels / the compose
// override).
func RouteLabels(router, host string, port int, urlPath, internalPath string, tlsEnabled bool) map[string]string {
	rule := fmt.Sprintf("Host(`%s`)", host)
	if urlPath != "" {
		rule += fmt.Sprintf(" && PathPrefix(`%s`)", urlPath)
	}
	labels := map[string]string{
		"traefik.http.routers." + router + ".rule":                      rule,
		"traefik.http.routers." + router + ".entrypoints":               "web",
		"traefik.http.services." + router + ".loadbalancer.server.port": strconv.Itoa(port),
	}
	mws := rewriteMiddlewares(router, urlPath, internalPath, labels)
	if len(mws) > 0 {
		labels["traefik.http.routers."+router+".middlewares"] = strings.Join(mws, ",")
	}
	if tlsEnabled {
		tls := router + "-tls"
		labels["traefik.http.routers."+tls+".rule"] = rule
		labels["traefik.http.routers."+tls+".entrypoints"] = "websecure"
		labels["traefik.http.routers."+tls+".tls"] = "true"
		labels["traefik.http.routers."+tls+".tls.certresolver"] = "le"
		labels["traefik.http.routers."+tls+".service"] = router
		if len(mws) > 0 {
			labels["traefik.http.routers."+tls+".middlewares"] = strings.Join(mws, ",")
		}
	}
	return labels
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
