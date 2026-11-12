package store

import (
	"context"
	"testing"

	"github.com/outhaul-dev/outhaul/internal/core"
)

func TestUpdateAppSource(t *testing.T) {
	s := openWithBox(t)
	ctx := context.Background()
	app, err := s.CreateApp(ctx, core.App{
		Name: "web", Source: core.SourcePublic, RepoURL: "https://example.com/r.git",
		Kind: core.KindNixpacks, Domain: "web.example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	// public -> github
	if err := s.UpdateAppSource(ctx, app.ID, core.SourceGithub,
		"https://github.com/o/n.git", "o/n", 0, "", ""); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetApp(ctx, app.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Source != core.SourceGithub || got.GithubRepo != "o/n" ||
		got.RepoURL != "https://github.com/o/n.git" {
		t.Fatalf("source not updated: %+v", got)
	}
	// github -> ssh, storing a deploy key
	if err := s.UpdateAppSource(ctx, app.ID, core.SourceSSH,
		"git@github.com:o/n.git", "", 0, "ssh-ed25519 AAA deploy", "PRIVATEKEY"); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetApp(ctx, app.ID)
	if got.Source != core.SourceSSH || got.SSHPublicKey != "ssh-ed25519 AAA deploy" {
		t.Fatalf("ssh source not updated: %+v", got)
	}
	if got.SSHPrivateKey != "" {
		t.Fatalf("GetApp must never populate the private key, got %q", got.SSHPrivateKey)
	}
}

func TestUpdateAppKind(t *testing.T) {
	s := openWithBox(t)
	ctx := context.Background()
	app, err := s.CreateApp(ctx, core.App{
		Name: "web", Source: core.SourcePublic, RepoURL: "https://example.com/r.git",
		Kind: core.KindNixpacks, Domain: "web.example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateAppKind(ctx, app.ID, core.KindCompose, "stack/docker-compose.yml", ""); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetApp(ctx, app.ID)
	if got.Kind != core.KindCompose || got.ComposePath != "stack/docker-compose.yml" {
		t.Fatalf("kind not updated: %+v", got)
	}
}

// The row seeded from app.Domain is open by default and protected when the
// caller asks for it; the app's own Domain field is unchanged either way.
func TestCreateAppProtectedSeedsAProtectedPrimaryRow(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()

	open, err := st.CreateApp(ctx, core.App{Name: "open", RepoURL: "https://example.com/o.git", Domain: "open.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := st.ListDomains(ctx, open.ID)
	if err != nil || len(rows) != 1 {
		t.Fatalf("open rows = %v (%v)", rows, err)
	}
	if rows[0].Protected {
		t.Error("CreateApp seeded a protected row")
	}

	guarded, err := st.CreateAppProtected(ctx, core.App{Name: "guarded", RepoURL: "https://example.com/g.git", Domain: "guarded.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	rows, err = st.ListDomains(ctx, guarded.ID)
	if err != nil || len(rows) != 1 {
		t.Fatalf("guarded rows = %v (%v)", rows, err)
	}
	if !rows[0].Protected || rows[0].Host != "guarded.example.com" || rows[0].Port != primaryDomainPort {
		t.Errorf("guarded row = %+v, want a protected row on guarded.example.com", rows[0])
	}
	if guarded.Domain != "guarded.example.com" {
		t.Errorf("app.Domain = %q, want the submitted host", guarded.Domain)
	}

	// A compose app seeds no row, so the protected seed changes nothing there.
	stack, err := st.CreateAppProtected(ctx, core.App{Name: "stack", RepoURL: "https://example.com/s.git", Kind: core.KindCompose})
	if err != nil {
		t.Fatal(err)
	}
	if rows, _ := st.ListDomains(ctx, stack.ID); len(rows) != 0 {
		t.Errorf("compose rows = %v, want none", rows)
	}
}
