package store

import (
	"context"
	"database/sql"
	"errors"
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

func TestDomainProtectedRoundTrips(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	app := testApp(t, st, "wiki", core.KindCompose, "")

	d, err := st.AddDomain(ctx, core.Domain{AppID: app.ID, Host: "wiki.example.com", Service: "web", Port: 3000, TLS: true, Protected: true})
	if err != nil {
		t.Fatalf("AddDomain: %v", err)
	}
	if got := mustGetDomain(t, st, app.ID, d.ID); !got.Protected {
		t.Errorf("AddDomain did not store Protected: %+v", got)
	}
	d.Protected = false
	if err := st.UpdateDomain(ctx, d); err != nil {
		t.Fatalf("UpdateDomain: %v", err)
	}
	if got := mustList(t, st, app.ID); got[0].Protected {
		t.Errorf("UpdateDomain did not clear Protected: %+v", got[0])
	}
	if err := st.SetDomainProtected(ctx, app.ID, d.ID, true); err != nil {
		t.Fatalf("SetDomainProtected: %v", err)
	}
	if got := mustGetDomain(t, st, app.ID, d.ID); !got.Protected {
		t.Errorf("SetDomainProtected(true) not applied: %+v", got)
	}
	all, err := st.ListAllDomains(ctx)
	if err != nil || len(all) != 1 || !all[0].Protected {
		t.Errorf("ListAllDomains = %+v (%v), want one protected row", all, err)
	}
	if err := st.SetDomainProtected(ctx, app.ID, d.ID, false); err != nil {
		t.Fatalf("SetDomainProtected: %v", err)
	}
	if got := mustGetDomain(t, st, app.ID, d.ID); got.Protected {
		t.Errorf("SetDomainProtected(false) not applied: %+v", got)
	}
}

// SetDomainProtected is scoped to the row's app, like every other domain write.
func TestSetDomainProtectedScopedToApp(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	app := testApp(t, st, "wiki", core.KindCompose, "")
	other := testApp(t, st, "blog", core.KindCompose, "")
	d, err := st.AddDomain(ctx, core.Domain{AppID: app.ID, Host: "wiki.example.com", Service: "web", Port: 3000})
	if err != nil {
		t.Fatalf("AddDomain: %v", err)
	}

	if err := st.SetDomainProtected(ctx, other.ID, d.ID, true); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("SetDomainProtected through the wrong app: err = %v, want sql.ErrNoRows", err)
	}
	if got := mustGetDomain(t, st, app.ID, d.ID); got.Protected {
		t.Error("row protected through the wrong app")
	}
}

// mustGetDomain reads one row, failing the test if it can't: a lookup error
// must not pass for an unprotected row.
func mustGetDomain(t *testing.T, st *Store, appID, id int64) core.Domain {
	t.Helper()
	d, err := st.GetDomain(context.Background(), appID, id)
	if err != nil {
		t.Fatalf("GetDomain(%d, %d): %v", appID, id, err)
	}
	return d
}

// AnyDomainProtected says whether Traefik must be able to reach the gate on
// the host, whatever else is configured.
func TestAnyDomainProtected(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	check := func(want bool) {
		t.Helper()
		got, err := st.AnyDomainProtected(ctx)
		if err != nil {
			t.Fatalf("AnyDomainProtected: %v", err)
		}
		if got != want {
			t.Errorf("AnyDomainProtected = %v, want %v", got, want)
		}
	}
	check(false)
	app := testApp(t, st, "wiki", core.KindCompose, "")
	d, err := st.AddDomain(ctx, core.Domain{AppID: app.ID, Host: "wiki.example.com", Service: "web", Port: 3000})
	if err != nil {
		t.Fatalf("AddDomain: %v", err)
	}
	check(false)
	if err := st.SetDomainProtected(ctx, app.ID, d.ID, true); err != nil {
		t.Fatalf("SetDomainProtected: %v", err)
	}
	check(true)
	if err := st.SetDomainProtected(ctx, app.ID, d.ID, false); err != nil {
		t.Fatalf("SetDomainProtected: %v", err)
	}
	check(false)
}

