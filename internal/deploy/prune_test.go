package deploy

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/outhaul-dev/outhaul/internal/core"
)

// recordingPruner captures PruneApp invocations and can write to the deploy log.
type recordingPruner struct {
	apps []int64
	err  error
}

func (p *recordingPruner) PruneApp(_ context.Context, app core.App, out io.Writer) error {
	p.apps = append(p.apps, app.ID)
	if p.err != nil {
		return p.err
	}
	io.WriteString(out, "Pruned old image outhaul/x:1\n")
	return nil
}

// deployLogs subscribes to a deployment's log stream and returns everything
// published once the pipeline closes it.
func (h *harness) deployLogs(dep core.Deployment, run func()) []string {
	history, ch, cancel := h.broker.Subscribe(dep.ID)
	defer cancel()
	run()
	lines := append([]string{}, history...)
	for s := range ch { // closed by the pipeline's deferred broker.Close
		lines = append(lines, s)
	}
	return lines
}

func TestSuccessfulDeployTriggersPruner(t *testing.T) {
	h := newHarness(t)
	pruner := &recordingPruner{}
	h.worker.SetPruner(pruner)
	app := h.app(t, "web")
	dep := h.claimedDeployment(t, app.ID)

	lines := h.deployLogs(dep, func() { h.worker.runPipeline(context.Background(), dep) })

	if got := h.status(t, dep.ID).Status; got != core.StatusRunning {
		t.Fatalf("status = %q, want running", got)
	}
	if len(pruner.apps) != 1 || pruner.apps[0] != app.ID {
		t.Errorf("pruner calls = %v, want exactly [%d]", pruner.apps, app.ID)
	}
	if !containsPrefix(lines, "Pruned old image") {
		t.Errorf("prune activity should appear in the deploy log; got %v", lines)
	}
}

func TestFailedDeployDoesNotPrune(t *testing.T) {
	h := newHarness(t)
	pruner := &recordingPruner{}
	h.worker.SetPruner(pruner)
	h.builder.err = errors.New("boom")
	app := h.app(t, "web")
	dep := h.claimedDeployment(t, app.ID)

	h.worker.runPipeline(context.Background(), dep)

	if got := h.status(t, dep.ID).Status; got != core.StatusFailed {
		t.Fatalf("status = %q, want failed", got)
	}
	if len(pruner.apps) != 0 {
		t.Errorf("a failed deploy must not prune; calls = %v", pruner.apps)
	}
}

func TestPrunerFailureDoesNotFailDeploy(t *testing.T) {
	h := newHarness(t)
	pruner := &recordingPruner{err: errors.New("daemon hiccup")}
	h.worker.SetPruner(pruner)
	app := h.app(t, "web")
	dep := h.claimedDeployment(t, app.ID)

	lines := h.deployLogs(dep, func() { h.worker.runPipeline(context.Background(), dep) })

	if got := h.status(t, dep.ID).Status; got != core.StatusRunning {
		t.Fatalf("status = %q, want running (pruning is best-effort)", got)
	}
	if !containsPrefix(lines, "WARNING: pruning old images failed") {
		t.Errorf("the prune failure should be visible in the deploy log; got %v", lines)
	}
}
