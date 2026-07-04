package compose

import (
	"context"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/slipwaydev/slipway/internal/core"
)

func TestBuildArgs(t *testing.T) {
	got := buildArgs([]string{"docker-compose.yml", "slipway.override.yml"}, "slipway-web")
	want := []string{"compose", "-p", "slipway-web", "-f", "docker-compose.yml", "-f", "slipway.override.yml", "build"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("buildArgs = %v, want %v", got, want)
	}
}

func TestUpArgs(t *testing.T) {
	got := upArgs([]string{"docker-compose.yml"}, "slipway-web", 90*time.Second)
	want := []string{"compose", "-p", "slipway-web", "-f", "docker-compose.yml",
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
	err := d.Stop(context.Background(), "slipway-web", io.Discard)
	if err == nil || !strings.Contains(err.Error(), "compose v2 plugin") {
		t.Fatalf("expected an actionable docker-not-found error, got %v", err)
	}
}

func TestProjectName(t *testing.T) {
	if got := ProjectName("web"); got != "slipway-web" {
		t.Errorf("ProjectName = %q", got)
	}
}

func TestOverride(t *testing.T) {
	app := core.App{Name: "shop", Domain: "shop.example.com", ComposeService: "web", ComposePort: 3000}

	got := string(Override(app, "slipway", false))

	for _, want := range []string{
		"services:\n  \"web\":\n",
		"      \"slipway\": {}\n",
		"      default: {}\n",
		`"traefik.enable": "true"`,
		`"traefik.docker.network": "slipway"`,
		"\"traefik.http.routers.slipway-shop.rule\": \"Host(`shop.example.com`)\"",
		`"traefik.http.services.slipway-shop.loadbalancer.server.port": "3000"`,
		"networks:\n  \"slipway\":\n    external: true\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("override missing %q; got:\n%s", want, got)
		}
	}
	if strings.Contains(got, "certresolver") {
		t.Error("TLS labels must not appear when TLS is disabled")
	}

	tls := string(Override(app, "slipway", true))
	if !strings.Contains(tls, `"traefik.http.routers.slipway-shop-tls.tls.certresolver": "le"`) {
		t.Errorf("TLS override missing certresolver labels; got:\n%s", tls)
	}
}
