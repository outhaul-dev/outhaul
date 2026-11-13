package githttp

import (
	"compress/gzip"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/netip"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/outhaul-dev/outhaul/internal/core"
	"github.com/outhaul-dev/outhaul/internal/gitrepo"
	"github.com/outhaul-dev/outhaul/internal/pushauth"
)

// Store is what the handler reads (the store satisfies it).
type Store interface {
	pushauth.Store
	FindPushToken(ctx context.Context, raw string) (core.PushToken, bool, error)
	TouchPushToken(ctx context.Context, id int64, at time.Time, ip string) error
	GetAppByName(ctx context.Context, name string) (core.App, error)
}

// Options configures a Handler.
type Options struct {
	Store         Store
	Repos         *gitrepo.Manager
	PublicURL     string        // https://outhaul.example.com, no trailing slash
	MaxDays       int           // OUTHAUL_PUSH_TOKEN_MAX_DAYS
	SSOMaxAge     time.Duration // OUTHAUL_PUSH_TOKEN_SSO_MAX_AGE
	Limiter       *Limiter
	ClientAddr    func(*http.Request) netip.Addr
	OnPublicHost  func(*http.Request) bool // nil means never: every request is refused
	PlainHTTP     func(*http.Request) bool // known to be plain HTTP; nil means never
	StreamContext func(*http.Request) (context.Context, context.CancelFunc)
	Ready         func() bool      // the hook bridge is listening; nil means yes
	Now           func() time.Time // nil means time.Now
	KeepAlive     int              // seconds; 0 means 5
}

// Handler serves git's smart HTTP protocol for push apps: push, fetch and
// clone, each request signed in with a push token and nothing else. No cookie
// is read or set, and a request's body is read only once it has passed every
// check.
type Handler struct {
	o       Options
	refused refusalLog

	// Tests learn when git has started and when it has been waited for.
	afterStart func(*os.Process)
	afterWait  func(*os.ProcessState)
}

const (
	serviceReceive = "git-receive-pack"
	serviceUpload  = "git-upload-pack"

	challenge = `Basic realm="outhaul"`
)

// New builds the handler, filling in the defaults Options names.
func New(o Options) *Handler {
	o.PublicURL = strings.TrimRight(o.PublicURL, "/")
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.KeepAlive <= 0 {
		o.KeepAlive = 5
	}
	if o.Limiter == nil {
		o.Limiter = NewLimiter(o.Now)
	}
	if o.ClientAddr == nil {
		o.ClientAddr = Proxies{}.ClientAddr
	}
	if o.StreamContext == nil {
		o.StreamContext = func(r *http.Request) (context.Context, context.CancelFunc) {
			return context.WithCancel(r.Context())
		}
	}
	return &Handler{o: o, refused: refusalLog{last: map[string]time.Time{}}}
}

// gitCall is one request on its way through the checks.
type gitCall struct {
	w       http.ResponseWriter
	r       *http.Request
	name    string // {repo} with one trailing .git removed; not yet checked
	addr    netip.Addr
	service string // serviceReceive or serviceUpload, once known
}

// InfoRefs answers GET /git/{repo}/info/refs, the first request of every push,
// fetch and clone.
func (h *Handler) InfoRefs(w http.ResponseWriter, r *http.Request) {
	c, ok := h.open(w, r)
	if !ok {
		return
	}
	c.service = r.URL.Query().Get("service")
	if c.service != serviceReceive && c.service != serviceUpload {
		h.refuse(c, http.StatusNotFound, "dumb", "refused a request for git's dumb protocol", "404 page not found")
		return
	}
	h.serve(c, true)
}

// Service answers POST /git/{repo}/git-receive-pack (a push) and
// /git-upload-pack (a fetch or clone).
func (h *Handler) Service(w http.ResponseWriter, r *http.Request) {
	c, ok := h.open(w, r)
	if !ok {
		return
	}
	c.service = path.Base(r.URL.Path)
	if c.service != serviceReceive && c.service != serviceUpload {
		h.refuse(c, http.StatusNotFound, "dumb", "refused a request for git's dumb protocol", "404 page not found")
		return
	}
	if want := "application/x-" + c.service + "-request"; r.Header.Get("Content-Type") != want {
		h.refuse(c, http.StatusUnsupportedMediaType, "type", "refused a git POST with the wrong content type",
			"outhaul: a git request must have Content-Type "+want)
		return
	}
	h.serve(c, false)
}

