package server

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/outhaul-dev/outhaul/internal/core"
	"github.com/outhaul-dev/outhaul/internal/cron"
	"github.com/outhaul-dev/outhaul/internal/store"
)

// prefixRe restricts backup key prefixes to characters that stay sane in
// object keys and URLs.
var prefixRe = regexp.MustCompile(`^[a-zA-Z0-9._/-]*$`)

// destinationTestTimeout bounds the settings page's Test probe.
const destinationTestTimeout = 30 * time.Second

// backupRow is a backup schedule plus what its panel row shows.
type backupRow struct {
	core.Backup
	Destination string
	Runs        []core.BackupRun
}

// backupPanelData assembles the shared Backups panel's data for one target.
func (s *Server) backupPanelData(ctx context.Context, kind string, targetID int64) (map[string]any, error) {
	backups, err := s.store.ListBackupsForTarget(ctx, kind, targetID)
	if err != nil {
		return nil, err
	}
	dests, err := s.store.ListDestinations(ctx)
	if err != nil {
		return nil, err
	}
	destName := make(map[int64]string, len(dests))
	for _, d := range dests {
		destName[d.ID] = d.Name
	}
	rows := make([]backupRow, 0, len(backups))
	for _, b := range backups {
		runs, err := s.store.ListBackupRuns(ctx, b.ID, 5)
		if err != nil {
			return nil, err
		}
		rows = append(rows, backupRow{Backup: b, Destination: destName[b.DestinationID], Runs: runs})
	}
	return map[string]any{
		"Backups":        rows,
		"Destinations":   dests,
		"BackupKind":     kind,
		"BackupTargetID": targetID,
	}, nil
}

