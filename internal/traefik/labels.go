// Package traefik generates the container labels that configure Traefik's
// Docker provider, and manages the Traefik proxy container itself. Label
// generation is a pure function so it can be tested without a running daemon.
package traefik

import (
	"fmt"
	"strconv"

	"github.com/slipwaydev/slipway/internal/core"
)

// Labels returns the Docker-provider labels that make Traefik route the app's
// domain to its container on the given internal port. M1 uses the plain HTTP
// entrypoint ("web"); TLS is a later seam.
//
// The "slipway.*" labels are ownership markers Slipway uses to recognise the
// containers it manages (they are ignored by Traefik).
func Labels(app core.App, port int) map[string]string {
	router := routerName(app.Name)
	return map[string]string{
		"traefik.enable":  "true",
		"slipway.managed": "true",
		"slipway.app":     app.Name,

		"traefik.http.routers." + router + ".rule":        fmt.Sprintf("Host(`%s`)", app.Domain),
		"traefik.http.routers." + router + ".entrypoints": "web",
		"traefik.http.services." + router + ".loadbalancer.server.port": strconv.Itoa(port),
	}
}

// routerName namespaces Traefik router/service names per app so two apps never
// collide.
func routerName(appName string) string {
	return "slipway-" + appName
}
