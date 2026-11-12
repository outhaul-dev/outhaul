package server

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/outhaul-dev/outhaul/internal/core"
	"github.com/outhaul-dev/outhaul/internal/store"
)

// Signing in and out for protected routes. The gate sends a visitor to
// /gate/start on the public URL with rd, the address they asked for, and n,
// the SHA-256 hex of the pending cookie it set on that address's host. Once
// they are signed in there (a gateway session, in the gateway cookie on
// Outhaul's host), redirectWithCode sends them back to rd with a one-time
// code that works only alongside that pending cookie.
//
// None of this logs rd, n, a code, a token or a password, and every answer
// is no-store and no-referrer: rd and n are in the page's URL, and a code is
// in a redirect's.

// errNotProtected means a return address isn't one the gate answers for.
var errNotProtected = errors.New("not a protected address")

// errOddPath means a return address was refused for how its path is spelled:
// a browser or Traefik could read it differently from the gate.
var errOddPath = errors.New("a path spelled in a way the gate doesn't sign in to")

// errBadCredentials means a username and password sign nobody in.
var errBadCredentials = errors.New("invalid credentials")

const (
	// gateWrongHostCopy answers a sign-in posted anywhere but the public
	// URL's host, where the gateway cookie couldn't be kept.
	gateWrongHostCopy = "Sign in at Outhaul's own address. Open the app's address again to get there."
	// gateSignOutFailedCopy is shown when a gateway session couldn't be ended.
	gateSignOutFailedCopy = "Signing out didn't work. Try again."
)

// gatePrivate marks an answer that names rd, n or a code: no cache keeps it,
// and no Referer carries its URL anywhere.
func gatePrivate(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
}

// gatePage marks a page a person reads, in place of gatePrivate: no cache
// keeps it, and its URL goes in a Referer only to Outhaul's own origin, so
// nothing outside Outhaul sees rd or n. Redirects keep no-referrer. Under
// no-referrer a browser that sends no Sec-Fetch-Site posts the page's forms
// with Origin: null, which cross-origin protection refuses; under
// same-origin it sends the page's own origin.
func gatePage(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "same-origin")
}

// gateCookieName names the gateway cookie. Like the SSO flow cookie it
// follows the public URL, where it is set and read, not the request: on an
// https:// public URL it is a __Host- cookie, which a browser takes only from
// this host itself, Secure, with Path=/ and no Domain, so no app on a sibling
// subdomain can set or shadow it.
func (s *Server) gateCookieName() string {
	if s.publicURLIsHTTPS() {
		return ssoHostCookiePrefix + gateCookieBase
	}
	return gateCookieBase
}

// gateCookie is the gateway cookie, with no value or lifetime yet.
func (s *Server) gateCookie() *http.Cookie {
	return &http.Cookie{
		Name:     s.gateCookieName(),
		Path:     "/",
		HttpOnly: true,
		Secure:   s.publicURLIsHTTPS(),
		SameSite: http.SameSiteLaxMode,
	}
}

func (s *Server) setGateCookie(w http.ResponseWriter, token string) {
	c := s.gateCookie()
	c.Value, c.MaxAge = token, int(gateSessionTTL/time.Second)
	http.SetCookie(w, c)
}

func (s *Server) clearGateCookie(w http.ResponseWriter) {
	c := s.gateCookie()
	c.MaxAge = -1
	http.SetCookie(w, c)
}

// gateToken is the request's gateway cookie, or "" for none the gate could
// have made.
func (s *Server) gateToken(r *http.Request) string {
	c, err := r.Cookie(s.gateCookieName())
	if err != nil || c.Value == "" || len(c.Value) > maxRouteToken {
		return ""
	}
	return c.Value
}

// gateSession returns the request's live gateway session's token. ok is false
// with no cookie, or one for a session that has ended. err is for a store
// that failed.
func (s *Server) gateSession(r *http.Request) (token string, ok bool, err error) {
	token = s.gateToken(r)
	if token == "" {
		return "", false, nil
	}
	_, err = s.store.GetGateSession(r.Context(), token)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return token, true, nil
}

