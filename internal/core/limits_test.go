package core

import (
	"strings"
	"testing"
)

func TestLimitsOr(t *testing.T) {
	got := Limits{MilliCPUs: 500}.Or(Limits{MilliCPUs: 1000, Memory: 1 << 30, Pids: 512})
	if got.MilliCPUs != 500 {
		t.Errorf("MilliCPUs = %d, want 500 (the receiver's non-zero field)", got.MilliCPUs)
	}
	if got.Memory != 1<<30 {
		t.Errorf("Memory = %d, want %d (the fallback's)", got.Memory, 1<<30)
	}
	if got.Pids != 512 {
		t.Errorf("Pids = %d, want 512 (the fallback's)", got.Pids)
	}

	if got := (Limits{Memory: 42}).Or(Limits{}); got != (Limits{Memory: 42}) {
		t.Errorf("Or with an empty fallback = %+v, want the receiver unchanged", got)
	}
	if got := (Limits{}).Or(Limits{Memory: 42}); got != (Limits{Memory: 42}) {
		t.Errorf("Or of an empty receiver = %+v, want the fallback", got)
	}
}

func TestParseCPUs(t *testing.T) {
	cases := []struct {
		in   string
		want int64
	}{
		{"1", 1000},
		{"0.5", 500},
		{"1.5", 1500},
		{"0.01", 10},
		{"0.010", 10},
		{"16", 16000},
	}
	for _, tc := range cases {
		got, err := ParseCPUs(tc.in)
		if err != nil {
			t.Errorf("ParseCPUs(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseCPUs(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}

	for _, in := range []string{"", "0", "0.001", "50%", "500m", "1,5", ".5", "1e2", "-1", "1.0001", " 1"} {
		if got, err := ParseCPUs(in); err == nil {
			t.Errorf("ParseCPUs(%q) = %d, want an error", in, got)
		}
	}
}

func TestParseMemory(t *testing.T) {
	cases := []struct {
		in   string
		want int64
	}{
		{"512m", 512 << 20},
		{"1g", 1 << 30},
		{"1.5g", 3 << 29},
		{"1gb", 1 << 30},
		{"1G", 1 << 30},
		{"6291456", 6 << 20},
		{"6m", 6 << 20},
		{"8192k", 8 << 20},
		{"8388608b", 8 << 20},
	}
	for _, tc := range cases {
		got, err := ParseMemory(tc.in)
		if err != nil {
			t.Errorf("ParseMemory(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseMemory(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}

	for _, in := range []string{"", "0", "5m", "1t", "1 g", "1gib", "-1", "1.5.0g", "1MB"} {
		if got, err := ParseMemory(in); err == nil {
			t.Errorf("ParseMemory(%q) = %d, want an error", in, got)
		}
	}

	// The refusal names the minimum the way a setting writes it.
	if _, err := ParseMemory("5m"); err == nil || !strings.Contains(err.Error(), "6m") {
		t.Errorf("ParseMemory(\"5m\") error = %v, want one naming 6m", err)
	}
}
