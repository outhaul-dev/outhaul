package sso

import (
	"context"
	"net/url"
)

// Fake is a Provider for tests. AuthURL records what it was given; Exchange
// returns ExchangeErr if set, else Identity.
type Fake struct {
	Identity      Identity
	ExchangeErr   error
	LastAuthState string
	LastNonce     string
}

func (f *Fake) AuthURL(state, nonce, verifier string) string {
	f.LastAuthState, f.LastNonce = state, nonce
	return "https://sso.fake.invalid/authorize?state=" + url.QueryEscape(state)
}

func (f *Fake) Exchange(ctx context.Context, code, nonce, verifier string) (Identity, error) {
	if f.ExchangeErr != nil {
		return Identity{}, f.ExchangeErr
	}
	return f.Identity, nil
}
