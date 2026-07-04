// Package traefik generates the container labels that configure Traefik's
// Docker provider, and manages the Traefik proxy container itself. Label
// generation is a pure function so it can be tested without a running daemon.
package traefik

import (
	"fmt"
	"strconv"

	"github.com/james-smart/outhaul/internal/core"
)

// Labels returns the Docker-provider labels that make Traefik route the app's
// domain to its container on the given internal port. The plain HTTP
// entrypoint ("web") is always configured; when tlsEnabled is true, a second
// "websecure" router is added, sharing the same service, terminating TLS via
// the "le" (Let's Encrypt) certificate resolver.
//
// The "outhaul.*" labels are ownership markers Outhaul uses to recognise the
// containers it manages (they are ignored by Traefik).
func Labels(app core.App, port int, tlsEnabled bool) map[string]string {
	labels := RouteLabels(routerName(app.Name), app.Domain, port, tlsEnabled)
	labels["traefik.enable"] = "true"
	labels["outhaul.managed"] = "true"
	labels["outhaul.app"] = app.Name
	return labels
}

// RouteLabels returns the router and service labels for one host→port route.
// Router names must be unique across everything Traefik sees: nixpacks apps
// derive theirs from the app name (via Labels above); compose apps derive one
// per published domain. Callers still need traefik.enable and any ownership
// labels — a container carrying several routes sets those once.
func RouteLabels(router, host string, port int, tlsEnabled bool) map[string]string {
	labels := map[string]string{
		"traefik.http.routers." + router + ".rule":                      fmt.Sprintf("Host(`%s`)", host),
		"traefik.http.routers." + router + ".entrypoints":               "web",
		"traefik.http.services." + router + ".loadbalancer.server.port": strconv.Itoa(port),
	}
	if tlsEnabled {
		tls := router + "-tls"
		labels["traefik.http.routers."+tls+".rule"] = fmt.Sprintf("Host(`%s`)", host)
		labels["traefik.http.routers."+tls+".entrypoints"] = "websecure"
		labels["traefik.http.routers."+tls+".tls"] = "true"
		labels["traefik.http.routers."+tls+".tls.certresolver"] = "le"
		labels["traefik.http.routers."+tls+".service"] = router
	}
	return labels
}

// routerName namespaces Traefik router/service names per app so two apps never
// collide.
func routerName(appName string) string {
	return "outhaul-" + appName
}
