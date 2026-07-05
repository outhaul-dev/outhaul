package core

import "time"

// Database engines Outhaul can manage. Each maps to an official image, a
// container port, and an env/credential convention (see internal/dbaas).
const (
	EnginePostgres = "postgres"
	EngineMySQL    = "mysql"
	EngineRedis    = "redis"
)

// Database lifecycle states. Unlike deployments there is no transition
// matrix: a database has one long-lived container, and the only ambiguity a
// stored status resolves is "being created" vs "failed to create", which
// cannot be read back from Docker.
const (
	DBCreating = "creating" // provisioning in progress (pull, create, start)
	DBRunning  = "running"  // provisioned and started
	DBStopped  = "stopped"  // stopped by the operator
	DBFailed   = "failed"   // provisioning failed; Reason says why
)

// Database is a managed database container: one engine instance inside a
// project, on the shared Docker network, with its data bind-mounted under the
// data dir so it survives container recreation.
type Database struct {
	ID        int64
	ProjectID int64
	Name      string // unique, DNS-safe; container is named outhaul-db-<name>
	Engine    string // EnginePostgres | EngineMySQL | EngineRedis
	Image     string // Docker image ref (a default per engine, overridable)
	Username  string // login user; empty for Redis (password-only auth)
	Password  string // generated at create; stored encrypted at rest
	DBName    string // initial database name; empty for Redis
	ExtPort   int    // published host port; 0 = internal-only

	Status    string // DBCreating | DBRunning | DBStopped | DBFailed
	Reason    string // failure detail when Status == DBFailed
	CreatedAt time.Time
}
