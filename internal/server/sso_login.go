package server

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/oauth2"

	"github.com/outhaul-dev/outhaul/internal/core"
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
// once each keep theirs. On an HTTPS public URL the name also starts with
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
// Return and GateNonce are set for a route visit: the address to go back to
// with a code, and n, the hash of the pending cookie the code is bound to.
// The callback checks both again, because the person can edit them and a
// row may have changed meanwhile.
type ssoFlow struct {
	State, Nonce, Verifier string
	Return, GateNonce      string
	Expires                time.Time
}

// routeVisit reports whether the flow signs in for a protected route rather
// than to Outhaul.
func (f ssoFlow) routeVisit() bool { return f.Return != "" || f.GateNonce != "" }

// encodeSSOFlow encodes a flow for its cookie. HTML escaping is off, so a
// return address full of "&" doesn't grow sixfold on its way into a cookie
// that must stay under 4 KB.
func encodeSSOFlow(f ssoFlow) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(f) // strings and a time: it can't fail
	return base64.RawURLEncoding.EncodeToString(bytes.TrimSuffix(b.Bytes(), []byte("\n")))
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

// ssoFlowCookie is the cookie a flow with this state lives in, with no value
// or lifetime yet: start fills it in, and the callback finds it by name and
// clears it. States are hex (NewToken), so anything else names no cookie.
//
// Its name, path and Secure flag follow the public URL's scheme, not the
// request's, because the callback always arrives on the public URL: in tunnel
// mode Traefik serves plain HTTP and says so in X-Forwarded-Proto while
// browsers are on HTTPS. On an HTTPS public URL it is a __Host- cookie: a
// browser accepts one only from this host itself, Secure, with Path=/ and no
// Domain, so a sibling subdomain (a preview or an app) can't toss one in. A
// plain-HTTP public URL, for local development, can't use the prefix, so it
// keeps the plain name scoped to /auth/sso. The callback looks only for that
// one name.
func (s *Server) ssoFlowCookie(state string) (*http.Cookie, bool) {
	if len(state) < ssoFlowKeyLen {
		return nil, false
	}
	key := state[:ssoFlowKeyLen]
	for i := 0; i < len(key); i++ {
		if c := key[i]; !('0' <= c && c <= '9' || 'a' <= c && c <= 'f') {
			return nil, false
		}
	}
	c := &http.Cookie{
		Name:     ssoStateCookiePrefix + key,
		Path:     ssoFlowPath,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	}
	if s.publicURLIsHTTPS() {
		c.Name, c.Path, c.Secure = ssoHostCookiePrefix+c.Name, "/", true
	}
	return c, true
}

// publicURLIsHTTPS reports whether the public URL is https://. Cookies for
// pages that only run on the public URL take their Secure flag and __Host-
// prefix from it, not from how the request looks.
func (s *Server) publicURLIsHTTPS() bool {
	u, err := url.Parse(s.publicURL)
	return err == nil && u.Scheme == "https"
}

// onPublicHost reports whether r arrived on the public URL's host. Hosts are
// compared with their ports, ignoring case, one trailing dot and a port the
// public URL's scheme implies. It is false when no public URL is set.
func (s *Server) onPublicHost(r *http.Request) bool {
	if !s.publicURLSet() {
		return false
	}
	pub, err := url.Parse(s.publicURL)
	if err != nil || pub.Host == "" {
		return false
	}
	return hostWithoutDefaultPort(r.Host, pub.Scheme) == hostWithoutDefaultPort(pub.Host, pub.Scheme)
}

// ssoMovedParam marks a start that ssoStartElsewhere has already sent to the
// public URL. One that arrives on another host all the same was moved by a
// proxy in front that rewrites Host, and moving it again would loop.
const ssoMovedParam = "moved"

// ssoStartElsewhere is where a sign-in started on another host than the
// public URL's must start instead: the same path and query on the public URL,
// marked with ssoMovedParam. The flow cookie belongs to the host that answers
// start, and the provider sends the browser back to the public URL, so a
// sign-in started anywhere else could never finish. On the public URL's own
// host ok is false. pubHost is the public URL's host, for logging.
func (s *Server) ssoStartElsewhere(r *http.Request) (to, pubHost string, ok bool) {
	if !s.publicURLSet() || s.onPublicHost(r) {
		return "", "", false
	}
	pub, err := url.Parse(s.publicURL)
	if err != nil || pub.Host == "" {
		return "", "", false
	}
	q := r.URL.RawQuery
	if q != "" {
		q += "&"
	}
	u := url.URL{Scheme: pub.Scheme, Host: pub.Host, Path: r.URL.Path, RawQuery: q + ssoMovedParam + "=1"}
	return u.String(), pub.Host, true
}

