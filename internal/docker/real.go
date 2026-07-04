package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types/build"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
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

func (r *real) ContainerLogs(ctx context.Context, id string, tail int) (io.ReadCloser, error) {
	info, err := r.cli.ContainerInspect(ctx, id)
	if err != nil {
		return nil, err
	}
	rc, err := r.cli.ContainerLogs(ctx, id, container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Follow:     true,
		Tail:       strconv.Itoa(tail),
	})
	if err != nil {
		return nil, err
	}
	// TTY containers produce a raw stream; everything else is multiplexed with
	// 8-byte frame headers that must be stripped.
	if info.Config != nil && info.Config.Tty {
		return rc, nil
	}
	pr, pw := io.Pipe()
	go func() {
		_, err := stdcopy.StdCopy(pw, pw, rc)
		pw.CloseWithError(err)
	}()
	return &demuxedLogs{PipeReader: pr, src: rc}, nil
}

// demuxedLogs closes the underlying Docker stream alongside the pipe, which
// unblocks the demux goroutine when the consumer walks away.
type demuxedLogs struct {
	*io.PipeReader
	src io.Closer
}

func (d *demuxedLogs) Close() error {
	d.src.Close()
	return d.PipeReader.Close()
}

func (r *real) ContainerStats(ctx context.Context, id string) (Stats, error) {
	resp, err := r.cli.ContainerStats(ctx, id, false)
	if err != nil {
		return Stats{}, err
	}
	defer resp.Body.Close()
	var raw container.StatsResponse
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return Stats{}, fmt.Errorf("decode stats: %w", err)
	}
	s := statsFromAPI(raw)
	if info, err := r.cli.ContainerInspect(ctx, id); err == nil && info.State != nil {
		if t, err := time.Parse(time.RFC3339Nano, info.State.StartedAt); err == nil {
			s.StartedAt = t
		}
	}
	return s, nil
}

