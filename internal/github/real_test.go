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
			"repositories": []map[string]any{
				{"full_name": "a/b", "default_branch": "main"},
				{"full_name": "c/d", "default_branch": "master"},
			},
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
	// The default branch must be captured so the create-app form can pre-fill
	// it (repos on "master" otherwise fail a hardcoded "main" clone).
	if repos[0].DefaultBranch != "main" || repos[1].DefaultBranch != "master" {
		t.Errorf("default branches = %q, %q; want main, master", repos[0].DefaultBranch, repos[1].DefaultBranch)
	}
}

func TestHTTPClientUpsertPRComment(t *testing.T) {
	t.Run("create", func(t *testing.T) {
		var gotMethod, gotPath, gotCT, gotAuth, gotBody string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == http.MethodGet && r.URL.Path == "/repos/me/app/issues/42/comments":
				if got := r.Header.Get("Authorization"); got != "token tok" {
					t.Errorf("list auth = %q", got)
				}
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte("[]"))
			case r.Method == http.MethodPost && r.URL.Path == "/repos/me/app/issues/42/comments":
				gotMethod, gotPath = r.Method, r.URL.Path
				gotCT = r.Header.Get("Content-Type")
				gotAuth = r.Header.Get("Authorization")
				var payload struct {
					Body string `json:"body"`
				}
				json.NewDecoder(r.Body).Decode(&payload)
				gotBody = payload.Body
				w.WriteHeader(http.StatusCreated)
			default:
				t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			}
		}))
		defer srv.Close()

		c := New()
		c.BaseURL = srv.URL
		if err := c.UpsertPRComment(context.Background(), "tok", "me/app", 42, "hello"); err != nil {
			t.Fatalf("UpsertPRComment: %v", err)
		}
		if gotMethod != http.MethodPost || gotPath != "/repos/me/app/issues/42/comments" {
			t.Errorf("create request = %s %s, want POST /repos/me/app/issues/42/comments", gotMethod, gotPath)
		}
		if gotCT != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", gotCT)
		}
		if gotAuth != "token tok" {
			t.Errorf("Authorization = %q", gotAuth)
		}
		if !strings.Contains(gotBody, previewCommentMarker) || !strings.Contains(gotBody, "hello") {
			t.Errorf("posted body = %q, want marker and text", gotBody)
		}
	})

	t.Run("update", func(t *testing.T) {
		var gotMethod, gotPath, gotCT, gotAuth, gotBody string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == http.MethodGet && r.URL.Path == "/repos/me/app/issues/42/comments":
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode([]map[string]any{
					{"id": 111, "body": "unrelated chatter"},
					{"id": 555, "body": previewCommentMarker + "\nold"},
				})
			case r.Method == http.MethodPatch && r.URL.Path == "/repos/me/app/issues/comments/555":
				gotMethod, gotPath = r.Method, r.URL.Path
				gotCT = r.Header.Get("Content-Type")
				gotAuth = r.Header.Get("Authorization")
				var payload struct {
					Body string `json:"body"`
				}
				json.NewDecoder(r.Body).Decode(&payload)
				gotBody = payload.Body
				w.WriteHeader(http.StatusOK)
			default:
				t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			}
		}))
		defer srv.Close()

		c := New()
		c.BaseURL = srv.URL
		if err := c.UpsertPRComment(context.Background(), "tok", "me/app", 42, "fresh"); err != nil {
			t.Fatalf("UpsertPRComment: %v", err)
		}
		if gotMethod != http.MethodPatch || gotPath != "/repos/me/app/issues/comments/555" {
			t.Errorf("update request = %s %s, want PATCH /repos/me/app/issues/comments/555", gotMethod, gotPath)
		}
		if gotCT != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", gotCT)
		}
		if gotAuth != "token tok" {
			t.Errorf("Authorization = %q", gotAuth)
		}
		if !strings.Contains(gotBody, previewCommentMarker) || !strings.Contains(gotBody, "fresh") {
			t.Errorf("patched body = %q, want marker and text", gotBody)
		}
	})
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
