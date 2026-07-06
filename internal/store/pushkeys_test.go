package store

import (
	"context"
	"testing"

	"github.com/james-smart/outhaul/internal/core"
)

func TestPushKeyCRUD(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	pk, err := s.AddPushKey(ctx, core.PushKey{
		Label:       "laptop",
		Fingerprint: "SHA256:aaa",
		PublicKey:   "ssh-ed25519 AAAA laptop",
	})
	if err != nil {
		t.Fatalf("AddPushKey: %v", err)
	}
	if pk.ID == 0 || pk.CreatedAt.IsZero() {
		t.Fatalf("AddPushKey did not set ID/CreatedAt: %+v", pk)
	}
	if pk.LastUsedAt != nil {
		t.Fatalf("new key should have nil LastUsedAt, got %v", pk.LastUsedAt)
	}

	keys, err := s.ListPushKeys(ctx)
	if err != nil || len(keys) != 1 {
		t.Fatalf("ListPushKeys = %v, %v; want 1 key", keys, err)
	}

	got, ok, err := s.FindPushKeyByFingerprint(ctx, "SHA256:aaa")
	if err != nil || !ok || got.ID != pk.ID {
		t.Fatalf("FindPushKeyByFingerprint = %+v, %v, %v", got, ok, err)
	}

	if _, ok, _ := s.FindPushKeyByFingerprint(ctx, "SHA256:missing"); ok {
		t.Fatalf("expected miss for unknown fingerprint")
	}

	if err := s.TouchPushKey(ctx, pk.ID); err != nil {
		t.Fatalf("TouchPushKey: %v", err)
	}
	got, _, _ = s.FindPushKeyByFingerprint(ctx, "SHA256:aaa")
	if got.LastUsedAt == nil {
		t.Fatalf("TouchPushKey did not stamp LastUsedAt")
	}

	if err := s.DeletePushKey(ctx, pk.ID); err != nil {
		t.Fatalf("DeletePushKey: %v", err)
	}
	keys, _ = s.ListPushKeys(ctx)
	if len(keys) != 0 {
		t.Fatalf("expected 0 keys after delete, got %d", len(keys))
	}
}

func TestPushKeyDuplicateFingerprint(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, err := s.AddPushKey(ctx, core.PushKey{Label: "a", Fingerprint: "SHA256:x", PublicKey: "k"}); err != nil {
		t.Fatalf("first add: %v", err)
	}
	if _, err := s.AddPushKey(ctx, core.PushKey{Label: "b", Fingerprint: "SHA256:x", PublicKey: "k2"}); err == nil {
		t.Fatalf("expected UNIQUE violation on duplicate fingerprint")
	}
}
