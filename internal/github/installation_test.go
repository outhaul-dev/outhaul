package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func timeNowForTest() time.Time { return time.Unix(1_700_000_000, 0) }

func TestHTTPClientInstallationReadsAccount(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/app/installations/9001" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer JWT" {
			t.Errorf("auth = %q, want the App JWT", got)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"id":      9001,
			"account": map[string]any{"login": "acme-corp", "type": "Organization"},
		})
	}))
	defer srv.Close()

	c := &HTTPClient{BaseURL: srv.URL, HTTP: srv.Client()}
	got, err := c.Installation(context.Background(), "JWT", 9001)
	if err != nil {
		t.Fatalf("Installation: %v", err)
	}
	if got.ID != 9001 || got.AccountLogin != "acme-corp" || got.AccountType != "Organization" {
		t.Errorf("got %+v", got)
	}
}

// The fake scopes installations to the calling App the way GitHub does, by
// reading the App id out of the JWT's iss claim.
func TestFakeInstallationScopedToCallingApp(t *testing.T) {
	pem, _ := testKeyPEM(t)
	f := &Fake{InstallationsByApp: map[int64][]Installation{
		77: {{ID: 9001, AccountLogin: "acme-corp", AccountType: "Organization"}},
	}}

	jwtOwner, err := AppJWT(pem, 77, timeNowForTest())
	if err != nil {
		t.Fatalf("AppJWT: %v", err)
	}
	got, err := f.Installation(context.Background(), jwtOwner, 9001)
	if err != nil {
		t.Fatalf("owner App got an error: %v", err)
	}
	if got.AccountLogin != "acme-corp" {
		t.Errorf("account = %q", got.AccountLogin)
	}

	jwtOther, _ := AppJWT(pem, 55, timeNowForTest())
	if _, err := f.Installation(context.Background(), jwtOther, 9001); err == nil {
		t.Error("an App that does not own the installation must get an error")
	}
}
