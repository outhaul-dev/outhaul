package compose

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"
)

// Call records one Runner invocation on the Fake.
type Call struct {
	Verb    string // "build", "up", "stop", "restart", "down"
	Dir     string
	Files   []string
	Project string
}

// Fake is an in-memory Runner for unit tests. Per-verb error fields inject
// failures; Calls records every invocation in order. Hook (optional) runs on
// every call while the deploy's work dir still exists, so tests can inspect
// generated files (.env, the override) before the pipeline cleans up.
type Fake struct {
	mu    sync.Mutex
	Calls []Call

	Hook func(c Call)

	FailBuild   error
	FailUp      error
	FailStop    error
	FailRestart error
	FailDown    error
}

func (f *Fake) Build(_ context.Context, dir string, files []string, project string, out io.Writer) error {
	return f.record(Call{Verb: "build", Dir: dir, Files: files, Project: project}, f.FailBuild, out)
}

func (f *Fake) Up(_ context.Context, dir string, files []string, project string, _ time.Duration, out io.Writer) error {
	return f.record(Call{Verb: "up", Dir: dir, Files: files, Project: project}, f.FailUp, out)
}

func (f *Fake) Stop(_ context.Context, project string, out io.Writer) error {
	return f.record(Call{Verb: "stop", Project: project}, f.FailStop, out)
}

func (f *Fake) Restart(_ context.Context, project string, out io.Writer) error {
	return f.record(Call{Verb: "restart", Project: project}, f.FailRestart, out)
}

func (f *Fake) Down(_ context.Context, project string, out io.Writer) error {
	return f.record(Call{Verb: "down", Project: project}, f.FailDown, out)
}

func (f *Fake) record(c Call, fail error, out io.Writer) error {
	f.mu.Lock()
	f.Calls = append(f.Calls, c)
	hook := f.Hook
	f.mu.Unlock()
	if hook != nil {
		hook(c)
	}
	if fail != nil {
		return fail
	}
	if out != nil {
		fmt.Fprintf(out, "compose %s %s\n", c.Verb, c.Project)
	}
	return nil
}

// CallsFor returns the recorded calls with the given verb.
func (f *Fake) CallsFor(verb string) []Call {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Call
	for _, c := range f.Calls {
		if c.Verb == verb {
			out = append(out, c)
		}
	}
	return out
}
