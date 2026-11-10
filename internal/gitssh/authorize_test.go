package gitssh

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/outhaul-dev/outhaul/internal/core"
	"github.com/outhaul-dev/outhaul/internal/sshkey"
	"github.com/outhaul-dev/outhaul/internal/store"
	"golang.org/x/crypto/ssh"
)

// keyFor registers a key owned by userID and returns its fingerprint.
func keyFor(t *testing.T, st *store.Store, userID int64) string {
	t.Helper()
	_, pub, err := sshkey.Generate()
	if err != nil {
		t.Fatal(err)
	}
	pk, _, _, _, err := ssh.ParseAuthorizedKey([]byte(pub))
	if err != nil {
		t.Fatal(err)
	}
	fp := ssh.FingerprintSHA256(pk)
	if _, err := st.AddPushKey(context.Background(), core.PushKey{
		Label: "t", PublicKey: pub, Fingerprint: fp, UserID: userID,
	}); err != nil {
		t.Fatal(err)
	}
	return fp
}

func newUser(t *testing.T, st *store.Store, role core.Role) core.User {
	t.Helper()
	u, err := st.CreatePasswordAccount(context.Background(), fmt.Sprintf("u-%d", time.Now().UnixNano()), role)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestAuthorize(t *testing.T) {
	ctx := context.Background()
	srv, st, _ := testServer(t)
	p, err := st.CreateProject(ctx, core.Project{Name: "p"})
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name   string
		role   core.Role
		member bool
		want   error
	}{
		{"system admin, not a member", core.RoleSystemAdmin, false, nil},
		{"project admin, member", core.RoleProjectAdmin, true, nil},
		{"project member, member", core.RoleProjectMember, true, nil},
		{"project member, not a member", core.RoleProjectMember, false, errPushDenied},
		{"viewer, member", core.RoleProjectViewer, true, errPushDenied},
		{"no role", core.RoleNone, true, errPushDenied},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			u := newUser(t, st, c.role)
			if c.member {
				if err := st.AddProjectMember(ctx, p.ID, u.ID); err != nil {
					t.Fatal(err)
				}
			}
			fp := keyFor(t, st, u.ID)
			if err := srv.authorize(ctx, fp, p.ID); !errors.Is(err, c.want) {
				t.Fatalf("authorize = %v; want %v", err, c.want)
			}
		})
	}

	t.Run("key with no owner", func(t *testing.T) {
		fp := keyFor(t, st, 0)
		if err := srv.authorize(ctx, fp, p.ID); !errors.Is(err, errPushDenied) {
			t.Fatalf("authorize = %v; want errPushDenied", err)
		}
	})
	t.Run("unknown key", func(t *testing.T) {
		if err := srv.authorize(ctx, "SHA256:nope", p.ID); !errors.Is(err, errPushDenied) {
			t.Fatalf("authorize = %v; want errPushDenied", err)
		}
	})
	t.Run("store error is not a denial", func(t *testing.T) {
		u := newUser(t, st, core.RoleSystemAdmin)
		fp := keyFor(t, st, u.ID)
		srv2 := &Server{keyring: failingKeyring{srv.keyring}}
		err := srv2.authorize(ctx, fp, p.ID)
		if err == nil || errors.Is(err, errPushDenied) {
			t.Fatalf("authorize = %v; want a plain store error", err)
		}
	})
}

type failingKeyring struct{ Keyring }

func (failingKeyring) GetUser(context.Context, int64) (core.User, error) {
	return core.User{}, errors.New("db down")
}

func TestColdPushCountsAsTheDefaultProject(t *testing.T) {
	ctx := context.Background()
	srv, st, gitRoot := testServer(t)
	addr := srv.Addr()
	u := newUser(t, st, core.RoleProjectMember)
	key := registerKeyFor(t, st, u.ID)

	out, err := runGit(gitEnv(key, addr), "", "ls-remote", remoteURL(addr, "fresh"))
	if err == nil {
		t.Fatalf("push by a member off the default project should fail; output:\n%s", out)
	}
	if !strings.Contains(out, "outhaul: you don't have permission to use 'fresh'") {
		t.Fatalf("output = %q; want the permission message", out)
	}
	if _, err := os.Stat(filepath.Join(gitRoot, "fresh.git")); !os.IsNotExist(err) {
		t.Fatalf("bare repo should not exist; stat err = %v", err)
	}

	if err := st.AddProjectMember(ctx, core.DefaultProjectID, u.ID); err != nil {
		t.Fatal(err)
	}
	if out, err := runGit(gitEnv(key, addr), "", "ls-remote", remoteURL(addr, "fresh")); err != nil {
		t.Fatalf("after joining the default project: %v\n%s", err, out)
	}
}

