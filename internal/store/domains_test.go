package store

import (
	"context"
	"testing"

	"github.com/outhaul-dev/outhaul/internal/core"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(t.TempDir()+"/test.db", nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func testApp(t *testing.T, st *Store, name, kind, domain string) core.App {
	t.Helper()
	app, err := st.CreateApp(context.Background(), core.App{
		Name: name, RepoURL: "https://example.com/" + name + ".git", Domain: domain, Kind: kind,
	})
	if err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	return app
}

func TestAddAndListDomain(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	app := testApp(t, st, "shop", core.KindCompose, "")

	d, err := st.AddDomain(ctx, core.Domain{
		AppID: app.ID, Host: "shop.example.com", Service: "web", Port: 3000,
		Path: "/api", InternalPath: "/", TLS: true,
	})
	if err != nil {
		t.Fatalf("AddDomain: %v", err)
	}
	if d.ID == 0 {
		t.Fatal("AddDomain returned no ID")
	}
	got, err := st.ListDomains(ctx, app.ID)
	if err != nil || len(got) != 1 {
		t.Fatalf("ListDomains = %v (%v), want 1 row", got, err)
	}
	if got[0].Host != "shop.example.com" || got[0].Service != "web" || got[0].Port != 3000 ||
		got[0].Path != "/api" || got[0].InternalPath != "/" || !got[0].TLS {
		t.Errorf("round-trip mismatch: %+v", got[0])
	}
}

func TestDomainUniquePerHostAndPath(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	app := testApp(t, st, "shop", core.KindCompose, "")
	base := core.Domain{AppID: app.ID, Host: "shop.example.com", Service: "web", Port: 3000}
	if _, err := st.AddDomain(ctx, base); err != nil {
		t.Fatalf("first AddDomain: %v", err)
	}
	// Same host, different path is allowed.
	p := base
	p.Path = "/api"
	if _, err := st.AddDomain(ctx, p); err != nil {
		t.Fatalf("same host different path should be allowed: %v", err)
	}
	// Same host, same path collides.
	if _, err := st.AddDomain(ctx, base); err == nil {
		t.Error("duplicate (host, path) should violate the unique constraint")
	}
}

func TestUpdateDomain(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	app := testApp(t, st, "shop", core.KindCompose, "")
	d, _ := st.AddDomain(ctx, core.Domain{AppID: app.ID, Host: "a.example.com", Service: "web", Port: 3000, TLS: true})

	d.Host = "b.example.com"
	d.Port = 8000
	d.TLS = false
	if err := st.UpdateDomain(ctx, d); err != nil {
		t.Fatalf("UpdateDomain: %v", err)
	}
	got, _ := st.GetDomain(ctx, app.ID, d.ID)
	if got.Host != "b.example.com" || got.Port != 8000 || got.TLS {
		t.Errorf("update not applied: %+v", got)
	}
	// The primary-domain mirror follows the rename.
	if a, _ := st.GetApp(ctx, app.ID); a.Domain != "b.example.com" {
		t.Errorf("primary = %q, want b.example.com after rename", a.Domain)
	}
}

func TestDeleteDomainScopedToApp(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	app := testApp(t, st, "shop", core.KindCompose, "")
	other := testApp(t, st, "blog", core.KindCompose, "")
	d, _ := st.AddDomain(ctx, core.Domain{AppID: app.ID, Host: "a.example.com", Service: "web", Port: 3000})

	// GetDomain is scoped to its app: the wrong app id must not read it.
	if _, err := st.GetDomain(ctx, other.ID, d.ID); err == nil {
		t.Error("GetDomain with the wrong app id should return an error")
	}
	// Wrong app id must not delete it.
	if err := st.DeleteDomain(ctx, other.ID, d.ID); err != nil {
		t.Fatalf("DeleteDomain (wrong app): %v", err)
	}
	if got, _ := st.ListDomains(ctx, app.ID); len(got) != 1 {
		t.Fatal("domain deleted through the wrong app")
	}
	if err := st.DeleteDomain(ctx, app.ID, d.ID); err != nil {
		t.Fatalf("DeleteDomain: %v", err)
	}
	if got, _ := st.ListDomains(ctx, app.ID); len(got) != 0 {
		t.Fatal("domain not deleted")
	}
}

func TestPrimaryDomainMirror(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	app := testApp(t, st, "shop", core.KindCompose, "")

	d1, _ := st.AddDomain(ctx, core.Domain{AppID: app.ID, Host: "b.example.com", Service: "web", Port: 3000})
	if got, _ := st.GetApp(ctx, app.ID); got.Domain != "b.example.com" {
		t.Errorf("primary = %q, want first row host", got.Domain)
	}
	// Adding an alphabetically-earlier host makes it the primary.
	st.AddDomain(ctx, core.Domain{AppID: app.ID, Host: "a.example.com", Service: "web", Port: 3000})
	if got, _ := st.GetApp(ctx, app.ID); got.Domain != "a.example.com" {
		t.Errorf("primary = %q, want new earliest host", got.Domain)
	}
	// Deleting all rows blanks the primary.
	st.DeleteDomain(ctx, app.ID, d1.ID)
	for _, d := range mustList(t, st, app.ID) {
		st.DeleteDomain(ctx, app.ID, d.ID)
	}
	if got, _ := st.GetApp(ctx, app.ID); got.Domain != "" {
		t.Errorf("primary = %q, want blank when no rows", got.Domain)
	}
}

func mustList(t *testing.T, st *Store, appID int64) []core.Domain {
	t.Helper()
	d, err := st.ListDomains(context.Background(), appID)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestListAllDomainsTagsApp(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	shop := testApp(t, st, "shop", core.KindCompose, "")
	st.AddDomain(ctx, core.Domain{AppID: shop.ID, Host: "shop.example.com", Service: "web", Port: 3000})
	// nixpacks app created with a domain seeds its own row (see CreateApp).
	testApp(t, st, "web", core.KindNixpacks, "web.example.com")

	all, err := st.ListAllDomains(ctx)
	if err != nil {
		t.Fatalf("ListAllDomains: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("ListAllDomains = %d rows, want 2", len(all))
	}
	byHost := map[string]core.DomainListing{}
	for _, l := range all {
		byHost[l.Host] = l
	}
	if byHost["shop.example.com"].AppName != "shop" || byHost["shop.example.com"].AppKind != core.KindCompose {
		t.Errorf("shop listing wrong: %+v", byHost["shop.example.com"])
	}
	if byHost["web.example.com"].AppName != "web" || byHost["web.example.com"].Service != "" {
		t.Errorf("web listing wrong: %+v", byHost["web.example.com"])
	}
}

func TestCreateAppSeedsPrimaryDomainRow(t *testing.T) {
	st := testStore(t)
	app := testApp(t, st, "web", core.KindNixpacks, "web.example.com")
	got := mustList(t, st, app.ID)
	if len(got) != 1 || got[0].Host != "web.example.com" || got[0].Port != 8080 || got[0].Service != "" || !got[0].TLS {
		t.Errorf("CreateApp did not seed the primary domain row: %+v", got)
	}
}
