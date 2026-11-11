package compose

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
)

// Model is one stack's normalised Compose configuration, as
// `docker compose config --format json` prints it: the project's merged
// files, with every variable already resolved. It is held in memory only and
// must never be logged.
type Model struct {
	Project  string
	Services []Service // sorted by Name
	Raw      map[string]json.RawMessage
}

// Service is one service of a Model.
type Service struct {
	Name                string
	Image               string // its `image`; "<project>-<name>" when it only builds
	Restart             string // its own `restart`; "" when it has none
	DeployRestartPolicy bool   // it sets deploy.restart_policy
	Labels              map[string]string
	Raw                 map[string]json.RawMessage // every service key, as printed
}

// HasRestart reports whether the service declares a restart policy of its own,
// either as `restart` or under `deploy`.
func (s Service) HasRestart() bool {
	return s.Restart != "" || s.DeployRestartPolicy
}

// ParseModel parses one `docker compose config --format json` document. It is
// pure and accepts keys it does not know, keeping them in Model.Raw and
// Service.Raw.
func ParseModel(b []byte) (Model, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return Model{}, fmt.Errorf("parsing compose config: %w", err)
	}
	m := Model{Raw: raw}
	if v, ok := raw["name"]; ok {
		if err := json.Unmarshal(v, &m.Project); err != nil {
			return Model{}, fmt.Errorf("parsing compose project name: %w", err)
		}
	}

	var services map[string]json.RawMessage
	if v, ok := raw["services"]; ok {
		if err := json.Unmarshal(v, &services); err != nil {
			return Model{}, fmt.Errorf("parsing compose services: %w", err)
		}
	}
	m.Services = make([]Service, 0, len(services))
	for name, value := range services {
		s, err := parseService(m.Project, name, value)
		if err != nil {
			return Model{}, err
		}
		m.Services = append(m.Services, s)
	}
	sort.Slice(m.Services, func(i, j int) bool { return m.Services[i].Name < m.Services[j].Name })
	return m, nil
}

// parseService decodes the fields Outhaul reasons about and keeps every other
// key in Raw.
func parseService(project, name string, b []byte) (Service, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(b, &fields); err != nil {
		return Service{}, fmt.Errorf("parsing compose service %q: %w", name, err)
	}
	s := Service{Name: name, Raw: fields}
	if v, ok := fields["image"]; ok {
		if err := json.Unmarshal(v, &s.Image); err != nil {
			return Service{}, fmt.Errorf("parsing compose service %q image: %w", name, err)
		}
	}
	if v, ok := fields["restart"]; ok {
		if err := json.Unmarshal(v, &s.Restart); err != nil {
			return Service{}, fmt.Errorf("parsing compose service %q restart: %w", name, err)
		}
	}
	if v, ok := fields["labels"]; ok {
		if err := json.Unmarshal(v, &s.Labels); err != nil {
			return Service{}, fmt.Errorf("parsing compose service %q labels: %w", name, err)
		}
	}
	if v, ok := fields["deploy"]; ok {
		var deploy map[string]json.RawMessage
		if err := json.Unmarshal(v, &deploy); err != nil {
			return Service{}, fmt.Errorf("parsing compose service %q deploy: %w", name, err)
		}
		_, s.DeployRestartPolicy = deploy["restart_policy"]
	}
	if s.Image == "" {
		if _, ok := fields["build"]; ok {
			s.Image = project + "-" + name
		}
	}
	return s, nil
}

// ReadModel asks the runner for the stack's normalised config and parses it.
// Compose's warnings reach warn (the deploy log); stdout stays out of it.
func ReadModel(ctx context.Context, r Runner, dir string, files []string, project string, warn io.Writer) (Model, error) {
	b, err := r.Config(ctx, dir, files, project, warn)
	if err != nil {
		return Model{}, err
	}
	return ParseModel(b)
}
