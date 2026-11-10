// Package sso signs people in through an OIDC provider: discovery, the
// authorization code flow with PKCE, and ID token verification. The rest of
// the server talks to the Provider interface, which Fake implements for tests.
package sso

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// httpTimeout bounds every HTTP call made to the provider: discovery, the
// token exchange and the key-set fetch during verification.
const httpTimeout = 10 * time.Second

// ErrNoEmail means the ID token carried no usable email or preferred_username.
var ErrNoEmail = errors.New("sso: the provider sent no usable email or username")

// Config is what a system admin enters on the Settings page.
type Config struct {
	Issuer, ClientID, ClientSecret, RedirectURL string
}

// Identity is who the provider says signed in. Issuer and Subject identify the
// account; Email is for display and for protected apps.
type Identity struct {
	Issuer, Subject, Email string
}

// Provider starts and finishes a sign-in.
type Provider interface {
	AuthURL(state, nonce, verifier string) string
	Exchange(ctx context.Context, code, nonce, verifier string) (Identity, error)
}

type provider struct {
	oauth    oauth2.Config
	verifier *oidc.IDTokenVerifier
	client   *http.Client
}

// Discover fetches the provider's configuration and returns a Provider.
func Discover(ctx context.Context, cfg Config) (Provider, error) {
	// The client, not only a context deadline, carries the bound: go-oidc's
	// remote key set keeps using the context it was created with.
	client := &http.Client{Timeout: httpTimeout}
	ctx = oidc.ClientContext(ctx, client)
	ctx, cancel := context.WithTimeout(ctx, httpTimeout)
	defer cancel()
	op, err := oidc.NewProvider(ctx, cfg.Issuer)
	if err != nil {
		return nil, fmt.Errorf("sso: discovery failed: %w", err)
	}
	return &provider{
		oauth: oauth2.Config{
			ClientID:     cfg.ClientID,
			ClientSecret: cfg.ClientSecret,
			RedirectURL:  cfg.RedirectURL,
			Endpoint:     op.Endpoint(),
			Scopes:       []string{oidc.ScopeOpenID, "email", "profile"},
		},
		verifier: op.VerifierContext(oidc.ClientContext(context.Background(), client),
			&oidc.Config{ClientID: cfg.ClientID}),
		client: client,
	}, nil
}

func (p *provider) AuthURL(state, nonce, verifier string) string {
	return p.oauth.AuthCodeURL(state,
		oidc.Nonce(nonce),
		oauth2.S256ChallengeOption(verifier))
}

func (p *provider) Exchange(ctx context.Context, code, nonce, verifier string) (Identity, error) {
	ctx = oidc.ClientContext(ctx, p.client)
	ctx, cancel := context.WithTimeout(ctx, httpTimeout)
	defer cancel()
	tok, err := p.oauth.Exchange(ctx, code, oauth2.VerifierOption(verifier))
	if err != nil {
		return Identity{}, fmt.Errorf("sso: code exchange failed: %w", err)
	}
	raw, _ := tok.Extra("id_token").(string)
	if raw == "" {
		return Identity{}, errors.New("sso: the token response had no id_token")
	}
	idt, err := p.verifier.Verify(ctx, raw)
	if err != nil {
		return Identity{}, fmt.Errorf("sso: id_token not accepted: %w", err)
	}
	if nonce == "" || idt.Nonce != nonce {
		return Identity{}, errors.New("sso: id_token nonce does not match")
	}
	if idt.Subject == "" {
		return Identity{}, errors.New("sso: id_token has no subject")
	}
	var claims struct {
		Email             string `json:"email"`
		EmailVerified     any    `json:"email_verified"`
		PreferredUsername string `json:"preferred_username"`
	}
	if err := idt.Claims(&claims); err != nil {
		return Identity{}, fmt.Errorf("sso: reading id_token claims: %w", err)
	}
	email := strings.TrimSpace(claims.Email)
	if explicitlyFalse(claims.EmailVerified) {
		email = ""
	}
	if email == "" {
		email = strings.TrimSpace(claims.PreferredUsername)
	}
	if email == "" {
		return Identity{}, ErrNoEmail
	}
	return Identity{Issuer: idt.Issuer, Subject: idt.Subject, Email: email}, nil
}

// explicitlyFalse reports whether email_verified is false, sent either as a
// JSON boolean or as a string. Absent or anything else counts as usable.
func explicitlyFalse(v any) bool {
	switch x := v.(type) {
	case bool:
		return !x
	case string:
		return strings.EqualFold(strings.TrimSpace(x), "false")
	}
	return false
}
