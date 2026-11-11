#!/usr/bin/env sh
# Drives the real Azure Blob client against Azurite. Not run by CI.
#
#   sh deploy/test/integration_azure.sh
#
# Needs Docker, Go and openssl. It starts one mcr.microsoft.com/azure-storage/
# azurite:3.37.0 container (outhaul-aztest-*), on a loopback port only, with a
# throwaway certificate generated into a temp directory, and a container named
# "outhaul". It then runs TestAzureAzurite, which puts and gets a small object
# and one larger than the 8 MiB block boundary, lists them, deletes them, and
# checks a missing object is an error.
#
# What it shows: the requests and XML this client writes are accepted by an
# implementation we didn't write, including blocks and list pagination.
# What it cannot show: the instance metadata service, real token validation,
# Azure roles, the storage account's firewall, real service limits, or a
# refresh after a token expires. Those need the real host; the header of
# docs/STATE-BACKUP.md lists them.
#
# The container and temp directory are removed on exit, failure included.
# Prints one PASS or FAIL line per check and exits non-zero on failure.
set -eu

image=mcr.microsoft.com/azure-storage/azurite:3.37.0
container=outhaul-aztest-$$
account=devstoreaccount1
container_name=outhaul

if ! docker info >/dev/null 2>&1; then
	echo "BLOCKED: Docker is not usable here"
	exit 2
fi
if [ -n "$(docker ps -aq --filter name=outhaul-aztest)" ]; then
	echo "BLOCKED: outhaul-aztest-* containers already exist; remove them first:"
	docker ps -a --filter name=outhaul-aztest --format '  container {{.Names}}'
	exit 2
fi

work=$(mktemp -d)
cleanup() {
	docker rm -f "$container" >/dev/null 2>&1 || true
	rm -rf "$work"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
trap 'exit 129' HUP
trap 'exit 141' PIPE

passed=0
failed=0
pass() {
	passed=$((passed + 1))
	printf 'PASS  %s\n' "$1"
}
fail() {
	failed=$((failed + 1))
	printf 'FAIL  %s\n' "$1"
}
info() { printf 'INFO  %s\n' "$1"; }
finish() {
	printf '%d passed, %d failed\n' "$passed" "$failed"
	[ "$failed" -eq 0 ]
}
die() {
	printf 'ERROR %s\n' "$1"
	docker logs "$container" 2>&1 | tail -30 || true
	exit 1
}

root=$(cd "$(dirname "$0")/../.." && pwd)

echo "== certificate"
openssl req -x509 -newkey rsa:2048 -nodes -days 1 \
	-subj /CN=127.0.0.1 \
	-keyout "$work/key.pem" -out "$work/cert.pem" >/dev/null 2>&1 ||
	die "openssl could not make the throwaway certificate"
chmod 644 "$work/cert.pem" "$work/key.pem"
info "a throwaway certificate for 127.0.0.1, in $work"

echo "== azurite"
docker run -d --name "$container" \
	-p 127.0.0.1::10000 \
	-v "$work:/certs:ro" \
	"$image" azurite-blob --oauth basic \
	--cert /certs/cert.pem --key /certs/key.pem \
	--blobHost 0.0.0.0 --blobPort 10000 >/dev/null ||
	die "docker run failed"

port=""
i=0
while [ "$i" -lt 60 ]; do
	port=$(docker port "$container" 10000/tcp 2>/dev/null | sed -n 's/.*://p' | head -n 1)
	if [ -n "$port" ] && curl -k -s -o /dev/null "https://127.0.0.1:$port/"; then
		break
	fi
	if [ "$(docker inspect -f '{{.State.Running}}' "$container" 2>/dev/null)" != "true" ]; then
		die "Azurite stopped before answering"
	fi
	port=""
	i=$((i + 1))
	sleep 1
done
[ -n "$port" ] || die "Azurite did not answer on its loopback port"
pass "Azurite answers on 127.0.0.1:$port"

# Azurite's --oauth basic checks the claims' shape, not a signature. "Bearer
# azurite" is refused with 403 AuthenticationFailed, so mint the unsigned JWT
# the service accepts.
b64url() { openssl base64 -A | tr '+/' '-_' | tr -d '='; }
now=$(date +%s)
header=$(printf '%s' '{"alg":"none","typ":"JWT"}' | b64url)
payload=$(printf '{"aud":"https://storage.azure.com","iss":"https://sts.windows.net/example-tenant/","iat":%s,"nbf":%s,"exp":%s}' \
	"$now" "$now" "$((now + 3600))" | b64url)
token="$header.$payload."

echo "== container"
code=$(curl -k -s -o "$work/create.out" -w '%{http_code}' -X PUT \
	"https://127.0.0.1:$port/$account/$container_name?restype=container" \
	-H "Authorization: Bearer $token" \
	-H "x-ms-version: 2023-11-03" \
	-H "x-ms-date: $(date -u '+%a, %d %b %Y %H:%M:%S GMT')")
if [ "$code" = "201" ]; then
	pass "created the '$container_name' container"
else
	fail "creating the '$container_name' container answered HTTP $code"
	cat "$work/create.out" 2>/dev/null || true
fi

echo "== client"
if (cd "$root" && OUTHAUL_AZURE_TEST_ENDPOINT="https://127.0.0.1:$port/$account" \
	go test ./internal/blobstore -run '^TestAzureAzurite$' -count=1); then
	pass "TestAzureAzurite"
else
	fail "TestAzureAzurite"
fi

finish