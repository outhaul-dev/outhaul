package core

import "testing"

func TestCanTransition(t *testing.T) {
	// The full legal-transition matrix from ARCHITECTURE.md. Every ordered
	// pair of distinct statuses is enumerated so an accidental extra edge
	// (or a missing one) fails a test rather than slipping through.
	legal := map[DeployStatus]map[DeployStatus]bool{
		StatusQueued:    {StatusBuilding: true, StatusCancelled: true},
		StatusBuilding:  {StatusDeploying: true, StatusFailed: true, StatusCancelled: true},
		StatusDeploying: {StatusRunning: true, StatusFailed: true},
		StatusRunning:   {},
		StatusFailed:    {},
		StatusCancelled: {},
	}

	all := []DeployStatus{
		StatusQueued, StatusBuilding, StatusDeploying,
		StatusRunning, StatusFailed, StatusCancelled,
	}

	for _, from := range all {
		for _, to := range all {
			want := legal[from][to] // self-transitions and everything unlisted are false
			got := CanTransition(from, to)
			if got != want {
				t.Errorf("CanTransition(%q, %q) = %v, want %v", from, to, got, want)
			}
		}
	}
}

func TestCanTransitionRejectsSelfTransitions(t *testing.T) {
	all := []DeployStatus{
		StatusQueued, StatusBuilding, StatusDeploying,
		StatusRunning, StatusFailed, StatusCancelled,
	}
	for _, s := range all {
		if CanTransition(s, s) {
			t.Errorf("CanTransition(%q, %q) = true, want false (no self-transitions)", s, s)
		}
	}
}

func TestCanTransitionUnknownStatus(t *testing.T) {
	if CanTransition(DeployStatus("bogus"), StatusRunning) {
		t.Error("transition from unknown status should be rejected")
	}
	if CanTransition(StatusQueued, DeployStatus("bogus")) {
		t.Error("transition to unknown status should be rejected")
	}
}

func TestIsTerminal(t *testing.T) {
	tests := []struct {
		status DeployStatus
		want   bool
	}{
		{StatusQueued, false},
		{StatusBuilding, false},
		{StatusDeploying, false},
		{StatusRunning, true},
		{StatusFailed, true},
		{StatusCancelled, true},
	}
	for _, tt := range tests {
		if got := tt.status.IsTerminal(); got != tt.want {
			t.Errorf("%q.IsTerminal() = %v, want %v", tt.status, got, tt.want)
		}
	}
}

func TestIsActive(t *testing.T) {
	// "active" = a worker may currently be operating on it. Used by crash
	// recovery to decide which rows to fail on restart, and by the dispatcher
	// to enforce per-app serialization.
	tests := []struct {
		status DeployStatus
		want   bool
	}{
		{StatusQueued, false}, // queued is safe to resume, not yet owned by a worker
		{StatusBuilding, true},
		{StatusDeploying, true},
		{StatusRunning, false},
		{StatusFailed, false},
		{StatusCancelled, false},
	}
	for _, tt := range tests {
		if got := tt.status.IsActive(); got != tt.want {
			t.Errorf("%q.IsActive() = %v, want %v", tt.status, got, tt.want)
		}
	}
}

func TestCanCancel(t *testing.T) {
	// Cancellation is allowed only before the container goes live.
	tests := []struct {
		status DeployStatus
		want   bool
	}{
		{StatusQueued, true},
		{StatusBuilding, true},
		{StatusDeploying, false},
		{StatusRunning, false},
		{StatusFailed, false},
		{StatusCancelled, false},
	}
	for _, tt := range tests {
		if got := tt.status.CanCancel(); got != tt.want {
			t.Errorf("%q.CanCancel() = %v, want %v", tt.status, got, tt.want)
		}
	}
}
