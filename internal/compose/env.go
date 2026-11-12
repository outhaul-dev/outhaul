package compose

import "strings"

// RestrictedEnv keeps only the variables a Compose run needs, so a Compose
// file's ${NAME} cannot read Outhaul's own settings or secrets. Everything
// else is dropped, including every OUTHAUL_* variable and the cloud
// credentials an operator happens to have exported. The stack's own .env,
// which Compose reads from its project directory, still supplies the app's
// variables.
func RestrictedEnv(environ []string) []string {
	kept := make([]string, 0, len(environ))
	for _, kv := range environ {
		name, _, ok := strings.Cut(kv, "=")
		if !ok || !keepEnv(name) {
			continue
		}
		kept = append(kept, kv)
	}
	return kept
}

// keptEnvNames are the exact names a Compose run needs: a usable PATH and
// home, locale settings, a temp dir, the CA bundle, and the proxy settings
// curl and Go read.
var keptEnvNames = map[string]struct{}{
	"PATH":          {},
	"HOME":          {},
	"USER":          {},
	"LOGNAME":       {},
	"LANG":          {},
	"TMPDIR":        {},
	"SSL_CERT_FILE": {},
	"SSL_CERT_DIR":  {},
}

// keptEnvPrefixes carry per-tool configuration: locale variants, the XDG
// directories, and how the Docker CLI, BuildKit, Buildx and Compose are set
// up (host, TLS certificates, progress output).
var keptEnvPrefixes = []string{"LC_", "XDG_", "DOCKER_", "BUILDKIT_", "BUILDX_", "COMPOSE_"}

// keptProxyNames are matched in any case: the uppercase forms are
// conventional, but curl and Go also read the lowercase ones.
var keptProxyNames = map[string]struct{}{
	"HTTP_PROXY":  {},
	"HTTPS_PROXY": {},
	"NO_PROXY":    {},
}

func keepEnv(name string) bool {
	if _, ok := keptProxyNames[strings.ToUpper(name)]; ok {
		return true
	}
	if _, ok := keptEnvNames[name]; ok {
		return true
	}
	for _, prefix := range keptEnvPrefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// withPWD returns env with PWD=<dir> appended. Go adds PWD itself only when it
// passes the whole environment on, so a filtered one must say where it runs.
// os/exec keeps the last value of a repeated name, so a PWD already in env is
// replaced rather than duplicated.
func withPWD(env []string, dir string) []string {
	if dir == "" {
		return env
	}
	out := make([]string, 0, len(env)+1)
	out = append(out, env...)
	return append(out, "PWD="+dir)
}
