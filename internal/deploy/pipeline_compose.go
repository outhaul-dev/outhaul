package deploy

import (
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/outhaul-dev/outhaul/internal/compose"
	"github.com/outhaul-dev/outhaul/internal/core"
)

// runComposePipeline drives a compose-kind deployment: clone (or, for
// template apps, write the stored compose snapshot), write .env and the
// domain override next to the compose file, `compose build` (building), then
// `compose up -d --wait` (deploying) with --wait as the health gate.
//
// Unlike the nixpacks path there is no blue-green cutover: compose recreates
// changed containers in place, so a failed up can leave the stack partially
// replaced. That divergence is deliberate — double-running a whole stack
// (volumes, published ports, scaling) has no safe generic answer.
func (w *Worker) runComposePipeline(ctx context.Context, dep core.Deployment, app core.App, out io.Writer) {
	logf(out, "Deploying compose stack %s (%s)", app.Name, composeSource(app))

	envVars, err := w.loadEnv(ctx, app)
	if err != nil {
		w.fail(dep, core.StatusBuilding, err.Error(), out)
		return
	}
	domains, err := w.store.ListDomains(ctx, app.ID)
	if err != nil {
		w.fail(dep, core.StatusBuilding, "load domains: "+err.Error(), out)
		return
	}

	workDir, cleanup, err := w.composeWorkDir(ctx, dep, app, out)
	if err != nil {
		w.fail(dep, core.StatusBuilding, err.Error(), out)
		return
	}
	defer cleanup()

	composeFile := filepath.Join(workDir, filepath.FromSlash(app.ComposePath))
	if _, err := os.Stat(composeFile); err != nil {
		w.fail(dep, core.StatusBuilding,
			fmt.Sprintf("compose file %q not found on branch %q", app.ComposePath, app.Branch), out)
		return
	}
	stackDir := filepath.Dir(composeFile)

	// All env vars (secrets included) go into .env beside the compose file,
	// where compose reads them for ${VAR} interpolation — Dokploy's layout.
	// The file lives only inside this deploy's work dir.
	if err := os.WriteFile(filepath.Join(stackDir, ".env"), envFile(envVars), 0o600); err != nil {
		w.fail(dep, core.StatusBuilding, "write .env: "+err.Error(), out)
		return
	}

	// files are work-dir-relative (the runner runs in workDir); the override
	// sits beside the user's compose file and layers domain exposure over it.
	files := []string{app.ComposePath}
	if len(domains) > 0 {
		ov := compose.Override(app, domains, w.cfg.Network, w.effectiveTLS(ctx))
		if err := os.WriteFile(filepath.Join(stackDir, compose.OverrideFile), ov, 0o644); err != nil {
			w.fail(dep, core.StatusBuilding, "write override: "+err.Error(), out)
			return
		}
		files = append(files, path.Join(path.Dir(app.ComposePath), compose.OverrideFile))
	}

	project := compose.ProjectName(app.Name)
	logf(out, "Building stack %s...", project)
	if err := w.compose.Build(ctx, workDir, files, project, out); err != nil {
		w.fail(dep, core.StatusBuilding, "build failed: "+err.Error(), out)
		return
	}

	// --- building -> deploying (the cancel window closes here) ---
	ok, err := w.store.SetStatus(context.Background(), dep.ID, core.StatusBuilding, core.StatusDeploying, "")
	if err != nil {
		w.fail(dep, core.StatusBuilding, "advance to deploying: "+err.Error(), out)
		return
	}
	if !ok {
		logf(out, "Deployment is no longer building (cancelled); aborting.")
		return
	}

	// The app may have been deleted while we were building (a deploying
	// deployment is not cancellable); don't bring up a stack for a gone app.
	if _, err := w.store.GetApp(ctx, app.ID); err != nil {
		w.fail(dep, core.StatusDeploying, "app was deleted during deploy", out)
		return
	}

	logf(out, "Starting stack and waiting for services to become ready...")
	if err := w.compose.Up(ctx, workDir, files, project, w.cfg.HealthTimeout, out); err != nil {
		w.fail(dep, core.StatusDeploying, "stack failed to become ready: "+err.Error(), out)
		return
	}

	// --- deploying -> running ---
	if _, err := w.store.SetStatus(context.Background(), dep.ID, core.StatusDeploying, core.StatusRunning, ""); err != nil {
		logf(out, "WARNING: could not record running status: %v", err)
	}
	// Retire the rows of the deploys this stack replaced (bookkeeping only).
	if _, err := w.store.SupersedeOthers(context.Background(), app.ID, dep.ID); err != nil {
		logf(out, "WARNING: could not retire superseded deployments: %v", err)
	}
	if len(domains) > 0 {
		hosts := make([]string, len(domains))
		for i, d := range domains {
			hosts[i] = "http://" + d.Host
		}
		logf(out, "Done. %s is live at %s", app.Name, strings.Join(hosts, ", "))
	} else {
		logf(out, "Done. Stack %s is up.", project)
	}
}

// composeWorkDir obtains the work dir holding the stack's compose file:
// cloned from the app's repo, or — for template apps, which have no repo —
// created fresh with the app's compose snapshot written into it.
func (w *Worker) composeWorkDir(ctx context.Context, dep core.Deployment, app core.App, out io.Writer) (string, func(), error) {
	if app.ComposeRaw == "" {
		return w.cloneWorkDir(ctx, dep, app, out)
	}
	workDir := filepath.Join(w.cfg.WorkDir(), fmt.Sprintf("dep-%d", dep.ID))
	if err := os.RemoveAll(workDir); err != nil {
		return "", nil, fmt.Errorf("prepare work dir: %w", err)
	}
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return "", nil, fmt.Errorf("prepare work dir: %w", err)
	}
	logf(out, "Writing template compose file (%s)...", app.TemplateID)
	target := filepath.Join(workDir, filepath.FromSlash(app.ComposePath))
	if err := os.WriteFile(target, []byte(app.ComposeRaw), 0o644); err != nil {
		os.RemoveAll(workDir)
		return "", nil, fmt.Errorf("write compose file: %w", err)
	}
	return workDir, func() { os.RemoveAll(workDir) }, nil
}

// composeSource describes where the stack definition comes from, for logs.
func composeSource(app core.App) string {
	if app.TemplateID != "" {
		return "template " + app.TemplateID
	}
	return app.RepoURL
}

// envFile renders env vars in dotenv format. Values are double-quoted with
// backslash, quote, and newline escapes — the syntax compose's dotenv parser
// understands — so arbitrary values survive the round-trip.
func envFile(vars []core.EnvVar) []byte {
	var b strings.Builder
	b.WriteString("# Generated by Outhaul on every deploy — do not edit.\n")
	for _, v := range vars {
		b.WriteString(v.Key)
		b.WriteByte('=')
		b.WriteString(quoteEnv(v.Value))
		b.WriteByte('\n')
	}
	return []byte(b.String())
}

func quoteEnv(v string) string {
	v = strings.ReplaceAll(v, `\`, `\\`)
	v = strings.ReplaceAll(v, `"`, `\"`)
	v = strings.ReplaceAll(v, "\n", `\n`)
	return `"` + v + `"`
}
