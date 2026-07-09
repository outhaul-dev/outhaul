package store

import (
	"context"
	"testing"
)

func TestCloudflareTokenRoundTrip(t *testing.T) {
	st := openWithBox(t) // helper from env_test.go: store opened with a secret.Box
	ctx := context.Background()

	if _, ok, err := st.CloudflareToken(ctx); err != nil || ok {
		t.Fatalf("expected no token initially, got ok=%v err=%v", ok, err)
	}
	if on, err := st.TunnelEnabled(ctx); err != nil || on {
		t.Fatalf("expected tunnel disabled initially, got on=%v err=%v", on, err)
	}

	if err := st.SetCloudflareToken(ctx, "tok-abc"); err != nil {
		t.Fatalf("SetCloudflareToken: %v", err)
	}
	tok, ok, err := st.CloudflareToken(ctx)
	if err != nil || !ok || tok != "tok-abc" {
		t.Fatalf("CloudflareToken = %q, ok=%v, err=%v; want tok-abc", tok, ok, err)
	}
	if on, err := st.TunnelEnabled(ctx); err != nil || !on {
		t.Fatalf("expected tunnel enabled, got on=%v err=%v", on, err)
	}

	// The stored value must be sealed, not plaintext.
	raw, _, _ := st.GetSetting(ctx, cloudflareTokenKey)
	if raw == "tok-abc" {
		t.Fatal("token stored in plaintext; must be sealed")
	}

	if err := st.ClearCloudflareToken(ctx); err != nil {
		t.Fatalf("ClearCloudflareToken: %v", err)
	}
	if _, ok, _ := st.CloudflareToken(ctx); ok {
		t.Fatal("token should be gone after clear")
	}
}
