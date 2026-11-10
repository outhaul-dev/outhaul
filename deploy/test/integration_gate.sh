#!/usr/bin/env sh
# Proves the protected-route labels on a real Traefik. Not run by CI.
#
#   sh deploy/test/integration_gate.sh
#   sh deploy/test/integration_gate.sh --outhaul HOST:PORT
#
# The first form puts a stub in place of Outhaul. The second uses a running
# Outhaul's /gate/check instead; see "With a running Outhaul" below.
#
# Needs Docker and Go. It starts, on one scratch network:
#   - an echo backend carrying the labels traefik.RouteLabels gives four rows
#     (printed by deploy/test/gatelabels, so this covers the real label code),
#     each with an HTTPS router when traefik.AppLabels would give it one on a
#     TLS server:
#       wiki.test             protected
#       wiki.test/webhooks    open        (an open path under a protected host)
#       shop.test             open
#       shop.test/admin       protected, stripped to /, its own TLS flag off
#                             (a protected path under an open host)
#   - a gate stub standing in for Outhaul's /gate/check, on the admin port, so
#     the labels use the address config.GateAddress really gives. Traefik finds
#     it as host.docker.internal through an /etc/hosts entry (--add-host), the
#     way Outhaul's Traefik finds the host gateway. It answers 200 with
#     X-Outhaul-User: dana when the request carries the cookie gatetest=ok, and
#     otherwise a 302 with a Set-Cookie. It logs every header it receives.
#   - a second stub holding the network alias host.docker.internal, answering
#     200 with X-Outhaul-User: eve. The gate checks must reach the first one.
#   - two more echo backends for the priority checks: "new", with the labels
#     Outhaul writes now, and "old", with labels written by hand the way an
#     Outhaul before the traversal term wrote them (no term, no priority). On
#     mix1.test the old one holds /api and the new one the host; on mix2.test
#     the reverse. "old" also has probe routers on re.test, which show whether
#     PathRegexp sees a path escaped or decoded.
#   - a container Compose creates from the override gatelabels -compose
#     prints, never started, whose labels are compared with the ones above.
#   - four Traefiks (GATETEST_TRAEFIK_IMAGE, default traefik:v3.7.6), on
#     loopback ports only:
#       main      web and websecure side by side, with no redirect, so each
#                 row's HTTP and HTTPS routers are both checked. Outhaul runs
#                 web alone (no TLS, or the tunnel), or web redirecting to
#                 websecure;
#       trusting  like main, but its entrypoints trust a client's X-Forwarded-*
#                 headers. Outhaul's don't, and that alone hides a gate that
#                 trusts them, so the X-Forwarded-* checks also run here;
#       redirect  web redirecting to websecure, with the flags Outhaul's
#                 Traefik uses under TLS;
#       unmapped  like main, without the /etc/hosts entry: the control showing
#                 that the second stub answers when nothing maps the name.
#
# The Traefiks expose containers by default, and see only those labelled
# outhaul.gatetest=true. No test container carries traefik.enable, so a Traefik
# that exposes only containers labelled traefik.enable=true (Outhaul's own)
# ignores them all.
#
# Paths are sent exactly as written (curl --path-as-is) to check that no row's
# router takes a path with a ".." segment in any spelling, in both directions
# (an open path under a protected host, a protected path under an open one),
# and that paths which only look like one still get through.
#
# Every container and the networks are named outhaul-gatetest-*, and are
# removed on exit, failure included. Prints one PASS or FAIL line per
# assertion, and INFO lines for what a run finds out, and exits non-zero if
# any assertion fails.
#
# With a running Outhaul (--outhaul HOST:PORT)
#
# HOST is host-gateway (the Docker host, which is how Outhaul's own Traefik
# reaches Outhaul) or an IPv4 address, and PORT is Outhaul's admin port (the
# port of OUTHAUL_LISTEN_ADDR, 8080 by default). Only the main Traefik and the
# echo backend start, with no stubs: host.docker.internal is mapped to HOST,
# and the labels use config.GateAddress for PORT. The test's containers are on
# their own network, so a firewall that lets only Outhaul's network reach the
# admin port (as the installer sets up) keeps them out; the script stops and
# says so. Run it where the Docker bridge can reach the admin port, such as a
# development machine.
#
# Outhaul needs a public URL. None of wiki.test or shop.test has to be one of
# its domains: the gate decides from cookies alone. Optional settings:
#   GATETEST_PUBLIC_URL    Outhaul's public URL. With it, the sign-in redirect
#                          must point there; without it, only its path is
#                          checked.
#   GATETEST_ROUTE_HOST    A host with a protected domain row in that Outhaul,
#   GATETEST_ROUTE_COOKIE  and the value of the __Host-outhaul_route cookie a
#                          browser got there after signing in. The backend also
#                          serves that host, as a protected row, and the checks
#                          that need someone signed in use the cookie. Without
#                          them those checks print SKIP.
# It checks, over HTTP and HTTPS, that a page load with no cookie is sent to
# /gate/start on the public URL; that a POST, a JSON request, a CORS preflight
# and a WebSocket handshake get 401; that //evil.example/ stays on the host;
# and, with the cookie, that it passes and works on no other host. Last, if
# it runs in a terminal, it asks you to stop Outhaul and checks that protected
# paths then answer 500 and open ones 200.
set -u

usage() {
	echo "usage: sh deploy/test/integration_gate.sh [--outhaul HOST:PORT]" >&2
	echo "  HOST is host-gateway or an IPv4 address; PORT is Outhaul's admin port" >&2
	exit 2
}
mode=stub
case $# in
0) ;;
2)
	[ "$1" = --outhaul ] || usage
	mode=outhaul
	outhaul_addr=$2
	case $outhaul_addr in *:*) ;; *) usage ;; esac
	outhaul_host=${outhaul_addr%:*}
	outhaul_port=${outhaul_addr##*:}
	case $outhaul_host in
	host-gateway) ;;
	*[!0-9.]* | *..* | .* | *.) usage ;;
	*.*.*.*) ;;
	*) usage ;;
	esac
	case $outhaul_port in
	'' | *[!0-9]*) usage ;;
	esac
	;;
*) usage ;;
esac
public_url=${GATETEST_PUBLIC_URL:-}
route_host=${GATETEST_ROUTE_HOST:-}
route_cookie=${GATETEST_ROUTE_COOKIE:-}
if [ "$mode" = outhaul ]; then
	if [ -n "$route_host$route_cookie" ] && { [ -z "$route_host" ] || [ -z "$route_cookie" ]; }; then
		echo "GATETEST_ROUTE_HOST and GATETEST_ROUTE_COOKIE are set together, or not at all" >&2
		exit 2
	fi
	case $route_host in
	*[!a-z0-9.-]*)
		echo "GATETEST_ROUTE_HOST: a host name in lower case, such as wiki.example.com" >&2
		exit 2
		;;
	esac
	case $route_cookie in
	*[!0-9a-f]*)
		echo "GATETEST_ROUTE_COOKIE: the cookie's value only, 64 hex characters" >&2
		exit 2
		;;
	esac
	case $route_host in
	wiki.test | shop.test)
		echo "GATETEST_ROUTE_HOST: use a host other than wiki.test and shop.test" >&2
		exit 2
		;;
	esac
fi

root=$(cd "$(dirname "$0")/../.." && pwd)
net=outhaul-gatetest-net
traefik_c=outhaul-gatetest-traefik
trusting_c=outhaul-gatetest-traefik-trusting
redirect_c=outhaul-gatetest-traefik-redirect
unmapped_c=outhaul-gatetest-traefik-unmapped
app_c=outhaul-gatetest-app
old_c=outhaul-gatetest-old
new_c=outhaul-gatetest-new
gate_c=outhaul-gatetest-gate
alias_c=outhaul-gatetest-alias
probe_c=outhaul-gatetest-probe
compose_c=outhaul-gatetest-compose-web-1
traefik_image=${GATETEST_TRAEFIK_IMAGE:-traefik:v3.7.6}
python_image=python:3-slim
admin_port=8080
if [ "$mode" = outhaul ]; then admin_port=$outhaul_port; fi
docker_sock=${GATETEST_DOCKER_SOCK:-/var/run/docker.sock}

if ! docker info >/dev/null 2>&1; then
	echo "BLOCKED: Docker is not usable here"
	exit 2
fi
if [ -n "$(docker ps -aq --filter name=outhaul-gatetest)" ] ||
	[ -n "$(docker network ls -q --filter name=outhaul-gatetest)" ]; then
	echo "BLOCKED: outhaul-gatetest-* containers or networks already exist; remove them first:"
	docker ps -a --filter name=outhaul-gatetest --format '  container {{.Names}}'
	docker network ls --filter name=outhaul-gatetest --format '  network {{.Name}}'
	exit 2
fi

