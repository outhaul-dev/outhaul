package server

import (
	"context"
	"io"
	"net/url"
	"os"
	"path/filepath"
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
	if rs.n == 0 {
		t.Error("adding a domain must trigger a cert sync")
	}
}