// hostWithoutDefaultPort lowercases a host[:port], drops one trailing dot
// from the host, which names the same host fully qualified, and drops the
// port scheme implies, as browsers leave it out of the Host header.
func hostWithoutDefaultPort(hostport, scheme string) string {
	hostport = strings.ToLower(hostport)
	host, port, err := net.SplitHostPort(hostport)
	if err != nil { // no port
		return strings.TrimSuffix(hostport, ".")
	}
	host = strings.TrimSuffix(host, ".")
	if port == "" || scheme == "https" && port == "443" || scheme == "http" && port == "80" {
		if strings.Contains(host, ":") {
			return "[" + host + "]"
		}
		return host
	}
	return net.JoinHostPort(host, port)
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
	_, ok, err := s.ssoSettings(ctx)
	if err != nil {
		log.Printf("sso: reading the settings for the login page: %v", err)
		return false
	}
	return ok
}

// maxConcurrentSSOExchanges bounds calls to the provider's token endpoint in
// flight. Anyone can make up a flow cookie and so make the callback call it.
const maxConcurrentSSOExchanges = 4

// errSSOBusy means no exchange slot freed up before the request ended.
var errSSOBusy = errors.New("too many sign-ins are being finished at once")

// defaultSSOExchangeWait is how long a callback waits for an exchange slot
// before the person is told SSO isn't available.
const defaultSSOExchangeWait = 10 * time.Second

// withExchangeSlot runs fn, which calls the provider's token endpoint, once
// one of the maxConcurrentSSOExchanges slots is free. It returns errSSOBusy,
// without running fn, when no slot frees within s.ssoExchangeWait or ctx ends
// first.
func (s *Server) withExchangeSlot(ctx context.Context, fn func()) error {
	t := time.NewTimer(s.ssoExchangeWait)
	defer t.Stop()
	select {
	case s.ssoExchanges <- struct{}{}:
	case <-t.C:
		return errSSOBusy
	case <-ctx.Done():
		return errSSOBusy
	}
	defer func() { <-s.ssoExchanges }()
	fn()
	return nil
}

// ssoRefused sends the browser back to the login page with one of the fixed
// error codes in ssoLoginErrors.
func ssoRefused(w http.ResponseWriter, r *http.Request, code string) {
	http.Redirect(w, r, "/login?error="+code, http.StatusSeeOther)
}

// ssoRefusedFor is ssoRefused for a sign-in whose flow was read. A route
// visit goes back to the gate's sign-in page for its address, which checks
// rd and n again, so the person can still sign in with a password.
func ssoRefusedFor(w http.ResponseWriter, r *http.Request, f ssoFlow, code string) {
	if !f.routeVisit() {
		ssoRefused(w, r, code)
		return
	}
	http.Redirect(w, r, "/gate/start?"+url.Values{"rd": {f.Return}, "n": {f.GateNonce}, "error": {code}}.Encode(), http.StatusSeeOther)
}

