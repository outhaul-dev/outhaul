package server

import (
	"database/sql"
	"errors"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/outhaul-dev/outhaul/internal/core"
	"github.com/outhaul-dev/outhaul/internal/store"
)

// setupLinkTTL is how long a setup link works.
const setupLinkTTL = 7 * 24 * time.Hour

const (
	setupLinkGone  = "This link has expired or has already been used."
	lastAdminError = "This is the last system admin that signs in with a password. Make another account a system admin first, or nobody could manage Outhaul if single sign-on stopped working."
)

// userRow is one line of the Users page.
type userRow struct {
	core.UserRow
	Name     string
	Self     bool
	Waiting  bool // an SSO account with no role yet
	Password bool
	Admin    bool // reaches every project
}

// usersPage renders the Users page. extra carries the error or the one-time
// link of the request that is being answered.
func (s *Server) usersPage(w http.ResponseWriter, r *http.Request, status int, extra map[string]any) {
	list, err := s.store.ListUsers(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	me, _ := userFrom(r)
	rows := make([]userRow, 0, len(list))
	waiting := 0
	for _, u := range list {
		row := userRow{
			UserRow:  u,
			Name:     u.Display(),
			Self:     u.ID == me.ID,
			Waiting:  u.SignIn == core.SignInSSO && u.Role == core.RoleNone,
			Password: u.SignIn == core.SignInPassword,
			Admin:    u.Role == core.RoleSystemAdmin,
		}
		if row.Waiting {
			waiting++
		}
		rows = append(rows, row)
	}
	data := map[string]any{
		"Title":     "Users",
		"Active":    "users",
		"Users":     rows,
		"Waiting":   waiting,
		"NewRole":   core.RoleProjectMember,
		"AllRoles":  append([]core.Role{core.RoleNone}, core.AssignableRoles...),
		"SetupDays": int(setupLinkTTL / (24 * time.Hour)),
	}
	for k, v := range extra {
		data[k] = v
	}
	s.render(w, r, status, "users", data)
}

func (s *Server) handleUsersList(w http.ResponseWriter, r *http.Request) {
	s.usersPage(w, r, http.StatusOK, nil)
}

// baseURL is where people reach Outhaul: the public URL when one is set,
// otherwise the address this request came to.
func (s *Server) baseURL(r *http.Request) string {
	if s.publicURL != "" {
		return strings.TrimRight(s.publicURL, "/")
	}
	scheme := "http"
	if requestIsHTTPS(r) {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// setupLinkFor issues a fresh setup link for the account through issue, which
// stores it. The link is only ever returned to the caller to show once.
func (s *Server) setupLinkFor(r *http.Request, issue func(token string, expires time.Time) error) (string, error) {
	token := NewToken()
	if err := issue(token, time.Now().Add(setupLinkTTL)); err != nil {
		return "", err
	}
	return s.baseURL(r) + "/account/setup?token=" + url.QueryEscape(token), nil
}

func validUsername(name string) error {
	if len(name) < 3 {
		return errors.New("The username must be at least 3 characters.")
	}
	if len(name) > 64 {
		return errors.New("The username must be at most 64 characters.")
	}
	if strings.HasPrefix(strings.ToLower(name), "sso:") {
		return errors.New(`The username can't start with "sso:".`)
	}
	for _, c := range name {
		if unicode.IsSpace(c) || unicode.IsControl(c) {
			return errors.New("The username can't contain spaces.")
		}
	}
	return nil
}

func (s *Server) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	username := strings.TrimSpace(r.FormValue("username"))
	role, ok := core.ParseRole(r.FormValue("role"))
	fail := func(status int, msg string) {
		s.usersPage(w, r, status, map[string]any{
			"Error": msg, "OpenDialog": "user-dialog", "FormUsername": username, "FormRole": r.FormValue("role"),
		})
	}
	if !ok {
		fail(http.StatusBadRequest, "Choose a role from the list.")
		return
	}
	if err := validUsername(username); err != nil {
		fail(http.StatusBadRequest, err.Error())
		return
	}
	if _, err := s.store.GetUserByName(r.Context(), username); err == nil {
		fail(http.StatusBadRequest, "There is already an account with that username.")
		return
	} else if !errors.Is(err, sql.ErrNoRows) {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	user, err := s.store.CreatePasswordAccount(r.Context(), username, role)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			fail(http.StatusBadRequest, "There is already an account with that username.")
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	link, err := s.setupLinkFor(r, func(token string, expires time.Time) error {
		return s.store.CreateSetupLink(r.Context(), user.ID, token, expires)
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.showSetupLink(w, r, user, link)
}

// showSetupLink answers the request that issued a link. It is the only place
// the link is shown: it is neither stored in readable form nor put in a URL.
func (s *Server) showSetupLink(w http.ResponseWriter, r *http.Request, user core.User, link string) {
	w.Header().Set("Cache-Control", "no-store")
	s.usersPage(w, r, http.StatusOK, map[string]any{"NewLink": link, "NewLinkFor": user.Username})
}

// targetUser loads the account named in the path, answering 404 itself when
// there is none.
func (s *Server) targetUser(w http.ResponseWriter, r *http.Request) (core.User, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return core.User{}, false
	}
	u, err := s.store.GetUser(r.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return core.User{}, false
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return core.User{}, false
	}
	return u, true
}

// refuse re-renders the Users page with the reason a change was refused.
func (s *Server) refuse(w http.ResponseWriter, r *http.Request, err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, store.ErrLastPasswordAdmin) {
		s.usersPage(w, r, http.StatusBadRequest, map[string]any{"Error": lastAdminError})
		return true
	}
	http.Error(w, err.Error(), http.StatusInternalServerError)
	return true
}

func (s *Server) handleSetUserRole(w http.ResponseWriter, r *http.Request) {
	u, ok := s.targetUser(w, r)
	if !ok {
		return
	}
	role, valid := core.ParseRole(r.FormValue("role"))
	if !valid {
		s.usersPage(w, r, http.StatusBadRequest, map[string]any{"Error": "Choose a role from the list."})
		return
	}
	if s.refuse(w, r, s.store.SetUserRole(r.Context(), u.ID, role)) {
		return
	}
	http.Redirect(w, r, "/users", http.StatusSeeOther)
}

func (s *Server) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	u, ok := s.targetUser(w, r)
	if !ok {
		return
	}
	if s.refuse(w, r, s.store.DeleteUser(r.Context(), u.ID)) {
		return
	}
	http.Redirect(w, r, "/users", http.StatusSeeOther)
}

func (s *Server) handleResetUserPassword(w http.ResponseWriter, r *http.Request) {
	u, ok := s.targetUser(w, r)
	if !ok {
		return
	}
	if me, _ := userFrom(r); me.ID == u.ID {
		s.usersPage(w, r, http.StatusBadRequest, map[string]any{
			"Error": "You can't reset your own password here. Change it on your account page, /account.",
		})
		return
	}
	if u.SignIn != core.SignInPassword {
		s.usersPage(w, r, http.StatusBadRequest, map[string]any{
			"Error": "This person signs in with SSO, so they have no password to reset.",
		})
		return
	}
	link, err := s.setupLinkFor(r, func(token string, expires time.Time) error {
		return s.store.ResetUserPassword(r.Context(), u.ID, token, expires)
	})
	if s.refuse(w, r, err) {
		return
	}
	s.showSetupLink(w, r, u, link)
}

// --- the page a setup link opens ---

func (s *Server) setupPage(w http.ResponseWriter, r *http.Request, status int, data map[string]any) {
	data["Title"] = "Set your password"
	data["HideChrome"] = true
	// The link is a secret in the address bar: keep it out of caches and Referer.
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	s.render(w, r, status, "account_setup", data)
}

func setupToken(r *http.Request) string {
	if t := r.FormValue("token"); t != "" {
		return t
	}
	return r.URL.Query().Get("token")
}

func (s *Server) handleAccountSetupForm(w http.ResponseWriter, r *http.Request) {
	token := setupToken(r)
	user, err := s.store.SetupLinkUser(r.Context(), token)
	if errors.Is(err, sql.ErrNoRows) {
		s.setupPage(w, r, http.StatusGone, map[string]any{"Invalid": true, "Message": setupLinkGone})
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.setupPage(w, r, http.StatusOK, map[string]any{"Token": token, "Username": user.Username})
}

func (s *Server) handleAccountSetupSubmit(w http.ResponseWriter, r *http.Request) {
	token := setupToken(r)
	user, err := s.store.SetupLinkUser(r.Context(), token)
	if errors.Is(err, sql.ErrNoRows) {
		s.setupPage(w, r, http.StatusGone, map[string]any{"Invalid": true, "Message": setupLinkGone})
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	again := func(status int, msg string) {
		s.setupPage(w, r, status, map[string]any{"Token": token, "Username": user.Username, "Error": msg})
	}
	password := r.FormValue("password")
	if err := validateCredentials(user.Username, password); err != nil {
		again(http.StatusBadRequest, err.Error())
		return
	}
	var hash string
	if busy := s.withHashSlot(r.Context(), func() { hash, err = HashPassword(password) }); busy != nil {
		again(http.StatusServiceUnavailable, busyMessage)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	user, err = s.store.CompleteSetupLink(r.Context(), token, hash)
	if errors.Is(err, sql.ErrNoRows) {
		s.setupPage(w, r, http.StatusGone, map[string]any{"Invalid": true, "Message": setupLinkGone})
		return
	}
	if err != nil {
		log.Printf("server: complete setup link for user %d: %v", user.ID, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if err := s.login(w, r, user); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
