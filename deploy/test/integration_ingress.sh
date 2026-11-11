#!/usr/bin/env sh
# Proves DNS-01 certificates, the wildcard and HTTPS-only ports on a real
# Traefik. Not run by CI.
#
#   sh deploy/test/integration_ingress.sh
#
# Needs Docker and Go. It builds deploy/test/proxyflags, deploy/test/gatelabels
# and deploy/test/dnsadapter, then starts on one scratch network:
#   - pebble (Let's Encrypt's test ACME server) with PEBBLE_VA_NOSLEEP=1 and
#     -dnsserver pointing at pebble-challtestsrv;
#   - pebble-challtestsrv as the DNS the wildcard's TXT record lives in;
#   - dnsadapter on the host, answering lego's httpreq provider and forwarding
#     to challtestsrv. HTTPREQ_ENDPOINT reaches it as host.docker.internal, and
#     is passed to Traefik by name through OUTHAUL_ACME_DNS_ENV's path.
#   - a Traefik started from the flags and environment deploy/test/proxyflags
#     prints, which are traefik.ProxyFlags' (proxySpec's own). The script adds
#     what points it at Pebble and keeps its propagation check out of the way:
#       --certificatesresolvers.le.acme.caserver=https://pebble:14000/dir
#       --certificatesresolvers.le.acme.dnschallenge.resolvers=challtestsrv:8053
#       --certificatesresolvers.le.acme.dnschallenge.disablepropagationcheck=true
#       SSL_CERT_FILE=/etc/pebble/pebble.minica.pem (Pebble's root, mounted)
#     Outhaul gets no setting for any of these.
#   - echo backends labelled by deploy/test/gatelabels, so the labels are the
#     real traefik.RouteLabels output. They carry traefik.enable=true and
#     outhaul.ingtest=true; Traefik is constrained to the latter.
#
# It checks that one wildcard certificate is issued, that three hosts under it
# cause no more orders, that a host outside it gets its own certificate, that
# an HTTPS-only Traefik publishes only 443, that a request under the wildcard
# is answered with the wildcard certificate, and reports (INFO) what happens
# when the wildcard is turned on after a host already has its own certificate.
#
# Every container and the network are named outhaul-ingtest-*, on loopback
# ports only, and are removed on exit, failure included. Prints PASS, FAIL and
# INFO lines, and exits non-zero if any assertion fails.
set -u

root=$(cd "$(dirname "$0")/../.." && pwd)
net=outhaul-ingtest-net
traefik_image=${INGTEST_TRAEFIK_IMAGE:-traefik:v3.7.6}
pebble_image=${INGTEST_PEBBLE_IMAGE:-ghcr.io/letsencrypt/pebble:latest}
chall_image=${INGTEST_CHALLTESTSRV_IMAGE:-ghcr.io/letsencrypt/pebble-challtestsrv:latest}
python_image=python:3-slim
domain=shared.demo.example.com
wildcard="*.$domain"
outside=other.example.org
docker_sock=${INGTEST_DOCKER_SOCK:-/var/run/docker.sock}

if ! docker info >/dev/null 2>&1; then
	echo "BLOCKED: Docker is not usable here"
	exit 2
fi
if [ -n "$(docker ps -aq --filter name=outhaul-ingtest)" ] ||
	[ -n "$(docker network ls -q --filter name=outhaul-ingtest)" ]; then
	echo "BLOCKED: outhaul-ingtest-* containers or networks already exist; remove them first:"
	docker ps -a --filter name=outhaul-ingtest --format '  container {{.Names}}'
	docker network ls --filter name=outhaul-ingtest --format '  network {{.Name}}'
	exit 2
fi

