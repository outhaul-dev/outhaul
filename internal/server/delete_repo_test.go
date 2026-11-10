package server

import (
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"context"

	"github.com/outhaul-dev/outhaul/internal/core"
	"github.com/outhaul-dev/outhaul/internal/gitrepo"
)

// An app that was deployed by push and later switched to another source still
// has its bare repository. Deleting the app must remove it, or a later fetch
// of that name would be counted as a push to the default project.
func TestDeleteAppRemovesItsPushRepoWhateverItsSourceIsNow(t *testing.T) {
	e := newTestEnv(t)
	e.completeSetup(t)
	repos := gitrepo.New(filepath.Join(t.TempDir(), "git"), "/bin/true", "/tmp/sock")
	e.srv.SetRepos(repos)
	app, err := e.store.CreateApp(context.Background(), core.App{
		Name: "switched", Source: core.SourceGithub, GithubRepo: "me/switched", Branch: "main",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := repos.Init(app.Name); err != nil {
		t.Skipf("git not available: %v", err)
	}
	dir, _ := repos.Path(app.Name)

	resp := e.postForm(t, "/apps/"+itoa(app.ID)+"/delete", url.Values{})
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("delete = %d, want 303", resp.StatusCode)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("bare repository still exists after delete; stat err = %v", err)
	}
}
