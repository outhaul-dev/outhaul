package tunnel

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/outhaul-dev/outhaul/internal/docker"
)

func testConfig() ConnectorConfig {
	return ConnectorConfig{
		Image:   "cloudflare/cloudflared:test",
		Network: "outhaul",
		Token:   "tok-abc",
	}
}

// recordingFake captures the spec passed to CreateContainer.
type recordingFake struct {
	*docker.Fake
	created docker.ContainerSpec
}

func (r *recordingFake) CreateContainer(ctx context.Context, spec docker.ContainerSpec) (string, error) {
	r.created = spec
	return r.Fake.CreateContainer(ctx, spec)
}

func TestEnsureConnectorCreatesRunningContainer(t *testing.T) {
	ctx := context.Background()
	rec := &recordingFake{Fake: docker.NewFake()}

	if err := EnsureConnector(ctx, rec, testConfig(), nil); err != nil {
		t.Fatalf("EnsureConnector: %v", err)
	}
	c, _ := rec.FindContainer(ctx, ContainerName)
	if c == nil || !c.Running() {
		t.Fatalf("connector not created/running: %+v", c)
	}
	// Token travels as an env var, never as a command argument.
	if !hasEnv(rec.created.Env, "TUNNEL_TOKEN=tok-abc") {
		t.Errorf("TUNNEL_TOKEN env missing: %v", rec.created.Env)
	}
	if strings.Contains(strings.Join(rec.created.Cmd, " "), "tok-abc") {
		t.Errorf("token must not appear in cmd: %v", rec.created.Cmd)
	}
	if strings.Join(rec.created.Cmd, " ") != "tunnel --no-autoupdate run" {
		t.Errorf("cmd = %v, want tunnel --no-autoupdate run", rec.created.Cmd)
	}
	if len(rec.created.Ports) != 0 {
		t.Errorf("connector must publish no host ports, got %v", rec.created.Ports)
	}
	if rec.created.RestartPolicy != "unless-stopped" {
		t.Errorf("restart policy = %q, want unless-stopped", rec.created.RestartPolicy)
	}
}

func TestEnsureConnectorTokenNeverInLabels(t *testing.T) {
	ctx := context.Background()
	rec := &recordingFake{Fake: docker.NewFake()}
	if err := EnsureConnector(ctx, rec, testConfig(), nil); err != nil {
		t.Fatalf("EnsureConnector: %v", err)
	}
	for k, v := range rec.created.Labels {
		if strings.Contains(v, "tok-abc") {
			t.Errorf("raw token leaked into label %s=%s", k, v)
		}
	}
}

func TestEnsureConnectorIsIdempotent(t *testing.T) {
	ctx := context.Background()
	f := docker.NewFake()
	for i := 0; i < 3; i++ {
		if err := EnsureConnector(ctx, f, testConfig(), nil); err != nil {
			t.Fatalf("EnsureConnector call %d: %v", i, err)
		}
	}
	count := 0
	for _, c := range f.Containers {
		if c.Name == ContainerName {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("expected 1 connector, found %d", count)
	}
}

func TestEnsureConnectorRecreatesOnTokenChange(t *testing.T) {
	ctx := context.Background()
	f := docker.NewFake()
	if err := EnsureConnector(ctx, f, testConfig(), nil); err != nil {
		t.Fatalf("EnsureConnector: %v", err)
	}
	before, _ := f.FindContainer(ctx, ContainerName)

	cc := testConfig()
	cc.Token = "tok-rotated"
	if err := EnsureConnector(ctx, f, cc, nil); err != nil {
		t.Fatalf("EnsureConnector rotated: %v", err)
	}
	after, _ := f.FindContainer(ctx, ContainerName)
	if after == nil || after.ID == before.ID {
		t.Error("rotating the token must recreate the connector")
	}
}

func TestEnsureConnectorStartsStoppedContainer(t *testing.T) {
	ctx := context.Background()
	f := docker.NewFake()
	cc := testConfig()

	// A connector container exists but is not running (e.g. after a host
	// reboot where restart policy did not fire). Build it from the same spec
	// EnsureConnector would produce so the config hash matches and it is
	// adopted rather than recreated.
	id, _ := f.CreateContainer(ctx, connectorSpec(cc))
	f.StopContainer(ctx, id, 0)

	if err := EnsureConnector(ctx, f, cc, nil); err != nil {
		t.Fatalf("EnsureConnector: %v", err)
	}
	c, _ := f.FindContainer(ctx, ContainerName)
	if c == nil || c.ID != id {
		t.Fatalf("expected existing connector %q to be adopted, got %+v", id, c)
	}
	if !c.Running() {
		t.Errorf("existing connector container should be started, state=%q", c.State)
	}
}

func TestEnsureConnectorKeepsOldContainerWhenPullFails(t *testing.T) {
	ctx := context.Background()
	f := docker.NewFake()
	if err := EnsureConnector(ctx, f, testConfig(), nil); err != nil {
		t.Fatalf("EnsureConnector: %v", err)
	}
	before, _ := f.FindContainer(ctx, ContainerName)

	// Token rotates (forces recreate path) but the image pull now fails.
	f.FailPull = func(string) error { return errors.New("registry down") }
	cc := testConfig()
	cc.Token = "tok-rotated"
	if err := EnsureConnector(ctx, f, cc, nil); err == nil {
		t.Fatal("expected EnsureConnector to fail when the pull fails")
	}
	after, _ := f.FindContainer(ctx, ContainerName)
	if after == nil || after.ID != before.ID {
		t.Error("old connector container must survive a failed pull (no teardown before pull)")
	}
}

func TestRemoveConnector(t *testing.T) {
	ctx := context.Background()
	f := docker.NewFake()
	if err := EnsureConnector(ctx, f, testConfig(), nil); err != nil {
		t.Fatalf("EnsureConnector: %v", err)
	}
	if err := RemoveConnector(ctx, f); err != nil {
		t.Fatalf("RemoveConnector: %v", err)
	}
	if c, _ := f.FindContainer(ctx, ContainerName); c != nil {
		t.Error("connector should be gone after RemoveConnector")
	}
	// Idempotent: removing again is a no-op.
	if err := RemoveConnector(ctx, f); err != nil {
		t.Fatalf("RemoveConnector (2nd): %v", err)
	}
}

func hasEnv(items []string, want string) bool {
	for _, s := range items {
		if s == want {
			return true
		}
	}
	return false
}
