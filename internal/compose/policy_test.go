package compose

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/outhaul-dev/outhaul/internal/core"
)

// strictPolicy is the policy for Outhaul's strict mode. The recorded path
// fixtures place the checkout at /work.
var strictPolicy = Policy{Strict: true, WorkDir: "/work"}

// networkPolicy is strictPolicy with the shared network a stack may join.
var networkPolicy = Policy{Strict: true, WorkDir: "/work", Network: "outhaul"}

// policyCases names every recorded or hand-written model under
// testdata/policy. A want entry with an empty Reason only requires it to be
// non-empty; a set Reason is compared exactly.
//
// pureOnly marks JSON that cannot come from `docker compose config`: Compose
// drops service-level x-* keys and refuses a top-level key that is neither an
// extension field nor a standard section before it prints a model. The
// real-Compose test skips these rows.
var policyCases = []policyCase{
	// Refused service keys, one fixture each.
	{
		name: "privileged", file: "privileged.json", policy: strictPolicy,
		want: []Violation{{Where: `service "web"`, Option: "privileged"}},
	},
	{
		name: "cap_add", file: "cap-add.json", policy: strictPolicy,
		want: []Violation{{Where: `service "web"`, Option: "cap_add"}},
	},
	{
		name: "devices", file: "devices.json", policy: strictPolicy,
		want: []Violation{{Where: `service "web"`, Option: "devices"}},
	},
	{
		name: "cgroup_parent", file: "cgroup-parent.json", policy: strictPolicy,
		want: []Violation{{Where: `service "web"`, Option: "cgroup_parent"}},
	},
	{
		name: "ports", file: "ports.json", policy: strictPolicy,
		want: []Violation{{
			Where:  `service "web"`,
			Option: "ports",
			Reason: "publishing a host port is not allowed. Give the service a domain; services reach each other on the stack's network.",
		}},
	},
	{
		name: "uts", file: "uts.json", policy: strictPolicy,
		want: []Violation{{Where: `service "web"`, Option: "uts"}},
	},
	{
		name: "userns_mode", file: "userns-mode.json", policy: strictPolicy,
		want: []Violation{{Where: `service "web"`, Option: "userns_mode"}},
	},
	{
		name: "device_cgroup_rules", file: "device-cgroup-rules.json", policy: strictPolicy,
		want: []Violation{{Where: `service "web"`, Option: "device_cgroup_rules"}},
	},
	{
		name: "gpus", file: "gpus.json", policy: strictPolicy,
		want: []Violation{{Where: `service "web"`, Option: "gpus"}},
	},
	{
		name: "runtime", file: "runtime.json", policy: strictPolicy,
		want: []Violation{{Where: `service "web"`, Option: "runtime"}},
	},
	{
		name: "use_api_socket", file: "use-api-socket.json", policy: strictPolicy,
		want: []Violation{{Where: `service "web"`, Option: "use_api_socket"}},
	},
	{
		name: "provider", file: "provider.json", policy: strictPolicy,
		want: []Violation{{Where: `service "web"`, Option: "provider"}},
	},
	{
		name: "pre_start", file: "pre-start.json", policy: strictPolicy,
		want: []Violation{{Where: `service "web"`, Option: "pre_start"}},
	},
	{
		name: "container_name", file: "container-name.json", policy: strictPolicy,
		want: []Violation{{Where: `service "web"`, Option: "container_name"}},
	},
	{
		name: "oom_kill_disable", file: "oom-kill-disable.json", policy: strictPolicy,
		want: []Violation{{Where: `service "web"`, Option: "oom_kill_disable"}},
	},
	{
		name: "mac_address", file: "mac-address.json", policy: strictPolicy,
		want: []Violation{{Where: `service "web"`, Option: "mac_address"}},
	},
	{
		name: "external_links", file: "external-links.json", policy: strictPolicy,
		want: []Violation{{Where: `service "web"`, Option: "external_links"}},
	},
	{
		name: "annotations", file: "annotations.json", policy: strictPolicy,
		want: []Violation{{Where: `service "web"`, Option: "annotations"}},
	},
	{
		name: "cpu_rt_runtime", file: "cpu-rt-runtime.json", policy: strictPolicy,
		want: []Violation{{Where: `service "web"`, Option: "cpu_rt_runtime"}},
	},
	{
		name: "cpu_rt_period", file: "cpu-rt-period.json", policy: strictPolicy,
		want: []Violation{{Where: `service "web"`, Option: "cpu_rt_period"}},
	},
	{
		name: "cpu_count", file: "cpu-count.json", policy: strictPolicy,
		want: []Violation{{Where: `service "web"`, Option: "cpu_count"}},
	},
	{
		name: "cpu_percent", file: "cpu-percent.json", policy: strictPolicy,
		want: []Violation{{Where: `service "web"`, Option: "cpu_percent"}},
	},
	{
		name: "credential_spec", file: "credential-spec.json", policy: strictPolicy,
		want: []Violation{{Where: `service "web"`, Option: "credential_spec"}},
	},
	{
		name: "isolation", file: "isolation.json", policy: strictPolicy,
		want: []Violation{{Where: `service "web"`, Option: "isolation"}},
	},
	{
		name: "models", file: "models.json", policy: strictPolicy,
		want: []Violation{{Where: `service "web"`, Option: "models"}},
	},
	{
		name: "unknown service key", file: "unknown-service-key.json", policy: strictPolicy, pureOnly: true,
		want: []Violation{{
			Where:  `service "web"`,
			Option: "future_option",
			Reason: "this key is not in the server's Compose allow-list.",
		}},
	},

	// The spec's allowed-as-written list, in one service.
	{name: "allowed service keys", file: "allowed-service-keys.json", policy: strictPolicy},

	// Conditional keys: passing forms.
	{name: "volumes allowed", file: "volumes-allowed.json", policy: strictPolicy},
	{
		name: "volumes_from a service", file: "volumes-from.json", policy: strictPolicy,
		want: []Violation{{Where: `service "other"`, Option: "volumes_from"}},
	},
	{
		name: "network_mode", file: "network-mode.json", policy: strictPolicy,
		want: []Violation{
			{Where: `service "container"`, Option: "network_mode"},
			{Where: `service "host"`, Option: "network_mode"},
		},
	},
	{
		name: "pid", file: "pid.json", policy: strictPolicy,
		want: []Violation{
			{Where: `service "container"`, Option: "pid"},
			{Where: `service "host"`, Option: "pid"},
		},
	},
	{
		name: "ipc", file: "ipc.json", policy: strictPolicy,
		want: []Violation{
			{Where: `service "container"`, Option: "ipc"},
			{Where: `service "host"`, Option: "ipc"},
		},
	},
	{
		name: "cgroup", file: "cgroup.json", policy: strictPolicy,
		want: []Violation{{Where: `service "host"`, Option: "cgroup"}},
	},
	{
		name: "security_opt", file: "security-opt.json", policy: strictPolicy,
		want: []Violation{
			{Where: `service "apparmor"`, Option: "security_opt"},
			{Where: `service "label"`, Option: "security_opt"},
			{Where: `service "seccomp"`, Option: "security_opt"},
			{Where: `service "systempaths"`, Option: "security_opt"},
		},
	},
	{
		name: "networks", file: "networks.json", policy: strictPolicy,
		want: []Violation{
			{Where: `service "ipv4"`, Option: "networks.default.ipv4_address"},
			{Where: `service "ipv6"`, Option: "networks.default.ipv6_address"},
			{Where: `service "mac"`, Option: "networks.default.mac_address"},
			{Where: `service "other"`, Option: "networks.default.link_local_ips"},
		},
	},
	{
		name: "secrets", file: "secrets.json", policy: strictPolicy,
		want: []Violation{
			{Where: `secret "extbad"`, Option: "external"},
			{Where: `secret "filebad"`, Option: "file"},
			{Where: `service "web"`, Option: "secrets"},
			{Where: `service "web"`, Option: "secrets"},
		},
	},
	{
		name: "configs", file: "configs.json", policy: strictPolicy,
		want: []Violation{
			{Where: `config "extbad"`, Option: "external"},
			{Where: `config "filebad"`, Option: "file"},
			{Where: `service "web"`, Option: "configs"},
			{Where: `service "web"`, Option: "configs"},
		},
	},
	{
		name: "hooks", file: "hooks.json", policy: strictPolicy,
		want: []Violation{
			{Where: `service "web"`, Option: "post_start", Reason: "a hook may not run privileged."},
			{Where: `service "web"`, Option: "pre_stop", Reason: "a hook may not run privileged."},
		},
	},
	{name: "hooks allowed", file: "hooks-allowed.json", policy: strictPolicy},
	{
		name: "unknown hook key", file: "unknown-hook-key.json", policy: strictPolicy, pureOnly: true,
		want: []Violation{{
			Where:  `service "web"`,
			Option: "post_start.devices",
			Reason: "this key is not in the server's Compose allow-list.",
		}},
	},
	{
		name: "deploy", file: "deploy.json", policy: strictPolicy,
		want: []Violation{
			{Where: `service "refused"`, Option: "deploy.resources.reservations.devices"},
			{Where: `service "refused"`, Option: "deploy.resources.reservations.generic_resources"},
		},
	},
	{
		name: "oom_score_adj", file: "oom-score-adj.json", policy: strictPolicy,
		want: []Violation{{Where: `service "negative"`, Option: "oom_score_adj"}},
	},
	{
		name: "refused volumes", file: "volumes-refused.json", policy: strictPolicy,
		want: []Violation{
			{Where: `service "web"`, Option: "volumes"},
			{Where: `service "web"`, Option: "volumes"},
			{Where: `service "web"`, Option: "volumes"},
			{Where: `service "web"`, Option: "volumes"},
		},
	},

	// Top-level sections.
	{name: "top-level sections", file: "top-level-allowed.json", policy: networkPolicy},
	{name: "extension field", file: "extension-field.json", policy: strictPolicy, pureOnly: true},
	{
		name: "unknown top-level key", file: "unknown-top-level-key.json", policy: strictPolicy, pureOnly: true,
		want: []Violation{{
			Where:  "compose file",
			Option: "future_section",
			Reason: "this section is not in the server's Compose allow-list.",
		}},
	},
	{name: "anchors", file: "anchors.json", policy: strictPolicy},
	{
		name: "anchors resolved into a service", file: "anchors-refused.json", policy: strictPolicy,
		want: []Violation{{Where: `service "web"`, Option: "privileged"}},
	},
	{
		name: "top-level volumes", file: "volumes-top-level.json", policy: strictPolicy,
		want: []Violation{
			{Where: `volume "custom"`, Option: "name"},
			{Where: `volume "extra"`, Option: "external"},
			{Where: `volume "host"`, Option: "driver"},
			{Where: `volume "opts"`, Option: "driver_opts"},
		},
	},
	{
		name: "top-level networks", file: "networks-top-level.json", policy: networkPolicy,
		want: []Violation{
			{Where: `network "custom"`, Option: "name"},
			{Where: `network "driver"`, Option: "driver"},
			{Where: `network "extra"`, Option: "external"},
			{Where: `network "ipamdriver"`, Option: "ipam.driver"},
			{Where: `network "opts"`, Option: "driver_opts"},
			{Where: `network "other"`, Option: "external"},
			{Where: `network "subnet"`, Option: "ipam.config"},
		},
	},
	{
		name: "external network not the shared one", file: "top-level-allowed.json",
		policy: Policy{Strict: true, WorkDir: "/work", Network: "othernet"},
		want:   []Violation{{Where: `network "shared"`, Option: "external"}},
	},
	{
		name: "top-level secrets", file: "secrets-top-level.json", policy: strictPolicy,
		want: []Violation{
			{Where: `secret "drvbad"`, Option: "driver"},
			{Where: `secret "drvbad"`, Option: "file"},
			{Where: `secret "namegood"`, Option: "name"},
			{Where: `secret "optsbad"`, Option: "driver_opts"},
			{Where: `secret "optsbad"`, Option: "file"},
			{Where: `secret "tmplbad"`, Option: "file"},
			{Where: `secret "tmplbad"`, Option: "template_driver"},
			{Where: `service "web"`, Option: "secrets"},
			{Where: `service "web"`, Option: "secrets"},
			{Where: `service "web"`, Option: "secrets"},
		},
	},
	{
		name: "top-level configs", file: "configs-top-level.json", policy: strictPolicy,
		want: []Violation{
			{Where: `config "namegood"`, Option: "name"},
			{Where: `config "tmplbad"`, Option: "file"},
			{Where: `config "tmplbad"`, Option: "template_driver"},
			{Where: `service "web"`, Option: "configs"},
		},
	},
	{
		name: "secret and config driver options", file: "secret-config-extra.json", policy: strictPolicy, pureOnly: true,
		want: []Violation{
			{Where: `config "configdriver"`, Option: "driver"},
			{Where: `config "configopts"`, Option: "driver_opts"},
			{Where: `secret "secretdriver"`, Option: "driver"},
			{Where: `secret "secretopts"`, Option: "driver_opts"},
		},
	},

	// Build.
	{name: "build allowed", file: "build-allowed.json", policy: strictPolicy},
	{
		name: "build refused", file: "build-refused.json", policy: strictPolicy,
		want: []Violation{
			{Where: `secret "filebad"`, Option: "file"},
			{Where: `service "dockerfile"`, Option: "build.dockerfile"},
			{Where: `service "imagedh"`, Option: "image"},
			{Where: `service "imageouthaul"`, Option: "image"},
			{Where: `service "nethost"`, Option: "build.network"},
			{Where: `service "netother"`, Option: "build.network"},
			{Where: `service "oci"`, Option: "build.context"},
			{Where: `service "outside"`, Option: "build.context"},
			{Where: `service "secretfile"`, Option: "build.secrets"},
			{Where: `service "tagdh"`, Option: "build.tags"},
			{Where: `service "tagouthaul"`, Option: "build.tags"},
		},
	},
	{
		name: "build refused keys", file: "build-refused-keys.json", policy: strictPolicy,
		want: []Violation{
			{Where: `service "web"`, Option: "build.cache_from"},
			{Where: `service "web"`, Option: "build.cache_to"},
			{Where: `service "web"`, Option: "build.entitlements"},
			{Where: `service "web"`, Option: "build.isolation"},
			{Where: `service "web"`, Option: "build.privileged"},
			{Where: `service "web"`, Option: "build.ssh"},
		},
	},
	{
		name: "unknown build key", file: "unknown-build-key.json", policy: strictPolicy, pureOnly: true,
		want: []Violation{{
			Where:  `service "web"`,
			Option: "build.future_option",
			Reason: "this key is not in the server's Compose allow-list.",
		}},
	},

	// Paths.
	{
		name: "paths", file: "paths.json", policy: strictPolicy,
		want: []Violation{
			{Where: `service "web"`, Option: "build.additional_contexts.outside"},
			{Where: `service "web"`, Option: "env_file"},
		},
	},
	{
		name: "label_file outside the checkout", file: "label-file-outside.json", policy: strictPolicy, pureOnly: true,
		want: []Violation{{Where: `service "web"`, Option: "label_file"}},
	},

	// Routing labels, refused even with strict off.
	{
		name: "routing labels", file: "routing-labels.json",
		policy: Policy{RoutingLabels: true, WorkDir: "/work"},
		want: []Violation{
			{Where: `service "web"`, Option: "labels.Outhaul.App"},
			{Where: `service "web"`, Option: "labels.TRAEFIK.ENABLE"},
			{Where: `service "web"`, Option: "labels.traefik.http.routers.evil.rule"},
		},
	},
	{
		name: "routing labels allowed", file: "routing-labels.json",
		policy: Policy{WorkDir: "/work"},
	},

	// Strict off is no allow-list at all.
	{
		name: "strict off", file: "privileged.json", policy: Policy{},
	},
}

