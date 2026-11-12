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

func TestRoleForGroups(t *testing.T) {
	mapping := []core.GroupRole{
		{Group: "eng", Role: core.RoleProjectMember},
		{Group: "admins", Role: core.RoleSystemAdmin},
		{Group: "all", Role: core.RoleProjectViewer},
	}
	cases := []struct {
		groups []string
		want   core.Role
	}{
		{nil, core.RoleNone},
		{[]string{}, core.RoleNone},
		{[]string{"eng"}, core.RoleProjectMember},
		{[]string{"eng", "admins"}, core.RoleSystemAdmin},
		{[]string{"admins", "all"}, core.RoleSystemAdmin},
		{[]string{"ENG"}, core.RoleNone}, // exact comparison
		{[]string{"nobody"}, core.RoleNone},
	}
	for _, c := range cases {
		if got := core.RoleForGroups(c.groups, mapping); got != c.want {
			t.Errorf("RoleForGroups(%v) = %q, want %q", c.groups, got, c.want)
		}
	}
	if got := core.RoleForGroups([]string{"eng"}, nil); got != core.RoleNone {
		t.Errorf("no mapping = %q, want none", got)
	}
}
