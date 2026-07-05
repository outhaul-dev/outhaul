package builder

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestDockerName(t *testing.T) {
	if got := (&Docker{}).Name(); got != "dockerfile" {
		t.Errorf("Name() = %q, want dockerfile", got)
	}
}

func TestDockerBuildArgs(t *testing.T) {
	tests := []struct {
		name string
		req  BuildRequest
		want []string
	}{
		{
			name: "default Dockerfile at the repo root",
			req:  BuildRequest{ContextDir: "/work/repo", ImageTag: "outhaul/web:5"},
			want: []string{"build", "/work/repo",
				"--file", "/work/repo/Dockerfile", "--tag", "outhaul/web:5"},
		},
		{
			name: "custom path joined to the context, env sorted for determinism",
			req: BuildRequest{
				ContextDir: "/work/repo",
				ImageTag:   "img:1",
				Dockerfile: "build/Dockerfile.prod",
				Env:        map[string]string{"PORT": "8080", "APP_ENV": "prod"},
			},
			want: []string{"build", "/work/repo",
				"--file", "/work/repo/build/Dockerfile.prod", "--tag", "img:1",
				"--build-arg", "APP_ENV=prod", "--build-arg", "PORT=8080"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := dockerBuildArgs(tt.req); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("dockerBuildArgs() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDockerBuildFailsClearlyWhenDockerfileMissing(t *testing.T) {
	d := &Docker{}
	var out bytes.Buffer
	req := BuildRequest{ContextDir: t.TempDir(), ImageTag: "x:1", Dockerfile: "deploy/Dockerfile"}
	err := d.Build(context.Background(), req, &out)
	if err == nil {
		t.Fatal("expected an error when the Dockerfile is missing")
	}
	if !strings.Contains(err.Error(), "deploy/Dockerfile") || !strings.Contains(err.Error(), "deploy settings") {
		t.Errorf("error should name the path and point at deploy settings, got: %v", err)
	}
}

func TestDockerBuildFailsClearlyWhenBinaryMissing(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	d := &Docker{Bin: "docker-does-not-exist-9f3c"}
	var out bytes.Buffer
	err := d.Build(context.Background(), BuildRequest{ContextDir: dir, ImageTag: "x:1"}, &out)
	if err == nil {
		t.Fatal("expected an error when the docker binary is missing")
	}
	if !strings.Contains(err.Error(), "docker binary") {
		t.Errorf("error should mention the docker binary, got: %v", err)
	}
}
