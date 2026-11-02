package core_test

import (
	"testing"

	"github.com/outhaul-dev/outhaul/internal/core"
)

func TestRoleAtLeast(t *testing.T) {
	if !core.RoleSystemAdmin.AtLeast(core.RoleProjectViewer) {
		t.Fatal("admin >= viewer")
	}
	if core.RoleProjectViewer.AtLeast(core.RoleProjectMember) {
		t.Fatal("viewer < member")
	}
	if core.RoleNone.AtLeast(core.RoleProjectViewer) {
		t.Fatal("none < viewer")
	}
	if core.Role("bogus").AtLeast(core.RoleNone) {
		t.Fatal("unknown role has no rank")
	}
}

func TestRoleLabelsAndParse(t *testing.T) {
	want := map[core.Role]string{
		core.RoleSystemAdmin:   "System admin",
		core.RoleProjectAdmin:  "Project admin",
		core.RoleProjectMember: "Project member",
		core.RoleProjectViewer: "Project viewer",
		core.RoleNone:          "No role",
	}
	for r, label := range want {
		if r.Label() != label {
			t.Errorf("%q label = %q, want %q", r, r.Label(), label)
		}
		got, ok := core.ParseRole(string(r))
		if !ok || got != r {
			t.Errorf("ParseRole(%q) = %q, %v", r, got, ok)
		}
	}
	if _, ok := core.ParseRole("bogus"); ok {
		t.Fatal("bogus parsed")
	}
	if n := len(core.AssignableRoles); n != 4 || core.AssignableRoles[n-1] != core.RoleSystemAdmin {
		t.Fatalf("AssignableRoles = %v, want four, highest last", core.AssignableRoles)
	}
}
