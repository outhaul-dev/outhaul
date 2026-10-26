package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/outhaul-dev/outhaul/internal/update"
)

// What the Updates panel and its handlers say.
const (
	updateInstallerCopy    = "This update includes installer changes. Updating here does not apply them; re-run the installer to get them."
	updateTraefikCopy      = "This update restarts Traefik: every app is unreachable for a few seconds."
	updateConfirmCopy      = "Outhaul restarts, and the admin UI is gone for a minute or so. Apps keep running. GitHub pushes that arrive during the restart are not deployed; redeploy those apps by hand."
	updateNotSetUpCopy     = "Updating from the UI is not set up on this server. Re-run the installer once to set it up."
	updateUnknownBuildCopy = "This build does not record its commit, so it cannot update from the UI."
	updateAheadCopy        = "This server runs a commit main does not have. Updating replaces it with main."
	updateBusyCopy         = "Wait for the running deploy, backup or restore to finish."
	updateHeldCopy         = "An update is in progress. Try again when it finishes."
	updateRunningCopy      = "An update is already running."
	updateInterruptedCopy  = "The updater stopped without reporting a result. On the server, see " + update.LogPath + " and journalctl -u outhaul-update."
	updateStuckCopy        = "Outhaul has not come back. On the server: sudo cat " + update.LogPath
)

// SetBuild records the running binary's build stamp, which /healthz and the
// Updates panel report.
func (s *Server) SetBuild(b update.Build) { s.build = b }

// SetUpdates wires the Updates panel: the directory shared with the root
// updater, and the check of the running build against main. Without it the
// panel, its routes and the navigation badge are absent.
func (s *Server) SetUpdates(d *update.Dir, k *update.Checker) { s.updates, s.checker = d, k }

func (s *Server) updatesWired() bool { return s.updates != nil && s.checker != nil }

// updateInProgress reports an update requested or running. Manual backups and
// restores are refused meanwhile: the updater is about to stop Outhaul.
func (s *Server) updateInProgress() bool { return s.updatesWired() && s.updates.InProgress() }

// updateAvailable drives the navigation badge.
func (s *Server) updateAvailable() bool { return s.updatesWired() && s.checker.UpdateAvailable() }

// updatesPanel is what the Settings page's Updates panel shows.
type updatesPanel struct {
	Build      update.Build
	CheckedAt  time.Time
	Summary    string         // the check's result, in a sentence
	Commits    []updateCommit // main's commits this build lacks, newest first
	MoreURL    string         // the whole comparison, when Commits stops short of it
	Behind     int
	Warnings   []string
	Last       *updateResult // the last update's outcome; nil if none, or one is in progress
	InProgress bool          // show the updater's log instead of the Update button
	Blocked    string        // why Update cannot run now; "" if it can
	Confirm    string
	Stuck      string // what the log says once Outhaul has not answered for three minutes
}

type updateCommit struct {
	Short, Subject string
	Date           time.Time
}

// updateResult is the last update's outcome, from the status file.
type updateResult struct {
	Label, Class, Message string
	At                    time.Time
}

