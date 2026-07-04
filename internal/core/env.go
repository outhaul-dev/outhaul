package core

import (
	"fmt"
	"regexp"
	"strings"
)

// EnvVar is a single environment variable for an app. Value is plaintext at this
// layer; the store encrypts it at rest. Secret vars are injected into the running
// container only, never into the build (so they are not baked into an image layer).
type EnvVar struct {
	Key      string
	Value    string
	IsSecret bool
}

// projectRefRe matches ${{project.KEY}} placeholders in app env values —
// Dokploy's syntax for referencing a project's shared variables. KEY follows
// the same UPPER_SNAKE_CASE rule the UI enforces on env keys.
var projectRefRe = regexp.MustCompile(`\$\{\{project\.([A-Z_][A-Z0-9_]*)\}\}`)

// ResolveEnv returns appVars with every ${{project.KEY}} placeholder replaced
// by the project's shared variable of that name. Shared variables are never
// injected unreferenced. A value that pulled in a secret shared variable
// becomes secret itself, so it stays out of build environments. A reference
// to an undefined shared variable is an error: failing the deploy beats
// shipping a literal placeholder into a container.
func ResolveEnv(appVars, projectVars []EnvVar) ([]EnvVar, error) {
	shared := make(map[string]EnvVar, len(projectVars))
	for _, v := range projectVars {
		shared[v.Key] = v
	}
	resolved := make([]EnvVar, len(appVars))
	var missing []string
	for i, v := range appVars {
		secret := v.IsSecret
		v.Value = projectRefRe.ReplaceAllStringFunc(v.Value, func(ref string) string {
			key := projectRefRe.FindStringSubmatch(ref)[1]
			p, ok := shared[key]
			if !ok {
				missing = append(missing, fmt.Sprintf("%s (in %s)", ref, v.Key))
				return ref
			}
			if p.IsSecret {
				secret = true
			}
			return p.Value
		})
		v.IsSecret = secret
		resolved[i] = v
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("env references undefined project variable(s): %s", strings.Join(missing, ", "))
	}
	return resolved, nil
}
