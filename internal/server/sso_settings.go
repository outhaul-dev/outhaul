package server

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/outhaul-dev/outhaul/internal/core"
	"github.com/outhaul-dev/outhaul/internal/sso"
	"github.com/outhaul-dev/outhaul/internal/store"
)

// ssoDiscoveryRetry is how long a failed discovery is remembered, so a flood
// of sign-ins while the provider is down makes one call to it per period.
const ssoDiscoveryRetry = 5 * time.Second

// errSSONotConfigured means no SSO provider is connected.
var errSSONotConfigured = errors.New("server: no SSO provider is configured")

// ssoFromEnvironmentMessage is the refusal to change settings the environment owns.
const ssoFromEnvironmentMessage = "Single sign-on is set from the environment (OUTHAUL_SSO_ISSUER, OUTHAUL_SSO_CLIENT_ID, OUTHAUL_SSO_CLIENT_SECRET_FILE). Change it there and restart Outhaul."

// SSOEnvironment is the SSO configuration the process environment gave at
// startup. FromEnvironment says the issuer, client ID and secret were set and
// win over the stored settings. The groups claim, scopes and role mapping apply
// to either source.
type SSOEnvironment struct {
	FromEnvironment                bool
	Issuer, ClientID, ClientSecret string
	GroupsClaim                    string
	Scopes                         []string
	RoleGroups                     []core.GroupRole
}

// SetSSOEnvironment installs the environment's SSO configuration. Call it
// before Handler.
func (s *Server) SetSSOEnvironment(v SSOEnvironment) { s.ssoEnv = v }

// ssoSettings is the provider in force: the environment's while it is set,
// otherwise the stored one.
func (s *Server) ssoSettings(ctx context.Context) (store.SSOSettings, bool, error) {
	if s.ssoEnv.FromEnvironment {
		return store.SSOSettings{Issuer: s.ssoEnv.Issuer, ClientID: s.ssoEnv.ClientID, ClientSecret: s.ssoEnv.ClientSecret}, true, nil
	}
	return s.store.GetSSOSettings(ctx)
}

// ssoConfig is the discovery and exchange config for settings, carrying the
// environment's claim and scopes.
func (s *Server) ssoConfig(v store.SSOSettings) sso.Config {
	return sso.Config{
		Issuer: v.Issuer, ClientID: v.ClientID, ClientSecret: v.ClientSecret,
		RedirectURL: s.ssoRedirectURI(),
		GroupsClaim: s.ssoEnv.GroupsClaim, Scopes: s.ssoEnv.Scopes,
	}
}

// ProbeSSO runs discovery once at startup, so the log says at once whether the
// provider answered. It never stops Outhaul: a failure is logged, and sign-in
// reports it again.
func (s *Server) ProbeSSO(ctx context.Context) {
	if _, err := s.ssoProvider(ctx); err != nil {
		if errors.Is(err, errSSONotConfigured) {
			return
		}
		log.Printf("sso: the provider did not answer at startup: %v", err)
		return
	}
	log.Printf("sso: the provider answered at startup")
}

// ssoRedirectURI is the callback the admin registers at the provider.
func (s *Server) ssoRedirectURI() string {
	return strings.TrimRight(s.publicURL, "/") + "/auth/sso/callback"
}

// ssoProvider returns the connected provider, running discovery the first time
// and after the settings change. Without a public URL there is nowhere for the
// provider to send people back, so SSO counts as not connected.
//
// One discovery runs at a time, and a failure is remembered for
// ssoRetryAfter, so a flood of sign-ins while the provider is down makes one
// call to it per period. Saving or removing the settings forgets both the
// provider and the failure. Saving runs its own discovery, never this one.
func (s *Server) ssoProvider(ctx context.Context) (*ssoConn, error) {
	if !s.publicURLSet() {
		return nil, errSSONotConfigured
	}
	if p, err, known := s.ssoRemembered(); known {
		return p, err
	}
	select {
	case s.ssoDiscovering <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-s.ssoDiscovering }()
	// Another request may have finished a discovery while this one waited.
	if p, err, known := s.ssoRemembered(); known {
		return p, err
	}
	s.ssoMu.Lock()
	gen := s.ssoGen
	s.ssoMu.Unlock()

	cfg, ok, err := s.ssoSettings(ctx)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errSSONotConfigured
	}
	// Not the request's own context: the requests waiting on this discovery
	// mustn't all fail because the first one gave up. Discover bounds itself.
	dp, err := s.discover(context.WithoutCancel(ctx), s.ssoConfig(cfg))
	s.ssoMu.Lock()
	defer s.ssoMu.Unlock()
	// Settings changed while discovery ran: the result is for the old ones,
	// so it isn't remembered.
	remember := s.ssoGen == gen
	if err != nil {
		if remember {
			s.ssoFailed, s.ssoFailedUntil = err, time.Now().Add(s.ssoRetryAfter)
		}
		return nil, err
	}
	p := &ssoConn{Provider: dp, issuer: cfg.Issuer, clientID: cfg.ClientID}
	if remember {
		s.ssoCached = p
	}
	return p, nil
}

// ssoRemembered returns the cached provider, or a discovery failure that is
// still fresh. known is false when discovery has to run.
func (s *Server) ssoRemembered() (p *ssoConn, failed error, known bool) {
	s.ssoMu.Lock()
	defer s.ssoMu.Unlock()
	if s.ssoCached != nil {
		return s.ssoCached, nil, true
	}
	if s.ssoFailed != nil && time.Now().Before(s.ssoFailedUntil) {
		return nil, s.ssoFailed, true
	}
	return nil, nil, false
}

