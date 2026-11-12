package server

import (
	"context"
	"crypto/rand"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/outhaul-dev/outhaul/internal/core"
	"github.com/outhaul-dev/outhaul/internal/gitrepo"
)

// gitRoutesEnv is a server with git over HTTPS on and a repositories manager
// whose hook does nothing. It returns the git root.
func gitRoutesEnv(t *testing.T) (*testEnv, string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "git")
	e := newTestEnvWith(t, func(s *Server) {
		s.SetGitHTTP(GitHTTPOptions{DefaultDays: 7, MaxDays: 30, SSOMaxAge: 7 * 24 * time.Hour})
		s.SetRepos(gitrepo.New(root, "/bin/true", filepath.Join(t.TempDir(), "hook.sock")))
	})
	return e, root
}

func gitInfoRefs(host string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/git/api.git/info/refs?service=git-upload-pack", nil)
	req.Host = host
	return req
}

func TestGitRoutesAbsentWhenOff(t *testing.T) {
	e := newTestEnv(t)
	rec := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rec, gitInfoRefs("slip.example.com"))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestGitRoutesPresentWhenOn(t *testing.T) {
	e, _ := gitRoutesEnv(t)
	session := e.signInAs(t, "root", core.RoleSystemAdmin)
	h := e.srv.Handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, gitInfoRefs("slip.example.com"))
	if rec.Code != http.StatusUnauthorized || rec.Header().Get("WWW-Authenticate") != `Basic realm="outhaul"` {
		t.Errorf("no credentials: %d, WWW-Authenticate %q; want 401 and the challenge", rec.Code, rec.Header().Get("WWW-Authenticate"))
	}

	// A session cookie signs nothing in here.
	req := gitInfoRefs("slip.example.com")
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: session})
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("a session cookie and no token: %d, want 401", rec.Code)
	}
}

// With no repositories manager the routes say git push is unavailable,
// rather than failing on it.
func TestGitRoutesWithoutReposAreUnavailable(t *testing.T) {
	e := newGitHTTPEnv(t)
	rec := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rec, gitInfoRefs("slip.example.com"))
	if rec.Code != http.StatusServiceUnavailable || rec.Body.String() != "outhaul: git push is not available; see the server log\n" {
		t.Errorf("%d %q, want 503 and the message", rec.Code, rec.Body.String())
	}
}

