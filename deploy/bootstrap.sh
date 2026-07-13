#!/usr/bin/env sh
# Outhaul remote installer. Served at https://outhaul.sh/install.
# Usage: curl -fsSL https://outhaul.sh/install | sh
# Runs under Debian's dash — keep it POSIX, no bashisms.
set -eu

OUTHAUL_REPO="${OUTHAUL_REPO:-https://github.com/outhaul-dev/outhaul.git}"

# ------------------------------------------------------------------ helpers --
# (functions added by later tasks live above main)

main() {
	printf 'outhaul installer\n'
}

# Run main only when executed, not when sourced by the test suite.
if [ "${OUTHAUL_INSTALLER_LIB:-0}" != 1 ]; then
	main "$@"
fi
