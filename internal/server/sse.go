package server

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"sort"

	"github.com/slipwaydev/slipway/internal/compose"
	"github.com/slipwaydev/slipway/internal/core"
)

// handleLogsSSE streams a deployment's build/deploy log to the browser as
// Server-Sent Events. It first replays history, then forwards live lines until
// the deployment finishes (broker closes the topic) or the client disconnects.
func (s *Server) handleLogsSSE(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // disable proxy buffering

	history, lines, cancel := s.broker.Subscribe(id)
	defer cancel()

	for _, line := range history {
		writeSSE(w, "log", line)
	}
	flusher.Flush()

	for {
		select {
		case <-r.Context().Done():
			return
		case line, open := <-lines:
			if !open {
				// Topic closed: the deployment reached a terminal state. Tell the
				// client to stop and reload for the final status.
				writeSSE(w, "done", "")
				flusher.Flush()
				return
			}
			writeSSE(w, "log", line)
			flusher.Flush()
		}
	}
}

// writeSSE writes a single named SSE event with a one-line data payload.
func writeSSE(w http.ResponseWriter, event, data string) {
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
}

// runtimeLogTails whitelists the tail-length selector's values; anything else
// falls back to the default so no user-supplied number reaches the Docker API.
var runtimeLogTails = map[string]int{"100": 100, "500": 500, "1000": 1000, "5000": 5000}

const defaultLogTail = 100

// handleRuntimeLogsSSE streams a running (or stopped) container's stdout+stderr
// to the browser as Server-Sent Events: the last ?tail= lines, then following
// live until the container stops or the client disconnects. Unlike build logs
// there is no broker or history here — the Docker daemon is the log store, and
// each request opens its own follow stream.
func (s *Server) handleRuntimeLogsSSE(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	app, err := s.store.GetApp(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	s.streamContainerLogs(w, r, func() (string, string) {
		return s.runtimeLogTarget(r.Context(), app, r.URL.Query().Get("service"))
	})
}

// streamContainerLogs is the shared body of the runtime-log SSE endpoints
// (apps and databases): resolve a container, replay the last ?tail= lines,
// then follow until the container stops or the client disconnects.
func (s *Server) streamContainerLogs(w http.ResponseWriter, r *http.Request, resolve func() (containerID, errMsg string)) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // disable proxy buffering

	tail := defaultLogTail
	if t, ok := runtimeLogTails[r.URL.Query().Get("tail")]; ok {
		tail = t
	}

	// Anything that stops the stream travels as an in-stream "err" event:
	// EventSource clients cannot read the body of a non-200 response.
	cid, errMsg := resolve()
	if errMsg != "" {
		writeSSE(w, "err", errMsg)
		flusher.Flush()
		return
	}
	logs, err := s.runtime.ContainerLogs(r.Context(), cid, tail)
	if err != nil {
		writeSSE(w, "err", "Could not open the container's logs: "+err.Error())
		flusher.Flush()
		return
	}
	defer logs.Close()
	// A follow stream only ends when the container stops; close it when the
	// browser disconnects so the scanner below unblocks.
	go func() {
		<-r.Context().Done()
		logs.Close()
	}()

	sc := bufio.NewScanner(logs)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		writeSSE(w, "log", sc.Text())
		flusher.Flush()
	}
	if r.Context().Err() != nil {
		return // client went away; nothing to tell it
	}
	// Stream EOF: the container stopped (or was never running).
	writeSSE(w, "done", "")
	flusher.Flush()
}

// runtimeLogTarget resolves which container's logs to stream. It returns a
// human-readable message instead of an HTTP error when there is nothing to
// stream (no deploy yet, unknown compose service) — the SSE handler forwards
// it in-stream. Compose apps tail one service at a time; an empty service
// picks the stack's first, alphabetically.
func (s *Server) runtimeLogTarget(ctx context.Context, app core.App, service string) (containerID, errMsg string) {
	if app.Kind == core.KindCompose {
		cs, err := s.runtime.ListContainers(ctx,
			map[string]string{"com.docker.compose.project": compose.ProjectName(app.Name)})
		if err != nil {
			return "", "Could not list the stack's containers: " + err.Error()
		}
		if len(cs) == 0 {
			return "", "No containers for this app yet — deploy it first."
		}
		sort.Slice(cs, func(i, j int) bool {
			return cs[i].Labels["com.docker.compose.service"] < cs[j].Labels["com.docker.compose.service"]
		})
		if service == "" {
			return cs[0].ID, ""
		}
		for _, c := range cs {
			if c.Labels["com.docker.compose.service"] == service {
				return c.ID, ""
			}
		}
		return "", fmt.Sprintf("The stack has no %q service.", service)
	}
	c, err := s.runtime.FindContainer(ctx, appContainerPrefix+app.Name)
	if err != nil {
		return "", "Could not find the app's container: " + err.Error()
	}
	if c == nil {
		return "", "No container for this app yet — deploy it first."
	}
	return c.ID, ""
}
