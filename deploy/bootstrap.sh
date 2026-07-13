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

# Pure: decide color level from explicit inputs (testable).
# 0=plain 1=16-color 2=256-color 3=truecolor
_color_level() { # no_color is_tty colorterm tput_colors
	[ -n "$1" ] && { echo 0; return; }
	[ "$2" != 1 ] && { echo 0; return; }
	case "$3" in *truecolor*|*24bit*) echo 3; return;; esac
	if [ "${4:-0}" -ge 256 ]; then echo 2
	elif [ "${4:-0}" -ge 8 ]; then echo 1
	else echo 0; fi
}

# Gathers the real environment and calls _color_level.
detect_color_level() {
	tc=$(tput colors 2>/dev/null || echo 0)
	if [ -t 1 ]; then tty=1; else tty=0; fi
	_color_level "${NO_COLOR:-}" "$tty" "${COLORTERM:-}" "$tc"
}

main() {
	printf 'outhaul installer\n'
}

# Run main only when executed, not when sourced by the test suite.
if [ "${OUTHAUL_INSTALLER_LIB:-0}" != 1 ]; then
	main "$@"
fi
