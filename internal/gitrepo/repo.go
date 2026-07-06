// Package gitrepo owns the per-app bare git repositories that back push-source
// apps: creating them (with a post-receive hook that relays deploys back to
// Outhaul), resolving them path-safely by app name, and removing them.
package gitrepo

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Manager creates and resolves bare repos under Root. binPath is the absolute
// outhaul binary the post-receive hook execs; sockPath is the unix socket the
// hook relays through. Both are baked into each hook so it needs no environment.
type Manager struct {
	Root     string
	binPath  string
	sockPath string
}

// New returns a Manager rooted at root (typically cfg.GitDir()).
func New(root, binPath, sockPath string) *Manager {
	return &Manager{Root: root, binPath: binPath, sockPath: sockPath}
}

// Path returns the bare repo path for app, rejecting any name that is not a
// single safe path segment (defense-in-depth against traversal). It does not
// require the repo to exist.
func (m *Manager) Path(app string) (string, error) {
	if app == "" || app == "." || app == ".." ||
		strings.ContainsAny(app, "/\\") || strings.Contains(app, "..") {
		return "", fmt.Errorf("invalid repo name %q", app)
	}
	return filepath.Join(m.Root, app+".git"), nil
}

// Init creates the bare repo for app if absent and (re)writes its post-receive
// hook. Idempotent: initializing an existing repo is not an error.
func (m *Manager) Init(app string) error {
	dir, err := m.Path(app)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(m.Root, 0o700); err != nil {
		return fmt.Errorf("create git root: %w", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "HEAD")); os.IsNotExist(statErr) {
		cmd := exec.Command("git", "init", "--bare", "--quiet", dir)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("git init --bare: %v: %s", err, out)
		}
	}
	return m.writeHook(app, dir)
}

// writeHook installs hooks/post-receive as a shell relay into `outhaul git-hook`.
func (m *Manager) writeHook(app, dir string) error {
	hooksDir := filepath.Join(dir, "hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		return err
	}
	// The hook passes app name and socket path explicitly so it needs no env.
	script := fmt.Sprintf("#!/bin/sh\nexec %s git-hook %s %s\n",
		shellQuote(m.binPath), shellQuote(app), shellQuote(m.sockPath))
	return os.WriteFile(filepath.Join(hooksDir, "post-receive"), []byte(script), 0o755)
}

// Remove deletes the bare repo for app. Removing an absent repo is a no-op.
func (m *Manager) Remove(app string) error {
	dir, err := m.Path(app)
	if err != nil {
		return err
	}
	return os.RemoveAll(dir)
}

// shellQuote single-quotes s for safe embedding in the /bin/sh hook script.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