work=$(mktemp -d)
cleanup() {
	docker rm -f "$traefik_c" "$trusting_c" "$redirect_c" "$unmapped_c" "$app_c" "$old_c" "$new_c" "$gate_c" "$alias_c" "$probe_c" "$compose_c" >/dev/null 2>&1 || true
	docker network rm outhaul-gatetest-compose_default >/dev/null 2>&1 || true
	docker network rm "$net" >/dev/null 2>&1 || true
	rm -rf "$work"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
# Output piped to a reader that has gone (| head) would otherwise end the
# script without the EXIT trap, leaving the containers behind.
trap 'exit 129' HUP
trap 'exit 141' PIPE

passed=0
failed=0
skipped=0
pass() {
	passed=$((passed + 1))
	printf 'PASS  %s\n' "$1"
}
skip() {
	skipped=$((skipped + 1))
	printf 'SKIP  %s\n' "$1"
}
info() { # what a run found out, without passing or failing
	printf 'INFO  %s\n' "$1"
}
finish() {
	if [ "$skipped" -gt 0 ]; then
		printf '%d passed, %d failed, %d skipped\n' "$passed" "$failed" "$skipped"
	else
		printf '%d passed, %d failed\n' "$passed" "$failed"
	fi
	[ "$failed" -eq 0 ]
}
fail() {
	failed=$((failed + 1))
	printf 'FAIL  %s\n' "$1"
	shift
	for line in "$@"; do
		printf '        %s\n' "$line"
	done
}
die() {
	printf 'ERROR %s\n' "$1"
	if docker inspect "$traefik_c" >/dev/null 2>&1; then
		echo "Traefik log:"
		docker logs "$traefik_c" 2>&1 | tail -40
	fi
	exit 1
}

echo "== setup"
(cd "$root" && go build -o "$work/gatelabels" ./deploy/test/gatelabels) || die "go build gatelabels"
routes="gt-wiki,wiki.test,,,protected gt-hooks,wiki.test,/webhooks,,open gt-shop,shop.test,,,open gt-admin,shop.test,/admin,/,protected,notls"
if [ "$mode" = outhaul ] && [ -n "$route_host" ]; then
	routes="$routes gt-real,$route_host,,,protected"
fi
# shellcheck disable=SC2086 # one word per route
"$work/gatelabels" -tls -port 8000 -admin-port "$admin_port" $routes >"$work/labels" || die "gatelabels"
printf 'outhaul.gatetest=true\n' >>"$work/labels"
echo "labels on the backend:"
sed 's/^/  /' "$work/labels"
if [ "$mode" = stub ]; then
	# Two more backends, for the priority checks: "new" carries the labels
	# Outhaul writes now, and "old" the labels an Outhaul before the traversal
	# term wrote (no term, no priority label), written out here by hand. On
	# mix1.test the old one holds /api and the new one the host; on mix2.test
	# the reverse. "old" also carries two probe routers on re.test, which show
	# whether PathRegexp sees a path escaped or decoded.
	"$work/gatelabels" -port 8000 'gt-mixnew-host,mix1.test,,,open' 'gt-mixnew-api,mix2.test,/api,,open' >"$work/labels-new" ||
		die "gatelabels"
	printf 'outhaul.gatetest=true\n' >>"$work/labels-new"
	old_router() { # name rule
		printf 'traefik.http.routers.%s.rule=%s\n' "$1" "$2"
		printf 'traefik.http.routers.%s.entrypoints=web\n' "$1"
		printf 'traefik.http.routers.%s.service=%s\n' "$1" "$1"
		printf 'traefik.http.services.%s.loadbalancer.server.port=8000\n' "$1"
	}
	{
		old_router gt-mixold-api 'Host(`mix1.test`) && PathPrefix(`/api`)'
		old_router gt-mixold-host 'Host(`mix2.test`)'
		old_router gt-re-raw 'Host(`re.test`) && PathRegexp(`(?i)%2f`)'
		old_router gt-re-decoded 'Host(`re.test`) && PathRegexp(`^/a/b$`)'
		old_router gt-re-host 'Host(`re.test`)'
		printf 'traefik.http.routers.gt-re-raw.priority=200\n'
		printf 'traefik.http.routers.gt-re-raw.middlewares=gt-re-raw-h\n'
		printf 'traefik.http.middlewares.gt-re-raw-h.headers.customrequestheaders.X-Gatetest-Router=raw\n'
		printf 'traefik.http.routers.gt-re-decoded.priority=100\n'
		printf 'traefik.http.routers.gt-re-decoded.middlewares=gt-re-decoded-h\n'
		printf 'traefik.http.middlewares.gt-re-decoded-h.headers.customrequestheaders.X-Gatetest-Router=decoded\n'
		printf 'outhaul.gatetest=true\n'
	} >"$work/labels-old"
	echo "labels on the new backend:"
	sed 's/^/  /' "$work/labels-new"
	echo "labels on the old backend:"
	sed 's/^/  /' "$work/labels-old"
fi

echo_py=$(
	cat <<'EOF'
import http.server, os
class H(http.server.BaseHTTPRequestHandler):
    def answer(self):
        lines = ["backend: " + os.environ.get("GATETEST_NAME", "app"), "path: " + self.path]
        lines += ["h %s: %s" % (k.lower(), v) for k, v in self.headers.items()]
        body = ("\n".join(lines) + "\n").encode()
        self.send_response(200)
        self.send_header("Content-Type", "text/plain")
        self.send_header("Content-Length", str(len(body)))
        # What an app may say to a CDN and a browser; see the CDN checks.
        self.send_header("CDN-Cache-Control", "max-age=600")
        self.send_header("Cache-Control", "max-age=60")
        self.end_headers()
        self.wfile.write(body)
    do_GET = do_POST = answer
    def log_message(self, *args):
        pass
http.server.ThreadingHTTPServer(("", 8000), H).serve_forever()
EOF
)
gate_py=$(
	cat <<'EOF'
import http.server, os, signal, sys
# SIGUSR1 closes the port and keeps the container (and its address): exec
# closes the listening socket.
signal.signal(signal.SIGUSR1, lambda *_: os.execvp("sleep", ["sleep", "infinity"]))
class H(http.server.BaseHTTPRequestHandler):
    def answer(self):
        tid = self.headers.get("X-Gatetest-Id", "-")
        out = ["GATE %s path: %s" % (tid, self.path)]
        out += ["GATE %s h %s: %s" % (tid, k.lower(), v) for k, v in self.headers.items()]
        sys.stdout.write("\n".join(out) + "\n")
        sys.stdout.flush()
        if "gatetest=ok" in self.headers.get("Cookie", ""):
            self.send_response(200)
            self.send_header("X-Outhaul-User", "dana")
        else:
            self.send_response(302)
            self.send_header("Location", "https://outhaul.test/gate/start?rd=1")
            self.send_header("Set-Cookie", "gatetest=pending; Path=/; HttpOnly")
        self.send_header("Content-Length", "0")
        self.end_headers()
    do_GET = do_POST = do_HEAD = answer
    def log_message(self, *args):
        pass
http.server.ThreadingHTTPServer(("", %PORT%), H).serve_forever()
EOF
)
gate_py=$(printf '%s\n' "$gate_py" | sed "s/%PORT%/$admin_port/")
alias_py=$(
	cat <<'EOF'
import http.server, sys
class H(http.server.BaseHTTPRequestHandler):
    def answer(self):
        sys.stdout.write("ALIAS %s path: %s\n" % (self.headers.get("X-Gatetest-Id", "-"), self.path))
        sys.stdout.flush()
        self.send_response(200)
        self.send_header("X-Outhaul-User", "eve")
        self.send_header("Content-Length", "0")
        self.end_headers()
    do_GET = do_POST = do_HEAD = answer
    def log_message(self, *args):
        pass
http.server.ThreadingHTTPServer(("", %PORT%), H).serve_forever()
EOF
)
alias_py=$(printf '%s\n' "$alias_py" | sed "s/%PORT%/$admin_port/")
# What Outhaul's address gives a container on the test network that maps
# host.docker.internal as the Traefiks do: "answers <status>" for /healthz,
# "refused", or "unreachable: <reason>".
probe_py=$(
	cat <<'EOF'
import sys, urllib.error, urllib.request
try:
    with urllib.request.urlopen("http://host.docker.internal:%s/healthz" % sys.argv[1], timeout=5) as r:
        print("answers %d" % r.status)
except urllib.error.HTTPError as e:
    print("answers %d" % e.code)
except urllib.error.URLError as e:
    print("refused" if isinstance(e.reason, ConnectionRefusedError) else "unreachable: %s" % e.reason)
except ConnectionRefusedError:
    print("refused")
except Exception as e:
    print("unreachable: %s" % e)
EOF
)

api_version=$(docker version --format '{{.Server.APIVersion}}') || die "docker version"
docker network create "$net" >/dev/null || die "create network $net"
if [ "$mode" = stub ]; then
	docker run -d --name "$gate_c" --network "$net" \
		"$python_image" python -u -c "$gate_py" >/dev/null || die "start the gate stub"
	gate_ip=$(docker inspect --format '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "$gate_c")
	[ -n "$gate_ip" ] || die "find the gate stub's address"
	docker run -d --name "$alias_c" --network "$net" --network-alias host.docker.internal \
		"$python_image" python -u -c "$alias_py" >/dev/null || die "start the alias stub"
else
	gate_ip=$outhaul_host
fi
probe_outhaul() {
	docker run --rm --name "$probe_c" --network "$net" --add-host "host.docker.internal:$gate_ip" \
		"$python_image" python -c "$probe_py" "$admin_port" 2>&1 | tail -1
}
docker run -d --name "$app_c" --network "$net" --label-file "$work/labels" \
	"$python_image" python -u -c "$echo_py" >/dev/null || die "start the echo backend"
if [ "$mode" = stub ]; then
	for b in old new; do
		docker run -d --name "outhaul-gatetest-$b" --network "$net" --label-file "$work/labels-$b" -e "GATETEST_NAME=$b" \
			"$python_image" python -u -c "$echo_py" >/dev/null || die "start the $b backend"
	done
fi
start_traefik() { # name, mapped|unmapped, extra Traefik arguments...
	name=$1
	if [ "$2" = mapped ]; then hosts="host.docker.internal:$gate_ip"; else hosts=; fi
	shift 2
	docker run -d --name "$name" --network "$net" ${hosts:+--add-host "$hosts"} \
		-p 127.0.0.1::80 -p 127.0.0.1::443 -p 127.0.0.1::8080 \
		-v "$docker_sock:/var/run/docker.sock:ro" \
		-e "DOCKER_API_VERSION=$api_version" \
		"$traefik_image" \
		--providers.docker=true \
		--providers.docker.exposedbydefault=true \
		"--providers.docker.network=$net" \
		'--providers.docker.constraints=Label(`outhaul.gatetest`,`true`)' \
		--entrypoints.web.address=:80 \
		--entrypoints.websecure.address=:443 \
		--api.insecure=true \
		--log.level=INFO --log.nocolor=true "$@" >/dev/null || die "start $name"
}
start_traefik "$traefik_c" mapped
if [ "$mode" = stub ]; then
	start_traefik "$trusting_c" mapped \
		--entrypoints.web.forwardedheaders.insecure=true \
		--entrypoints.websecure.forwardedheaders.insecure=true
	start_traefik "$redirect_c" mapped \
		--entrypoints.web.http.redirections.entrypoint.to=websecure \
		--entrypoints.web.http.redirections.entrypoint.scheme=https
	start_traefik "$unmapped_c" unmapped
fi

host_port() { # container, container port
	docker port "$1" "$2/tcp" | sed -n 's/^127\.0\.0\.1:\([0-9][0-9]*\)$/\1/p' | head -1
}
http_port=$(host_port "$traefik_c" 80)
https_port=$(host_port "$traefik_c" 443)
api_port=$(host_port "$traefik_c" 8080)
[ -n "$http_port" ] && [ -n "$https_port" ] && [ -n "$api_port" ] || die "find Traefik's loopback ports"
echo "Main Traefik $traefik_image on 127.0.0.1: web $http_port, websecure $https_port, api $api_port"
if [ "$mode" = stub ]; then
	trusting_http_port=$(host_port "$trusting_c" 80)
	trusting_https_port=$(host_port "$trusting_c" 443)
	redirect_http_port=$(host_port "$redirect_c" 80)
	redirect_https_port=$(host_port "$redirect_c" 443)
	unmapped_http_port=$(host_port "$unmapped_c" 80)
	unmapped_https_port=$(host_port "$unmapped_c" 443)
	[ -n "$trusting_http_port" ] && [ -n "$trusting_https_port" ] &&
		[ -n "$redirect_http_port" ] && [ -n "$redirect_https_port" ] &&
		[ -n "$unmapped_http_port" ] && [ -n "$unmapped_https_port" ] || die "find Traefik's loopback ports"
	echo "Trusting Traefik on 127.0.0.1: web $trusting_http_port, websecure $trusting_https_port"
	echo "Redirecting Traefik on 127.0.0.1: web $redirect_http_port, websecure $redirect_https_port"
	echo "Unmapped Traefik on 127.0.0.1: web $unmapped_http_port, websecure $unmapped_https_port"
	echo "Gate stub at $gate_ip, mapped as host.docker.internal in every Traefik but the unmapped one"
else
	echo "Outhaul at $outhaul_addr, mapped as host.docker.internal; the gate is http://host.docker.internal:$admin_port/gate/check"
fi

# req SCHEME HOST PATH [curl args...]: prints the status code; the response
# headers go to $work/h and the body to $work/b. It goes to the Traefik named
# by $via: main (the default), trusting, redirect or unmapped.
via=main
req() {
	scheme=$1 host=$2 path=$3
	shift 3
	case $via:$scheme in
	trusting:https) port=$trusting_https_port ;;
	trusting:*) port=$trusting_http_port ;;
	redirect:https) port=$redirect_https_port ;;
	redirect:*) port=$redirect_http_port ;;
	unmapped:https) port=$unmapped_https_port ;;
	unmapped:*) port=$unmapped_http_port ;;
	*:https) port=$https_port ;;
	*) port=$http_port ;;
	esac
	: >"$work/h"
	: >"$work/b"
	curl -sS -k --max-time 15 -o "$work/b" -D "$work/h" -w '%{http_code}' \
		--connect-to "::127.0.0.1:$port" "$scheme://$host$path" "$@" 2>"$work/err" || true
}
header() { # the value of response header $1 (case-insensitive), or nothing
	tr -d '\r' <"$work/h" | awk -v n="$1" 'BEGIN { n = tolower(n) ":" } tolower(substr($0, 1, length(n))) == n { sub(/^[^:]*:[ \t]*/, ""); print }'
}
api() { curl -sS --max-time 5 "http://127.0.0.1:$api_port/api/http/$1" 2>/dev/null; }
gate_lines() { # what the stub logged for test id $1, without the "GATE <id> " prefix
	docker logs "$gate_c" 2>/dev/null | sed -n "s/^GATE $1 //p"
}
alias_lines() { # what the alias stub logged for test id $1
	docker logs "$alias_c" 2>/dev/null | sed -n "s/^ALIAS $1 //p"
}
count_lines() { # how many lines of $1 equal $2
	printf '%s\n' "$1" | grep -cFx -- "$2"
}