// ssoConn is the connected provider with the settings it was discovered
// with, so a sign-in can tell whether they changed while it ran.
type ssoConn struct {
	sso.Provider
	issuer, clientID string
}

// ssoStillCurrent reports whether the stored settings are still the ones p was
// discovered with: removing SSO, or changing the issuer or client ID, ends
// every SSO sign-in, including one in flight.
func (s *Server) ssoStillCurrent(ctx context.Context, p *ssoConn) bool {
	cur, ok, err := s.ssoSettings(ctx)
	if err != nil {
		log.Printf("sso: rereading the settings to finish a sign-in: %v", err)
		return false
	}
	return ok && cur.Issuer == p.issuer && cur.ClientID == p.clientID
}

// dropSSOProvider forgets the cached provider and any remembered discovery
// failure; call when settings change.
func (s *Server) dropSSOProvider() {
	s.ssoMu.Lock()
	s.ssoCached = nil
	s.ssoFailed = nil
	s.ssoGen++
	s.ssoMu.Unlock()
}

// ssoPanel is the SSO panel's view data.
func (s *Server) ssoPanel(r *http.Request) map[string]any {
	p := map[string]any{"RedirectURI": s.ssoRedirectURI()}
	if len(s.ssoEnv.RoleGroups) > 0 {
		p["RoleGroups"] = s.ssoEnv.RoleGroups
		p["GroupsClaim"] = s.ssoEnv.GroupsClaim
	}
	if len(s.ssoEnv.Scopes) > 0 {
		p["Scopes"] = strings.Join(s.ssoEnv.Scopes, " ")
	}
	if s.ssoEnv.FromEnvironment {
		p["FromEnvironment"] = true
		p["Issuer"], p["ClientID"] = s.ssoEnv.Issuer, s.ssoEnv.ClientID
		p["HasSecret"], p["Configured"] = true, true
		if _, ok, err := s.store.GetSSOSettings(r.Context()); err == nil && ok {
			p["StoredAlso"] = true
		}
		return p
	}
	if cur, ok, err := s.store.GetSSOSettings(r.Context()); err == nil && ok {
		p["Issuer"], p["ClientID"] = cur.Issuer, cur.ClientID
		p["HasSecret"] = cur.ClientSecret != ""
		p["Configured"] = true
	}
	return p
}

// renderSSOError redisplays Settings with the typed values kept (never the
// secret) and a message. confirm asks for the issuer-change confirmation.
func (s *Server) renderSSOError(w http.ResponseWriter, r *http.Request, msg, issuer, clientID string, confirm bool) {
	p := s.ssoPanel(r)
	p["Issuer"], p["ClientID"] = issuer, clientID
	p["NeedsConfirm"] = confirm
	s.renderSettingsWith(w, r, http.StatusBadRequest, msg, map[string]any{"SSO": p})
}

// handleSaveSSO checks the settings with discovery and stores them.
func (s *Server) handleSaveSSO(w http.ResponseWriter, r *http.Request) {
	if s.ssoEnv.FromEnvironment {
		http.Error(w, ssoFromEnvironmentMessage, http.StatusConflict)
		return
	}
	ctx := r.Context()
	if !s.publicURLSet() {
		s.renderSettings(w, r, http.StatusBadRequest, "Single sign-on needs OUTHAUL_PUBLIC_URL to be set.")
		return
	}
	issuer := strings.TrimSpace(r.FormValue("issuer"))
	clientID := strings.TrimSpace(r.FormValue("client_id"))
	secret := strings.TrimSpace(r.FormValue("client_secret"))
	cur, exists, err := s.ssoSettings(ctx)
	if err != nil {
		log.Printf("sso: reading the settings to save them: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if secret == "" && exists {
		secret = cur.ClientSecret
	}
	_, err = s.discover(ctx, s.ssoConfig(store.SSOSettings{Issuer: issuer, ClientID: clientID, ClientSecret: secret}))
	if err != nil {
		s.renderSSOError(w, r, strings.TrimPrefix(err.Error(), "sso: "), issuer, clientID, false)
		return
	}
	issuerChanged := exists && cur.Issuer != issuer
	if issuerChanged && r.FormValue("confirm_issuer_change") != "1" {
		s.renderSSOError(w, r, "Changing the issuer needs your confirmation.", issuer, clientID, true)
		return
	}
	// A new issuer or client ID means different accounts: their sessions end
	// with the save, in one transaction, so a failure leaves both as they were.
	endSessions := exists && (issuerChanged || cur.ClientID != clientID)
	if err := s.store.SetSSOSettings(ctx, store.SSOSettings{Issuer: issuer, ClientID: clientID, ClientSecret: secret}, endSessions); err != nil {
		log.Printf("sso: saving settings: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.dropSSOProvider()
	http.Redirect(w, r, "/settings#sso", http.StatusSeeOther)
}

// handleDeleteSSO removes the provider and ends every SSO session, in one
// transaction. Passwords keep working.
//
// It works without OUTHAUL_PUBLIC_URL, so settings saved before it was unset
// can always be removed.
func (s *Server) handleDeleteSSO(w http.ResponseWriter, r *http.Request) {
	if s.ssoEnv.FromEnvironment {
		http.Error(w, ssoFromEnvironmentMessage, http.StatusConflict)
		return
	}
	if err := s.store.ClearSSOSettings(r.Context()); err != nil {
		log.Printf("sso: removing settings: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.dropSSOProvider()
	http.Redirect(w, r, "/settings#sso", http.StatusSeeOther)
}
