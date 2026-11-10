package sso

import (
	"context"
	"fmt"
	"net/url"
	"sync"
)

// Fake is a Provider for tests. AuthURL records what it was given; Exchange
// records its arguments and returns ExchangeErr if set, else Identity. If
// LastNonce is set, Exchange also fails when given a different nonce. The
// methods are safe to call concurrently; read the fields after calls finish.
type Fake struct {
	Identity    Identity
	ExchangeErr error

	LastAuthState string
	LastNonce     string
	LastVerifier  string

	LastExchangeCode     string
	LastExchangeNonce    string
	LastExchangeVerifier string

	mu sync.Mutex
}

func (f *Fake) AuthURL(state, nonce, verifier string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.LastAuthState, f.LastNonce, f.LastVerifier = state, nonce, verifier
	return "https://sso.fake.invalid/authorize?state=" + url.QueryEscape(state)
}

func (f *Fake) Exchange(ctx context.Context, code, nonce, verifier string) (Identity, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.LastExchangeCode, f.LastExchangeNonce, f.LastExchangeVerifier = code, nonce, verifier
	if f.ExchangeErr != nil {
		return Identity{}, f.ExchangeErr
	}
	if f.LastNonce != "" && nonce != f.LastNonce {
		return Identity{}, fmt.Errorf("sso fake: nonce %q does not match the one given to AuthURL", nonce)
	}
	return f.Identity, nil
}
