package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/outhaul-dev/outhaul/internal/core"
	"github.com/outhaul-dev/outhaul/internal/update"
)

const (
	testCommit = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	copyInstaller    = "This update includes installer changes. Updating here does not apply them; re-run the installer to get them."
	copyTraefik      = "This update restarts Traefik: every app is unreachable for a few seconds."
	copyConfirm      = "Outhaul restarts, and the admin UI is gone for a minute or so. Apps keep running. GitHub pushes that arrive during the restart are not deployed; redeploy those apps by hand."
	copyNotSetUp     = "Updating from the UI is not set up on this server. Re-run the installer once to set it up."
	copyUnknownBuild = "This build does not record its commit, so it cannot update from the UI."
	copyAhead        = "This server runs a commit main does not have. Updating replaces it with main."
	copyBusy         = "Wait for the running deploy, backup or restore to finish."
	copyHeld         = "An update is in progress. Try again when it finishes."
	copyRunning      = "An update is already running."
	copyInterrupted  = "The updater stopped without reporting a result. On the server, see /var/lib/outhaul-updater/log and journalctl -u outhaul-update."
	copyStuck        = "Outhaul has not come back. On the server: sudo cat /var/lib/outhaul-updater/log"

	updateButtonDisabled = `id="update-button" disabled`
)

var testBuild = update.Build{Commit: testCommit, Repo: "https://github.com/o/r.git"}

// fakeCompare is GitHub's compare API: it answers every request with the
// reply set last, or with code when that is non-zero.
type fakeCompare struct {
	*httptest.Server
	mu    sync.Mutex
	reply []byte
	code  int
	calls int
}

func newFakeCompare(t *testing.T) *fakeCompare {
	t.Helper()
	f := &fakeCompare{}
	f.answer("identical", 0, nil)
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls++
		if f.code != 0 {
			http.Error(w, "nope", f.code)
			return
		}
		w.Write(f.reply)
	}))
	t.Cleanup(f.Close)
	return f
}

// answer sets the reply: GitHub's status, ahead_by, the commits oldest first
// as {sha, message} pairs, and the changed files.
func (f *fakeCompare) answer(status string, aheadBy int, commits [][2]string, files ...string) {
	cs := []map[string]any{}
	for i, c := range commits {
		cs = append(cs, map[string]any{"sha": c[0], "commit": map[string]any{
			"message":   c[1],
			"committer": map[string]any{"date": time.Date(2026, 10, 1+i, 9, 0, 0, 0, time.UTC)},
		}})
	}
	fs := []map[string]any{}
	for _, name := range files {
		fs = append(fs, map[string]any{"filename": name})
	}
	b, _ := json.Marshal(map[string]any{
		"status": status, "ahead_by": aheadBy, "commits": cs, "files": fs,
		"html_url": "https://github.com/o/r/compare/" + testCommit + "...main",
	})
	f.mu.Lock()
	f.reply, f.code = b, 0
	f.mu.Unlock()
}

func (f *fakeCompare) fail(code int) {
	f.mu.Lock()
	f.code = code
	f.mu.Unlock()
}

func (f *fakeCompare) requests() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// updatesEnv is a test server with the Updates panel wired to a temp update
// directory and a fake GitHub.
type updatesEnv struct {
	*testEnv
	dir string
	gh  *fakeCompare
}