if [ "$mode" = stub ]; then vias="main trusting redirect unmapped"; else vias=main; fi
for via in $vias; do
	if [ "$via" = redirect ]; then first=https; else first=http; fi
	ready=no
	i=0
	while [ $i -lt 30 ]; do
		if [ "$(req "$first" shop.test /)" = 200 ] && [ "$(req https shop.test /)" = 200 ] &&
			{ [ "$mode" = outhaul ] || [ "$via" = redirect ] || [ "$(req http mix2.test /)" = 200 ]; }; then
			ready=yes
			break
		fi
		sleep 1
		i=$((i + 1))
	done
	[ "$ready" = yes ] || die "the $via Traefik never served shop.test (the open row). Last status: $(req https shop.test /)"
done
via=main
echo

# The Accept header a browser sends with a page load.
browser_accept='Accept: text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8'
has() { # whether $1 contains $2
	case $1 in *"$2"*) return 0 ;; esac
	return 1
}
is_hex64() {
	case $1 in '' | *[!0-9a-f]*) return 1 ;; esac
	[ ${#1} -eq 64 ]
}
sha256() { printf '%s' "$1" | sha256sum | cut -c1-64; }

# check_sign_in STATUS SCHEME HOST PATH RD_PATH: the last response is the
# gate's answer to a page load of HOST PATH with no cookie: a 302 to
# /gate/start on the public URL, coming back to https://HOST (at the HTTPS
# port) and RD_PATH, the path as the redirect escapes it, and a pending cookie
# on HOST whose SHA-256 the redirect carries as n.
check_sign_in() {
	desc="$2 $3$4, a page load with no cookie"
	loc=$(header Location)
	cookie=$(header Set-Cookie | grep '^__Host-outhaul_pending=' | head -1)
	pending=${cookie#__Host-outhaul_pending=}
	pending=${pending%%;*}
	n=${loc#*\?n=}
	n=${n%%&*}
	if [ -n "$public_url" ]; then
		case $loc in "${public_url%/}/gate/start?n="*) where=yes ;; *) where=no ;; esac
	else
		case $loc in http://*/gate/start"?n="* | https://*/gate/start"?n="*) where=yes ;; *) where=no ;; esac
	fi
	case $loc in
	*"&rd=https%3A%2F%2F$3$5" | *"&rd=https%3A%2F%2F$3%3A"[0-9]*"$5") back=yes ;;
	*) back=no ;;
	esac
	if [ "$1" = 302 ] && [ "$where" = yes ] && [ "$back" = yes ] && is_hex64 "$pending" &&
		[ "$n" = "$(sha256 "$pending")" ] && has "$cookie" '; Path=/' && has "$cookie" '; HttpOnly' &&
		has "$cookie" '; Secure'; then
		pass "$desc: 302 to /gate/start on the public URL, coming back to https://$3 and $5, with a pending cookie on $3 that n names"
	else
		hint=
		if [ "$1" = 503 ]; then hint="503: does this Outhaul have a public URL (OUTHAUL_PUBLIC_URL)?"; fi
		fail "$desc: want a 302 to /gate/start, coming back to https://$3 and $5, with a pending cookie" \
			"status=$1" "location='$loc'" "set-cookie='$cookie'" ${hint:+"$hint"}
	fi
}
expect_401() { # STATUS DESCRIPTION: the last response is the gate's 401
	if [ "$1" = 401 ] && [ -z "$(header Location)" ] && [ -z "$(header Set-Cookie)" ]; then
		pass "$2: 401, with no redirect and no cookie"
	else
		fail "$2: want 401" "status=$1" "location='$(header Location)'" "$(cat "$work/b")"
	fi
}

