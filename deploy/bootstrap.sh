#!/usr/bin/env sh
# Outhaul remote installer. Served at https://outhaul.sh/install.
# Usage: curl -fsSL https://outhaul.sh/install | sh
# Runs under Debian's dash — keep it POSIX, no bashisms.
set -eu

OUTHAUL_REPO="${OUTHAUL_REPO:-https://github.com/outhaul-dev/outhaul.git}"

# ------------------------------------------------------------------ helpers --

# Prints the concrete Go version to download (e.g. 1.26.4). Prefers the
# `toolchain goX.Y.Z` directive; falls back to the `go X.Y[.Z]` directive.
go_version_from_gomod() {
	f=${1:-go.mod}
	v=$(awk '/^toolchain[ \t]+go[0-9]/ { sub(/^go/,"",$2); print $2; exit }' "$f")
	[ -n "$v" ] || v=$(awk '/^go[ \t]+[0-9]/ { print $2; exit }' "$f")
	printf '%s\n' "$v"
}

main() {
	printf 'outhaul installer\n'
}

# Run main only when executed, not when sourced by the test suite.
if [ "${OUTHAUL_INSTALLER_LIB:-0}" != 1 ]; then
	main "$@"
fi
