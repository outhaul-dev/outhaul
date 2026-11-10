package server

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/outhaul-dev/outhaul/internal/traefik"
)

// Protected routes. Traefik's forward auth asks GET /gate/check about every
// request to a protected domain row. A visitor signs in on Outhaul's public
// URL, which sends them back to the app's host with a one-time code; the gate
// turns the code into a route cookie on that host, so a cookie for Outhaul's
// own host never reaches an app.

const (
	// gateCodeParam is the reserved query parameter that carries a one-time
	// code back to an app's host.
	gateCodeParam = "__outhaul_code"

	// routeCookie holds a route session on one app host. The __Host- prefix
	// makes a browser take it only from that host itself, Secure, with Path=/
	// and no Domain, so no other host can set or shadow it.
	routeCookie = "__Host-outhaul_route"

	// gateSessionTTL is how long gateway and route sessions last.
	gateSessionTTL = 8 * time.Hour

	// pendingCookie ties a sign-in to the browser that started it. The gate
	// sets it on the app's host when it sends a browser to sign in, and puts
	// the SHA-256 hex of its value in the sign-in URL as n. A code minted from
	// that sign-in works only alongside this cookie.
	pendingCookie = "__Host-outhaul_pending"

	// pendingTTL is how long a browser has to finish signing in.
	pendingTTL = 10 * time.Minute

	// gateCodeTTL is how long a one-time code works.
	gateCodeTTL = 60 * time.Second

	// gateCookieBase names the gateway cookie, which holds a gateway session
	// on Outhaul's own host; see gateCookieName.
	gateCookieBase = "outhaul_gate"

	// gateLapsBase names the cookie on Outhaul's host that counts a browser's
	// arrivals at /gate/start for one host; see gateLooping.
	gateLapsBase = "outhaul_gate_laps"
	gateLapsTTL  = 2 * time.Minute
	gateMaxLaps  = 3

	// maxSSOReturn bounds a return address carried through SSO: it travels in
	// the flow cookie, which must stay under a browser's 4 KB.
	maxSSOReturn = 2000
)

// What a visitor sees when the gate refuses a sign-in link.
const (
	gateNotProtectedCopy = "This address isn't protected by Outhaul."
	gateBadLinkCopy      = "This sign-in link is incomplete. Open the app's address again to sign in."
)

// maxRouteToken bounds a route cookie the gate looks up. Real ones are
// NewToken's 64 hex characters.
const maxRouteToken = 128

// gateUnavailableCopy is what a visitor sees when the gate can't decide.
const gateUnavailableCopy = "Signing in isn't available right now."

// handleGateCheck answers Traefik's forward-auth call for one request to a
// protected route. Traefik passes the request on after a 2xx, copying
// traefik.IdentityHeader from the answer, and returns anything else to the
// visitor as it is, Location and Set-Cookie included.
//
// It trusts X-Forwarded-Host, -Uri and -Method, which the gate's middleware
// has Traefik set itself, and reads the visitor's Cookie, Accept and Upgrade,
// which Traefik copies. Someone calling the admin port directly can set any
// of these, and gains nothing without a valid cookie or code for that host.
//
// In order:
//   - A request carrying a code is handled first, so a URL with a code never
//     reaches the app. A good code sets the route cookie and redirects to the
//     URL without it.
//   - A valid route cookie for exactly this host passes, as its person.
//   - A browser is sent to sign in on the public URL. Anything else gets 401.
//
// A store that fails answers 503, never a pass and never a redirect to sign
// in. Every answer is no-store. Nothing logged names a token, a code, a cookie
// or the URI, whose query can hold a code.
func (s *Server) handleGateCheck(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	// The mux also routes HEAD here. Traefik always asks with GET.
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "Method not allowed.", http.StatusMethodNotAllowed)
		return
	}
	v, ok := readGateVisit(r)
	if !ok {
		if s.gateBadHostLog.allow(time.Now()) {
			log.Printf("gate: refused a check: X-Forwarded-Host %q isn't a host name (logged at most once a minute)", logText(r.Header.Get("X-Forwarded-Host")))
		}
		http.Error(w, "Bad request.", http.StatusBadRequest)
		return
	}
	if v.hasCode {
		s.gateRedeem(w, r, v)
		return
	}
	display, ok, err := s.routeUser(r, v.host)
	switch {
	case err != nil:
		gateUnavailable(w, v.host, "reading a route session", err)
	case ok:
		w.Header().Set(traefik.IdentityHeader, display)
		w.WriteHeader(http.StatusOK)
	default:
		s.gateSignIn(w, r, v)
	}
}