work=$(mktemp -d)
dns_pid=
cleanup() {
	if [ -n "$dns_pid" ]; then kill "$dns_pid" >/dev/null 2>&1 || true; fi
	docker rm -f outhaul-ingtest-traefik outhaul-ingtest-traefik-http-off outhaul-ingtest-traefik-nowild \
		outhaul-ingtest-app1 outhaul-ingtest-app2 outhaul-ingtest-app3 outhaul-ingtest-outside \
		outhaul-ingtest-app-off outhaul-ingtest-app-nowild \
		outhaul-ingtest-pebble outhaul-ingtest-chall >/dev/null 2>&1 || true
	docker network rm "$net" >/dev/null 2>&1 || true
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
	shift
	for line in "$@"; do printf '        %s\n' "$line"; done
}
info() { printf 'INFO  %s\n' "$1"; }
finish() {
	printf '%d passed, %d failed\n' "$passed" "$failed"
	[ "$failed" -eq 0 ]
}
die() {
	printf 'ERROR %s\n' "$1"
	if docker inspect outhaul-ingtest-traefik >/dev/null 2>&1; then
		echo "Traefik log:"
		docker logs outhaul-ingtest-traefik 2>&1 | tail -60
	fi
	if docker inspect outhaul-ingtest-pebble >/dev/null 2>&1; then
		echo "Pebble log:"
		docker logs outhaul-ingtest-pebble 2>&1 | tail -40
	fi
	exit 1
}

echo "== setup"
(cd "$root" && go build -o "$work/proxyflags" ./deploy/test/proxyflags) || die "go build proxyflags"
(cd "$root" && go build -o "$work/gatelabels" ./deploy/test/gatelabels) || die "go build gatelabels"
(cd "$root" && go build -o "$work/dnsadapter" ./deploy/test/dnsadapter) || die "go build dnsadapter"

# Pebble's root, so Traefik trusts its ACME endpoint (SAN pebble).
cid=$(docker create "$pebble_image") || die "create a pebble image container"
docker cp "$cid:/test/certs/pebble.minica.pem" "$work/pebble-root.pem" >/dev/null || die "copy Pebble's root"
docker rm "$cid" >/dev/null || true
mkdir -p "$work/acme" "$work/acme-http-off" "$work/acme-nowild"
touch "$work/acme/acme.json" "$work/acme-http-off/acme.json" "$work/acme-nowild/acme.json"
chmod 600 "$work/acme/acme.json" "$work/acme-http-off/acme.json" "$work/acme-nowild/acme.json"

docker network create "$net" >/dev/null || die "create network $net"
docker run -d --name outhaul-ingtest-chall --network "$net" --network-alias challtestsrv \
	-p 127.0.0.1::8055 "$chall_image" >/dev/null || die "start challtestsrv"
docker run -d --name outhaul-ingtest-pebble --network "$net" --network-alias pebble \
	-e PEBBLE_VA_NOSLEEP=1 -p 127.0.0.1::14000 -p 127.0.0.1::15000 \
	"$pebble_image" -config test/config/pebble-config.json -dnsserver challtestsrv:8053 >/dev/null || die "start pebble"
chall_port=$(docker port outhaul-ingtest-chall 8055/tcp | sed -n 's/^127\.0\.0\.1:\([0-9][0-9]*\)$/\1/p' | head -1)
[ -n "$chall_port" ] || die "find challtestsrv's management port"
"$work/dnsadapter" -listen :18080 -challtestsrv "http://127.0.0.1:$chall_port" >"$work/dnsadapter.log" 2>&1 &
dns_pid=$!
sleep 1
if ! kill -0 "$dns_pid" >/dev/null 2>&1; then
	die "dnsadapter did not start: $(cat "$work/dnsadapter.log")"
fi

# Wait for Pebble to answer its directory.
pebble_port=$(docker port outhaul-ingtest-pebble 14000/tcp | sed -n 's/^127\.0\.0\.1:\([0-9][0-9]*\)$/\1/p' | head -1)
[ -n "$pebble_port" ] || die "find Pebble's directory port"
i=0
while [ $i -lt 30 ]; do
	if curl -sk --max-time 2 "https://127.0.0.1:$pebble_port/dir" >/dev/null 2>&1; then break; fi
	sleep 1
	i=$((i + 1))
