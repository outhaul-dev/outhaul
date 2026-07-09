package server

import "context"

// TunnelControl is the subset of infra management the Settings page needs to
// turn the Cloudflare Tunnel on and off. The main package's infraController
// satisfies it. nil disables the tunnel settings card.
type TunnelControl interface {
	Enable(ctx context.Context, token string) error
	Disable(ctx context.Context) error
}

// SetTunnelControl wires tunnel management so the Settings page can toggle it.
func (s *Server) SetTunnelControl(c TunnelControl) { s.tunnel = c }
