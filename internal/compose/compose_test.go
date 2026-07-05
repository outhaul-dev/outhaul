package compose

import (
	"context"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/james-smart/outhaul/internal/core"
)

func TestBuildArgs(t *testing.T) {
	got := buildArgs([]string{"docker-compose.yml", "outhaul.override.yml"}, "outhaul-web")
	want := []string{"compose", "-p", "outhaul-web", "-f", "docker-compose.yml", "-f", "outhaul.override.yml", "build"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("buildArgs = %v, want %v", got, want)
	}
}

func TestUpArgs(t *testing.T) {
	got := upArgs([]string{"docker-compose.yml"}, "outhaul-web", 90*time.Second)
	want := []string{"compose", "-p", "outhaul-web", "-f", "docker-compose.yml",
		"up", "-d", "--wait", "--wait-timeout", "90", "--remove-orphans"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("upArgs = %v, want %v", got, want)
	}
	// A zero timeout (unset config) must still produce a valid flag value.
	got = upArgs([]string{"a.yml"}, "p", 0)
	if got[len(got)-3] != "--wait-timeout" || got[len(got)-2] != "1" {
		t.Errorf("zero timeout produced %v", got[len(got)-3:])
	}
}

func TestDockerMissingBinary(t *testing.T) {
	d := &Docker{Bin: "docker-does-not-exist-7a1b"}
	err := d.Stop(context.Background(), "outhaul-web", io.Discard)
	if err == nil || !strings.Contains(err.Error(), "compose v2 plugin") {
		t.Fatalf("expected an actionable docker-not-found error, got %v", err)
	}
}

func TestProjectName(t *testing.T) {
	if got := ProjectName("web"); got != "outhaul-web" {
		t.Errorf("ProjectName = %q", got)
	}
}

func TestOverride(t *testing.T) {
	app := core.App{Name: "shop"}
	domains := []core.ComposeDomain{
		{ID: 1, Domain: "shop.example.com", Service: "web", Port: 3000},
	}

	got := string(Override(app, domains, "outhaul", false))

	for _, want := range []string{
		"services:\n  \"web\":\n",
		"      \"outhaul\": {}\n",
		"      default: {}\n",
		`"traefik.enable": "true"`,
		`"traefik.docker.network": "outhaul"`,
		"\"traefik.http.routers.outhaul-shop-d1.rule\": \"Host(`shop.example.com`)\"",
		`"traefik.http.services.outhaul-shop-d1.loadbalancer.server.port": "3000"`,
		"networks:\n  \"outhaul\":\n    external: true\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("override missing %q; got:\n%s", want, got)
		}
	}
	if strings.Contains(got, "certresolver") {
		t.Error("TLS labels must not appear when TLS is disabled")
	}

	tls := string(Override(app, domains, "outhaul", true))
	if !strings.Contains(tls, `"traefik.http.routers.outhaul-shop-d1-tls.tls.certresolver": "le"`) {
		t.Errorf("TLS override missing certresolver labels; got:\n%s", tls)
	}
}

// TestOverrideMultipleDomains: several domains on one stack — two sharing a
// service, one on another service — render as one block per service, each
// domain with its own uniquely named router.
func TestOverrideMultipleDomains(t *testing.T) {
	app := core.App{Name: "shop"}
	domains := []core.ComposeDomain{
		{ID: 7, Domain: "shop.example.com", Service: "web", Port: 3000},
		{ID: 8, Domain: "www.example.com", Service: "web", Port: 3000},
		{ID: 9, Domain: "api.example.com", Service: "api", Port: 8080},
	}

	got := string(Override(app, domains, "outhaul", false))

	for _, want := range []string{
		"\"traefik.http.routers.outhaul-shop-d7.rule\": \"Host(`shop.example.com`)\"",
		"\"traefik.http.routers.outhaul-shop-d8.rule\": \"Host(`www.example.com`)\"",
		"\"traefik.http.routers.outhaul-shop-d9.rule\": \"Host(`api.example.com`)\"",
		`"traefik.http.services.outhaul-shop-d9.loadbalancer.server.port": "8080"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("override missing %q; got:\n%s", want, got)
		}
	}
	// One services block per service, sorted: api before web, each declared once.
	if n := strings.Count(got, "  \"web\":\n"); n != 1 {
		t.Errorf("web service declared %d times, want once; got:\n%s", n, got)
	}
	apiIdx, webIdx := strings.Index(got, "  \"api\":\n"), strings.Index(got, "  \"web\":\n")
	if apiIdx < 0 || webIdx < 0 || apiIdx > webIdx {
		t.Errorf("services not rendered once each in sorted order; got:\n%s", got)
	}
	// Both web domains' labels live under the single web block.
	webBlock := got[webIdx:]
	if !strings.Contains(webBlock, "outhaul-shop-d7") || !strings.Contains(webBlock, "outhaul-shop-d8") {
		t.Errorf("web block missing one of its domains' routers; got:\n%s", webBlock)
	}
	if !strings.Contains(got[apiIdx:webIdx], "outhaul-shop-d9") {
		t.Errorf("api block missing its router; got:\n%s", got[apiIdx:webIdx])
	}
}
