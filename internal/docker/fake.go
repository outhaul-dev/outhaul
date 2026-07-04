package docker

import (
	"context"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"sync"
	"time"
)

// Fake is an in-memory Client for unit tests. It records enough state to model
// the operations Outhaul performs (networks, containers, lifecycle) and mirrors
// Docker's "duplicate name" error so replace-then-create paths are exercised.
type Fake struct {
	mu         sync.Mutex
	seq        int
	Networks   map[string]bool
	Containers map[string]*Container // keyed by ID
	Pulled     []string              // images pulled, in order
	Created    []ContainerSpec       // specs passed to CreateContainer, in order
	IPs        map[string]string     // container ID -> IP (test-settable)
	Logs       map[string]string     // container ID -> log content (test-settable)
	LogTails   []int                 // tail values passed to ContainerLogs, in order
	Stats      map[string]Stats      // container ID -> stats sample (test-settable)

	Volumes map[string]map[string]string // volume name -> labels (test-settable)
	Execs   []ExecCall                   // ExecContainer invocations, in order
	Runs    []ContainerSpec              // RunContainer specs, in order

	Images           map[string]bool // local image tags (test-settable)
	RemovedImages    []string        // refs passed to RemoveImage, in order
	ImagePrunes      int             // PruneImages invocations
	BuildCachePrunes []time.Duration // olderThan values passed to PruneBuildCache

	// OnExec, when set, supplies ExecContainer's output and exit code.
	OnExec func(id string, cmd, env []string) (stdout, stderr string, exit int, err error)
	// OnRun, when set, supplies RunContainer's stdout and exit code.
	OnRun func(spec ContainerSpec) (stdout string, exit int, err error)

	// FailPull, when set, makes PullImage return an error for matching refs.
	FailPull func(ref string) error

	FailCreate func(spec ContainerSpec) error
	FailRemove func(id string) error

	// FailRemoveImage, when set, makes RemoveImage return an error for
	// matching refs (models "image is in use by a container").
	FailRemoveImage func(ref string) error
}

// ExecCall records one ExecContainer invocation.
type ExecCall struct {
	ContainerID string
	Cmd         []string
	Env         []string
}

// NewFake returns an empty Fake.
func NewFake() *Fake {
	return &Fake{
		Networks:   map[string]bool{},
		Containers: map[string]*Container{},
		IPs:        map[string]string{},
		Logs:       map[string]string{},
		Stats:      map[string]Stats{},
		Volumes:    map[string]map[string]string{},
		Images:     map[string]bool{},
	}
}

func (f *Fake) Ping(context.Context) error { return nil }

func (f *Fake) PullImage(_ context.Context, ref string, out io.Writer) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.FailPull != nil {
		if err := f.FailPull(ref); err != nil {
			return err
		}
	}
	f.Pulled = append(f.Pulled, ref)
	if out != nil {
		fmt.Fprintf(out, "pulled %s\n", ref)
	}
	return nil
}

func (f *Fake) EnsureNetwork(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Networks[name] = true
	return nil
}

func (f *Fake) FindContainer(_ context.Context, name string) (*Container, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c := f.byName(name); c != nil {
		clone := *c
		return &clone, nil
	}
	return nil, nil
}

func (f *Fake) ListContainers(_ context.Context, match map[string]string) ([]Container, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Container
	for _, c := range f.Containers {
		ok := true
		for k, v := range match {
			if c.Labels[k] != v {
				ok = false
				break
			}
		}
		if ok {
			out = append(out, *c)
		}
	}
	return out, nil
}

func (f *Fake) CreateContainer(_ context.Context, spec ContainerSpec) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.FailCreate != nil {
		if err := f.FailCreate(spec); err != nil {
			return "", err
		}
	}
	if f.byName(spec.Name) != nil {
		return "", fmt.Errorf("container name %q already in use", spec.Name)
	}
	f.Created = append(f.Created, spec)
	f.seq++
	id := fmt.Sprintf("ctr%d", f.seq)
	f.Containers[id] = &Container{
		ID:     id,
		Name:   spec.Name,
		Image:  spec.Image,
		State:  "created",
		Labels: spec.Labels,
	}
	f.IPs[id] = fmt.Sprintf("10.88.0.%d", f.seq)
	return id, nil
}

