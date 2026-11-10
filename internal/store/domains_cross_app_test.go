package store

import (
	"context"
	"errors"
	"testing"

	"github.com/outhaul-dev/outhaul/internal/core"
)

// Two apps routing the same host+path give Traefik two routers with identical
// rules, and it picks one arbitrarily. Every path that writes a route must
// refuse a host+path another app already serves.

func TestAddDomainRejectsHostRoutedByAnotherApp(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	testApp(t, st, "shop", core.KindNixpacks, "shop.example.com")
	other := testApp(t, st, "blog", core.KindCompose, "")

	_, err := st.AddDomain(ctx, core.Domain{AppID: other.ID, Host: "shop.example.com", Service: "web", Port: 80})
	if !errors.Is(err, ErrDomainTaken) {
		t.Fatalf("AddDomain err = %v, want ErrDomainTaken", err)
	}
	// Hosts are case-insensitive; rows written before lowercasing may be mixed.
	_, err = st.AddDomain(ctx, core.Domain{AppID: other.ID, Host: "SHOP.example.com", Service: "web", Port: 80})
	if !errors.Is(err, ErrDomainTaken) {
		t.Fatalf("AddDomain (case variant) err = %v, want ErrDomainTaken", err)
	}
}

func TestAddDomainAllowsSameHostOnAnotherPath(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	testApp(t, st, "shop", core.KindNixpacks, "shop.example.com")
	api := testApp(t, st, "api", core.KindCompose, "")

	if _, err := st.AddDomain(ctx, core.Domain{AppID: api.ID, Host: "shop.example.com", Path: "/api", Service: "web", Port: 80}); err != nil {
		t.Fatalf("AddDomain on a distinct path: %v", err)
	}
}

func TestCreateAppRejectsHostRoutedByAnotherApp(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	testApp(t, st, "shop", core.KindNixpacks, "shop.example.com")

	_, err := st.CreateApp(ctx, core.App{Name: "copy", RepoURL: "https://example.com/c.git", Domain: "shop.example.com", Kind: core.KindNixpacks})
	if !errors.Is(err, ErrDomainTaken) {
		t.Fatalf("CreateApp err = %v, want ErrDomainTaken", err)
	}
	if _, err := st.GetAppByName(ctx, "copy"); err == nil {
		t.Fatal("app row was created despite the domain conflict")
	}
}

func TestUpdateDomainRejectsHostRoutedByAnotherApp(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	testApp(t, st, "shop", core.KindNixpacks, "shop.example.com")
	blog := testApp(t, st, "blog", core.KindNixpacks, "blog.example.com")
	doms, _ := st.ListDomains(ctx, blog.ID)

	d := doms[0]
	d.TLS = !d.TLS // an edit that keeps its own host is fine
	if err := st.UpdateDomain(ctx, d); err != nil {
		t.Fatalf("UpdateDomain keeping own host: %v", err)
	}
	d.Host = "shop.example.com"
	if err := st.UpdateDomain(ctx, d); !errors.Is(err, ErrDomainTaken) {
		t.Fatalf("UpdateDomain err = %v, want ErrDomainTaken", err)
	}
}

// Installs from before the check may already hold a host+path two apps share.
// Those rows must stay editable (TLS, port, service) as long as the edit keeps
// their host and path; only moving onto another app's route is refused.
func TestUpdateDomainKeepsPreexistingDuplicateEditable(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	testApp(t, st, "shop", core.KindNixpacks, "shop.example.com")
	blog := testApp(t, st, "blog", core.KindCompose, "")
	if _, err := st.db.ExecContext(ctx,
		`INSERT INTO domains (app_id, host, service, port, path, internal_path, tls, created_at)
		 VALUES (?, 'shop.example.com', 'web', 80, '', '', 0, '2026-07-04T00:00:00Z')`, blog.ID); err != nil {
		t.Fatalf("seed legacy duplicate: %v", err)
	}
	doms, _ := st.ListDomains(ctx, blog.ID)

	d := doms[0]
	d.TLS = true
	if err := st.UpdateDomain(ctx, d); err != nil {
		t.Fatalf("editing a legacy duplicate without moving it: %v", err)
	}
	d.Host = "SHOP.example.com" // a case-only change is not a move
	if err := st.UpdateDomain(ctx, d); err != nil {
		t.Fatalf("case-only host edit on a legacy duplicate: %v", err)
	}
}

