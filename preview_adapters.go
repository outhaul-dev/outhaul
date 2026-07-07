package main

import (
	"context"
	"io"
	"time"

	"github.com/outhaul-dev/outhaul/internal/compose"
	"github.com/outhaul-dev/outhaul/internal/core"
	"github.com/outhaul-dev/outhaul/internal/dbaas"
	"github.com/outhaul-dev/outhaul/internal/docker"
	"github.com/outhaul-dev/outhaul/internal/store"
)

// previewDBProvisioner provisions/destroys isolated preview databases.
type previewDBProvisioner struct {
	st  *store.Store
	dbm *dbaas.Manager
}

// Provision creates the database row and synchronously starts its container so
// the caller can attach + deploy against a running DB.
func (p *previewDBProvisioner) Provision(ctx context.Context, d core.Database) (core.Database, error) {
	created, err := p.st.CreateDatabase(ctx, d) // persists row + keeps plaintext password
	if err != nil {
		return core.Database{}, err
	}
	if err := p.dbm.ProvisionSync(ctx, created); err != nil {
		_ = p.dbm.Remove(ctx, created) // drop row+data+partial container; safe on partial state
		return core.Database{}, err
	}
	created.Status = core.DBRunning // row is running now; keep the returned struct consistent
	return created, nil
}

// Destroy removes the container, data, and store row. Called by teardown only
// AFTER the child app (and its attachment rows) are deleted, so the row delete
// is FK-safe.
func (p *previewDBProvisioner) Destroy(ctx context.Context, d core.Database) error {
	return p.dbm.Remove(ctx, d)
}

// previewDocker tears down a preview's containers/stack.
type previewDocker struct {
	st      *store.Store
	runtime docker.Client
	compose compose.Runner
}

// RemoveApp removes a preview app's container (single-container) or whole
// compose stack, looked up by name (the child app still exists when teardown
// calls this). Mirrors handleDeleteApp's best-effort teardown.
func (d *previewDocker) RemoveApp(ctx context.Context, name string) error {
	app, err := d.st.GetAppByName(ctx, name)
	if err != nil {
		return err
	}
	if app.Kind == core.KindCompose {
		return d.compose.Down(ctx, compose.ProjectName(name), io.Discard)
	}
	c, err := d.runtime.FindContainer(ctx, core.AppContainerName(name))
	if err != nil || c == nil {
		return err
	}
	_ = d.runtime.StopContainer(ctx, c.ID, 10*time.Second)
	return d.runtime.RemoveContainer(ctx, c.ID, true)
}
