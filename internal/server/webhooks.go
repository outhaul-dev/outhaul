package server

import (
	"context"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/outhaul-dev/outhaul/internal/core"
	"github.com/outhaul-dev/outhaul/internal/tagdeploy"
	"github.com/outhaul-dev/outhaul/internal/webhook"
)

// maxWebhookBody caps how much of a webhook body we read.
const maxWebhookBody = 1 << 20 // 1 MiB

// handleGithubWebhook verifies a GitHub App delivery and deploys the matching
// apps. Every connected App posts here, so the delivery is first matched to the
// source that signed it: GitHub names the App in
// X-GitHub-Hook-Installation-Target-ID, and only that source's secret is
// checked. Fan-out is then scoped to the same source, so a push for one
// connected account can never deploy another account's app.
func (s *Server) handleGithubWebhook(w http.ResponseWriter, r *http.Request) {
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	if got := r.Header.Get("X-GitHub-Hook-Installation-Target-Type"); got != "integration" {
		log.Printf("github webhook: unexpected hook target type %q", got)
		http.Error(w, "unexpected hook target", http.StatusUnauthorized)
		return
	}
	appID, err := strconv.ParseInt(r.Header.Get("X-GitHub-Hook-Installation-Target-ID"), 10, 64)
	if err != nil {
		log.Printf("github webhook: unparseable hook installation target id %q: %v", r.Header.Get("X-GitHub-Hook-Installation-Target-ID"), err)
		http.Error(w, "unidentified hook", http.StatusUnauthorized)
		return
	}
	src, found, err := s.store.GitSourceByGithubAppID(r.Context(), appID)
	if err != nil {
		log.Printf("github webhook: look up app %d: %v", appID, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !found {
		log.Printf("github webhook: unknown app id %d", appID)
		http.Error(w, "unknown app", http.StatusUnauthorized)
		return
	}
	provider, err := s.sources.For(src.Kind)
	if err != nil {
		log.Printf("github webhook: provider unavailable for app %d: %v", appID, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !provider.VerifyWebhook(src, r.Header, body) {
		log.Printf("github webhook: signature mismatch for app %d", appID)
		http.Error(w, "bad signature", http.StatusUnauthorized)
		return
	}
	switch r.Header.Get("X-GitHub-Event") {
	case "push":
		ev, err := webhook.ParsePush(body)
		if err != nil {
			http.Error(w, "bad payload", http.StatusBadRequest)
			return
		}
		apps, err := s.store.AppsByGithubRepoSource(r.Context(), src.ID, ev.RepoFullName)
		if err != nil {
			log.Printf("github webhook: look up apps for %s/%s: %v", src.Display(), ev.RepoFullName, err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		var outcomes []string
		for _, app := range apps {
			outcomes = append(outcomes, s.maybeDeploy(r.Context(), app, ev))
		}
		writeOutcomes(w, outcomes)
		return
	case "pull_request":
		if s.previews == nil {
			w.WriteHeader(http.StatusOK)
			return
		}
		ev, err := webhook.ParsePullRequest(body)
		if err != nil {
			http.Error(w, "bad payload", http.StatusBadRequest)
			return
		}
		// Acknowledge now and let the preview manager work at its own pace: a
		// spawn can outlast GitHub's 10-second delivery timeout. A refused
		// event fails the delivery, so it shows as failed on GitHub and can be
		// redelivered by hand rather than being silently lost.
		if err := s.previews.Enqueue(src.ID, ev); err != nil {
			log.Printf("webhook: preview event for %s#%d: %v", ev.BaseRepoFullName, ev.Number, err)
			http.Error(w, "preview events unavailable, retry later", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		return
	default:
		w.WriteHeader(http.StatusOK) // ignore other events
		return
	}
}

// handleAppWebhook verifies a per-app generic webhook and deploys if the branch
// matches. The URL token picks the app; the delivery must carry a GitHub/Gitea
// HMAC signature or a GitLab token made with the app's separate webhook secret.
func (s *Server) handleAppWebhook(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	app, err := s.store.AppByWebhookToken(r.Context(), token)
	if err != nil {
		http.Error(w, "unknown webhook", http.StatusNotFound)
		return
	}
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	sigOK := webhook.VerifyGitHub(app.WebhookSecret, r.Header.Get("X-Hub-Signature-256"), body) ||
		webhook.VerifyGitLabToken(app.WebhookSecret, r.Header.Get("X-Gitlab-Token"))
	if !sigOK {
		http.Error(w, "bad signature", http.StatusUnauthorized)
		return
	}
	ev, err := webhook.ParsePush(body)
	if err != nil {
		http.Error(w, "bad payload", http.StatusBadRequest)
		return
	}
	writeOutcomes(w, []string{s.maybeDeploy(r.Context(), app, ev)})
}

// writeOutcomes answers a push delivery with 200 and, when any app had
// something to say, one line per app. A skipped tag is never an error status:
// senders would only retry it.
func writeOutcomes(w http.ResponseWriter, outcomes []string) {
	var lines []string
	for _, o := range outcomes {
		if o != "" {
			lines = append(lines, o)
		}
	}
	if len(lines) == 0 {
		w.WriteHeader(http.StatusOK)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, strings.Join(lines, "\n")+"\n")
}

// maybeDeploy enqueues a deployment when every auto-deploy gate passes, in
// order: the app's toggle, then the app's trigger.
//
// A branch-mode app needs an exact branch match and, when watch paths are
// configured, a changed file matching one of them. A payload carrying no
// file info at all (branch creation, thin webhook) fails open and deploys,
// since "we don't know what changed" must never silently drop a release.
//
// A tag-mode app ignores branch pushes and deleted tags, and lets the tag
// policy decide whether the pushed tag is newer than what is live or on its
// way; watch paths do not apply. The returned line says what happened for a
// tag push, for the response body; it is "" when there is nothing to say.
// Errors are logged, not surfaced (webhook senders retry noisily).
func (s *Server) maybeDeploy(ctx context.Context, app core.App, ev webhook.PushEvent) string {
	if !app.AutoDeploy {
		return ""
	}
	if app.DeployTrigger == core.TriggerTag {
		return s.maybeDeployTag(ctx, app, ev)
	}
	if ev.Branch == "" || ev.Branch != app.Branch {
		return ""
	}
	if len(app.WatchPaths) > 0 && len(ev.Changed) > 0 && !webhook.MatchAny(app.WatchPaths, ev.Changed) {
		log.Printf("webhook: app %d push to %s skipped: no changed file matches its watch paths", app.ID, ev.Branch)
		return ""
	}
	if _, err := s.store.CreateDeployment(ctx, app.ID); err != nil {
		log.Printf("webhook: could not enqueue deploy for app %d: %v", app.ID, err)
		return ""
	}
	s.deployer.Notify()
	return ""
}

func (s *Server) maybeDeployTag(ctx context.Context, app core.App, ev webhook.PushEvent) string {
	if ev.Tag == "" || ev.Deleted {
		return ""
	}
	var decision tagdeploy.Decision
	_, ok, err := s.store.EnqueueTagDeployment(ctx, app.ID, func(inPlay []string) (string, bool) {
		decision = tagdeploy.PolicyFor(app).Decide([]string{ev.Tag}, inPlay)
		return decision.Tag, decision.Tag != ""
	})
	if err != nil {
		log.Printf("webhook: could not enqueue deploy for app %d: %v", app.ID, err)
		return ""
	}
	if !ok {
		log.Printf("webhook: app %d tag %s not deployed: %s", app.ID, ev.Tag, decision.Reason)
		return app.Name + ": not deployed: " + decision.Reason
	}
	s.deployer.Notify()
	log.Printf("webhook: app %d deploying tag %s", app.ID, decision.Tag)
	return app.Name + ": deploying " + decision.Tag
}

func readBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookBody))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return nil, false
	}
	return body, true
}