func TestDemotionAppliesToTheNextPush(t *testing.T) {
	ctx := context.Background()
	srv, st, _ := testServer(t)
	addr := srv.Addr()
	u := newUser(t, st, core.RoleProjectMember)
	if err := st.AddProjectMember(ctx, core.DefaultProjectID, u.ID); err != nil {
		t.Fatal(err)
	}
	key := registerKeyFor(t, st, u.ID)

	if out, err := runGit(gitEnv(key, addr), "", "ls-remote", remoteURL(addr, "demo")); err != nil {
		t.Fatalf("member push: %v\n%s", err, out)
	}
	if err := st.SetUserRole(ctx, u.ID, core.RoleProjectViewer); err != nil {
		t.Fatal(err)
	}
	out, err := runGit(gitEnv(key, addr), "", "ls-remote", remoteURL(addr, "demo"))
	if err == nil || !strings.Contains(out, "you don't have permission") {
		t.Fatalf("push after demotion: err=%v output=%q; want refusal", err, out)
	}
}

// pushApp makes a push-source app in a new project and returns the project id.
func pushApp(t *testing.T, st *store.Store, name string) int64 {
	t.Helper()
	p, err := st.CreateProject(context.Background(), core.Project{Name: "proj-" + name})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateApp(context.Background(), core.App{Name: name, Source: core.SourcePush, ProjectID: p.ID}); err != nil {
		t.Fatal(err)
	}
	return p.ID
}

// commitIn makes a local repo with one commit and returns its directory.
func commitIn(t *testing.T, env []string) string {
	t.Helper()
	local := filepath.Join(t.TempDir(), "local")
	if err := os.MkdirAll(local, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, a := range [][]string{
		{"init", "-q", "-b", "main"}, {"config", "user.email", "t@example.com"},
		{"config", "user.name", "t"}, {"config", "commit.gpgsign", "false"},
		{"commit", "-q", "--allow-empty", "-m", "init"},
	} {
		if out, err := runGit(env, local, a...); err != nil {
			t.Fatalf("git %v: %v\n%s", a, err, out)
		}
	}
	return local
}

func TestExistingAppFollowsItsOwnProject(t *testing.T) {
	ctx := context.Background()
	srv, st, gitRoot := testServer(t)
	addr := srv.Addr()
	pid := pushApp(t, st, "inproj")

	onP := newUser(t, st, core.RoleProjectMember)
	if err := st.AddProjectMember(ctx, pid, onP.ID); err != nil {
		t.Fatal(err)
	}
	onDefault := newUser(t, st, core.RoleProjectMember)
	if err := st.AddProjectMember(ctx, core.DefaultProjectID, onDefault.ID); err != nil {
		t.Fatal(err)
	}

	okEnv := gitEnv(registerKeyFor(t, st, onP.ID), addr)
	local := commitIn(t, okEnv)
	if out, err := runGit(okEnv, local, "push", remoteURL(addr, "inproj"), "main"); err != nil {
		t.Fatalf("member of P should push: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(gitRoot, "inproj.git")); err != nil {
		t.Fatalf("bare repo should exist: %v", err)
	}

	// Refused: nothing is created for a second app they can't reach.
	pushApp(t, st, "other")
	badEnv := gitEnv(registerKeyFor(t, st, onDefault.ID), addr)
	local2 := commitIn(t, badEnv)
	out, err := runGit(badEnv, local2, "push", remoteURL(addr, "other"), "main")
	if err == nil || !strings.Contains(out, "you don't have permission to use 'other'") {
		t.Fatalf("push by a default-project member: err=%v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(gitRoot, "other.git")); !os.IsNotExist(err) {
		t.Fatalf("bare repo should not exist; stat err = %v", err)
	}
}

func TestPermissionIsCheckedBeforeNonPushRefusal(t *testing.T) {
	ctx := context.Background()
	srv, st, _ := testServer(t)
	p, err := st.CreateProject(ctx, core.Project{Name: "pub"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateApp(ctx, core.App{Name: "pubapp", Source: core.SourcePublic, ProjectID: p.ID}); err != nil {
		t.Fatal(err)
	}
	u := newUser(t, st, core.RoleProjectMember)
	fp := keyFor(t, st, u.ID)
	ch := &fakeChannel{}
	if code := srv.runGit(ctx, ch, "git-receive-pack 'pubapp'", fp); code != 1 {
		t.Fatalf("code = %d", code)
	}
	got := ch.stderr.String()
	if !strings.Contains(got, "you don't have permission") || strings.Contains(got, "push-deployable") {
		t.Fatalf("stderr = %q; want only the permission message", got)
	}
}
