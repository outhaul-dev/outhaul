package compose

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/outhaul-dev/outhaul/internal/core"
)

// Policy is the server's container policy for a Compose stack. Strict mode
// works from an allow-list: a service key that is not named below is refused.
type Policy struct {
	Strict        bool        // the allow-list below
	RoutingLabels bool        // refuse traefik.* and outhaul.* labels
	WorkDir       string      // local paths must stay inside it
	Network       string      // the one external network a stack may join
	Max           core.Limits // R7's maximum; a zero field is not compared
}

// Violation is one reason a Compose file is refused. Where names the object
// (`service "web"`), Option the key path, and Reason says what to write
// instead. No field quotes a value from the model.
type Violation struct {
	Where  string
	Option string
	Reason string
}

// Check returns every way m breaks p. The allow-list runs only in strict mode.
func Check(m Model, p Policy) []Violation {
	if !p.Strict {
		return nil
	}
	var out []Violation
	for _, s := range m.Services {
		out = append(out, checkService(m, s)...)
	}
	return out
}

// checkService walks one service's keys in sorted order against the three
// sets: allowed as written, allowed with a check, and refused by name.
func checkService(m Model, s Service) []Violation {
	where := fmt.Sprintf("service %q", s.Name)
	var out []Violation
	add := func(option, reason string) {
		out = append(out, Violation{Where: where, Option: option, Reason: reason})
	}
	for _, key := range sortedKeys(s.Raw) {
		if check, ok := conditionalKeys[key]; ok {
			check(m, s.Raw[key], add)
			continue
		}
		if reason, ok := refusedKeys[key]; ok {
			add(key, reason)
			continue
		}
		if _, ok := allowedKeys[key]; ok {
			continue
		}
		add(key, unknownKeyReason)
	}
	return out
}

const (
	unknownKeyReason = "this key is not in the server's Compose allow-list."
	unexpectedShape  = "this option could not be read."
)

// allowedKeys are the service keys the spec allows as written.
var allowedKeys = map[string]struct{}{
	"attach":            {},
	"blkio_config":      {},
	"cap_drop":          {},
	"command":           {},
	"cpu_period":        {},
	"cpu_quota":         {},
	"cpu_shares":        {},
	"cpus":              {},
	"cpuset":            {},
	"depends_on":        {},
	"develop":           {},
	"dns":               {},
	"dns_opt":           {},
	"dns_search":        {},
	"domainname":        {},
	"entrypoint":        {},
	"environment":       {},
	"expose":            {},
	"extra_hosts":       {},
	"group_add":         {},
	"healthcheck":       {},
	"hostname":          {},
	"image":             {},
	"init":              {},
	"links":             {},
	"logging":           {},
	"mem_limit":         {},
	"mem_reservation":   {},
	"mem_swappiness":    {},
	"memswap_limit":     {},
	"pids_limit":        {},
	"platform":          {},
	"profiles":          {},
	"pull_policy":       {},
	"read_only":         {},
	"restart":           {},
	"scale":             {},
	"shm_size":          {},
	"stdin_open":        {},
	"stop_grace_period": {},
	"stop_signal":       {},
	"storage_opt":       {},
	"sysctls":           {},
	"tmpfs":             {},
	"tty":               {},
	"ulimits":           {},
	"user":              {},
	"working_dir":       {},
}

// refusedKeys are the service keys refused by name, with why.
var refusedKeys = map[string]string{
	"privileged":          "a privileged container is not allowed.",
	"cap_add":             "capabilities may not be added; use cap_drop instead.",
	"devices":             "host devices are not available to services.",
	"cgroup_parent":       "a parent cgroup is not allowed.",
	"ports":               "publishing a host port is not allowed. Give the service a domain; services reach each other on the stack's network.",
	"uts":                 "sharing the host's UTS namespace is not allowed.",
	"userns_mode":         "a user namespace mode is not allowed.",
	"device_cgroup_rules": "device cgroup rules are not allowed.",
	"gpus":                "GPUs are not available to services on this server.",
	"runtime":             "choosing a container runtime is not allowed.",
	"use_api_socket":      "the Docker API socket is not available to services.",
	"provider":            "a Compose provider is not allowed.",
	"pre_start":           "pre-start hooks are not allowed.",
	"container_name":      "container names are managed by Outhaul; leave it unset.",
	"oom_kill_disable":    "disabling the OOM killer is not allowed.",
	"mac_address":         "a fixed MAC address is not allowed.",
	"external_links":      "links to containers outside this stack are not allowed.",
	"annotations":         "annotations are not allowed.",
	"cpu_rt_runtime":      "real-time CPU runtime is not allowed.",
	"cpu_rt_period":       "real-time CPU period is not allowed.",
	"cpu_count":           "a CPU count limit is not allowed; use cpus instead.",
	"cpu_percent":         "a CPU percentage limit is not allowed; use cpus instead.",
	"credential_spec":     "a credential spec is not allowed.",
	"isolation":           "choosing an isolation mode is not allowed.",
	"models":              "AI models are not available to services on this server.",
}

