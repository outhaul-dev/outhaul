package gitrepo

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/james-smart/outhaul/internal/core"
)

// composeNames lists candidate compose filenames in precedence order.
var composeNames = []string{"compose.yaml", "compose.yml", "docker-compose.yaml", "docker-compose.yml"}

// DetectKind inspects the top-level tree of ref in the bare repo at dir and
// returns the app build kind and, for compose, the detected compose file path.
// Precedence: root Dockerfile → dockerfile; else a root compose file → compose;
// else nixpacks.
func DetectKind(ctx context.Context, dir, ref string) (kind, composePath string, err error) {
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "ls-tree", "--name-only", ref)
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		return "", "", fmt.Errorf("git ls-tree %s: %v: %s", ref, err, strings.TrimSpace(errBuf.String()))
	}
	var names []string
	sc := bufio.NewScanner(&out)
	for sc.Scan() {
		names = append(names, strings.TrimSpace(sc.Text()))
	}
	kind, composePath = detectKindFromNames(names)
	return kind, composePath, nil
}

// detectKindFromNames is the pure decision over a top-level name listing.
func detectKindFromNames(names []string) (kind, composePath string) {
	set := make(map[string]bool, len(names))
	for _, n := range names {
		set[n] = true
	}
	if set["Dockerfile"] {
		return core.KindDockerfile, ""
	}
	for _, c := range composeNames {
		if set[c] {
			return core.KindCompose, c
		}
	}
	return core.KindNixpacks, ""
}
