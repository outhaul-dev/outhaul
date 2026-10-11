package deploy

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/outhaul-dev/outhaul/internal/core"
)

// forkPreview creates the child app a fork PR's preview runs as: ephemeral and
// building the base repo's refs/pull/<n>/head.
func forkPreview(t *testing.T, h *harness) core.App {
	t.Helper()
	app, err := h.store.CreateApp(context.Background(), core.App{
		Name: "web-pr-3", RepoURL: "https://example.com/web.git", Domain: "web-pr-3.test",
		Ephemeral: true, ParentID: 1, PRNumber: 3, Branch: core.ForkPreviewRef(3),
	})
	if err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	return app
}

// Docker copies an image's labels onto its containers, and Traefik reads
// container labels. A fork's image (e.g. built FROM an image of the fork
// author's choosing) carrying traefik.* labels could claim any host on the
// server, so a fork build with such labels never runs.
func TestForkPreviewImageWithTraefikLabelsIsRefused(t *testing.T) {
	h := newHarness(t)
	app := forkPreview(t, h)
	dep := h.claimedDeployment(t, app.ID)
	h.docker.ImageLabelsByRef = map[string]map[string]string{
		fmt.Sprintf("outhaul/%s:%d", app.Name, dep.ID): {"traefik.http.routers.evil.rule": "Host(`shop.example.com`)"},
	}

	h.worker.runPipeline(context.Background(), dep)

	got := h.status(t, dep.ID)
	if got.Status != core.StatusFailed || !strings.Contains(got.Reason, "traefik") {
		t.Fatalf("status = %q (%q), want failed naming the traefik labels", got.Status, got.Reason)
	}
	for _, spec := range h.docker.Created {
		if spec.Name == "outhaul-app-"+app.Name || strings.HasPrefix(spec.Name, "outhaul-deploy-") {
			t.Errorf("a container was created from the refused image: %s", spec.Name)
		}
	}
}
