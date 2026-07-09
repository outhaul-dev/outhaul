package server

import (
	"context"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

type fakeTunnel struct {
	enabledWith string
	disabled    bool
}

func (f *fakeTunnel) Enable(ctx context.Context, token string) error { f.enabledWith = token; return nil }
func (f *fakeTunnel) Disable(ctx context.Context) error              { f.disabled = true; return nil }

func TestHandleEnableTunnelRejectsEmptyToken(t *testing.T) {
	s := newTestEnv(t).srv
	ft := &fakeTunnel{}
	s.SetTunnelControl(ft)

	req := httptest.NewRequest("POST", "/settings/tunnel/enable", strings.NewReader("token=  "))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.handleEnableTunnel(rec, req)

	if ft.enabledWith != "" {
		t.Errorf("empty token should be rejected, got Enable(%q)", ft.enabledWith)
	}
	if rec.Code != 400 {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestHandleEnableTunnelStoresToken(t *testing.T) {
	s := newTestEnv(t).srv
	ft := &fakeTunnel{}
	s.SetTunnelControl(ft)

	form := url.Values{"token": {"tok-xyz"}}
	req := httptest.NewRequest("POST", "/settings/tunnel/enable", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.handleEnableTunnel(rec, req)

	if ft.enabledWith != "tok-xyz" {
		t.Errorf("Enable called with %q, want tok-xyz", ft.enabledWith)
	}
}
