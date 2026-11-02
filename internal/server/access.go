package server

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"net/http"
	"strings"

	"github.com/outhaul-dev/outhaul/internal/core"
)

// accessLevel says who may use a route. Every route in routes() states one.
type accessLevel int

const (
	// levelUnset is the zero value: a route that forgot its level. Handler
	// panics on it, so one can't ship.
	levelUnset accessLevel = iota

	levelPublic  // no sign-in
	levelAccount // signed in, with any role or none

	// A role, whichever projects the person is on.
	levelAnyRole
	levelMemberRole
	levelProjectAdminRole

	levelSystemAdmin

	// A role on the project the route's resolver finds.
	levelViewer
	levelMember
	levelProjectAdmin
)

// projectScoped reports whether the level is checked against one project, so
// its route needs a resolver.
func (l accessLevel) projectScoped() bool {
	return l == levelViewer || l == levelMember || l == levelProjectAdmin
}

// minRole is the lowest role a level admits. Anything without a case here gets
// only system admins, so a mistake refuses people instead of letting them in.
func (l accessLevel) minRole() core.Role {
	switch l {
	case levelAnyRole, levelViewer:
		return core.RoleProjectViewer
	case levelMemberRole, levelMember:
		return core.RoleProjectMember
	case levelProjectAdminRole, levelProjectAdmin:
		return core.RoleProjectAdmin
	}
	return core.RoleSystemAdmin
}

// projectResolver finds the project a request acts on. found is false when the
// id is malformed or names nothing; err is for a store that failed.
type projectResolver func(r *http.Request) (projectID int64, found bool, err error)

type userContextKey struct{}

// withUser returns r carrying the signed-in account.
func withUser(r *http.Request, u core.User) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), userContextKey{}, u))
}

// userFrom returns the signed-in account, which guard put on the request. A
// public route has none.
func userFrom(r *http.Request) (core.User, bool) {
	u, ok := r.Context().Value(userContextKey{}).(core.User)
	return u, ok
}

// canChange reports whether the person's role is project member or above.
func (s *Server) canChange(r *http.Request) bool {
	u, ok := userFrom(r)
	return ok && u.Role.AtLeast(core.RoleProjectMember)
}