# The checks against a running Outhaul (--outhaul).
outhaul_checks() {
	echo "== Outhaul at $outhaul_addr"
	got=$(probe_outhaul)
	if [ "$got" = "answers 200" ]; then
		pass "Outhaul answers /healthz at host.docker.internal:$admin_port, from the test network"
	else
		die "Outhaul's /healthz at host.docker.internal:$admin_port ($outhaul_host) gives '$got' from the test network. Check the address, and that no firewall keeps the Docker bridge from the admin port."
	fi
	echo

	for scheme in http https; do
		echo "== $scheme"
		code=$(req "$scheme" wiki.test /page -H "$browser_accept")
		check_sign_in "$code" "$scheme" wiki.test /page %2Fpage
		code=$(req "$scheme" shop.test /admin/x -H "$browser_accept")
		check_sign_in "$code" "$scheme" shop.test /admin/x %2Fadmin%2Fx
		# A browser reads a path that starts with // as naming another host.
		code=$(req "$scheme" wiki.test //evil.example/ --path-as-is -H "$browser_accept")
		check_sign_in "$code" "$scheme" wiki.test //evil.example/ %2Fevil.example%2F

		code=$(req "$scheme" wiki.test /page --data 'q=1' -H "$browser_accept")
		expect_401 "$code" "$scheme POST wiki.test/page (a form) with no cookie"
		code=$(req "$scheme" wiki.test /page -H 'Accept: application/json')
		expect_401 "$code" "$scheme GET wiki.test/page with Accept: application/json"
		code=$(req "$scheme" wiki.test /page -H 'Accept: */*')
		expect_401 "$code" "$scheme GET wiki.test/page with Accept: */* (a fetch call)"
		code=$(req "$scheme" wiki.test /page -X OPTIONS -H 'Accept: */*' -H 'Origin: https://other.test' \
			-H 'Access-Control-Request-Method: POST')
		expect_401 "$code" "$scheme OPTIONS wiki.test/page (a CORS preflight)"
		code=$(req "$scheme" wiki.test /socket --http1.1 -H 'Accept:' -H 'Connection: Upgrade' -H 'Upgrade: websocket' \
			-H 'Sec-WebSocket-Version: 13' -H 'Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==' -H 'Origin: https://wiki.test')
		expect_401 "$code" "$scheme a WebSocket handshake to wiki.test/socket with no cookie"
		# Which test refuses it: the same handshake, accepting text/html as no
		# browser's does, passes the gate's Accept test.
		code=$(req "$scheme" wiki.test /socket --http1.1 -H "$browser_accept" -H 'Connection: Upgrade' \
			-H 'Upgrade: websocket' -H 'Sec-WebSocket-Version: 13' -H 'Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==' \
			-H 'Origin: https://wiki.test')
		case $code in
		401) info "$scheme a WebSocket handshake that accepts text/html gets 401 too: Traefik passes Upgrade to the gate, and the gate's Upgrade test refuses it" ;;
		302) info "$scheme a WebSocket handshake that accepts text/html gets 302: Traefik doesn't pass Upgrade to the gate, so the gate's Accept test alone refuses a handshake" ;;
		*) fail "$scheme a WebSocket handshake that accepts text/html: want 401 or 302" "status=$code" "$(cat "$work/b")" ;;
		esac

		code=$(req "$scheme" wiki.test /webhooks/x -H "$browser_accept" -H 'X-Outhaul-User: mallory')
		body=$(cat "$work/b")
		if [ "$code" = 200 ] && [ "$(count_lines "$body" 'path: /webhooks/x')" = 1 ] &&
			! printf '%s\n' "$body" | grep -qi mallory; then
			pass "$scheme wiki.test/webhooks/x (open path, protected host), no cookie: served, without the forged X-Outhaul-User"
		else
			fail "$scheme wiki.test/webhooks/x, no cookie: want 200 from the backend, without X-Outhaul-User" "status=$code" "$body"
		fi

		if [ -z "$route_host" ]; then
			skip "$scheme the checks with someone signed in (set GATETEST_ROUTE_HOST and GATETEST_ROUTE_COOKIE)"
			echo
			continue
		fi
		jar="Cookie: __Host-outhaul_route=$route_cookie"
		code=$(req "$scheme" "$route_host" /page -H "$jar" -H "$browser_accept" -H 'X-Outhaul-User: mallory')
		body=$(cat "$work/b")
		user=$(printf '%s\n' "$body" | sed -n 's/^h x-outhaul-user: //p')
		if [ "$code" = 200 ] && [ "$(printf '%s\n' "$body" | grep -c '^h x-outhaul-user: ')" = 1 ] &&
			[ -n "$user" ] && [ "$user" != mallory ]; then
			pass "$scheme $route_host/page with the route cookie: served, and the app gets X-Outhaul-User: $user, not the forged one"
		else
			fail "$scheme $route_host/page with the route cookie: want 200, with X-Outhaul-User from Outhaul" \
				"status=$code" "location='$(header Location)'" "$body" \
				"Is GATETEST_ROUTE_COOKIE a live route cookie for $route_host? Sign-ins last 8 hours."
		fi
		code=$(req "$scheme" "$route_host" //evil.example/ --path-as-is -H "$jar" -H "$browser_accept")
		body=$(cat "$work/b")
		if [ "$code" = 200 ] && [ -z "$(header Location)" ] && printf '%s\n' "$body" | grep -q '^path: /'; then
			pass "$scheme $route_host//evil.example/ with the route cookie: served on $route_host at '$(printf '%s\n' "$body" | sed -n 's/^path: //p')', with no redirect"
		else
			fail "$scheme $route_host//evil.example/ with the route cookie: want 200 from the backend" \
				"status=$code" "location='$(header Location)'" "$body"
		fi
		code=$(req "$scheme" "$route_host" '//evil.example/?__outhaul_code=not-a-code' --path-as-is -H "$jar" -H "$browser_accept")
		loc=$(header Location)
		case $code:$loc in
		302:"https://$route_host/evil.example/" | 302:"https://$route_host:"[0-9]*"/evil.example/")
			pass "$scheme $route_host//evil.example/ with a code and the route cookie: 302 to $loc, on the host and without the code"
			;;
		*)
			fail "$scheme $route_host//evil.example/ with a code and the route cookie: want a 302 to https://$route_host/evil.example/" \
				"status=$code" "location='$loc'"
			;;
		esac
		code=$(req "$scheme" wiki.test /page -H "$jar" -H "$browser_accept")
		if [ "$code" = 302 ] && has "$(header Location)" '/gate/start?'; then
			pass "$scheme wiki.test/page with $route_host's route cookie: sent to sign in, as the cookie works only on its own host"
		else
			fail "$scheme wiki.test/page with $route_host's route cookie: want the sign-in redirect" \
				"status=$code" "location='$(header Location)'"
		fi
		echo
	done

	echo "== Outhaul stopped"
	answer=s
	if [ -t 0 ]; then
		printf 'Stop Outhaul now, and leave the machine it runs on up. Press Enter once it has stopped, or type s and Enter to skip: '
		read -r answer || answer=s
	fi
	case $answer in
	s | S)
		skip "with Outhaul stopped, protected paths answer 500 and open paths 200 (it asks in a terminal)"
		return
		;;
	esac
	i=0
	got=$(probe_outhaul)
	while [ "$got" != refused ] && [ $i -lt 30 ]; do
		sleep 1
		i=$((i + 1))
		got=$(probe_outhaul)
	done
	if [ "$got" = refused ]; then
		pass "Outhaul's address still resolves, and refuses connections"
	else
		fail "Outhaul's address refuses connections once it has stopped" "the test network gets: $got"
		return
	fi
	for scheme in http https; do
		for target in "wiki.test /page" "shop.test /admin/x"; do
			set -- $target
			code=$(req "$scheme" "$1" "$2" -H "$browser_accept")
			if [ "$code" = 500 ]; then
				pass "$scheme $1$2 (protected) with Outhaul stopped: 500, not the app"
			else
				fail "$scheme $1$2 (protected) with Outhaul stopped: want 500" "status=$code" "$(cat "$work/b")"
			fi
		done
		for target in "shop.test /cart" "wiki.test /webhooks"; do
			set -- $target
			code=$(req "$scheme" "$1" "$2" -H "$browser_accept")
			if [ "$code" = 200 ]; then
				pass "$scheme $1$2 (open) with Outhaul stopped: still 200"
			else
				fail "$scheme $1$2 (open) with Outhaul stopped: want 200" "status=$code" "$(cat "$work/b")"
			fi
		done
	done
	echo "You can start Outhaul again."
}

if [ "$mode" = outhaul ]; then
	outhaul_checks
	echo
	finish
	exit
fi

tid=0
echo "== the labels as Docker and Traefik keep them"
for c in "$traefik_c" "$trusting_c" "$redirect_c" "$unmapped_c" "$app_c" "$old_c" "$new_c" "$gate_c" "$alias_c"; do
	enable=$(docker inspect --format '{{range $k, $v := .Config.Labels}}{{if eq $k "traefik.enable"}}{{$k}}={{$v}}{{end}}{{end}}' "$c")
	if [ -z "$enable" ]; then
		pass "$c carries no traefik.enable label, so a Traefik that doesn't expose by default ignores it"
	else
		fail "$c carries no traefik.enable label" "it has $enable"
	fi
done
label=traefik.http.middlewares.gt-shop-noident.headers.customrequestheaders.X-Outhaul-User
got=$(docker inspect --format "{{range \$k, \$v := .Config.Labels}}{{if eq \$k \"$label\"}}present:[{{\$v}}]{{end}}{{end}}" "$app_c")
if [ "$got" = "present:[]" ]; then
	pass "the container keeps the empty-valued strip label ($label)"
