package github

import (
	"encoding/json"
	"testing"
)

func TestBuildManifest(t *testing.T) {
	raw, err := BuildManifest(ManifestParams{Name: "outhaul-abc123", PublicURL: "https://slip.example.com"})
	if err != nil {
		t.Fatalf("BuildManifest: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("manifest is not valid JSON: %v", err)
	}
	if m["name"] != "outhaul-abc123" {
		t.Errorf("name = %v", m["name"])
	}
	if m["url"] != "https://slip.example.com" {
		t.Errorf("url = %v", m["url"])
	}
	if m["redirect_url"] != "https://slip.example.com/github/callback" {
		t.Errorf("redirect_url = %v", m["redirect_url"])
	}
	if m["setup_url"] != "https://slip.example.com/github/setup" {
		t.Errorf("setup_url = %v", m["setup_url"])
	}
	if m["setup_on_update"] != true {
		t.Errorf("setup_on_update = %v, want true", m["setup_on_update"])
	}
	hook := m["hook_attributes"].(map[string]any)
	if hook["url"] != "https://slip.example.com/webhooks/github" {
		t.Errorf("hook url = %v", hook["url"])
	}
	if m["public"] != false {
		t.Errorf("public = %v, want false", m["public"])
	}
	perms := m["default_permissions"].(map[string]any)
	if perms["contents"] != "read" || perms["metadata"] != "read" {
		t.Errorf("permissions = %v", perms)
	}
	events := m["default_events"].([]any)
	if len(events) != 1 || events[0] != "push" {
		t.Errorf("events = %v", events)
	}
}

func TestBuildManifestTrimsTrailingSlash(t *testing.T) {
	raw, _ := BuildManifest(ManifestParams{Name: "x", PublicURL: "https://slip.example.com/"})
	var m map[string]any
	_ = json.Unmarshal([]byte(raw), &m)
	if m["redirect_url"] != "https://slip.example.com/github/callback" {
		t.Errorf("redirect_url = %v (trailing slash not trimmed)", m["redirect_url"])
	}
}
