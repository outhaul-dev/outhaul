// Package builder turns a cloned source tree into a runnable Docker image.
// Build strategies sit behind the Builder interface; Nixpacks is the M1
// implementation, with Dockerfile/buildpack strategies as later seams.
package builder

import (
	"context"
	"io"
)

// BuildRequest describes one build.
type BuildRequest struct {
	ContextDir string            // path to the checked-out source
	ImageTag   string            // tag to give the built image, e.g. "slipway/web:5"
	Env        map[string]string // build-time environment (empty in M1)
}

// Builder produces a Docker image from source, streaming build output to a
// writer.
type Builder interface {
	// Build builds ContextDir into an image tagged ImageTag, writing build logs
	// to out (line-buffered by the caller). It returns when the image exists or
	// the build fails.
	Build(ctx context.Context, req BuildRequest, out io.Writer) error

	// Name identifies the strategy (e.g. "nixpacks").
	Name() string
}
