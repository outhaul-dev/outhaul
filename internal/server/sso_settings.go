package server

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strings"

	"github.com/outhaul-dev/outhaul/internal/sso"
	"github.com/outhaul-dev/outhaul/internal/store"
)

// errSSONotConfigured means no SSO provider is connected.
var errSSONotConfigured = errors.New("server: no SSO provider is configured")

// ssoRedirectURI is the callback the admin registers at the provider.
func (s *Server) ssoRedirectURI() string {
	return strings.TrimRight(s.publicURL, "/") + "/auth/sso/callback"
}

// ssoProvider returns the connected provider, running discovery the first time
// and after the settings change. A failed discovery is not cached.
func (s *Server) ssoProvider(ctx context.Context) (sso.Provider, error) {
	s.ssoMu.Lock()
	if s.ssoCached != nil {
		p := s.ssoCached
		s.ssoMu.Unlock()
		return p, nil
	}
	gen := s.ssoGen
	s.ssoMu.Unlock()

	cfg, ok, err := s.store.GetSSOSettings(ctx)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errSSONotConfigured
	}
	p, err := s.discover(ctx, sso.Config{
		Issuer: cfg.Issuer, ClientID: cfg.ClientID, ClientSecret: cfg.ClientSecret,
		RedirectURL: s.ssoRedirectURI(),
	})
	if err != nil {
		return nil, err
	}
	s.ssoMu.Lock()
	defer s.ssoMu.Unlock()
	// Settings changed while discovery ran: this provider is for the old ones.
	if s.ssoGen == gen {
		s.ssoCached = p
	}
	return p, nil
}

// dropSSOProvider forgets the cached provider; call when settings change.
func (s *Server) dropSSOProvider() {
	s.ssoMu.Lock()
	s.ssoCached = nil
	s.ssoGen++
	s.ssoMu.Unlock()
}

// ssoPanel is the SSO panel's view data.
func (s *Server) ssoPanel(r *http.Request) map[string]any {
	p := map[string]any{"RedirectURI": s.ssoRedirectURI()}
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
	ctx := r.Context()
	if !s.publicURLSet() {
		s.renderSettings(w, r, http.StatusBadRequest, "Single sign-on needs OUTHAUL_PUBLIC_URL to be set.")
		return
	}
	issuer := strings.TrimSpace(r.FormValue("issuer"))
	clientID := strings.TrimSpace(r.FormValue("client_id"))
	secret := strings.TrimSpace(r.FormValue("client_secret"))
	cur, exists, err := s.store.GetSSOSettings(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if secret == "" && exists {
		secret = cur.ClientSecret
	}
	_, err = s.discover(ctx, sso.Config{
		Issuer: issuer, ClientID: clientID, ClientSecret: secret, RedirectURL: s.ssoRedirectURI(),
	})
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
	if err := s.store.ClearSSOSettings(r.Context()); err != nil {
		log.Printf("sso: removing settings: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.dropSSOProvider()
	http.Redirect(w, r, "/settings#sso", http.StatusSeeOther)
}
