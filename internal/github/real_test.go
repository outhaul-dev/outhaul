package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHTTPClientExchangeManifest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/app-manifests/thecode/conversions" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("auth = %q, want empty (unauthenticated)", got)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id": 42, "slug": "outhaul-x", "pem": "PEMDATA",
			"webhook_secret": "whs", "client_id": "cid", "client_secret": "csec",
		})
	}))
	defer srv.Close()

	c := New()
	c.BaseURL = srv.URL
	res, err := c.ExchangeManifest(context.Background(), "thecode")
	if err != nil {
		t.Fatalf("ExchangeManifest: %v", err)
	}
	if res.AppID != 42 || res.Slug != "outhaul-x" || res.PEM != "PEMDATA" ||
		res.WebhookSecret != "whs" || res.ClientID != "cid" || res.ClientSecret != "csec" {
		t.Errorf("bad result: %+v", res)
	}
}

func TestHTTPClientInstallationToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s", r.Method)
		}
		if r.URL.Path != "/app/installations/99/access_tokens" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer thejwt" {
			t.Errorf("auth = %q", got)
		}
		json.NewEncoder(w).Encode(map[string]any{"token": "ghs_tok"})
	}))
	defer srv.Close()

	c := New()
	c.BaseURL = srv.URL
	tok, err := c.InstallationToken(context.Background(), "thejwt", 99)
	if err != nil {
		t.Fatalf("InstallationToken: %v", err)
	}
	if tok != "ghs_tok" {
		t.Errorf("token = %q", tok)
	}
}

func TestHTTPClientListRepos(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s", r.Method)
		}
		if r.URL.Path != "/installation/repositories" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "token tok" {
			t.Errorf("auth = %q", got)
		}
		if got := r.URL.Query().Get("per_page"); got != "100" {
			t.Errorf("per_page = %q, want 100", got)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"repositories": []map[string]any{{"full_name": "a/b"}, {"full_name": "c/d"}},
		})
	}))
	defer srv.Close()

	c := New()
	c.BaseURL = srv.URL
	repos, err := c.ListRepos(context.Background(), "tok")
	if err != nil {
		t.Fatalf("ListRepos: %v", err)
	}
	if len(repos) != 2 || repos[0].FullName != "a/b" || repos[1].FullName != "c/d" {
		t.Errorf("repos = %+v", repos)
	}
}

func TestHTTPClientErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("boom"))
	}))
	defer srv.Close()

	c := New()
	c.BaseURL = srv.URL
	_, err := c.ListRepos(context.Background(), "tok")
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
	if !strings.Contains(err.Error(), "500") || !strings.Contains(err.Error(), "boom") {
		t.Errorf("error = %q, want it to contain 500 and boom", err)
	}
}
