// Package dbaas manages database containers: one engine instance per
// database, provisioned from an official image onto the shared Docker network
// with its data bind-mounted under the data dir. It is the databases
// counterpart of the deploy worker — the server talks to it through a small
// interface and never creates containers itself.
package dbaas

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"

	"github.com/james-smart/outhaul/internal/core"
)

// engine describes how to run one supported database engine.
type engine struct {
	DefaultImage string
	Port         int    // port the engine listens on inside the container
	DataDir      string // container path that must persist across recreation
	Scheme       string // connection-URL scheme
	HasUserDB    bool   // engine has a named user + initial database (Redis doesn't)
}

var engines = map[string]engine{
	core.EnginePostgres: {
		DefaultImage: "postgres:17",
		Port:         5432,
		DataDir:      "/var/lib/postgresql/data",
		Scheme:       "postgres",
		HasUserDB:    true,
	},
	core.EngineMySQL: {
		DefaultImage: "mysql:8.4",
		Port:         3306,
		DataDir:      "/var/lib/mysql",
		Scheme:       "mysql",
		HasUserDB:    true,
	},
	core.EngineRedis: {
		DefaultImage: "redis:7",
		Port:         6379,
		DataDir:      "/data",
		Scheme:       "redis",
		HasUserDB:    false,
	},
}

// ValidEngine reports whether name is a supported engine.
func ValidEngine(name string) bool {
	_, ok := engines[name]
	return ok
}

// DefaultImage returns the engine's pinned default image.
func DefaultImage(engineName string) string { return engines[engineName].DefaultImage }

// Port returns the engine's in-container listen port.
func Port(engineName string) int { return engines[engineName].Port }

// HasUserDB reports whether the engine has a named user and initial database
// (Redis authenticates with a password only).
func HasUserDB(engineName string) bool { return engines[engineName].HasUserDB }

// env builds the container environment that initializes credentials on first
// boot. The official images only read these when the data dir is empty; on
// recreation the persisted data wins, which is what we want.
func env(d core.Database) []string {
	switch d.Engine {
	case core.EnginePostgres:
		return []string{
			"POSTGRES_USER=" + d.Username,
			"POSTGRES_PASSWORD=" + d.Password,
			"POSTGRES_DB=" + d.DBName,
		}
	case core.EngineMySQL:
		// The generated password doubles as the root password: one credential
		// per database keeps the model (and the UI) simple.
		return []string{
			"MYSQL_ROOT_PASSWORD=" + d.Password,
			"MYSQL_USER=" + d.Username,
			"MYSQL_PASSWORD=" + d.Password,
			"MYSQL_DATABASE=" + d.DBName,
		}
	}
	return nil
}

// cmd builds the container command; only Redis needs one (auth is a flag, not
// an env var).
func cmd(d core.Database) []string {
	if d.Engine == core.EngineRedis {
		return []string{"redis-server", "--requirepass", d.Password}
	}
	return nil
}

// InternalURL is the connection URL apps use over the shared Docker network,
// where the container name is the hostname.
func InternalURL(d core.Database) string {
	return connURL(d, ContainerName(d.Name), engines[d.Engine].Port)
}

// ExternalURL is the connection URL for the published host port; host is a
// placeholder for the server's address (Outhaul doesn't know its public IP).
// Empty when no port is published.
func ExternalURL(d core.Database, host string) string {
	if d.ExtPort == 0 {
		return ""
	}
	return connURL(d, host, d.ExtPort)
}

func connURL(d core.Database, host string, port int) string {
	e := engines[d.Engine]
	if !e.HasUserDB {
		return fmt.Sprintf("%s://:%s@%s:%d/0", e.Scheme, d.Password, host, port)
	}
	return fmt.Sprintf("%s://%s:%s@%s:%d/%s", e.Scheme, d.Username, d.Password, host, port, d.DBName)
}

// NewPassword generates a URL-safe credential (hex, so connection URLs never
// need escaping).
func NewPassword() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand failure is unrecoverable
	}
	return hex.EncodeToString(b)
}
