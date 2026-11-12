package compose

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// A Compose file's ${NAME} reads the process environment, so under the strict
// container policy only the variables a Compose run needs are passed on.
// Outhaul's own settings and secrets, and the cloud credentials an operator
// happens to have exported, must not reach it.
func TestRestrictedEnv(t *testing.T) {
	environ := []string{
		"PATH=/usr/local/bin:/usr/bin",
		"HOME=/home/deploy",
		"USER=deploy",
		"LOGNAME=deploy",
		"LANG=C.UTF-8",
		"LC_ALL=en_GB.UTF-8",
		"LC_TIME=en_GB.UTF-8",
		"TMPDIR=/tmp",
		"XDG_DATA_HOME=/home/deploy/.local/share",
		"DOCKER_HOST=unix:///var/run/docker.sock",
		"BUILDKIT_PROGRESS=plain",
		"BUILDX_DEBUG=true",
		"COMPOSE_PROJECT_NAME=outhaul-demo",
		"SSL_CERT_FILE=/etc/ssl/certs/ca.pem",
		"SSL_CERT_DIR=/etc/ssl/certs",
		"HTTP_PROXY=http://proxy.example.com:3128",
		"https_proxy=http://proxy.example.com:3128",
		"NO_PROXY=localhost,127.0.0.1",
		// Everything below is Outhaul's or the operator's.
		"OUTHAUL_REQUIRE_HEADER_VALUE=s3cret",
		"OUTHAUL_ACME_DNS_ENV=CF_DNS_API_TOKEN",
		"AZURE_CLIENT_SECRET=cloud-secret",
		"AWS_SECRET_ACCESS_KEY=aws-secret",
		"PWD=/srv/outhaul",
		"TERM=xterm",
	}
	want := []string{
		"PATH=/usr/local/bin:/usr/bin",
		"HOME=/home/deploy",
		"USER=deploy",
		"LOGNAME=deploy",
		"LANG=C.UTF-8",
		"LC_ALL=en_GB.UTF-8",
		"LC_TIME=en_GB.UTF-8",
		"TMPDIR=/tmp",
		"XDG_DATA_HOME=/home/deploy/.local/share",
		"DOCKER_HOST=unix:///var/run/docker.sock",
		"BUILDKIT_PROGRESS=plain",
		"BUILDX_DEBUG=true",
		"COMPOSE_PROJECT_NAME=outhaul-demo",
		"SSL_CERT_FILE=/etc/ssl/certs/ca.pem",
		"SSL_CERT_DIR=/etc/ssl/certs",
		"HTTP_PROXY=http://proxy.example.com:3128",
		"https_proxy=http://proxy.example.com:3128",
		"NO_PROXY=localhost,127.0.0.1",
	}
	if got := RestrictedEnv(environ); !reflect.DeepEqual(got, want) {
		t.Errorf("RestrictedEnv = %q,\nwant %q", got, want)
	}
}

// A restricted environment must still tell the child where it runs: Go only
// adds PWD itself when it passes the whole environment on.
func TestDockerRunUsesEnvAndSetsPWD(t *testing.T) {
	t.Setenv("OUTHAUL_REQUIRE_HEADER_VALUE", "s3cret")
	dir := realTempDir(t)
	d := &Docker{Bin: tempScript(t, `#!/bin/sh
echo "PATH=$PATH"
echo "OUTHAUL_REQUIRE_HEADER_VALUE=$OUTHAUL_REQUIRE_HEADER_VALUE"
echo "PWD=$PWD"
`), Env: RestrictedEnv(os.Environ())}

	var out bytes.Buffer
	if err := d.Build(context.Background(), dir, []string{"docker-compose.yml"}, "outhaul-demo", &out); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if want := "PATH=" + os.Getenv("PATH") + "\n"; !strings.Contains(got, want) {
		t.Errorf("Build output = %q, want it to contain %q", got, want)
	}
	if want := "OUTHAUL_REQUIRE_HEADER_VALUE=\n"; !strings.Contains(got, want) {
		t.Errorf("Build output = %q, want Outhaul's own settings kept out (%q)", got, want)
	}
	if want := "PWD=" + dir + "\n"; !strings.Contains(got, want) {
		t.Errorf("Build output = %q, want the run dir as PWD (%q)", got, want)
	}
}

// Config reads a Compose file, so its run gets the same environment and PWD.
func TestDockerConfigUsesEnvAndSetsPWD(t *testing.T) {
	t.Setenv("OUTHAUL_REQUIRE_HEADER_VALUE", "s3cret")
	dir := realTempDir(t)
	d := &Docker{Bin: tempScript(t, `#!/bin/sh
echo "PATH=$PATH" >&2
echo "OUTHAUL_REQUIRE_HEADER_VALUE=$OUTHAUL_REQUIRE_HEADER_VALUE" >&2
echo "PWD=$PWD" >&2
printf '{}'
`), Env: RestrictedEnv(os.Environ())}

	var warn bytes.Buffer
	got, err := d.Config(context.Background(), dir, []string{"docker-compose.yml"}, "outhaul-demo", &warn)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "{}" {
		t.Errorf("Config stdout = %q, want the script's JSON only", got)
	}
	if want := "PATH=" + os.Getenv("PATH") + "\n"; !strings.Contains(warn.String(), want) {
		t.Errorf("Config warn = %q, want it to contain %q", warn.String(), want)
	}
	if want := "OUTHAUL_REQUIRE_HEADER_VALUE=\n"; !strings.Contains(warn.String(), want) {
		t.Errorf("Config warn = %q, want Outhaul's own settings kept out (%q)", warn.String(), want)
	}
	if want := "PWD=" + dir + "\n"; !strings.Contains(warn.String(), want) {
		t.Errorf("Config warn = %q, want the run dir as PWD (%q)", warn.String(), want)
	}
}

// Env nil is the default install: the runner inherits exactly as it always has.
func TestDockerInheritsEnvWhenUnset(t *testing.T) {
	t.Setenv("OUTHAUL_REQUIRE_HEADER_VALUE", "s3cret")
	d := &Docker{Bin: tempScript(t, `#!/bin/sh
echo "OUTHAUL_REQUIRE_HEADER_VALUE=$OUTHAUL_REQUIRE_HEADER_VALUE"
`)}

	var out bytes.Buffer
	if err := d.Stop(context.Background(), "outhaul-demo", &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "OUTHAUL_REQUIRE_HEADER_VALUE=s3cret") {
		t.Errorf("Stop output = %q, want the inherited environment", out.String())
	}
}

// realTempDir resolves symlinks (e.g. /var on macOS) so the child's PWD is
// exactly the path the test compares against.
func realTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}
