package pushauth

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/outhaul-dev/outhaul/internal/core"
	"github.com/outhaul-dev/outhaul/internal/store"
)

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestAllowed(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	p, err := st.CreateProject(ctx, core.Project{Name: "p"})
	if err != nil {
		t.Fatal(err)
	}
	other, err := st.CreateProject(ctx, core.Project{Name: "other"})
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name   string
		role   core.Role
		member *int64
		want   bool
	}{
		{"system admin on no project", core.RoleSystemAdmin, nil, true},
		{"project member on the project", core.RoleProjectMember, &p.ID, true},
		{"project member on another project", core.RoleProjectMember, &other.ID, false},
		{"project admin on the project", core.RoleProjectAdmin, &p.ID, true},
		{"viewer on the project", core.RoleProjectViewer, &p.ID, false},
		{"no role on the project", core.RoleNone, &p.ID, false},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			u, err := st.CreatePasswordAccount(ctx, fmt.Sprintf("u%d", i), c.role)
			if err != nil {
				t.Fatal(err)
			}
			if c.member != nil {
				if err := st.AddProjectMember(ctx, *c.member, u.ID); err != nil {
					t.Fatal(err)
				}
			}
			got, err := Allowed(ctx, st, u.ID, p.ID)
			if err != nil || got != c.want {
				t.Fatalf("Allowed = %v, %v; want %v, nil", got, err, c.want)
			}
		})
	}

	t.Run("a deleted account", func(t *testing.T) {
		u, err := st.CreatePasswordAccount(ctx, "gone", core.RoleProjectMember)
		if err != nil {
			t.Fatal(err)
		}
		if err := st.AddProjectMember(ctx, p.ID, u.ID); err != nil {
			t.Fatal(err)
		}
		if err := st.DeleteUser(ctx, u.ID); err != nil {
			t.Fatal(err)
		}
		got, err := Allowed(ctx, st, u.ID, p.ID)
		if err != nil || got {
			t.Fatalf("Allowed = %v, %v; want false, nil", got, err)
		}
	})

	t.Run("a store error is returned", func(t *testing.T) {
		st2 := openStore(t)
		u, err := st2.CreatePasswordAccount(ctx, "x", core.RoleSystemAdmin)
		if err != nil {
			t.Fatal(err)
		}
		st2.Close()
		got, err := Allowed(ctx, st2, u.ID, 1)
		if err == nil || got {
			t.Fatalf("Allowed = %v, %v; want false and an error", got, err)
		}
	})
}
