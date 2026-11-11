package core

import "testing"

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
