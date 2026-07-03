package server

import (
	"fmt"
	"net/http"
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