func (s *Server) updatesPanel(ctx context.Context) *updatesPanel {
	p := &updatesPanel{Build: s.build, Confirm: updateConfirmCopy, Stuck: updateStuckCopy, InProgress: s.updates.InProgress()}
	blocked, err := s.updateBlocked(ctx)
	if err != nil {
		// The POST checks again, and refuses with the error.
		log.Printf("updates panel: %v", err)
	}
	p.Blocked = blocked

	var installer, traefik bool
	if s.build.Known() {
		c := s.checker.Get(ctx, false)
		p.CheckedAt = c.At
		cmp := c.Comparison
		switch {
		case c.Err != nil:
			p.Summary = "Could not check for updates: " + strings.TrimRight(c.Err.Error(), ".") + ". Update still builds the newest main."
		case cmp.Status == update.CompareUpToDate:
			p.Summary = "Up to date with main."
		case cmp.Status == update.CompareBehind:
			p.Summary = fmt.Sprintf("%d %s behind main.", cmp.Behind, plural(cmp.Behind, "commit", "commits"))
			p.Behind = cmp.Behind
			for _, cm := range cmp.Commits {
				p.Commits = append(p.Commits, updateCommit{Short: update.Build{Commit: cm.SHA}.Short(), Subject: cm.Subject, Date: cm.Date})
			}
			if cmp.Behind > len(cmp.Commits) {
				p.MoreURL = cmp.URL
			}
			installer, traefik = cmp.InstallerChanged, cmp.TraefikChanged
		default:
			p.Summary = updateAheadCopy
		}
	}

	if !p.InProgress {
		st, ok, err := s.updates.Status()
		if err != nil {
			log.Printf("updates panel: read status: %v", err)
		}
		if ok {
			p.Last = lastUpdate(st, time.Now())
			// A version that needs an installer change may fail its health
			// check until the installer has run.
			installer = installer || st.State == update.StateRolledBack
		}
	}
	if installer {
		p.Warnings = append(p.Warnings, updateInstallerCopy)
	}
	if traefik {
		p.Warnings = append(p.Warnings, updateTraefikCopy)
	}
	return p
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// lastUpdate describes a finished (or abandoned) update's status.
func lastUpdate(st update.Status, now time.Time) *updateResult {
	r := &updateResult{Message: st.Message, At: st.Finished, Class: "state-failed"}
	if r.At.IsZero() {
		r.At = st.Started
	}
	switch st.State {
	case update.StateUpdated:
		r.Label, r.Class = "updated", "state-running"
	case update.StateUpToDate:
		r.Label, r.Class = "already up to date", "state-running"
	case update.StateBuildFailed:
		r.Label = "build failed"
	case update.StateRolledBack:
		r.Label = "rolled back"
	case update.StateFailed:
		r.Label = "failed"
	case update.StateNotStarted:
		r.Label = "did not start"
	case update.StateRunning:
		if !st.Interrupted(now) {
			return nil // InProgress said otherwise a moment ago; there is no result yet
		}
		r.Label = "interrupted"
		if r.Message == "" {
			r.Message = updateInterruptedCopy
		}
	default:
		r.Label, r.Class = st.State, "state-stopped"
	}
	return r
}

// updateBlocked says why Update cannot run now, or "" when it can.
func (s *Server) updateBlocked(ctx context.Context) (string, error) {
	switch {
	case !s.updates.Installed():
		return updateNotSetUpCopy, nil
	case !s.build.Known():
		return updateUnknownBuildCopy, nil
	}
	// Stopping Outhaul would abort the deploy or cancel the backup.
	deploys, err := s.store.CountActiveDeployments(ctx)
	if err != nil {
		return "", err
	}
	runs, err := s.store.CountRunningBackupRuns(ctx)
	if err != nil {
		return "", err
	}
	if deploys > 0 || runs > 0 {
		return updateBusyCopy, nil
	}
	return "", nil
}

// handleRequestUpdate asks the root updater to update Outhaul.
func (s *Server) handleRequestUpdate(w http.ResponseWriter, r *http.Request) {
	if !s.updatesWired() {
		http.NotFound(w, r)
		return
	}
	// Checked first, so a second click says why nothing happens even when
	// something else has started meanwhile.
	if s.updates.InProgress() {
		s.renderSettings(w, r, http.StatusConflict, updateRunningCopy)
		return
	}
	blocked, err := s.updateBlocked(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if blocked != "" {
		s.renderSettings(w, r, http.StatusConflict, blocked)
		return
	}
	if err := s.updates.Request(); errors.Is(err, update.ErrInProgress) {
		s.renderSettings(w, r, http.StatusConflict, updateRunningCopy)
		return
	} else if err != nil {
		s.renderSettings(w, r, http.StatusInternalServerError, "Could not request the update: "+err.Error())
		return
	}
	http.Redirect(w, r, "/settings#updates", http.StatusSeeOther)
}

// handleCheckForUpdate is Check now: it asks GitHub again, past the cache.
func (s *Server) handleCheckForUpdate(w http.ResponseWriter, r *http.Request) {
	if !s.updatesWired() {
		http.NotFound(w, r)
		return
	}
	s.checker.Get(r.Context(), true)
	http.Redirect(w, r, "/settings#updates", http.StatusSeeOther)
}

// updateProgress is one poll of the panel's log while an update runs.
type updateProgress struct {
	State   string `json:"state"`
	Message string `json:"message"`
	From    string `json:"from"`
	To      string `json:"to"`
	Log     string `json:"log"`
	Offset  int64  `json:"offset"`
	Commit  string `json:"commit"` // the commit of the Outhaul answering
}

// handleUpdateProgress reports the updater's status and its log from offset.
// The page polls it and reloads once state is no longer "running", so state
// is "running" exactly while the update is in progress. Until the updater
// claims a request, the status and log on disk are the last update's, and
// neither is sent. A run that still says running but was interrupted is
// reported as "interrupted".
func (s *Server) handleUpdateProgress(w http.ResponseWriter, r *http.Request) {
	if !s.updatesWired() {
		http.NotFound(w, r)
		return
	}
	offset, _ := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
	inProgress := s.updates.InProgress()
	st, _, err := s.updates.Status()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	p := updateProgress{Commit: s.build.Commit}
	interrupted := st.Interrupted(time.Now())
	if inProgress && (st.State != update.StateRunning || interrupted) {
		p.State = update.StateRunning
	} else {
		p.State, p.Message, p.From, p.To = st.State, st.Message, st.From, st.To
		if interrupted {
			p.State = "interrupted"
		}
		chunk, next, err := s.updates.ReadLog(offset)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		p.Log, p.Offset = string(chunk), next
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(p)
}