// guard wraps a route's handler with its access check. The account is read on
// every request, so a role change applies to the next one.
//
// An object outside someone's projects answers 404, the same as one that doesn't
// exist. A role too low for something they can see answers 403.
func (s *Server) guard(rt route) http.HandlerFunc {
	if rt.level == levelPublic {
		return rt.handler
	}
	return func(w http.ResponseWriter, r *http.Request) {
		sess, ok := s.currentSession(r)
		if !ok {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		user, err := s.store.GetUser(r.Context(), sess.UserID)
		if errors.Is(err, sql.ErrNoRows) {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		// Not a redirect: /login sends a valid session back here, so the browser
		// would bounce between the two until the store recovered.
		if err != nil {
			log.Printf("server: access check for %s: %v", rt.pattern, err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		r = withUser(r, user)

		if rt.level == levelAccount {
			rt.handler(w, r)
			return
		}
		// An unknown role counts as no role.
		if !user.Role.AtLeast(core.RoleProjectViewer) {
			s.render(w, r, http.StatusForbidden, "noaccess", map[string]any{"Title": "No access", "HideChrome": true})
			return
		}
		// A system admin skips the resolver as well as the role check, so the
		// handlers keep answering them as they always have: an unknown project
		// in a form is the form's 400, not a 404 from here.
		if user.Role == core.RoleSystemAdmin {
			rt.handler(w, r)
			return
		}
		if rt.level.projectScoped() {
			on, err := s.onProject(r, rt.project, user.ID)
			if err != nil {
				log.Printf("server: access check for %s: %v", rt.pattern, err)
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
			if !on {
				http.NotFound(w, r)
				return
			}
		}
		if !user.Role.AtLeast(rt.level.minRole()) {
			http.Error(w, "Your role doesn't allow this.", http.StatusForbidden)
			return
		}
		rt.handler(w, r)
	}
}

// onProject reports whether the request acts on a project the account is on.
func (s *Server) onProject(r *http.Request, resolve projectResolver, userID int64) (bool, error) {
	projectID, found, err := resolve(r)
	if err != nil || !found {
		return false, err
	}
	return s.store.IsProjectMember(r.Context(), projectID, userID)
}

// projectOfPath is for routes whose {id} is the project itself. It needs no
// lookup: nobody is a member of a project that doesn't exist.
func (s *Server) projectOfPath(r *http.Request) (int64, bool, error) {
	id, ok := parseID(r.PathValue("id"))
	return id, ok, nil
}

// projectOfApp is for routes whose {id} is an app. A preview keeps its parent's
// project.
func (s *Server) projectOfApp(r *http.Request) (int64, bool, error) {
	id, ok := parseID(r.PathValue("id"))
	if !ok {
		return 0, false, nil
	}
	return s.projectOfTarget(r.Context(), core.BackupTargetApp, id)
}

// projectOfDatabase is for routes whose {id} is a database.
func (s *Server) projectOfDatabase(r *http.Request) (int64, bool, error) {
	id, ok := parseID(r.PathValue("id"))
	if !ok {
		return 0, false, nil
	}
	return s.projectOfTarget(r.Context(), core.BackupTargetDatabase, id)
}

// projectOfDeployment is for routes whose {id} is a deployment: its project is
// its app's.
func (s *Server) projectOfDeployment(r *http.Request) (int64, bool, error) {
	id, ok := parseID(r.PathValue("id"))
	if !ok {
		return 0, false, nil
	}
	d, err := s.store.GetDeployment(r.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return s.projectOfTarget(r.Context(), core.BackupTargetApp, d.AppID)
}

// projectOfBackup is for routes whose {id} is a backup: its project is its
// target's.
func (s *Server) projectOfBackup(r *http.Request) (int64, bool, error) {
	id, ok := parseID(r.PathValue("id"))
	if !ok {
		return 0, false, nil
	}
	b, err := s.store.GetBackup(r.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return s.projectOfTarget(r.Context(), b.TargetKind, b.TargetID)
}

// projectOfBackupForm is for creating a backup. It reads the target as
// handleCreateBackup does, so the project checked is the one the backup lands in.
func (s *Server) projectOfBackupForm(r *http.Request) (int64, bool, error) {
	id, ok := parseID(r.FormValue("target_id"))
	if !ok {
		return 0, false, nil
	}
	return s.projectOfTarget(r.Context(), r.FormValue("target_kind"), id)
}

// projectOfForm is for creating an app in a project. It reads the form as
// handleCreateApp and handleDeployTemplate do, so the project checked is the one
// used. An empty value means the default project, which the person must also be on.
func (s *Server) projectOfForm(r *http.Request) (int64, bool, error) {
	v := strings.TrimSpace(r.FormValue("project_id"))
	if v == "" {
		return core.DefaultProjectID, true, nil
	}
	id, ok := parseID(v)
	return id, ok, nil
}

// projectOfTarget finds the project of an app or a database, the two things a
// project directly holds. Anything else, or a target that's gone, isn't found.
func (s *Server) projectOfTarget(ctx context.Context, kind string, id int64) (int64, bool, error) {
	var (
		projectID int64
		err       error
	)
	switch kind {
	case core.BackupTargetApp:
		var a core.App
		a, err = s.store.GetApp(ctx, id)
		projectID = a.ProjectID
	case core.BackupTargetDatabase:
		var d core.Database
		d, err = s.store.GetDatabase(ctx, id)
		projectID = d.ProjectID
	default:
		return 0, false, nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return projectID, true, nil
}

// projectScope is the projects a list page may show: all of them for a system
// admin, otherwise the ones the person is on.
type projectScope struct {
	all bool
	ids map[int64]bool
}

func (p projectScope) allows(id int64) bool { return p.all || p.ids[id] }

// idList returns the project IDs of a limited scope. It is empty for an all
// scope, which callers answer with the unfiltered store calls instead.
func (p projectScope) idList() []int64 {
	ids := make([]int64, 0, len(p.ids))
	for id := range p.ids {
		ids = append(ids, id)
	}
	return ids
}

// scopeFor returns the projects the signed-in person may see. A request with
// no account sees none.
func (s *Server) scopeFor(r *http.Request) (projectScope, error) {
	u, ok := userFrom(r)
	if !ok {
		return projectScope{ids: map[int64]bool{}}, nil
	}
	if u.Role == core.RoleSystemAdmin {
		return projectScope{all: true}, nil
	}
	list, err := s.store.ProjectIDsForUser(r.Context(), u.ID)
	if err != nil {
		return projectScope{}, err
	}
	ids := make(map[int64]bool, len(list))
	for _, id := range list {
		ids[id] = true
	}
	return projectScope{ids: ids}, nil
}

// projectsFor lists the projects a page may show or offer.
func (s *Server) projectsFor(r *http.Request) ([]core.Project, error) {
	scope, err := s.scopeFor(r)
	if err != nil {
		return nil, err
	}
	return s.scopedProjects(r.Context(), scope)
}

func (s *Server) scopedProjects(ctx context.Context, scope projectScope) ([]core.Project, error) {
	all, err := s.store.ListProjects(ctx)
	if err != nil || scope.all {
		return all, err
	}
	var out []core.Project
	for _, p := range all {
		if scope.allows(p.ID) {
			out = append(out, p)
		}
	}
	return out, nil
}

func (s *Server) scopedApps(ctx context.Context, scope projectScope) ([]core.App, error) {
	all, err := s.store.ListApps(ctx)
	if err != nil || scope.all {
		return all, err
	}
	var out []core.App
	for _, a := range all {
		if scope.allows(a.ProjectID) {
			out = append(out, a)
		}
	}
	return out, nil
}