done
[ $i -lt 30 ] || die "Pebble never answered"

echo_py=$(
	cat <<'EOF'
import http.server, os
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        body = ("backend: " + os.environ.get("INGTEST_NAME", "app") + "\npath: " + self.path + "\n").encode()
        self.send_response(200)
        self.send_header("Content-Type", "text/plain")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)
    def log_message(self, *args):
        pass
http.server.ThreadingHTTPServer(("", 8000), H).serve_forever()
EOF
)

# start_backend NAME HOST [extra gatelabels flags...]
start_backend() {
	name=$1 host=$2
	shift 2
	"$work/gatelabels" -tls -resolver le -wildcard "$wildcard" -port 8000 "$@" \
		"$name,$host,,,open" >"$work/labels-$name" || die "gatelabels for $name"
	printf 'traefik.enable=true\nouthaul.ingtest=true\n' >>"$work/labels-$name"
	docker run -d --name "outhaul-ingtest-$name" --network "$net" --label-file "$work/labels-$name" \
		-e "INGTEST_NAME=$name" "$python_image" python -u -c "$echo_py" >/dev/null || die "start backend $name"
}

# start_traefik NAME ACMEDIR [proxyflags options...]
# Starts Traefik from the real flags/env, pointing ACME at Pebble.
start_traefik() {
	name=$1 acme_dir=$2
	shift 2
	# shellcheck disable=SC2046 # one word per line
	set -- "$@" -dns-provider httpreq -dns-env "HTTPREQ_ENDPOINT=http://host.docker.internal:18080"
	"$work/proxyflags" "$@" cmd >"$work/cmd-$name" || die "proxyflags cmd"
	"$work/proxyflags" "$@" env >"$work/env-$name" || die "proxyflags env"
	env_flags=$(sed 's/^/-e /' "$work/env-$name" | tr '\n' ' ')
	publish80="-p 127.0.0.1::80"
	case " $* " in *" -http-off "*) publish80= ;; esac
	# shellcheck disable=SC2046
	docker run -d --name "$name" --network "$net" \
		--add-host host.docker.internal:host-gateway \
		$publish80 -p 127.0.0.1::443 -p 127.0.0.1::8080 \
		-v "$docker_sock:/var/run/docker.sock:ro" \
		-v "$acme_dir:/etc/traefik/acme" \
		-v "$work/pebble-root.pem:/etc/pebble/pebble.minica.pem:ro" \
		-e SSL_CERT_FILE=/etc/pebble/pebble.minica.pem \
		$env_flags \
		"$traefik_image" \
		$(cat "$work/cmd-$name") \
		"--providers.docker.network=$net" \
		'--providers.docker.constraints=Label(`outhaul.ingtest`,`true`)' \
		--api.insecure=true \
		--certificatesresolvers.le.acme.caserver=https://pebble:14000/dir \
		--certificatesresolvers.le.acme.dnschallenge.resolvers=challtestsrv:8053 \
		--certificatesresolvers.le.acme.dnschallenge.disablepropagationcheck=true \
		--log.level=INFO --log.nocolor=true >/dev/null || die "start $name"
}

