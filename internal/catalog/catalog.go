// Package catalog is the built-in template gallery: a curated set of
// open-source apps (adapted from Dokploy's blueprints) that deploy as
// compose stacks with one click. Each template embeds a compose file plus a
// template.json manifest describing the domains to route, the env vars to
// set, and the variables to generate (passwords, secrets, sslip.io domains).
//
// Templates ship inside the binary (go:embed): no network dependency at
// deploy time, and every Outhaul release carries a catalog its tests have
// rendered. A remote catalog and user-supplied compose are deliberate seams.
package catalog

import (
	"embed"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
)

//go:embed templates
var templatesFS embed.FS

// Template is one catalog entry: metadata for the gallery plus everything
// needed to render a deployable stack.
type Template struct {
	ID          string   // directory name; also the default app name
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Version     string   `json:"version"` // upstream app version the compose file pins
	Tags        []string `json:"tags"`
	Links       struct {
		Website string `json:"website"`
		Docs    string `json:"docs"`
		Github  string `json:"github"`
	} `json:"links"`

	// Variables are named values referenced as ${name} by env values and
	// domain hosts. Values may use generator helpers (${password:24}, ...);
	// see expand.go for the set.
	Variables map[string]string `json:"variables"`
	Domains   []DomainSpec      `json:"domains"`
	Env       []EnvSpec         `json:"env"`

	Compose string // docker-compose.yml contents
}

// DomainSpec routes one stack service through Traefik. Host defaults to
// "${domain}" (a generated sslip.io name) and may reference variables.
type DomainSpec struct {
	Service string `json:"service"`
	Port    int    `json:"port"`
	Host    string `json:"host"`
}

// EnvSpec is one env var to set on the created app. Secret entries are
// stored like user-entered secrets (encrypted, masked in the UI).
type EnvSpec struct {
	Key    string `json:"key"`
	Value  string `json:"value"`
	Secret bool   `json:"secret"`
}

var (
	loadOnce sync.Once
	loaded   []Template
	loadErr  error
)

// All returns every template, sorted by ID. The embedded catalog is parsed
// once; a malformed template is a build defect and comes back as an error.
func All() ([]Template, error) {
	loadOnce.Do(func() { loaded, loadErr = loadAll() })
	return loaded, loadErr
}

// Get returns the template with the given id.
func Get(id string) (Template, error) {
	all, err := All()
	if err != nil {
		return Template{}, err
	}
	for _, t := range all {
		if t.ID == id {
			return t, nil
		}
	}
	return Template{}, fmt.Errorf("catalog: no template %q", id)
}

func loadAll() ([]Template, error) {
	entries, err := templatesFS.ReadDir("templates")
	if err != nil {
		return nil, fmt.Errorf("catalog: read embedded templates: %w", err)
	}
	var all []Template
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		t, err := load(e.Name())
		if err != nil {
			return nil, err
		}
		all = append(all, t)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].ID < all[j].ID })
	return all, nil
}

func load(id string) (Template, error) {
	manifest, err := templatesFS.ReadFile("templates/" + id + "/template.json")
	if err != nil {
		return Template{}, fmt.Errorf("catalog %s: %w", id, err)
	}
	compose, err := templatesFS.ReadFile("templates/" + id + "/docker-compose.yml")
	if err != nil {
		return Template{}, fmt.Errorf("catalog %s: %w", id, err)
	}
	var t Template
	if err := json.Unmarshal(manifest, &t); err != nil {
		return Template{}, fmt.Errorf("catalog %s: parse template.json: %w", id, err)
	}
	t.ID = id
	t.Compose = string(compose)
	if err := validate(t); err != nil {
		return Template{}, fmt.Errorf("catalog %s: %w", id, err)
	}
	return t, nil
}

// validate rejects manifests the renderer or the gallery could not honor.
func validate(t Template) error {
	if t.Name == "" || t.Description == "" {
		return fmt.Errorf("manifest needs a name and description")
	}
	if len(t.Domains) == 0 {
		return fmt.Errorf("manifest needs at least one domain (a template no one can reach is useless)")
	}
	for _, d := range t.Domains {
		if d.Service == "" || d.Port <= 0 {
			return fmt.Errorf("domain entries need a service and port")
		}
		if !strings.Contains(t.Compose, d.Service+":") {
			return fmt.Errorf("domain service %q not found in the compose file", d.Service)
		}
	}
	for _, e := range t.Env {
		if e.Key == "" {
			return fmt.Errorf("env entries need a key")
		}
	}
	return nil
}