func TestGitRoutesPassCrossOriginProtection(t *testing.T) {
	e, _ := gitRoutesEnv(t)
	h := e.srv.Handler()
	post := func(site string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/git/api.git/git-receive-pack", strings.NewReader("0000"))
		req.Host = "slip.example.com"
		req.Header.Set("Content-Type", "application/x-git-receive-pack-request")
		if site != "" {
			req.Header.Set("Sec-Fetch-Site", site)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	if rec := post("cross-site"); rec.Code != http.StatusForbidden || rec.Header().Get("WWW-Authenticate") != "" {
		t.Errorf("from another site: %d, want the cross-origin check's 403", rec.Code)
	}
	if rec := post(""); rec.Code != http.StatusUnauthorized {
		t.Errorf("from git: %d, want 401 from the handler", rec.Code)
	}
}

// gitClient runs the real git client in dir, its configuration in a temporary
// directory, answering a password prompt with token.
func gitClient(t *testing.T, dir, token string, args ...string) (string, error) {
	t.Helper()
	home := t.TempDir()
	askpass := filepath.Join(home, "askpass.sh")
	script := "#!/bin/sh\ncase \"$1\" in\nUsername*) echo x ;;\n*) echo '" + token + "' ;;\nesac\n"
	if err := os.WriteFile(askpass, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", append([]string{"-c", "user.name=Test", "-c", "user.email=test@example.com"}, args...)...)
	cmd.Dir = dir
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + home,
		"GIT_CONFIG_GLOBAL=" + filepath.Join(home, ".gitconfig"),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=" + askpass,
		"LC_ALL=C",
	}
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// A push is bigger than the 1 MiB every other body is held to, and arrives
// chunked; it must arrive whole. Everything else is still capped.
func TestGitPushBodyIsNotCapped(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(home, ".gitconfig"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	e, root := gitRoutesEnv(t)
	ctx := context.Background()
	admin, err := e.store.CreatePasswordAccount(ctx, "root", core.RoleSystemAdmin)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	tok, _, err := e.store.CreatePushToken(ctx, admin.ID, "laptop", now, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	h := e.srv.Handler()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Host = "slip.example.com" // the public URL's host
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)

	work := t.TempDir()
	run := func(args ...string) string {
		t.Helper()
		out, err := gitClient(t, work, tok, args...)
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return out
	}
	run("init", "-q", "-b", "main")
	big := make([]byte, 3<<20)
	rand.Read(big)
	if err := os.WriteFile(filepath.Join(work, "big.bin"), big, 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "big.bin")
	run("commit", "-q", "-m", "big")
	commit := run("rev-parse", "HEAD")
	run("-c", "http.postBuffer=65536", "push", ts.URL+"/git/api.git", "main")
	if out, err := gitClient(t, filepath.Join(root, "api.git"), "", "cat-file", "-e", commit); err != nil {
		t.Errorf("the bare repository lacks the pushed commit: %v %s", err, out)
	}

	form := url.Values{"username": {"root"}, "password": {"x"}, "pad": {strings.Repeat("x", 2<<20)}}
	resp, err := http.Post(ts.URL+"/login", "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("POST /login with 2 MiB: %d, want 413", resp.StatusCode)
	}
}

func TestGitPlainHTTP(t *testing.T) {
	ask := func(t *testing.T, tls, tunnel bool, proto string, broken bool) bool {
		t.Helper()
		e := newTestEnvWith(t, func(s *Server) { s.tlsEnabled = tls })
		if tunnel {
			if err := e.store.SetCloudflareToken(context.Background(), "connector"); err != nil {
				t.Fatal(err)
			}
		}
		req := httptest.NewRequest(http.MethodGet, "/git/api.git/info/refs", nil)
		if proto != "" {
			req.Header.Set("X-Forwarded-Proto", proto)
		}
		if broken {
			// A cancelled context makes the tunnel setting unreadable.
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			req = req.WithContext(ctx)
		}
		return e.srv.gitPlainHTTP(req)
	}
	cases := []struct {
		name          string
		tls, tunnel   bool
		proto         string
		broken, plain bool
	}{
		{"TLS, no tunnel, X-Forwarded-Proto http", true, false, "http", false, true},
		{"X-Forwarded-Proto https", true, false, "https", false, false},
		{"no X-Forwarded-Proto", true, false, "", false, false},
		{"the tunnel on", true, true, "http", false, false},
		{"no TLS of Traefik's own", false, false, "http", false, false},
		{"the tunnel setting can't be read", true, false, "http", true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ask(t, c.tls, c.tunnel, c.proto, c.broken); got != c.plain {
				t.Errorf("gitPlainHTTP = %v, want %v", got, c.plain)
			}
		})
	}
}

func publicRoutes(s *Server) []string {
	var out []string
	for _, rt := range s.routes() {
		if rt.level == levelPublic {
			out = append(out, rt.pattern)
		}
	}
	return out
}

// TestPublicRoutesAreExactlyThese keeps the list with git over HTTPS off; on,
// exactly the three git routes are added, each signing itself in.
func TestGitRoutesArePublic(t *testing.T) {
	off := publicRoutes(newTestEnv(t).srv)
	e, _ := gitRoutesEnv(t)
	var added []string
	for _, p := range publicRoutes(e.srv) {
		if !slices.Contains(off, p) {
			added = append(added, p)
		}
	}
	slices.Sort(added)
	want := []string{"GET /git/{repo}/info/refs", "POST /git/{repo}/git-receive-pack", "POST /git/{repo}/git-upload-pack"}
	if !slices.Equal(added, want) {
		t.Errorf("public routes added with git over HTTPS on = %q, want %q", added, want)
	}
	for _, rt := range e.srv.routes() {
		if rt.level == levelUnset || rt.handler == nil {
			t.Errorf("%s has no level or no handler", rt.pattern)
		}
	}
}

// fakeSSH stands in for a running (addr set) or stopped SSH server.
type fakeSSH struct{ addr string }

func (f fakeSSH) Addr() string        { return f.addr }
func (f fakeSSH) Rebind(string) error { return nil }

const httpsTokenNote = `Account → Push tokens</a> as the password. Don't put the token in the remote's address.`

func pushAppFor(t *testing.T, e *testEnv, tag bool) core.App {
	t.Helper()
	app := core.App{
		Name: "pushed", Domain: "pushed.example.com",
		Source: core.SourcePush, Branch: "main", Kind: core.KindNixpacks,
	}
	if tag {
		app.DeployTrigger = "tag"
		app.TagPattern = "v*"
	}
	app, err := e.store.CreateApp(context.Background(), app)
	if err != nil {
		t.Fatal(err)
	}
	return app
}

func pageAs(t *testing.T, e *testEnv, tok, path string) string {
	t.Helper()
	resp := e.getAs(t, tok, path)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d", path, resp.StatusCode)
	}
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

func TestAppPagePushRemotes(t *testing.T) {
	const httpsLine = "git remote add outhaul https://slip.example.com/git/pushed.git"
	setup := func(sshAddr string, on bool) func(*Server) {
		return func(s *Server) {
			if on {
				s.SetGitHTTP(GitHTTPOptions{DefaultDays: 7, MaxDays: 30, SSOMaxAge: 7 * 24 * time.Hour})
			}
			if sshAddr != "" {
				s.SetSSHControl(fakeSSH{sshAddr})
			}
		}
	}
	tests := []struct {
		name     string
		on       bool
		ssh      string
		wantHTTP bool
		wantSSH  bool
	}{
		{"off, ssh running", false, ":2222", false, true},
		{"on, ssh running", true, ":2222", true, true},
		{"on, ssh off", true, "", true, false},
		{"off, ssh off", false, "", false, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEnvWith(t, setup(tc.ssh, tc.on))
			app := pushAppFor(t, e, false)
			tok := e.signInAs(t, "root", core.RoleSystemAdmin)
			page := pageAs(t, e, tok, "/apps/"+strconv.FormatInt(app.ID, 10))
			if got := strings.Contains(page, httpsLine); got != tc.wantHTTP {
				t.Errorf("https remote shown = %v, want %v", got, tc.wantHTTP)
			}
			if got := strings.Contains(page, httpsTokenNote); got != tc.wantHTTP {
				t.Errorf("token note shown = %v, want %v", got, tc.wantHTTP)
			}
			if got := strings.Contains(page, "ssh://git@"); got != tc.wantSSH {
				t.Errorf("ssh remote shown = %v, want %v", got, tc.wantSSH)
			}
			if !tc.on && strings.Contains(page, "/git/") {
				t.Error("page names /git/ with git over HTTPS off")
			}
		})
	}

	t.Run("tag line under the https lines", func(t *testing.T) {
		e := newTestEnvWith(t, setup("", true))
		app := pushAppFor(t, e, true)
		tok := e.signInAs(t, "root", core.RoleSystemAdmin)
		page := pageAs(t, e, tok, "/apps/"+strconv.FormatInt(app.ID, 10))
		i := strings.Index(page, httpsLine)
		if i < 0 || !strings.Contains(page[i:], "git tag &lt;tag&gt; &amp;&amp; git push outhaul &lt;tag&gt;") && !strings.Contains(page[i:], "git tag &lt;tag&gt; && git push outhaul &lt;tag&gt;") {
			t.Error("no tag line under the https lines")
		}
	})

	t.Run("a viewer sees neither", func(t *testing.T) {
		e := newTestEnvWith(t, setup(":2222", true))
		app := pushAppFor(t, e, false)
		tok := e.signInAs(t, "viewer", core.RoleProjectViewer, app.ProjectID)
		page := pageAs(t, e, tok, "/apps/"+strconv.FormatInt(app.ID, 10))
		if strings.Contains(page, "/git/") || strings.Contains(page, "ssh://git@") || strings.Contains(page, httpsTokenNote) {
			t.Error("a viewer sees a push remote")
		}
	})
}

func TestAccountPushKeysTextNamesHTTPS(t *testing.T) {
	on := newTestEnvWith(t, func(s *Server) {
		s.SetGitHTTP(GitHTTPOptions{DefaultDays: 7, MaxDays: 30, SSOMaxAge: 7 * 24 * time.Hour})
		s.SetSSHControl(fakeSSH{":2222"})
	})
	tok := on.signInAs(t, "root", core.RoleSystemAdmin)
	if p := pageAs(t, on, tok, "/account"); !strings.Contains(p, "https://slip.example.com/git/") {
		t.Error("push keys text doesn't name the https remote")
	}
	off := newTestEnvWith(t, func(s *Server) { s.SetSSHControl(fakeSSH{":2222"}) })
	tok = off.signInAs(t, "root", core.RoleSystemAdmin)
	p := pageAs(t, off, tok, "/account")
	if strings.Contains(p, "/git/") {
		t.Error("account page names /git/ with git over HTTPS off")
	}
	const today = `Add your laptop or CI key, then push to a push-to-deploy app's <code>ssh://git@…</code> remote.</p>`
	if !strings.Contains(p, today) {
		t.Error("push keys text changed with git over HTTPS off")
	}
}
