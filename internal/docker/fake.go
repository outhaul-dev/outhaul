package docker

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"
)

// Fake is an in-memory Client for unit tests. It records enough state to model
// the operations Slipway performs (networks, containers, lifecycle) and mirrors
// Docker's "duplicate name" error so replace-then-create paths are exercised.
type Fake struct {
	mu         sync.Mutex
	seq        int
	Networks   map[string]bool
	Containers map[string]*Container // keyed by ID
	Pulled     []string              // images pulled, in order
	Created    []ContainerSpec       // specs passed to CreateContainer, in order
	IPs        map[string]string     // container ID -> IP (test-settable)

	// FailPull, when set, makes PullImage return an error for matching refs.
	FailPull func(ref string) error

	FailCreate func(spec ContainerSpec) error
	FailRemove func(id string) error
}

// NewFake returns an empty Fake.
func NewFake() *Fake {
	return &Fake{
		Networks:   map[string]bool{},
		Containers: map[string]*Container{},
		IPs:        map[string]string{},
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

func (f *Fake) ContainerIP(_ context.Context, id, _ string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.Containers[id]; !ok {
		return "", fmt.Errorf("no such container: %s", id)
	}
	return f.IPs[id], nil
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
