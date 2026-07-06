// Package gitssh is the embedded SSH server for git-push-to-deploy. It
// authenticates connections by matching the client key's SHA256 fingerprint
// against the push_keys table, accepts only git-receive-pack / git-upload-pack
// against a per-app bare repo, and runs the git subprocess wired to the SSH
// channel. The post-receive hook (installed by gitrepo) streams the deploy back
// to the client; this package is only the transport.
package gitssh

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/james-smart/outhaul/internal/core"
	"github.com/james-smart/outhaul/internal/gitrepo"
	"golang.org/x/crypto/ssh"
)

// Keyring authorizes push keys by fingerprint and resolves apps (the store
// satisfies it).
type Keyring interface {
	FindPushKeyByFingerprint(ctx context.Context, fingerprint string) (core.PushKey, bool, error)
	TouchPushKey(ctx context.Context, id int64) error
	GetAppByName(ctx context.Context, name string) (core.App, error)
}

// Server is the git-push SSH server. It owns its listener so the port can be
// rebound live from the admin UI.
type Server struct {
	cfg     *ssh.ServerConfig
	keyring Keyring
	repos   *gitrepo.Manager

	mu sync.Mutex
	ln net.Listener
}

// New builds the server with a persistent host key and public-key auth.
func New(hostKey ssh.Signer, keyring Keyring, repos *gitrepo.Manager) *Server {
	s := &Server{keyring: keyring, repos: repos}
	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(c ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			fp := ssh.FingerprintSHA256(key)
			pk, ok, err := s.keyring.FindPushKeyByFingerprint(ctx, fp)
			if err != nil || !ok {
				return nil, fmt.Errorf("unknown key")
			}
			_ = s.keyring.TouchPushKey(ctx, pk.ID)
			return &ssh.Permissions{Extensions: map[string]string{"pushkey_fp": fp}}, nil
		},
	}
	cfg.AddHostKey(hostKey)
	s.cfg = cfg
	return s
}

// Listen binds addr and stores the listener. Call before Serve so a bind
// failure (e.g. the port is in use) surfaces synchronously to the caller.
func (s *Server) Listen(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.ln = ln
	s.mu.Unlock()
	return nil
}

// Serve accepts connections until ctx is cancelled. Listen must have been
// called first.
func (s *Server) Serve(ctx context.Context) error {
	if s.currentListener() == nil {
		return fmt.Errorf("gitssh: Serve called before Listen")
	}
	go func() {
		<-ctx.Done()
		s.mu.Lock()
		if s.ln != nil {
			s.ln.Close()
		}
		s.mu.Unlock()
	}()

	var tempDelay time.Duration
	for {
		conn, err := s.currentListener().Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if errors.Is(err, net.ErrClosed) {
				tempDelay = 0
				continue // listener swapped by Rebind; pick up the new one
			}
			if tempDelay == 0 {
				tempDelay = 5 * time.Millisecond
			} else {
				tempDelay *= 2
			}
			if tempDelay > time.Second {
				tempDelay = time.Second
			}
			log.Printf("gitssh: accept error: %v; retrying in %v", err, tempDelay)
			time.Sleep(tempDelay)
			continue
		}
		tempDelay = 0
		go s.handleConn(ctx, conn)
	}
}

func (s *Server) currentListener() net.Listener {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ln
}

// Addr returns the current listen address (empty if not listening).
func (s *Server) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ln == nil {
		return ""
	}
	return s.ln.Addr().String()
}

// Rebind switches the listener to addr. It binds the new address first, so a
// failure leaves the old listener serving and returns an error. The old
// listener is closed after the swap.
func (s *Server) Rebind(addr string) error {
	newLn, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	s.mu.Lock()
	old := s.ln
	s.ln = newLn
	s.mu.Unlock()
	if old != nil {
		old.Close()
	}
	return nil
}

func (s *Server) handleConn(ctx context.Context, nConn net.Conn) {
	defer nConn.Close()
	connCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Bound the unauthenticated handshake; clear the deadline after, since a
	// legitimate push/fetch transfer can take arbitrarily long.
	_ = nConn.SetDeadline(time.Now().Add(30 * time.Second))
	sconn, chans, reqs, err := ssh.NewServerConn(nConn, s.cfg)
	if err != nil {
		return
	}
	_ = nConn.SetDeadline(time.Time{})
	defer sconn.Close()
	go ssh.DiscardRequests(reqs)

	fp := ""
	if sconn.Permissions != nil {
		fp = sconn.Permissions.Extensions["pushkey_fp"]
	}
	for newCh := range chans {
		if newCh.ChannelType() != "session" {
			newCh.Reject(ssh.UnknownChannelType, "only session channels are supported")
			continue
		}
		ch, chReqs, err := newCh.Accept()
		if err != nil {
			continue
		}
		go s.handleSession(connCtx, ch, chReqs, fp)
	}
}

func (s *Server) handleSession(ctx context.Context, ch ssh.Channel, reqs <-chan *ssh.Request, fp string) {
	defer ch.Close()
	for req := range reqs {
		if req.Type != "exec" {
			req.Reply(false, nil)
			continue
		}
		var payload struct{ Command string }
		if err := ssh.Unmarshal(req.Payload, &payload); err != nil {
			req.Reply(false, nil)
			return
		}
		req.Reply(true, nil)
		code := s.runGit(ctx, ch, payload.Command, fp)
		sendExit(ch, code)
		return
	}
}

// runGit validates the command, resolves + lazily inits the bare repo, and runs
// the git subprocess wired to the channel. Returns the process exit code.
func (s *Server) runGit(ctx context.Context, ch ssh.Channel, command, fp string) uint32 {
	verb, repo, err := parseGitCommand(command)
	if err != nil {
		fmt.Fprintf(ch.Stderr(), "outhaul: %v\n", err)
		return 1
	}
	if !core.ValidAppName(repo) {
		fmt.Fprintf(ch.Stderr(), "outhaul: invalid app name %q (lowercase letters, digits, hyphens; 2-40 chars)\n", repo)
		return 1
	}
	log.Printf("gitssh: %s -> %s %s", fp, verb, repo)
	app, aerr := s.keyring.GetAppByName(ctx, repo)
	switch {
	case aerr == nil && app.Source != core.SourcePush:
		fmt.Fprintf(ch.Stderr(), "outhaul: app '%s' is not push-deployable\n", repo)
		return 1
	case aerr != nil && !errors.Is(aerr, sql.ErrNoRows):
		fmt.Fprintf(ch.Stderr(), "outhaul: %v\n", aerr)
		return 1
	}
	// aerr == nil (push app) or ErrNoRows (cold push): proceed to lazy Init.
	dir, err := s.repos.Path(repo)
	if err != nil {
		fmt.Fprintf(ch.Stderr(), "outhaul: %v\n", err)
		return 1
	}
	if err := s.repos.Init(repo); err != nil {
		fmt.Fprintf(ch.Stderr(), "outhaul: prepare repo: %v\n", err)
		return 1
	}
	sub := verb[len("git-"):] // "receive-pack" / "upload-pack"
	cmd := exec.CommandContext(ctx, "git", sub, dir)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME")}
	cmd.Stdin = ch
	cmd.Stdout = ch
	cmd.Stderr = ch.Stderr()
	if err := cmd.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code := ee.ExitCode()
			if code < 0 {
				code = 1
			}
			return uint32(code)
		}
		log.Printf("gitssh: run %s: %v", sub, err)
		return 1
	}
	return 0
}

func sendExit(ch ssh.Channel, code uint32) {
	ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{code}))
}
