// Command oidcstub is a stand-in OpenID Connect provider for
// deploy/test/integration_proxy.sh: discovery, an authorization endpoint that
// redirects straight back with a code, a token endpoint, and a JWKS, modelled
// on the idp type in internal/sso/sso_test.go.
//
//	oidcstub -listen :9000 -issuer http://127.0.0.1:9000 -client-id ID -client-secret SECRET -email dana@example.com
package main

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

type provider struct {
	issuer       string
	clientID     string
	clientSecret string
	email        string
	key          *rsa.PrivateKey

	mu    sync.Mutex
	codes map[string]authRequest
}

type authRequest struct {
	nonce       string
	redirectURI string
	clientID    string
}

func randomCode() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		log.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func (p *provider) discovery(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"issuer":                                p.issuer,
		"authorization_endpoint":                p.issuer + "/auth",
		"token_endpoint":                        p.issuer + "/token",
		"jwks_uri":                              p.issuer + "/jwks",
		"id_token_signing_alg_values_supported": []string{"RS256"},
	})
}

func (p *provider) auth(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	redirect := q.Get("redirect_uri")
	if redirect == "" {
		http.Error(w, "missing redirect_uri", http.StatusBadRequest)
		return
	}
	code := randomCode()
	p.mu.Lock()
	p.codes[code] = authRequest{nonce: q.Get("nonce"), redirectURI: redirect, clientID: q.Get("client_id")}
	p.mu.Unlock()
	u, err := url.Parse(redirect)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	rq := u.Query()
	rq.Set("code", code)
	if state := q.Get("state"); state != "" {
		rq.Set("state", state)
	}
	u.RawQuery = rq.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}

func (p *provider) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	code := r.PostForm.Get("code")
	p.mu.Lock()
	req, ok := p.codes[code]
	delete(p.codes, code)
	p.mu.Unlock()
	if !ok {
		http.Error(w, "unknown code", http.StatusBadRequest)
		return
	}
	clientID, secret, hasBasic := r.BasicAuth()
	if !hasBasic {
		clientID, secret = r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
	}
	if clientID != p.clientID || secret != p.clientSecret {
		http.Error(w, "bad client", http.StatusUnauthorized)
		return
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: p.key},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "k1"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	claims := map[string]any{
		"iss":   p.issuer,
		"sub":   "dana",
		"aud":   p.clientID,
		"exp":   time.Now().Add(time.Hour).Unix(),
		"iat":   time.Now().Unix(),
		"email": p.email,
	}
	if req.nonce != "" {
		claims["nonce"] = req.nonce
	}
	token, err := jwt.Signed(signer).Claims(claims).Serialize()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"access_token": "at", "token_type": "Bearer", "id_token": token,
	})
}

func (p *provider) jwks(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{
		{Key: &p.key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"},
	}})
}

func main() {
	listen := flag.String("listen", ":9000", "address to listen on")
	issuer := flag.String("issuer", "http://127.0.0.1:9000", "the issuer URL")
	clientID := flag.String("client-id", "", "OIDC client ID")
	clientSecret := flag.String("client-secret", "", "OIDC client secret")
	email := flag.String("email", "dana@example.com", "the email every sign-in returns")
	flag.Parse()
	if *clientID == "" || *clientSecret == "" {
		fmt.Fprintln(flag.CommandLine.Output(), "oidcstub: -client-id and -client-secret are required")
		flag.Usage()
		return
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		log.Fatal(err)
	}
	p := &provider{
		issuer: *issuer, clientID: *clientID, clientSecret: *clientSecret,
		email: *email, key: key, codes: map[string]authRequest{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", p.discovery)
	mux.HandleFunc("/auth", p.auth)
	mux.HandleFunc("/token", p.token)
	mux.HandleFunc("/jwks", p.jwks)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("ok\n"))
	})
	log.Printf("oidcstub issuer %s on %s", strings.TrimRight(*issuer, "/"), *listen)
	log.Fatal(http.ListenAndServe(*listen, mux))
}
