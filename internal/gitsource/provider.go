// Package gitsource is the seam between Outhaul and the Git hosts it deploys
// from. A core.GitSource records *which* account is connected; a Provider knows
// how to talk to it. GitHub App is the only implementation today — a second
// host is a new Provider here plus its own credential table in the store.
package gitsource

import (
	"context"
	"fmt"
	"net/http"

	"github.com/outhaul-dev/outhaul/internal/core"
)

// Repo is a repository a source can access.
type Repo struct {
	FullName      string // "owner/name"
	DefaultBranch string
}

// Provider is what one Git hosting integration must supply.
type Provider interface {
	// Kind is the core.GitSource kind this provider serves.
	Kind() string
	// Repos lists the repositories the source can access.
	Repos(ctx context.Context, src core.GitSource) ([]Repo, error)
	// Token returns a short-lived credential valid for both HTTPS clone and
	// API calls. For GitHub these are one object — an installation token.
	Token(ctx context.Context, src core.GitSource) (string, error)
	// VerifyWebhook reports whether body carries a valid signature for src.
	VerifyWebhook(src core.GitSource, h http.Header, body []byte) bool
}

// Registry resolves a source to the Provider that speaks its kind.
type Registry struct{ byKind map[string]Provider }

// NewRegistry indexes providers by kind. Later duplicates of a kind win.
func NewRegistry(providers ...Provider) *Registry {
	r := &Registry{byKind: make(map[string]Provider, len(providers))}
	for _, p := range providers {
		r.byKind[p.Kind()] = p
	}
	return r
}

// For returns the provider for a kind, or an error naming the unknown kind.
func (r *Registry) For(kind string) (Provider, error) {
	p, ok := r.byKind[kind]
	if !ok {
		return nil, fmt.Errorf("gitsource: no provider for kind %q", kind)
	}
	return p, nil
}

// TokenFor mints a credential for a source in one step. Callers that only need
// to clone or call an API use this instead of resolving the provider first.
func (r *Registry) TokenFor(ctx context.Context, src core.GitSource) (string, error) {
	p, err := r.For(src.Kind)
	if err != nil {
		return "", err
	}
	return p.Token(ctx, src)
}