type policyCase struct {
	name     string
	file     string
	pureOnly bool
	policy   Policy
	want     []Violation
}

func TestCheckServiceAllowList(t *testing.T) {
	for _, tc := range policyCases {
		t.Run(tc.name, func(t *testing.T) {
			b, err := os.ReadFile(filepath.Join("testdata", "policy", tc.file))
			if err != nil {
				t.Fatal(err)
			}
			m, err := ParseModel(b)
			if err != nil {
				t.Fatal(err)
			}
			got := Check(m, tc.policy)
			if len(got) != len(tc.want) {
				t.Fatalf("Check() = %+v, want %+v", got, tc.want)
			}
			for i := range got {
				if got[i].Where != tc.want[i].Where || got[i].Option != tc.want[i].Option {
					t.Errorf("violation %d = %+v, want Where %q Option %q", i, got[i], tc.want[i].Where, tc.want[i].Option)
				}
				if got[i].Reason == "" {
					t.Errorf("violation %d (%+v) has an empty Reason", i, got[i])
				}
				if tc.want[i].Reason != "" && got[i].Reason != tc.want[i].Reason {
					t.Errorf("violation %d Reason = %q, want %q", i, got[i].Reason, tc.want[i].Reason)
				}
			}
		})
	}
}

var composeVersion = regexp.MustCompile(`v?([0-9]+)\.([0-9]+)`)

