package deploy

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
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

// Nixpacks reads the checkout on the host, as Outhaul's user. A fork can commit
// symlinks; one resolving outside the checkout could pull host files (the
// secret key, other apps' data) into the fork's image.
func TestSymlinkEscape(t *testing.T) {
	cases := map[string]map[string]string{
		"absolute target":       {"leak": "/etc/hostname"},
		"relative escape":       {"leak": "../../etc/hostname"},
		"escape via a dir link": {"a": ".", "b": "a/.."},
	}
	for name, links := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			for l, target := range links {
				if err := os.Symlink(target, filepath.Join(dir, l)); err != nil {
					t.Fatal(err)
				}
			}
			if err := symlinkEscape(dir); err == nil {
				t.Errorf("links %v were accepted", links)
			}
		})
	}

	ok := t.TempDir()
	os.MkdirAll(filepath.Join(ok, "src"), 0o755)
	os.WriteFile(filepath.Join(ok, "src", "main.go"), []byte("package main"), 0o644)
	os.Symlink("src/main.go", filepath.Join(ok, "entry.go"))
	os.Symlink(".", filepath.Join(ok, "self"))
	if err := symlinkEscape(ok); err != nil {
		t.Errorf("links that stay inside the checkout were refused: %v", err)
	}
}

func TestForkPreviewCheckoutWithEscapingSymlinkIsRefused(t *testing.T) {
	h := newHarness(t)
	app := forkPreview(t, h)
	h.cloner.links = map[string]string{"leak": "/etc/hostname"}
	dep := h.claimedDeployment(t, app.ID)

	h.worker.runPipeline(context.Background(), dep)

	got := h.status(t, dep.ID)
	if got.Status != core.StatusFailed || !strings.Contains(got.Reason, "symlink") {
		t.Fatalf("status = %q (%q), want failed naming the symlink", got.Status, got.Reason)
	}
	if h.builder.lastReq.ImageTag != "" {
		t.Error("the checkout was built despite the escaping symlink")
	}
}

// Nixpacks config can name the base images (buildImage, runImage). A fork could
// name the parent's local production image (outhaul/<app>:<id>) and read its
// code and baked-in build env, so a fork preview may not choose its images.
func TestForkPreviewNixpacksImageOverrideIsRefused(t *testing.T) {
	for file, content := range map[string]string{
		"nixpacks.toml": "[start]\ncmd = \"./serve\"\nrunImage = \"outhaul/web:1\"\n",
		"nixpacks.json": `{"buildImage": "outhaul/web:1"}`,
	} {
		t.Run(file, func(t *testing.T) {
			h := newHarness(t)
			app := forkPreview(t, h)
			h.cloner.files = map[string]string{file: content}
			dep := h.claimedDeployment(t, app.ID)

			h.worker.runPipeline(context.Background(), dep)

			got := h.status(t, dep.ID)
			if got.Status != core.StatusFailed || !strings.Contains(got.Reason, "Image") {
				t.Fatalf("status = %q (%q), want failed naming the image override", got.Status, got.Reason)
			}
			if h.builder.lastReq.ImageTag != "" {
				t.Error("the checkout was built despite the image override")
			}
		})
	}
}

func TestForkPreviewPlainNixpacksConfigIsBuilt(t *testing.T) {
	h := newHarness(t)
	app := forkPreview(t, h)
	h.cloner.files = map[string]string{"nixpacks.toml": "[phases.build]\ncmds = [\"make\"]\n\n[start]\ncmd = \"./serve\"\n"}
	dep := h.claimedDeployment(t, app.ID)

	h.worker.runPipeline(context.Background(), dep)

	if got := h.status(t, dep.ID); got.Status != core.StatusRunning {
		t.Fatalf("status = %q (%q), want running", got.Status, got.Reason)
	}
}
