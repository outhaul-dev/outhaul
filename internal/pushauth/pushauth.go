// Package pushauth is the one rule both git transports (SSH and HTTPS) apply
// to decide whether an account may push to, and fetch from, a project.
package pushauth

import (
	"context"
	"database/sql"
	"errors"

	"github.com/outhaul-dev/outhaul/internal/core"
)

// Store is what Allowed reads (the store satisfies it).
type Store interface {
	GetUser(ctx context.Context, id int64) (core.User, error)
	IsProjectMember(ctx context.Context, projectID, userID int64) (bool, error)
}

// Allowed reports whether the account may push to and fetch from apps in the
// project: a system admin always; anyone else with project member or above
// and a place on the project. An account that no longer exists may not. The
// role is read now.
func Allowed(ctx context.Context, st Store, userID, projectID int64) (bool, error) {
	u, err := st.GetUser(ctx, userID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if u.Role == core.RoleSystemAdmin {
		return true, nil
	}
	if !u.Role.AtLeast(core.RoleProjectMember) {
		return false, nil
	}
	return st.IsProjectMember(ctx, projectID, u.ID)
}
