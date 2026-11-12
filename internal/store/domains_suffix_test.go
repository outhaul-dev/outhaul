package store

import (
	"context"
	"errors"
	"testing"

	"github.com/outhaul-dev/outhaul/internal/core"
)

func TestSuffixOnlyRefusesEveryWriter(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	st.SetDomainSuffix("shared.demo.example.com")
	shop := testApp(t, st, "shop", core.KindCompose, "")

	for _, host := range []string{"shop.example.com", "a.b.shared.demo.example.com", "shared.demo.example.com"} {
		if _, err := st.AddDomain(ctx, core.Domain{AppID: shop.ID, Host: host, Service: "web", Port: 80}); !errors.Is(err, ErrHostOutsideSuffix) {
			t.Errorf("AddDomain %q err = %v, want ErrHostOutsideSuffix", host, err)
		}
	}
	for _, host := range []string{"shop.shared.demo.example.com", "SHOP.Shared.Demo.Example.COM", "shop.shared.demo.example.com."} {
		if _, err := st.AddDomain(ctx, core.Domain{AppID: shop.ID, Host: host, Service: "web", Port: 80, Path: "/" + host}); err != nil {
			t.Errorf("AddDomain %q: %v", host, err)
		}
	}

	// The primary row a CreateApp seeds is checked too.
	if _, err := st.CreateApp(ctx, core.App{Name: "blog", RepoURL: "https://example.com/b.git", Domain: "blog.example.com"}); !errors.Is(err, ErrHostOutsideSuffix) {
		t.Errorf("CreateApp err = %v, want ErrHostOutsideSuffix", err)
	}
	_, err := st.CreateAppWithDomains(ctx,
		core.App{Name: "stack", RepoURL: "https://example.com/s.git", Kind: core.KindCompose},
		[]core.Domain{{Host: "outside.example.com", Service: "web", Port: 80}})
	if !errors.Is(err, ErrHostOutsideSuffix) {
		t.Errorf("CreateAppWithDomains err = %v, want ErrHostOutsideSuffix", err)
	}
}

func TestSuffixOnlyChecksOnlyAMove(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	old := testApp(t, st, "old", core.KindNixpacks, "old.example.com")
	st.SetDomainSuffix("shared.demo.example.com")
	doms, _ := st.ListDomains(ctx, old.ID)
	d := doms[0]

	d.TLS = !d.TLS
	if err := st.UpdateDomain(ctx, d); err != nil {
		t.Errorf("an edit that doesn't move an outside row: %v", err)
	}
	d.Path = "/other"
	if err := st.UpdateDomain(ctx, d); !errors.Is(err, ErrHostOutsideSuffix) {
		t.Errorf("moving an outside row: err = %v, want ErrHostOutsideSuffix", err)
	}

	got, err := st.DomainsOutsideSuffix(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].AppName != "old" {
		t.Errorf("DomainsOutsideSuffix = %+v, want the old app's row", got)
	}
	if err := st.DeleteDomain(ctx, old.ID, got[0].ID); err != nil {
		t.Fatalf("DeleteDomain: %v", err)
	}
}

func TestNoSuffixAllowsEveryHost(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	testApp(t, st, "first", core.KindNixpacks, "first.example.com")
	if got, err := st.DomainsOutsideSuffix(ctx); err != nil || len(got) != 0 {
		t.Errorf("DomainsOutsideSuffix with nothing set = %+v, %v", got, err)
	}
	st.SetDomainSuffix("shared.demo.example.com")
	st.SetDomainSuffix("")
	if _, err := st.AddDomain(ctx, core.Domain{AppID: 1, Host: "anything.example.com", Port: 8080}); err != nil {
		t.Errorf("AddDomain after clearing the suffix: %v", err)
	}
}
