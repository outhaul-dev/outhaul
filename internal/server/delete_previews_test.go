package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/outhaul-dev/outhaul/internal/core"
	"github.com/outhaul-dev/outhaul/internal/store"
	"github.com/outhaul-dev/outhaul/internal/webhook"
)

// destroyingPreviews stands in for the preview manager's teardown: it records
// each DestroyByID and, unless told to fail, deletes the child row as the real
// teardown does after removing its containers and databases.
type destroyingPreviews struct {
	st        *store.Store
	destroyed []int64 // child ids, in call order
	parentIDs []int64
	fail      error
	onDestroy func() // runs at the start of each DestroyByID
}

func (p *destroyingPreviews) Enqueue(int64, webhook.PullRequestEvent) error { return nil }

func (p *destroyingPreviews) DestroyByID(ctx context.Context, parentID, childID int64) error {
	if p.onDestroy != nil {
		p.onDestroy()
	}
	p.parentIDs = append(p.parentIDs, parentID)
	p.destroyed = append(p.destroyed, childID)
	if err := ctx.Err(); err != nil {
		return err // like the real teardown's Docker and DB calls
	}
	if p.fail != nil {
		return p.fail
	}
	return p.st.DeleteApp(ctx, childID)
}

func seedParentWithPreview(t *testing.T, e *testEnv) (parent, child core.App) {
	t.Helper()
	ctx := context.Background()
	parent, err := e.store.CreateApp(ctx, core.App{
		Name: "web", Source: core.SourceGithub, GithubRepo: "me/app", Kind: core.KindNixpacks, Branch: "main",
	})
	if err != nil {
		t.Fatalf("create parent: %v", err)
	}
	child, err = e.store.CreateApp(ctx, core.App{
		Name: core.PreviewAppName("web", 7), Source: core.SourceGithub, GithubRepo: "me/app", Kind: core.KindNixpacks,
		Branch: "feature", ParentID: parent.ID, PRNumber: 7, Ephemeral: true, PreviewStatus: core.PreviewReady,
	})
	if err != nil {
		t.Fatalf("create child: %v", err)
	}
	return parent, child
}

// A preview carries copies of the parent's secrets and keeps serving until torn
// down; once the parent row is gone nothing would ever reap it, and recreating
// the parent would collide with the leftover web-pr-N name.
func TestDeleteAppTearsDownItsPreviewsFirst(t *testing.T) {
	e := newTestEnv(t)
	e.completeSetup(t)
	parent, child := seedParentWithPreview(t, e)
	prev := &destroyingPreviews{st: e.store}
	e.srv.previews = prev

	resp := e.postForm(t, "/apps/"+itoa(parent.ID)+"/delete", url.Values{})
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("delete = %d, want 303", resp.StatusCode)
	}
	if len(prev.destroyed) != 1 || prev.destroyed[0] != child.ID || prev.parentIDs[0] != parent.ID {
		t.Fatalf("DestroyByID calls = parents %v children %v, want (%d, %d)", prev.parentIDs, prev.destroyed, parent.ID, child.ID)
	}
	if _, err := e.store.GetApp(context.Background(), parent.ID); err == nil {
		t.Error("parent row not deleted")
	}
}

func TestDeleteAppKeepsParentWhenPreviewTeardownFails(t *testing.T) {
	e := newTestEnv(t)
	e.completeSetup(t)
	parent, child := seedParentWithPreview(t, e)
	e.srv.previews = &destroyingPreviews{st: e.store, fail: errors.New("docker unavailable")}

	resp := e.postForm(t, "/apps/"+itoa(parent.ID)+"/delete", url.Values{})
	resp.Body.Close()
	if resp.StatusCode == http.StatusSeeOther {
		t.Fatal("delete succeeded although a preview could not be torn down")
	}
	if _, err := e.store.GetApp(context.Background(), parent.ID); err != nil {
		t.Errorf("parent row deleted despite failed preview teardown: %v", err)
	}
	if _, err := e.store.GetApp(context.Background(), child.ID); err != nil {
		t.Errorf("child row gone: %v", err)
	}
	if len(e.runtime.removed) != 0 {
		t.Errorf("parent container torn down before the delete was known to proceed: %v", e.runtime.removed)
	}
}

func TestDeleteAppRefusesWhenPreviewsCannotBeTornDown(t *testing.T) {
	e := newTestEnv(t)
	e.completeSetup(t)
	parent, _ := seedParentWithPreview(t, e)
	// e.srv.previews is nil: no manager to tear the child down with.

	resp := e.postForm(t, "/apps/"+itoa(parent.ID)+"/delete", url.Values{})
	resp.Body.Close()
	if resp.StatusCode == http.StatusSeeOther {
		t.Fatal("delete orphaned a preview: no preview manager, yet the parent was deleted")
	}
	if _, err := e.store.GetApp(context.Background(), parent.ID); err != nil {
		t.Errorf("parent row deleted: %v", err)
	}
}

// Deleting an app is several destructive steps. A browser that gives up mid-way
// (closed tab, navigation) cancelled the request context and stopped the
// delete half-done — a preview's database could be left behind. Once started,
// the delete runs to completion.
func TestDeleteAppCompletesWhenTheRequestIsCancelled(t *testing.T) {
	e := newTestEnv(t)
	e.completeSetup(t)
	parent, child := seedParentWithPreview(t, e)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.srv.previews = &destroyingPreviews{st: e.store, onDestroy: cancel}

	req := httptest.NewRequest("POST", "/apps/"+itoa(parent.ID)+"/delete", nil).WithContext(ctx)
	base, _ := url.Parse(e.http.URL)
	for _, c := range e.client.Jar.Cookies(base) {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rec, req)

	if _, err := e.store.GetApp(context.Background(), child.ID); err == nil {
		t.Error("preview survived a delete whose request was cancelled mid-way")
	}
	if _, err := e.store.GetApp(context.Background(), parent.ID); err == nil {
		t.Errorf("parent survived (status %d)", rec.Code)
	}
}