else
	fail "the container keeps the empty-valued strip label" "docker inspect gave: '$got'"
fi
# api_has PATH DESCRIPTION SUBSTRING...: the API object at PATH is enabled and
# contains every SUBSTRING.
api_has() {
	obj=$(api "$1")
	desc=$2
	shift 2
	for want in '"status":"enabled"' "$@"; do
		case $obj in
		*"$want"*) ;;
		*)
			fail "$desc" "missing $want" "API: $obj"
			return
			;;
		esac
	done
	pass "$desc"
}
api_has middlewares/gt-shop-noident@docker \
	"Traefik loads gt-shop-noident, emptying X-Outhaul-User and every other spelling of it" \
	'"customRequestHeaders":{"X-Outhaul-User":"","X-Outhaul_User":"","X_Outhaul-User":"","X_Outhaul_User":""}' \
	'"usedBy":["gt-shop-tls@docker","gt-shop@docker"]'
api_has middlewares/gt-wiki-noalias@docker \
	"Traefik loads gt-wiki-noalias, emptying every other spelling of X-Outhaul-User" \
	'"customRequestHeaders":{"X-Outhaul_User":"","X_Outhaul-User":"","X_Outhaul_User":""}' \
	'"usedBy":["gt-wiki-tls@docker","gt-wiki@docker"]'
if docker logs "$traefik_c" 2>&1 | grep -q 'trustForwardHeader is not configured'; then
	fail "Traefik raises no trustForwardHeader warning for the gate" \
		"$(docker logs "$traefik_c" 2>&1 | grep 'trustForwardHeader is not configured' | head -2)"
else
	pass "Traefik raises no trustForwardHeader warning for the gate (it is set to false)"
fi
if grep -q '^traefik\.http\.routers\.gt-admin-tls\.middlewares=gt-admin-gate,' "$work/labels"; then
	pass "gt-admin (protected, its own TLS flag off) has an HTTPS router, gated"
else
	fail "gt-admin (protected, its own TLS flag off) has an HTTPS router, gated" "$(grep gt-admin-tls "$work/labels")"
fi
for r in gt-wiki gt-wiki-tls gt-hooks gt-hooks-tls gt-shop gt-shop-tls gt-admin gt-admin-tls; do
	info=$(api "routers/$r@docker")
	case $info in
	*'"status":"enabled"'*) pass "router $r is enabled" ;;
	*) fail "router $r is enabled" "API: $info" ;;
	esac
done
info=$(api routers/gt-admin@docker)
case $info in
*'"middlewares":["gt-admin-gate@docker","gt-admin-noalias@docker","gt-admin-strip@docker"]'*)
	pass "gt-admin runs the gate, then the alias strip, then the path strip" ;;
*) fail "gt-admin runs the gate, then the alias strip, then the path strip" "API: $info" ;;
esac
compose_out=$("$work/gatelabels" -compose -tls -port 8000 'gt,wiki.test,,,protected' 'gt,wiki.test,/webhooks,,open' \
	'gt,wiki.test,/admin,,protected,notls')
printf 'services:\n  web:\n    image: %s\n' "$python_image" >"$work/compose.yml"
printf '%s\n' "$compose_out" >"$work/override.yml"
merged=$(docker compose -p outhaul-gatetest-compose -f "$work/compose.yml" -f "$work/override.yml" config 2>&1)
for want in \
	'traefik.http.middlewares.outhaul-gt-d2-noident.headers.customrequestheaders.X-Outhaul-User: ""' \
	'traefik.http.middlewares.outhaul-gt-d2-noident.headers.customrequestheaders.X_Outhaul_User: ""' \
	'traefik.http.middlewares.outhaul-gt-d2-noident.headers.customrequestheaders.X-Outhaul_User: ""' \
	'traefik.http.middlewares.outhaul-gt-d2-noident.headers.customrequestheaders.X_Outhaul-User: ""' \
	'traefik.http.middlewares.outhaul-gt-d1-noalias.headers.customrequestheaders.X_Outhaul_User: ""' \
	'traefik.http.middlewares.outhaul-gt-d1-noalias.headers.customrequestheaders.X-Outhaul_User: ""' \
	'traefik.http.middlewares.outhaul-gt-d1-noalias.headers.customrequestheaders.X_Outhaul-User: ""' \
	'traefik.http.middlewares.outhaul-gt-d1-gate.forwardauth.trustforwardheader: "false"' \
	'traefik.http.middlewares.outhaul-gt-d1-noalias.headers.customresponseheaders.CDN-Cache-Control: no-store' \
	'traefik.http.middlewares.outhaul-gt-d1-noalias.headers.customresponseheaders.Cloudflare-CDN-Cache-Control: no-store' \
	'traefik.http.routers.outhaul-gt-d3-tls.middlewares: outhaul-gt-d3-gate,outhaul-gt-d3-noalias'; do
	if printf '%s\n' "$merged" | grep -qF -- "$want"; then
		pass "docker compose keeps $want"
	else
		fail "docker compose keeps $want" "$(printf '%s\n' "$merged" | grep -i -e outhaul_user -e outhaul-user -e trustforward -e d3-tls -e cache-control -e error | head -8)"
	fi
done
# A container Compose makes from the override (created, never started, so
# no Traefik routes to it) carries each rule and priority exactly as a
# single-container app's labels do: the rule's backslashes survive YAML, and
# its $ survives Compose's interpolation.
if docker compose -p outhaul-gatetest-compose -f "$work/compose.yml" -f "$work/override.yml" create >"$work/err" 2>&1; then
	for pair in 'outhaul-gt-d1 gt-wiki' 'outhaul-gt-d1-tls gt-wiki-tls' 'outhaul-gt-d2 gt-hooks' 'outhaul-gt-d2-tls gt-hooks-tls'; do
		set -- $pair
		for key in rule priority; do
			want=$(sed -n "s/^traefik\\.http\\.routers\\.$2\\.$key=//p" "$work/labels")
			got=$(docker inspect --format "{{index .Config.Labels \"traefik.http.routers.$1.$key\"}}" "$compose_c")
			if [ -n "$want" ] && [ "$got" = "$want" ]; then
				pass "the Compose container's $1 $key is exactly the single-container label: $got"
			else
				fail "the Compose container's $1 $key is exactly the single-container label" "got:  $got" "want: $want"
			fi
		done
	done
else
	fail "docker compose creates a container from the override" "$(cat "$work/err")"
fi
docker rm -f "$compose_c" >/dev/null 2>&1
docker network rm outhaul-gatetest-compose_default >/dev/null 2>&1
echo

echo "== which host.docker.internal answers for the gate"
for target in "main http" "main https" "trusting http" "redirect https"; do
	set -- $target
	via=$1
	tid=$((tid + 1))
	code=$(req "$2" wiki.test /page -H 'Accept: text/html' -H "X-Gatetest-Id: $tid")
	if [ "$code" = 302 ] && [ "$(header Set-Cookie)" = "gatetest=pending; Path=/; HttpOnly" ] &&
		[ -n "$(gate_lines "$tid")" ] && [ -z "$(alias_lines "$tid")" ]; then
		pass "$2 via the $1 Traefik: the mapped stub answers for host.docker.internal, not the container holding the alias"
	else
		fail "$2 via the $1 Traefik: want the mapped stub's 302, and no call to the alias" \
			"status=$code" "gate saw: $(gate_lines "$tid")" "alias saw: $(alias_lines "$tid")" "$(cat "$work/b")"
	fi
done
via=unmapped
tid=$((tid + 1))
code=$(req http wiki.test /page -H 'Accept: text/html' -H "X-Gatetest-Id: $tid")
body=$(cat "$work/b")
if [ "$code" = 200 ] && [ "$(count_lines "$body" 'h x-outhaul-user: eve')" = 1 ] &&
	[ -n "$(alias_lines "$tid")" ] && [ -z "$(gate_lines "$tid")" ]; then
	pass "control: with no /etc/hosts entry, the alias answers for host.docker.internal (its 200 and 'eve' reach the backend), so the checks above tell the two apart"
else
	fail "control: with no /etc/hosts entry, want the alias's 200 and 'eve' at the backend" \
		"status=$code" "gate saw: $(gate_lines "$tid")" "alias saw: $(alias_lines "$tid")" "$body"
fi
via=main
echo

