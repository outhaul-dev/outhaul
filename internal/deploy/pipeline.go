package deploy

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/james-smart/outhaul/internal/builder"
	"github.com/james-smart/outhaul/internal/core"
	"github.com/james-smart/outhaul/internal/docker"
	"github.com/james-smart/outhaul/internal/github"
	"github.com/james-smart/outhaul/internal/traefik"
)

// AppPort is the internal port Outhaul asks apps to listen on (via $PORT) and
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

	if app.Kind == core.KindCompose {
		w.runComposePipeline(ctx, dep, app, out)
		return
	}

	logf(out, "Deploying %s (%s) to %s", app.Name, app.RepoURL, app.Domain)

	envVars, err := w.loadEnv(ctx, app)
	if err != nil {
		w.fail(dep, core.StatusBuilding, err.Error(), out)
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

	// --- clone + build (skipped for rollbacks, which arrive with an image) ---
	image := dep.Image
	if image == "" {
		workDir, cleanup, err := w.cloneWorkDir(ctx, dep, app, out)
		if err != nil {
			w.fail(dep, core.StatusBuilding, err.Error(), out)
			return
		}
		defer cleanup()

		b := w.builders.Nixpacks
		if app.Kind == core.KindDockerfile {
			b = w.builders.Dockerfile
		}
		image = fmt.Sprintf("outhaul/%s:%d", app.Name, dep.ID)
		logf(out, "Building image %s with %s...", image, b.Name())
		req := builder.BuildRequest{
			ContextDir: workDir,
			ImageTag:   image,
			Env:        buildEnv,
			Dockerfile: app.DockerfilePath,
		}
		if err := b.Build(ctx, req, out); err != nil {
			w.fail(dep, core.StatusBuilding, "build failed: "+err.Error(), out)
			return
		}
		if err := w.store.SetImage(context.Background(), dep.ID, image); err != nil {
			w.fail(dep, core.StatusBuilding, "record image: "+err.Error(), out)
			return
		}
	} else {
		// Env vars, domain, and routing are the app's CURRENT settings — only
		// the image is rolled back (see the rollback spec).
		logf(out, "Rolling back to image %s (built by deployment #%d); nothing to clone or build.", image, dep.RollbackOf)
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

	// Choose a deploy strategy. Apps with a persistent volume cannot overlap two
	// containers on the volume (single writer), so they stop-first (brief
	// downtime). Stateless apps use the zero-downtime blue-green cutover.
	vols, err := w.store.ListVolumes(ctx, app.ID)
	if err != nil {
		w.fail(dep, core.StatusDeploying, "load volumes: "+err.Error(), out)
		return
	}
	if len(vols) > 0 {
		if !w.deployStopFirst(ctx, dep, app, image, runtimeEnv, out) {
			return
		}
	} else {
		if !w.deployBlueGreen(ctx, dep, app, image, runtimeEnv, out) {
			return
		}
	}

	// --- deploying -> running ---
	if _, err := w.store.SetStatus(context.Background(), dep.ID, core.StatusDeploying, core.StatusRunning, ""); err != nil {
		logf(out, "WARNING: could not record running status: %v", err)
	}

	// Retention: trim this app's old images now that a new one shipped. A
	// failure never fails the deploy — the daily sweep retries.
	if w.pruner != nil {
		if err := w.pruner.PruneApp(ctx, app, out); err != nil {
			logf(out, "WARNING: pruning old images failed: %v", err)
		}
	}

	logf(out, "Done. %s is live at http://%s", app.Name, app.Domain)
}

// deployBlueGreen runs the zero-downtime cutover used by stateless apps: start
// a temp container invisible to Traefik, health-check it, then remove the old
// canonical and create the new one. Returns true when the app is live on the
// new container; false means it already recorded a failure.
func (w *Worker) deployBlueGreen(ctx context.Context, dep core.Deployment, app core.App, image string, runtimeEnv []string, out io.Writer) bool {
	// Named on the globally-unique deployment ID (not the app name) so it can
	// never collide with a canonical name: app names permit trailing
	// "-<digits>", so "outhaul-app-<name>-<depID>" could equal the canonical
	// name of an app literally named "<name>-<depID>".
	tempName := fmt.Sprintf("outhaul-deploy-%d", dep.ID)
	logf(out, "Starting new container and waiting for it to become healthy...")
	newID, err := w.createContainer(ctx, app, image, tempName, runtimeEnv, false)
	if err != nil {
		w.fail(dep, core.StatusDeploying, "start failed: "+err.Error(), out)
		return false
	}
	// cleanupContainer removes a container regardless of pipeline cancellation
	// (e.g. graceful shutdown cancels ctx), so temp containers never leak.
	cleanupContainer := func(id string) {
		rmCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = w.docker.RemoveContainer(rmCtx, id, true)
	}
	ip, err := w.docker.ContainerIP(ctx, newID, w.cfg.Network)
	if err != nil || ip == "" {
		cleanupContainer(newID)
		w.fail(dep, core.StatusDeploying, "could not resolve container IP", out)
		return false
	}
	healthURL := fmt.Sprintf("http://%s:%d/", ip, AppPort)
	if !w.healthCheck(ctx, healthURL, w.cfg.HealthTimeout) {
		cleanupContainer(newID)
		w.fail(dep, core.StatusDeploying, "health check failed: app did not respond within the timeout", out)
		return false
	}

	// The app may have been deleted while we were building/health-checking (a
	// deploying deployment is not cancellable). If so, abort before recreating
	// the canonical container, or we'd orphan a container for a gone app.
	if _, err := w.store.GetApp(ctx, app.ID); err != nil {
		cleanupContainer(newID)
		w.fail(dep, core.StatusDeploying, "app was deleted during deploy", out)
		return false
	}

	// --- healthy: cut over. Remove old, create canonical, then drop the temp. ---
	logf(out, "Healthy. Cutting over to the new container...")
	if err := w.removeContainerByName(ctx, containerName(app.Name)); err != nil {
		cleanupContainer(newID)
		w.fail(dep, core.StatusDeploying, "remove previous container: "+err.Error(), out)
		return false
	}
	if _, err := w.createContainer(ctx, app, image, containerName(app.Name), runtimeEnv, true); err != nil {
		cleanupContainer(newID)
		logf(out, "ERROR: cutover failed — the app has NO running container until the next successful deploy")
		w.fail(dep, core.StatusDeploying, "cutover failed (app is down): "+err.Error(), out)
		return false
	}
	cleanupContainer(newID) // canonical is up; drop the temp
	return true
}

// deployStopFirst runs the recreate deploy used by apps with a persistent
// volume: remove the old canonical container, start the new canonical (which
// mounts the volume), then health-check it. There is no overlap — a single
// writer holds the volume at any moment — at the cost of brief downtime.
// Returns true when the app is live; false means it already recorded a failure.
func (w *Worker) deployStopFirst(ctx context.Context, dep core.Deployment, app core.App, image string, runtimeEnv []string, out io.Writer) bool {
	logf(out, "App has persistent volumes; deploying stop-first (brief downtime).")
	if err := w.removeContainerByName(ctx, containerName(app.Name)); err != nil {
		w.fail(dep, core.StatusDeploying, "remove previous container: "+err.Error(), out)
		return false
	}
	// The app may have been deleted while we were building.
	if _, err := w.store.GetApp(ctx, app.ID); err != nil {
		w.fail(dep, core.StatusDeploying, "app was deleted during deploy", out)
		return false
	}
	newID, err := w.createContainer(ctx, app, image, containerName(app.Name), runtimeEnv, true)
	if err != nil {
		logf(out, "ERROR: start failed — the app has NO running container until the next successful deploy")
		w.fail(dep, core.StatusDeploying, "start failed (app is down): "+err.Error(), out)
		return false
	}
	ip, err := w.docker.ContainerIP(ctx, newID, w.cfg.Network)
	if err != nil || ip == "" {
		w.fail(dep, core.StatusDeploying, "could not resolve container IP", out)
		return false
	}
	healthURL := fmt.Sprintf("http://%s:%d/", ip, AppPort)
	if !w.healthCheck(ctx, healthURL, w.cfg.HealthTimeout) {
		w.fail(dep, core.StatusDeploying, "health check failed: app did not respond within the timeout", out)
		return false
	}
	return true
}

// loadEnv loads an app's env vars with ${{project.KEY}} references resolved
// against its project's shared variables. Errors come back ready to be used
// as a failure reason.
func (w *Worker) loadEnv(ctx context.Context, app core.App) ([]core.EnvVar, error) {
	appVars, err := w.store.ListEnv(ctx, app.ID)
	if err != nil {
		return nil, fmt.Errorf("load env: %w", err)
	}
	projectVars, err := w.store.ListProjectEnv(ctx, app.ProjectID)
	if err != nil {
		return nil, fmt.Errorf("load project env: %w", err)
	}
	return core.ResolveEnv(appVars, projectVars)
}

// cloneWorkDir creates the per-deploy work dir and clones the app's repo into
// it. The returned cleanup removes the dir and is non-nil exactly when err is
// nil. Errors come back ready to be used as a failure reason.
func (w *Worker) cloneWorkDir(ctx context.Context, dep core.Deployment, app core.App, out io.Writer) (string, func(), error) {
	workDir := filepath.Join(w.cfg.WorkDir(), fmt.Sprintf("dep-%d", dep.ID))
	if err := os.RemoveAll(workDir); err != nil {
		return "", nil, fmt.Errorf("prepare work dir: %w", err)
	}
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return "", nil, fmt.Errorf("prepare work dir: %w", err)
	}
	cleanup := func() { os.RemoveAll(workDir) }

	logf(out, "Cloning repository (%s)...", app.Source)
	spec, err := w.cloneSpec(ctx, app)
	if err != nil {
		cleanup()
		return "", nil, fmt.Errorf("prepare clone: %w", err)
	}
	if err := w.cloner.Clone(ctx, spec, workDir, out); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("clone failed: %w", err)
	}
	return workDir, cleanup, nil
}

