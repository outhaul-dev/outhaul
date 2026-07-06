package server

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/james-smart/outhaul/internal/core"
)

func TestAddVolumeToApp(t *testing.T) {
	e := newTestEnv(t)
	e.completeSetup(t)
	app, _ := e.store.CreateApp(context.Background(), core.App{Name: "web", RepoURL: "https://x/y.git", Domain: "web.test"})

	resp := e.postForm(t, "/apps/"+itoa(app.ID)+"/volumes", url.Values{"mount_path": {"/data"}})
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("add volume: status %d", resp.StatusCode)
	}
	vols, _ := e.store.ListVolumes(context.Background(), app.ID)
	if len(vols) != 1 || vols[0].MountPath != "/data" {
		t.Fatalf("volume not stored: %+v", vols)
	}
}

func TestAddVolumeRejectsRelativePath(t *testing.T) {
	e := newTestEnv(t)
	e.completeSetup(t)
	app, _ := e.store.CreateApp(context.Background(), core.App{Name: "web", RepoURL: "https://x/y.git", Domain: "web.test"})

	resp := e.postForm(t, "/apps/"+itoa(app.ID)+"/volumes", url.Values{"mount_path": {"data/../etc"}})
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for a relative/unsafe path, got %d", resp.StatusCode)
	}
	if vols, _ := e.store.ListVolumes(context.Background(), app.ID); len(vols) != 0 {
		t.Fatalf("no volume should be stored on a rejected path: %+v", vols)
	}
}

func TestAddVolumeRejectsTraversalOnAbsolutePath(t *testing.T) {
	e := newTestEnv(t)
	e.completeSetup(t)
	app, _ := e.store.CreateApp(context.Background(), core.App{Name: "web", RepoURL: "https://x/y.git", Domain: "web.test"})

	// This path PASSES mountPathRe (leading slash, allowed chars) so it exercises
	// the strings.Contains(mountPath, "..") backstop directly.
	resp := e.postForm(t, "/apps/"+itoa(app.ID)+"/volumes", url.Values{"mount_path": {"/data/../etc"}})
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for a traversal path, got %d", resp.StatusCode)
	}
	if vols, _ := e.store.ListVolumes(context.Background(), app.ID); len(vols) != 0 {
		t.Fatalf("no volume should be stored on a rejected path: %+v", vols)
	}
}

func TestAddVolumeRejectsForComposeApp(t *testing.T) {
	e := newTestEnv(t)
	e.completeSetup(t)
	app, _ := e.store.CreateApp(context.Background(), core.App{Name: "shop", RepoURL: "https://x/y.git", Kind: core.KindCompose, ComposePath: "docker-compose.yml"})

	resp := e.postForm(t, "/apps/"+itoa(app.ID)+"/volumes", url.Values{"mount_path": {"/data"}})
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for a compose app, got %d", resp.StatusCode)
	}
}

func TestUpdateAndDeleteVolume(t *testing.T) {
	e := newTestEnv(t)
	e.completeSetup(t)
	app, _ := e.store.CreateApp(context.Background(), core.App{Name: "web", RepoURL: "https://x/y.git", Domain: "web.test"})
	v, err := e.store.AddVolume(context.Background(), app.ID, "/data")
	if err != nil {
		t.Fatalf("AddVolume: %v", err)
	}

	resp := e.postForm(t, "/apps/"+itoa(app.ID)+"/volumes/"+itoa(v.ID), url.Values{"mount_path": {"/data2"}})
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("update: status %d", resp.StatusCode)
	}
	got, _ := e.store.GetVolume(context.Background(), app.ID, v.ID)
	if got.MountPath != "/data2" {
		t.Fatalf("mount path not updated: %+v", got)
	}

	resp = e.postForm(t, "/apps/"+itoa(app.ID)+"/volumes/"+itoa(v.ID)+"/delete", url.Values{})
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("delete: status %d", resp.StatusCode)
	}
	if vols, _ := e.store.ListVolumes(context.Background(), app.ID); len(vols) != 0 {
		t.Fatalf("volume not detached: %+v", vols)
	}
}

func TestUpdateNonexistentVolume404(t *testing.T) {
	e := newTestEnv(t)
	e.completeSetup(t)
	app, _ := e.store.CreateApp(context.Background(), core.App{Name: "web", RepoURL: "https://x/y.git", Domain: "web.test"})

	resp := e.postForm(t, "/apps/"+itoa(app.ID)+"/volumes/9999", url.Values{"mount_path": {"/data"}})
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("update of missing volume = %d, want 404", resp.StatusCode)
	}
}

func TestAppPageShowsVolumesSectionForNixpacks(t *testing.T) {
	e := newTestEnv(t)
	e.completeSetup(t)
	app, _ := e.store.CreateApp(context.Background(), core.App{Name: "web", RepoURL: "https://x/y.git", Domain: "web.test"})

	page := body(t, e.get(t, "/apps/"+itoa(app.ID)))
	if !strings.Contains(page, "openVolumeDialog") {
		t.Error("app page missing the volumes wizard trigger")
	}
}

func TestAppPageHidesVolumesSectionForCompose(t *testing.T) {
	e := newTestEnv(t)
	e.completeSetup(t)
	app, _ := e.store.CreateApp(context.Background(), core.App{Name: "shop", RepoURL: "https://x/y.git", Kind: core.KindCompose, ComposePath: "docker-compose.yml"})

	page := body(t, e.get(t, "/apps/"+itoa(app.ID)))
	if strings.Contains(page, "openVolumeDialog") {
		t.Error("compose app page should not show the volumes section")
	}
}