func (f *Fake) StartContainer(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.Containers[id]
	if !ok {
		return fmt.Errorf("no such container: %s", id)
	}
	c.State = "running"
	return nil
}

func (f *Fake) StopContainer(_ context.Context, id string, _ time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.Containers[id]
	if !ok {
		return fmt.Errorf("no such container: %s", id)
	}
	c.State = "exited"
	return nil
}

func (f *Fake) RemoveContainer(_ context.Context, id string, _ bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.FailRemove != nil {
		if err := f.FailRemove(id); err != nil {
			return err
		}
	}
	if _, ok := f.Containers[id]; !ok {
		return fmt.Errorf("no such container: %s", id)
	}
	delete(f.Containers, id)
	return nil
}

func (f *Fake) ContainerLogs(_ context.Context, id string, tail int) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.Containers[id]; !ok {
		return nil, fmt.Errorf("no such container: %s", id)
	}
	f.LogTails = append(f.LogTails, tail)
	return io.NopCloser(strings.NewReader(f.Logs[id])), nil
}

func (f *Fake) ContainerStats(_ context.Context, id string) (Stats, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.Containers[id]; !ok {
		return Stats{}, fmt.Errorf("no such container: %s", id)
	}
	return f.Stats[id], nil
}

func (f *Fake) ContainerIP(_ context.Context, id, _ string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.Containers[id]; !ok {
		return "", fmt.Errorf("no such container: %s", id)
	}
	return f.IPs[id], nil
}

func (f *Fake) ExecContainer(_ context.Context, id string, cmd, env []string, stdout, stderr io.Writer) (int, error) {
	f.mu.Lock()
	if _, ok := f.Containers[id]; !ok {
		f.mu.Unlock()
		return 0, fmt.Errorf("no such container: %s", id)
	}
	f.Execs = append(f.Execs, ExecCall{ContainerID: id, Cmd: cmd, Env: env})
	hook := f.OnExec
	f.mu.Unlock()
	if hook == nil {
		return 0, nil
	}
	out, errOut, exit, err := hook(id, cmd, env)
	if err != nil {
		return 0, err
	}
	if stdout != nil {
		io.WriteString(stdout, out)
	}
	if stderr != nil {
		io.WriteString(stderr, errOut)
	}
	return exit, nil
}

func (f *Fake) ListVolumes(_ context.Context, match map[string]string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var names []string
	for name, labels := range f.Volumes {
		ok := true
		for k, v := range match {
			if labels[k] != v {
				ok = false
				break
			}
		}
		if ok {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names, nil
}

func (f *Fake) RunContainer(_ context.Context, spec ContainerSpec, stdout, _ io.Writer) (int, error) {
	f.mu.Lock()
	f.Runs = append(f.Runs, spec)
	hook := f.OnRun
	f.mu.Unlock()
	if hook == nil {
		return 0, nil
	}
	out, exit, err := hook(spec)
	if err != nil {
		return 0, err
	}
	if stdout != nil {
		io.WriteString(stdout, out)
	}
	return exit, nil
}

func (f *Fake) ListImages(_ context.Context, refPattern string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var tags []string
	for tag := range f.Images {
		if ok, _ := path.Match(refPattern, tag); ok {
			tags = append(tags, tag)
		}
	}
	sort.Strings(tags)
	return tags, nil
}

func (f *Fake) RemoveImage(_ context.Context, ref string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.FailRemoveImage != nil {
		if err := f.FailRemoveImage(ref); err != nil {
			return err
		}
	}
	// A missing ref is success, mirroring the real client's not-found
	// swallowing; record the call either way.
	f.RemovedImages = append(f.RemovedImages, ref)
	delete(f.Images, ref)
	return nil
}

func (f *Fake) PruneImages(context.Context) (uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ImagePrunes++
	return 0, nil
}

func (f *Fake) PruneBuildCache(_ context.Context, olderThan time.Duration) (uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.BuildCachePrunes = append(f.BuildCachePrunes, olderThan)
	return 0, nil
}

func (f *Fake) Close() error { return nil }

// byName returns the container with the given name (caller holds the lock).
func (f *Fake) byName(name string) *Container {
	for _, c := range f.Containers {
		if c.Name == name {
			return c
		}
	}
	return nil
}
