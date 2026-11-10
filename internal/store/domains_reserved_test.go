package store

import (
	"context"
	"errors"
	"testing"

	"github.com/outhaul-dev/outhaul/internal/core"
)

// The host Outhaul's own admin UI is served on can't be given to an app, by any
// writer: an app there would share the admin UI's origin and cookies.

func TestReservedHostCantBeAdded(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	st.SetReservedHost("Outhaul.Example.com.") // as given: case and a trailing dot don't matter
	shop := testApp(t, st, "shop", core.KindCompose, "")

	for _, d := range []core.Domain{
		{Host: "outhaul.example.com"},
		{Host: "OUTHAUL.example.com"},
		{Host: "outhaul.example.com."},
		{Host: "outhaul.example.com", Path: "/x"},
	} {
		d.AppID, d.Service, d.Port = shop.ID, "web", 80
		if _, err := st.AddDomain(ctx, d); !errors.Is(err, ErrHostReserved) {
			t.Errorf("AddDomain %s%s err = %v, want ErrHostReserved", d.Host, d.Path, err)
		}
	}

	// An app created on it isn't created at all.
	_, err := st.CreateApp(ctx, core.App{Name: "outhaul", RepoURL: "https://example.com/o.git", Domain: "outhaul.example.com", Kind: core.KindNixpacks})
	if !errors.Is(err, ErrHostReserved) {
		t.Errorf("CreateApp err = %v, want ErrHostReserved", err)
	}
	_, err = st.CreateAppWithDomains(ctx,
		core.App{Name: "stack", RepoURL: "https://example.com/s.git", Kind: core.KindCompose},
		[]core.Domain{{Host: "outhaul.example.com", Path: "/api", Service: "web", Port: 80}})
	if !errors.Is(err, ErrHostReserved) {
		t.Errorf("CreateAppWithDomains err = %v, want ErrHostReserved", err)
	}
	for _, name := range []string{"outhaul", "stack"} {
		if _, err := st.GetAppByName(ctx, name); err == nil {
			t.Errorf("app %q was created although its route was refused", name)
		}
	}

	// Any other host is still fine, including one under it.
	for _, host := range []string{"app.example.com", "x.outhaul.example.com"} {
		if _, err := st.AddDomain(ctx, core.Domain{AppID: shop.ID, Host: host, Service: "web", Port: 80}); err != nil {
			t.Errorf("AddDomain %s: %v", host, err)
		}
	}
}

func TestReservedHostCantBeMovedOnto(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	st.SetReservedHost("outhaul.example.com")
	blog := testApp(t, st, "blog", core.KindNixpacks, "blog.example.com")
	doms, _ := st.ListDomains(ctx, blog.ID)

	d := doms[0]
	d.Host = "Outhaul.example.com"
	if err := st.UpdateDomain(ctx, d); !errors.Is(err, ErrHostReserved) {
		t.Fatalf("UpdateDomain err = %v, want ErrHostReserved", err)
	}
	if got, _ := st.GetDomain(ctx, blog.ID, d.ID); got.Host != "blog.example.com" {
		t.Errorf("refused move changed the row to %q", got.Host)
	}
}

// A row put on the host before it was reserved stays as it is. It can be
// deleted, and moving it to another path on the host is refused; editing it
// in place is the handlers' to refuse.
func TestRowAlreadyOnReservedHost(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	old := testApp(t, st, "old", core.KindNixpacks, "outhaul.example.com")
	st.SetReservedHost("outhaul.example.com")
	doms, _ := st.ListDomains(ctx, old.ID)
	d := doms[0]

	d.TLS = !d.TLS
	if err := st.UpdateDomain(ctx, d); err != nil {
		t.Errorf("an edit that doesn't move the row: %v", err)
	}
	d.Path = "/other"
	if err := st.UpdateDomain(ctx, d); !errors.Is(err, ErrHostReserved) {
		t.Errorf("moving it to another path on the host: err = %v, want ErrHostReserved", err)
	}

	got, err := st.ReservedHostDomains(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].AppName != "old" || got[0].ID != d.ID {
		t.Errorf("ReservedHostDomains = %+v, want the old app's row", got)
	}

	if err := st.DeleteDomain(ctx, old.ID, d.ID); err != nil {
		t.Fatalf("DeleteDomain: %v", err)
	}
	if got, _ := st.ReservedHostDomains(ctx); len(got) != 0 {
		t.Errorf("ReservedHostDomains after the delete = %+v", got)
	}
}

func TestNoReservedHostAllowsEveryHost(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	// Never set.
	testApp(t, st, "first", core.KindNixpacks, "outhaul.example.com")
	if got, err := st.ReservedHostDomains(ctx); err != nil || len(got) != 0 {
		t.Errorf("ReservedHostDomains with nothing reserved = %+v, %v", got, err)
	}

	// Set, then cleared.
	st.SetReservedHost("outhaul.example.com")
	st.SetReservedHost("")
	app := testApp(t, st, "second", core.KindCompose, "")
	if _, err := st.AddDomain(ctx, core.Domain{AppID: app.ID, Host: "outhaul.example.com", Path: "/api", Service: "web", Port: 80}); err != nil {
		t.Errorf("AddDomain with nothing reserved: %v", err)
	}
}
