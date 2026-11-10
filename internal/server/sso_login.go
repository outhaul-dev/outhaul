package server

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"time"

	"golang.org/x/oauth2"

	"github.com/outhaul-dev/outhaul/internal/sso"
)

// ssoSessionTTL is how long an SSO sign-in lasts on the Outhaul host, so
// someone removed at the provider loses Outhaul within this time. Password
// sessions keep sessionTTL.
const ssoSessionTTL = 8 * time.Hour

// ssoFlowTTL is how long a started sign-in can be finished.
const ssoFlowTTL = 10 * time.Minute

// ssoStateCookiePrefix names a sign-in's own cookie. The rest of the name is
// the first ssoFlowKeyLen characters of its state, so two tabs signing in at
// once each keep theirs. Over HTTPS the name also starts with
// ssoHostCookiePrefix (see ssoFlowCookie).
const ssoStateCookiePrefix = "outhaul_sso_"

const ssoHostCookiePrefix = "__Host-"

const (
	ssoFlowKeyLen    = 12
	ssoFlowPath      = "/auth/sso"
	maxSSOFlowCookie = 4096
)

// ssoFlow is a sign-in in progress. It lives only in its cookie: the server
// keeps nothing, so strangers starting sign-ins can't push anyone else's out.
// The person can read and edit it, which gains them nothing they couldn't do
// as their own client: the provider checks the PKCE verifier, the ID token
// must carry the nonce, and state only ties the callback to this browser.
//
// Return is always empty for now. Whatever uses it must check it again at
// the callback, because the person can edit it.
type ssoFlow struct {
	State, Nonce, Verifier, Return string
	Expires                        time.Time
}

func encodeSSOFlow(f ssoFlow) string {
	b, _ := json.Marshal(f) // strings and a time: it can't fail
	return base64.RawURLEncoding.EncodeToString(b)
}

// decodeSSOFlow reads a flow cookie. ok is false for one that is too large,
// isn't base64url JSON, or lacks a state, nonce, verifier or expiry.
func decodeSSOFlow(v string) (ssoFlow, bool) {
	if v == "" || len(v) > maxSSOFlowCookie {
		return ssoFlow{}, false
	}
	b, err := base64.RawURLEncoding.DecodeString(v)
	if err != nil {
		return ssoFlow{}, false
	}
	var f ssoFlow
	if err := json.Unmarshal(b, &f); err != nil {
		return ssoFlow{}, false
	}
	if f.State == "" || f.Nonce == "" || f.Verifier == "" || f.Expires.IsZero() {
		return ssoFlow{}, false
	}
	return f, true
}

// ssoFlowCookie names and scopes the cookie a flow with this state lives in.
// States are hex (NewToken), so anything else names no cookie.
//
// Over HTTPS it is a __Host- cookie: a browser accepts one only from this
// host itself, Secure, with Path=/ and no Domain, so a sibling subdomain (a
// preview or an app) can't toss one in. Plain HTTP, for local development,
// can't use the prefix, so it keeps the plain name scoped to /auth/sso. The
// callback looks only for the name of its own scheme.
func ssoFlowCookie(r *http.Request, state string) (name, path string, ok bool) {
	if len(state) < ssoFlowKeyLen {
		return "", "", false
	}
	key := state[:ssoFlowKeyLen]
	for i := 0; i < len(key); i++ {
		if c := key[i]; !('0' <= c && c <= '9' || 'a' <= c && c <= 'f') {
			return "", "", false
		}
	}
	if requestIsHTTPS(r) {
		return ssoHostCookiePrefix + ssoStateCookiePrefix + key, "/", true
	}
	return ssoStateCookiePrefix + key, ssoFlowPath, true
}

// ssoLoginErrors are the only messages a failed SSO sign-in can put on the
// login page. The provider's own text is logged, never shown.
var ssoLoginErrors = map[string]string{
	"sso_unavailable": "Single sign-on isn't available right now. You can still sign in with a password.",
	"sso_failed":      "Signing in with SSO didn't work. Try again.",
	"sso_no_email":    "Your SSO account has no email address or username, so Outhaul can't sign you in. Ask whoever runs your sign-in provider.",
}

// ssoConnected reports whether the login page offers SSO. It reads only the
// settings: a provider that is down is reported when someone tries it.
func (s *Server) ssoConnected(ctx context.Context) bool {
	if !s.publicURLSet() {
		return false
	}
	_, ok, err := s.store.GetSSOSettings(ctx)
	if err != nil {
		log.Printf("sso: reading the settings for the login page: %v", err)
		return false
	}
	return ok
}

// ssoRefused sends the browser back to the login page with one of the fixed
// error codes in ssoLoginErrors.
func ssoRefused(w http.ResponseWriter, r *http.Request, code string) {
	http.Redirect(w, r, "/login?error="+code, http.StatusSeeOther)
}

