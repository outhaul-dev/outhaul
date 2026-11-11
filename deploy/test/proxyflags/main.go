// Command proxyflags prints the Traefik flags and environment Outhaul gives
// its proxy for an ACME install, so deploy/test/integration_ingress.sh can
// start a real Traefik from the real proxySpec code.
//
//	proxyflags [-http-off] [-wildcard W] [-dns-provider P] [-dns-env NAME=value]... cmd|env
//
// cmd prints one flag per line; env prints one NAME=value per line.
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/outhaul-dev/outhaul/internal/traefik"
)

type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error {
	if !strings.Contains(v, "=") {
		return fmt.Errorf("want NAME=value, got %q", v)
	}
	*s = append(*s, v)
	return nil
}

func main() {
	httpOff := flag.Bool("http-off", false, "publish no plain-HTTP entrypoint")
	wildcard := flag.String("wildcard", "", "request one certificate for this wildcard")
	dnsProvider := flag.String("dns-provider", "", "lego DNS provider code")
	var dnsEnv stringList
	flag.Var(&dnsEnv, "dns-env", "NAME=value passed to Traefik (repeatable)")
	flag.Parse()
	if flag.NArg() != 1 || (flag.Arg(0) != "cmd" && flag.Arg(0) != "env") {
		fmt.Fprintln(os.Stderr, "usage: proxyflags [-http-off] [-wildcard W] [-dns-provider P] [-dns-env NAME=value]... cmd|env")
		os.Exit(2)
	}
	pc := traefik.ProxyConfig{
		ContainerName:  "outhaul-traefik",
		Image:          "traefik:v3.7.6",
		Network:        "outhaul",
		HTTPPort:       "80",
		HTTPSPort:      "443",
		ACME:           true,
		ACMEEmail:      "ops@example.com",
		ACMEStorageDir: "/var/lib/outhaul/traefik/acme",
		DNSProvider:    *dnsProvider,
		DNSEnv:         dnsEnv,
		Routing:        traefik.Routing{TLS: true, CertResolver: "le", HTTPOff: *httpOff, Wildcard: *wildcard},
	}
	cmd, env := traefik.ProxyFlags(pc)
	var lines []string
	if flag.Arg(0) == "cmd" {
		lines = cmd
	} else {
		lines = env
	}
	for _, line := range lines {
		fmt.Println(line)
	}
}