// serviceCheck reads one conditional key and reports what it refuses.
type serviceCheck func(m Model, raw json.RawMessage, add func(option, reason string))

// conditionalKeys are allowed only after a check. build, labels, label_file
// and env_file are checked with paths and labels in a later task; for now
// they pass.
var conditionalKeys = map[string]serviceCheck{
	"volumes":       checkVolumes,
	"volumes_from":  checkVolumesFrom,
	"network_mode":  checkNetworkMode,
	"pid":           checkPID,
	"ipc":           checkIPC,
	"cgroup":        checkCgroup,
	"security_opt":  checkSecurityOpt,
	"networks":      checkNetworks,
	"secrets":       checkRefs("secrets", "secret"),
	"configs":       checkRefs("configs", "config"),
	"post_start":    checkHooks("post_start"),
	"pre_stop":      checkHooks("pre_stop"),
	"deploy":        checkDeploy,
	"oom_score_adj": checkOOMScoreAdj,
	"build":         allowServiceKey,
	"labels":        allowServiceKey,
	"label_file":    allowServiceKey,
	"env_file":      allowServiceKey,
}

func allowServiceKey(Model, json.RawMessage, func(string, string)) {}

func checkVolumes(_ Model, raw json.RawMessage, add func(string, string)) {
	var vols []struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &vols); err != nil {
		add("volumes", unexpectedShape)
		return
	}
	for _, v := range vols {
		switch v.Type {
		case "volume", "tmpfs":
		case "bind":
			add("volumes", "a bind mount is a host path. Use a named volume.")
		case "image":
			add("volumes", "an image-backed volume is not allowed. Use a named volume.")
		default:
			add("volumes", "only named volumes and tmpfs are allowed.")
		}
	}
}

func checkVolumesFrom(_ Model, raw json.RawMessage, add func(string, string)) {
	var refs []string
	if err := json.Unmarshal(raw, &refs); err != nil {
		add("volumes_from", unexpectedShape)
		return
	}
	for _, ref := range refs {
		if strings.HasPrefix(ref, "container:") {
			add("volumes_from", "sharing another container's volumes is not allowed. Use a service of this stack instead.")
		}
	}
}

func checkNetworkMode(_ Model, raw json.RawMessage, add func(string, string)) {
	var mode string
	if err := json.Unmarshal(raw, &mode); err != nil {
		add("network_mode", unexpectedShape)
		return
	}
	if mode != "none" && !strings.HasPrefix(mode, "service:") {
		add("network_mode", "only none or a service of this stack is allowed. Give the service a domain; services reach each other on the stack's network.")
	}
}

func checkPID(_ Model, raw json.RawMessage, add func(string, string)) {
	var mode string
	if err := json.Unmarshal(raw, &mode); err != nil {
		add("pid", unexpectedShape)
		return
	}
	if !strings.HasPrefix(mode, "service:") {
		add("pid", "only a service of this stack is allowed.")
	}
}

func checkIPC(_ Model, raw json.RawMessage, add func(string, string)) {
	var mode string
	if err := json.Unmarshal(raw, &mode); err != nil {
		add("ipc", unexpectedShape)
		return
	}
	switch mode {
	case "private", "shareable", "none":
		return
	}
	if !strings.HasPrefix(mode, "service:") {
		add("ipc", "only private, shareable, none or a service of this stack is allowed.")
	}
}

func checkCgroup(_ Model, raw json.RawMessage, add func(string, string)) {
	var mode string
	if err := json.Unmarshal(raw, &mode); err != nil {
		add("cgroup", unexpectedShape)
		return
	}
	if mode != "private" {
		add("cgroup", "only private is allowed.")
	}
}

