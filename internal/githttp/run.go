package githttp

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/outhaul-dev/outhaul/internal/githook"
)

const (
	// maxStderrLines is how many lines of git's standard error one request
	// may put in the log.
	maxStderrLines = 5
	// maxStderrLine cuts a longer line.
	maxStderrLine = 512
	// waitDelay bounds how long git's pipes are waited for once git has
	// exited or been killed. A hook git started can hold its standard error
	// open after git itself is gone.
	waitDelay = 5 * time.Second
)

// gitArgs is the command line for service ("git-receive-pack" or
// "git-upload-pack") in dir. The keep-alive is set here, so the host's own git
// configuration can't turn it off.
func gitArgs(service string, keepAlive int, advertise bool, dir string) []string {
	ka := strconv.Itoa(keepAlive)
	args := []string{
		"-c", "receive.keepAlive=" + ka,
		"-c", "uploadpack.keepAlive=" + ka,
		strings.TrimPrefix(service, "git-"), "--stateless-rpc",
	}
	if advertise {
		args = append(args, "--advertise-refs")
	}
	return append(args, dir)
}

// gitEnv is git's whole environment: PATH and HOME, the protocol the client
// asked for, and for a push what the hook passes on to the bridge (the token's
// row id and the client's address, never the token).
func gitEnv(r *http.Request, service string, tokenID int64, addr netip.Addr) []string {
	env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME")}
	if p := r.Header.Get("Git-Protocol"); validGitProtocol(p) {
		env = append(env, "GIT_PROTOCOL="+p)
	}
	if service == serviceReceive {
		env = append(env, githook.EnvTokenID+"="+strconv.FormatInt(tokenID, 10))
		if addr.IsValid() {
			env = append(env, githook.EnvClientAddr+"="+addr.String())
		}
	}
	return env
}

// validGitProtocol reports whether a Git-Protocol header may be passed to git:
// only letters, digits, '=' and ':'.
func validGitProtocol(p string) bool {
	if p == "" || len(p) > 256 {
		return false
	}
	for i := 0; i < len(p); i++ {
		c := p[i]
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '=' && c != ':' {
			return false
		}
	}
	return true
}

// wantsV2 reports whether git will answer in protocol version 2: only
// upload-pack speaks it, and only when the header reaches git.
func wantsV2(r *http.Request, service string) bool {
	p := r.Header.Get("Git-Protocol")
	if service != serviceUpload || !validGitProtocol(p) {
		return false
	}
	for _, f := range strings.Split(p, ":") {
		if f == "version=2" {
			return true
		}
	}
	return false
}

// serviceAnnouncement is the pkt-line that starts a version 0 or 1
// advertisement, and the flush packet after it.
func serviceAnnouncement(service string) []byte {
	line := "# service=" + service + "\n"
	return fmt.Appendf(nil, "%04x%s0000", len(line)+4, line)
}

// run runs git for c with stdin (nil for none) as its standard input and the
// response as its standard output. The response starts with first. A body
// that says Content-Encoding: gzip is unpacked first. It returns once git has
// exited and been waited for; when the request's stream context ends (the
// client went away, or shutdown began) git is killed.
func (h *Handler) run(c *gitCall, args, env []string, stdin io.Reader, contentType string, first []byte) {
	ctx, cancel := h.o.StreamContext(c.r)
	defer cancel()
	rc := http.NewResponseController(c.w)
	if stdin != nil && strings.EqualFold(c.r.Header.Get("Content-Encoding"), "gzip") {
		zr, ok := h.gunzip(ctx, c, rc, stdin)
		if !ok {
			return
		}
		defer zr.Close()
		stdin = zr
	}
	out := &flushWriter{w: c.w, rc: rc, first: first}
	errLog := &stderrLog{prefix: fmt.Sprintf("githttp: git %s for %s: ", strings.TrimPrefix(c.service, "git-"), c.name)}

	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = env
	cmd.Stdout = out
	cmd.Stderr = errLog
	cmd.Cancel = func() error {
		// End a read of the body that waits on the client, and a write the
		// client isn't reading, so nothing outlives the request.
		_ = rc.SetReadDeadline(time.Now())
		_ = rc.SetWriteDeadline(time.Now())
		return cmd.Process.Kill()
	}
	cmd.WaitDelay = waitDelay
	var feed io.WriteCloser
	if stdin != nil {
		var err error
		if feed, err = cmd.StdinPipe(); err != nil {
			h.fail(c, "git's standard input", err)
			return
		}
	}
	c.w.Header().Set("Content-Type", contentType)
	if err := cmd.Start(); err != nil {
		h.fail(c, "start git", err)
		return
	}
	if h.afterStart != nil {
		h.afterStart(cmd.Process)
	}
	// The body is copied here, not by exec, so that once git is gone a read
	// still waiting on the client can be ended before the handler returns.
	fed := make(chan struct{})
	if feed != nil {
		go func() {
			defer close(fed)
			_, _ = io.Copy(feed, stdin)
			feed.Close()
		}()
	} else {
		close(fed)
	}
	err := cmd.Wait()
	select {
	case <-fed:
	default:
		_ = rc.SetReadDeadline(time.Now())
		<-fed
	}
	errLog.flush()
	if h.afterWait != nil {
		h.afterWait(cmd.ProcessState)
	}
	wrote := out.finish(err == nil)
	if err == nil || ctx.Err() != nil {
		return
	}
	log.Printf("githttp: git %s for %s: %v", strings.TrimPrefix(c.service, "git-"), c.name, err)
	if !wrote {
		answer(c.w, http.StatusInternalServerError, "outhaul: git failed; see the server log")
	}
}

