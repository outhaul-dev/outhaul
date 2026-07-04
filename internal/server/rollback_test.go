package server

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/james-smart/outhaul/internal/core"
)

// seedFinishedDeploy walks a deployment through the real state machine to
// running, with a built image recorded — the shape a rollback reuses.
func seedFinishedDeploy(t *testing.T, e *testEnv, appID int64, image string) core.Deployment {
	t.Helper()
	ctx := context.Background()
	d, err := e.store.CreateDeployment(ctx, appID)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := e.store.ClaimDeployment(ctx, d.ID); err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	if err := e.store.SetImage(ctx, d.ID, image); err != nil {
		t.Fatal(err)
	}
	mustTransition(t, e, d.ID, core.StatusBuilding, core.StatusDeploying)
	mustTransition(t, e, d.ID, core.StatusDeploying, core.StatusRunning)
	got, err := e.store.GetDeployment(ctx, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func mustTransition(t *testing.T, e *testEnv, id int64, from, to core.DeployStatus) {
	t.Helper()
	if ok, err := e.store.SetStatus(context.Background(), id, from, to, ""); err != nil || !ok {
		t.Fatalf("transition %s -> %s: ok=%v err=%v", from, to, ok, err)
	}
}

func TestRollbackEnqueuesReuseOfImage(t *testing.T) {
	e := newTestEnv(t)
	e.login(t)
	app := seedRunningApp(t, e, "")
	src := seedFinishedDeploy(t, e, app.ID, "outhaul/web:1")

	resp := e.postForm(t, "/deployments/"+itoa(src.ID)+"/rollback", url.Values{})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", resp.StatusCode)
	}

	loc := resp.Header.Get("Location")
	if !strings.HasPrefix(loc, "/deployments/") {
		t.Fatalf("redirect = %q, want the new deployment's page", loc)
	}
	newID, ok := parseID(strings.TrimPrefix(loc, "/deployments/"))
	if !ok {
		t.Fatalf("could not parse new deployment ID from %q", loc)
	}
	if newID == src.ID {
		t.Fatal("rollback should create a NEW deployment, not reuse the source row")
	}

	dep, err := e.store.GetDeployment(context.Background(), newID)
	if err != nil {
		t.Fatal(err)
	}
	if dep.Status != core.StatusQueued {
		t.Errorf("status = %q, want queued", dep.Status)
	}
	if dep.Image != "outhaul/web:1" || dep.RollbackOf != src.ID {
		t.Errorf("rollback row = image %q rollback_of %d, want outhaul/web:1 / %d",
			dep.Image, dep.RollbackOf, src.ID)
	}
	if e.deployer.notified == 0 {
		t.Error("worker was not notified of the queued rollback")
	}
}

func TestRollbackRejectsCompose(t *testing.T) {
	e := newTestEnv(t)
	e.completeSetup(t)
	app := seedComposeStack(t, e)
	// Even with an image crafted onto the row, compose stacks can't roll back.
	src := seedFinishedDeploy(t, e, app.ID, "someimage:1")

	resp := e.postForm(t, "/deployments/"+itoa(src.ID)+"/rollback", url.Values{})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if got := body(t, resp); !strings.Contains(got, "compose") {
		t.Errorf("error should explain the compose limitation, got %q", got)
	}
}

func TestRollbackRejectsUnbuiltDeployment(t *testing.T) {
	e := newTestEnv(t)
	e.login(t)
	app := seedRunningApp(t, e, "")
	// A failed clone/build leaves no image behind.
	d, err := e.store.CreateDeployment(context.Background(), app.ID)
	if err != nil {
		t.Fatal(err)
	}

	resp := e.postForm(t, "/deployments/"+itoa(d.ID)+"/rollback", url.Values{})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if got := body(t, resp); !strings.Contains(got, "image") {
		t.Errorf("error should mention the missing image, got %q", got)
	}
}

func TestRollbackRejectsPrunedImage(t *testing.T) {
	e := newTestEnv(t)
	e.login(t)
	app := seedRunningApp(t, e, "")
	src := seedFinishedDeploy(t, e, app.ID, "outhaul/web:1")
	if err := e.store.MarkImagePruned(context.Background(), src.Image); err != nil {
		t.Fatal(err)
	}

	resp := e.postForm(t, "/deployments/"+itoa(src.ID)+"/rollback", url.Values{})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if got := body(t, resp); !strings.Contains(got, "pruned") {
		t.Errorf("error should explain the image was pruned, got %q", got)
	}
}

func TestPrunedDeploymentHidesRollbackButton(t *testing.T) {
	e := newTestEnv(t)
	e.login(t)
	app := seedRunningApp(t, e, "")
	src := seedFinishedDeploy(t, e, app.ID, "outhaul/web:1")
	if err := e.store.MarkImagePruned(context.Background(), src.Image); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{"/apps/" + itoa(app.ID), "/deployments/" + itoa(src.ID)} {
		page := body(t, e.get(t, path))
		if strings.Contains(page, "/deployments/"+itoa(src.ID)+"/rollback") {
			t.Errorf("%s: pruned deployment must not offer a Rollback action", path)
		}
		if !strings.Contains(page, "image pruned") {
			t.Errorf("%s: pruned deployment should say why rollback is gone", path)
		}
	}
}

func TestRollbackUnknownDeployment404(t *testing.T) {
	e := newTestEnv(t)
	e.login(t)
	resp := e.postForm(t, "/deployments/9999/rollback", url.Values{})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func TestAppPageShowsRollbackButtonAndProvenance(t *testing.T) {
	e := newTestEnv(t)
	e.login(t)
	app := seedRunningApp(t, e, "")
	src := seedFinishedDeploy(t, e, app.ID, "outhaul/web:1")
	if _, err := e.store.CreateRollback(context.Background(), app.ID, src.Image, src.ID); err != nil {
		t.Fatal(err)
	}

	page := body(t, e.get(t, "/apps/"+itoa(app.ID)))
	if !strings.Contains(page, "/deployments/"+itoa(src.ID)+"/rollback") {
		t.Error("finished deployment row should offer a Rollback action")
	}
	if !strings.Contains(page, "↩ #"+itoa(src.ID)) {
		t.Error("rollback row should be labelled with the deployment it reuses")
	}
}

func TestDeploymentPageShowsRollbackButton(t *testing.T) {
	e := newTestEnv(t)
	e.login(t)
	app := seedRunningApp(t, e, "")
	src := seedFinishedDeploy(t, e, app.ID, "outhaul/web:1")

	page := body(t, e.get(t, "/deployments/"+itoa(src.ID)))
	if !strings.Contains(page, "/deployments/"+itoa(src.ID)+"/rollback") {
		t.Error("finished deployment page should offer a Rollback action")
	}

	// A queued deployment (no image, cancellable) must not offer one.
	d, err := e.store.CreateDeployment(context.Background(), app.ID)
	if err != nil {
		t.Fatal(err)
	}
	page = body(t, e.get(t, "/deployments/"+itoa(d.ID)))
	if strings.Contains(page, "/deployments/"+itoa(d.ID)+"/rollback") {
		t.Error("queued deployment page should not offer a Rollback action")
	}
}