for scheme in http https; do
	echo "== $scheme"

	# A protected path under an open host: the stub's 302 and Set-Cookie reach the client.
	tid=$((tid + 1))
	code=$(req "$scheme" shop.test /admin/x -H 'Accept: text/html' -H "X-Gatetest-Id: $tid")
	loc=$(header Location)
	cookie=$(header Set-Cookie)
	if [ "$code" = 302 ] && [ "$loc" = "https://outhaul.test/gate/start?rd=1" ] &&
		[ "$cookie" = "gatetest=pending; Path=/; HttpOnly" ]; then
		pass "$scheme shop.test/admin/x (protected path, open host), no cookie: the stub's 302, Location and Set-Cookie reach the client"
	else
		fail "$scheme shop.test/admin/x, no cookie: want the stub's 302 with Location and Set-Cookie" \
			"status=$code location='$loc' set-cookie='$cookie'" "$(cat "$work/err")"
	fi
	tid=$((tid + 1))
	code=$(req "$scheme" wiki.test /page -H 'Accept: text/html' -H "X-Gatetest-Id: $tid")
	if [ "$code" = 302 ] && [ "$(header Set-Cookie)" = "gatetest=pending; Path=/; HttpOnly" ]; then
		pass "$scheme wiki.test/page (protected host), no cookie: the stub's 302 with Set-Cookie"
	else
		fail "$scheme wiki.test/page, no cookie: want the stub's 302 with Set-Cookie" "status=$code" "$(cat "$work/b")"
	fi

	# Whether Traefik passes a WebSocket handshake's Upgrade to the gate. If it
	# doesn't, the gate's Accept test alone refuses a handshake.
	tid=$((tid + 1))
	code=$(req "$scheme" wiki.test /socket --http1.1 -H 'Accept:' -H 'Connection: Upgrade' -H 'Upgrade: websocket' \
		-H 'Sec-WebSocket-Version: 13' -H 'Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==' -H "X-Gatetest-Id: $tid")
	lines=$(gate_lines "$tid")
	if [ -n "$lines" ] && [ "$code" = 302 ]; then
		pass "$scheme a WebSocket handshake to wiki.test (protected host) goes to the gate"
		if [ "$(count_lines "$lines" 'h upgrade: websocket')" = 1 ]; then
			info "$scheme Traefik passes Upgrade: websocket to the gate"
		else
			info "$scheme Traefik doesn't pass Upgrade to the gate, so the gate's Accept test is what refuses a handshake"
		fi
	else
		fail "$scheme a WebSocket handshake to wiki.test: want the gate asked, and its 302 back" "status=$code" "gate saw: $lines"
	fi

	# A path that starts with // reaches the gate, and what Traefik says it is.
	tid=$((tid + 1))
	code=$(req "$scheme" wiki.test //evil.example/ --path-as-is -H 'Accept: text/html' -H "X-Gatetest-Id: $tid")
	uri=$(gate_lines "$tid" | sed -n 's/^h x-forwarded-uri: //p')
	if [ "$code" = 302 ] && [ -n "$uri" ]; then
		pass "$scheme wiki.test//evil.example/ (protected host), no cookie: the gate is asked, and answers"
		info "$scheme for wiki.test//evil.example/, Traefik gives the gate X-Forwarded-Uri: $uri"
	else
		fail "$scheme wiki.test//evil.example/: want the gate asked, and its 302 back" "status=$code" "gate saw: $(gate_lines "$tid")"
	fi

	# A 200 from the stub forwards X-Outhaul-User: dana, replacing a forged value.
	tid=$((tid + 1))
	code=$(req "$scheme" wiki.test /page -H 'Cookie: gatetest=ok' -H 'X-Outhaul-User: mallory' -H "X-Gatetest-Id: $tid")
	body=$(cat "$work/b")
	if [ "$code" = 200 ] && [ "$(count_lines "$body" 'h x-outhaul-user: dana')" = 1 ] &&
		! printf '%s\n' "$body" | grep -qi mallory; then
		pass "$scheme wiki.test/page, gate says 200: the backend gets X-Outhaul-User: dana, and the forged 'mallory' is gone"
	else
		fail "$scheme wiki.test/page, gate says 200: want X-Outhaul-User: dana only" "status=$code" "$body"
	fi
	# A CDN in front must not cache a protected route's answer. The app says
	# CDN-Cache-Control: max-age=600 and Cache-Control: max-age=60.
	cdn=$(header CDN-Cache-Control) cf=$(header Cloudflare-CDN-Cache-Control) cc=$(header Cache-Control)
	if [ "$code" = 200 ] && [ "$cdn" = no-store ] && [ "$cf" = no-store ] && [ "$cc" = max-age=60 ]; then
		pass "$scheme wiki.test/page (protected): CDN-Cache-Control and Cloudflare-CDN-Cache-Control are no-store, over the app's own, and its Cache-Control is unchanged"
	else
		fail "$scheme wiki.test/page (protected): want both CDN headers no-store, and Cache-Control: max-age=60" "status=$code" \
			"CDN-Cache-Control: $cdn" "Cloudflare-CDN-Cache-Control: $cf" "Cache-Control: $cc"
	fi
	tid=$((tid + 1))
	code=$(req "$scheme" shop.test /admin/x -H 'Cookie: gatetest=ok' -H 'x-outhaul-user: mallory' -H "X-Gatetest-Id: $tid")
	body=$(cat "$work/b")
	if [ "$code" = 200 ] && [ "$(count_lines "$body" 'h x-outhaul-user: dana')" = 1 ] &&
		! printf '%s\n' "$body" | grep -qi mallory && [ "$(count_lines "$body" 'path: /x')" = 1 ]; then
		pass "$scheme shop.test/admin/x, gate says 200: the backend gets dana (forged lower-case header gone) on the stripped path /x"
	else
		fail "$scheme shop.test/admin/x, gate says 200: want dana only, on path /x" "status=$code" "$body"
	fi
	lines=$(gate_lines "$tid")
	if [ "$(count_lines "$lines" 'h x-forwarded-uri: /admin/x')" = 1 ] && [ "$(count_lines "$lines" 'path: /gate/check')" = 1 ]; then
		pass "$scheme the gate is called at /gate/check and sees the unstripped /admin/x: it runs before the strip"
	else
		fail "$scheme the gate sees /gate/check and the unstripped URI /admin/x" "$lines"
	fi

	# Servers that turn headers into CGI-style variables read X_Outhaul_User,
	# X-Outhaul_User and X_Outhaul-User as X-Outhaul-User, so none of them may
	# reach an app, on any row, in any case.
	for target in "wiki.test /page" "shop.test /admin/x"; do
		set -- $target
		tid=$((tid + 1))
		code=$(req "$scheme" "$1" "$2" -H 'Cookie: gatetest=ok' -H 'X_Outhaul_User: mallory1' \
			-H 'x_outhaul_user: mallory2' -H 'X_OUTHAUL_USER: mallory3' -H 'X-Outhaul_User: mallory4' \
			-H 'X_Outhaul-User: mallory5' -H "X-Gatetest-Id: $tid")
		body=$(cat "$work/b")
		if [ "$code" = 200 ] && [ "$(count_lines "$body" 'h x-outhaul-user: dana')" = 1 ] &&
			! printf '%s\n' "$body" | grep -qi -e mallory -e 'x_outhaul_user'; then
			pass "$scheme $1$2 (protected), gate says 200: X_Outhaul_User, X-Outhaul_User and X_Outhaul-User are gone, and dana still arrives"
		else
			fail "$scheme $1$2 (protected), gate says 200: want dana only, and no other spelling" "status=$code" "$body"
		fi
	done
	tid=$((tid + 1))
	code=$(req "$scheme" shop.test /cart -H 'X_Outhaul_User: mallory1' \
		-H 'x_outhaul_user: mallory2' -H 'X_OUTHAUL_USER: mallory3' -H 'X-Outhaul_User: mallory4' \
		-H 'X_Outhaul-User: mallory5' -H "X-Gatetest-Id: $tid")
	body=$(cat "$work/b")
	if [ "$code" = 200 ] && ! printf '%s\n' "$body" | grep -qi -e mallory -e 'x_outhaul_user'; then
		pass "$scheme shop.test/cart (open host): served, and X_Outhaul_User, X-Outhaul_User and X_Outhaul-User are gone"
	else
		fail "$scheme shop.test/cart: want 200 with no other spelling of X-Outhaul-User" "status=$code" "$body"
	fi

	# An open row strips a client-sent X-Outhaul-User, and still serves.
	tid=$((tid + 1))
	code=$(req "$scheme" shop.test /cart -H 'X-Outhaul-User: mallory' -H "X-Gatetest-Id: $tid")
	body=$(cat "$work/b")
	if [ "$code" = 200 ] && ! printf '%s\n' "$body" | grep -qi 'x-outhaul-user'; then
		pass "$scheme shop.test/cart (open host): served, and the forged X-Outhaul-User is stripped"
	else
		fail "$scheme shop.test/cart: want 200 with no X-Outhaul-User" "status=$code" "$body"
	fi
	cdn=$(header CDN-Cache-Control) cf=$(header Cloudflare-CDN-Cache-Control)
	if [ "$code" = 200 ] && [ "$cdn" = max-age=600 ] && [ -z "$cf" ]; then
		pass "$scheme shop.test/cart (open host): the app's own CDN-Cache-Control comes through, and no Cloudflare-CDN-Cache-Control is added"
	else
		fail "$scheme shop.test/cart (open host): want the app's CDN-Cache-Control: max-age=600 and no other" "status=$code" \
			"CDN-Cache-Control: $cdn" "Cloudflare-CDN-Cache-Control: $cf"
	fi
	if [ -z "$(gate_lines "$tid")" ]; then
		pass "$scheme shop.test/cart: no gate call"
	else
		fail "$scheme shop.test/cart: the gate was called" "$(gate_lines "$tid")"
	fi

	# Open paths under a protected host reach the backend with no gate call,
	# including /webhooksfoo: PathPrefix is a plain string prefix.
	for p in /webhooks /webhooks/x /webhooksfoo; do
		tid=$((tid + 1))
		code=$(req "$scheme" wiki.test "$p" -H 'Accept: text/html' -H 'X-Outhaul-User: mallory' \
			-H 'X_Outhaul_User: mallory' -H 'X-Outhaul_User: mallory' -H 'X_Outhaul-User: mallory' \
			-H "X-Gatetest-Id: $tid")
		body=$(cat "$work/b")
		if [ "$code" = 200 ] && [ "$(count_lines "$body" "path: $p")" = 1 ] &&
			! printf '%s\n' "$body" | grep -qi -e mallory -e 'outhaul.user' && [ -z "$(gate_lines "$tid")" ]; then
			pass "$scheme wiki.test$p (open path, protected host): reaches the backend, no gate call, every forged spelling stripped"
		else
			fail "$scheme wiki.test$p: want 200 from the backend with no gate call and no spelling of X-Outhaul-User" \
				"status=$code" "$body" "gate saw: $(gate_lines "$tid")"
		fi
	done
	cdn=$(header CDN-Cache-Control) cf=$(header Cloudflare-CDN-Cache-Control)
	if [ "$code" = 200 ] && [ "$cdn" = max-age=600 ] && [ -z "$cf" ]; then
		pass "$scheme wiki.test/webhooksfoo (open path, protected host): the app's own CDN-Cache-Control comes through, and no Cloudflare-CDN-Cache-Control is added"
	else
		fail "$scheme wiki.test/webhooksfoo (open path): want the app's CDN-Cache-Control: max-age=600 and no other" "status=$code" \
			"CDN-Cache-Control: $cdn" "Cloudflare-CDN-Cache-Control: $cf"
	fi

	# What the stub receives: the browser's Cookie and Accept, and X-Forwarded-*
	# set by Traefik, not by the client, even through entrypoints that trust a
	# client's X-Forwarded-* (only the gate's own setting stops them there).
	for via in main trusting; do
		if [ "$scheme" = https ]; then forged_proto=http; else forged_proto=https; fi
		tid=$((tid + 1))
		code=$(req "$scheme" wiki.test '/page?q=1' \
			-H 'Cookie: gatetest=ok; theme=dark' -H 'Accept: text/html,application/xhtml+xml' \
			-H 'X-Forwarded-Host: evil.test' -H 'X-Forwarded-Uri: /webhooks' -H 'X-Forwarded-Method: POST' \
			-H "X-Forwarded-Proto: $forged_proto" -H 'X-Forwarded-Prefix: /evil' \
			-H 'X-Forwarded-Port: 666' -H 'X-Real-Ip: 6.6.6.6' -H "X-Gatetest-Id: $tid")
		lines=$(gate_lines "$tid")
		check_gate() { # description, expected line, header name
			if [ "$(count_lines "$lines" "$2")" = 1 ] && [ "$(printf '%s\n' "$lines" | grep -c "^h $3: ")" = 1 ]; then
				pass "$scheme via the $via Traefik, the gate receives exactly '$2' ($1)"
			else
				fail "$scheme via the $via Traefik, the gate receives exactly '$2' ($1)" "status=$code" "$lines"
			fi
		}
		check_gate "the browser's cookies" 'h cookie: gatetest=ok; theme=dark' cookie
		check_gate "the browser's Accept" 'h accept: text/html,application/xhtml+xml' accept
		check_gate "client sent evil.test" 'h x-forwarded-host: wiki.test' x-forwarded-host
		check_gate "client sent /webhooks" 'h x-forwarded-uri: /page?q=1' x-forwarded-uri
		check_gate "client sent POST" 'h x-forwarded-method: GET' x-forwarded-method
		check_gate "client sent $forged_proto" "h x-forwarded-proto: $scheme" x-forwarded-proto
		for forged in 'h x-forwarded-prefix: /evil' 'h x-forwarded-port: 666' 'h x-real-ip: 6.6.6.6'; do
			if [ "$(count_lines "$lines" "$forged")" = 0 ] && [ -n "$lines" ]; then
				pass "$scheme via the $via Traefik, the gate doesn't receive the client's '${forged#h }'"
			else
				fail "$scheme via the $via Traefik, the gate doesn't receive the client's '${forged#h }'" "$lines"
			fi
		done
	done
	via=main
	echo
done

# Paths sent exactly as written (--path-as-is), where a client, Traefik and an
# app could read a path differently. probe SCHEME HOST PATH [curl args...]
# sends one, and sets code, gated (yes when the gate stub was asked), seen (the
# path the backend got, or "" when the request never reached it) and backend
# (which backend container answered).
probe() {
	tid=$((tid + 1))
	probe_scheme=$1 probe_host=$2 probe_path=$3
	shift 3
	code=$(req "$probe_scheme" "$probe_host" "$probe_path" --path-as-is -H 'Accept: text/html' -H "X-Gatetest-Id: $tid" "$@")
	if [ -n "$(gate_lines "$tid")" ]; then gated=yes; else gated=no; fi
	seen=$(sed -n 's/^path: //p' "$work/b" | head -1)
	backend=$(sed -n 's/^backend: //p' "$work/b" | head -1)
}
# what the last probe did, in words
probe_said() {
	if [ -n "$seen" ]; then
		printf 'status %s, gate asked: %s, the %s backend got %s' "$code" "$gated" "$backend" "$seen"
	else
		printf 'status %s, gate asked: %s, the backend got nothing' "$code" "$gated"
	fi
}
for scheme in http https; do
	echo "== $scheme, paths as written"
	# Every router refuses a path with a ".." segment in any spelling, so none
	# reaches the app through a row's router: Traefik's own clean-up resolves
	# the plain forms first and sends them to the row they name (here the
	# gate), and no router takes the rest. Both directions: open /webhooks
	# under the protected wiki.test, and the protected /admin under the open
	# shop.test.
	for target in \
		'wiki.test /webhooks/../page' 'wiki.test /webhooks/%2e%2e/page' \
		'wiki.test /webhooks%2f..%2fpage' 'wiki.test /webhooks/..%2fpage' 'wiki.test /webhooks/%2e%2e%2fpage' \
		'wiki.test /webhooks/..;/page' 'wiki.test /webhooks/..%3bpage' 'wiki.test /webhooks/..%5cpage' \
		'wiki.test /webhooks/..\page' \
		'shop.test /x/../admin' 'shop.test /x/%2e%2e/admin' \
		'shop.test /x/..%2fadmin' 'shop.test /x/%2e%2e%2fadmin' 'shop.test /x/..%2Fadmin' \
		'shop.test /x/..;/admin' 'shop.test /x/..%3badmin' 'shop.test /x/..%5cadmin' 'shop.test /x/..\admin'; do
		set -- $target
		probe "$scheme" "$1" "$2"
		said=$(probe_said)
		if [ -n "$seen" ]; then
			fail "$scheme $1$2 (a .. segment): reached the backend" "$said"
		elif [ "$gated" = yes ] && [ "$code" = 302 ]; then
			pass "$scheme $1$2 (a .. segment): Traefik resolved it, and the gate was asked ($said)"
		elif [ "$gated" = no ] && [ "$code" = 404 ]; then
			pass "$scheme $1$2 (a .. segment): no router takes it, 404 ($said)"
		else
			fail "$scheme $1$2 (a .. segment): want the gate's 302 or a 404" "$said" "$(cat "$work/b")"
		fi
	done
	# Paths that only look like one still reach the app: open rows with no
	# gate call, the protected row through the gate.
	for target in \
		'shop.test /a%2Fb' 'shop.test /file..txt' 'shop.test /.well-known/x' 'shop.test /a/.../b' 'shop.test /..foo' \
		'wiki.test /webhooks/a%2Fb' 'wiki.test /webhooks/file..txt' 'wiki.test /webhooks/.well-known/x' \
		'wiki.test /webhooks/a/.../b' 'wiki.test /webhooks/..foo'; do
		set -- $target
		probe "$scheme" "$1" "$2"
		if [ "$code" = 200 ] && [ "$gated" = no ] && [ "$seen" = "$2" ]; then
			pass "$scheme $1$2 (open): reaches the app as $2, with no gate call"
		else
			fail "$scheme $1$2 (open): want 200 from the app on $2, with no gate call" "$(probe_said)"
		fi
	done
	for p in /a%2Fb /file..txt /.well-known/x /a/.../b /..foo; do
		probe "$scheme" wiki.test "$p" -H 'Cookie: gatetest=ok'
		if [ "$code" = 200 ] && [ "$gated" = yes ] && [ "$seen" = "$p" ]; then
			pass "$scheme wiki.test$p (protected, signed in): reaches the app as $p, through the gate"
		else
			fail "$scheme wiki.test$p (protected, signed in): want 200 from the app on $p, through the gate" "$(probe_said)"
		fi
	done
	# PathPrefix compares case: /ADMIN isn't the protected /admin row, so the
	# open row serves it. The docs say so, and recommend protecting the whole
	# address and opening named paths.
	probe "$scheme" shop.test /ADMIN/x
	if [ "$code" = 200 ] && [ "$gated" = no ] && [ "$seen" = /ADMIN/x ]; then
		pass "$scheme shop.test/ADMIN/x: the open row serves it, with no gate call (PathPrefix compares case, as the docs say)"
	else
		fail "$scheme shop.test/ADMIN/x: expected the open row to serve it" "$(probe_said)"
	fi
	for p in /admin%2fx /./admin/x; do
		probe "$scheme" shop.test "$p"
		if [ -z "$seen" ] && [ "$gated" = yes ] && [ "$code" = 302 ]; then
			pass "$scheme shop.test$p (protected /admin under an open host): the gate is asked ($(probe_said))"
		else
			fail "$scheme shop.test$p (protected /admin under an open host): want the gate's 302" "$(probe_said)"
		fi
	done
	# A client naming the identity header in Connection asks Traefik to drop
	# it as hop-by-hop. Signed in, the backend must still get the gate's
	# answer, and never a value of the client's.
	tid=$((tid + 1))
	code=$(req "$scheme" wiki.test /page --http1.1 -H 'Cookie: gatetest=ok' -H 'Connection: X-Outhaul-User' \
		-H 'X-Outhaul-User: mallory' -H "X-Gatetest-Id: $tid")
	body=$(cat "$work/b")
	if [ "$code" = 200 ] && [ "$(count_lines "$body" 'h x-outhaul-user: dana')" = 1 ] &&
		[ "$(printf '%s\n' "$body" | grep -ci '^h x-outhaul-user: ')" = 1 ]; then
		pass "$scheme wiki.test/page signed in, with Connection: X-Outhaul-User: the backend still gets X-Outhaul-User: dana, and only that"
	else
		fail "$scheme wiki.test/page signed in, with Connection: X-Outhaul-User: want X-Outhaul-User: dana at the backend" \
			"status=$code" "$body"
	fi
	echo
done

echo "== what PathRegexp sees"
probe http re.test /a%2fb
case $(sed -n 's/^h x-gatetest-router: //p' "$work/b") in
raw) info "PathRegexp sees the path still escaped: /a%2fb matched PathRegexp(\`(?i)%2f\`), and the app got $seen" ;;
decoded) info "PathRegexp sees the path decoded: /a%2fb matched PathRegexp(\`^/a/b\$\`), and the app got $seen" ;;
*) info "PathRegexp: /a%2fb matched neither probe router ($(probe_said))" ;;
esac
probe http re.test /a%2eb
info "an escaped dot: /a%2eb reaches the app as $seen"
echo

