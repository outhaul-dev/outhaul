package dbaas

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/outhaul-dev/outhaul/internal/core"
	"github.com/outhaul-dev/outhaul/internal/docker"
	"github.com/outhaul-dev/outhaul/internal/store"
)

// containerPrefix names database containers, disjoint from the app
// (outhaul-app-), deploy temp (outhaul-deploy-), compose (outhaul-<name>),
// and Traefik (outhaul-traefik) namespaces.
const containerPrefix = "outhaul-db-"

// ContainerName returns the container name for a database.
func ContainerName(dbName string) string { return containerPrefix + dbName }

// provisionTimeout bounds one provision attempt, image pull included.
const provisionTimeout = 15 * time.Minute

// helperImage deletes data directories the engine chowned to a
// container-internal uid, which Outhaul cannot remove when it runs as an
// unprivileged service user.
const helperImage = "busybox:stable"

// stopTimeout is how long an engine gets to shut down cleanly.
const stopTimeout = 30 * time.Second

// Manager provisions and controls database containers, keeping the store row
// in sync with what it did. All Docker access for databases goes through it.
type Manager struct {
	store   *store.Store
	docker  docker.Client
	network string // shared Docker network the container joins
	dataDir string // host root for per-database data directories

	// removeAll is os.RemoveAll, injected so tests can force the
	// unprivileged-user fallback path in removeData.
	removeAll func(string) error

	// provisionDone, when non-nil, is signalled after each async provision
	// attempt finishes (tests use it to wait deterministically).
	provisionDone chan struct{}
}

// NewManager wires a Manager. dataDir is config.DatabasesDir().
func NewManager(st *store.Store, dc docker.Client, network, dataDir string) *Manager {
	return &Manager{store: st, docker: dc, network: network, dataDir: dataDir, removeAll: os.RemoveAll}
}

// DataPath is the host directory holding a database's persistent data.
func (m *Manager) DataPath(dbName string) string { return filepath.Join(m.dataDir, dbName) }

// Provision (re)creates the database container in the background: pull the
// image, remove any existing container, create and start a fresh one. The row
// moves creating → running/failed. Reprovisioning is how a failed database is
// retried and how a changed external port is applied — data survives in the
// bind mount.
func (m *Manager) Provision(d core.Database) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), provisionTimeout)
		defer cancel()
		if err := m.provision(ctx, d); err != nil {
			if serr := m.store.SetDatabaseStatus(ctx, d.ID, core.DBFailed, err.Error()); serr != nil {
				log.Printf("database %s: record failure: %v (original: %v)", d.Name, serr, err)
			}
		}
		if m.provisionDone != nil {
			m.provisionDone <- struct{}{}
		}
	}()
}

// ProvisionSync provisions the database synchronously (pull, create, start, set
// status), returning any error. Unlike Provision it blocks and reports failure
// to the caller instead of only recording it on the row. Used where the caller
// must know the database is running before proceeding (e.g. preview spin-up).
func (m *Manager) ProvisionSync(ctx context.Context, d core.Database) error {
	return m.provision(ctx, d)
}

