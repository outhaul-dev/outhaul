package store

import (
	"context"
	"testing"

	"github.com/james-smart/outhaul/internal/core"
)

func TestDockerfileAppRoundTrip(t *testing.T) {
	st := gitTestStore(t)
	ctx := context.Background()

	app, err := st.CreateApp(ctx, core.App{
		Name: "api", RepoURL: "https://example.com/r.git", Domain: "api.example.com",
		Kind: core.KindDockerfile, DockerfilePath: "deploy/Dockerfile.prod",
	})
	if err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	got, err := st.GetApp(ctx, app.ID)
	if err != nil {
		t.Fatalf("GetApp: %v", err)
	}
	if got.Kind != core.KindDockerfile || got.DockerfilePath != "deploy/Dockerfile.prod" {
		t.Errorf("round trip = kind %q path %q, want dockerfile deploy/Dockerfile.prod", got.Kind, got.DockerfilePath)
	}

	if err := st.UpdateAppDockerfilePath(ctx, app.ID, "Dockerfile"); err != nil {
		t.Fatalf("UpdateAppDockerfilePath: %v", err)
	}
	got, _ = st.GetApp(ctx, app.ID)
	if got.DockerfilePath != "Dockerfile" {
		t.Errorf("DockerfilePath after update = %q, want Dockerfile", got.DockerfilePath)
	}
}
