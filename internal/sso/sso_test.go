package sso

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

const (
	testClient = "outhaul-client"
	testNonce  = "nonce-1"
	testVerif  = "verifier-0123456789-0123456789-0123456789-abc"
)

// idp is a minimal OIDC provider. claims is merged over a valid default set
// each time the token endpoint is hit.
type idp struct {
	*httptest.Server
	key       *rsa.PrivateKey
	signKey   *rsa.PrivateKey // if set, tokens are signed with this instead
	claims    map[string]any
	omitToken bool
	gotForm   url.Values
}

func newIDP(t *testing.T) *idp {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	p := &idp{key: key, claims: map[string]any{}}
	mux := http.NewServeMux()
	p.Server = httptest.NewServer(mux)
	t.Cleanup(p.Close)
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                p.URL,
			"authorization_endpoint":                p.URL + "/auth",
			"token_endpoint":                        p.URL + "/token",
			"jwks_uri":                              p.URL + "/jwks",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{
			{Key: &key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"},
		}})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		p.gotForm = r.PostForm
		w.Header().Set("Content-Type", "application/json")
		resp := map[string]any{"access_token": "at", "token_type": "Bearer"}
		if !p.omitToken {
			resp["id_token"] = p.token(t)
		}
		json.NewEncoder(w).Encode(resp)
	})
	return p
}

func (p *idp) token(t *testing.T) string {
	t.Helper()
	k := p.key
	if p.signKey != nil {
		k = p.signKey
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: k},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "k1"))
	if err != nil {
		t.Fatal(err)
	}
	c := map[string]any{
		"iss":   p.URL,
		"sub":   "abc",
		"aud":   testClient,
		"exp":   time.Now().Add(time.Hour).Unix(),
		"iat":   time.Now().Unix(),
		"nonce": testNonce,
		"email": "dana@example.com",
	}
	for k, v := range p.claims {
		if v == nil {
			delete(c, k)
		} else {
			c[k] = v
		}
	}
	s, err := jwt.Signed(signer).Claims(c).Serialize()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func (p *idp) cfg() Config {
	return Config{Issuer: p.URL, ClientID: testClient, ClientSecret: "shh", RedirectURL: "https://outhaul.example.com/auth/sso/callback"}
}

func discover(t *testing.T, p *idp) Provider {
	t.Helper()
	prov, err := Discover(context.Background(), p.cfg())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	return prov
}

func exchange(t *testing.T, p *idp) (Identity, error) {
	t.Helper()
	return discover(t, p).Exchange(context.Background(), "code-1", testNonce, testVerif)
}

func TestExchangeReturnsIdentity(t *testing.T) {
	p := newIDP(t)
	got, err := exchange(t, p)
	if err != nil {
		t.Fatal(err)
	}
	want := Identity{Issuer: p.URL, Subject: "abc", Email: "dana@example.com"}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	if p.gotForm.Get("code") != "code-1" || p.gotForm.Get("code_verifier") != testVerif {
		t.Fatalf("token request missed code or verifier: %v", p.gotForm)
	}
}

