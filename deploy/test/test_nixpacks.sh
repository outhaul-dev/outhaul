#!/usr/bin/env sh
# shellcheck disable=SC2034 # the sourced bootstrap.sh reads these globals
here=$(cd "$(dirname "$0")" && pwd)
. "$here/assert.sh"
OUTHAUL_INSTALLER_LIB=1 . "$here/../bootstrap.sh"

assert_eq "pinned version" "1.41.0" "$NIXPACKS_VERSION"
assert_eq "amd64 digest" "0f55de7874507b9cf7502113120bd96f2ab6979f78d10eaf2eb2ade9207b3af6" "$NIXPACKS_SHA256_AMD64"
assert_eq "arm64 digest" "912bd02dd2bb6f9c3a9ed965fe8a68b4aa318dc7a2546e2eca6f2806a894ba39" "$NIXPACKS_SHA256_ARM64"
assert_eq "amd64 target" "x86_64-unknown-linux-musl" "$(nixpacks_target x86_64)"
assert_eq "arm64 target" "aarch64-unknown-linux-musl" "$(nixpacks_target aarch64)"
assert_eq "amd64 digest lookup" "$NIXPACKS_SHA256_AMD64" "$(nixpacks_sha256 x86_64)"
assert_eq "arm64 digest lookup" "$NIXPACKS_SHA256_ARM64" "$(nixpacks_sha256 aarch64)"
assert_fail "unknown arch has no target" nixpacks_target riscv64

T=$(mktemp -d)
quiet() { ( "$@" ) >/dev/null 2>&1; }
mkdir "$T/bin" "$T/src"
printf '#!/bin/sh\necho "nixpacks %s"\n' "$NIXPACKS_VERSION" > "$T/bin/nixpacks"
chmod +x "$T/bin/nixpacks"
PATH=$T/bin:$PATH
assert_eq "reads the pin" "$NIXPACKS_VERSION" "$(nixpacks_installed_version)"

# A fixture archive whose digest we set as the pin: real sha256sum.
mkdir "$T/archive"
printf '#!/bin/sh\necho nixpacks\n' > "$T/archive/nixpacks"
tar -C "$T/archive" -czf "$T/nixpacks.tar.gz" nixpacks
sum=$(sha256sum "$T/nixpacks.tar.gz" | awk '{print $1}')
NIXPACKS_SHA256_AMD64=$sum
NIXPACKS_PATH=$T/installed
curl() {
	while [ "$#" -gt 0 ]; do
		if [ "$1" = -o ]; then cp "$T/nixpacks.tar.gz" "$2"; return 0; fi
		shift
	done
	return 1
}
LOGFILE=/dev/null
assert_ok "installs the pin" install_nixpacks_pin >/dev/null
assert_ok "installs one executable" [ -x "$NIXPACKS_PATH" ]

# none → installs the pin.
rm -f "$T/bin/nixpacks"
NIXPACKS_PATH=$T/none
assert_ok "none: installs the pin" ensure_nixpacks >/dev/null
assert_ok "none: installs one executable" [ -x "$T/none" ]

# the pin already installed → nothing.
printf '#!/bin/sh\necho "nixpacks %s"\n' "$NIXPACKS_VERSION" > "$T/bin/nixpacks"
chmod +x "$T/bin/nixpacks"
NIXPACKS_PATH=$T/same
out=$(ensure_nixpacks 2>&1)
assert_contains "same: says present" "$out" "nixpacks $NIXPACKS_VERSION present"
assert_ok "same: installed nothing" [ ! -e "$NIXPACKS_PATH" ]

# another version, NIXPACKS_REPLACE=0 → left alone, and the note names the pin.
printf '#!/bin/sh\necho "nixpacks 1.2.3"\n' > "$T/bin/nixpacks"
chmod +x "$T/bin/nixpacks"
NIXPACKS_REPLACE=0
NIXPACKS_PATH=$T/kept
out=$(ensure_nixpacks 2>&1)
assert_contains "other: note names the pin" "$out" "$NIXPACKS_VERSION"
assert_ok "other: left alone" [ ! -e "$NIXPACKS_PATH" ]

# another version, NIXPACKS_REPLACE=1 → replaced.
NIXPACKS_REPLACE=1
NIXPACKS_PATH=$T/replaced
assert_ok "replace: installs the pin" ensure_nixpacks >/dev/null
assert_ok "replace: installs one executable" [ -x "$NIXPACKS_PATH" ]
NIXPACKS_REPLACE=0

# A wrong checksum fails and leaves nothing behind.
NIXPACKS_SHA256_AMD64=deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef
NIXPACKS_PATH=$T/bad
assert_fail "wrong checksum fails" quiet install_nixpacks_pin
assert_ok "wrong checksum installs nothing" [ ! -e "$NIXPACKS_PATH" ]
NIXPACKS_SHA256_AMD64=$sum

# Upstream's script is gone for good.
assert_fail "no pipe to bash" grep -F '| bash' "$here/../bootstrap.sh"
assert_fail "no upstream install script" grep -F 'nixpacks.com/install.sh' "$here/../bootstrap.sh"

rm -rf "$T"
[ "${TESTS_FAIL:-0}" -eq 0 ]