// composeAvailable reports whether the installed Compose is 2.35 or newer,
// the first release with `config --no-env-resolution`.
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

// TestCheckAgainstRealCompose runs every fixture that Compose can load through
// the installed `docker compose`, so the allow-list is compared with what
// Compose really prints. Reasons may name the fixture's absolute path where
// the pure test names /work, so only Where and Option are compared.
func TestCheckAgainstRealCompose(t *testing.T) {
	if !composeAvailable(t) {
		t.Skip("docker compose 2.35 or newer is not installed")
	}
	dir := filepath.Join("testdata", "policy")
	absDir, err := filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range policyCases {
		if tc.pureOnly {
			continue
		}
		t.Run(tc.name, func(t *testing.T) {
			yml := strings.TrimSuffix(tc.file, ".json") + ".yml"
			cmd := exec.Command("docker", "compose", "-p", "policy", "-f", yml,
				"config", "--format", "json", "--no-env-resolution")
			cmd.Dir = dir
			b, err := cmd.Output()
			if err != nil {
				t.Fatalf("docker compose config %s: %v", yml, err)
			}
			m, err := ParseModel(b)
			if err != nil {
				t.Fatal(err)
			}
			p := tc.policy
			p.WorkDir = absDir
			got := Check(m, p)
			if !sameWhereOption(got, tc.want) {
				t.Errorf("Check() = %+v, want Where and Option of %+v", got, tc.want)
			}
			for _, v := range got {
				if v.Reason == "" {
					t.Errorf("violation %+v has an empty Reason", v)
				}
			}
		})
	}
}

