package logstream

import (
	"testing"
	"time"
)

// recv reads one line with a timeout so a broken broker fails fast instead of
// hanging the test.
func recv(t *testing.T, ch <-chan string) (string, bool) {
	t.Helper()
	select {
	case line, ok := <-ch:
		return line, ok
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for a log line")
		return "", false
	}
}

func TestSubscribeReceivesLiveLines(t *testing.T) {
	b := New()
	_, ch, cancel := b.Subscribe(1)
	defer cancel()

	b.Publish(1, "hello")
	line, ok := recv(t, ch)
	if !ok || line != "hello" {
		t.Fatalf("got (%q, %v), want (hello, true)", line, ok)
	}
}

func TestSubscribeReplaysHistory(t *testing.T) {
	b := New()
	b.Publish(1, "line-1")
	b.Publish(1, "line-2")

	history, _, cancel := b.Subscribe(1)
	defer cancel()

	if len(history) != 2 || history[0] != "line-1" || history[1] != "line-2" {
		t.Fatalf("history = %v, want [line-1 line-2]", history)
	}
}

func TestHistoryThenLiveAreContiguous(t *testing.T) {
	b := New()
	b.Publish(1, "past")

	history, ch, cancel := b.Subscribe(1)
	defer cancel()
	b.Publish(1, "future")

	if len(history) != 1 || history[0] != "past" {
		t.Fatalf("history = %v, want [past]", history)
	}
	if line, _ := recv(t, ch); line != "future" {
		t.Fatalf("live line = %q, want future", line)
	}
}

func TestCloseEndsTheStream(t *testing.T) {
	b := New()
	_, ch, cancel := b.Subscribe(1)
	defer cancel()

	b.Publish(1, "a")
	recv(t, ch) // drain "a"
	b.Close(1)

	// Channel must close so an SSE range loop terminates.
	if _, ok := recv(t, ch); ok {
		t.Fatal("expected channel to be closed after Close")
	}
}

func TestSubscribeAfterCloseGetsHistoryAndClosedChannel(t *testing.T) {
	b := New()
	b.Publish(1, "only")
	b.Close(1)

	history, ch, cancel := b.Subscribe(1)
	defer cancel()

	if len(history) != 1 || history[0] != "only" {
		t.Fatalf("history = %v, want [only]", history)
	}
	if _, ok := <-ch; ok {
		t.Fatal("channel for a closed topic should already be closed")
	}
}

func TestMultipleSubscribersEachReceive(t *testing.T) {
	b := New()
	_, ch1, c1 := b.Subscribe(1)
	defer c1()
	_, ch2, c2 := b.Subscribe(1)
	defer c2()

	b.Publish(1, "broadcast")
	if l, _ := recv(t, ch1); l != "broadcast" {
		t.Errorf("sub1 got %q", l)
	}
	if l, _ := recv(t, ch2); l != "broadcast" {
		t.Errorf("sub2 got %q", l)
	}
}

func TestTopicsAreIsolated(t *testing.T) {
	b := New()
	_, ch1, cancel := b.Subscribe(1)
	defer cancel()

	b.Publish(2, "other-topic")
	select {
	case line := <-ch1:
		t.Fatalf("topic 1 received a line meant for topic 2: %q", line)
	case <-time.After(50 * time.Millisecond):
		// expected: nothing arrives
	}
}

func TestCancelUnsubscribes(t *testing.T) {
	b := New()
	_, ch, cancel := b.Subscribe(1)
	cancel()
	// After cancel the channel is closed and no further lines are delivered.
	if _, ok := <-ch; ok {
		t.Fatal("cancel should close the subscriber channel")
	}
}
