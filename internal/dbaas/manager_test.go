package dbaas

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/outhaul-dev/outhaul/internal/core"
	"github.com/outhaul-dev/outhaul/internal/docker"
	"github.com/outhaul-dev/outhaul/internal/secret"
	"github.com/outhaul-dev/outhaul/internal/store"
)

type harness struct {
	m      *Manager
	store  *store.Store
	docker *docker.Fake
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	box, err := secret.Load(filepath.Join(dir, "key"))
	if err != nil {
		t.Fatalf("secret.Load: %v", err)
	}
	st, err := store.Open(filepath.Join(dir, "test.db"), box)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	fake := docker.NewFake()
	return &harness{
		m:      NewManager(st, fake, "outhaul", filepath.Join(dir, "databases")),
		store:  st,
		docker: fake,
	}
}

func (h *harness) database(t *testing.T, name, engine string) core.Database {
	t.Helper()
	d := core.Database{
		Name: name, Engine: engine, Image: DefaultImage(engine), Password: "pw123",
	}
	if HasUserDB(engine) {
		d.Username = name
		d.DBName = name
	}
	d, err := h.store.CreateDatabase(context.Background(), d)
	if err != nil {
		t.Fatalf("CreateDatabase: %v", err)
	}
	return d
}

func (h *harness) status(t *testing.T, id int64) core.Database {
	t.Helper()
	d, err := h.store.GetDatabase(context.Background(), id)
	if err != nil {
		t.Fatalf("GetDatabase: %v", err)
	}
	return d
}

func hasEnv(spec docker.ContainerSpec, kv string) bool {
	for _, e := range spec.Env {
		if e == kv {
			return true
		}
	}
	return false
}

func TestProvisionCreatesAndStartsContainer(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	d := h.database(t, "shop", core.EnginePostgres)

	if err := h.m.provision(ctx, d); err != nil {
		t.Fatalf("provision: %v", err)
	}

	if got := h.status(t, d.ID); got.Status != core.DBRunning {
		t.Fatalf("status = %q (reason %q), want running", got.Status, got.Reason)
	}
	if len(h.docker.Pulled) != 1 || h.docker.Pulled[0] != "postgres:17" {
		t.Errorf("pulled %v, want the engine image", h.docker.Pulled)
	}
	c, _ := h.docker.FindContainer(ctx, "outhaul-db-shop")
	if c == nil || !c.Running() {
		t.Fatalf("container = %+v, want running outhaul-db-shop", c)
	}
	if c.Labels["outhaul.role"] != "database" || c.Labels["outhaul.db"] != "shop" {
		t.Errorf("labels = %v, want outhaul.role=database and outhaul.db=shop", c.Labels)
	}

	spec := h.docker.Created[0]
	if !hasEnv(spec, "POSTGRES_USER=shop") || !hasEnv(spec, "POSTGRES_PASSWORD=pw123") || !hasEnv(spec, "POSTGRES_DB=shop") {
		t.Errorf("env = %v, missing postgres credentials", spec.Env)
	}
	if spec.RestartPolicy != "unless-stopped" {
		t.Errorf("restart policy = %q, want unless-stopped (reboots are Docker's job)", spec.RestartPolicy)
	}
	if len(spec.Networks) != 1 || spec.Networks[0] != "outhaul" {
		t.Errorf("networks = %v, want the shared network", spec.Networks)
	}
	if len(spec.Ports) != 0 {
		t.Errorf("ports = %v, want none (internal-only by default)", spec.Ports)
	}
	if len(spec.Mounts) != 1 || spec.Mounts[0].Source != h.m.DataPath("shop") || spec.Mounts[0].Target != "/var/lib/postgresql/data" {
		t.Errorf("mounts = %v, want the data dir bound to the postgres data path", spec.Mounts)
	}
	if fi, err := os.Stat(h.m.DataPath("shop")); err != nil || !fi.IsDir() {
		t.Errorf("data dir not created: %v", err)
	}
}