# A row's router names its priority: the length of its rule without the
# traversal term, which is the priority Traefik gave a container deployed
# before the term. On mix1.test the old container holds /api and the new one
# the host; on mix2.test the reverse. The path row wins for its prefix either
# way, as before.
echo "== priority, between containers deployed before and after the traversal term"
for target in 'mix1.test /api/x old' 'mix1.test /other new' 'mix2.test /api/x new' 'mix2.test /other old'; do
	set -- $target
	probe http "$1" "$2"
	if [ "$code" = 200 ] && [ "$backend" = "$3" ] && [ "$seen" = "$2" ]; then
		pass "http $1$2: the $3 container answers ($(probe_said))"
	else
		fail "http $1$2: want the $3 container" "$(probe_said)"
	fi
done
# api_priority ROUTER: the priority Traefik's API gives it
api_priority() { api "routers/$1" | sed -n 's/.*"priority":\([0-9-][0-9]*\).*/\1/p'; }
for target in 'gt-wiki wiki.test -' 'gt-wiki-tls wiki.test -' 'gt-hooks wiki.test /webhooks' 'gt-hooks-tls wiki.test /webhooks' \
	'gt-admin shop.test /admin' 'gt-admin-tls shop.test /admin' 'gt-mixold-api mix1.test /api' 'gt-mixold-host mix2.test -'; do
	set -- $target
	rule="Host(\`$2\`)"
	if [ "$3" != - ]; then rule="$rule && PathPrefix(\`$3\`)"; fi
	got=$(api_priority "$1@docker")
	if [ "$got" = "${#rule}" ]; then
		pass "router $1 has priority $got, the length of $rule"
	else
		fail "router $1 has priority ${#rule}, the length of $rule" "API: $(api "routers/$1@docker")"
	fi