// statsFromAPI reduces Docker's raw stats sample the way the docker CLI does.
func statsFromAPI(raw container.StatsResponse) Stats {
	var s Stats
	cpuDelta := float64(raw.CPUStats.CPUUsage.TotalUsage) - float64(raw.PreCPUStats.CPUUsage.TotalUsage)
	sysDelta := float64(raw.CPUStats.SystemUsage) - float64(raw.PreCPUStats.SystemUsage)
	online := float64(raw.CPUStats.OnlineCPUs)
	if online == 0 {
		online = float64(len(raw.CPUStats.CPUUsage.PercpuUsage))
	}
	if cpuDelta > 0 && sysDelta > 0 {
		s.CPUPercent = cpuDelta / sysDelta * online * 100
	}
	// Usage includes reclaimable page cache; subtract it (cgroup v1 names the
	// counter total_inactive_file, v2 names it inactive_file).
	mem := raw.MemoryStats.Usage
	if v, ok := raw.MemoryStats.Stats["total_inactive_file"]; ok && v < mem {
		mem -= v
	} else if v, ok := raw.MemoryStats.Stats["inactive_file"]; ok && v < mem {
		mem -= v
	}
	s.MemUsage = mem
	s.MemLimit = raw.MemoryStats.Limit
	for _, n := range raw.Networks {
		s.NetRx += n.RxBytes
		s.NetTx += n.TxBytes
	}
	return s
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
		Mounts:       volumeMounts(spec.Mounts),
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

func (r *real) ListImages(ctx context.Context, refPattern string) ([]string, error) {
	list, err := r.cli.ImageList(ctx, image.ListOptions{
		Filters: filters.NewArgs(filters.Arg("reference", refPattern)),
	})
	if err != nil {
		return nil, err
	}
	var tags []string
	for _, img := range list {
		// The reference filter matches an image when ANY of its tags does;
		// re-check per tag so an image also tagged outside the pattern does
		// not leak foreign tags into the result.
		for _, tag := range img.RepoTags {
			if ok, err := path.Match(refPattern, tag); err == nil && ok {
				tags = append(tags, tag)
			} else if repo, _, found := strings.Cut(tag, ":"); found {
				if ok, err := path.Match(refPattern, repo); err == nil && ok {
					tags = append(tags, tag)
				}
			}
		}
	}
	sort.Strings(tags)
	return tags, nil
}

func (r *real) RemoveImage(ctx context.Context, ref string) error {
	_, err := r.cli.ImageRemove(ctx, ref, image.RemoveOptions{PruneChildren: true})
	if client.IsErrNotFound(err) {
		return nil // already gone: that is the state we wanted
	}
	return err
}

func (r *real) PruneImages(ctx context.Context) (uint64, error) {
	report, err := r.cli.ImagesPrune(ctx, filters.NewArgs(filters.Arg("dangling", "true")))
	if err != nil {
		return 0, err
	}
	return report.SpaceReclaimed, nil
}

func (r *real) PruneBuildCache(ctx context.Context, olderThan time.Duration) (uint64, error) {
	report, err := r.cli.BuildCachePrune(ctx, build.CachePruneOptions{
		All:     true, // "all" here means shared/internal cache too, still bounded by the filter
		Filters: filters.NewArgs(filters.Arg("until", olderThan.String())),
	})
	if err != nil {
		return 0, err
	}
	if report == nil {
		return 0, nil
	}
	return report.SpaceReclaimed, nil
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
	var binds []string
	for _, m := range mounts {
		if m.Volume {
			continue // named volumes go through the Mounts API
		}
		b := m.Source + ":" + m.Target
		if m.ReadOnly {
			b += ":ro"
		}
		binds = append(binds, b)
	}
	return binds
}

func volumeMounts(mounts []Mount) []mount.Mount {
	var vols []mount.Mount
	for _, m := range mounts {
		if !m.Volume {
			continue
		}
		vols = append(vols, mount.Mount{
			Type:     mount.TypeVolume,
			Source:   m.Source,
			Target:   m.Target,
			ReadOnly: m.ReadOnly,
		})
	}
	return vols
}

func (r *real) ExecContainer(ctx context.Context, id string, cmd, env []string, stdout, stderr io.Writer) (int, error) {
	exec, err := r.cli.ContainerExecCreate(ctx, id, container.ExecOptions{
		Cmd:          cmd,
		Env:          env,
		AttachStdout: true,
		AttachStderr: true,
	})
	if err != nil {
		return 0, err
	}
	resp, err := r.cli.ContainerExecAttach(ctx, exec.ID, container.ExecAttachOptions{})
	if err != nil {
		return 0, err
	}
	defer resp.Close()
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	if _, err := stdcopy.StdCopy(stdout, stderr, resp.Reader); err != nil {
		return 0, err
	}
	info, err := r.cli.ContainerExecInspect(ctx, exec.ID)
	if err != nil {
		return 0, err
	}
	return info.ExitCode, nil
}

func (r *real) ListVolumes(ctx context.Context, match map[string]string) ([]string, error) {
	args := filters.NewArgs()
	for k, v := range match {
		args.Add("label", k+"="+v)
	}
	resp, err := r.cli.VolumeList(ctx, volume.ListOptions{Filters: args})
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(resp.Volumes))
	for _, v := range resp.Volumes {
		names = append(names, v.Name)
	}
	sort.Strings(names)
	return names, nil
}

func (r *real) RunContainer(ctx context.Context, spec ContainerSpec, stdout, stderr io.Writer) (int, error) {
	id, err := r.CreateContainer(ctx, spec)
	if err != nil {
		return 0, err
	}
	// Best-effort cleanup even on the error paths below.
	defer r.cli.ContainerRemove(context.WithoutCancel(ctx), id, container.RemoveOptions{Force: true})

	// Attach before starting so no output is lost.
	att, err := r.cli.ContainerAttach(ctx, id, container.AttachOptions{
		Stream: true, Stdout: true, Stderr: true,
	})
	if err != nil {
		return 0, err
	}
	defer att.Close()

	waitC, errC := r.cli.ContainerWait(ctx, id, container.WaitConditionNextExit)
	if err := r.cli.ContainerStart(ctx, id, container.StartOptions{}); err != nil {
		return 0, err
	}
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	if _, err := stdcopy.StdCopy(stdout, stderr, att.Reader); err != nil {
		return 0, err
	}
	select {
	case res := <-waitC:
		if res.Error != nil {
			return 0, fmt.Errorf("wait: %s", res.Error.Message)
		}
		return int(res.StatusCode), nil
	case err := <-errC:
		return 0, err
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}
