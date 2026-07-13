#!/usr/bin/env sh
here=$(cd "$(dirname "$0")" && pwd)
. "$here/assert.sh"
OUTHAUL_INSTALLER_LIB=1 . "$here/../bootstrap.sh"

# parse_args
parse_args --from-checkout /tmp/xyz --verbose
assert_eq "from-checkout flag" "1" "$FROM_CHECKOUT"
assert_eq "checkout dir"       "/tmp/xyz" "$CHECKOUT_DIR"
assert_eq "verbose flag"       "1" "$VERBOSE"

# choose_ingress mode c (input "3")
in=$(mktemp)
printf '3\n' > "$in"
choose_ingress 3<"$in" >/dev/null 2>&1
assert_eq "ingress c -> MODE c" "c" "$MODE"
assert_eq "ingress c no url"    "" "$PUBLIC_URL"

# choose_ingress mode b (input "2")
printf '2\n' > "$in"
choose_ingress 3<"$in" >/dev/null 2>&1
assert_eq "ingress b -> MODE b" "b" "$MODE"
rm -f "$in"

[ "${TESTS_FAIL:-0}" -eq 0 ]