// startGateSession starts a gateway session for subject and sets its cookie,
// so the person can pass protected routes without signing in again. It does
// this only on the public URL's host: anywhere else the browser may drop the
// cookie, leaving a session nobody holds, so it returns "" and no error.
func (s *Server) startGateSession(w http.ResponseWriter, r *http.Request, subject, kind, display string) (string, error) {
	token, err := s.createGateSession(r, subject, kind, display)
	if token != "" {
		s.setGateCookie(w, token)
	}
	return token, err
}

// createGateSession is startGateSession without the cookie, for a sign-in
// that can still be refused: the caller sets the cookie with setGateCookie
// once it can't, so a refusal leaves the browser's cookie as it was.
func (s *Server) createGateSession(r *http.Request, subject, kind, display string) (string, error) {
	if !s.onPublicHost(r) {
		return "", nil
	}
	token := NewToken()
	if err := s.store.CreateGateSession(r.Context(), token, core.GateSession{
		Subject: subject, Kind: kind, Display: display, ExpiresAt: time.Now().Add(gateSessionTTL),
	}); err != nil {
		return "", err
	}
	return token, nil
}

// startPasswordGateSession is startGateSession for a password account whose
// password was just checked against user.PasswordHash. The session is made
// only if the account still has that hash: a reset or change that committed
// during the check has ended the account's gateway sessions, and this one
// mustn't outlive it. That case returns store.ErrPasswordChanged.
func (s *Server) startPasswordGateSession(w http.ResponseWriter, r *http.Request, user core.User) (string, error) {
	if !s.onPublicHost(r) {
		return "", nil
	}
	token := NewToken()
	if err := s.store.CreatePasswordGateSession(r.Context(), token, user.ID, user.PasswordHash, time.Now().Add(gateSessionTTL)); err != nil {
		return "", err
	}
	s.setGateCookie(w, token)
	return token, nil
}

// endReplacedGateSession ends the gateway session this browser held before
// keep replaced it, and with it the route sessions it carried, so whoever
// signs in next on a shared browser doesn't pass as the last person. It is
// called once a sign-in can no longer be refused.
func (s *Server) endReplacedGateSession(r *http.Request, keep string) {
	old := s.gateToken(r)
	if keep == "" || old == "" || old == keep {
		return
	}
	if err := s.store.DeleteGateSession(context.WithoutCancel(r.Context()), old); err != nil {
		log.Printf("gate: ending a replaced gateway session: %v", err)
	}
}

// endGateSession ends the browser's gateway session, with its route
// sessions, and clears its cookie. A delete that fails is tried once more,
// without the request's context, which a browser that gave up has cancelled.
// If that fails too, nothing is cleared and the error is returned, so the
// person can try again.
func (s *Server) endGateSession(w http.ResponseWriter, r *http.Request) error {
	if token := s.gateToken(r); token != "" {
		if err := s.store.DeleteGateSession(r.Context(), token); err != nil {
			if err := s.store.DeleteGateSession(context.WithoutCancel(r.Context()), token); err != nil {
				return err
			}
		}
	}
	s.clearGateCookie(w)
	return nil
}

// validNonce reports whether n is what the gate puts in a sign-in URL: the
// SHA-256 hex of a pending cookie, 64 lower-case hex characters.
func validNonce(n string) bool { return isLowerHex64(n) }

func isLowerHex64(v string) bool {
	if len(v) != 64 {
		return false
	}
	for i := 0; i < len(v); i++ {
		if b := v[i]; !('0' <= b && b <= '9' || 'a' <= b && b <= 'f') {
			return false
		}
	}
	return true
}

// publicHost is the public URL's host as the gate compares hosts, or "".
func (s *Server) publicHost() string {
	if !s.publicURLSet() {
		return ""
	}
	u, err := url.Parse(s.publicURL)
	if err != nil {
		return ""
	}
	return normalizeHost(u.Host)
}

