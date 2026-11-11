package blobstore

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// testSource builds a source pointed at url with no retry delays.
func testSource(t *testing.T, url string, now func() time.Time) *azureTokenSource {
	t.Helper()
	src := NewTokenSource("").(*azureTokenSource)
	src.url = strings.TrimSuffix(url, "/") + "/metadata/identity/oauth2/token"
	src.backoff = make([]time.Duration, 5)
	if now != nil {
		src.now = now
	}
	return src
}

// tokenServer answers with an incrementing token and a fixed expiry.
func tokenServer(t *testing.T, expiry time.Time, calls *int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(calls, 1)
		if r.Method != http.MethodGet || r.URL.Path != "/metadata/identity/oauth2/token" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		q := r.URL.Query()
		if q.Get("api-version") != "2018-02-01" {
			t.Errorf("api-version = %q", q.Get("api-version"))
		}
		if q.Get("resource") != "https://storage.azure.com/" {
			t.Errorf("resource = %q", q.Get("resource"))
		}
		if r.Header.Get("Metadata") != "true" {
			t.Errorf("Metadata header = %q", r.Header.Get("Metadata"))
		}
		json.NewEncoder(w).Encode(map[string]string{
			"access_token": fmt.Sprintf("tok-%d", n),
			"expires_on":   fmt.Sprint(expiry.Unix()),
		})
	}))
}

func TestTokenSourceRequestAndCache(t *testing.T) {
	var calls int32
	base := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	srv := tokenServer(t, base.Add(time.Hour), &calls)
	defer srv.Close()

	src := testSource(t, srv.URL, func() time.Time { return base })
	tok, err := src.Token(context.Background())
	if err != nil || tok != "tok-1" {
		t.Fatalf("Token = %q, %v; want tok-1", tok, err)
	}
	tok, err = src.Token(context.Background())
	if err != nil || tok != "tok-1" {
		t.Fatalf("second Token = %q, %v; want cached tok-1", tok, err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("requests = %d, want 1", got)
	}
}

func TestTokenSourceSendsClientID(t *testing.T) {
	var calls int32
	base := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		if got := r.URL.Query().Get("client_id"); got != "8f7c1a2e-0000-4000-8000-000000000000" {
			t.Errorf("client_id = %q", got)
		}
		json.NewEncoder(w).Encode(map[string]string{
			"access_token": "tok-1",
			"expires_on":   fmt.Sprint(base.Add(time.Hour).Unix()),
		})
	}))
	defer srv.Close()

	src := testSource(t, srv.URL, func() time.Time { return base })
	src.clientID = "8f7c1a2e-0000-4000-8000-000000000000"
	if _, err := src.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Errorf("requests = %d, want 1", calls)
	}
}

func TestTokenSourceRefreshesFiveMinutesBeforeExpiry(t *testing.T) {
	var calls int32
	base := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	srv := tokenServer(t, base.Add(time.Hour), &calls)
	defer srv.Close()

	now := base
	src := testSource(t, srv.URL, func() time.Time { return now })
	if _, err := src.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	now = base.Add(54 * time.Minute) // six minutes before expiry: cached
	tok, err := src.Token(context.Background())
	if err != nil || tok != "tok-1" {
		t.Fatalf("six minutes before = %q, %v; want cached tok-1", tok, err)
	}
	now = base.Add(56 * time.Minute) // four minutes before expiry: fetch
	tok, err = src.Token(context.Background())
	if err != nil || tok != "tok-2" {
		t.Fatalf("four minutes before = %q, %v; want tok-2", tok, err)
	}
}

func TestTokenSourceRefresh(t *testing.T) {
	var calls int32
	base := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	srv := tokenServer(t, base.Add(time.Hour), &calls)
	defer srv.Close()

	src := testSource(t, srv.URL, func() time.Time { return base })
	if _, err := src.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	tok, err := src.Refresh(context.Background())
	if err != nil || tok != "tok-2" {
		t.Fatalf("Refresh = %q, %v; want tok-2", tok, err)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("requests = %d, want 2", got)
	}
}

func TestTokenSourceUsesTheOldTokenWhenARefreshFails(t *testing.T) {
	var calls int32
	base := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	var failing atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		if failing.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		json.NewEncoder(w).Encode(map[string]string{
			"access_token": "tok-1",
			"expires_on":   fmt.Sprint(base.Add(time.Hour).Unix()),
		})
	}))
	defer srv.Close()

	now := base
	src := testSource(t, srv.URL, func() time.Time { return now })
	if _, err := src.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	failing.Store(true)
	now = base.Add(56 * time.Minute) // inside the refresh window, still unexpired
	tok, err := src.Token(context.Background())
	if err != nil || tok != "tok-1" {
		t.Fatalf("Token after a failed refresh = %q, %v; want the old tok-1", tok, err)
	}
}

func TestTokenSourceNamesTheMetadataService(t *testing.T) {
	t.Run("closed port", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		url := srv.URL
		srv.Close()

		src := testSource(t, url, nil)
		start := time.Now()
		_, err := src.Token(context.Background())
		if err == nil || !strings.Contains(err.Error(), "169.254.169.254") || !strings.Contains(err.Error(), "managed identity") {
			t.Fatalf("error = %v, want the metadata address and managed identity", err)
		}
		if elapsed := time.Since(start); elapsed > 3*time.Second {
			t.Errorf("closed port took %v, want under 3s", elapsed)
		}
	})

	t.Run("400 names the client", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{
				"error":             "invalid_request",
				"error_description": "bad client",
			})
		}))
		defer srv.Close()

		_, err := testSource(t, srv.URL, nil).Token(context.Background())
		if err == nil || !strings.Contains(err.Error(), "bad client") || !strings.Contains(err.Error(), "AZURE_CLIENT_ID") {
			t.Fatalf("error = %v, want the description and AZURE_CLIENT_ID", err)
		}
	})

	for _, status := range []int{404, 410, 429, 500} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var calls int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				atomic.AddInt32(&calls, 1)
				w.WriteHeader(status)
			}))
			defer srv.Close()

			_, err := testSource(t, srv.URL, nil).Token(context.Background())
			if err == nil || !strings.Contains(err.Error(), fmt.Sprint(status)) {
				t.Fatalf("error = %v, want status %d", err, status)
			}
			if got := atomic.LoadInt32(&calls); got < 2 {
				t.Errorf("requests = %d, want a retry", got)
			}
		})
	}

	t.Run("no token", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			json.NewEncoder(w).Encode(map[string]string{"expires_on": "1"})
		}))
		defer srv.Close()

		_, err := testSource(t, srv.URL, nil).Token(context.Background())
		if err == nil || !strings.Contains(err.Error(), "unexpected answer from the metadata service") {
			t.Fatalf("error = %v, want the unexpected-answer message", err)
		}
	})
}

func TestTokenSourceNeverLeaksTheToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"access_token":"secret-token"}`)
	}))
	defer srv.Close()

	_, err := testSource(t, srv.URL, nil).Token(context.Background())
	if err == nil {
		t.Fatal("Token succeeded, want an error")
	}
	if strings.Contains(err.Error(), "secret-token") {
		t.Fatalf("error leaks the token: %v", err)
	}
}