host_port() {
	docker port "$1" "$2/tcp" | sed -n 's/^127\.0\.0\.1:\([0-9][0-9]*\)$/\1/p' | head -1
}
request() { # SCHEME HOST PATH [curl args...] -> status code; body in $work/b
	scheme=$1 host=$2 path=$3
	shift 3
	case $scheme in
	https) port=$https_port ;;
	*) port=$http_port ;;
	esac
	curl -sS -k --max-time 20 -o "$work/b" -w '%{http_code}' \
		--connect-to "::127.0.0.1:$port" "$scheme://$host$path" "$@" 2>"$work/err" || true
}
wait_for_cert() { # FILE COUNT SECONDS -> waits for the ACME store to hold COUNT certs
	i=0
	while [ $i -lt "$3" ]; do
		[ "$(cert_count "$1")" -ge "$2" ] && return 0
		sleep 1
		i=$((i + 1))
	done
	return 1
}
wait_for() { # SCHEME HOST SECONDS -> waits for a 200
	scheme=$1 host=$2 limit=$3
	i=0
	while [ $i -lt "$limit" ]; do
		[ "$(request "$scheme" "$host" /)" = 200 ] && return 0
		sleep 1
		i=$((i + 1))
	done
	return 1
}
cert_count() {
	python3 - "$1" <<'PY'
import json, sys
try:
    d = json.load(open(sys.argv[1]))
except Exception:
    print(0); raise SystemExit
certs = d.get("le", {}).get("Certificates") or []
print(len(certs))
PY
}
cert_names() {
	python3 - "$1" <<'PY'
import json, sys
d = json.load(open(sys.argv[1]))
for c in d.get("le", {}).get("Certificates") or []:
    print(c.get("domain", {}).get("main", ""))
PY
}
order_count() {
	docker logs outhaul-ingtest-pebble 2>&1 | grep -c 'Added order'
}
san() { # HOST -> the certificate a request for HOST is served with
	openssl s_client -connect "127.0.0.1:$https_port" -servername "$1" </dev/null 2>/dev/null |
		openssl x509 -noout -ext subjectAltName 2>/dev/null | tr -d ' \n'
}

echo "== the wildcard is issued once"
start_traefik outhaul-ingtest-traefik "$work/acme" -wildcard "$wildcard"
http_port=$(host_port outhaul-ingtest-traefik 80)
https_port=$(host_port outhaul-ingtest-traefik 443)
[ -n "$http_port" ] && [ -n "$https_port" ] || die "find Traefik's loopback ports"
start_backend app1 "app1.$domain"
wait_for https "app1.$domain" 60 || die "app1 under the wildcard never answered 200; Traefik log: $(docker logs outhaul-ingtest-traefik 2>&1 | tail -20)"
wait_for_cert "$work/acme/acme.json" 1 60 || die "the wildcard was never issued"
[ "$(cert_count "$work/acme/acme.json")" = 1 ] &&
	[ "$(cert_names "$work/acme/acme.json")" = "$wildcard" ] &&
	pass "acme.json holds one certificate, for $wildcard" ||
	fail "acme.json holds one certificate, for $wildcard" "certificates: $(cert_names "$work/acme/acme.json")"
first_orders=$(order_count)
[ "$first_orders" = 1 ] && pass "Pebble saw one order for the wildcard" ||
	fail "Pebble saw one order for the wildcard" "orders: $first_orders"
echo

echo "== hosts under the wildcard cause no more orders"
start_backend app2 "app2.$domain"
start_backend app3 "app3.$domain"
wait_for https "app2.$domain" 30 && wait_for https "app3.$domain" 30 ||
	die "app2 or app3 under the wildcard never answered 200"
now_orders=$(order_count)
[ "$now_orders" = "$first_orders" ] && pass "two more hosts under the wildcard caused no further order" ||
	fail "two more hosts under the wildcard caused no further order" "orders went from $first_orders to $now_orders"
[ "$(cert_count "$work/acme/acme.json")" = 1 ] && pass "acme.json still holds only the wildcard" ||
	fail "acme.json still holds only the wildcard" "certificates: $(cert_names "$work/acme/acme.json")"
echo

echo "== a host outside the wildcard gets its own certificate"
start_backend outside "$outside"
wait_for https "$outside" 60 || die "$outside never answered 200"
wait_for_cert "$work/acme/acme.json" 2 60 || die "$outside never got its own certificate"
[ "$(order_count)" = "$((first_orders + 1))" ] &&
	pass "a host outside the wildcard caused one more order" ||
	fail "a host outside the wildcard caused one more order" "orders: $(order_count), before: $first_orders"
