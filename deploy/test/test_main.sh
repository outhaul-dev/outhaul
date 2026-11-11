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

# git_ssh_from_port
assert_eq "git port -> ssh address" ":2222" "$(git_ssh_from_port 2222)"
assert_eq "no git port -> off" "off" "$(git_ssh_from_port '')"

# --- unattended flags -------------------------------------------------------
# check_args exits 2, which would end this file, so every case runs in a
# subshell; `if ( … )` keeps set -e from acting on the 2.
arg_rc() { if ( parse_args "$@"; check_args ) >/dev/null 2>&1; then echo 0; else echo $?; fi; }

assert_eq "answer flag needs --non-interactive" 2 "$(arg_rc --ingress local)"
assert_eq "missing ingress" 2 "$(arg_rc --non-interactive)"
assert_eq "letsencrypt needs a URL" 2 "$(arg_rc --non-interactive --ingress letsencrypt --acme-email a@example.com)"
assert_eq "letsencrypt needs an email" 2 "$(arg_rc --non-interactive --ingress letsencrypt --public-url https://o.example.com)"
assert_eq "local-ca refuses an email" 2 "$(arg_rc --non-interactive --ingress local-ca --acme-email a@example.com)"
assert_eq "ref with from-checkout" 2 "$(arg_rc --ref abc --from-checkout /tmp)"
assert_eq "bad git port" 2 "$(arg_rc --non-interactive --ingress local --git-ssh 70000)"
assert_eq "bad nixpacks" 2 "$(arg_rc --non-interactive --ingress local --nixpacks maybe)"
assert_eq "bad firewall" 2 "$(arg_rc --non-interactive --ingress local --firewall always)"
assert_eq "bad updates" 2 "$(arg_rc --non-interactive --ingress local --updates weekly)"
assert_eq "a missing value" 2 "$(arg_rc --ingress)"
assert_eq "an unknown option" 2 "$(arg_rc --bogus)"
assert_eq "the accepted command" 0 "$(arg_rc --non-interactive --ingress local --git-ssh off --nixpacks on --firewall leave --updates off)"

# An --env-file must be root's, absolute, regular and not group/other
# writable; its path must survive the drop-in's EnvironmentFile= line.
T=$(mktemp -d)
: >"$T/env"
stat() { echo '0 600'; }
assert_eq "empty env file value" 2 "$(arg_rc --non-interactive --ingress local --env-file '')"
assert_eq "relative env file" 2 "$(arg_rc --non-interactive --ingress local --env-file rel.env)"
assert_eq "missing env file" 2 "$(arg_rc --non-interactive --ingress local --env-file /no/such/file)"
stat() { echo '1000 600'; }
assert_eq "not root's env file" 2 "$(arg_rc --non-interactive --ingress local --env-file "$T/env")"
stat() { echo '0 666'; }
assert_eq "writable env file" 2 "$(arg_rc --non-interactive --ingress local --env-file "$T/env")"
stat() { echo '0 600'; }
: >"$T/a\"b"
: >"$T/a%b"
: >"$T/a
b"
assert_eq "quote in an env path" 2 "$(arg_rc --non-interactive --ingress local --env-file "$T/a\"b")"
assert_eq "percent in an env path" 2 "$(arg_rc --non-interactive --ingress local --env-file "$T/a%b")"
assert_eq "newline in an env path" 2 "$(arg_rc --non-interactive --ingress local --env-file "$T/a
b")"
mkdir -p "$T/with space"
: >"$T/with space/env"
assert_eq "a space is allowed" 0 "$(arg_rc --non-interactive --ingress local --env-file "$T/with space/env")"

# proxy also needs an ACME DNS provider line in one of the env files.
printf 'OUTHAUL_ACME_DNS_PROVIDER=azuredns\n' >"$T/env"
stat() { echo '0 600'; }
assert_eq "proxy without a provider" 2 "$(arg_rc --non-interactive --ingress proxy --public-url https://o.example.com --acme-email a@example.com)"
assert_eq "proxy with a provider" 0 "$(arg_rc --non-interactive --ingress proxy --public-url https://o.example.com --acme-email a@example.com --env-file "$T/env")"

# The env files keep their order; --ref wins over OUTHAUL_REF.
parse_args --env-file /a --env-file /b
assert_eq "env files in order" "/a
/b" "$(printf '%s' "$ENV_FILES")"
# shellcheck disable=SC2034 # OUTHAUL_REF is read by main, not by this snippet
assert_eq "flag beats OUTHAUL_REF" from-flag \
	"$( OUTHAUL_REF=from-env REF=from-env; parse_args --ref from-flag; printf '%s\n' "$REF" )"

# A malformed ENV_FILES (one blank line, as an empty --env-file once left it)
# must not write a body of only [Service]: that drops install.env and so
# re-enables the :2222 listener on a host installed with --git-ssh off.
SERVICE_DROPIN=$T/malformed/outhaul.service.d/10-install.conf
UPDATE_DROPIN=$T/malformed/outhaul-update.service.d/10-install.conf
mkdir -p "$(dirname "$SERVICE_DROPIN")" "$(dirname "$UPDATE_DROPIN")"
printf 'sentinel\n' >"$SERVICE_DROPIN"
printf 'sentinel\n' >"$UPDATE_DROPIN"
# shellcheck disable=SC2034 # read by write_dropins, sourced from bootstrap.sh
ENV_FILES='
'
NONINTERACTIVE=0 write_dropins
assert_eq "a blank env file keeps the service drop-in" sentinel "$(cat "$SERVICE_DROPIN")"
assert_eq "a blank env file keeps the update drop-in" sentinel "$(cat "$UPDATE_DROPIN")"

rm -rf "$T"

[ "${TESTS_FAIL:-0}" -eq 0 ]