// gateRedeem answers a request carrying a code. A code arrives on a page
// navigation, so any other method carrying one is refused outright, route
// cookie or not. Only the first code is tried, and only in the browser holding
// the pending cookie the code was minted for. A code that doesn't work never
// lets the request through: a valid cookie sends the visitor on to the URL
// without the code, and anyone else starts again.
func (s *Server) gateRedeem(w http.ResponseWriter, r *http.Request, v gateVisit) {
	if v.method != http.MethodGet && v.method != http.MethodHead {
		http.Error(w, "Bad request.", http.StatusBadRequest)
		return
	}
	port, err := s.appPort(r.Context())
	if err != nil {
		gateUnavailable(w, v.host, "reading the tunnel setting", err)
		return
	}
	if v.code != "" {
		if pending := pendingValue(r); pending == "" {
			if s.gateNoPendingLog.allow(time.Now()) {
				log.Printf("gate: refused a sign-in code on %q: this browser has no sign-in in progress (logged at most once a minute)", v.host)
			}
		} else {
			token := NewToken()
			_, err := s.store.RedeemGateCode(r.Context(), v.code, v.host, sha256Hex(pending), token, time.Now().Add(gateSessionTTL))
			if err == nil {
				http.SetCookie(w, &http.Cookie{
					Name:     routeCookie,
					Value:    token,
					Path:     "/",
					MaxAge:   int(gateSessionTTL / time.Second),
					Secure:   true,
					HttpOnly: true,
					SameSite: http.SameSiteLaxMode,
				})
				http.SetCookie(w, newPendingCookie("", -1))
				http.Redirect(w, r, v.url(port), http.StatusFound)
				return
			}
			if !errors.Is(err, sql.ErrNoRows) {
				gateUnavailable(w, v.host, "redeeming a sign-in code", err)
				return
			}
			if s.gateBadCodeLog.allow(time.Now()) {
				log.Printf("gate: refused a sign-in code on %q: it is unknown, used, expired, for another host, or for another browser (logged at most once a minute)", v.host)
			}
		}
	}
	_, ok, err := s.routeUser(r, v.host)
	switch {
	case err != nil:
		gateUnavailable(w, v.host, "reading a route session", err)
	case ok:
		http.Redirect(w, r, v.url(port), http.StatusFound)
	default:
		s.gateSignIn(w, r, v)
	}
}

// gateSignIn sends a browser to sign in on the public URL, to come back to
// v's URL, and answers anything else 401. The browser gets a pending cookie on
// this host, whose hash the sign-in URL carries, so the code it comes back
// with works only here. One it already holds is kept, so two tabs signing in
// to the same app don't spoil each other's codes, and isn't sent again, so
// reusing it doesn't extend its life. With no public URL there is nowhere to
// sign in, so a browser gets 503.
func (s *Server) gateSignIn(w http.ResponseWriter, r *http.Request, v gateVisit) {
	if !v.browser {
		http.Error(w, "Sign in to use this address.", http.StatusUnauthorized)
		return
	}
	if !s.publicURLSet() {
		if s.gateNoPublicURLLog.allow(time.Now()) {
			log.Printf("gate: can't send a visitor to %q to sign in: Outhaul has no public URL (logged at most once a minute)", v.host)
		}
		http.Error(w, gateUnavailableCopy, http.StatusServiceUnavailable)
		return
	}
	port, err := s.appPort(r.Context())
	if err != nil {
		gateUnavailable(w, v.host, "reading the tunnel setting", err)
		return
	}
	pending := pendingValue(r)
	if pending == "" {
		pending = NewToken()
		http.SetCookie(w, newPendingCookie(pending, int(pendingTTL/time.Second)))
	}
	http.Redirect(w, r, s.gateStartURL(v.url(port), sha256Hex(pending)), http.StatusFound)
}

