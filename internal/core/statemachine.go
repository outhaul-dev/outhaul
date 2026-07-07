package core

// DeployStatus is the lifecycle state of a single deploy attempt. The
// deployments table is also the job queue, so this value is the single source
// of truth for where an attempt is. See ARCHITECTURE.md for the diagram.
type DeployStatus string

const (
	StatusQueued     DeployStatus = "queued"
	StatusBuilding   DeployStatus = "building"
	StatusDeploying  DeployStatus = "deploying"
	StatusRunning    DeployStatus = "running"
	StatusFailed     DeployStatus = "failed"
	StatusCancelled  DeployStatus = "cancelled"
	StatusSuperseded DeployStatus = "superseded"
)

// transitions is the legal-transition matrix. A pair absent from this map is
// illegal, which makes self-transitions and unknown statuses reject by default.
// running -> superseded is the one terminal-to-terminal edge: a running row is
// retired when a newer deploy takes its place (its container is already gone).
var transitions = map[DeployStatus]map[DeployStatus]bool{
	StatusQueued:     {StatusBuilding: true, StatusCancelled: true},
	StatusBuilding:   {StatusDeploying: true, StatusFailed: true, StatusCancelled: true},
	StatusDeploying:  {StatusRunning: true, StatusFailed: true},
	StatusRunning:    {StatusSuperseded: true},
	StatusFailed:     {},
	StatusCancelled:  {},
	StatusSuperseded: {},
}

// CanTransition reports whether moving a deployment from -> to is legal.
func CanTransition(from, to DeployStatus) bool {
	return transitions[from][to]
}

// IsTerminal reports whether the status is a settled end state (running,
// failed, cancelled, superseded). running is terminal for lifecycle purposes
// even though it may later be retired to superseded by a newer deploy.
func (s DeployStatus) IsTerminal() bool {
	switch s {
	case StatusRunning, StatusFailed, StatusCancelled, StatusSuperseded:
		return true
	default:
		return false
	}
}

// IsActive reports whether a worker may currently be operating on the attempt
// (building or deploying). Crash recovery fails active rows on restart;
// queued rows are safe to resume and are not active.
func (s DeployStatus) IsActive() bool {
	switch s {
	case StatusBuilding, StatusDeploying:
		return true
	default:
		return false
	}
}

// CanCancel reports whether an operator may cancel the attempt. Allowed only
// before the container goes live (queued or building).
func (s DeployStatus) CanCancel() bool {
	switch s {
	case StatusQueued, StatusBuilding:
		return true
	default:
		return false
	}
}
