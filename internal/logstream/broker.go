// Package logstream is an in-memory pub/sub broker for build and deploy log
// lines, keyed by deployment ID. It decouples the deploy pipeline (producer)
// from SSE handlers (consumers): the pipeline never knows about HTTP, and
// handlers never know about the build.
//
// Each topic retains a full history of its lines so a browser that opens the
// log view mid-build or after completion still sees everything. History lives
// only for the process lifetime; durable log storage is a later seam.
package logstream

import "sync"

// subBuffer bounds each subscriber's channel. If a consumer falls this far
// behind, live lines are dropped for that consumer — but the complete log
// remains in the topic history, so a re-subscribe recovers it.
const subBuffer = 1024

type subscriber struct {
	ch     chan string
	closed bool
}

type topic struct {
	mu     sync.Mutex
	lines  []string
	subs   map[*subscriber]struct{}
	closed bool
}

// Broker fans log lines out to subscribers per deployment.
type Broker struct {
	mu     sync.Mutex
	topics map[int64]*topic
}

// New returns an empty Broker.
func New() *Broker {
	return &Broker{topics: map[int64]*topic{}}
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
	t := b.topicFor(id)
	t.mu.Lock()
	defer t.mu.Unlock()

	history = append([]string(nil), t.lines...)
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
// channel so their range loops terminate. History is retained for later views.
func (b *Broker) Close(id int64) {
	t := b.topicFor(id)
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
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
}
