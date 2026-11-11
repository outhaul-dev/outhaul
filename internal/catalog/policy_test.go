package catalog

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
	"time"

	"github.com/outhaul-dev/outhaul/internal/compose"
)

// TestEveryTemplatePassesTheStrictPolicy keeps the gallery deployable. A
// one-click template is a Compose file Outhaul runs under the same strict
// policy as a deployed compose app, so a template that uses a refused key
// would be offered in the gallery and then fail at deploy time. The test asks
// the installed `docker compose` to normalise each template with no variables
// set — the policy checks keys and shapes, not values — and requires zero
// violations. It needs no Docker daemon and skips when Compose is missing or
// older than 2.35.
func TestEveryTemplatePassesTheStrictPolicy(t *testing.T) {
	if !composeAvailable(t) {
		t.Skip("docker compose 2.35 or newer is not installed")
	}
	templates, err := All()
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(templates) == 0 {
		t.Fatal("embedded catalog is empty")
	}
	for _, tmpl := range templates {
		t.Run(tmpl.ID, func(t *testing.T) {
			const file = "docker-compose.yml"
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, file), []byte(tmpl.Compose), 0o644); err != nil {
				t.Fatalf("writing the template: %v", err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			// An explicit empty environment keeps the host's own variables
			// from filling in ${domain} and the generated secrets. Compose
			// still warns that they are unset; that warning is expected and
			// not a failure, so it goes to a buffer the test only prints if
			// Compose itself fails.
			var warnings bytes.Buffer
			runner := &compose.Docker{Env: []string{}}
			out, err := runner.Config(ctx, dir, []string{file}, "check-"+tmpl.ID, &warnings)
			if err != nil {
				t.Fatalf("docker compose config: %v\n%s", err, warnings.String())
			}
			m, err := compose.ParseModel(out)
			if err != nil {
				t.Fatalf("parsing docker compose config: %v", err)
			}
			for _, v := range compose.Check(m, compose.Policy{Strict: true, WorkDir: dir, Network: "outhaul"}) {
				t.Errorf("%s %s: %s", v.Where, v.Option, v.Reason)
			}
		})
	}
}

var composeVersion = regexp.MustCompile(`v?([0-9]+)\.([0-9]+)`)

// composeAvailable reports whether the installed Compose is 2.35 or newer,
// the first release with `config --no-env-resolution`. It is local to this
// test package because the catalog test has no other reason to talk to the
// Compose binary.
func composeAvailable(t *testing.T) bool {
	t.Helper()
	for _, args := range [][]string{{"compose", "version", "--short"}, {"compose", "version"}} {
		out, err := exec.Command("docker", args...).Output()
		if err != nil {
			continue
		}
		m := composeVersion.FindStringSubmatch(string(out))
		if m == nil {
			continue
		}
		major, _ := strconv.Atoi(m[1])
		minor, _ := strconv.Atoi(m[2])
		return major > 2 || (major == 2 && minor >= 35)
	}
	return false
}