func TestProvisionPublishesExternalPort(t *testing.T) {
	h := newHarness(t)
	d := h.database(t, "shop", core.EnginePostgres)
	d.ExtPort = 5433

	if err := h.m.provision(context.Background(), d); err != nil {
		t.Fatalf("provision: %v", err)
	}
	ports := h.docker.Created[0].Ports
	if len(ports) != 1 || ports[0].HostPort != "5433" || ports[0].ContainerPort != "5432" || ports[0].Proto != "tcp" {
		t.Errorf("ports = %v, want host 5433 -> container 5432/tcp", ports)
	}
}

func TestProvisionRedisUsesRequirepass(t *testing.T) {
	h := newHarness(t)
	d := h.database(t, "cache", core.EngineRedis)

	if err := h.m.provision(context.Background(), d); err != nil {
		t.Fatalf("provision: %v", err)
	}
	spec := h.docker.Created[0]
	if len(spec.Env) != 0 {
		t.Errorf("env = %v, want none for redis", spec.Env)
	}
	want := []string{"redis-server", "--requirepass", "pw123"}
	if len(spec.Cmd) != 3 || spec.Cmd[0] != want[0] || spec.Cmd[1] != want[1] || spec.Cmd[2] != want[2] {
		t.Errorf("cmd = %v, want %v", spec.Cmd, want)
	}
}

// A reprovision (retry after failure, or a changed external port) replaces the
// existing container; the bind mount carries the data across.
func TestProvisionReplacesExistingContainer(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	d := h.database(t, "shop", core.EnginePostgres)

	if err := h.m.provision(ctx, d); err != nil {
		t.Fatal(err)
	}
	first, _ := h.docker.FindContainer(ctx, "outhaul-db-shop")

	d.ExtPort = 5433
	if err := h.m.provision(ctx, d); err != nil {
		t.Fatalf("reprovision: %v", err)
	}
	second, _ := h.docker.FindContainer(ctx, "outhaul-db-shop")
	if second == nil || second.ID == first.ID {
		t.Fatalf("container was not replaced: first %+v second %+v", first, second)
	}
	if !second.Running() {
		t.Error("replacement container not started")
	}
}

// Provision (the async entry point) records a pull failure on the row.
func TestProvisionFailureMarksRowFailed(t *testing.T) {
	h := newHarness(t)
	h.docker.FailPull = func(ref string) error { return errors.New("registry unreachable") }
	h.m.provisionDone = make(chan struct{}, 1)
	d := h.database(t, "shop", core.EnginePostgres)

	h.m.Provision(d)
	<-h.m.provisionDone

	got := h.status(t, d.ID)
	if got.Status != core.DBFailed {
		t.Fatalf("status = %q, want failed", got.Status)
	}
	if !strings.Contains(got.Reason, "registry unreachable") {
		t.Errorf("reason = %q, want the pull error", got.Reason)
	}
}

func TestStopAndStartSyncRow(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	d := h.database(t, "shop", core.EnginePostgres)
	if err := h.m.provision(ctx, d); err != nil {
		t.Fatal(err)
	}

	if err := h.m.Stop(ctx, d); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if c, _ := h.docker.FindContainer(ctx, "outhaul-db-shop"); c.Running() {
		t.Error("container still running after Stop")
	}
	if got := h.status(t, d.ID); got.Status != core.DBStopped {
		t.Errorf("status = %q, want stopped", got.Status)
	}

	if err := h.m.Start(ctx, d); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if c, _ := h.docker.FindContainer(ctx, "outhaul-db-shop"); !c.Running() {
		t.Error("container not running after Start")
	}
	if got := h.status(t, d.ID); got.Status != core.DBRunning {
		t.Errorf("status = %q, want running", got.Status)
	}
}