func sameWhereOption(got, want []Violation) bool {
	keys := func(vs []Violation) []string {
		out := make([]string, 0, len(vs))
		for _, v := range vs {
			out = append(out, v.Where+"\x00"+v.Option)
		}
		sort.Strings(out)
		return out
	}
	g, w := keys(got), keys(want)
	if len(g) != len(w) {
		return false
	}
	for i := range g {
		if g[i] != w[i] {
			return false
		}
	}
	return true
}

func TestReservedImage(t *testing.T) {
	cases := []struct {
		ref  string
		want bool
	}{
		{"outhaul/app:1", true},
		{"outhaul/app", true},
		{"docker.io/outhaul/app:1", true},
		{"index.docker.io/outhaul/app:1", true},
		{"registry-1.docker.io/outhaul/app:1", true},
		{"outhaul", true},
		{"example.com/outhaul/app:1", false},
		{"example/outhaul:1", false},
		{"myouthaul/app:1", false},
		{"outhaul-dev/app:1", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := reservedImage(tc.ref); got != tc.want {
			t.Errorf("reservedImage(%q) = %v, want %v", tc.ref, got, tc.want)
		}
	}
}

func TestInside(t *testing.T) {
	base := t.TempDir()
	checkout := filepath.Join(base, "checkout")
	if err := os.MkdirAll(filepath.Join(checkout, "app"), 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(checkout, link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(checkout, "escape")); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name    string
		workDir string
		path    string
		want    bool
	}{
		{"inside", checkout, filepath.Join(checkout, "app"), true},
		{"the checkout itself", checkout, checkout, true},
		{"outside", checkout, outside, false},
		{"climbing out", checkout, filepath.Join(checkout, "..", "outside"), false},
		{"sibling with the same prefix", checkout, checkout + "-other", false},
		{"symlinked checkout, resolved path", link, filepath.Join(checkout, "app"), true},
		{"symlinked checkout, path as given", link, filepath.Join(link, "app"), true},
		{"symlink out of the checkout is not followed", checkout, filepath.Join(checkout, "escape", "file"), true},
		{"missing checkout falls back to text", filepath.Join(base, "missing"), filepath.Join(base, "missing", "app"), true},
		{"no checkout", "", filepath.Join(checkout, "app"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := inside(tc.workDir, tc.path); got != tc.want {
				t.Errorf("inside(%q, %q) = %v, want %v", tc.workDir, tc.path, got, tc.want)
			}
		})
	}
}

// An env_file or label_file inside the checkout can still name a host file
// through a symlink. Compose prints the link's own path, so the text check
// passes, but it follows the link at `up`. The resolved path must stay inside
// too; a link that stays inside, or a file that does not exist yet, passes.
func TestEnvAndLabelFilesRefuseSymlinksOutOfTheCheckout(t *testing.T) {
	base := t.TempDir()
	work := filepath.Join(base, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(base, "host.env")
	if err := os.WriteFile(outside, []byte("SECRET=host\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(work, "app.env")
	if err := os.WriteFile(inside, []byte("APP=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	leak := filepath.Join(work, "leak.env")
	if err := os.Symlink(outside, leak); err != nil {
		t.Fatal(err)
	}
	linkInside := filepath.Join(work, "link.env")
	if err := os.Symlink(inside, linkInside); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(work, "missing.env")

	model := fmt.Sprintf(`{"services":{"web":{"image":"nginx","env_file":[{"path":%q},{"path":%q},{"path":%q}],"label_file":[%q,%q]}}}`,
		leak, linkInside, missing, leak, linkInside)
	m, err := ParseModel([]byte(model))
	if err != nil {
		t.Fatal(err)
	}

	got := Check(m, Policy{Strict: true, WorkDir: work})
	if len(got) != 2 {
		t.Fatalf("Check() = %+v, want one env_file and one label_file refusal", got)
	}
	options := map[string]bool{}
	for _, v := range got {
		if v.Where != `service "web"` {
			t.Errorf("violation %+v has Where %q, want service \"web\"", v, v.Where)
		}
		options[v.Option] = true
		if !strings.Contains(v.Reason, leak) {
			t.Errorf("violation %+v does not name the offending path %q", v, leak)
		}
		if strings.Contains(v.Reason, linkInside) || strings.Contains(v.Reason, missing) {
			t.Errorf("violation %+v refuses a link that stays inside or a missing file", v)
		}
	}
	if !options["env_file"] || !options["label_file"] {
		t.Errorf("refusals = %+v, want both env_file and label_file", got)
	}
}

// insideResolved follows the file itself, unlike inside, which deliberately
// looks only at the path as printed.
func TestInsideResolved(t *testing.T) {
	base := t.TempDir()
	work := filepath.Join(base, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(base, "outside.env")
	if err := os.WriteFile(outside, []byte("X=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(work, "app.env")
	if err := os.WriteFile(inside, []byte("X=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	escape := filepath.Join(work, "escape")
	if err := os.Symlink(outside, escape); err != nil {
		t.Fatal(err)
	}
	linkInside := filepath.Join(work, "link")
	if err := os.Symlink(inside, linkInside); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(work, "missing")

	cases := []struct {
		name string
		path string
		want bool
	}{
		{"a real file inside", inside, true},
		{"a link that stays inside", linkInside, true},
		{"a link out of the checkout", escape, false},
		{"a missing file keeps the text result", missing, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := insideResolved(work, tc.path); got != tc.want {
				t.Errorf("insideResolved(%q, %q) = %v, want %v", work, tc.path, got, tc.want)
			}
		})
	}
}

// checkJSON parses a hand-written model and runs Check.
func checkJSON(t *testing.T, model string, p Policy) []Violation {
	t.Helper()
	m, err := ParseModel([]byte(model))
	if err != nil {
		t.Fatalf("ParseModel: %v", err)
	}
	return Check(m, p)
}

func limitPolicy() Policy {
	return Policy{Strict: true, WorkDir: "/work",
		Max: core.Limits{MilliCPUs: 1000, Memory: 1 << 30, Pids: 512}}
}

func TestCheckRefusesAServiceAboveTheAppLimit(t *testing.T) {
	vs := checkJSON(t, `{"name":"outhaul-shop","services":{"web":{"image":"nginx","cpus":2,"mem_limit":"2147483648","pids_limit":1024}}}`, limitPolicy())
	if len(vs) != 3 {
		t.Fatalf("violations = %+v, want one for each resource", vs)
	}
	wantFigures := map[string][2]string{
		"cpus":       {"2", "1"},
		"mem_limit":  {"2147483648", "1073741824"},
		"pids_limit": {"1024", "512"},
	}
	for _, v := range vs {
		figures, ok := wantFigures[v.Option]
		if !ok {
			t.Errorf("unexpected violation %+v", v)
			continue
		}
		for _, f := range figures {
			if !strings.Contains(v.Reason, f) {
				t.Errorf("%s reason %q does not name %s", v.Option, v.Reason, f)
			}
		}
		if !strings.Contains(v.Reason, "a system admin can raise the app's limit") {
			t.Errorf("%s reason %q does not say the limit can be raised", v.Option, v.Reason)
		}
		delete(wantFigures, v.Option)
	}
	if len(wantFigures) > 0 {
		t.Errorf("missing violations for %v", wantFigures)
	}
}

func TestCheckAllowsEqualAndLowerValues(t *testing.T) {
	for _, model := range []string{
		`{"name":"outhaul-shop","services":{"web":{"image":"nginx","cpus":1,"mem_limit":"1073741824","pids_limit":512}}}`,
		`{"name":"outhaul-shop","services":{"web":{"image":"nginx","cpus":0.5,"mem_limit":"536870912","pids_limit":64}}}`,
		`{"name":"outhaul-shop","services":{"web":{"image":"nginx","deploy":{"resources":{"limits":{"cpus":1,"memory":"1073741824","pids":512}}}}}}`,
	} {
		if vs := checkJSON(t, model, limitPolicy()); len(vs) != 0 {
			t.Errorf("checkJSON(%s) = %+v, want no violations", model, vs)
		}
	}
}

func TestCheckRefusesUnlimitedPids(t *testing.T) {
	vs := checkJSON(t, `{"name":"outhaul-shop","services":{"web":{"image":"nginx","pids_limit":-1}}}`, limitPolicy())
	if len(vs) != 1 {
		t.Fatalf("violations = %+v, want one", vs)
	}
	if !strings.Contains(vs[0].Reason, "unlimited") || !strings.Contains(vs[0].Reason, "512") {
		t.Errorf("reason = %q, want it to say the count is unlimited and name the limit", vs[0].Reason)
	}
}

func TestCheckRefusesCPUQuotaAndAsksForCpus(t *testing.T) {
	vs := checkJSON(t, `{"name":"outhaul-shop","services":{"web":{"image":"nginx","cpu_quota":50000,"cpu_period":100000}}}`, limitPolicy())
	if len(vs) != 1 {
		t.Fatalf("violations = %+v, want one", vs)
	}
	if !strings.Contains(vs[0].Reason, "set cpus instead") {
		t.Errorf("reason = %q, want it to ask for cpus instead", vs[0].Reason)
	}
}

func TestCheckLeavesLimitsAloneWithoutStrictMode(t *testing.T) {
	p := Policy{WorkDir: "/work", Max: core.Limits{MilliCPUs: 1000, Memory: 1 << 30, Pids: 512}}
	vs := checkJSON(t, `{"name":"outhaul-shop","services":{"web":{"image":"nginx","cpus":2,"mem_limit":"2147483648","pids_limit":1024}}}`, p)
	if len(vs) != 0 {
		t.Errorf("violations without strict mode = %+v, want none", vs)
	}
}