// A compose app's first route is created with the app. Committing the app first
// and refusing the route afterwards left a half-created app with no domain.
func TestCreateAppWithDomainsIsAllOrNothing(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	testApp(t, st, "shop", core.KindNixpacks, "shop.example.com")

	_, err := st.CreateAppWithDomains(ctx,
		core.App{Name: "stack", RepoURL: "https://example.com/s.git", Kind: core.KindCompose},
		[]core.Domain{{Host: "shop.example.com", Service: "web", Port: 80}})
	if !errors.Is(err, ErrDomainTaken) {
		t.Fatalf("err = %v, want ErrDomainTaken", err)
	}
	if _, err := st.GetAppByName(ctx, "stack"); err == nil {
		t.Fatal("app row was created although its first route was refused")
	}

	app, err := st.CreateAppWithDomains(ctx,
		core.App{Name: "stack", RepoURL: "https://example.com/s.git", Kind: core.KindCompose},
		[]core.Domain{{Host: "stack.example.com", Service: "web", Port: 80}})
	if err != nil {
		t.Fatalf("CreateAppWithDomains: %v", err)
	}
	if doms, _ := st.ListDomains(ctx, app.ID); len(doms) != 1 || doms[0].AppID != app.ID || doms[0].Host != "stack.example.com" {
		t.Fatalf("domains = %+v, want the one route bound to the new app", doms)
	}
}

// A refused route says which app holds it, and that app's project, so a
// caller can name it only to someone who may see that project.
func TestDomainTakenNamesTheHolder(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	p, err := st.CreateProject(ctx, core.Project{Name: "other"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateApp(ctx, core.App{Name: "shop", RepoURL: "https://example.com/s.git", Domain: "shop.example.com", ProjectID: p.ID}); err != nil {
		t.Fatal(err)
	}
	blog := testApp(t, st, "blog", core.KindCompose, "")
	_, err = st.AddDomain(ctx, core.Domain{AppID: blog.ID, Host: "SHOP.example.com", Service: "web", Port: 80})
	var taken *DomainTakenError
	if !errors.As(err, &taken) || !errors.Is(err, ErrDomainTaken) {
		t.Fatalf("AddDomain err = %v, want a DomainTakenError", err)
	}
	if taken.App != "shop" || taken.ProjectID != p.ID || taken.Host != "SHOP.example.com" || taken.Path != "" {
		t.Errorf("DomainTakenError = %+v", *taken)
	}
	if want := `domain is already routed by another app: SHOP.example.com is served by app "shop"`; err.Error() != want {
		t.Errorf("err = %q, want %q", err, want)
	}
}

// OtherProjectsOnHost says whether an app outside a project has a row on a
// host (in any case, on any path), and whether one of those rows is protected.
func TestOtherProjectsOnHost(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	p, err := st.CreateProject(ctx, core.Project{Name: "other"})
	if err != nil {
		t.Fatal(err)
	}
	own := testApp(t, st, "own", core.KindCompose, "")
	theirs, err := st.CreateApp(ctx, core.App{Name: "theirs", RepoURL: "https://example.com/t.git", Kind: core.KindCompose, ProjectID: p.ID})
	if err != nil {
		t.Fatal(err)
	}
	add := func(app core.App, host, path string, protected bool) {
		t.Helper()
		if _, err := st.AddDomain(ctx, core.Domain{AppID: app.ID, Host: host, Path: path, Service: "web", Port: 80, Protected: protected}); err != nil {
			t.Fatal(err)
		}
	}
	add(own, "mine.test", "", true)
	add(theirs, "open.test", "/x", false)
	add(theirs, "shut.test", "/admin", true)
	add(theirs, "shut.test", "", false)
	for _, c := range []struct {
		host          string
		project       int64
		any, shielded bool
	}{
		{"nobody.test", core.DefaultProjectID, false, false},
		{"mine.test", core.DefaultProjectID, false, false},
		{"mine.test", p.ID, true, true},
		{"OPEN.test", core.DefaultProjectID, true, false},
		{"open.test", p.ID, false, false},
		{"Shut.Test", core.DefaultProjectID, true, true},
	} {
		anyRow, protected, err := st.OtherProjectsOnHost(ctx, c.host, c.project)
		if err != nil {
			t.Fatal(err)
		}
		if anyRow != c.any || protected != c.shielded {
			t.Errorf("OtherProjectsOnHost(%s, %d) = %v, %v; want %v, %v", c.host, c.project, anyRow, protected, c.any, c.shielded)
		}
	}
}