done
# The routers outside the rows: the redirect Traefik's entrypoint redirect and
# what the API lists of its own.
redirect_api_port=$(host_port "$redirect_c" 8080)
redirect_api() { curl -sS --max-time 5 "http://127.0.0.1:$redirect_api_port/api/http/$1" 2>/dev/null; }
internal=$(redirect_api routers | grep -o '"name":"[^"]*@internal"' | sed 's/^"name":"//; s/"$//')
if [ -z "$internal" ]; then
	info "the redirect Traefik's API lists no internal routers"
fi
for name in $internal; do
	obj=$(redirect_api "routers/$name")
	prio=$(printf '%s' "$obj" | sed -n 's/.*"priority":\([0-9-]*\).*/\1/p')
	info "the redirect Traefik's internal router $name has priority ${prio:-unset}"
done
echo

# Under TLS, Outhaul's web entrypoint redirects every request to websecure, so
# only HTTPS routers serve. The protected /admin row's own TLS flag is off: it
# still has its HTTPS router, or shop.test's open one would answer /admin.
echo "== HTTPS behind the web-to-websecure redirect"
via=redirect
code=$(req https shop.test /)
if [ "$code" = 200 ] && [ "$(count_lines "$(cat "$work/b")" 'path: /')" = 1 ]; then
	pass "https shop.test/ (open, TLS on): served"
else
	fail "https shop.test/ (open, TLS on): want 200 from the backend" "status=$code" "$(cat "$work/b")"
fi
tid=$((tid + 1))
code=$(req http shop.test /admin/x -H "X-Gatetest-Id: $tid")
loc=$(header Location)
case $code:$loc in
30[178]:https://shop.test/admin/x | 30[178]:https://shop.test:443/admin/x)
	if [ -z "$(gate_lines "$tid")" ] && ! grep -q '^path:' "$work/b"; then
		pass "http shop.test/admin/x: redirected to HTTPS ($code), no gate call and no backend"
	else
		fail "http shop.test/admin/x: want only the redirect" "gate saw: $(gate_lines "$tid")" "$(cat "$work/b")"
	fi
	;;
*) fail "http shop.test/admin/x: want a redirect to https://shop.test/admin/x" "status=$code location='$loc'" "$(cat "$work/b")" ;;
esac
tid=$((tid + 1))
code=$(req https shop.test /admin/x -H 'Accept: text/html' -H "X-Gatetest-Id: $tid")
if [ "$code" = 302 ] && [ "$(header Location)" = "https://outhaul.test/gate/start?rd=1" ] &&
	[ "$(header Set-Cookie)" = "gatetest=pending; Path=/; HttpOnly" ] && [ -n "$(gate_lines "$tid")" ] &&
	! grep -q '^path:' "$work/b"; then
	pass "https shop.test/admin/x (protected, its own TLS flag off), no cookie: the gate's 302, never the backend"
else
	fail "https shop.test/admin/x (protected, its own TLS flag off), no cookie: want the gate's 302" \
		"status=$code" "gate saw: $(gate_lines "$tid")" "$(cat "$work/b")"
fi
tid=$((tid + 1))
code=$(req https shop.test /admin/x -H 'Cookie: gatetest=ok' -H 'X-Outhaul-User: mallory' -H "X-Gatetest-Id: $tid")
body=$(cat "$work/b")
if [ "$code" = 200 ] && [ "$(count_lines "$body" 'h x-outhaul-user: dana')" = 1 ] &&
	! printf '%s\n' "$body" | grep -qi mallory && [ "$(count_lines "$body" 'path: /x')" = 1 ] &&
	[ -n "$(gate_lines "$tid")" ]; then
	pass "https shop.test/admin/x, gate says 200: dana on the stripped path /x (the row's own HTTPS router, not shop.test's)"
else
	fail "https shop.test/admin/x, gate says 200: want dana only, on path /x" "status=$code" "$body"
fi
via=main
echo

# The stub closes its port but keeps its address, as the host does when
# Outhaul stops. (Stopping its container would free the address, and a dial to
# a freed address waits for ARP to give up.)
echo "== the gate stopped"
docker kill -s USR1 "$gate_c" >/dev/null || die "stop the gate stub"
i=0
while [ $i -lt 10 ] && [ "$(req http wiki.test /page -H 'Cookie: gatetest=ok')" != 500 ]; do
	sleep 1
	i=$((i + 1))
done
for scheme in http https; do
	for target in "wiki.test /page" "shop.test /admin/x"; do
		set -- $target
		code=$(req "$scheme" "$1" "$2" -H 'Cookie: gatetest=ok' -H 'Accept: text/html')
		if [ "$code" = 500 ]; then
			pass "$scheme $1$2 (protected) with the gate down: 500, not the app, and not the alias (still up)"
		else
			fail "$scheme $1$2 (protected) with the gate down: want 500" "status=$code" "$(cat "$work/b")"
		fi
	done
	for target in "shop.test /cart" "wiki.test /webhooks"; do
		set -- $target
		code=$(req "$scheme" "$1" "$2")
		if [ "$code" = 200 ]; then
			pass "$scheme $1$2 (open) with the gate down: still 200"
		else
			fail "$scheme $1$2 (open) with the gate down: want 200" "status=$code" "$(cat "$work/b")"
		fi
	done
done
via=redirect
code=$(req https shop.test /admin/x -H 'Cookie: gatetest=ok' -H 'Accept: text/html')
if [ "$code" = 500 ]; then
	pass "https shop.test/admin/x behind the redirect, with the gate down: 500"
else
	fail "https shop.test/admin/x behind the redirect, with the gate down: want 500" "status=$code" "$(cat "$work/b")"
fi
via=main
echo

# Traefik's own account of the gate being down, and anything else it
# complained about. Informational: the assertions above decide the result.
echo "Traefik's errors:"
docker logs "$traefik_c" 2>&1 | grep -e ' ERR ' -e ' FTL ' | sed 's/^/  /' | tail -8
echo

finish