// open sets the no-cache headers every answer carries and makes the checks
// that come before any credential is asked for: the bridge is up, the request
// is on the public URL's host, and it isn't known to be plain HTTP.
func (h *Handler) open(w http.ResponseWriter, r *http.Request) (*gitCall, bool) {
	hd := w.Header()
	hd.Set("Expires", "Fri, 01 Jan 1980 00:00:00 GMT")
	hd.Set("Pragma", "no-cache")
	hd.Set("Cache-Control", "no-cache, max-age=0, must-revalidate")
	c := &gitCall{w: w, r: r, name: strings.TrimSuffix(r.PathValue("repo"), ".git"), addr: h.o.ClientAddr(r)}

	if h.o.Store == nil || h.o.Repos == nil || (h.o.Ready != nil && !h.o.Ready()) {
		h.refuse(c, http.StatusServiceUnavailable, "ready", "git over HTTPS is unavailable: the hook bridge isn't listening",
			"outhaul: git push is not available; see the server log")
		return nil, false
	}
	// No challenge anywhere else, so a remote that points at the admin port
	// is never asked for a token.
	if h.o.OnPublicHost == nil || !h.o.OnPublicHost(r) {
		h.refuse(c, http.StatusNotFound, "host", "refused a git request on a host other than the public URL's", "404 page not found")
		return nil, false
	}
	if h.o.PlainHTTP != nil && h.o.PlainHTTP(r) {
		to := h.o.PublicURL + "/git/"
		if core.ValidAppName(c.name) {
			to += c.name + ".git"
		}
		h.refuse(c, http.StatusForbidden, "plain", "refused a git request over plain HTTP",
			"outhaul: this remote uses http://. Change it to "+to)
		return nil, false
	}
	return c, true
}

