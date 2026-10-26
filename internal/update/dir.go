package update

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The update directory is the only channel between Outhaul and the root
// updater: "updater" (marker, written by the installer), "request" (created by
// Outhaul, contents never read) and "status" (written by the updater). The
// updater's log stays in its own directory, where Outhaul only reads it.

// Updater states, as written to the status file.
const (
	StateRunning     = "running"
	StateUpToDate    = "up_to_date"
	StateBuildFailed = "build_failed"
	StateUpdated     = "updated"
	StateRolledBack  = "rolled_back"
	StateFailed      = "failed"
	StateNotStarted  = "not_started"
)

// LogPath is the updater's log on the server: root's own file, in the
// updater's directory (WORK_DIR in deploy/update.sh), which is still there
// when an update leaves Outhaul down. Outhaul can read it, not change it.
const LogPath = "/var/lib/outhaul-updater/log"

const (
	requestTTL       = 2 * time.Minute
	runningTTL       = 40 * time.Minute
	logChunk         = 64 << 10
	notStartedNotice = "The updater did not start. Re-run the installer to set it up again."
)

// ErrInProgress is returned by Request while an update is pending or running.
var ErrInProgress = errors.New("an update is already in progress")

// Status is the updater's last reported state.
type Status struct {
	State, From, To, Message string
	Started, Finished        time.Time
}

// Interrupted reports a run that claims to be running but began more than 40
// minutes ago, so the updater must have died.
func (s Status) Interrupted(now time.Time) bool {
	return s.State == StateRunning && now.Sub(s.Started) > runningTTL
}

// Dir is the update directory, and the updater's log. It is safe for
// concurrent use.
type Dir struct {
	path string
	log  string
	now  func() time.Time
	mu   sync.Mutex
}

// NewDir reads and writes the update directory at path, and reads the
// updater's log at logPath (LogPath on a server).
func NewDir(path, logPath string, now func() time.Time) *Dir {
	return &Dir{path: path, log: logPath, now: now}
}

func (d *Dir) file(name string) string { return filepath.Join(d.path, name) }

// Installed reports whether the installer set up the updater.
func (d *Dir) Installed() bool {
	_, err := os.Stat(d.file("updater"))
	return err == nil
}

// Request asks the updater to run, unless an update is already pending.
func (d *Dir) Request() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.inProgress() {
		return ErrInProgress
	}
	f, err := os.OpenFile(d.file("request"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if errors.Is(err, os.ErrExist) {
		return ErrInProgress
	}
	if err != nil {
		return err
	}
	return f.Close()
}

// InProgress reports whether an update is requested or running. A request the
// updater never claimed within two minutes is removed and recorded as
// not_started.
func (d *Dir) InProgress() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.inProgress()
}

func (d *Dir) inProgress() bool {
	now := d.now()
	st, hasStatus, _ := d.readStatus()
	if hasStatus && st.State == StateRunning && !st.Interrupted(now) {
		return true
	}
	fi, err := os.Stat(d.file("request"))
	if err != nil {
		return false
	}
	if now.Sub(fi.ModTime()) <= requestTTL {
		return true
	}
	_ = os.Remove(d.file("request"))
	_ = d.writeStatus(Status{State: StateNotStarted, Message: notStartedNotice, Started: now, Finished: now})
	return false
}

// Status returns the updater's status; false when there is no status file.
func (d *Dir) Status() (Status, bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.readStatus()
}

func (d *Dir) readStatus() (Status, bool, error) {
	b, err := os.ReadFile(d.file("status"))
	if errors.Is(err, os.ErrNotExist) {
		return Status{}, false, nil
	}
	if err != nil {
		return Status{}, false, err
	}
	var s Status
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), "=")
		if !ok {
			continue
		}
		switch k {
		case "state":
			s.State = v
		case "from":
			s.From = v
		case "to":
			s.To = v
		case "message":
			s.Message = v
		case "started":
			s.Started = parseUnix(v)
		case "finished":
			s.Finished = parseUnix(v)
		}
	}
	return s, true, nil
}

func parseUnix(v string) time.Time {
	n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
	if err != nil {
		return time.Time{}
	}
	return time.Unix(n, 0)
}

// writeStatus writes by temp file + rename, as the updater does.
func (d *Dir) writeStatus(s Status) error {
	var b strings.Builder
	b.WriteString("state=" + s.State + "\n")
	b.WriteString("message=" + s.Message + "\n")
	b.WriteString("started=" + strconv.FormatInt(s.Started.Unix(), 10) + "\n")
	b.WriteString("finished=" + strconv.FormatInt(s.Finished.Unix(), 10) + "\n")
	tmp := d.file("status.tmp")
	if err := os.WriteFile(tmp, []byte(b.String()), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, d.file("status"))
}

// ReadLog returns up to 64 KiB of the updater's log from offset, and the
// offset to continue from. A missing log is empty; an offset past the end (a
// new, shorter log) restarts from 0.
func (d *Dir) ReadLog(offset int64) ([]byte, int64, error) {
	f, err := os.Open(d.log)
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	if offset < 0 || offset > fi.Size() {
		offset = 0
	}
	buf := make([]byte, logChunk)
	n, err := f.ReadAt(buf, offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, 0, err
	}
	return buf[:n], offset + int64(n), nil
}

// LogTail returns the end of the updater's log: all of it up to 64 KiB, or
// else the whole lines of its last 64 KiB. A missing log is empty.
func (d *Dir) LogTail() ([]byte, error) {
	f, err := os.Open(d.log)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if fi.Size() <= logChunk {
		b, err := io.ReadAll(io.LimitReader(f, logChunk))
		return b, err
	}
	// The last 64 KiB and the byte before them, which says whether they
	// begin with a whole line.
	buf := make([]byte, logChunk+1)
	n, err := f.ReadAt(buf, fi.Size()-logChunk-1)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	buf = buf[:n]
	if i := bytes.IndexByte(buf, '\n'); i >= 0 {
		return buf[i+1:], nil
	}
	if n == 0 {
		return nil, nil // the log was replaced meanwhile
	}
	return buf[1:], nil // one line longer than 64 KiB
}
