package docker

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
)

// real is the SDK-backed Client driving the local Docker daemon.
type real struct {
	cli *client.Client
}

// New returns a Client connected to the Docker daemon. host may be empty to use
// the SDK's environment defaults (DOCKER_HOST or the local socket).
func New(host string) (Client, error) {
	opts := []client.Opt{client.FromEnv, client.WithAPIVersionNegotiation()}
	if host != "" {
		opts = append(opts, client.WithHost(host))
	}
	cli, err := client.NewClientWithOpts(opts...)
	if err != nil {
		return nil, fmt.Errorf("docker client: %w", err)
	}
	return &real{cli: cli}, nil
}

func (r *real) Ping(ctx context.Context) error {
	_, err := r.cli.Ping(ctx)
	return err
}

func (r *real) PullImage(ctx context.Context, ref string, out io.Writer) error {
	rc, err := r.cli.ImagePull(ctx, ref, image.PullOptions{})
	if err != nil {
		return err
	}
	defer rc.Close()
	if out == nil {
		out = io.Discard
	}
	// The stream must be drained for the pull to complete.
	_, err = io.Copy(out, rc)
	return err
}

func (r *real) EnsureNetwork(ctx context.Context, name string) error {
	nets, err := r.cli.NetworkList(ctx, network.ListOptions{
		Filters: filters.NewArgs(filters.Arg("name", name)),
	})
	if err != nil {
		return err
	}
	for _, n := range nets {
		if n.Name == name {
			return nil // already exists
		}
	}
	_, err = r.cli.NetworkCreate(ctx, name, network.CreateOptions{Driver: "bridge"})
	return err
}

func (r *real) FindContainer(ctx context.Context, name string) (*Container, error) {
	list, err := r.cli.ContainerList(ctx, container.ListOptions{
		All:     true,
		Filters: filters.NewArgs(filters.Arg("name", "^/"+name+"$")),
	})
	if err != nil {
		return nil, err
	}
	for _, c := range list {
		for _, n := range c.Names {
			if n == "/"+name {
				return &Container{
					ID:     c.ID,
					Name:   name,
					Image:  c.Image,
					State:  c.State,
					Labels: c.Labels,
				}, nil
			}
		}
	}
	return nil, nil
}

func (r *real) ListContainers(ctx context.Context, match map[string]string) ([]Container, error) {
	args := filters.NewArgs()
	for k, v := range match {
		args.Add("label", k+"="+v)
	}
	list, err := r.cli.ContainerList(ctx, container.ListOptions{All: true, Filters: args})
	if err != nil {
		return nil, err
	}
	out := make([]Container, 0, len(list))
	for _, c := range list {
		name := ""
		if len(c.Names) > 0 {
			name = strings.TrimPrefix(c.Names[0], "/")
		}
		out = append(out, Container{ID: c.ID, Name: name, Image: c.Image, State: c.State, Labels: c.Labels})
	}
	return out, nil
}

func (r *real) ContainerIP(ctx context.Context, id, network string) (string, error) {
	info, err := r.cli.ContainerInspect(ctx, id)
	if err != nil {
		return "", err
	}
	if info.NetworkSettings == nil {
		return "", nil
	}
	if ep, ok := info.NetworkSettings.Networks[network]; ok && ep != nil {
		return ep.IPAddress, nil
	}
	return "", nil
}

func (r *real) CreateContainer(ctx context.Context, spec ContainerSpec) (string, error) {
	exposed, bindings, err := portConfig(spec.Ports)
	if err != nil {
		return "", err
	}

	cfg := &container.Config{
		Image:        spec.Image,
		Labels:       spec.Labels,
		Env:          spec.Env,
		Cmd:          spec.Cmd,
		ExposedPorts: exposed,
	}

	host := &container.HostConfig{
		PortBindings: bindings,
		Binds:        bindMounts(spec.Mounts),
	}
	if spec.RestartPolicy != "" {
		host.RestartPolicy = container.RestartPolicy{
			Name: container.RestartPolicyMode(spec.RestartPolicy),
		}
	}

	var netCfg *network.NetworkingConfig
	if len(spec.Networks) > 0 {
		endpoints := make(map[string]*network.EndpointSettings, len(spec.Networks))
		for _, n := range spec.Networks {
			endpoints[n] = &network.EndpointSettings{}
		}
		netCfg = &network.NetworkingConfig{EndpointsConfig: endpoints}
	}

	resp, err := r.cli.ContainerCreate(ctx, cfg, host, netCfg, nil, spec.Name)
	if err != nil {
		return "", err
	}
	return resp.ID, nil
}

func (r *real) StartContainer(ctx context.Context, id string) error {
	return r.cli.ContainerStart(ctx, id, container.StartOptions{})
}

func (r *real) StopContainer(ctx context.Context, id string, timeout time.Duration) error {
	secs := int(timeout.Seconds())
	return r.cli.ContainerStop(ctx, id, container.StopOptions{Timeout: &secs})
}

func (r *real) RemoveContainer(ctx context.Context, id string, force bool) error {
	return r.cli.ContainerRemove(ctx, id, container.RemoveOptions{Force: force})
}

func (r *real) Close() error { return r.cli.Close() }

// portConfig builds the exposed-port set and host port bindings.
func portConfig(ports []PortMapping) (nat.PortSet, nat.PortMap, error) {
	if len(ports) == 0 {
		return nil, nil, nil
	}
	exposed := nat.PortSet{}
	bindings := nat.PortMap{}
	for _, p := range ports {
		proto := p.Proto
		if proto == "" {
			proto = "tcp"
		}
		port, err := nat.NewPort(proto, p.ContainerPort)
		if err != nil {
			return nil, nil, err
		}
		exposed[port] = struct{}{}
		bindings[port] = []nat.PortBinding{{HostIP: "0.0.0.0", HostPort: p.HostPort}}
	}
	return exposed, bindings, nil
}

func bindMounts(mounts []Mount) []string {
	if len(mounts) == 0 {
		return nil
	}
	binds := make([]string, 0, len(mounts))
	for _, m := range mounts {
		b := m.Source + ":" + m.Target
		if m.ReadOnly {
			b += ":ro"
		}
		binds = append(binds, b)
	}
	return binds
}