// serve signs the request in, checks what it may do, and runs git.
func (h *Handler) serve(c *gitCall, advertise bool) {
	ctx := c.r.Context()
	_, password, ok := c.r.BasicAuth()
	if !ok {
		// Git's first request never has credentials: not a failed attempt,
		// and not logged.
		answer(c.w, http.StatusUnauthorized, "outhaul: sign in with any username and a push token as the password. Create one at "+h.o.PublicURL+"/account")
		return
	}
	tok, ok := h.signIn(c, password)
	if !ok {
		return
	}
	now := h.o.Now()
	if err := h.o.Store.TouchPushToken(ctx, tok.ID, now, addrText(c.addr)); err != nil {
		log.Printf("githttp: record the use of push token %d: %v", tok.ID, err)
	}

	owner, err := h.o.Store.GetUser(ctx, tok.UserID)
	if errors.Is(err, sql.ErrNoRows) {
		// The account went with its tokens while this request was checked.
		h.refuse(c, http.StatusUnauthorized, "token", "refused a push token that isn't valid", h.invalidToken())
		return
	}
	if err != nil {
		h.fail(c, fmt.Sprintf("read the owner of push token %d", tok.ID), err)
		return
	}
	if owner.SignIn == core.SignInSSO && (owner.LastSignInAt == nil || now.Sub(*owner.LastSignInAt) > h.o.SSOMaxAge) {
		age := ageText(h.o.SSOMaxAge)
		h.refuse(c, http.StatusForbidden, "sso",
			fmt.Sprintf("refused push token %d: user %d last signed in with SSO more than %s ago", tok.ID, owner.ID, age),
			"outhaul: your last SSO sign-in to Outhaul was more than "+age+" ago. Sign in at "+h.o.PublicURL+"/login, then push again.")
		return
	}

	if !core.ValidAppName(c.name) {
		h.refuse(c, http.StatusNotFound, "name", "refused a git request for a name that isn't a valid app name",
			"outhaul: that isn't a valid app name")
		return
	}
	app, aerr := h.o.Store.GetAppByName(ctx, c.name)
	if aerr != nil && !errors.Is(aerr, sql.ErrNoRows) {
		h.failWith(c, "look up app "+c.name, aerr, "outhaul: could not look up '"+c.name+"'")
		return
	}
	// A first push lands in the default project, where the bridge creates
	// the app. Permission comes before the app's kind, so nobody learns which
	// names exist in projects they aren't on.
	projectID := core.DefaultProjectID
	if aerr == nil {
		projectID = app.ProjectID
	}
	allowed, err := pushauth.Allowed(ctx, h.o.Store, tok.UserID, projectID)
	if err != nil {
		h.failWith(c, "check permission for "+c.name, err, "outhaul: could not check permission for '"+c.name+"'")
		return
	}
	if !allowed {
		h.refuse(c, http.StatusForbidden, "role",
			fmt.Sprintf("refused push token %d: user %d may not use app %s", tok.ID, tok.UserID, c.name),
			"outhaul: you don't have permission to use '"+c.name+"'")
		return
	}
	if aerr == nil && app.Source != core.SourcePush {
		h.refuse(c, http.StatusNotFound, "kind", fmt.Sprintf("refused push token %d: app %s is not push-deployable", tok.ID, c.name),
			"outhaul: app '"+c.name+"' is not push-deployable")
		return
	}
	if aerr != nil && c.service == serviceUpload {
		h.refuse(c, http.StatusNotFound, "noapp", fmt.Sprintf("refused a fetch of %s, which no app has", c.name),
			"outhaul: there is no app called '"+c.name+"'")
		return
	}

	dir, err := h.o.Repos.Path(c.name)
	if err != nil {
		h.failWith(c, "find the repository for "+c.name, err, "outhaul: could not prepare the repository for '"+c.name+"'")
		return
	}
	// As over SSH, before every git run: Init creates a missing repository
	// and always rewrites its hook, so one left stale by a moved binary or
	// socket is repaired by the next request.
	if err := h.o.Repos.Init(c.name); err != nil {
		h.failWith(c, "prepare the repository for "+c.name, err, "outhaul: could not prepare the repository for '"+c.name+"'")
		return
	}
	// A bare repository's HEAD names the host git's default branch, so a
	// plain clone of an app on another branch would check out nothing. A
	// clone that needs -b beats a 500, so a failure here is only logged.
	if c.service == serviceUpload {
		if err := h.o.Repos.PointHead(c.name, app.Branch); err != nil {
			log.Printf("githttp: point the HEAD of %s at its branch: %v", c.name, err)
		}
	}

	args := gitArgs(c.service, h.o.KeepAlive, advertise, dir)
	env := gitEnv(c.r, c.service, tok.ID, c.addr)
	if advertise {
		var first []byte
		if !wantsV2(c.r, c.service) {
			first = serviceAnnouncement(c.service)
		}
		h.run(c, args, env, nil, "application/x-"+c.service+"-advertisement", first)
		return
	}
	var body io.Reader = c.r.Body
	if strings.EqualFold(c.r.Header.Get("Content-Encoding"), "gzip") {
		zr, err := gzip.NewReader(c.r.Body)
		if err != nil {
			h.refuse(c, http.StatusBadRequest, "gzip", "refused a git POST whose body says it is gzip and isn't",
				"outhaul: the request body says it is gzip, and it isn't")
			return
		}
		defer zr.Close()
		body = zr
	}
	h.run(c, args, env, body, "application/x-"+c.service+"-result", nil)
}

