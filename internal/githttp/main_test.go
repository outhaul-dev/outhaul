package githttp

import (
	"fmt"
	"os"
	"testing"

	"github.com/outhaul-dev/outhaul/internal/githook"
)

// The hook gitrepo writes runs `<bin> git-hook <app> <socket>`. In these
// tests <bin> is this test binary, so a push reaches a real bridge.
func TestMain(m *testing.M) {
	if len(os.Args) == 4 && os.Args[1] == "git-hook" {
		code, err := githook.RunHookFrom(os.Args[3], os.Args[2], githook.OriginFromEnv(os.Getenv), os.Stdin, os.Stdout)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
		}
		os.Exit(code)
	}
	os.Exit(m.Run())
}
