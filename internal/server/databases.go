package server

import (
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/james-smart/outhaul/internal/core"
	"github.com/james-smart/outhaul/internal/dbaas"
)

// databasePath is the detail page for a database.
func databasePath(id int64) string { return "/databases/" + strconv.FormatInt(id, 10) }

// handleCreateDatabase creates a managed database in a project and kicks off
// provisioning (pull + create + start) in the background, then sends the
// operator to the database page to watch it come up.
func (s *Server) handleCreateDatabase(w http.ResponseWriter, r *http.Request) {
	projectID, ok := parseID(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	p, err := s.store.GetProject(r.Context(), projectID)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	name := strings.TrimSpace(r.FormValue("name"))
	engine := r.FormValue("engine")
	image := strings.TrimSpace(r.FormValue("image"))

	if !appNameRe.MatchString(name) {
		s.renderProject(w, r, http.StatusBadRequest, p, "Database name must be lowercase letters, digits and hyphens (2–40 chars).")
		return
	}
	// "root" would collide with MySQL's built-in superuser (the database's name
	// doubles as its username); reserve it across engines for one simple rule.
	if name == "root" {
		s.renderProject(w, r, http.StatusBadRequest, p, "The name \"root\" is reserved.")
		return
	}
	if !dbaas.ValidEngine(engine) {
		s.renderProject(w, r, http.StatusBadRequest, p, "Unknown database engine.")
		return
	}
	extPort, msg := parseExtPort(r.FormValue("external_port"))
	if msg != "" {
		s.renderProject(w, r, http.StatusBadRequest, p, msg)
		return
	}
	if image == "" {
		image = dbaas.DefaultImage(engine)
	}

	d := core.Database{
		ProjectID: projectID,
		Name:      name,
		Engine:    engine,
		Image:     image,
		Password:  dbaas.NewPassword(),
		ExtPort:   extPort,
	}
	if dbaas.HasUserDB(engine) {
		d.Username = name
		d.DBName = name
	}
	d, err = s.store.CreateDatabase(r.Context(), d)
	if err != nil {
		// Most likely a duplicate name (UNIQUE constraint).
		s.renderProject(w, r, http.StatusBadRequest, p, "Could not create database: "+err.Error())
		return
	}
	s.databases.Provision(d)
	http.Redirect(w, r, databasePath(d.ID), http.StatusSeeOther)
}

// parseExtPort validates the optional external-port form field. 0 means
// internal-only.
func parseExtPort(v string) (int, string) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, ""
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > 65535 {
		return 0, "External port must be a port number (1–65535), or empty for internal-only."
	}
	return n, ""
}

// renderDatabase renders the database detail page (also used to redisplay it
// with an error after a rejected action).
func (s *Server) renderDatabase(w http.ResponseWriter, r *http.Request, status int, d core.Database, errMsg string) {
	p, err := s.store.GetProject(r.Context(), d.ProjectID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// The stored status says what the manager last did; the container state is
	// the runtime truth (e.g. "exited" after a crash, with restarts pending).
	containerState := "not created"
	if c, err := s.runtime.FindContainer(r.Context(), dbaas.ContainerName(d.Name)); err == nil && c != nil {
		containerState = c.State
	}
	// The external URL's host is whatever address the operator reached the
	// admin UI on — the best guess a single-server setup has for itself.
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	data := map[string]any{
		"Title":          d.Name,
		"Active":         "projects",
		"Database":       d,
		"Project":        p,
		"ContainerState": containerState,
		"InternalURL":    dbaas.InternalURL(d),
		"ExternalURL":    dbaas.ExternalURL(d, host),
		"EnvExample":     strings.ToUpper(strings.ReplaceAll(d.Name, "-", "_")) + "_URL",
	}
	// Redis is cache-shaped and has no dump tooling (matching Dokploy); its
	// page omits the backups panel entirely.
	if d.Engine != core.EngineRedis {
		panel, err := s.backupPanelData(r.Context(), core.BackupTargetDatabase, d.ID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		for k, v := range panel {
			data[k] = v
		}
	}
	if errMsg != "" {
		data["Error"] = errMsg
	}
	s.render(w, status, "database", data)
}

func (s *Server) handleDatabaseDetail(w http.ResponseWriter, r *http.Request) {
	d, ok := s.databaseFromPath(w, r)
	if !ok {
		return
	}
	s.renderDatabase(w, r, http.StatusOK, d, "")
}

func (s *Server) handleStartDatabase(w http.ResponseWriter, r *http.Request) {
	d, ok := s.databaseFromPath(w, r)
	if !ok {
		return
	}
	if err := s.databases.Start(r.Context(), d); err != nil {
		s.renderDatabase(w, r, http.StatusInternalServerError, d, "Could not start the database: "+err.Error())
		return
	}
	http.Redirect(w, r, databasePath(d.ID), http.StatusSeeOther)
}

func (s *Server) handleStopDatabase(w http.ResponseWriter, r *http.Request) {
	d, ok := s.databaseFromPath(w, r)
	if !ok {
		return
	}
	if err := s.databases.Stop(r.Context(), d); err != nil {
		s.renderDatabase(w, r, http.StatusInternalServerError, d, "Could not stop the database: "+err.Error())
		return
	}
	http.Redirect(w, r, databasePath(d.ID), http.StatusSeeOther)
}

// handleDatabaseSettings applies a changed external port by reprovisioning:
// the container is recreated with the new port mapping, data intact in the
// bind mount. It doubles as the retry path for a failed provision.
func (s *Server) handleDatabaseSettings(w http.ResponseWriter, r *http.Request) {
	d, ok := s.databaseFromPath(w, r)
	if !ok {
		return
	}
	extPort, msg := parseExtPort(r.FormValue("external_port"))
	if msg != "" {
		s.renderDatabase(w, r, http.StatusBadRequest, d, msg)
		return
	}
	if err := s.store.SetDatabaseExtPort(r.Context(), d.ID, extPort); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	d.ExtPort = extPort
	s.databases.Provision(d)
	http.Redirect(w, r, databasePath(d.ID), http.StatusSeeOther)
}

func (s *Server) handleDeleteDatabase(w http.ResponseWriter, r *http.Request) {
	d, ok := s.databaseFromPath(w, r)
	if !ok {
		return
	}
	if err := s.databases.Remove(r.Context(), d); err != nil {
		s.renderDatabase(w, r, http.StatusInternalServerError, d, "Could not delete the database: "+err.Error())
		return
	}
	http.Redirect(w, r, "/projects/"+strconv.FormatInt(d.ProjectID, 10), http.StatusSeeOther)
}

// handleDatabaseLogsSSE live-tails the database container's logs, exactly like
// an app's runtime logs.
func (s *Server) handleDatabaseLogsSSE(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	d, err := s.store.GetDatabase(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	s.streamContainerLogs(w, r, func() (string, string) {
		c, err := s.runtime.FindContainer(r.Context(), dbaas.ContainerName(d.Name))
		if err != nil {
			return "", "Could not find the database's container: " + err.Error()
		}
		if c == nil {
			return "", "No container for this database yet."
		}
		return c.ID, ""
	})
}

// databaseFromPath loads the database named by the {id} path segment, writing
// a 404 when it doesn't resolve.
func (s *Server) databaseFromPath(w http.ResponseWriter, r *http.Request) (core.Database, bool) {
	id, ok := parseID(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return core.Database{}, false
	}
	d, err := s.store.GetDatabase(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return core.Database{}, false
	}
	return d, true
}
