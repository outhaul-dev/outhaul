package deploy

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// symlinkEscape returns an error naming the first symlink under dir that
// resolves (following every link in the chain) to a path outside dir, or that
// cannot be resolved at all. A fork preview's checkout is untrusted, and
// Nixpacks reads it on the host as Outhaul's user: a link out of the checkout
// could pull host files into the fork's image. Each link is resolved for real
// rather than by cleaning its text, because a ".." after a symlinked directory
// climbs from wherever that link points.
func symlinkEscape(dir string) error {
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return err
	}
	return filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&fs.ModeSymlink == 0 {
			return nil
		}
		rel, _ := filepath.Rel(dir, path)
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			return fmt.Errorf("symlink %s cannot be resolved: %w", rel, err)
		}
		inside, err := filepath.Rel(root, resolved)
		if err != nil || inside == ".." || strings.HasPrefix(inside, ".."+string(filepath.Separator)) {
			return fmt.Errorf("symlink %s points outside the repository", rel)
		}
		return nil
	})
}

// nixpacksConfigFiles are the repo-root files Nixpacks reads its config from.
var nixpacksConfigFiles = []string{"nixpacks.toml", "nixpacks.json"}

// nixpacksImageOverride returns an error if dir's Nixpacks config names its own
// base images. A fork could point buildImage or runImage at the parent's local
// production image (outhaul/<app>:<id>) and read its code and baked-in build
// env. The check is a plain text match on the key names: a false positive only
// refuses a fork preview, which is the safe direction.
func nixpacksImageOverride(dir string) error {
	for _, name := range nixpacksConfigFiles {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		for _, key := range []string{"buildImage", "runImage"} {
			if strings.Contains(string(b), key) {
				return fmt.Errorf("%s sets %s; fork previews build on Nixpacks' default images only", name, key)
			}
		}
	}
	return nil
}

// checkForkCheckout runs the checks an untrusted fork checkout must pass before
// it is built: no symlinks out of the repository, no Nixpacks image overrides.
func checkForkCheckout(dir string) error {
	if err := symlinkEscape(dir); err != nil {
		return err
	}
	return nixpacksImageOverride(dir)
}
