package compose

import (
	"encoding/json"
	"fmt"
	"path/filepath"
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

// Check returns every way m breaks p. The allow-list runs in strict mode; the
// routing-label rule runs whenever p.RoutingLabels is set, strict or not.
// Violations are ordered by Where, keeping the order the keys were checked.
func Check(m Model, p Policy) []Violation {
	var out []Violation
	if p.Strict {
		out = append(out, checkTopLevel(m, p)...)
		for _, s := range m.Services {
			out = append(out, checkService(m, s, p)...)
		}
	}
	if p.RoutingLabels {
		for _, s := range m.Services {
			out = append(out, checkRoutingLabels(s)...)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Where < out[j].Where })
	return out
}

// checkService walks one service's keys in sorted order against the three
// sets: allowed as written, allowed with a check, and refused by name.
func checkService(m Model, s Service, p Policy) []Violation {
	where := fmt.Sprintf("service %q", s.Name)
	var out []Violation
	add := func(option, reason string) {
		out = append(out, Violation{Where: where, Option: option, Reason: reason})
	}
	for _, key := range sortedKeys(s.Raw) {
		if check, ok := conditionalKeys[key]; ok {
			check(m, s.Raw[key], p, add)
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
	if _, builds := s.Raw["build"]; builds && reservedImage(s.Image) {
		add("image", "an image under outhaul/ is reserved for Outhaul's rollback images.")
	}
	return out
}

const (
	unknownKeyReason     = "this key is not in the server's Compose allow-list."
	unknownSectionReason = "this section is not in the server's Compose allow-list."
	unexpectedShape      = "this option could not be read."
)

// topLevelSections are the only standard top-level sections strict mode
// allows. A key whose lowercased name starts with x- is an extension field and
// is also allowed; anchors written there are resolved into services and
// checked there.
var topLevelSections = map[string]struct{}{
	"name":     {},
	"services": {},
	"volumes":  {},
	"networks": {},
	"secrets":  {},
	"configs":  {},
}

// checkTopLevel walks the model's top-level keys in sorted order. Section
// entries are checked against the spec's tables.
func checkTopLevel(m Model, p Policy) []Violation {
	var out []Violation
	for _, key := range sortedKeys(m.Raw) {
		if strings.HasPrefix(strings.ToLower(key), "x-") {
			continue
		}
		if _, ok := topLevelSections[key]; !ok {
			out = append(out, Violation{Where: "compose file", Option: key, Reason: unknownSectionReason})
			continue
		}
		switch key {
		case "volumes":
			out = append(out, checkSection(m, "volumes", "volume", func(name, key string, fields map[string]json.RawMessage, add func(string, string)) {
				checkVolumeField(m.Project, name, key, fields, add)
			})...)
		case "networks":
			out = append(out, checkSection(m, "networks", "network", func(name, key string, fields map[string]json.RawMessage, add func(string, string)) {
				checkNetworkField(m.Project, name, key, fields, p, add)
			})...)
		case "secrets", "configs":
			section := key
			out = append(out, checkSection(m, section, strings.TrimSuffix(section, "s"), func(name, key string, fields map[string]json.RawMessage, add func(string, string)) {
				checkFileField(m.Project, section, name, key, fields, add)
			})...)
		}
	}
	return out
}

// checkSection decodes one top-level section's entries and walks each entry's
// keys in sorted order.
func checkSection(m Model, section, noun string, check func(name, key string, fields map[string]json.RawMessage, add func(string, string))) []Violation {
	entries := sectionEntries(m, section)
	var out []Violation
	for _, name := range sortedKeys(entries) {
		where := fmt.Sprintf("%s %q", noun, name)
		add := func(option, reason string) {
			out = append(out, Violation{Where: where, Option: option, Reason: reason})
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(entries[name], &fields); err != nil {
			add(section, unexpectedShape)
			continue
		}
		for _, key := range sortedKeys(fields) {
			check(name, key, fields, add)
		}
	}
	return out
}

func checkVolumeField(project, name, key string, fields map[string]json.RawMessage, add func(string, string)) {
	switch key {
	case "labels":
	case "name":
		if !isExternal(fields) && !hasDefaultName(project, name, fields[key]) {
			add("name", "the volume's name must be the default one.")
		}
	case "driver":
		var driver string
		if err := json.Unmarshal(fields[key], &driver); err != nil {
			add("driver", unexpectedShape)
			return
		}
		if driver != "local" {
			add("driver", "only the local driver is allowed.")
		}
	case "driver_opts":
		add("driver_opts", "driver options can turn a named volume into a host path.")
	case "external":
		add("external", "an external volume is not managed by this stack.")
	default:
		add(key, unknownKeyReason)
	}
}

func checkNetworkField(project, name, key string, fields map[string]json.RawMessage, p Policy, add func(string, string)) {
	switch key {
	case "labels", "internal", "attachable", "enable_ipv4", "enable_ipv6":
	case "name":
		if isExternal(fields) {
			return // external networks are checked on the external key
		}
		if !hasDefaultName(project, name, fields[key]) {
			add("name", "the network's name must be the default one.")
		}
	case "driver":
		var driver string
		if err := json.Unmarshal(fields[key], &driver); err != nil {
			add("driver", unexpectedShape)
			return
		}
		if driver != "bridge" {
			add("driver", "only the bridge driver is allowed.")
		}
	case "driver_opts":
		add("driver_opts", "network driver options are not allowed.")
	case "external":
		if !isExternal(fields) {
			return
		}
		var externalName string
		if raw, ok := fields["name"]; ok {
			if err := json.Unmarshal(raw, &externalName); err != nil {
				add("external", unexpectedShape)
				return
			}
		} else {
			externalName = name
		}
		if externalName != p.Network {
			add("external", "a stack may only join the server's shared network.")
		}
	case "ipam":
		var ipam map[string]json.RawMessage
		if err := json.Unmarshal(fields[key], &ipam); err != nil {
			add("ipam", unexpectedShape)
			return
		}
		for _, k := range sortedKeys(ipam) {
			switch k {
			case "config":
				add("ipam.config", "a fixed subnet is not allowed; Docker chooses the network's addresses.")
			case "driver":
				add("ipam.driver", "an IPAM driver is not allowed.")
			default:
				add("ipam."+k, unknownKeyReason)
			}
		}
	default:
		add(key, unknownKeyReason)
	}
}

func checkFileField(project, section, name, key string, fields map[string]json.RawMessage, add func(string, string)) {
	noun := "secret"
	alt := "environment"
	if section == "configs" {
		noun, alt = "config", "content"
	}
	switch key {
	case "environment", "content", "labels":
	case "name":
		if !isExternal(fields) && !hasDefaultName(project, name, fields[key]) {
			add("name", "the "+noun+"'s name must be the default one.")
		}
	case "file":
		add("file", "a "+noun+" from a host file is not allowed. Define it with "+alt+" instead.")
	case "external":
		add("external", "an external "+noun+" is not allowed. Define it with "+alt+" instead.")
	case "driver":
		add("driver", "a "+noun+" driver is not allowed.")
	case "driver_opts":
		add("driver_opts", "driver options are not allowed.")
	case "template_driver":
		add("template_driver", "a template driver is not allowed.")
	default:
		add(key, unknownKeyReason)
	}
}

// isExternal reports whether a top-level entry sets external: true. An
// external entry's name is chosen by its Compose key, not by the default rule.
func isExternal(fields map[string]json.RawMessage) bool {
	raw, ok := fields["external"]
	if !ok {
		return false
	}
	var external bool
	return json.Unmarshal(raw, &external) == nil && external
}

// hasDefaultName reports whether a top-level entry's name is the
// <project>_<key> Compose gives it when name is unset.
func hasDefaultName(project, key string, raw json.RawMessage) bool {
	var name string
	if err := json.Unmarshal(raw, &name); err != nil {
		return false
	}
	return name == project+"_"+key
}

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
type serviceCheck func(m Model, raw json.RawMessage, p Policy, add func(option, reason string))

// conditionalKeys are allowed only after a check. labels is allowed as
// written; the routing-label rule reads the merged map separately.
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
	"build":         checkBuild,
	"labels":        allowServiceKey,
	"label_file":    checkLabelFiles,
	"env_file":      checkEnvFiles,
}

func allowServiceKey(Model, json.RawMessage, Policy, func(string, string)) {}

func checkVolumes(_ Model, raw json.RawMessage, _ Policy, add func(string, string)) {
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

func checkVolumesFrom(_ Model, raw json.RawMessage, _ Policy, add func(string, string)) {
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

func checkNetworkMode(_ Model, raw json.RawMessage, _ Policy, add func(string, string)) {
	var mode string
	if err := json.Unmarshal(raw, &mode); err != nil {
		add("network_mode", unexpectedShape)
		return
	}
	if mode != "none" && !strings.HasPrefix(mode, "service:") {
		add("network_mode", "only none or a service of this stack is allowed. Give the service a domain; services reach each other on the stack's network.")
	}
}

func checkPID(_ Model, raw json.RawMessage, _ Policy, add func(string, string)) {
	var mode string
	if err := json.Unmarshal(raw, &mode); err != nil {
		add("pid", unexpectedShape)
		return
	}
	if !strings.HasPrefix(mode, "service:") {
		add("pid", "only a service of this stack is allowed.")
	}
}

func checkIPC(_ Model, raw json.RawMessage, _ Policy, add func(string, string)) {
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

func checkCgroup(_ Model, raw json.RawMessage, _ Policy, add func(string, string)) {
	var mode string
	if err := json.Unmarshal(raw, &mode); err != nil {
		add("cgroup", unexpectedShape)
		return
	}
	if mode != "private" {
		add("cgroup", "only private is allowed.")
	}
}

func checkSecurityOpt(_ Model, raw json.RawMessage, _ Policy, add func(string, string)) {
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

func checkNetworks(_ Model, raw json.RawMessage, _ Policy, add func(string, string)) {
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

// checkEnvFiles keeps every env_file path inside the checkout.
func checkEnvFiles(_ Model, raw json.RawMessage, p Policy, add func(string, string)) {
	var files []struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(raw, &files); err != nil {
		add("env_file", unexpectedShape)
		return
	}
	for _, f := range files {
		if !inside(p.WorkDir, f.Path) {
			add("env_file", "env_file must stay inside the checkout.")
		}
	}
}

// checkLabelFiles keeps every label_file path inside the checkout. Compose
// has already merged the labels it adds into Service.Labels.
func checkLabelFiles(_ Model, raw json.RawMessage, p Policy, add func(string, string)) {
	var files []string
	if err := json.Unmarshal(raw, &files); err != nil {
		add("label_file", unexpectedShape)
		return
	}
	for _, f := range files {
		if !inside(p.WorkDir, f) {
			add("label_file", "label_file must stay inside the checkout.")
		}
	}
}

// reservedImage reports whether ref names a repository under outhaul/, where
// Outhaul keeps every app's rollback images. Docker resolves a reference with
// a Docker Hub registry prefix to the same repository as the short name, so
// the prefix is stripped first. A reference with no slash is compared as its
// only component, which refuses a bare "outhaul" conservatively.
func reservedImage(ref string) bool {
	for _, prefix := range []string{"registry-1.docker.io/", "index.docker.io/", "docker.io/"} {
		if rest, ok := strings.CutPrefix(ref, prefix); ok {
			ref = rest
			break
		}
	}
	repository, _, _ := strings.Cut(ref, "/")
	return repository == "outhaul"
}

// checkBuild walks a service's build section against the spec's table.
func checkBuild(m Model, raw json.RawMessage, p Policy, add func(string, string)) {
	var build map[string]json.RawMessage
	if err := json.Unmarshal(raw, &build); err != nil {
		add("build", unexpectedShape)
		return
	}
	for _, key := range sortedKeys(build) {
		switch key {
		case "context":
			checkBuildContext(p, build[key], "build.context", add)
		case "additional_contexts":
			var contexts map[string]json.RawMessage
			if err := json.Unmarshal(build[key], &contexts); err != nil {
				add("build.additional_contexts", unexpectedShape)
				continue
			}
			for _, name := range sortedKeys(contexts) {
				checkBuildContext(p, contexts[name], "build.additional_contexts."+name, add)
			}
		case "dockerfile":
			checkDockerfile(build, p, add)
		case "network":
			var network string
			if err := json.Unmarshal(build[key], &network); err != nil {
				add("build.network", unexpectedShape)
				continue
			}
			if network != "default" && network != "none" {
				add("build.network", "the build network must be default or none.")
			}
		case "secrets":
			checkBuildSecrets(m, build[key], add)
		case "args", "dockerfile_inline", "extra_hosts", "labels", "no_cache",
			"platforms", "pull", "shm_size", "target", "ulimits":
		case "tags":
			var tags []string
			if err := json.Unmarshal(build[key], &tags); err != nil {
				add("build.tags", unexpectedShape)
				continue
			}
			for _, tag := range tags {
				if reservedImage(tag) {
					add("build.tags", "a tag under outhaul/ is reserved for Outhaul's rollback images.")
				}
			}
		case "privileged":
			add("build.privileged", "a privileged build is not allowed.")
		case "entitlements":
			add("build.entitlements", "build entitlements are not allowed.")
		case "ssh":
			add("build.ssh", "SSH agent forwarding in a build is not allowed.")
		case "cache_from":
			add("build.cache_from", "pulling build cache from another image is not allowed.")
		case "cache_to":
			add("build.cache_to", "writing build cache to another image is not allowed.")
		case "isolation":
			add("build.isolation", "choosing an isolation mode is not allowed.")
		default:
			add("build."+key, unknownKeyReason)
		}
	}
}

// checkBuildContext allows a path inside the checkout, a Git or HTTP URL,
// docker-image://, and a named target or service. A local path is absolute in
// the model; oci-layout is refused.
func checkBuildContext(p Policy, raw json.RawMessage, option string, add func(string, string)) {
	var context string
	if err := json.Unmarshal(raw, &context); err != nil {
		add(option, unexpectedShape)
		return
	}
	switch {
	case strings.HasPrefix(context, "oci-layout://"):
		add(option, "an oci-layout build context is not allowed.")
	case filepath.IsAbs(context) && !inside(p.WorkDir, context):
		add(option, "the build context must stay inside the checkout.")
	}
}

// checkDockerfile keeps the Dockerfile inside the checkout. It is relative to
// the build context, so a local context is joined before the check.
func checkDockerfile(build map[string]json.RawMessage, p Policy, add func(string, string)) {
	var dockerfile string
	if err := json.Unmarshal(build["dockerfile"], &dockerfile); err != nil {
		add("build.dockerfile", unexpectedShape)
		return
	}
	if filepath.IsAbs(dockerfile) {
		if !inside(p.WorkDir, dockerfile) {
			add("build.dockerfile", "the Dockerfile must stay inside the checkout.")
		}
		return
	}
	var context string
	if raw, ok := build["context"]; ok {
		_ = json.Unmarshal(raw, &context)
	}
	if filepath.IsAbs(context) {
		if !inside(p.WorkDir, context) {
			return // the context is already refused
		}
		if !inside(p.WorkDir, filepath.Join(context, dockerfile)) {
			add("build.dockerfile", "the Dockerfile must stay inside the checkout.")
		}
		return
	}
	clean := filepath.Clean(dockerfile)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		add("build.dockerfile", "the Dockerfile must stay inside the checkout.")
	}
}

// checkBuildSecrets allows only secrets defined with environment. A file
// secret is a bind mount of that host path at build time.
func checkBuildSecrets(m Model, raw json.RawMessage, add func(string, string)) {
	var refs []struct {
		Source string `json:"source"`
	}
	if err := json.Unmarshal(raw, &refs); err != nil {
		add("build.secrets", unexpectedShape)
		return
	}
	defs := sectionEntries(m, "secrets")
	for _, ref := range refs {
		definition := defs[ref.Source]
		if len(definition) == 0 {
			continue
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(definition, &fields); err != nil {
			add("build.secrets", unexpectedShape)
			continue
		}
		if _, ok := fields["environment"]; !ok {
			add("build.secrets", "a build secret must be defined with environment.")
		}
	}
}

// checkRoutingLabels refuses traefik.* and outhaul.* labels in any case. The
// merged map includes what a label_file adds.
func checkRoutingLabels(s Service) []Violation {
	if len(s.Labels) == 0 {
		return nil
	}
	where := fmt.Sprintf("service %q", s.Name)
	keys := make([]string, 0, len(s.Labels))
	for k := range s.Labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []Violation
	for _, k := range keys {
		switch {
		case strings.HasPrefix(strings.ToLower(k), "traefik."):
			out = append(out, Violation{Where: where, Option: "labels." + k, Reason: "a traefik.* label would add a route Outhaul did not write. Outhaul writes the routing labels."})
		case strings.HasPrefix(strings.ToLower(k), "outhaul."):
			out = append(out, Violation{Where: where, Option: "labels." + k, Reason: "an outhaul.* label is reserved for Outhaul."})
		}
	}
	return out
}

// checkRefs refuses a service's secrets or configs that come from a host file
// or are external: both are the file the service would otherwise get.
func checkRefs(section, noun string) serviceCheck {
	return func(m Model, raw json.RawMessage, _ Policy, add func(string, string)) {
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
	return func(_ Model, raw json.RawMessage, _ Policy, add func(string, string)) {
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

func checkDeploy(_ Model, raw json.RawMessage, _ Policy, add func(string, string)) {
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

func checkOOMScoreAdj(_ Model, raw json.RawMessage, _ Policy, add func(string, string)) {
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

// inside reports whether p is inside workDir. Both are compared as text after
// filepath.Clean; workDir is also tried as EvalSymlinks resolves it, because
// Compose may print the resolved path of a checkout reached through a symlink.
// Symlinks inside the checkout are not followed.
func inside(workDir, p string) bool {
	if workDir == "" {
		return false
	}
	if within(filepath.Clean(workDir), filepath.Clean(p)) {
		return true
	}
	resolved, err := filepath.EvalSymlinks(workDir)
	if err != nil {
		return false
	}
	return within(filepath.Clean(resolved), filepath.Clean(p))
}

func within(dir, p string) bool {
	rel, err := filepath.Rel(dir, p)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}