// validReturnURL checks that raw is an address the gate answers for, and
// returns it rebuilt from its parsed parts, which is the only form anything
// redirects to, and its host as the gate stores hosts. It returns
// errNotProtected for any other address, and another error when the store
// fails; host is set whenever raw's host could be read, for logging.
//
// The address is https, with no userinfo and no fragment (even an empty
// one, which a code added later would land in). Its authority is plain
// ASCII, written as Go read it, with a plausible host that isn't Outhaul's
// own and the port the app's host is served on (appPort). Its path is one a
// browser and Traefik can't read differently from the gate: no backslash,
// no escaped dot, slash or backslash, nothing path.Clean would change apart
// from a trailing slash. And the domain row Traefik would pick for it is
// protected, whether Traefik reads the path decoded or as written. A path
// refused for its spelling, or protected read one way and not the other,
// returns errOddPath instead, so the person can be told what to do.
func (s *Server) validReturnURL(ctx context.Context, raw string) (u *url.URL, host string, err error) {
	const scheme = "https://"
	if len(raw) < len(scheme) || !strings.EqualFold(raw[:len(scheme)], scheme) || strings.Contains(raw, "#") {
		return nil, "", errNotProtected
	}
	authority := raw[len(scheme):]
	if i := strings.IndexAny(authority, "/?"); i >= 0 {
		authority = authority[:i]
	}
	for i := 0; i < len(authority); i++ {
		if c := authority[i]; c <= ' ' || c >= 0x7f || c == '%' {
			return nil, "", errNotProtected
		}
	}
	p, err := url.Parse(raw)
	if err != nil || p.Scheme != "https" || p.User != nil || p.Opaque != "" || p.Host == "" || p.Host != authority {
		return nil, "", errNotProtected
	}
	host = normalizeHost(p.Host)
	if !plausibleHost(host) || host == s.publicHost() {
		return nil, "", errNotProtected
	}
	port, err := s.appPort(ctx)
	if err != nil {
		return nil, host, err
	}
	if got := p.Port(); got != port && !(port == "" && got == "443") {
		return nil, host, errNotProtected
	}

	decoded, escaped := p.Path, p.EscapedPath()
	if decoded == "" {
		decoded, escaped = "/", "/"
	}
	lower := strings.ToLower(escaped)
	if strings.Contains(decoded, `\`) || strings.Contains(escaped, `\`) ||
		strings.Contains(lower, "%2e") || strings.Contains(lower, "%2f") || strings.Contains(lower, "%5c") {
		return nil, host, errOddPath
	}
	clean := path.Clean(decoded)
	if clean != "/" && strings.HasSuffix(decoded, "/") {
		clean += "/"
	}
	if clean != decoded {
		return nil, host, errOddPath
	}
	protected := 0
	for _, urlPath := range []string{decoded, escaped} {
		ok, err := s.store.RouteIsProtected(ctx, host, urlPath)
		if err != nil {
			return nil, host, err
		}
		if ok {
			protected++
		}
	}
	switch protected {
	case 0:
		return nil, host, errNotProtected
	case 1:
		return nil, host, errOddPath
	}

	u = &url.URL{Scheme: "https", Host: host, Path: decoded, RawPath: p.RawPath, RawQuery: escapeUnsafe(p.RawQuery)}
	if port != "" {
		u.Host += ":" + port
	}
	return u, host, nil
}

// refuseReturnURL answers a return address validReturnURL refused. One
// refused for its path's spelling gets a page saying so, with a link to the
// app's main page when that is protected, as a sign-in for it would work.
func (s *Server) refuseReturnURL(w http.ResponseWriter, r *http.Request, host string, err error) {
	switch {
	case errors.Is(err, errOddPath):
		data := map[string]any{"Title": "Sign in", "HideChrome": true}
		if root, err := s.store.RouteIsProtected(r.Context(), host, "/"); err == nil && root {
			if port, err := s.appPort(r.Context()); err == nil {
				link := url.URL{Scheme: "https", Host: host, Path: "/"}
				if port != "" {
					link.Host += ":" + port
				}
				data["Link"] = link.String()
			}
		}
		gatePage(w)
		s.render(w, r, http.StatusBadRequest, "gate_refused", data)
	case errors.Is(err, errNotProtected):
		http.Error(w, gateNotProtectedCopy, http.StatusBadRequest)
	default:
		gateUnavailable(w, host, "checking a return address", err)
	}
}

// redirectWithCode sends the browser back to returnURL with a one-time code
// that carries the gateway session rawGateToken to its host, and works only
// in the browser whose pending cookie hashes to nonceHash. It checks both
// itself, so nothing can mint a code for an address or a browser that wasn't
// checked, and redirects to the address rebuilt by validReturnURL, without
// any code it already had. The code is added to the raw query as it is: a
// round trip through url.Values would drop pairs separated by ";". 303 makes
// the browser follow with a GET navigation, which sends the Lax pending
// cookie.
func (s *Server) redirectWithCode(w http.ResponseWriter, r *http.Request, rawGateToken, returnURL, nonceHash string) {
	if !validNonce(nonceHash) {
		http.Error(w, gateBadLinkCopy, http.StatusBadRequest)
		return
	}
	u, host, err := s.validReturnURL(r.Context(), returnURL)
	if err != nil {
		s.refuseReturnURL(w, r, host, err)
		return
	}
	if rawGateToken == "" {
		gateUnavailable(w, host, "minting a sign-in code", errors.New("no gateway session"))
		return
	}
	code := NewToken()
	if err := s.store.CreateGateCode(r.Context(), code, rawGateToken, host, nonceHash, time.Now().Add(gateCodeTTL)); err != nil {
		gateUnavailable(w, host, "minting a sign-in code", err)
		return
	}
	_, rest, _ := takeGateCode(u.RawQuery)
	if rest != "" {
		rest += "&"
	}
	u.RawQuery = rest + gateCodeParam + "=" + code
	http.Redirect(w, r, u.String(), http.StatusSeeOther)
}

// gateLoginPage is the sign-in page for a route: the usual one, naming the
// app's host, posting to /gate/sign-in with rd and n, and with an SSO button
// that carries them. A return address too long to travel through SSO gets no
// button.
func (s *Server) gateLoginPage(w http.ResponseWriter, r *http.Request, status int, host, rd, n, errMsg string) {
	data := map[string]any{"Title": "Sign in", "HideChrome": true, "GateHost": host, "RD": rd, "N": n}
	if len(rd) <= maxSSOReturn && s.ssoConnected(r.Context()) {
		data["SSO"] = true
		data["SSOStart"] = "/auth/sso/start?" + url.Values{"rd": {rd}, "n": {n}}.Encode()
	}
	if errMsg != "" {
		data["Error"] = errMsg
	}
	gatePage(w)
	s.render(w, r, status, "login", data)
}

// isPrefetch reports whether r is a browser fetching a page ahead of a
// navigation that may never happen: Sec-Purpose (prefetch, or prefetch for
// a prerender), or the older Purpose and X-Moz.
func isPrefetch(r *http.Request) bool {
	for _, h := range []string{"Sec-Purpose", "Purpose", "X-Moz"} {
		for _, v := range r.Header.Values(h) {
			if strings.Contains(strings.ToLower(v), "prefetch") {
				return true
			}
		}
	}
	return false
}

// handleGateStart is where the gate sends a visitor to sign in. With a live
// gateway session it sends them straight back with a code; that is safe only
// because the code works in nothing but the browser that holds the pending
// cookie n stands for. Anyone else sees the sign-in page for that address.
//
// Only a GET navigation mints a code. A HEAD or a prefetch with a live
// gateway session is answered without one: minting replaces the session's
// earlier code for the host, which a real navigation may still be carrying.
func (s *Server) handleGateStart(w http.ResponseWriter, r *http.Request) {
	gatePrivate(w)
	if !s.publicURLSet() {
		http.Error(w, gateUnavailableCopy, http.StatusServiceUnavailable)
		return
	}
	// The gateway cookie belongs to the public URL's host, so a sign-in that
	// arrives anywhere else moves there first, as SSO's start does.
	if to, pubHost, ok := s.ssoStartElsewhere(r); ok {
		if r.URL.Query().Has(ssoMovedParam) {
			if s.gateMovedLog.allow(time.Now()) {
				log.Printf("gate: sign-in page refused: it was sent on to %q, the public URL's host, but arrived on %q; a proxy in front may be rewriting the Host header (logged at most once a minute)",
					pubHost, logText(r.Host))
			}
			http.Error(w, gateUnavailableCopy, http.StatusServiceUnavailable)
			return
		}
		http.Redirect(w, r, to, http.StatusFound)
		return
	}
	q := r.URL.Query()
	rd, n := q.Get("rd"), q.Get("n")
	if !validNonce(n) {
		http.Error(w, gateBadLinkCopy, http.StatusBadRequest)
		return
	}
	_, host, err := s.validReturnURL(r.Context(), rd)
	if err != nil {
		s.refuseReturnURL(w, r, host, err)
		return
	}
	token, ok, err := s.gateSession(r)
	if err != nil {
		gateUnavailable(w, host, "reading a gateway session", err)
		return
	}
	if ok {
		switch {
		case r.Method == http.MethodHead:
			w.WriteHeader(http.StatusOK)
			return
		case isPrefetch(r):
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if s.gateLooping(w, r, host, n) {
			gatePage(w)
			http.Error(w, "Your browser didn't keep the sign-in cookie for "+host+
				", so it would keep coming back here. Allow cookies for that address, then open it again.", http.StatusBadRequest)
			return
		}
		s.redirectWithCode(w, r, token, rd, n)
		return
	}
	// Only a known code shows anything, and only its fixed message.
	s.gateLoginPage(w, r, http.StatusOK, host, rd, n, ssoLoginErrors[q.Get("error")])
}

// gateLapsCookie is the cookie that counts arrivals for gateLooping, with no
// value or lifetime yet.
func (s *Server) gateLapsCookie() *http.Cookie {
	c := s.gateCookie()
	c.Name = gateLapsBase
	if s.publicURLIsHTTPS() {
		c.Name = ssoHostCookiePrefix + gateLapsBase
	}
	return c
}

// gateLooping counts this browser's arrival at /gate/start, signed in and
// bound for host with nonce n, and reports whether it is the gateMaxLaps-th
// lap for that host within gateLapsTTL of the last. A browser that refuses
// the app host's cookies never keeps the pending or route cookie, so with a
// gateway session it would otherwise go round between the gate and here for
// ever, bringing a new n each time. Tabs that share one pending cookie bring
// the same n, so an arrival with the previous lap's n isn't counted.
//
// The count lives in a cookie on this host, as count:host:tag, where tag
// stands for n without being it. An arrival for another host starts it again,
// and reaching the limit clears it, so trying again after the page starts
// afresh.
func (s *Server) gateLooping(w http.ResponseWriter, r *http.Request, host, n string) bool {
	c := s.gateLapsCookie()
	tag := sha256Hex(n)[:16]
	laps, last := 0, ""
	if got, err := r.Cookie(c.Name); err == nil {
		if count, rest, ok := strings.Cut(got.Value, ":"); ok {
			if h, t, ok := strings.Cut(rest, ":"); ok && h == host {
				if v, err := strconv.Atoi(count); err == nil && v > 0 {
					laps, last = v, t
				}
			}
		}
	}
	if tag != last {
		laps++
	}
	if laps >= gateMaxLaps {
		c.MaxAge = -1
		http.SetCookie(w, c)
		return true
	}
	c.Value = strconv.Itoa(laps) + ":" + host + ":" + tag
	c.MaxAge = int(gateLapsTTL / time.Second)
	http.SetCookie(w, c)
	return false
}

// checkPassword finds the password account named username and checks
// password against it, in a hash slot. It returns errHashBusy when no slot
// freed up in time, and errBadCredentials for an unknown name, an account
// whose password isn't set yet, an SSO account (which the store never
// returns here anyway), or a wrong password. The account it returns carries
// the hash that was checked, for the store to check again as it makes a
// session.
func (s *Server) checkPassword(ctx context.Context, username, password string) (core.User, error) {
	user, err := s.store.GetUserByName(ctx, username)
	known := err == nil
	if s.signIn != nil {
		wait, ok := s.signIn.allow(username, known)
		if !ok {
			if known && s.signInLog.allow(time.Now()) {
				log.Printf("server: sign-in for %q is locked after repeated failures", logText(username))
			}
			return core.User{}, &signInLockedError{wait: wait}
		}
	}
	if err != nil || user.PasswordHash == "" || user.SignIn != core.SignInPassword {
		return core.User{}, errBadCredentials
	}
	var ok bool
	if busy := s.withHashSlot(ctx, func() { ok, err = VerifyPassword(user.PasswordHash, password) }); busy != nil {
		if s.signIn != nil {
			s.signIn.giveBack(username, known)
		}
		return core.User{}, busy
	}
	if err != nil || !ok {
		return core.User{}, errBadCredentials
	}
	if s.signIn != nil {
		s.signIn.clear(username)
	}
	return user, nil
}

// handleGateSignIn signs a person in with a password for a route: a gateway
// session, and no Outhaul session, so an account with no role can use it.
// It makes the same checks as /login.
func (s *Server) handleGateSignIn(w http.ResponseWriter, r *http.Request) {
	gatePrivate(w)
	if !s.publicURLSet() {
		http.Error(w, gateUnavailableCopy, http.StatusServiceUnavailable)
		return
	}
	if !s.onPublicHost(r) {
		http.Error(w, gateWrongHostCopy, http.StatusBadRequest)
		return
	}
	rd, n := r.PostFormValue("rd"), r.PostFormValue("n")
	if !validNonce(n) {
		http.Error(w, gateBadLinkCopy, http.StatusBadRequest)
		return
	}
	_, host, err := s.validReturnURL(r.Context(), rd)
	if err != nil {
		s.refuseReturnURL(w, r, host, err)
		return
	}
	user, err := s.checkPassword(r.Context(), r.PostFormValue("username"), r.PostFormValue("password"))
	var locked *signInLockedError
	if errors.As(err, &locked) {
		w.Header().Set("Retry-After", strconv.Itoa(locked.retryAfter()))
		s.gateLoginPage(w, r, http.StatusTooManyRequests, host, rd, n, locked.Error())
		return
	}
	if errors.Is(err, errHashBusy) {
		s.gateLoginPage(w, r, http.StatusServiceUnavailable, host, rd, n, busyMessage)
		return
	}
	if err != nil {
		s.gateLoginPage(w, r, http.StatusUnauthorized, host, rd, n, "Invalid username or password.")
		return
	}
	token, err := s.startPasswordGateSession(w, r, user)
	if errors.Is(err, store.ErrPasswordChanged) {
		s.gateLoginPage(w, r, http.StatusUnauthorized, host, rd, n, "Invalid username or password.")
		return
	}
	if err != nil {
		gateUnavailable(w, host, "starting a gateway session", err)
		return
	}
	s.endReplacedGateSession(r, token)
	// Recording the time is for the Users page; it must not block signing in.
	if err := s.store.TouchUserSignIn(r.Context(), user.ID); err != nil {
		log.Printf("gate: record sign-in for user %d: %v", user.ID, err)
	}
	s.redirectWithCode(w, r, token, rd, n)
}

// handleGateSignOutPage asks before signing out, so a link can't do it.
func (s *Server) handleGateSignOutPage(w http.ResponseWriter, r *http.Request) {
	gatePage(w)
	s.render(w, r, http.StatusOK, "gate_signout", map[string]any{"Title": "Sign out", "HideChrome": true})
}

// handleGateSignOut ends the browser's gateway session and every route
// session it carried. An Outhaul session is left alone: /logout ends that.
func (s *Server) handleGateSignOut(w http.ResponseWriter, r *http.Request) {
	gatePage(w)
	if err := s.endGateSession(w, r); err != nil {
		log.Printf("gate: signing out: %v", err)
		http.Error(w, gateSignOutFailedCopy, http.StatusServiceUnavailable)
		return
	}
	s.render(w, r, http.StatusOK, "gate_signout", map[string]any{"Title": "Signed out", "HideChrome": true, "Done": true})
}
