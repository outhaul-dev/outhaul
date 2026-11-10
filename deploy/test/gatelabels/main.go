// Command gatelabels prints the Traefik labels Outhaul gives domain rows, for
// deploy/test/integration_gate.sh to put on a real container. It calls
// traefik.RouteLabels, so the integration test covers the real label code.
//
//	gatelabels [-port 8000] [-tls] [-admin-port 8080 | -gate URL] ROUTE...
//	gatelabels -compose [-port 8000] [-tls] [-admin-port 8080 | -gate URL] ROUTE...
//
// Each ROUTE is "router,host,path,internalPath,open|protected"; an empty path
// or internal path is allowed. The default output is a Docker label file (one
// key=value per line, sorted). With -compose, the routes become rows of a
// Compose app named by their router, on service "web", and the output is the
// Compose override Outhaul writes for them.
package main

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/outhaul-dev/outhaul/internal/compose"
	"github.com/outhaul-dev/outhaul/internal/config"
	"github.com/outhaul-dev/outhaul/internal/core"
	"github.com/outhaul-dev/outhaul/internal/traefik"
)

type route struct {
	router, host, path, internalPath string
	protected                        bool
}

func parseRoute(s string) (route, error) {
	f := strings.Split(s, ",")
	if len(f) != 5 || f[0] == "" || f[1] == "" {
		return route{}, fmt.Errorf("route %q: want router,host,path,internalPath,open|protected", s)
	}
	r := route{router: f[0], host: f[1], path: f[2], internalPath: f[3]}
	switch f[4] {
	case "protected":
		r.protected = true
	case "open":
	default:
		return route{}, fmt.Errorf("route %q: the last field is open or protected, not %q", s, f[4])
	}
	return r, nil
}

func main() {
	port := flag.Int("port", 8000, "container port every route forwards to")
	tls := flag.Bool("tls", false, "also give each route its HTTPS router (no cert resolver, as under the local CA)")
	adminPort := flag.String("admin-port", "8080", "Outhaul's admin port; the gate address is config.GateAddress for it")
	gate := flag.String("gate", "", "forward-auth address to use instead of the one -admin-port gives")
	asCompose := flag.Bool("compose", false, "print the Compose override instead of a label file")
	flag.Parse()

	address := *gate
	if address == "" {
		address = config.Config{ListenAddr: ":" + *adminPort}.GateAddress()
	}
	var routes []route
	for _, arg := range flag.Args() {
		r, err := parseRoute(arg)
		if err != nil {
			fmt.Fprintln(os.Stderr, "gatelabels:", err)
			os.Exit(2)
		}
		routes = append(routes, r)
	}
	if len(routes) == 0 {
		fmt.Fprintln(os.Stderr, "gatelabels: no routes given")
		os.Exit(2)
	}

	if *asCompose {
		// RouterName gives "outhaul-<app>-d<id>", so the app is named after the
		// first route and its rows are numbered in order.
		var domains []core.Domain
		for i, r := range routes {
			domains = append(domains, core.Domain{
				ID: int64(i + 1), Host: r.host, Service: "web", Port: *port,
				Path: r.path, InternalPath: r.internalPath, TLS: *tls, Protected: r.protected,
			})
		}
		os.Stdout.Write(compose.Override(core.App{Name: routes[0].router}, domains, "outhaul-gatetest-net", *tls, "", address))
		return
	}

	labels := map[string]string{}
	for _, r := range routes {
		for k, v := range traefik.RouteLabels(r.router, r.host, *port, r.path, r.internalPath, *tls, "", r.protected, address) {
			labels[k] = v
		}
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		if strings.ContainsAny(k, "\n= \t") || strings.Contains(labels[k], "\n") {
			fmt.Fprintf(os.Stderr, "gatelabels: label %q can't go in a label file\n", k)
			os.Exit(1)
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Printf("%s=%s\n", k, labels[k])
	}
}
