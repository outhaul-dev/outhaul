package compose

import (
	"os"
	"path/filepath"
	"testing"
)

// strictPolicy is the policy for Outhaul's strict mode. WorkDir is set even
// though this task's checks do not read paths: the path fixtures do later.
var strictPolicy = Policy{Strict: true, WorkDir: "/work"}

// policyCases names every recorded or hand-written model under
// testdata/policy. A want entry with an empty Reason only requires it to be
// non-empty; a set Reason is compared exactly.
//
// pureOnly marks JSON that cannot come from `docker compose config`: Compose
// drops service-level x-* keys and refuses a top-level key that is neither an
// extension field nor a standard section before it prints a model. The
// real-Compose test (Task 3) skips these rows.
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
			{Where: `service "web"`, Option: "secrets"},
			{Where: `service "web"`, Option: "secrets"},
		},
	},
	{
		name: "configs", file: "configs.json", policy: strictPolicy,
		want: []Violation{
			{Where: `service "web"`, Option: "configs"},
			{Where: `service "web"`, Option: "configs"},
		},
	},
	{
		name: "hooks", file: "hooks.json", policy: strictPolicy,
		want: []Violation{
			{Where: `service "web"`, Option: "post_start"},
			{Where: `service "web"`, Option: "pre_stop"},
		},
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
