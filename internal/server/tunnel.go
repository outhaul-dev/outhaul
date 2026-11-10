package server

import (
	"context"
	"net/http"
	"strings"
)

// TunnelControl is the subset of infra management the Settings page needs to
// turn the Cloudflare Tunnel on and off. The main package's infraController
// satisfies it. nil disables the tunnel settings card.
type TunnelControl interface {
	Enable(ctx context.Context, token string) error
	Disable(ctx context.Context) error
}

// SetTunnelControl wires tunnel management so the Settings page can toggle it.
func (s *Server) SetTunnelControl(c TunnelControl) { s.tunnel = c }

// ProxyControl reconciles the managed proxy with the stored settings now. The
// main package's infraController satisfies it. Turning sign-in on asks for one
// first, so Traefik maps the gate's host before a gated container is deployed.
// nil means the proxy can't be reconciled, and sign-in can't be turned on.
// ReconcileProxy's error names what failed, worded to follow "Outhaul
// couldn't" ("start the tunnel connector: ...").
type ProxyControl interface {
	ReconcileProxy(ctx context.Context) error
}

// SetProxyControl wires the proxy reconcile that turning sign-in on needs.
func (s *Server) SetProxyControl(p ProxyControl) { s.proxy = p }

// handleEnableTunnel persists the pasted connector token and brings the tunnel
// up. An empty token is rejected; a live reconcile failure re-renders the page.
func (s *Server) handleEnableTunnel(w http.ResponseWriter, r *http.Request) {
	if s.tunnel == nil {
		s.renderSettings(w, r, http.StatusServiceUnavailable, "Tunnel management is unavailable (is Docker running?).")
		return
	}
	token := strings.TrimSpace(r.FormValue("token"))
	if token == "" {
		s.renderSettings(w, r, http.StatusBadRequest, "Paste the Cloudflare connector token to enable the tunnel.")
		return
	}
	if err := s.tunnel.Enable(r.Context(), token); err != nil {
		s.renderSettings(w, r, http.StatusInternalServerError, "Could not enable the tunnel: "+err.Error())
		return
	}
	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}

// handleDisableTunnel clears the token and tears the connector down, returning
// Traefik to its Let's Encrypt posture.
func (s *Server) handleDisableTunnel(w http.ResponseWriter, r *http.Request) {
	if s.tunnel == nil {
		s.renderSettings(w, r, http.StatusServiceUnavailable, "Tunnel management is unavailable (is Docker running?).")
		return
	}
	if err := s.tunnel.Disable(r.Context()); err != nil {
		s.renderSettings(w, r, http.StatusInternalServerError, "Could not disable the tunnel: "+err.Error())
		return
	}
	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}