// Upgrading leaves every existing route open; nothing redeploys.
func TestProtectedMigrationKeepsRowsOpen(t *testing.T) {
	db := openMigratedBefore(t, "0028")
	if _, err := db.Exec(`INSERT INTO apps (id, name, repo_url, domain, created_at)
	                      VALUES (7, 'wiki', 'r', 'wiki.example.com', '2026-07-04T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO domains (id, app_id, host, service, port, path, internal_path, tls, created_at)
	                      VALUES (3, 7, 'wiki.example.com', '', 8080, '', '', 1, '2026-07-04T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if err := migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	d, err := scanDomain(db.QueryRow(`SELECT ` + domainCols + ` FROM domains WHERE id = 3`))
	if err != nil {
		t.Fatalf("read domain after migration: %v", err)
	}
	if d.Protected {
		t.Errorf("migrated row is protected: %+v", d)
	}
	if d.Host != "wiki.example.com" || !d.TLS {
		t.Errorf("migrated row changed: %+v", d)
	}
}

// The tables the gate's sessions live in: ending a gateway session ends the
// route sessions and one-time codes minted from it.
func TestGateTablesCascadeFromGatewaySession(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := st.db.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec(`INSERT INTO gate_sessions (token_hash, subject, kind, display, created_at, expires_at)
	      VALUES ('g1', 'user:1', 'password', 'dana', '2026-11-02T00:00:00Z', '2026-11-02T08:00:00Z')`)
	exec(`INSERT INTO route_sessions (token_hash, gate_token_hash, host, display, expires_at)
	      VALUES ('r1', 'g1', 'wiki.example.com', 'dana', '2026-11-02T08:00:00Z')`)
	exec(`INSERT INTO gate_codes (code_hash, gate_token_hash, host, nonce_hash, expires_at)
	      VALUES ('c1', 'g1', 'wiki.example.com', 'n1', '2026-11-02T00:01:00Z')`)
	exec(`DELETE FROM gate_sessions WHERE token_hash = 'g1'`)

	for _, table := range []string{"route_sessions", "gate_codes"} {
		var n int
		if err := st.db.QueryRowContext(ctx, `SELECT count(*) FROM `+table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("%s has %d rows after its gateway session was deleted, want 0", table, n)
		}
	}
}

// RouteIsProtected answers the way Traefik routes: of every app's rows on the
// host, those whose path is a plain string prefix of the URL path, and the
// longest of them wins.
func TestRouteIsProtected(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	wiki := testApp(t, st, "wiki", core.KindCompose, "")
	hooks := testApp(t, st, "hooks", core.KindCompose, "")
	shop := testApp(t, st, "shop", core.KindCompose, "")

	add := func(appID int64, host, path string, protected bool) core.Domain {
		t.Helper()
		d, err := st.AddDomain(ctx, core.Domain{AppID: appID, Host: host, Path: path, Service: "web", Port: 3000, TLS: true})
		if err != nil {
			t.Fatalf("AddDomain %s%s: %v", host, path, err)
		}
		if protected {
			if err := st.SetDomainProtected(ctx, appID, d.ID, true); err != nil {
				t.Fatalf("SetDomainProtected: %v", err)
			}
		}
		return d
	}
	// The rows go in shorter-first on one host and longer-first on the other,
	// so neither "first match wins" nor "last match wins" passes by accident.
	add(wiki.ID, "wiki.example.com", "", true)
	add(hooks.ID, "wiki.example.com", "/webhooks", false)
	add(shop.ID, "shop.example.com", "/admin", true)
	add(shop.ID, "shop.example.com", "", false)
	// PathPrefix(`/`) is a longer rule than a bare Host, so "/" wins everywhere.
	add(wiki.ID, "root.example.com", "", false)
	add(hooks.ID, "root.example.com", "/", true)

	cases := []struct {
		host, path string
		want       bool
	}{
		{"wiki.example.com", "/", true},
		{"wiki.example.com", "/page", true},
		{"wiki.example.com", "", true},
		{"wiki.example.com", "/webhooks", false},
		{"wiki.example.com", "/webhooks/x", false},
		{"wiki.example.com", "/webhooksfoo", false},
		{"shop.example.com", "/admin", true},
		{"shop.example.com", "/admin/x", true},
		{"shop.example.com", "/administrator", true},
		{"shop.example.com", "/", false},
		{"shop.example.com", "", false},
		{"shop.example.com", "/cart", false},
		{"WIKI.example.com", "/page", true},
		{"unknown.example.com", "/", false},
		{"root.example.com", "", true},
		{"root.example.com", "/x", true},
	}
	for _, c := range cases {
		got, err := st.RouteIsProtected(ctx, c.host, c.path)
		if err != nil {
			t.Fatalf("RouteIsProtected(%q, %q): %v", c.host, c.path, err)
		}
		if got != c.want {
			t.Errorf("RouteIsProtected(%q, %q) = %v, want %v", c.host, c.path, got, c.want)
		}
	}
}

// Rows written before hosts were lowercased, and two apps sharing a host and
// path from before the cross-app check: the stored case doesn't matter, and on
// a tie an open row wins, because Traefik could route the request to it.
func TestRouteIsProtectedLegacyRows(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	a := testApp(t, st, "a", core.KindCompose, "")
	b := testApp(t, st, "b", core.KindCompose, "")
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := st.db.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	insert := `INSERT INTO domains (app_id, host, service, port, path, internal_path, tls, protected, created_at)
	           VALUES (?, ?, 'web', 3000, ?, '', 1, ?, '2026-07-04T00:00:00Z')`
	exec(insert, a.ID, "Docs.Example.com", "", 1)
	exec(insert, a.ID, "tie.example.com", "/x", 1)
	exec(insert, b.ID, "tie.example.com", "/x", 0)
	exec(insert, a.ID, "eit.example.com", "/x", 0)
	exec(insert, b.ID, "eit.example.com", "/x", 1)

	for _, c := range []struct {
		host, path string
		want       bool
	}{
		{"docs.example.com", "/", true},
		{"tie.example.com", "/x/y", false},
		{"eit.example.com", "/x/y", false},
	} {
		got, err := st.RouteIsProtected(ctx, c.host, c.path)
		if err != nil {
			t.Fatalf("RouteIsProtected(%q, %q): %v", c.host, c.path, err)
		}
		if got != c.want {
			t.Errorf("RouteIsProtected(%q, %q) = %v, want %v", c.host, c.path, got, c.want)
		}
	}
}
