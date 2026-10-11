package previewmgr

import (
	"context"
	"strings"
	"testing"

	"github.com/outhaul-dev/outhaul/internal/core"
)

func allowForks(c *core.PreviewConfig) { c.AllowForkPRs = true }

// A fork's commits are not on any branch of the base repo; building the fork's
// branch name there built the wrong code (or nothing). The base repo exposes
// the PR head as refs/pull/<n>/head.
func TestForkPreviewBuildsPullRequestRef(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	parent := h.seedGithubApp(t, "web", "acme/web", allowForks)

	if err := h.mgr.Handle(ctx, h.sourceID, prEvent("opened", 7, "patch-1", "acme/web", true)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	child, err := h.st.GetPreviewByPR(ctx, parent.ID, 7)
	if err != nil {
		t.Fatalf("fork preview not created: %v", err)
	}
	if child.Branch != "refs/pull/7/head" {
		t.Errorf("fork preview builds %q, want refs/pull/7/head", child.Branch)
	}
}

func TestSameRepoPreviewStillBuildsItsBranch(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	parent := h.seedGithubApp(t, "web", "acme/web", allowForks)

	if err := h.mgr.Handle(ctx, h.sourceID, prEvent("opened", 8, "feature-x", "acme/web", false)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	child, err := h.st.GetPreviewByPR(ctx, parent.ID, 8)
	if err != nil {
		t.Fatal(err)
	}
	if child.Branch != "feature-x" {
		t.Errorf("same-repo preview builds %q, want feature-x", child.Branch)
	}
}

// Fork code is untrusted. Its preview gets only the variables the operator
// marked "Preview only" (plus Outhaul's own preview vars and its own empty
// databases) — never shared or production values.
func TestForkPreviewGetsOnlyPreviewScopedEnv(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	parent := h.seedGithubApp(t, "web", "acme/web", allowForks)
	for _, v := range []struct{ key, val, scope string }{
		{"API_KEY", "shared-secret", core.ScopeShared},
		{"PROD_DB", "prod-secret", core.ScopeProd},
		{"PREVIEW_FLAG", "on", core.ScopePreview},
		{"MODE", "live", core.ScopeShared},
		{"MODE", "preview", core.ScopePreview},
	} {
		if err := h.st.SetEnvScoped(ctx, parent.ID, v.key, v.val, true, v.scope); err != nil {
			t.Fatalf("SetEnvScoped %s: %v", v.key, err)
		}
	}

	if err := h.mgr.Handle(ctx, h.sourceID, prEvent("opened", 7, "patch-1", "acme/web", true)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	child, err := h.st.GetPreviewByPR(ctx, parent.ID, 7)
	if err != nil {
		t.Fatalf("fork preview not created: %v", err)
	}
	envs, _ := h.st.ListEnv(ctx, child.ID)
	for _, v := range envs {
		if v.Key == "API_KEY" || v.Key == "PROD_DB" || v.Value == "live" {
			t.Errorf("fork preview received %s=%q, which is not Preview-only", v.Key, v.Value)
		}
	}
	for key, val := range map[string]string{"PREVIEW_FLAG": "on", "MODE": "preview", "OUTHAUL_PREVIEW": "true", "OUTHAUL_PR_NUMBER": "7"} {
		if !hasEnv(envs, key, val) {
			t.Errorf("fork preview missing %s=%s: %+v", key, val, envs)
		}
	}
}

// A compose app's runtime (host mounts, privileged mode, host networking) is
// defined by its compose file — which, in a fork PR, the fork author controls.
// Running it would hand them the host, so compose apps get no fork previews.
func TestForkPreviewRefusedForComposeApp(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	parent, err := h.st.CreateApp(ctx, core.App{
		Name: "stack", RepoURL: "https://github.com/acme/stack.git", Source: core.SourceGithub,
		GithubRepo: "acme/stack", GitSourceID: h.sourceID, Branch: "main", Kind: core.KindCompose,
		ComposePath: "docker-compose.yml",
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg := core.DefaultPreviewConfig(parent.ID)
	cfg.Enabled, cfg.AllowForkPRs = true, true
	if err := h.st.SetPreviewConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}

	if err := h.mgr.Handle(ctx, h.sourceID, prEvent("opened", 7, "patch-1", "acme/stack", true)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if _, err := h.st.GetPreviewByPR(ctx, parent.ID, 7); err == nil {
		t.Fatal("a fork preview was created for a Docker Compose app")
	}
	got := h.gh.comments["acme/stack#7"]
	if len(got) != 1 || !strings.Contains(got[0], "Docker Compose") {
		t.Errorf("PR comments = %v, want one notice explaining compose apps get no fork previews", got)
	}
}