// signIn finds the live push token the password holds: the right shape (a
// password of any other shape is never looked up), known, and not expired.
// Otherwise it counts a failed attempt from the client's address and answers.
// A live token is never held back by the count.
func (h *Handler) signIn(c *gitCall, password string) (core.PushToken, bool) {
	raw := strings.Trim(password, " \t\r\n\v\f")
	var tok core.PushToken
	found := false
	if core.ValidPushTokenShape(raw) {
		var err error
		tok, found, err = h.o.Store.FindPushToken(c.r.Context(), raw)
		if err != nil {
			h.fail(c, "look up a push token", err)
			return core.PushToken{}, false
		}
	}
	if found && !tok.Expired(h.o.Now(), h.o.MaxDays) {
		return tok, true
	}
	if wait, limited := h.o.Limiter.Fail(c.addr); limited {
		// Not logged: a flood of these is what the limit keeps out of the log.
		secs := int((wait + time.Second - 1) / time.Second)
		secs = max(secs, 1)
		c.w.Header().Set("Retry-After", strconv.Itoa(secs))
		answer(c.w, http.StatusTooManyRequests, "outhaul: too many failed sign-ins from your address. Try again in "+waitText(secs)+".")
		return core.PushToken{}, false
	}
	if found {
		ended := tok.EndsAt(h.o.MaxDays).UTC().Format("2 January 2006")
		h.refuse(c, http.StatusUnauthorized, "expired", fmt.Sprintf("refused push token %d, which expired on %s", tok.ID, ended),
			"outhaul: that push token expired on "+ended+". Create a new one at "+h.o.PublicURL+"/account")
		return core.PushToken{}, false
	}
	h.refuse(c, http.StatusUnauthorized, "token", "refused a push token that isn't valid", h.invalidToken())
	return core.PushToken{}, false
}

func (h *Handler) invalidToken() string {
	return "outhaul: that push token isn't valid. Create one at " + h.o.PublicURL + "/account"
}

// refuse answers status with msg, and logs why at most once a minute per kind,
// with the client's address. A 401 carries the challenge; nothing else does.
func (h *Handler) refuse(c *gitCall, status int, kind, why, msg string) {
	if h.refused.allow(kind, h.o.Now()) {
		log.Printf("githttp: %s, from %s (refusals like it are logged once a minute)", why, logAddr(c.addr))
	}
	answer(c.w, status, msg)
}

// fail answers 500 when something on the server went wrong.
func (h *Handler) fail(c *gitCall, what string, err error) {
	h.failWith(c, what, err, "outhaul: something went wrong on the server; see its log")
}

func (h *Handler) failWith(c *gitCall, what string, err error, msg string) {
	log.Printf("githttp: %s: %v", what, err)
	answer(c.w, http.StatusInternalServerError, msg)
}

// answer writes a one-line text/plain body, which git prints as a remote:
// line.
func answer(w http.ResponseWriter, status int, msg string) {
	hd := w.Header()
	hd.Set("Content-Type", "text/plain; charset=utf-8")
	hd.Set("X-Content-Type-Options", "nosniff")
	if status == http.StatusUnauthorized {
		hd.Set("WWW-Authenticate", challenge)
	}
	w.WriteHeader(status)
	_, _ = io.WriteString(w, msg+"\n")
}

// addrText is the address recorded for a token's last use: "" when unknown.
func addrText(a netip.Addr) string {
	if !a.IsValid() {
		return ""
	}
	return a.String()
}

func logAddr(a netip.Addr) string {
	if !a.IsValid() {
		return "an unknown address"
	}
	return a.String()
}

// ageText is d in whole days when it is whole days ("7 days"), and otherwise
// as a duration without its trailing zero units ("36h").
func ageText(d time.Duration) string {
	const day = 24 * time.Hour
	if d > 0 && d%day == 0 {
		if n := d / day; n != 1 {
			return fmt.Sprintf("%d days", n)
		}
		return "1 day"
	}
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

// waitText is a wait of secs seconds in words.
func waitText(secs int) string {
	switch {
	case secs == 1:
		return "1 second"
	case secs == 60:
		return "1 minute"
	case secs%60 == 0:
		return fmt.Sprintf("%d minutes", secs/60)
	}
	return fmt.Sprintf("%d seconds", secs)
}

// refusalLog lets each kind of refusal through to the log at most once a
// minute, so a client that repeats one can't flood it.
type refusalLog struct {
	mu   sync.Mutex
	last map[string]time.Time
}

func (l *refusalLog) allow(kind string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if t, ok := l.last[kind]; ok && now.Sub(t) < time.Minute {
		return false
	}
	l.last[kind] = now
	return true
}