// createContainer creates and starts a container for the app. When traefikOn is
// true it carries the app's routing labels; otherwise Traefik ignores it
// (traefik.enable=false) — used for the health-check phase.
func (w *Worker) createContainer(ctx context.Context, app core.App, image, name string, env []string, traefikOn bool) (string, error) {
	var labels map[string]string
	if traefikOn {
		domains, err := w.store.ListDomains(ctx, app.ID)
		if err != nil {
			return "", fmt.Errorf("load domains: %w", err)
		}
		labels = traefik.AppLabels(app, domains, AppPort, w.cfg.TLSEnabled())
	} else {
		labels = map[string]string{
			"traefik.enable":  "false",
			"outhaul.managed": "true",
			"outhaul.app":     app.Name,
		}
	}
	// Attach the app's persistent volumes (single-container apps only; compose
	// runs its own pipeline). Create each Docker volume idempotently first so
	// it carries Outhaul's labels for the inventory. Volumes survive container
	// replacement, so data persists across deploys.
	var mounts []docker.Mount
	vols, err := w.store.ListVolumes(ctx, app.ID)
	if err != nil {
		return "", fmt.Errorf("load volumes: %w", err)
	}
	for _, v := range vols {
		if err := w.docker.CreateVolume(ctx, v.Name, core.VolumeLabels(app.Name)); err != nil {
			return "", fmt.Errorf("create volume %s: %w", v.Name, err)
		}
		mounts = append(mounts, docker.Mount{Source: v.Name, Target: v.MountPath, Volume: true})
	}

	// Only the canonical (Traefik-routed) container should survive a host
	// reboot or Docker restart. The temp health-check container carries
	// runtime env (including secrets); if Outhaul crashes between create and
	// cleanup, an "unless-stopped" temp container would restart forever since
	// crash recovery only touches DB rows, not orphaned Docker state.
	restart := ""
	if traefikOn {
		restart = "unless-stopped"
	}
	spec := docker.ContainerSpec{
		Name:          name,
		Image:         image,
		Labels:        labels,
		Env:           env,
		Networks:      []string{w.cfg.Network},
		RestartPolicy: restart,
		Mounts:        mounts,
	}
	id, err := w.docker.CreateContainer(ctx, spec)
	if err != nil {
		return "", fmt.Errorf("create container: %w", err)
	}
	if err := w.docker.StartContainer(ctx, id); err != nil {
		return "", fmt.Errorf("start container: %w", err)
	}
	return id, nil
}

