package server

import (
	"net"

	"github.com/outhaul-dev/outhaul/internal/gitrepo"
)

// SSHControl is the subset of the git-push SSH server the Settings page needs to
// display and change the listen address. *gitssh.Server satisfies it.
type SSHControl interface {
	Addr() string
	Rebind(addr string) error
}

// SetSSHControl wires the SSH server so the Settings page can rebind its port.
func (s *Server) SetSSHControl(c SSHControl) { s.sshControl = c }

// SetRepos wires the bare-repo manager so app deletion can remove the repo.
func (s *Server) SetRepos(m *gitrepo.Manager) { s.repos = m }

// sshAddr returns the git-push SSH server's current listen address, or "" when
// the server isn't running.
func (s *Server) sshAddr() string {
	if s.sshControl != nil {
		return s.sshControl.Addr()
	}
	return ""
}

// pushRemote builds the ssh remote URL shown on a push app's page, e.g.
// ssh://git@203.0.113.9:2222/api. Host falls back to a placeholder when the
// server IP is unknown; port falls back to :2222.
func (s *Server) pushRemote(app string) string {
	host := s.serverIP
	if host == "" {
		host = "<your-server>"
	}
	port := ":2222"
	if a := s.sshAddr(); a != "" {
		if _, p, err := net.SplitHostPort(a); err == nil && p != "" {
			port = ":" + p
		}
	}
	return "ssh://git@" + host + port + "/" + app
}
