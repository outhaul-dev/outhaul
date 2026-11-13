// Package gitrepo owns the per-app bare git repositories that back push-source
// apps: creating them (with a post-receive hook that relays deploys back to
// Outhaul), resolving them path-safely by app name, and removing them.
package gitrepo

import (
	"fmt"
	"io"
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

// Exists reports whether app's bare repository is present.
func (m *Manager) Exists(app string) bool {
	dir, err := m.Path(app)
	if err != nil {
		return false
	}
	_, err = os.Stat(filepath.Join(dir, "HEAD"))
	return err == nil
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
// A hook that already holds the script and is executable is left alone.
// Otherwise the script is written to a temporary file beside it and renamed
// over it, so a push whose hook starts meanwhile runs the whole old script or
// the whole new one, never an empty or half-written file.
func (m *Manager) writeHook(app, dir string) error {
	hooksDir := filepath.Join(dir, "hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		return err
	}
	// The hook passes app name and socket path explicitly so it needs no env.
	script := fmt.Sprintf("#!/bin/sh\nexec %s git-hook %s %s\n",
		shellQuote(m.binPath), shellQuote(app), shellQuote(m.sockPath))
	hook := filepath.Join(hooksDir, "post-receive")
	if hookIsCurrent(hook, script) {
		return nil
	}
	f, err := os.CreateTemp(hooksDir, "post-receive.*")
	if err != nil {
		return fmt.Errorf("write the post-receive hook: %w", err)
	}
	tmp := f.Name()
	_, err = f.WriteString(script)
	if err == nil {
		err = f.Chmod(0o755)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, hook)
	}
	if err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("write the post-receive hook: %w", err)
	}
	return nil
}

// hookIsCurrent reports whether hook holds exactly script and is executable.
func hookIsCurrent(hook, script string) bool {
	f, err := os.Open(hook)
	if err != nil {
		return false
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm() != 0o755 || fi.Size() != int64(len(script)) {
		return false
	}
	data := make([]byte, len(script)+1)
	n, _ := io.ReadFull(f, data)
	return n == len(script) && string(data[:n]) == script
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
