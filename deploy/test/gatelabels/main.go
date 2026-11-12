// Command gatelabels prints the Traefik labels Outhaul gives domain rows, for
// deploy/test/integration_gate.sh to put on a real container. It calls
// traefik.RouteLabels, and decides each row's HTTPS router as
// traefik.AppLabels does (core.Domain.WantsTLS), so the integration test
// covers the real label code.
//
//	gatelabels [-port 8000] [-tls] [-http-off] [-wildcard W] [-resolver NAME] [-header NAME=VALUE] [-admin-port 8080 | -gate URL] ROUTE...
//	gatelabels -compose [-port 8000] [-tls] [-http-off] [-wildcard W] [-resolver NAME] [-header NAME=VALUE] [-admin-port 8080 | -gate URL] ROUTE...
//
// Each ROUTE is "router,host,path,internalPath,open|protected[,notls]"; an
// empty path or internal path is allowed. notls turns the row's own TLS flag
// off (it is on otherwise); -tls is the server-wide switch. The default output is a Docker label file (one
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
	protected, notls                 bool
}

// domain is the row as the store would hold it.
func (r route) domain() core.Domain {
	return core.Domain{Host: r.host, Path: r.path, InternalPath: r.internalPath, TLS: !r.notls, Protected: r.protected}
}

func parseRoute(s string) (route, error) {
	f := strings.Split(s, ",")
	notls := len(f) == 6 && f[5] == "notls"
	if notls {
		f = f[:5]
	}
	if len(f) != 5 || f[0] == "" || f[1] == "" {
		return route{}, fmt.Errorf("route %q: want router,host,path,internalPath,open|protected[,notls]", s)
	}
	r := route{router: f[0], host: f[1], path: f[2], internalPath: f[3], notls: notls}
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
	tls := flag.Bool("tls", false, "also give each route its HTTPS router")
	httpOff := flag.Bool("http-off", false, "publish no plain HTTP: every route has one router on websecure")
	wildcard := flag.String("wildcard", "", "the wildcard certificate every covered host inherits")
	resolver := flag.String("resolver", "", `the TLS cert resolver ("le" under Let's Encrypt; empty under the local CA)`)
	header := flag.String("header", "", "NAME=VALUE every rule requires")
	adminPort := flag.String("admin-port", "8080", "Outhaul's admin port; the gate address is config.GateAddress for it")
	gate := flag.String("gate", "", "forward-auth address to use instead of the one -admin-port gives")
	asCompose := flag.Bool("compose", false, "print the Compose override instead of a label file")
	flag.Parse()

	address := *gate
	if address == "" {
		address = config.Config{ListenAddr: ":" + *adminPort}.GateAddress()
	}
	rt := traefik.Routing{TLS: *tls, CertResolver: *resolver, GateAddress: address, HTTPOff: *httpOff, Wildcard: *wildcard}
	if *header != "" {
		name, value, ok := strings.Cut(*header, "=")
		if !ok || name == "" || value == "" {
			fmt.Fprintln(os.Stderr, "gatelabels: -header wants NAME=VALUE")
			os.Exit(2)
		}
		rt.HeaderName, rt.HeaderValue = name, value
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
			d := r.domain()
			d.ID, d.Service, d.Port = int64(i+1), "web", *port
			domains = append(domains, d)
		}
		os.Stdout.Write(compose.Override(core.App{Name: routes[0].router}, domains, "outhaul-gatetest-net", rt, nil))
		return
	}

	labels := map[string]string{}
	if stamp := rt.Stamp(); stamp != "" {
		labels[traefik.IngressLabel] = stamp
	}
	for _, r := range routes {
		for k, v := range traefik.RouteLabels(r.router, r.domain(), *port, rt) {
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
