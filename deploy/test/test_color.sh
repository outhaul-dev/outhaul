#!/usr/bin/env sh
here=$(cd "$(dirname "$0")" && pwd)
. "$here/assert.sh"
OUTHAUL_INSTALLER_LIB=1 . "$here/../bootstrap.sh"

# args: no_color is_tty colorterm tput_colors
assert_eq "NO_COLOR forces plain" "0" "$(_color_level 1 1 truecolor 256)"
assert_eq "non-tty forces plain"  "0" "$(_color_level '' 0 truecolor 256)"
assert_eq "truecolor -> 3"        "3" "$(_color_level '' 1 truecolor 256)"
assert_eq "256 -> 2"              "2" "$(_color_level '' 1 '' 256)"
assert_eq "8 -> 1"                "1" "$(_color_level '' 1 '' 8)"
assert_eq "none -> 0"             "0" "$(_color_level '' 1 '' 0)"

[ "${TESTS_FAIL:-0}" -eq 0 ]
