package store

import (
	"context"
	"testing"
)

func TestSettingsGetSetUpsert(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if _, ok, err := s.GetSetting(ctx, "ssh_addr"); err != nil || ok {
		t.Fatalf("missing key: got ok=%v err=%v; want ok=false err=nil", ok, err)
	}
	if err := s.SetSetting(ctx, "ssh_addr", ":2222"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	v, ok, err := s.GetSetting(ctx, "ssh_addr")
	if err != nil || !ok || v != ":2222" {
		t.Fatalf("GetSetting = %q, %v, %v; want :2222,true,nil", v, ok, err)
	}
	if err := s.SetSetting(ctx, "ssh_addr", ":2200"); err != nil {
		t.Fatalf("SetSetting upsert: %v", err)
	}
	v, _, _ = s.GetSetting(ctx, "ssh_addr")
	if v != ":2200" {
		t.Fatalf("upsert value = %q; want :2200", v)
	}
}