// handleSSOStart begins a sign-in: it puts a new flow in its own cookie and
// sends the browser to the provider. With rd or n it signs in for a
// protected route, which the gate's sign-in page links to; both are checked
// as /gate/start checks them.
func (s *Server) handleSSOStart(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	if to, pubHost, ok := s.ssoStartElsewhere(r); ok {
		if r.URL.Query().Has(ssoMovedParam) {
			if s.ssoMovedLog.allow(time.Now()) {
				log.Printf("sso: sign-in refused: it was sent on to %q, the public URL's host, but arrived on %q; a proxy in front may be rewriting the Host header (logged at most once a minute)",
					pubHost, logText(r.Host))
			}
			ssoRefused(w, r, "sso_unavailable")
			return
		}
		http.Redirect(w, r, to, http.StatusFound)
		return
	}
	q := r.URL.Query()
	f := ssoFlow{Return: q.Get("rd"), GateNonce: q.Get("n")}
	route := q.Has("rd") || q.Has("n")
	if route {
		// The address travels in the flow cookie, which must stay under 4 KB.
		if len(f.Return) > maxSSOReturn {
			http.Error(w, gateNotProtectedCopy, http.StatusBadRequest)
			return
		}
		if !validNonce(f.GateNonce) {
			http.Error(w, gateBadLinkCopy, http.StatusBadRequest)
			return
		}
		if _, host, err := s.validReturnURL(r.Context(), f.Return); err != nil {
			s.refuseReturnURL(w, r, host, err)
			return
		}
	} else if _, ok := s.currentSession(r); ok {
		// A route visit goes on even so: someone signed in to Outhaul may have
		// no gateway session, and would land on the dashboard instead.
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	p, err := s.ssoProvider(r.Context())
	if errors.Is(err, errSSONotConfigured) {
		if route {
			http.Redirect(w, r, "/gate/start?"+url.Values{"rd": {f.Return}, "n": {f.GateNonce}}.Encode(), http.StatusSeeOther)
			return
		}
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	if err != nil {
		log.Printf("sso: starting a sign-in: %v", err)
		ssoRefusedFor(w, r, f, "sso_unavailable")
		return
	}
	f.State, f.Nonce, f.Verifier = NewToken(), NewToken(), oauth2.GenerateVerifier()
	f.Expires = time.Now().Add(ssoFlowTTL)
	c, _ := s.ssoFlowCookie(f.State) // a NewToken state always names one
	c.Value = encodeSSOFlow(f)
	c.MaxAge = int(ssoFlowTTL / time.Second)
	if len(c.Name)+len(c.Value) > maxSSOFlowCookie {
		http.Error(w, gateNotProtectedCopy, http.StatusBadRequest)
		return
	}
	http.SetCookie(w, c)
	http.Redirect(w, r, p.AuthURL(f.State, f.Nonce, f.Verifier), http.StatusFound)
}

// takeSSOFlow finds the flow named by state, clears its cookie whatever
// happens next, and decodes it. The provider accepts a code once and the
// cookie is gone after the first callback, so a replay has nothing to use.
func (s *Server) takeSSOFlow(w http.ResponseWriter, r *http.Request, state string) (ssoFlow, bool) {
	c, ok := s.ssoFlowCookie(state)
	if !ok {
		return ssoFlow{}, false
	}
	got, err := r.Cookie(c.Name)
	if err != nil {
		return ssoFlow{}, false
	}
	c.MaxAge = -1
	http.SetCookie(w, c)
	return decodeSSOFlow(got.Value)
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
	flow, ok := s.takeSSOFlow(w, r, state)
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
		ssoRefusedFor(w, r, flow, "sso_failed")
		return
	}
	if e := q.Get("error"); e != "" {
		log.Printf("sso: the provider refused the sign-in: %q %q", logText(e), logText(q.Get("error_description")))
		ssoRefusedFor(w, r, flow, "sso_failed")
		return
	}
	code := q.Get("code")
	if code == "" {
		log.Printf("sso: sign-in refused: the provider sent no code")
		ssoRefusedFor(w, r, flow, "sso_failed")
		return
	}
	p, err := s.ssoProvider(r.Context())
	if err != nil {
		log.Printf("sso: finishing a sign-in: %v", err)
		ssoRefusedFor(w, r, flow, "sso_unavailable")
		return
	}
	var id sso.Identity
	if busy := s.withExchangeSlot(r.Context(), func() {
		id, err = p.Exchange(r.Context(), code, flow.Nonce, flow.Verifier)
	}); busy != nil {
		log.Printf("sso: sign-in refused: %v", busy)
		ssoRefusedFor(w, r, flow, "sso_unavailable")
		return
	}
	if errors.Is(err, sso.ErrNoEmail) {
		log.Printf("sso: sign-in refused: %v", err)
		ssoRefusedFor(w, r, flow, "sso_no_email")
		return
	}
	if err != nil {
		log.Printf("sso: sign-in refused: %v", err)
		ssoRefusedFor(w, r, flow, "sso_failed")
		return
	}
	// Removing SSO, or changing its issuer or client ID, ends every SSO
	// sign-in. One that was talking to the provider meanwhile ends too.
	if !s.ssoStillCurrent(r.Context(), p) {
		log.Printf("sso: sign-in refused: the SSO settings changed while it ran")
		ssoRefusedFor(w, r, flow, "sso_failed")
		return
	}
	if flow.routeVisit() {
		s.finishRouteSSO(w, r, p, flow, id)
		return
	}
	role := core.RoleForGroups(id.Groups, s.ssoEnv.RoleGroups)
	mapped := len(s.ssoEnv.RoleGroups) > 0
	user, previous, err := s.store.FindOrCreateSSOUser(r.Context(), id.Issuer, id.Subject, id.Email, role, mapped)
	if err != nil {
		log.Printf("sso: finding the account for a sign-in: %v", err)
		ssoRefused(w, r, "sso_failed")
		return
	}
	if mapped && user.Role != previous {
		log.Printf("sso: account %d (%s): role changed from %q to %q at sign-in; the groups claim held %d group(s)",
			user.ID, user.Display(), previous, user.Role, len(id.Groups))
	}
	token, expires, err := s.createSession(r.Context(), user)
	if err != nil {
		log.Printf("sso: signing in account %d: %v", user.ID, err)
		ssoRefused(w, r, "sso_failed")
		return
	}
	// The sign-in covers protected routes too. Like the session, the gateway
	// session exists before the check below, so a change to the settings
	// either fails that check or, committing later, ends it with the others.
	// Its cookie is set only once the sign-in can't be refused.
	gateToken, err := s.createGateSession(r, user.GateSubject(), core.SignInSSO, user.Display())
	if err != nil {
		if err := s.store.DeleteSession(context.WithoutCancel(r.Context()), token); err != nil {
			log.Printf("sso: ending account %d's refused session: %v", user.ID, err)
		}
		log.Printf("sso: starting account %d's gateway session: %v", user.ID, err)
		ssoRefused(w, r, "sso_failed")
		return
	}
	// Checked again now the sessions exist: a change that committed since the
	// check above is seen here, and one that commits after this ends the new
	// sessions in its own transaction. A browser that has gone away cancels
	// the request, which also fails this check, so the sessions it leaves
	// behind are ended without the request's context.
	if !s.ssoStillCurrent(r.Context(), p) {
		if err := s.store.DeleteSession(context.WithoutCancel(r.Context()), token); err != nil {
			log.Printf("sso: ending account %d's refused session: %v", user.ID, err)
		}
		s.endRefusedGateSession(w, r, gateToken)
		log.Printf("sso: sign-in refused: the SSO settings changed while it ran")
		ssoRefused(w, r, "sso_failed")
		return
	}
	var old string
	if c, err := r.Cookie(sessionCookie); err == nil {
		old = c.Value
	}
	if gateToken != "" {
		s.setGateCookie(w, gateToken)
		s.endReplacedGateSession(r, gateToken)
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

// finishRouteSSO finishes an SSO sign-in for a protected route, once the
// provider has said who the person is and the settings are still the ones it
// asked. It makes no account and leaves any Outhaul session alone: it starts
// a gateway session for the SSO identity, checks the settings once more, and
// sends the person back to the address with a code. The address and n come
// from the flow cookie, which the person can edit, and a row may have been
// turned off since start, so both are checked again first.
func (s *Server) finishRouteSSO(w http.ResponseWriter, r *http.Request, p *ssoConn, flow ssoFlow, id sso.Identity) {
	if !validNonce(flow.GateNonce) {
		log.Printf("sso: route sign-in refused: the flow carries no valid sign-in link")
		http.Error(w, gateBadLinkCopy, http.StatusBadRequest)
		return
	}
	if _, host, err := s.validReturnURL(r.Context(), flow.Return); err != nil {
		s.refuseReturnURL(w, r, host, err)
		return
	}
	if !s.onPublicHost(r) {
		log.Printf("sso: route sign-in refused: the callback arrived on %q, not the public URL's host", logText(r.Host))
		ssoRefusedFor(w, r, flow, "sso_failed")
		return
	}
	token, err := s.createGateSession(r, core.SSOGateSubject(id.Issuer, id.Subject), core.SignInSSO, id.Email)
	if err == nil && token == "" {
		err = errors.New("no gateway session off the public URL's host")
	}
	if err != nil {
		log.Printf("sso: starting a route sign-in's gateway session: %v", err)
		ssoRefusedFor(w, r, flow, "sso_failed")
		return
	}
	// As for an Outhaul sign-in: a change that committed since the first
	// check is seen here, and one that commits after this ends the session.
	if !s.ssoStillCurrent(r.Context(), p) {
		s.endRefusedGateSession(w, r, token)
		log.Printf("sso: sign-in refused: the SSO settings changed while it ran")
		ssoRefusedFor(w, r, flow, "sso_failed")
		return
	}
	s.setGateCookie(w, token)
	s.endReplacedGateSession(r, token)
	s.redirectWithCode(w, r, token, flow.Return, flow.GateNonce)
}

// endRefusedGateSession ends a gateway session just made for a sign-in that
// was then refused. Its cookie was never set, so the gateway cookie the
// browser already holds, and the session it names, are left as they were;
// the cookie is cleared only if it names the session being ended. It runs
// without the request's context, which a browser that went away has
// cancelled.
func (s *Server) endRefusedGateSession(w http.ResponseWriter, r *http.Request, token string) {
	if token == "" {
		return
	}
	if err := s.store.DeleteGateSession(context.WithoutCancel(r.Context()), token); err != nil {
		log.Printf("sso: ending a refused sign-in's gateway session: %v", err)
	}
	if s.gateToken(r) == token {
		s.clearGateCookie(w)
	}
}
