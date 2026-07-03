package server

import (
	"context"
	"io"
	"log"
	"net/http"

	"github.com/slipwaydev/slipway/internal/core"
	"github.com/slipwaydev/slipway/internal/webhook"
)

// maxWebhookBody caps how much of a webhook body we read.
const maxWebhookBody = 1 << 20 // 1 MiB

// handleGithubWebhook verifies the GitHub App webhook and deploys all matching
// apps. Non-push events and non-matching pushes are 200 no-ops.
func (s *Server) handleGithubWebhook(w http.ResponseWriter, r *http.Request) {
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	ga, configured, err := s.store.GithubApp(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !configured || !webhook.VerifyGitHub(ga.WebhookSecret, r.Header.Get("X-Hub-Signature-256"), body) {
		http.Error(w, "bad signature", http.StatusUnauthorized)
		return
	}
	if r.Header.Get("X-GitHub-Event") != "push" {
		w.WriteHeader(http.StatusOK) // ignore non-push events
		return
	}
	ev, err := webhook.ParsePush(body)
	if err != nil {
		http.Error(w, "bad payload", http.StatusBadRequest)
		return
	}
	apps, err := s.store.AppsByGithubRepo(r.Context(), ev.RepoFullName)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	for _, app := range apps {
		s.maybeDeploy(r.Context(), app, ev.Branch)
	}
	w.WriteHeader(http.StatusOK)
}

// handleAppWebhook verifies a per-app generic webhook and deploys if the branch
// matches. Accepts a GitHub HMAC signature or a GitLab token.
func (s *Server) handleAppWebhook(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	app, err := s.store.AppByWebhookSecret(r.Context(), token)
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
	s.maybeDeploy(r.Context(), app, ev.Branch)
	w.WriteHeader(http.StatusOK)
}

// maybeDeploy enqueues a deployment when auto-deploy is on and the branch
// matches. Errors are logged, not surfaced (webhook senders retry noisily).
func (s *Server) maybeDeploy(ctx context.Context, app core.App, pushedBranch string) {
	if !app.AutoDeploy || pushedBranch == "" || pushedBranch != app.Branch {
		return
	}
	if _, err := s.store.CreateDeployment(ctx, app.ID); err != nil {
		log.Printf("webhook: could not enqueue deploy for app %d: %v", app.ID, err)
		return
	}
	s.deployer.Notify()
}

func readBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookBody))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return nil, false
	}
	return body, true
}
