package server

import (
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/outhaul-dev/outhaul/internal/core"
	"github.com/outhaul-dev/outhaul/internal/store"
)

// GitHTTPOptions turns git over HTTPS on. Call SetGitHTTP before Handler.
type GitHTTPOptions struct {
	DefaultDays, MaxDays int
	SSOMaxAge            time.Duration
	// ClientAddr is the address a request is counted and recorded under; nil
	// takes the connection's own address.
	ClientAddr func(*http.Request) netip.Addr
	// BridgeReady reports whether the hook bridge is listening; nil means it
	// is.
	BridgeReady func() bool
}

// SetGitHTTP turns on push tokens and their pages. Call it before Handler.
func (s *Server) SetGitHTTP(o GitHTTPOptions) {
	s.gitHTTP = o
	s.gitHTTPSet = true
}

// gitHTTPOn reports whether git over HTTPS is on.
func (s *Server) gitHTTPOn() bool { return s.gitHTTPSet }

// tokenView is a push token as a page shows it.
type tokenView struct {
	core.PushToken
	Owner   string
	Ends    time.Time
	Expired bool
}

// viewToken is t as a page shows it, with owner's name.
func (s *Server) viewToken(t core.PushToken, owner string, now time.Time) tokenView {
	return tokenView{PushToken: t, Owner: owner, Ends: t.EndsAt(s.gitHTTP.MaxDays), Expired: t.Expired(now, s.gitHTTP.MaxDays)}
}

func (s *Server) tokenRows(rows []core.PushTokenRow) []tokenView {
	now := time.Now()
	out := make([]tokenView, 0, len(rows))
	for _, r := range rows {
		out = append(out, s.viewToken(r.PushToken, r.Owner, now))
	}
	return out
}

// addPushTokenData puts the account page's push token data into data. It
// answers 500 itself and returns false when the list can't be read.
func (s *Server) addPushTokenData(w http.ResponseWriter, r *http.Request, me core.User, data map[string]any) bool {
	now := time.Now()
	list, err := s.store.ListPushTokensForUser(r.Context(), me.ID, now)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return false
	}
	views := make([]tokenView, 0, len(list))
	for _, t := range list {
		views = append(views, s.viewToken(t, "", now))
	}
	data["GitHTTP"] = true
	data["GitHTTPRemoteBase"] = strings.TrimRight(s.publicURL, "/")
	data["PushTokens"] = views
	data["TokenDefaultDays"] = s.gitHTTP.DefaultDays
	data["TokenMaxDays"] = s.gitHTTP.MaxDays
	if me.SignIn == core.SignInSSO && me.LastSignInAt != nil {
		data["SSOTokensUntil"] = me.LastSignInAt.Add(s.gitHTTP.SSOMaxAge)
	}
	return true
}

const pushTokenLabelRefusal = "The label must be 1 to 64 characters, with no control characters."

// handleAddPushToken creates a token and answers with the account page showing
// its value. That response is the only place the value ever appears: it is not
// a redirect, and the value is in no URL, cookie, session or flash message.
func (s *Server) handleAddPushToken(w http.ResponseWriter, r *http.Request) {
	label, ok := core.CleanPushTokenLabel(r.FormValue("label"))
	if !ok {
		s.renderAccount(w, r, http.StatusBadRequest, pushTokenLabelRefusal)
		return
	}
	days, err := strconv.Atoi(r.FormValue("days"))
	if err != nil || days < 1 || days > s.gitHTTP.MaxDays {
		s.renderAccount(w, r, http.StatusBadRequest, fmt.Sprintf("Expires in must be a whole number of days from 1 to %d.", s.gitHTTP.MaxDays))
		return
	}
	me, _ := userFrom(r)
	now := time.Now()
	raw, _, err := s.store.CreatePushToken(r.Context(), me.ID, label, now, now.AddDate(0, 0, days))
	switch {
	case errors.Is(err, store.ErrPushTokenLabel):
		s.renderAccount(w, r, http.StatusBadRequest, pushTokenLabelRefusal)
		return
	case errors.Is(err, store.ErrPushTokenLabelTaken):
		s.renderAccount(w, r, http.StatusBadRequest, "You already have a push token with that label.")
		return
	case errors.Is(err, store.ErrPushTokenLimit):
		s.renderAccount(w, r, http.StatusBadRequest, fmt.Sprintf("You can have at most %d push tokens. Revoke one first.", core.MaxPushTokensPerUser))
		return
	case err != nil:
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	s.renderAccountWith(w, r, http.StatusOK, "", map[string]any{"NewPushToken": raw, "NewPushTokenLabel": label})
}

// handleDeletePushToken revokes one of the signed-in account's tokens. Someone
// else's token id deletes nothing.
func (s *Server) handleDeletePushToken(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(r.PathValue("id"))
	if !ok {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	me, _ := userFrom(r)
	if err := s.store.DeletePushTokenForUser(r.Context(), me.ID, id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/account", http.StatusSeeOther)
}

// handleAdminDeletePushToken lets a system admin revoke a token of the account
// in the path. A token that belongs to another account deletes nothing.
func (s *Server) handleAdminDeletePushToken(w http.ResponseWriter, r *http.Request) {
	userID, ok1 := parseID(r.PathValue("id"))
	tokenID, ok2 := parseID(r.PathValue("tokenID"))
	if !ok1 || !ok2 {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if err := s.store.DeletePushTokenForUser(r.Context(), userID, tokenID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/users", http.StatusSeeOther)
}
