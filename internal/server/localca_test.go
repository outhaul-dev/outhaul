package server

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/outhaul-dev/outhaul/internal/core"
)

func TestCARootDownload(t *testing.T) {
	e := newTestEnv(t)
	caFile := filepath.Join(t.TempDir(), "rootCA.pem")
	if err := os.WriteFile(caFile, []byte("-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	e.srv.SetLocalCAFile(caFile)

	resp := e.get(t, "/ca.pem")
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/x-pem-file" {
		t.Errorf("Content-Type = %q", ct)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) == "" || string(body)[:10] != "-----BEGIN" {
		t.Errorf("body = %q", body)
	}
}

func TestCARootNotFoundWhenDisabled(t *testing.T) {
	e := newTestEnv(t)
	resp := e.get(t, "/ca.pem")
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Errorf("status %d; want 404 when local CA is off", resp.StatusCode)
	}
}

type recordingSyncer struct{ n int }

func (r *recordingSyncer) Sync(context.Context) error { r.n++; return nil }

func TestDomainChangeTriggersCertSync(t *testing.T) {
	e := newTestEnv(t)
	e.completeSetup(t)
	rs := &recordingSyncer{}
	e.srv.SetCertSync(rs)
	app, _ := e.store.CreateApp(context.Background(), core.App{Name: "web", RepoURL: "https://x/y.git", Domain: "web.test"})
	form := url.Values{"host_kind": {"custom"}, "host": {"alias.test"}, "tls": {"on"}}
	resp := e.postForm(t, "/apps/"+itoa(app.ID)+"/domains", form)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("add domain status = %d, want 303", resp.StatusCode)
	}
	if rs.n == 0 {
		t.Error("adding a domain must trigger a cert sync")
	}
}

// TestNonComposeAppCreationTriggersCertSync covers the common case: creating a
// nixpacks (or dockerfile) app through the real /apps endpoint the UI uses.
// store.CreateApp seeds a TLS-enabled primary domain row for these apps inside
// its own transaction (see internal/store/apps.go's addDomainTx call and
// TestCreateAppSeedsPrimaryDomainRow) — no separate AddDomain call happens in
// the handler, so the sync hook must fire from handleCreateApp itself, not just
// from the compose-only firstDomain branch.
func TestNonComposeAppCreationTriggersCertSync(t *testing.T) {
	e := newTestEnv(t)
	e.completeSetup(t)
	rs := &recordingSyncer{}
	e.srv.SetCertSync(rs)

	resp := e.postForm(t, "/apps", appForm("web", "web.example.com"))
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("create app status = %d, want 303", resp.StatusCode)
	}
	if rs.n == 0 {
		t.Error("creating a nixpacks app with a domain must trigger a cert sync")
	}
}

// TestDomainFormCopyMatchesTLSMode checks the domain wizard's HTTPS-toggle
// copy names the actual certificate source instead of always saying "Let's
// Encrypt", which used to be hardcoded regardless of LocalCA.
func TestDomainFormCopyMatchesTLSMode(t *testing.T) {
	e := newTestEnv(t)
	e.completeSetup(t)
	app, _ := e.store.CreateApp(context.Background(), core.App{Name: "web", RepoURL: "https://x/y.git", Domain: "web.test"})

	page := body(t, e.get(t, "/apps/"+itoa(app.ID)))
	if !strings.Contains(page, "Automate HTTPS (Let's Encrypt)") {
		t.Error("without a local CA, the toggle should still read Let's Encrypt")
	}
	if strings.Contains(page, "Automate HTTPS (local CA)") {
		t.Error("without a local CA, the toggle should not claim local-CA copy")
	}

	caFile := filepath.Join(t.TempDir(), "rootCA.pem")
	if err := os.WriteFile(caFile, []byte("-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	e.srv.SetLocalCAFile(caFile)

	page = body(t, e.get(t, "/apps/"+itoa(app.ID)))
	if !strings.Contains(page, "Automate HTTPS (local CA)") {
		t.Error("with a local CA, the toggle should read local CA")
	}
	if strings.Contains(page, "Automate HTTPS (Let's Encrypt)") {
		t.Error("with a local CA, the toggle should not mention Let's Encrypt")
	}
}
