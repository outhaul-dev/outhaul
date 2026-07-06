package core

import "regexp"

// AppNameRe restricts app names to values safe as container names, Traefik
// router names, and DNS labels: lowercase letters, digits, and hyphens, 2–40
// characters, not starting or ending with a hyphen.
var AppNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,38}[a-z0-9]$`)

// ValidAppName reports whether name is a valid app identifier per AppNameRe.
func ValidAppName(name string) bool { return AppNameRe.MatchString(name) }