func TestRemoveDeletesContainerDataAndRow(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	d := h.database(t, "shop", core.EnginePostgres)
	if err := h.m.provision(ctx, d); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(h.m.DataPath("shop"), "base")
	if err := os.WriteFile(marker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := h.m.Remove(ctx, d); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if c, _ := h.docker.FindContainer(ctx, "outhaul-db-shop"); c != nil {
		t.Error("container survived Remove")
	}
	if _, err := os.Stat(h.m.DataPath("shop")); !os.IsNotExist(err) {
		t.Error("data dir survived Remove")
	}
	if _, err := h.store.GetDatabase(ctx, d.ID); err == nil {
		t.Error("row survived Remove")
	}
	if len(h.docker.Runs) != 0 {
		t.Errorf("helper containers = %v, want none when plain removal works", h.docker.Runs)
	}
}

// Under an unprivileged service user the engine has chowned its data to a
// container-internal uid and plain removal fails; the (root) daemon must then
// delete it via a helper container.
func TestRemoveFallsBackToHelperContainer(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	d := h.database(t, "shop", core.EnginePostgres)
	if err := h.m.provision(ctx, d); err != nil {
		t.Fatal(err)
	}
	h.m.removeAll = func(string) error { return errors.New("permission denied") }

	if err := h.m.Remove(ctx, d); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if len(h.docker.Runs) != 1 {
		t.Fatalf("helper runs = %d, want 1", len(h.docker.Runs))
	}
	spec := h.docker.Runs[0]
	if spec.Image != helperImage {
		t.Errorf("helper image = %q, want %q", spec.Image, helperImage)
	}
	if len(spec.Cmd) != 3 || spec.Cmd[0] != "rm" || spec.Cmd[1] != "-rf" || spec.Cmd[2] != "/data/shop" {
		t.Errorf("helper cmd = %v, want rm -rf /data/shop", spec.Cmd)
	}
	if len(spec.Mounts) != 1 || spec.Mounts[0].Source != h.m.dataDir || spec.Mounts[0].Target != "/data" {
		t.Errorf("helper mounts = %v, want the databases root at /data", spec.Mounts)
	}
	if spec.Labels["outhaul.role"] != "helper" {
		t.Errorf("labels = %v, want outhaul.role=helper", spec.Labels)
	}
	if !hasPulled(h.docker.Pulled, helperImage) {
		t.Errorf("pulled = %v, want the helper image pulled", h.docker.Pulled)
	}
	if _, err := h.store.GetDatabase(ctx, d.ID); err == nil {
		t.Error("row survived Remove")
	}
}

func TestRemoveHelperFailureSurfaces(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	d := h.database(t, "shop", core.EnginePostgres)
	if err := h.m.provision(ctx, d); err != nil {
		t.Fatal(err)
	}
	h.m.removeAll = func(string) error { return errors.New("permission denied") }
	h.docker.OnRun = func(docker.ContainerSpec) (string, int, error) { return "", 1, nil }

	err := h.m.Remove(ctx, d)
	if err == nil || !strings.Contains(err.Error(), "exited 1") {
		t.Fatalf("Remove = %v, want the helper exit code surfaced", err)
	}
	if _, gerr := h.store.GetDatabase(ctx, d.ID); gerr != nil {
		t.Error("row deleted even though the data dir survived")
	}
}

func hasPulled(pulled []string, ref string) bool {
	for _, p := range pulled {
		if p == ref {
			return true
		}
	}
	return false
}

func TestConnectionURLs(t *testing.T) {
	pg := core.Database{Name: "shop", Engine: core.EnginePostgres, Username: "shop", Password: "pw", DBName: "shop"}
	if got, want := InternalURL(pg), "postgres://shop:pw@outhaul-db-shop:5432/shop"; got != want {
		t.Errorf("InternalURL = %q, want %q", got, want)
	}
	if got := ExternalURL(pg, "203.0.113.7"); got != "" {
		t.Errorf("ExternalURL without a published port = %q, want empty", got)
	}
	pg.ExtPort = 5433
	if got, want := ExternalURL(pg, "203.0.113.7"), "postgres://shop:pw@203.0.113.7:5433/shop"; got != want {
		t.Errorf("ExternalURL = %q, want %q", got, want)
	}

	redis := core.Database{Name: "cache", Engine: core.EngineRedis, Password: "pw"}
	if got, want := InternalURL(redis), "redis://:pw@outhaul-db-cache:6379/0"; got != want {
		t.Errorf("redis InternalURL = %q, want %q", got, want)
	}
}