// SetHTTPSPort records the host port Traefik's HTTPS entrypoint is published
// on (OUTHAUL_HTTPS_PORT). Call before Handler.
func (s *Server) SetHTTPSPort(port string) { s.httpsPort = port }

// appPort is the port a browser uses for HTTPS on an app's host, "" for 443:
// the configured HTTPS port, when Traefik publishes its HTTPS entrypoint
// there. It does that when TLS is on and no tunnel is; a tunnel's edge serves
// 443 itself. It never comes from a request, whose Host and forwarded headers
// the visitor chooses.
func (s *Server) appPort(ctx context.Context) (string, error) {
	if !s.tlsEnabled || s.httpsPort == "" || s.httpsPort == "443" {
		return "", nil
	}
	tunnel, err := s.store.TunnelEnabled(ctx)
	if err != nil || tunnel {
		return "", err
	}
	return s.httpsPort, nil
}

// newPendingCookie is the pending cookie with value, lasting maxAge seconds;
// a negative maxAge clears it.
func newPendingCookie(value string, maxAge int) *http.Cookie {
	return &http.Cookie{
		Name:     pendingCookie,
		Value:    value,
		Path:     "/",
		MaxAge:   maxAge,
		Secure:   true,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	}
}

// pendingValue is the request's pending cookie, or "" when it has none that
// the gate could have made (NewToken's 64 lower-case hex characters).
func pendingValue(r *http.Request) string {
	c, err := r.Cookie(pendingCookie)
	if err != nil || !isLowerHex64(c.Value) {
		return ""
	}
	return c.Value
}

// sha256Hex is the nonce a pending cookie's value stands for in a sign-in URL
// and on a code.
func sha256Hex(v string) string {
	sum := sha256.Sum256([]byte(v))
	return hex.EncodeToString(sum[:])
}

func gateUnavailable(w http.ResponseWriter, host, doing string, err error) {
	log.Printf("gate: %s on %q: %v", doing, host, err)
	http.Error(w, gateUnavailableCopy, http.StatusServiceUnavailable)
}

// routeUser returns who the request's route cookie signs in on host. ok is
// false with no cookie, or one that isn't a live route session for exactly
// that host. err is for a store that failed.
func (s *Server) routeUser(r *http.Request, host string) (display string, ok bool, err error) {
	c, err := r.Cookie(routeCookie)
	if err != nil || c.Value == "" || len(c.Value) > maxRouteToken {
		return "", false, nil
	}
	display, err = s.store.RouteSessionUser(r.Context(), c.Value, host)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return display, true, nil
}

// gateStartURL is the sign-in page on the public URL, for a visitor to come
// back to returnURL, from the browser whose pending cookie hashes to
// nonceHash (n, left out when empty). It is "" when no public URL is set: a
// relative URL would be read on the app's host.
func (s *Server) gateStartURL(returnURL, nonceHash string) string {
	if !s.publicURLSet() {
		return ""
	}
	q := url.Values{"rd": {returnURL}}
	if nonceHash != "" {
		q.Set("n", nonceHash)
	}
	return strings.TrimRight(s.publicURL, "/") + "/gate/start?" + q.Encode()
}

// gateVisit is the request Traefik asks the gate about.
type gateVisit struct {
	host    string // normalized, and plausibleHost
	path    string // starts with exactly one "/"
	query   string // raw, in its order, without gateCodeParam; "" for none
	hasCode bool   // gateCodeParam was in the query
	code    string // its first value, unescaped
	method  string // X-Forwarded-Method
	browser bool   // a page navigation; see readGateVisit
}

// url is where v goes without its code: https, on v's host, at port (from
// appPort; "" for 443). It is built from the parts, never from the raw URI, so
// its path can't name another host.
func (v gateVisit) url(port string) string {
	u := "https://" + v.host
	if port != "" {
		u += ":" + port
	}
	u += v.path
	if v.query != "" {
		u += "?" + v.query
	}
	return u
}

