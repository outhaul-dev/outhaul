package builder

import (
	"bytes"
	"context"
	"reflect"
	"strings"
	"testing"
)

func TestNixpacksName(t *testing.T) {
	if got := (&Nixpacks{}).Name(); got != "nixpacks" {
		t.Errorf("Name() = %q, want nixpacks", got)
	}
}

func TestBuildArgs(t *testing.T) {
	tests := []struct {
		name string
		req  BuildRequest
		want []string
	}{
		{
			name: "no env",
			req:  BuildRequest{ContextDir: "/work/repo", ImageTag: "slipway/web:5"},
			want: []string{"build", "/work/repo", "--name", "slipway/web:5"},
		},
		{
			name: "env sorted for determinism",
			req: BuildRequest{
				ContextDir: "/work/repo",
				ImageTag:   "img:1",
				Env:        map[string]string{"PORT": "8080", "APP_ENV": "prod"},
			},
			want: []string{"build", "/work/repo", "--name", "img:1",
				"--env", "APP_ENV=prod", "--env", "PORT=8080"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := buildArgs(tt.req); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("buildArgs() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestBuildFailsClearlyWhenBinaryMissing(t *testing.T) {
	n := &Nixpacks{Bin: "nixpacks-does-not-exist-9f3c"}
	var out bytes.Buffer
	err := n.Build(context.Background(), BuildRequest{ContextDir: ".", ImageTag: "x:1"}, &out)
	if err == nil {
		t.Fatal("expected an error when the nixpacks binary is missing")
	}
	if !strings.Contains(err.Error(), "nixpacks") {
		t.Errorf("error should mention nixpacks, got: %v", err)
	}
}