// handleCreateBackup attaches a schedule to a database or compose app.
// Validation errors are plain 400s (same convention as the env handlers).
func (s *Server) handleCreateBackup(w http.ResponseWriter, r *http.Request) {
	kind := r.FormValue("target_kind")
	targetID, ok := parseID(r.FormValue("target_id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	returnPath, err := s.validateBackupTarget(r.Context(), kind, targetID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	destID, ok := parseID(r.FormValue("destination_id"))
	if !ok {
		http.Error(w, "Pick a destination.", http.StatusBadRequest)
		return
	}
	if _, err := s.store.GetDestination(r.Context(), destID); err != nil {
		http.Error(w, "Unknown destination.", http.StatusBadRequest)
		return
	}
	schedule := strings.TrimSpace(r.FormValue("schedule"))
	if _, err := cron.Parse(schedule); err != nil {
		http.Error(w, "Invalid schedule: "+err.Error(), http.StatusBadRequest)
		return
	}
	prefix := strings.Trim(strings.TrimSpace(r.FormValue("prefix")), "/")
	if !prefixRe.MatchString(prefix) {
		http.Error(w, "Prefix may contain letters, digits, and . _ - / only.", http.StatusBadRequest)
		return
	}
	retention := 0
	if v := strings.TrimSpace(r.FormValue("retention")); v != "" {
		retention, err = strconv.Atoi(v)
		if err != nil || retention < 0 {
			http.Error(w, "Retention must be a non-negative number (0 keeps everything).", http.StatusBadRequest)
			return
		}
	}

	if _, err := s.store.CreateBackup(r.Context(), core.Backup{
		TargetKind: kind, TargetID: targetID, DestinationID: destID,
		Schedule: schedule, Prefix: prefix, Retention: retention,
	}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, returnPath, http.StatusSeeOther)
}

// validateBackupTarget checks the target exists and is backupable, returning
// the page to bounce back to.
func (s *Server) validateBackupTarget(ctx context.Context, kind string, targetID int64) (string, error) {
	switch kind {
	case core.BackupTargetDatabase:
		db, err := s.store.GetDatabase(ctx, targetID)
		if err != nil {
			return "", errors.New("unknown database")
		}
		if db.Engine == core.EngineRedis {
			return "", errors.New("redis databases cannot be backed up (no dump tooling; it is cache-shaped)")
		}
		return databasePath(db.ID), nil
	case core.BackupTargetApp:
		app, err := s.store.GetApp(ctx, targetID)
		if err != nil {
			return "", errors.New("unknown app")
		}
		if app.Kind != core.KindCompose {
			vols, err := s.store.ListVolumes(ctx, app.ID)
			if err != nil {
				return "", err
			}
			if len(vols) == 0 {
				return "", errors.New("this app has no volumes to back up; attach one first (or back up the database it uses)")
			}
		}
		return "/apps/" + strconv.FormatInt(app.ID, 10), nil
	default:
		return "", errors.New("unknown backup target kind")
	}
}

// backupFromPath loads the backup named by {id}, plus its bounce-back page.
func (s *Server) backupFromPath(w http.ResponseWriter, r *http.Request) (core.Backup, string, bool) {
	id, ok := parseID(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return core.Backup{}, "", false
	}
	b, err := s.store.GetBackup(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return core.Backup{}, "", false
	}
	returnPath := "/"
	switch b.TargetKind {
	case core.BackupTargetDatabase:
		returnPath = databasePath(b.TargetID)
	case core.BackupTargetApp:
		returnPath = "/apps/" + strconv.FormatInt(b.TargetID, 10)
	}
	return b, returnPath, true
}

func (s *Server) handleRunBackup(w http.ResponseWriter, r *http.Request) {
	b, returnPath, ok := s.backupFromPath(w, r)
	if !ok {
		return
	}
	s.backups.RunNow(b)
	http.Redirect(w, r, returnPath, http.StatusSeeOther)
}

// handleRestorePage lists the archives a schedule can restore from — the
// objects under its own bucket directory, newest first.
func (s *Server) handleRestorePage(w http.ResponseWriter, r *http.Request) {
	b, returnPath, ok := s.backupFromPath(w, r)
	if !ok {
		return
	}
	dir, err := s.backups.RestoreDir(r.Context(), b)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	objs, err := s.backups.ListRestoreObjects(r.Context(), b)
	if err != nil {
		http.Error(w, "listing archives failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	dest, err := s.store.GetDestination(r.Context(), b.DestinationID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	confirm := "Restore this archive? The database's CURRENT DATA IS REPLACED with the archive's contents. Consider Run now (a fresh backup) first."
	active := "projects"
	if b.TargetKind == core.BackupTargetApp {
		confirm = "Restore this archive? The app is stopped, the volume EMPTIED and refilled from the archive, then restarted. Consider Run now (a fresh backup) first."
		active = "apps"
	}
	s.render(w, http.StatusOK, "restore", map[string]any{
		"Title": "Restore", "Active": active,
		"Backup":      b,
		"Destination": dest.Name,
		"Dir":         dir,
		"Objects":     objs,
		"ReturnPath":  returnPath,
		"Confirm":     confirm,
	})
}

// handleRestoreBackup starts an asynchronous restore of one archive. The key
// must sit under the schedule's own directory — the restore page only ever
// posts those; anything else is a crafted request.
func (s *Server) handleRestoreBackup(w http.ResponseWriter, r *http.Request) {
	b, returnPath, ok := s.backupFromPath(w, r)
	if !ok {
		return
	}
	dir, err := s.backups.RestoreDir(r.Context(), b)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	key := strings.TrimSpace(r.FormValue("key"))
	if key == "" || !strings.HasPrefix(key, dir+"/") || key == dir+"/" {
		http.Error(w, "archive key is not under this backup's directory", http.StatusBadRequest)
		return
	}
	s.backups.RestoreNow(b, key)
	http.Redirect(w, r, returnPath, http.StatusSeeOther)
}

func (s *Server) handleToggleBackup(w http.ResponseWriter, r *http.Request) {
	b, returnPath, ok := s.backupFromPath(w, r)
	if !ok {
		return
	}
	if err := s.store.SetBackupEnabled(r.Context(), b.ID, !b.Enabled); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, returnPath, http.StatusSeeOther)
}

func (s *Server) handleDeleteBackup(w http.ResponseWriter, r *http.Request) {
	b, returnPath, ok := s.backupFromPath(w, r)
	if !ok {
		return
	}
	if err := s.store.DeleteBackup(r.Context(), b.ID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, returnPath, http.StatusSeeOther)
}

// --- destinations (settings page) ---

func (s *Server) handleCreateDestination(w http.ResponseWriter, r *http.Request) {
	d := core.Destination{
		Name:      strings.TrimSpace(r.FormValue("name")),
		Endpoint:  strings.TrimSpace(r.FormValue("endpoint")),
		Region:    strings.TrimSpace(r.FormValue("region")),
		Bucket:    strings.TrimSpace(r.FormValue("bucket")),
		AccessKey: strings.TrimSpace(r.FormValue("access_key")),
		SecretKey: r.FormValue("secret_key"),
	}
	if !appNameRe.MatchString(d.Name) {
		s.renderSettings(w, r, http.StatusBadRequest, "Destination name must be lowercase letters, digits and hyphens (2–40 chars).")
		return
	}
	if u, err := url.Parse(d.Endpoint); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		s.renderSettings(w, r, http.StatusBadRequest, "Endpoint must be an http(s) URL, e.g. https://s3.eu-west-2.amazonaws.com")
		return
	}
	if d.Bucket == "" || d.AccessKey == "" || d.SecretKey == "" {
		s.renderSettings(w, r, http.StatusBadRequest, "Bucket, access key, and secret key are required.")
		return
	}
	if _, err := s.store.CreateDestination(r.Context(), d); err != nil {
		// Most likely a duplicate name (UNIQUE constraint).
		s.renderSettings(w, r, http.StatusBadRequest, "Could not add destination: "+err.Error())
		return
	}
	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}

func (s *Server) handleTestDestination(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	d, err := s.store.GetDestination(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), destinationTestTimeout)
	defer cancel()
	if err := s.backups.TestDestination(ctx, d); err != nil {
		s.renderSettings(w, r, http.StatusOK, "Destination "+d.Name+" failed: "+err.Error())
		return
	}
	http.Redirect(w, r, "/settings?tested="+url.QueryEscape(d.Name), http.StatusSeeOther)
}

func (s *Server) handleDeleteDestination(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err := s.store.DeleteDestination(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrDestinationInUse) {
			s.renderSettings(w, r, http.StatusConflict, "This destination still has backup schedules pointing at it. Remove those first.")
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}