func (m *Manager) provision(ctx context.Context, d core.Database) error {
	if err := m.store.SetDatabaseStatus(ctx, d.ID, core.DBCreating, ""); err != nil {
		return err
	}
	if err := m.docker.PullImage(ctx, d.Image, io.Discard); err != nil {
		return fmt.Errorf("pull %s: %w", d.Image, err)
	}
	if err := os.MkdirAll(m.DataPath(d.Name), 0o700); err != nil {
		return fmt.Errorf("create data dir: %w", err)
	}
	// Remove any previous container (failed attempt or old config); the data
	// dir carries the state, so recreation is safe.
	if existing, err := m.docker.FindContainer(ctx, ContainerName(d.Name)); err != nil {
		return err
	} else if existing != nil {
		if err := m.docker.RemoveContainer(ctx, existing.ID, true); err != nil {
			return fmt.Errorf("remove old container: %w", err)
		}
	}
	eng := engines[d.Engine]
	spec := docker.ContainerSpec{
		Name:  ContainerName(d.Name),
		Image: d.Image,
		Labels: map[string]string{
			"outhaul.managed": "true",
			"outhaul.role":    "database",
			"outhaul.db":      d.Name,
		},
		Env:      env(d),
		Cmd:      cmd(d),
		Networks: []string{m.network},
		Mounts:   []docker.Mount{{Source: m.DataPath(d.Name), Target: eng.DataDir}},
		// Docker owns reboots/crashes, same as app containers.
		RestartPolicy: "unless-stopped",
	}
	if d.ExtPort > 0 {
		spec.Ports = []docker.PortMapping{{
			HostPort:      strconv.Itoa(d.ExtPort),
			ContainerPort: strconv.Itoa(eng.Port),
			Proto:         "tcp",
		}}
	}
	id, err := m.docker.CreateContainer(ctx, spec)
	if err != nil {
		return fmt.Errorf("create container: %w", err)
	}
	if err := m.docker.StartContainer(ctx, id); err != nil {
		return fmt.Errorf("start container: %w", err)
	}
	return m.store.SetDatabaseStatus(ctx, d.ID, core.DBRunning, "")
}

// Start starts a stopped database container.
func (m *Manager) Start(ctx context.Context, d core.Database) error {
	c, err := m.docker.FindContainer(ctx, ContainerName(d.Name))
	if err != nil {
		return err
	}
	if c == nil {
		return fmt.Errorf("no container for database %s; apply settings to recreate it", d.Name)
	}
	if err := m.docker.StartContainer(ctx, c.ID); err != nil {
		return err
	}
	return m.store.SetDatabaseStatus(ctx, d.ID, core.DBRunning, "")
}

// Stop stops the database container (data stays; Start brings it back).
func (m *Manager) Stop(ctx context.Context, d core.Database) error {
	c, err := m.docker.FindContainer(ctx, ContainerName(d.Name))
	if err != nil {
		return err
	}
	if c != nil {
		if err := m.docker.StopContainer(ctx, c.ID, stopTimeout); err != nil {
			return err
		}
	}
	return m.store.SetDatabaseStatus(ctx, d.ID, core.DBStopped, "")
}

// Remove deletes the database: container, data directory, and row. This is
// irreversible — the caller's UI must say the data goes with it.
func (m *Manager) Remove(ctx context.Context, d core.Database) error {
	c, err := m.docker.FindContainer(ctx, ContainerName(d.Name))
	if err != nil {
		return err
	}
	if c != nil {
		if err := m.docker.RemoveContainer(ctx, c.ID, true); err != nil {
			return err
		}
	}
	if err := m.removeData(ctx, d.Name); err != nil {
		return err
	}
	return m.store.DeleteDatabase(ctx, d.ID)
}

// removeData deletes a database's data directory. Engines chown their data to
// a container-internal uid, so when Outhaul runs as an unprivileged user a
// plain removal fails with EACCES; in that case the (root) daemon does it for
// us via a helper container that mounts the databases root and removes the
// one subtree.
func (m *Manager) removeData(ctx context.Context, name string) error {
	err := m.removeAll(m.DataPath(name))
	if err == nil {
		return nil
	}
	if perr := m.docker.PullImage(ctx, helperImage, io.Discard); perr != nil {
		return fmt.Errorf("remove data dir: %w (helper pull: %v)", err, perr)
	}
	var stderr bytes.Buffer
	spec := docker.ContainerSpec{
		Name:  "outhaul-db-rm-" + name,
		Image: helperImage,
		Cmd:   []string{"rm", "-rf", "/data/" + name},
		Labels: map[string]string{
			"outhaul.managed": "true",
			"outhaul.role":    "helper",
		},
		Mounts: []docker.Mount{{Source: m.dataDir, Target: "/data"}},
	}
	exit, rerr := m.docker.RunContainer(ctx, spec, io.Discard, &stderr)
	if rerr != nil {
		return fmt.Errorf("remove data dir helper: %w", rerr)
	}
	if exit != 0 {
		return fmt.Errorf("remove data dir helper exited %d: %s", exit, strings.TrimSpace(stderr.String()))
	}
	return nil
}