func checkSecurityOpt(_ Model, raw json.RawMessage, add func(string, string)) {
	var opts []string
	if err := json.Unmarshal(raw, &opts); err != nil {
		add("security_opt", unexpectedShape)
		return
	}
	for _, opt := range opts {
		if opt == "no-new-privileges" || opt == "no-new-privileges:true" {
			continue
		}
		add("security_opt", "only no-new-privileges is allowed.")
	}
}

func checkNetworks(_ Model, raw json.RawMessage, add func(string, string)) {
	var nets map[string]json.RawMessage
	if err := json.Unmarshal(raw, &nets); err != nil {
		add("networks", unexpectedShape)
		return
	}
	for _, name := range sortedKeys(nets) {
		attrs := nets[name]
		if len(attrs) == 0 || string(attrs) == "null" {
			continue
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(attrs, &fields); err != nil {
			add("networks", unexpectedShape)
			continue
		}
		for _, key := range sortedKeys(fields) {
			if key == "aliases" {
				continue
			}
			option := "networks." + name + "." + key
			switch key {
			case "ipv4_address", "ipv6_address", "mac_address":
				add(option, "a fixed address is not allowed; use a service name on the stack's network.")
			default:
				add(option, "only aliases are allowed on a network.")
			}
		}
	}
}

// checkRefs refuses a service's secrets or configs that come from a host file
// or are external: both are the file the service would otherwise get.
func checkRefs(section, noun string) serviceCheck {
	return func(m Model, raw json.RawMessage, add func(string, string)) {
		var refs []struct {
			Source string `json:"source"`
		}
		if err := json.Unmarshal(raw, &refs); err != nil {
			add(section, unexpectedShape)
			return
		}
		defs := sectionEntries(m, section)
		for _, ref := range refs {
			def := defs[ref.Source]
			if len(def) == 0 {
				continue
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(def, &fields); err != nil {
				add(section, unexpectedShape)
				continue
			}
			if _, ok := fields["file"]; ok {
				add(section, fmt.Sprintf("a %s from a host file is not allowed. Define it with environment or content instead.", noun))
			} else if _, ok := fields["external"]; ok {
				add(section, fmt.Sprintf("an external %s is not allowed. Define it with environment or content instead.", noun))
			}
		}
	}
}

func sectionEntries(m Model, section string) map[string]json.RawMessage {
	raw, ok := m.Raw[section]
	if !ok {
		return nil
	}
	var defs map[string]json.RawMessage
	if err := json.Unmarshal(raw, &defs); err != nil {
		return nil
	}
	return defs
}

// checkHooks refuses a privileged post_start or pre_stop hook. Compose fills
// the rest of a hook with the service's own image and networking, so only
// privileged is a choice the file made.
func checkHooks(key string) serviceCheck {
	return func(_ Model, raw json.RawMessage, add func(string, string)) {
		var hooks []map[string]json.RawMessage
		if err := json.Unmarshal(raw, &hooks); err != nil {
			add(key, unexpectedShape)
			return
		}
		for _, hook := range hooks {
			if _, ok := hook["privileged"]; ok {
				add(key, "a hook may not run privileged.")
			}
		}
	}
}

func checkDeploy(_ Model, raw json.RawMessage, add func(string, string)) {
	var deploy struct {
		Resources struct {
			Reservations map[string]json.RawMessage `json:"reservations"`
		} `json:"resources"`
	}
	if err := json.Unmarshal(raw, &deploy); err != nil {
		add("deploy", unexpectedShape)
		return
	}
	if _, ok := deploy.Resources.Reservations["devices"]; ok {
		add("deploy.resources.reservations.devices", "device reservations are not allowed.")
	}
	if _, ok := deploy.Resources.Reservations["generic_resources"]; ok {
		add("deploy.resources.reservations.generic_resources", "generic resource reservations are not allowed.")
	}
}

func checkOOMScoreAdj(_ Model, raw json.RawMessage, add func(string, string)) {
	var n int64
	if err := json.Unmarshal(raw, &n); err != nil {
		add("oom_score_adj", unexpectedShape)
		return
	}
	if n < 0 {
		add("oom_score_adj", "a negative OOM score adjustment is not allowed.")
	}
}

func sortedKeys(m map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