// newUpdatesEnv builds the env for build b; installed writes the updater
// marker the installer leaves. It does not log in.
func newUpdatesEnv(t *testing.T, b update.Build, installed bool) *updatesEnv {
	t.Helper()
	e := newTestEnv(t)
	dir := t.TempDir()
	if installed {
		if err := os.WriteFile(filepath.Join(dir, "updater"), []byte("1\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gh := newFakeCompare(t)
	e.srv.SetBuild(b)
	e.srv.SetUpdates(update.NewDir(dir, time.Now),
		update.NewChecker(b, &update.Comparer{APIBase: gh.URL}, time.Now))
	return &updatesEnv{testEnv: e, dir: dir, gh: gh}
}

func (e *updatesEnv) writeStatus(t *testing.T, st update.Status) {
	t.Helper()
	s := fmt.Sprintf("state=%s\nfrom=%s\nto=%s\nmessage=%s\nstarted=%d\nfinished=%d\n",
		st.State, st.From, st.To, st.Message, st.Started.Unix(), st.Finished.Unix())
	if err := os.WriteFile(filepath.Join(e.dir, "status"), []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (e *updatesEnv) writeFile(t *testing.T, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(e.dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (e *updatesEnv) requested() bool {
	_, err := os.Stat(filepath.Join(e.dir, "request"))
	return err == nil
}

func assertContains(t *testing.T, page string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(page, want) {
			t.Errorf("page missing %q", want)
		}
	}
}

func assertLacks(t *testing.T, page string, unwanted ...string) {
	t.Helper()
	for _, s := range unwanted {
		if strings.Contains(page, s) {
			t.Errorf("page has %q", s)
		}
	}
}

func TestUpdatesPanelShowsCommitsBehind(t *testing.T) {
	e := newUpdatesEnv(t, testBuild, true)
	e.login(t)
	e.gh.answer("ahead", 2, [][2]string{
		{"bbbbbbb111111111111111111111111111111111", "Fix the thing\n\nwith a body"},
		{"ccccccc222222222222222222222222222222222", "Add the other thing"},
	}, "deploy/bootstrap.sh")

	page := body(t, e.get(t, "/settings"))
	assertContains(t, page,
		`id="updates"`, "aaaaaaa", "2 commits behind",
		"Fix the thing", "Add the other thing", "bbbbbbb", "ccccccc",
		copyInstaller, "Checked", "Check now", copyConfirm)
	assertLacks(t, page, "with a body", copyTraefik, updateButtonDisabled, "local build")
}

func TestUpdatesPanelNotSetUp(t *testing.T) {
	e := newUpdatesEnv(t, testBuild, false)
	e.login(t)

	page := body(t, e.get(t, "/settings"))
	assertContains(t, page, copyNotSetUp, updateButtonDisabled)
}

func TestUpdatesPanelUnknownBuild(t *testing.T) {
	e := newUpdatesEnv(t, update.Build{}, true)
	e.login(t)

	page := body(t, e.get(t, "/settings"))
	assertContains(t, page, copyUnknownBuild, updateButtonDisabled)
	assertLacks(t, page, "Could not check")
	if n := e.gh.requests(); n != 0 {
		t.Errorf("asked GitHub %d times about a build with no commit", n)
	}
}

// Each check result, and the build and warnings shown with it.
func TestUpdatesPanelCheckResults(t *testing.T) {
	many := make([][2]string, 60)
	for i := range many {
		many[i] = [2]string{fmt.Sprintf("%040d", i), fmt.Sprintf("commit %d", i)}
	}
	cases := []struct {
		name  string
		build update.Build
		setup func(*fakeCompare)
		wants []string
		lacks []string
	}{
		{"up to date", testBuild, func(f *fakeCompare) { f.answer("identical", 0, nil) },
			[]string{"Up to date"}, []string{"commits behind", copyInstaller, copyTraefik, updateButtonDisabled}},
		{"ahead or diverged", testBuild, func(f *fakeCompare) { f.answer("diverged", 0, nil) },
			[]string{copyAhead}, []string{updateButtonDisabled}},
		{"could not check", testBuild, func(f *fakeCompare) { f.fail(http.StatusInternalServerError) },
			[]string{"Could not check for updates: github compare: 500 Internal Server Error: nope. Update still builds the newest main."},
			[]string{updateButtonDisabled}},
		{"traefik changes", testBuild, func(f *fakeCompare) {
			f.answer("ahead", 1, [][2]string{{"ddddddd333333333333333333333333333333333", "Bump Traefik"}}, "internal/traefik/proxy.go")
		}, []string{"1 commit behind", copyTraefik}, []string{"1 commits", copyInstaller}},
		{"more than fifty", testBuild, func(f *fakeCompare) { f.answer("ahead", 60, many) },
			[]string{"60 commits behind", "commit 59", "commit 10", `href="https://github.com/o/r/compare/` + testCommit + `...main"`},
			[]string{"commit 9<"}},
		{"local build", update.Build{Commit: testCommit, Repo: testBuild.Repo, Local: true}, func(f *fakeCompare) { f.answer("diverged", 0, nil) },
			[]string{"local build"}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newUpdatesEnv(t, c.build, true)
			e.login(t)
			c.setup(e.gh)
			page := body(t, e.get(t, "/settings"))
			assertContains(t, page, c.wants...)
			assertLacks(t, page, c.lacks...)
		})
	}
}

func TestUpdateRequestWritesTheFile(t *testing.T) {
	e := newUpdatesEnv(t, testBuild, true)
	e.login(t)

	resp := e.postForm(t, "/settings/update", url.Values{})
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/settings#updates" {
		t.Fatalf("got %d -> %q, want 303 -> /settings#updates", resp.StatusCode, resp.Header.Get("Location"))
	}
	if !e.requested() {
		t.Fatal("no request file after Update")
	}

	// A second click (another tab, a double submit) starts nothing new.
	resp = e.postForm(t, "/settings/update", url.Values{})
	page := body(t, resp)
	if resp.StatusCode == http.StatusSeeOther {
		t.Fatal("a second Update was accepted while one is in progress")
	}
	assertContains(t, page, copyRunning, `id="update-log"`)
	assertLacks(t, page, `id="update-button"`)
	entries, err := os.ReadDir(e.dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, en := range entries {
		names = append(names, en.Name())
	}
	if strings.Join(names, " ") != "request updater" {
		t.Errorf("update dir = %v, want just the one request and the marker", names)
	}
}

func TestUpdateRefusedWhileBusy(t *testing.T) {
	cases := []struct {
		name string
		busy func(t *testing.T, e *updatesEnv)
	}{
		{"deploy building", func(t *testing.T, e *updatesEnv) {
			ctx := context.Background()
			app, err := e.store.CreateApp(ctx, core.App{Name: "web", RepoURL: "https://x/y.git", Domain: "web.test"})
			if err != nil {
				t.Fatal(err)
			}
			d, err := e.store.CreateDeployment(ctx, app.ID)
			if err != nil {
				t.Fatal(err)
			}
			if ok, err := e.store.ClaimDeployment(ctx, d.ID); err != nil || !ok {
				t.Fatalf("claim: ok=%v err=%v", ok, err)
			}
		}},
		{"backup running", func(t *testing.T, e *updatesEnv) {
			dest := seedDestination(t, e.testEnv, "minio")
			db := seedDatabase(t, e.testEnv, "shop", core.EnginePostgres)
			b, err := e.store.CreateBackup(context.Background(), core.Backup{
				TargetKind: core.BackupTargetDatabase, TargetID: db.ID, DestinationID: dest.ID, Schedule: "0 3 * * *",
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := e.store.StartBackupRun(context.Background(), b.ID, core.RunKindBackup); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newUpdatesEnv(t, testBuild, true)
			e.login(t)
			c.busy(t, e)

			assertContains(t, body(t, e.get(t, "/settings")), copyBusy, updateButtonDisabled)

			resp := e.postForm(t, "/settings/update", url.Values{})
			page := body(t, resp)
			if resp.StatusCode == http.StatusSeeOther {
				t.Fatal("Update accepted while busy")
			}
			assertContains(t, page, copyBusy)
			if e.requested() {
				t.Error("request file written while busy")
			}
		})
	}
}

func TestUpdateRequestRefusedWhenItCannotRun(t *testing.T) {
	for _, c := range []struct {
		name      string
		build     update.Build
		installed bool
		want      string
	}{
		{"not set up", testBuild, false, copyNotSetUp},
		{"unknown build", update.Build{}, true, copyUnknownBuild},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newUpdatesEnv(t, c.build, c.installed)
			e.login(t)
			resp := e.postForm(t, "/settings/update", url.Values{})
			page := body(t, resp)
			if resp.StatusCode == http.StatusSeeOther {
				t.Fatal("Update accepted")
			}
			assertContains(t, page, c.want)
			if e.requested() {
				t.Error("request file written")
			}
		})
	}
}

func TestUpdateRequestNeedsAuth(t *testing.T) {
	e := newUpdatesEnv(t, testBuild, true)

	resp := e.postForm(t, "/settings/update", url.Values{})
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login" {
		t.Fatalf("got %d -> %q, want 303 -> /login", resp.StatusCode, resp.Header.Get("Location"))
	}
	if e.requested() {
		t.Error("request file written without a session")
	}
}

type progressReply struct {
	State, Message, From, To, Log, Commit string
	Offset                                int64
}

func (e *updatesEnv) progress(t *testing.T, offset int64) progressReply {
	t.Helper()
	resp := e.get(t, fmt.Sprintf("/settings/update/progress?offset=%d", offset))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("progress status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("content type = %q", ct)
	}
	var p progressReply
	if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestUpdateProgressJSON(t *testing.T) {
	e := newUpdatesEnv(t, testBuild, true)
	e.login(t)
	e.writeStatus(t, update.Status{State: update.StateRunning, From: "aaaaaaa", Started: time.Now()})
	e.writeFile(t, "log", "step 1\n")

	p := e.progress(t, 0)
	if p.State != "running" || p.Log != "step 1\n" || p.Offset != 7 || p.From != "aaaaaaa" || p.Commit != testCommit {
		t.Errorf("progress = %+v", p)
	}
	p = e.progress(t, 7)
	if p.State != "running" || p.Log != "" || p.Offset != 7 {
		t.Errorf("progress from 7 = %+v", p)
	}

	// Finished: the script reloads on any state but running.
	e.writeStatus(t, update.Status{State: update.StateUpdated, Message: "updated from aaaaaaa to bbbbbbb", Started: time.Now(), Finished: time.Now()})
	if p = e.progress(t, 7); p.State != "updated" || p.Message != "updated from aaaaaaa to bbbbbbb" {
		t.Errorf("progress after the update = %+v", p)
	}
}

// A request the updater has not picked up yet is in progress, but the status
// and log on disk still belong to the last update.
func TestUpdateProgressBeforeTheUpdaterStarts(t *testing.T) {
	e := newUpdatesEnv(t, testBuild, true)
	e.login(t)
	e.writeStatus(t, update.Status{State: update.StateUpdated, Message: "old news", Started: time.Now(), Finished: time.Now()})
	e.writeFile(t, "log", "the last update's log\n")
	e.writeFile(t, "request", "")

	p := e.progress(t, 0)
	if p.State != "running" || p.Log != "" || p.Offset != 0 || p.Message != "" {
		t.Errorf("progress = %+v, want running with nothing from the last update", p)
	}
}

// A run that says running but began over 40 minutes ago is over; a page left
// polling must stop and reload.
func TestUpdateProgressInterrupted(t *testing.T) {
	e := newUpdatesEnv(t, testBuild, true)
	e.login(t)
	e.writeStatus(t, update.Status{State: update.StateRunning, Started: time.Now().Add(-41 * time.Minute)})

	if p := e.progress(t, 0); p.State == "running" {
		t.Errorf("progress = %+v, want a state other than running", p)
	}
}

func TestUpdatePanelShowsTheLastResult(t *testing.T) {
	e := newUpdatesEnv(t, testBuild, true)
	e.login(t)

	e.writeStatus(t, update.Status{State: update.StateRolledBack, From: "aaaaaaa", To: "bbbbbbb",
		Message: "rolled back: the new version did not answer /healthz", Started: time.Now(), Finished: time.Now()})
	page := body(t, e.get(t, "/settings"))
	assertContains(t, page, "rolled back: the new version did not answer /healthz", copyInstaller, `id="update-button"`)
	assertLacks(t, page, `id="update-log"`)

	e.writeStatus(t, update.Status{State: update.StateRunning, Started: time.Now().Add(-41 * time.Minute)})
	page = body(t, e.get(t, "/settings"))
	assertContains(t, page, "interrupted", copyInterrupted, `id="update-button"`)
	assertLacks(t, page, `id="update-log"`, copyInstaller)

	// Running for real: the log, not a result or the button.
	e.writeStatus(t, update.Status{State: update.StateRunning, Started: time.Now()})
	page = body(t, e.get(t, "/settings"))
	assertContains(t, page, `id="update-log"`, "/settings/update/progress")
	assertLacks(t, page, "interrupted", `id="update-button"`)
}

// Outhaul restarts during an update, so the poller expects failed requests.
// When it has not answered for three minutes, the page says where the log is.
func TestUpdatePollerSaysWhereTheLogIsWhenOuthaulStaysDown(t *testing.T) {
	e := newUpdatesEnv(t, testBuild, true)
	e.login(t)
	e.writeStatus(t, update.Status{State: update.StateRunning, Started: time.Now()})

	page := body(t, e.get(t, "/settings"))
	assertContains(t, page, `data-stuck="`+copyStuck+`"`, "180000", "Outhaul is restarting")
}

func TestManualBackupHeldDuringUpdate(t *testing.T) {
	e := newUpdatesEnv(t, testBuild, true)
	e.login(t)
	dest := seedDestination(t, e.testEnv, "minio")
	db := seedDatabase(t, e.testEnv, "shop", core.EnginePostgres)
	b, err := e.store.CreateBackup(context.Background(), core.Backup{
		TargetKind: core.BackupTargetDatabase, TargetID: db.ID, DestinationID: dest.ID, Schedule: "0 3 * * *",
	})
	if err != nil {
		t.Fatal(err)
	}
	e.writeFile(t, "request", "")

	resp := e.postForm(t, "/backups/"+itoa(b.ID)+"/run", url.Values{})
	if page := body(t, resp); resp.StatusCode != http.StatusConflict || !strings.Contains(page, copyHeld) {
		t.Errorf("run: %d %q, want 409 with the held copy", resp.StatusCode, page)
	}
	if len(e.backups.ran) != 0 {
		t.Errorf("RunNow called during an update: %v", e.backups.ran)
	}

	resp = e.postForm(t, "/backups/"+itoa(b.ID)+"/restore", url.Values{"key": {"dir/shop.dump"}})
	if page := body(t, resp); resp.StatusCode != http.StatusConflict || !strings.Contains(page, copyHeld) {
		t.Errorf("restore: %d %q, want 409 with the held copy", resp.StatusCode, page)
	}
	if len(e.backups.restored) != 0 {
		t.Errorf("RestoreNow called during an update: %v", e.backups.restored)
	}
}

func TestNavBadgeWhenBehind(t *testing.T) {
	e := newUpdatesEnv(t, testBuild, true)
	e.login(t)
	const badge = `class="nav-dot"`

	e.gh.answer("ahead", 1, [][2]string{{"bbbbbbb111111111111111111111111111111111", "Fix"}})
	resp := e.postForm(t, "/settings/update/check", url.Values{})
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/settings#updates" {
		t.Fatalf("check: %d -> %q, want 303 -> /settings#updates", resp.StatusCode, resp.Header.Get("Location"))
	}
	assertContains(t, body(t, e.get(t, "/")), badge)

	e.gh.answer("identical", 0, nil)
	e.postForm(t, "/settings/update/check", url.Values{}).Body.Close()
	assertLacks(t, body(t, e.get(t, "/")), badge)
	if n := e.gh.requests(); n != 2 {
		t.Errorf("GitHub asked %d times, want 2 (Check now always asks)", n)
	}
}

// Without SetUpdates (a server not wired for updates) there is no panel, no
// badge and no update routes.
func TestUpdatesHiddenWithoutSetUpdates(t *testing.T) {
	e := newTestEnv(t)
	e.login(t)

	assertLacks(t, body(t, e.get(t, "/settings")), `id="updates"`, `class="nav-dot"`)
	for _, r := range []struct{ method, path string }{
		{http.MethodPost, "/settings/update"},
		{http.MethodPost, "/settings/update/check"},
		{http.MethodGet, "/settings/update/progress"},
	} {
		var resp *http.Response
		if r.method == http.MethodGet {
			resp = e.get(t, r.path)
		} else {
			resp = e.postForm(t, r.path, url.Values{})
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s %s = %d, want 404", r.method, r.path, resp.StatusCode)
		}
	}
}
