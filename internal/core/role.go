package core

// Role is what an account may do. The zero value ranks below every role.
type Role string

const (
	RoleNone          Role = "none"
	RoleProjectViewer Role = "project_viewer"
	RoleProjectMember Role = "project_member"
	RoleProjectAdmin  Role = "project_admin"
	RoleSystemAdmin   Role = "system_admin"
)

// AssignableRoles are the roles an admin can give an account, highest last.
var AssignableRoles = []Role{RoleProjectViewer, RoleProjectMember, RoleProjectAdmin, RoleSystemAdmin}

func (r Role) rank() int {
	switch r {
	case RoleNone:
		return 0
	case RoleProjectViewer:
		return 1
	case RoleProjectMember:
		return 2
	case RoleProjectAdmin:
		return 3
	case RoleSystemAdmin:
		return 4
	}
	return -1
}

// AtLeast reports whether r ranks at or above min. An unknown role ranks
// below everything, so it is never at least anything.
func (r Role) AtLeast(min Role) bool {
	return r.rank() >= 0 && r.rank() >= min.rank()
}

// Label is the role's name as shown to people.
func (r Role) Label() string {
	switch r {
	case RoleSystemAdmin:
		return "System admin"
	case RoleProjectAdmin:
		return "Project admin"
	case RoleProjectMember:
		return "Project member"
	case RoleProjectViewer:
		return "Project viewer"
	}
	return "No role"
}

// ParseRole accepts only the five known role values.
func ParseRole(s string) (Role, bool) {
	r := Role(s)
	return r, r.rank() >= 0
}