// gunzip reads the gzip header of body under ctx, the request's stream
// context, so a client that says gzip and then sends nothing holds the handler
// only until the client goes away or shutdown begins. A body that isn't gzip
// is refused with 400 before git starts.
func (h *Handler) gunzip(ctx context.Context, c *gitCall, rc *http.ResponseController, body io.Reader) (*gzip.Reader, bool) {
	stop := context.AfterFunc(ctx, func() { _ = rc.SetReadDeadline(time.Now()) })
	zr, err := gzip.NewReader(body)
	stop()
	if err == nil {
		return zr, true
	}
	if ctx.Err() == nil {
		h.refuse(c, http.StatusBadRequest, "gzip", "refused a git POST whose body says it is gzip and isn't",
			"outhaul: the request body says it is gzip, and it isn't")
	}
	return nil, false
}

// errResponseClosed is what flushWriter returns once the handler is done.
var errResponseClosed = errors.New("githttp: the response is finished")

// flushWriter sends each write to the client at once: without a flush the
// server holds short lines back until 4 KiB has gathered. The first write sends
// first ahead of git's own bytes. Once finished it takes nothing more.
type flushWriter struct {
	mu     sync.Mutex
	w      http.ResponseWriter
	rc     *http.ResponseController
	first  []byte
	wrote  bool
	closed bool
}

func (f *flushWriter) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return 0, errResponseClosed
	}
	if err := f.start(); err != nil {
		return 0, err
	}
	n, err := f.w.Write(p)
	if err != nil {
		return n, err
	}
	return n, f.flush()
}

// start sends first, once.
func (f *flushWriter) start() error {
	if f.wrote {
		return nil
	}
	f.wrote = true
	if len(f.first) == 0 {
		return nil
	}
	_, err := f.w.Write(f.first)
	return err
}

func (f *flushWriter) flush() error {
	if err := f.rc.Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return err
	}
	return nil
}

// finish sends first when git succeeded without writing anything, and ends the
// writer. It reports whether anything was sent.
func (f *flushWriter) finish(ok bool) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if ok && !f.wrote {
		if f.start() == nil {
			_ = f.flush()
		}
	}
	f.closed = true
	return f.wrote
}

// stderrLog puts the first few lines git writes to its standard error in the
// log, and drops the rest.
type stderrLog struct {
	mu     sync.Mutex
	prefix string
	buf    []byte
	lines  int
}

func (s *stderrLog) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(p)
	for len(p) > 0 && s.lines < maxStderrLines {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			if room := maxStderrLine - len(s.buf); room > 0 {
				s.buf = append(s.buf, p[:min(room, len(p))]...)
			}
			break
		}
		if room := maxStderrLine - len(s.buf); room > 0 {
			s.buf = append(s.buf, p[:min(room, i)]...)
		}
		s.logLine()
		p = p[i+1:]
	}
	return n, nil
}

// flush logs a last line that had no newline.
func (s *stderrLog) flush() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.buf) > 0 && s.lines < maxStderrLines {
		s.logLine()
	}
}

func (s *stderrLog) logLine() {
	line := strings.TrimSpace(string(s.buf))
	s.buf = s.buf[:0]
	if line == "" {
		return
	}
	log.Printf("%s%q", s.prefix, line)
	s.lines++
}