// removeContainerByName stops and removes the named container if it exists.
func (w *Worker) removeContainerByName(ctx context.Context, name string) error {
	existing, err := w.docker.FindContainer(ctx, name)
	if err != nil {
		return fmt.Errorf("inspect container: %w", err)
	}
	if existing == nil {
		return nil
	}
	if existing.Running() {
		_ = w.docker.StopContainer(ctx, existing.ID, stopTimeout)
	}
	return w.docker.RemoveContainer(ctx, existing.ID, true)
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

// cloneSpec builds the clone spec for an app, resolving credentials by source.
func (w *Worker) cloneSpec(ctx context.Context, app core.App) (CloneSpec, error) {
	spec := CloneSpec{URL: app.RepoURL, Branch: app.Branch}
	switch app.Source {
	case core.SourceSSH:
		key, err := w.store.SSHPrivateKey(ctx, app.ID)
		if err != nil {
			return CloneSpec{}, fmt.Errorf("load ssh key: %w", err)
		}
		if key == "" {
			return CloneSpec{}, fmt.Errorf("app has no ssh deploy key")
		}
		spec.Auth = Auth{Kind: AuthSSH, SSHKey: key}
	case core.SourceGithub:
		token, err := w.githubToken(ctx)
		if err != nil {
			return CloneSpec{}, err
		}
		spec.URL = "https://github.com/" + app.GithubRepo + ".git"
		spec.Auth = Auth{Kind: AuthToken, Token: token}
	}
	return spec, nil
}

// githubToken mints a fresh installation access token from the configured App.
func (w *Worker) githubToken(ctx context.Context) (string, error) {
	ga, ok, err := w.store.GithubApp(ctx)
	if err != nil {
		return "", fmt.Errorf("load github app: %w", err)
	}
	if !ok {
		return "", fmt.Errorf("github app not configured")
	}
	if ga.InstallationID == 0 {
		return "", fmt.Errorf("github app not installed")
	}
	jwt, err := github.AppJWT(ga.PrivateKey, ga.AppID, time.Now())
	if err != nil {
		return "", fmt.Errorf("build app jwt: %w", err)
	}
	return w.gh.InstallationToken(ctx, jwt, ga.InstallationID)
}

// containerName is the deterministic name of an app's running container.
// App names are validated on creation to be safe as container/label
// identifiers, so no escaping is needed here.
func containerName(appName string) string {
	return "outhaul-app-" + appName
}
