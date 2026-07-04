package dbaas

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/slipwaydev/slipway/internal/core"
	"github.com/slipwaydev/slipway/internal/docker"
	"github.com/slipwaydev/slipway/internal/store"
)

// containerPrefix names database containers, disjoint from the app
// (slipway-app-), deploy temp (slipway-deploy-), compose (slipway-<name>),
// and Traefik (slipway-traefik) namespaces.
const containerPrefix = "slipway-db-"

// ContainerName returns the container name for a database.
func ContainerName(dbName string) string { return containerPrefix + dbName }

// provisionTimeout bounds one provision attempt, image pull included.
const provisionTimeout = 15 * time.Minute

// stopTimeout is how long an engine gets to shut down cleanly.
const stopTimeout = 30 * time.Second

// Manager provisions and controls database containers, keeping the store row
// in sync with what it did. All Docker access for databases goes through it.
type Manager struct {
	store   *store.Store
	docker  docker.Client
	network string // shared Docker network the container joins
	dataDir string // host root for per-database data directories

	// provisionDone, when non-nil, is signalled after each async provision
	// attempt finishes (tests use it to wait deterministically).
	provisionDone chan struct{}
}

// NewManager wires a Manager. dataDir is config.DatabasesDir().
func NewManager(st *store.Store, dc docker.Client, network, dataDir string) *Manager {
	return &Manager{store: st, docker: dc, network: network, dataDir: dataDir}
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
			"slipway.managed": "true",
			"slipway.role":    "database",
			"slipway.db":      d.Name,
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
	if err := os.RemoveAll(m.DataPath(d.Name)); err != nil {
		return fmt.Errorf("remove data dir: %w", err)
	}
	return m.store.DeleteDatabase(ctx, d.ID)
}
