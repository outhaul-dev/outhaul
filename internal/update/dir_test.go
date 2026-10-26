package update

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

var t0 = time.Now().Truncate(time.Second)

func newTestDir(t *testing.T) (*Dir, string) {
	t.Helper()
	p := t.TempDir()
	return NewDir(p, filepath.Join(p, "log"), func() time.Time { return t0 }), p
}

func write(t *testing.T, p, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(p, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func unix(t time.Time) string { return strconv.FormatInt(t.Unix(), 10) }

func TestRequestCreatesTheFileOnce(t *testing.T) {
	d, p := newTestDir(t)
	if err := d.Request(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(p, "request")); err != nil {
		t.Fatal(err)
	}
	if err := d.Request(); !errors.Is(err, ErrInProgress) {
		t.Fatalf("got %v", err)
	}
}

func TestInProgressWhileTheUpdaterRuns(t *testing.T) {
	d, p := newTestDir(t)
	write(t, p, "status", "state=running\nstarted="+unix(t0.Add(-5*time.Minute))+"\n")
	if !d.InProgress() {
		t.Fatal("want in progress")
	}
	if err := d.Request(); !errors.Is(err, ErrInProgress) {
		t.Fatalf("got %v", err)
	}
}

func TestInterruptedRunDoesNotHold(t *testing.T) {
	d, p := newTestDir(t)
	write(t, p, "status", "state=running\nstarted="+unix(t0.Add(-41*time.Minute))+"\n")
	if d.InProgress() {
		t.Fatal("interrupted run must not hold")
	}
	s, ok, err := d.Status()
	if err != nil || !ok || !s.Interrupted(t0) {
		t.Fatalf("%+v %v %v", s, ok, err)
	}
}

func TestUnclaimedRequestExpires(t *testing.T) {
	d, p := newTestDir(t)
	req := filepath.Join(p, "request")
	write(t, p, "request", "")
	if err := os.Chtimes(req, t0.Add(-time.Minute), t0.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if !d.InProgress() {
		t.Fatal("fresh request holds")
	}
	if _, err := os.Stat(req); err != nil {
		t.Fatal("request must be kept")
	}
	if err := os.Chtimes(req, t0.Add(-3*time.Minute), t0.Add(-3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if d.InProgress() {
		t.Fatal("stale request must not hold")
	}
	if _, err := os.Stat(req); !os.IsNotExist(err) {
		t.Fatal("request must be removed")
	}
	s, ok, err := d.Status()
	if err != nil || !ok || s.State != StateNotStarted ||
		s.Message != "The updater did not start. Re-run the installer to set it up again." {
		t.Fatalf("%+v %v %v", s, ok, err)
	}
}

func TestStatusParsesTheUpdatersFile(t *testing.T) {
	d, p := newTestDir(t)
	write(t, p, "status", "state=updated\nfrom=aaa\nto=bbb\nmessage=updated from aaa to bbb\nstarted=100\nfinished=160\nbogus=x\n")
	s, ok, err := d.Status()
	if err != nil || !ok {
		t.Fatal(err, ok)
	}
	want := Status{State: StateUpdated, From: "aaa", To: "bbb", Message: "updated from aaa to bbb",
		Started: time.Unix(100, 0), Finished: time.Unix(160, 0)}
	if s != want {
		t.Fatalf("got %+v", s)
	}
	d2, _ := newTestDir(t)
	if _, ok, err := d2.Status(); ok || err != nil {
		t.Fatal(ok, err)
	}
}

func TestReadLogFromOffset(t *testing.T) {
	d, p := newTestDir(t)
	if c, n, err := d.ReadLog(5); err != nil || len(c) != 0 || n != 0 {
		t.Fatal(c, n, err)
	}
	write(t, p, "log", "one\ntwo\n")
	for _, tc := range []struct {
		off  int64
		want string
		next int64
	}{{0, "one\ntwo\n", 8}, {4, "two\n", 8}, {100, "one\ntwo\n", 8}} {
		c, n, err := d.ReadLog(tc.off)
		if err != nil || string(c) != tc.want || n != tc.next {
			t.Fatalf("off %d: %q %d %v", tc.off, c, n, err)
		}
	}
}

func TestReadLogCapsAtSixtyFourKiB(t *testing.T) {
	d, p := newTestDir(t)
	write(t, p, "log", string(make([]byte, 100<<10)))
	c, n, err := d.ReadLog(0)
	if err != nil || len(c) != 64<<10 || n != 64<<10 {
		t.Fatal(len(c), n, err)
	}
}

func TestInstalledNeedsTheMarker(t *testing.T) {
	d, p := newTestDir(t)
	if d.Installed() {
		t.Fatal("no marker")
	}
	write(t, p, "updater", "1\n")
	if !d.Installed() {
		t.Fatal("marker")
	}
}

// The admin UI names the updater's own log, which the updater keeps in its
// directory: the two must agree.
func TestLogPathIsTheUpdatersLog(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "deploy", "update.sh"))
	if err != nil {
		t.Fatal(err)
	}
	var work string
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, "WORK_DIR="); ok {
			work = v
		}
	}
	if work == "" {
		t.Fatal("no WORK_DIR= line in deploy/update.sh")
	}
	if want := work + "/log"; LogPath != want {
		t.Errorf("LogPath = %q, want %q (deploy/update.sh's WORK_DIR)", LogPath, want)
	}
}

func TestLogTail(t *testing.T) {
	d, p := newTestDir(t)
	if c, err := d.LogTail(); err != nil || len(c) != 0 {
		t.Fatalf("no log: %q %v", c, err)
	}
	write(t, p, "log", "one\ntwo\n")
	if c, err := d.LogTail(); err != nil || string(c) != "one\ntwo\n" {
		t.Fatalf("short log: %q %v", c, err)
	}

	// Longer than 64 KiB: the end, from the start of a line.
	var b strings.Builder
	b.WriteString("the first line\n")
	for b.Len() < 100<<10 {
		b.WriteString(strings.Repeat("x", 99) + "\n")
	}
	b.WriteString("the last line\n")
	whole := b.String()
	write(t, p, "log", whole)
	c, err := d.LogTail()
	if err != nil {
		t.Fatal(err)
	}
	if len(c) == 0 || len(c) > 64<<10 {
		t.Fatalf("tail is %d bytes, want 1..%d", len(c), 64<<10)
	}
	if !strings.HasSuffix(whole, string(c)) || !strings.HasSuffix(string(c), "the last line\n") {
		t.Fatalf("tail is not the end of the log: %q", c[:20])
	}
	if start := len(whole) - len(c); whole[start-1] != '\n' {
		t.Fatalf("tail starts mid-line: %q", c[:20])
	}
}

// The updater keeps its log in its own directory, not the update directory:
// both readers use the log path the Dir was given.
func TestTheLogIsReadFromItsOwnPath(t *testing.T) {
	p := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "log")
	d := NewDir(p, logPath, func() time.Time { return t0 })
	write(t, p, "log", "not this one\n")
	if err := os.WriteFile(logPath, []byte("the updater's log\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if c, _, err := d.ReadLog(0); err != nil || string(c) != "the updater's log\n" {
		t.Errorf("ReadLog = %q, %v", c, err)
	}
	if c, err := d.LogTail(); err != nil || string(c) != "the updater's log\n" {
		t.Errorf("LogTail = %q, %v", c, err)
	}
}
