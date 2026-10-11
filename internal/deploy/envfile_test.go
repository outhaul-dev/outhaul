package deploy

import (
	"strings"
	"testing"

	"github.com/outhaul-dev/outhaul/internal/core"
)

// Compose interpolates $VAR inside double-quoted dotenv values, so an
// unescaped secret like pa$word reaches the stack as "pa" (and compose logs
// the "word" fragment as an unset variable). A literal $ must be written $$.
func TestEnvFileEscapesDollar(t *testing.T) {
	got := string(envFile([]core.EnvVar{
		{Key: "P", Value: "pa$word"},
		{Key: "HASH", Value: "$2y$10$abc"},
	}))
	for _, want := range []string{`P="pa$$word"`, `HASH="$$2y$$10$$abc"`} {
		if !strings.Contains(got, want+"\n") {
			t.Errorf("env file missing %s:\n%s", want, got)
		}
	}
}
