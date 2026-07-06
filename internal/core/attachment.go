package core

import "regexp"

// Attachment links an app to a managed database, injecting the database's
// connection URL into the app's environment as EnvVar at deploy time. The link
// stores no connection string: the value is computed from the Database row on
// every deploy, so credential rotation propagates automatically.
type Attachment struct {
	ID         int64
	AppID      int64
	DatabaseID int64
	EnvVar     string // UPPER_SNAKE_CASE; unique per app
}

// envVarRe is the UPPER_SNAKE_CASE rule for env var keys (mirrors the server's
// envKeyRe; a later task may unify them).
var envVarRe = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)

// ValidEnvVar reports whether name is a legal environment variable key.
func ValidEnvVar(name string) bool { return envVarRe.MatchString(name) }
