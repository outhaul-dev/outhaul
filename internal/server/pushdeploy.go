package server

import "github.com/james-smart/outhaul/internal/gitrepo"

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
