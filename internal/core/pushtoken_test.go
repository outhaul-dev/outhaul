package core

import (
	"strings"
	"testing"
	"time"
)

func TestValidPushTokenShape(t *testing.T) {
	hex64 := strings.Repeat("0123456789abcdef", 4)
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"ohp_" + hex64, true},
		{"ohp_" + hex64[:63], false},
		{"ohp_" + hex64 + "0", false},
		{"ohp_" + strings.ToUpper(hex64), false},
		{"ohq_" + hex64, false},
		{"", false},
		{"ohp_" + hex64 + "\n", false},
		{"ohp_" + strings.Repeat("g", 64), false},
	} {
		if got := ValidPushTokenShape(tc.in); got != tc.want {
			t.Errorf("ValidPushTokenShape(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestCleanPushTokenLabel(t *testing.T) {
	for _, tc := range []struct {
		in, want string
		ok       bool
	}{
		{" ci ", "ci", true},
		{"", "", false},
		{"   ", "", false},
		{strings.Repeat("a", 65), "", false},
		{"a\tb", "", false},
		{"a\x00b", "", false},
		{strings.Repeat("a", 64), strings.Repeat("a", 64), true},
		{"déploiement", "déploiement", true},
		{strings.Repeat("é", 64), strings.Repeat("é", 64), true},
	} {
		got, ok := CleanPushTokenLabel(tc.in)
		if ok != tc.ok || got != tc.want {
			t.Errorf("CleanPushTokenLabel(%q) = %q, %v; want %q, %v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestPushTokenEndsAt(t *testing.T) {
	day0 := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	tok := PushToken{CreatedAt: day0, ExpiresAt: day0.AddDate(0, 0, 20)}
	if got := tok.EndsAt(30); !got.Equal(day0.AddDate(0, 0, 20)) {
		t.Errorf("EndsAt(30) = %v, want day 20", got)
	}
	if got := tok.EndsAt(10); !got.Equal(day0.AddDate(0, 0, 10)) {
		t.Errorf("EndsAt(10) = %v, want day 10", got)
	}
	end := tok.EndsAt(30)
	if tok.Expired(end.Add(-time.Nanosecond), 30) {
		t.Error("expired just before EndsAt")
	}
	if !tok.Expired(end, 30) {
		t.Error("not expired at exactly EndsAt")
	}
}
