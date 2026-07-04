package core

import "time"

// Deployment is one deploy attempt for an App. The deployments table doubles as
// the job queue; Status (see statemachine.go) is the source of truth for where
// the attempt is in its lifecycle.
type Deployment struct {
	ID     int64
	AppID  int64
	Status DeployStatus

	// Reason carries a human-readable explanation for terminal failure/cancel
	// states (e.g. "clone failed", "interrupted by restart"). Empty otherwise.
	Reason string

	// Image is the tag of the image this attempt runs. For a normal deploy it
	// is set once the build succeeds (empty until then); for a rollback it is
	// pre-set at enqueue time, which is what tells the pipeline to skip the
	// clone and build.
	Image string

	// RollbackOf is the ID of the deployment whose image this attempt reuses,
	// or 0 for a normal build-from-source deploy.
	RollbackOf int64

	CreatedAt  time.Time
	StartedAt  *time.Time // when a worker claimed it (queued -> building)
	FinishedAt *time.Time // when it reached a terminal state
}