// readGateVisit reads the request Traefik describes. ok is false when
// X-Forwarded-Host isn't a plausible host name.
//
// A browser is a GET or HEAD that accepts text/html and asks for no Upgrade.
// Traefik may drop Upgrade, a hop-by-hop header, so the Accept test catches a
// WebSocket by itself: browsers don't send text/html on a WebSocket handshake.
func readGateVisit(r *http.Request) (gateVisit, bool) {
	raw := r.Header.Get("X-Forwarded-Host")
	// Browsers send a host in ASCII. Case folding could turn some other
	// spelling into an ASCII host.
	for i := 0; i < len(raw); i++ {
		if raw[i] >= 0x80 {
			return gateVisit{}, false
		}
	}
	host := normalizeHost(raw)
	if !plausibleHost(host) {
		return gateVisit{}, false
	}
	rawPath, rawQuery, _ := strings.Cut(r.Header.Get("X-Forwarded-Uri"), "?")
	v := gateVisit{
		host: host,
		// A browser reads a path starting "//" or "/\" as naming another
		// host, so however many slashes and backslashes lead, one is kept.
		path:   "/" + escapeUnsafe(strings.TrimLeft(rawPath, `/\`)),
		method: r.Header.Get("X-Forwarded-Method"),
	}
	var rest string
	v.code, rest, v.hasCode = takeGateCode(rawQuery)
	v.query = escapeUnsafe(rest)
	v.browser = (v.method == http.MethodGet || v.method == http.MethodHead) &&
		acceptsHTML(r.Header) && len(r.Header.Values("Upgrade")) == 0
	return v, true
}

// normalizeHost is a host as the gate compares it: lower-cased, without a
// port, and without one trailing dot, which a browser keeps from a fully
// qualified name.
func normalizeHost(h string) string {
	h = strings.ToLower(h)
	if host, _, err := net.SplitHostPort(h); err == nil {
		h = host
	}
	return strings.TrimSuffix(h, ".")
}

// plausibleHost reports whether a normalized host could be a domain row's:
// letters, digits, dots, hyphens and underscores, at most 253 bytes. Nothing
// in it can end or change the authority of a URL built from it.
func plausibleHost(h string) bool {
	if h == "" || len(h) > 253 {
		return false
	}
	for i := 0; i < len(h); i++ {
		c := h[i]
		if !('a' <= c && c <= 'z' || '0' <= c && c <= '9' || c == '.' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

// takeGateCode removes every gateCodeParam from a raw query, however its name
// is escaped, and returns the first one's value. The rest of the query keeps
// its order and spelling.
func takeGateCode(rawQuery string) (code, rest string, found bool) {
	if rawQuery == "" {
		return "", "", false
	}
	pairs := strings.Split(rawQuery, "&")
	kept := pairs[:0]
	for _, p := range pairs {
		k, val, _ := strings.Cut(p, "=")
		if name, err := url.QueryUnescape(k); err != nil || name != gateCodeParam {
			kept = append(kept, p)
			continue
		}
		if !found {
			code, _ = url.QueryUnescape(val) // a malformed value is no code
			found = true
		}
	}
	return code, strings.Join(kept, "&"), found
}

// escapeUnsafe percent-encodes the bytes a URL can't hold as they are:
// controls, space, anything outside ASCII, and "#". A request target has no
// fragment, so a "#" in one is data; left alone it would start a fragment, and
// a code added to the URL later could land in it. Everything else, existing
// escapes included, stays as the visitor sent it.
func escapeUnsafe(s string) string {
	const hexDigits = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c > ' ' && c < 0x7f && c != '#' {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hexDigits[c>>4])
		b.WriteByte(hexDigits[c&0x0f])
	}
	return b.String()
}

// acceptsHTML reports whether the visitor's Accept names text/html.
func acceptsHTML(h http.Header) bool {
	for _, v := range h.Values("Accept") {
		if strings.Contains(strings.ToLower(v), "text/html") {
			return true
		}
	}
	return false
}

// onceAMinute lets a log line that would repeat on every request through at
// most once a minute. The zero value is ready to use.
type onceAMinute struct {
	mu   sync.Mutex
	last time.Time
}

// allow reports whether the line may be logged at now, and if so starts the
// next minute.
func (o *onceAMinute) allow(now time.Time) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.last.IsZero() && now.Sub(o.last) < time.Minute {
		return false
	}
	o.last = now
	return true
}
