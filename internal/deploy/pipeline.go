package deploy

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/slipwaydev/slipway/internal/builder"
	"github.com/slipwaydev/slipway/internal/core"
	"github.com/slipwaydev/slipway/internal/docker"
	"github.com/slipwaydev/slipway/internal/traefik"
)

// AppPort is the internal port Slipway asks apps to listen on (via $PORT) and
// tells Traefik to route to. Nixpacks-built apps that honour $PORT work out of
// the box; per-app ports are a later seam.
const AppPort = 8080

// stopTimeout bounds how long we wait for an old container to stop.
const stopTimeout = 10 * time.Second

// runPipeline drives a single claimed (building) deployment through
// build -> deploy -> running, or to failed. It always closes the log stream.
//
// The deployment is assumed to already be in the building state (the dispatcher
// claimed it). ctx is the pipeline's cancellable context; status writes use a
// background context so a terminal state is still persisted if ctx was cancelled
// (shutdown), and so an operator cancel (which flips the row to cancelled) is
// never clobbered.
func (w *Worker) runPipeline(ctx context.Context, dep core.Deployment) {
	defer w.broker.Close(dep.ID)
	out := w.logWriter(dep.ID)

	app, err := w.store.GetApp(ctx, dep.AppID)
	if err != nil {
		w.fail(dep, core.StatusBuilding, "load app: "+err.Error(), out)
		return
	}

	logf(out, "Deploying %s (%s) to %s", app.Name, app.RepoURL, app.Domain)

	envVars, err := w.store.ListEnv(ctx, app.ID)
	if err != nil {
		w.fail(dep, core.StatusBuilding, "load env: "+err.Error(), out)
		return
	}
	buildEnv := map[string]string{"PORT": fmt.Sprintf("%d", AppPort)}
	runtimeEnv := []string{fmt.Sprintf("PORT=%d", AppPort)}
	for _, v := range envVars {
		if v.Key == "PORT" {
			continue // PORT is managed by the pipeline (also rejected at the UI layer)
		}
		runtimeEnv = append(runtimeEnv, v.Key+"="+v.Value)
		if !v.IsSecret {
			buildEnv[v.Key] = v.Value
		}
	}

	// --- clone ---
	workDir := filepath.Join(w.cfg.WorkDir(), fmt.Sprintf("dep-%d", dep.ID))
	if err := os.RemoveAll(workDir); err != nil {
		w.fail(dep, core.StatusBuilding, "prepare work dir: "+err.Error(), out)
		return
	}
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		w.fail(dep, core.StatusBuilding, "prepare work dir: "+err.Error(), out)
		return
	}
	defer os.RemoveAll(workDir)

	logf(out, "Cloning repository...")
	if err := w.cloner.Clone(ctx, app.RepoURL, workDir, out); err != nil {
		w.fail(dep, core.StatusBuilding, "clone failed: "+err.Error(), out)
		return
	}

	// --- build ---
	image := fmt.Sprintf("slipway/%s:%d", app.Name, dep.ID)
	logf(out, "Building image %s with %s...", image, w.builder.Name())
	req := builder.BuildRequest{
		ContextDir: workDir,
		ImageTag:   image,
		Env:        buildEnv,
	}
	if err := w.builder.Build(ctx, req, out); err != nil {
		w.fail(dep, core.StatusBuilding, "build failed: "+err.Error(), out)
		return
	}
	if err := w.store.SetImage(context.Background(), dep.ID, image); err != nil {
		w.fail(dep, core.StatusBuilding, "record image: "+err.Error(), out)
		return
	}

	// --- building -> deploying ---
	ok, err := w.store.SetStatus(context.Background(), dep.ID, core.StatusBuilding, core.StatusDeploying, "")
	if err != nil {
		w.fail(dep, core.StatusBuilding, "advance to deploying: "+err.Error(), out)
		return
	}
	if !ok {
		// The row is no longer building — an operator cancelled during the
		// build. Stop here without touching the live container.
		logf(out, "Deployment is no longer building (cancelled); aborting.")
		return
	}

	// --- start container ---
	logf(out, "Starting container...")
	if err := w.startContainer(ctx, app, image, runtimeEnv, out); err != nil {
		w.fail(dep, core.StatusDeploying, "start failed: "+err.Error(), out)
		return
	}

	// --- deploying -> running ---
	if _, err := w.store.SetStatus(context.Background(), dep.ID, core.StatusDeploying, core.StatusRunning, ""); err != nil {
		logf(out, "WARNING: could not record running status: %v", err)
	}
	logf(out, "Done. %s is live at http://%s", app.Name, app.Domain)
}

// startContainer replaces any existing container for the app with a fresh one
// built from image, wired with Traefik labels and joined to the shared network.
func (w *Worker) startContainer(ctx context.Context, app core.App, image string, env []string, out io.Writer) error {
	name := containerName(app.Name)

	if existing, err := w.docker.FindContainer(ctx, name); err != nil {
		return fmt.Errorf("inspect existing container: %w", err)
	} else if existing != nil {
		logf(out, "Removing previous container %s", name)
		if existing.Running() {
			_ = w.docker.StopContainer(ctx, existing.ID, stopTimeout)
		}
		if err := w.docker.RemoveContainer(ctx, existing.ID, true); err != nil {
			return fmt.Errorf("remove previous container: %w", err)
		}
	}

	spec := docker.ContainerSpec{
		Name:          name,
		Image:         image,
		Labels:        traefik.Labels(app, AppPort),
		Env:           env,
		Networks:      []string{w.cfg.Network},
		RestartPolicy: "unless-stopped",
	}
	id, err := w.docker.CreateContainer(ctx, spec)
	if err != nil {
		return fmt.Errorf("create container: %w", err)
	}
	if err := w.docker.StartContainer(ctx, id); err != nil {
		return fmt.Errorf("start container: %w", err)
	}
	return nil
}

// fail records a terminal failure with a guarded transition. If the guard does
// not apply (the row already moved on, e.g. an operator cancelled it), the
// failure is not forced over the existing terminal state.
func (w *Worker) fail(dep core.Deployment, from core.DeployStatus, reason string, out io.Writer) {
	logf(out, "ERROR: %s", reason)
	ok, err := w.store.SetStatus(context.Background(), dep.ID, from, core.StatusFailed, reason)
	if err != nil {
		logf(out, "WARNING: could not record failure: %v", err)
		return
	}
	if !ok {
		logf(out, "(deployment already advanced past %s; leaving its status unchanged)", from)
	}
}

// containerName is the deterministic name of an app's running container.
// App names are validated on creation to be safe as container/label
// identifiers, so no escaping is needed here.
func containerName(appName string) string {
	return "slipway-app-" + appName
}
