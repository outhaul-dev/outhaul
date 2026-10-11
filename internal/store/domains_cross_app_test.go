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