func TestExchangeFallsBackToPreferredUsername(t *testing.T) {
	p := newIDP(t)
	p.claims["email"] = nil
	p.claims["preferred_username"] = "dana"
	got, err := exchange(t, p)
	if err != nil || got.Email != "dana" {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestExchangeRefusesNoIdentityClaim(t *testing.T) {
	p := newIDP(t)
	p.claims["email"] = nil
	if _, err := exchange(t, p); !errors.Is(err, ErrNoEmail) {
		t.Fatalf("err = %v, want ErrNoEmail", err)
	}
}

func TestExchangeTreatsEmptyClaimsAsMissing(t *testing.T) {
	p := newIDP(t)
	p.claims["email"] = "  "
	p.claims["preferred_username"] = ""
	if _, err := exchange(t, p); !errors.Is(err, ErrNoEmail) {
		t.Fatalf("err = %v, want ErrNoEmail", err)
	}
	p.claims["email"] = ""
	p.claims["preferred_username"] = " dana "
	got, err := exchange(t, p)
	if err != nil || got.Email != "dana" {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestExchangeIgnoresUnverifiedEmail(t *testing.T) {
	for _, unverified := range []any{false, "false", "FALSE"} {
		p := newIDP(t)
		p.claims["email_verified"] = unverified
		p.claims["preferred_username"] = "dana"
		got, err := exchange(t, p)
		if err != nil || got.Email != "dana" {
			t.Fatalf("%v with username: got %+v, %v", unverified, got, err)
		}
		delete(p.claims, "preferred_username")
		if _, err := exchange(t, p); !errors.Is(err, ErrNoEmail) {
			t.Fatalf("%v without username: err = %v, want ErrNoEmail", unverified, err)
		}
	}
	for _, usable := range []any{nil, true, "true"} {
		p := newIDP(t)
		p.claims["email_verified"] = usable
		got, err := exchange(t, p)
		if err != nil || got.Email != "dana@example.com" {
			t.Fatalf("%v: got %+v, %v", usable, got, err)
		}
	}
}

func TestExchangeRefusesWrongNonce(t *testing.T) {
	p := newIDP(t)
	p.claims["nonce"] = "other"
	if _, err := exchange(t, p); err == nil {
		t.Fatal("expected error")
	}
}

func TestExchangeRefusesMissingNonce(t *testing.T) {
	p := newIDP(t)
	p.claims["nonce"] = nil
	if _, err := exchange(t, p); err == nil {
		t.Fatal("expected error")
	}
}

func TestExchangeRefusesWrongAudience(t *testing.T) {
	p := newIDP(t)
	p.claims["aud"] = "someone-else"
	if _, err := exchange(t, p); err == nil {
		t.Fatal("expected error")
	}
}

func TestExchangeRefusesWrongIssuer(t *testing.T) {
	p := newIDP(t)
	p.claims["iss"] = "https://evil.example.com"
	if _, err := exchange(t, p); err == nil {
		t.Fatal("expected error")
	}
}

func TestExchangeRefusesExpiredToken(t *testing.T) {
	p := newIDP(t)
	p.claims["exp"] = time.Now().Add(-time.Hour).Unix()
	if _, err := exchange(t, p); err == nil {
		t.Fatal("expected error")
	}
}

func TestExchangeRefusesBadSignature(t *testing.T) {
	p := newIDP(t)
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	p.signKey = other
	if _, err := exchange(t, p); err == nil {
		t.Fatal("expected error")
	}
}

func TestExchangeRefusesEmptySubject(t *testing.T) {
	p := newIDP(t)
	p.claims["sub"] = ""
	if _, err := exchange(t, p); err == nil {
		t.Fatal("expected error")
	}
}

func TestExchangeRefusesResponseWithoutIDToken(t *testing.T) {
	p := newIDP(t)
	p.omitToken = true
	_, err := exchange(t, p)
	if err == nil || !strings.Contains(err.Error(), "id_token") {
		t.Fatalf("err = %v, want one naming id_token", err)
	}
}

func TestAuthURLCarriesPKCE(t *testing.T) {
	p := newIDP(t)
	raw := discover(t, p).AuthURL("state-1", testNonce, testVerif)
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(raw, p.URL+"/auth?") {
		t.Fatalf("url = %s", raw)
	}
	q := u.Query()
	for k, want := range map[string]string{
		"state":                 "state-1",
		"nonce":                 testNonce,
		"code_challenge_method": "S256",
		"scope":                 "openid email profile",
		"client_id":             testClient,
		"response_type":         "code",
		"redirect_uri":          p.cfg().RedirectURL,
	} {
		if q.Get(k) != want {
			t.Errorf("%s = %q, want %q", k, q.Get(k), want)
		}
	}
	if q.Get("code_challenge") == "" {
		t.Error("no code_challenge")
	}
	if strings.Contains(raw, testVerif) {
		t.Error("raw verifier leaked into the URL")
	}
}

func TestDiscoverFailsWhenUnreachable(t *testing.T) {
	p := newIDP(t)
	cfg := p.cfg()
	p.Close()
	if _, err := Discover(context.Background(), cfg); err == nil {
		t.Fatal("expected error")
	}
}

func TestFakeImplementsProvider(t *testing.T) {
	f := &Fake{Identity: Identity{Issuer: "i", Subject: "s", Email: "e"}}
	var _ Provider = f
	u := f.AuthURL("st", "no", "ve")
	if !strings.Contains(u, "st") || f.LastAuthState != "st" || f.LastNonce != "no" {
		t.Fatalf("url=%s state=%q nonce=%q", u, f.LastAuthState, f.LastNonce)
	}
	if got, err := f.Exchange(context.Background(), "c", "no", "ve"); err != nil || got != f.Identity {
		t.Fatalf("got %+v, %v", got, err)
	}
	f.ExchangeErr = errors.New("boom")
	if _, err := f.Exchange(context.Background(), "c", "no", "ve"); err == nil {
		t.Fatal("expected ExchangeErr")
	}
}
