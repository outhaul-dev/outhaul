package server

import (
	"net/http"
	"runtime"
	"strconv"
	"strings"

	"github.com/outhaul-dev/outhaul/internal/core"
)

// limitSources names where each value in force came from.
type limitSources struct {
	CPU, Memory, Pids string
}

// handleAppLimits stores one app's own container ceiling. Empty means the
// server default, so an empty form clears the app's override. The route and
// the panel exist only when a server default is set.
func (s *Server) handleAppLimits(w http.ResponseWriter, r *http.Request) {
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
	if app.Ephemeral {
		http.Error(w, "a preview uses its parent's limits", http.StatusBadRequest)
		return
	}
	if !s.hasDefaultLimits() {
		http.NotFound(w, r)
		return
	}

	var l core.Limits
	if v := strings.TrimSpace(r.FormValue("limit_cpus")); v != "" {
		milli, err := core.ParseCPUs(v)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if milli > int64(runtime.NumCPU())*1000 {
			http.Error(w, "limit_cpus: "+v+" CPUs is more than this server's "+strconv.Itoa(runtime.NumCPU()), http.StatusBadRequest)
			return
		}
		l.MilliCPUs = milli
	}
	if v := strings.TrimSpace(r.FormValue("limit_memory")); v != "" {
		b, err := core.ParseMemory(v)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		l.Memory = b
	}
	if v := strings.TrimSpace(r.FormValue("limit_pids")); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 1 {
			http.Error(w, "a whole number of processes, 1 or more", http.StatusBadRequest)
			return
		}
		l.Pids = n
	}

	if err := s.store.SetAppLimits(r.Context(), id, l); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/apps/"+strconv.FormatInt(id, 10), http.StatusSeeOther)
}
