package deploy

import (
	"fmt"
	"io/fs"
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
