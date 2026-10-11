// Package logstream is an in-memory pub/sub broker for build and deploy log
// lines, keyed by deployment ID. It decouples the deploy pipeline (producer)
// from SSE handlers (consumers): the pipeline never knows about HTTP, and
// handlers never know about the build.
//
// Each topic retains a history of its lines so a browser that opens the log
// view mid-build or after completion still sees the log. History is in memory
// only and bounded: a topic keeps the tail of its log, and only the most
// recently finished topics are kept at all. Durable log storage is a later
// seam.
package logstream

import (
	"fmt"
	"sync"
)

// subBuffer bounds each subscriber's channel. If a consumer falls this far
// behind, live lines are dropped for that consumer — but the log remains in
// the topic history, so a re-subscribe recovers it.
const subBuffer = 1024

const (
	// defaultKeepClosed is how many finished deployments' logs stay viewable.
	defaultKeepClosed = 50
	// defaultMaxLines bounds one topic's history. Past twice this, the oldest
	// lines are dropped down to it, so a topic holds at most 2x.
	defaultMaxLines = 10000
)

type subscriber struct {
	ch     chan string
	closed bool
}

type topic struct {
	mu      sync.Mutex
	lines   []string
	dropped int // lines trimmed off the front of the history
	subs    map[*subscriber]struct{}
	closed  bool
}

// history snapshots the topic's lines, led by a note when the front of the
// log was trimmed. Callers hold t.mu.
func (t *topic) history(maxLines int) []string {
	if t.dropped == 0 {
		return append([]string(nil), t.lines...)
	}
	out := make([]string, 0, len(t.lines)+1)
	out = append(out, fmt.Sprintf("… %d earlier lines were dropped; only the last %d or so are kept.", t.dropped, maxLines))
	return append(out, t.lines...)
}

// Broker fans log lines out to subscribers per deployment.
type Broker struct {
	mu     sync.Mutex
	topics map[int64]*topic
	closed []int64 // closed topic IDs, oldest first

	keepClosed int // finished topics retained
	maxLines   int // history bound per topic
}

// New returns an empty Broker.
func New() *Broker {
	return &Broker{
		topics:     map[int64]*topic{},
		keepClosed: defaultKeepClosed,
		maxLines:   defaultMaxLines,
	}
}

// topicFor returns (creating if needed) the topic for a deployment ID.
func (b *Broker) topicFor(id int64) *topic {
	b.mu.Lock()
	defer b.mu.Unlock()
	t, ok := b.topics[id]
	if !ok {
		t = &topic{subs: map[*subscriber]struct{}{}}
		b.topics[id] = t
	}
	return t
}

// Publish appends a line to the deployment's history and delivers it to current
// subscribers. It never blocks the producer: a full subscriber buffer drops the
// live line (still recoverable from history).
func (b *Broker) Publish(id int64, line string) {
	t := b.topicFor(id)
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return
	}
	t.lines = append(t.lines, line)
	if len(t.lines) > 2*b.maxLines {
		cut := len(t.lines) - b.maxLines
		t.lines = append([]string(nil), t.lines[cut:]...)
		t.dropped += cut
	}
	for s := range t.subs {
		select {
		case s.ch <- line:
		default: // slow consumer; drop, history retains the line
		}
	}
}

// Subscribe returns the deployment's log history so far, a channel of subsequent
// lines, and a cancel function to unsubscribe. History and registration are
// snapshotted atomically, so no line is missed or duplicated across the seam.
// If the topic is already closed, the returned channel is closed too.
func (b *Broker) Subscribe(id int64) (history []string, lines <-chan string, cancel func()) {
	return b.subscribe(b.topicFor(id))
}

// SubscribeExisting is Subscribe for a deployment that has already finished:
// it subscribes only if the broker still holds the deployment's topic, and
// reports ok=false otherwise without creating one. A topic created for a
// finished deployment would never be closed, so its viewer would wait forever.
func (b *Broker) SubscribeExisting(id int64) (history []string, lines <-chan string, cancel func(), ok bool) {
	b.mu.Lock()
	t, ok := b.topics[id]
	b.mu.Unlock()
	if !ok {
		return nil, nil, nil, false
	}
	history, lines, cancel = b.subscribe(t)
	return history, lines, cancel, true
}

func (b *Broker) subscribe(t *topic) (history []string, lines <-chan string, cancel func()) {
	t.mu.Lock()
	defer t.mu.Unlock()

	history = t.history(b.maxLines)
	s := &subscriber{ch: make(chan string, subBuffer)}

	if t.closed {
		close(s.ch)
		s.closed = true
		return history, s.ch, func() {}
	}

	t.subs[s] = struct{}{}
	return history, s.ch, func() {
		t.mu.Lock()
		defer t.mu.Unlock()
		if !s.closed {
			s.closed = true
			close(s.ch)
			delete(t.subs, s)
		}
	}
}

// Close marks the deployment's stream finished and closes every subscriber
// channel so their range loops terminate. History is retained for later views
// until keepClosed newer topics have closed after it.
func (b *Broker) Close(id int64) {
	t := b.topicFor(id)
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return
	}
	t.closed = true
	for s := range t.subs {
		if !s.closed {
			s.closed = true
			close(s.ch)
		}
	}
	t.subs = map[*subscriber]struct{}{}
	t.mu.Unlock()

	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = append(b.closed, id)
	for len(b.closed) > b.keepClosed {
		delete(b.topics, b.closed[0])
		b.closed = b.closed[1:]
	}
}