// handleSSOStart begins a sign-in: it puts a new flow in its own cookie and
// sends the browser to the provider.
func (s *Server) handleSSOStart(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if _, ok := s.currentSession(r); ok {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	p, err := s.ssoProvider(r.Context())
	if errors.Is(err, errSSONotConfigured) {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	if err != nil {
		log.Printf("sso: starting a sign-in: %v", err)
		ssoRefused(w, r, "sso_unavailable")
		return
	}
	f := ssoFlow{
		State:    NewToken(),
		Nonce:    NewToken(),
		Verifier: oauth2.GenerateVerifier(),
		Expires:  time.Now().Add(ssoFlowTTL),
	}
	name, path, _ := ssoFlowCookie(r, f.State)
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    encodeSSOFlow(f),
		Path:     path,
		MaxAge:   int(ssoFlowTTL / time.Second),
		HttpOnly: true,
		Secure:   requestIsHTTPS(r),
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, p.AuthURL(f.State, f.Nonce, f.Verifier), http.StatusFound)
}

// takeSSOFlow finds the flow named by state, clears its cookie whatever
// happens next, and decodes it. The provider accepts a code once and the
// cookie is gone after the first callback, so a replay has nothing to use.
func takeSSOFlow(w http.ResponseWriter, r *http.Request, state string) (ssoFlow, bool) {
	name, path, ok := ssoFlowCookie(r, state)
	if !ok {
		return ssoFlow{}, false
	}
	c, err := r.Cookie(name)
	if err != nil {
		return ssoFlow{}, false
	}
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    "",
		Path:     path,
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   requestIsHTTPS(r),
		SameSite: http.SameSiteLaxMode,
	})
	return decodeSSOFlow(c.Value)
}

// logText bounds text that came in a request before it is logged.
func logText(s string) string {
	const max = 200
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}

// handleSSOCallback finishes a sign-in. It never logs or shows the code,
// state, nonce or verifier.
func (s *Server) handleSSOCallback(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	q := r.URL.Query()
	state := q.Get("state")
	flow, ok := takeSSOFlow(w, r, state)
	if !ok {
		log.Printf("sso: sign-in refused: this browser has no sign-in in progress for that state")
		ssoRefused(w, r, "sso_failed")
		return
	}
	if subtle.ConstantTimeCompare([]byte(flow.State), []byte(state)) != 1 {
		log.Printf("sso: sign-in refused: the state doesn't match this browser's sign-in")
		ssoRefused(w, r, "sso_failed")
		return
	}
	if !time.Now().Before(flow.Expires) {
		log.Printf("sso: sign-in refused: it was started more than %v ago", ssoFlowTTL)
		ssoRefused(w, r, "sso_failed")
		return
	}
	if e := q.Get("error"); e != "" {
		log.Printf("sso: the provider refused the sign-in: %q %q", logText(e), logText(q.Get("error_description")))
		ssoRefused(w, r, "sso_failed")
		return
	}
	code := q.Get("code")
	if code == "" {
		log.Printf("sso: sign-in refused: the provider sent no code")
		ssoRefused(w, r, "sso_failed")
		return
	}
	p, err := s.ssoProvider(r.Context())
	if err != nil {
		log.Printf("sso: finishing a sign-in: %v", err)
		ssoRefused(w, r, "sso_unavailable")
		return
	}
	id, err := p.Exchange(r.Context(), code, flow.Nonce, flow.Verifier)
	if errors.Is(err, sso.ErrNoEmail) {
		log.Printf("sso: sign-in refused: %v", err)
		ssoRefused(w, r, "sso_no_email")
		return
	}
	if err != nil {
		log.Printf("sso: sign-in refused: %v", err)
		ssoRefused(w, r, "sso_failed")
		return
	}
	// Removing SSO, or changing its issuer or client ID, ends every SSO
	// sign-in. One that was talking to the provider meanwhile ends too.
	if !s.ssoStillCurrent(r.Context(), p) {
		log.Printf("sso: sign-in refused: the SSO settings changed while it ran")
		ssoRefused(w, r, "sso_failed")
		return
	}
	user, err := s.store.FindOrCreateSSOUser(r.Context(), id.Issuer, id.Subject, id.Email)
	if err != nil {
		log.Printf("sso: finding the account for a sign-in: %v", err)
		ssoRefused(w, r, "sso_failed")
		return
	}
	token, expires, err := s.createSession(r.Context(), user)
	if err != nil {
		log.Printf("sso: signing in account %d: %v", user.ID, err)
		ssoRefused(w, r, "sso_failed")
		return
	}
	// Checked again now the session exists: a change that committed since the
	// check above is seen here, and one that commits after this ends the new
	// session in its own transaction.
	if !s.ssoStillCurrent(r.Context(), p) {
		if err := s.store.DeleteSession(r.Context(), token); err != nil {
			log.Printf("sso: ending account %d's refused session: %v", user.ID, err)
		}
		log.Printf("sso: sign-in refused: the SSO settings changed while it ran")
		ssoRefused(w, r, "sso_failed")
		return
	}
	var old string
	if c, err := r.Cookie(sessionCookie); err == nil {
		old = c.Value
	}
	s.finishLogin(w, r, user, token, expires)
	// The new session replaces any this browser had; the old one stops working.
	if old != "" {
		if err := s.store.DeleteSession(r.Context(), old); err != nil {
			log.Printf("sso: ending the session replaced by account %d's sign-in: %v", user.ID, err)
		}
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
