package deploy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Rails force_ssl, Django SECURE_SSL_REDIRECT and friends answer plain HTTP
// with a redirect to https://<container-ip>/…, which a TLS-less container can
// never serve. The redirect itself proves the app is listening; following it
// turned every such deploy into a health-check timeout.
func TestHTTPPollTreatsRedirectAsHealthy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://127.0.0.1:1/", http.StatusMovedPermanently)
	}))
	defer srv.Close()

	start := time.Now()
	if !httpPoll(context.Background(), srv.URL+"/", 5*time.Second) {
		t.Fatal("redirecting app reported unhealthy")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("health check took %v; a redirect should count on the first probe", d)
	}
}