names=$(cert_names "$work/acme/acme.json" | tr '\n' ' ')
case " $names " in *" $outside "*) pass "acme.json now holds a certificate for $outside" ;; *) fail "acme.json now holds a certificate for $outside" "certificates: $names" ;; esac
echo

echo "== a request under the wildcard is served the wildcard certificate"
got=$(san "app1.$domain")
case $got in
*san:DNS:*.shared.demo.example.com* | *"DNS:*.shared.demo.example.com"*) pass "app1.$domain is served a certificate whose SAN is the wildcard" ;;
*) fail "app1.$domain is served a certificate whose SAN is the wildcard" "SAN: $got" ;;
esac
echo

echo "== HTTPS only publishes no port 80"
docker rm -f outhaul-ingtest-traefik outhaul-ingtest-app1 outhaul-ingtest-app2 outhaul-ingtest-app3 outhaul-ingtest-outside >/dev/null 2>&1
start_backend app-off "app1.$domain" -http-off
start_traefik outhaul-ingtest-traefik-http-off "$work/acme-http-off" -http-off -wildcard "$wildcard"
http_off_c=outhaul-ingtest-traefik-http-off
https_port=$(host_port "$http_off_c" 443)
[ -n "$https_port" ] || die "find the HTTPS-only Traefik's HTTPS port"
[ -z "$(host_port "$http_off_c" 80)" ] && pass "the HTTPS-only Traefik publishes no port 80" ||
	fail "the HTTPS-only Traefik publishes no port 80" "docker port: $(docker port "$http_off_c")"
wait_for https "app1.$domain" 60 || die "the HTTPS-only Traefik never served app1"
wait_for_cert "$work/acme-http-off/acme.json" 1 60 || die "the HTTPS-only Traefik never obtained a certificate"
if docker logs "$http_off_c" 2>&1 | grep -q "EntryPoint doesn't exist"; then
	fail "the HTTPS-only Traefik logs no 'EntryPoint doesn't exist'" "$(docker logs "$http_off_c" 2>&1 | grep "EntryPoint doesn't exist" | head -2)"
else
	pass "the HTTPS-only Traefik logs no 'EntryPoint doesn't exist'"
fi
docker rm -f "$http_off_c" outhaul-ingtest-app-off >/dev/null 2>&1
echo

echo "== turning the wildcard on after a host has its own certificate"
start_backend app-nowild "app1.$domain" -wildcard ""
start_traefik outhaul-ingtest-traefik-nowild "$work/acme-nowild"
https_port=$(host_port outhaul-ingtest-traefik-nowild 443)
[ -n "$https_port" ] || die "find the no-wildcard Traefik's HTTPS port"
wait_for https "app1.$domain" 60 || die "the no-wildcard Traefik never served app1"
wait_for_cert "$work/acme-nowild/acme.json" 1 60 || die "the no-wildcard Traefik never obtained app1's certificate"
sleep 2
before=$(san "app1.$domain")
docker rm -f outhaul-ingtest-traefik-nowild outhaul-ingtest-app-nowild >/dev/null 2>&1
start_backend app-nowild "app1.$domain"
start_traefik outhaul-ingtest-traefik-nowild "$work/acme-nowild" -wildcard "$wildcard"
https_port=$(host_port outhaul-ingtest-traefik-nowild 443)
[ -n "$https_port" ] || die "find the wildcard Traefik's HTTPS port"
wait_for https "app1.$domain" 60 || die "the wildcard Traefik never served app1"
after=$(san "app1.$domain")
info "with its own certificate first: $before"
info "after restarting with the wildcard on the same acme.json: $after"
docker rm -f outhaul-ingtest-traefik-nowild >/dev/null 2>&1
echo

finish
