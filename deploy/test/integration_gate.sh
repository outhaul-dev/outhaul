#!/usr/bin/env sh
# Proves the protected-route labels on a real Traefik. Not run by CI.
#
#   sh deploy/test/integration_gate.sh
#
# Needs Docker and Go. It starts, on one scratch network:
#   - Traefik (GATETEST_TRAEFIK_IMAGE, default traefik:v3.7.6), on loopback
#     ports only, seeing only containers labelled outhaul.gatetest=true,
#     configured as Outhaul runs it;
#   - a second Traefik whose entrypoints trust a client's X-Forwarded-*
#     headers. Outhaul's own don't, and that alone hides a gate that trusts
#     them, so the X-Forwarded-* checks also run against this one;
#   - an echo backend carrying the labels traefik.RouteLabels gives four rows
#     (printed by deploy/test/gatelabels, so this covers the real label code):
#       wiki.test             protected
#       wiki.test/webhooks    open        (an open path under a protected host)
#       shop.test             open
#       shop.test/admin       protected, stripped to /  (a protected path under an open host)
#   - a gate stub standing in for Outhaul's /gate/check. It is reachable as
#     host.docker.internal on the admin port, so the labels use the address
#     config.GateAddress really gives. It answers 200 with X-Outhaul-User: dana
#     when the request carries the cookie gatetest=ok, and otherwise a 302 with
#     a Set-Cookie. It logs every header it receives.
#
# Every container and the network are named outhaul-gatetest-*, and are removed
# on exit, failure included. Prints one PASS or FAIL line per assertion and
# exits non-zero if any fails.
set -u

root=$(cd "$(dirname "$0")/../.." && pwd)
net=outhaul-gatetest-net
traefik_c=outhaul-gatetest-traefik
trusting_c=outhaul-gatetest-traefik-trusting
app_c=outhaul-gatetest-app
gate_c=outhaul-gatetest-gate
traefik_image=${GATETEST_TRAEFIK_IMAGE:-traefik:v3.7.6}
python_image=python:3-slim
admin_port=8080
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
	docker rm -f "$traefik_c" "$trusting_c" "$app_c" "$gate_c" >/dev/null 2>&1 || true
	docker network rm "$net" >/dev/null 2>&1 || true
	rm -rf "$work"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

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
routes="gt-wiki,wiki.test,,,protected gt-hooks,wiki.test,/webhooks,,open gt-shop,shop.test,,,open gt-admin,shop.test,/admin,/,protected"
# shellcheck disable=SC2086 # one word per route
"$work/gatelabels" -tls -port 8000 -admin-port "$admin_port" $routes >"$work/labels" || die "gatelabels"
printf 'outhaul.gatetest=true\ntraefik.enable=true\n' >>"$work/labels"
echo "labels on the backend:"
sed 's/^/  /' "$work/labels"

echo_py=$(
	cat <<'EOF'
import http.server
class H(http.server.BaseHTTPRequestHandler):
    def answer(self):
        lines = ["path: " + self.path]
        lines += ["h %s: %s" % (k.lower(), v) for k, v in self.headers.items()]
        body = ("\n".join(lines) + "\n").encode()
        self.send_response(200)
        self.send_header("Content-Type", "text/plain")
        self.send_header("Content-Length", str(len(body)))
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
import http.server, sys
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

api_version=$(docker version --format '{{.Server.APIVersion}}') || die "docker version"
docker network create "$net" >/dev/null || die "create network $net"
docker run -d --name "$gate_c" --network "$net" --network-alias host.docker.internal \
	"$python_image" python -u -c "$gate_py" >/dev/null || die "start the gate stub"
docker run -d --name "$app_c" --network "$net" --label-file "$work/labels" \
	"$python_image" python -u -c "$echo_py" >/dev/null || die "start the echo backend"
start_traefik() { # name, extra Traefik arguments...
	name=$1
	shift
	docker run -d --name "$name" --network "$net" \
		-p 127.0.0.1::80 -p 127.0.0.1::443 -p 127.0.0.1::8080 \
		-v "$docker_sock:/var/run/docker.sock:ro" \
		-e "DOCKER_API_VERSION=$api_version" \
		"$traefik_image" \
		--providers.docker=true \
		--providers.docker.exposedbydefault=false \
		"--providers.docker.network=$net" \
		'--providers.docker.constraints=Label(`outhaul.gatetest`,`true`)' \
		--entrypoints.web.address=:80 \
		--entrypoints.websecure.address=:443 \
		--api.insecure=true \
		--log.level=INFO --log.nocolor=true "$@" >/dev/null || die "start $name"
}
start_traefik "$traefik_c"
start_traefik "$trusting_c" \
	--entrypoints.web.forwardedheaders.insecure=true \
	--entrypoints.websecure.forwardedheaders.insecure=true

host_port() { # container, container port
	docker port "$1" "$2/tcp" | sed -n 's/^127\.0\.0\.1:\([0-9][0-9]*\)$/\1/p' | head -1
}
http_port=$(host_port "$traefik_c" 80)
https_port=$(host_port "$traefik_c" 443)
api_port=$(host_port "$traefik_c" 8080)
trusting_http_port=$(host_port "$trusting_c" 80)
trusting_https_port=$(host_port "$trusting_c" 443)
[ -n "$http_port" ] && [ -n "$https_port" ] && [ -n "$api_port" ] &&
	[ -n "$trusting_http_port" ] && [ -n "$trusting_https_port" ] || die "find Traefik's loopback ports"
echo "Traefik $traefik_image on 127.0.0.1: web $http_port, websecure $https_port, api $api_port"
echo "Trusting Traefik on 127.0.0.1: web $trusting_http_port, websecure $trusting_https_port"

# req SCHEME HOST PATH [curl args...]: prints the status code; the response
# headers go to $work/h and the body to $work/b. It goes to the Traefik
# configured as Outhaul runs it, or to the trusting one when $via is "trusting".
via=outhaul
req() {
	scheme=$1 host=$2 path=$3
	shift 3
	case $via:$scheme in
	trusting:https) port=$trusting_https_port ;;
	trusting:*) port=$trusting_http_port ;;
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
count_lines() { # how many lines of $1 equal $2
	printf '%s\n' "$1" | grep -cFx -- "$2"
}

for via in outhaul trusting; do
	ready=no
	i=0
	while [ $i -lt 30 ]; do
		if [ "$(req http shop.test /)" = 200 ] && [ "$(req https shop.test /)" = 200 ]; then
			ready=yes
			break
		fi
		sleep 1
		i=$((i + 1))
	done
	[ "$ready" = yes ] || die "the $via Traefik never served shop.test (the open row). Last status: $(req http shop.test /)"
done
via=outhaul
echo

echo "== the labels as Docker and Traefik keep them"
label=traefik.http.middlewares.gt-shop-noident.headers.customrequestheaders.X-Outhaul-User
got=$(docker inspect --format "{{range \$k, \$v := .Config.Labels}}{{if eq \$k \"$label\"}}present:[{{\$v}}]{{end}}{{end}}" "$app_c")
if [ "$got" = "present:[]" ]; then
	pass "the container keeps the empty-valued strip label ($label)"
else
	fail "the container keeps the empty-valued strip label" "docker inspect gave: '$got'"
fi
mw=$(api middlewares/gt-shop-noident@docker)
case $mw in
*'"status":"enabled"'*'"customRequestHeaders":{"X-Outhaul-User":""}'* | *'"customRequestHeaders":{"X-Outhaul-User":""}'*'"status":"enabled"'*)
	pass "Traefik loads gt-shop-noident with customRequestHeaders X-Outhaul-User = \"\"" ;;
*) fail "Traefik loads gt-shop-noident with an empty X-Outhaul-User" "API: $mw" ;;
esac
for r in gt-wiki gt-wiki-tls gt-hooks gt-hooks-tls gt-shop gt-shop-tls gt-admin gt-admin-tls; do
	info=$(api "routers/$r@docker")
	case $info in
	*'"status":"enabled"'*) pass "router $r is enabled" ;;
	*) fail "router $r is enabled" "API: $info" ;;
	esac
done
info=$(api routers/gt-admin@docker)
case $info in
*'"middlewares":["gt-admin-gate@docker","gt-admin-strip@docker"]'*) pass "gt-admin runs the gate before the strip" ;;
*) fail "gt-admin runs the gate before the strip" "API: $info" ;;
esac
compose_out=$("$work/gatelabels" -compose -tls -port 8000 'gt,wiki.test,,,protected' 'gt,wiki.test,/webhooks,,open')
printf 'services:\n  web:\n    image: %s\n' "$python_image" >"$work/compose.yml"
printf '%s\n' "$compose_out" >"$work/override.yml"
merged=$(docker compose -p outhaul-gatetest-compose -f "$work/compose.yml" -f "$work/override.yml" config 2>&1)
if printf '%s\n' "$merged" | grep -qF 'traefik.http.middlewares.outhaul-gt-d2-noident.headers.customrequestheaders.X-Outhaul-User: ""'; then
	pass "docker compose keeps the empty strip label from the override"
else
	fail "docker compose keeps the empty strip label from the override" "$(printf '%s\n' "$merged" | grep -i -e noident -e error | head -5)"
fi
echo

tid=0
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

	# An open row strips a client-sent X-Outhaul-User, and still serves.
	tid=$((tid + 1))
	code=$(req "$scheme" shop.test /cart -H 'X-Outhaul-User: mallory' -H "X-Gatetest-Id: $tid")
	body=$(cat "$work/b")
	if [ "$code" = 200 ] && ! printf '%s\n' "$body" | grep -qi 'x-outhaul-user'; then
		pass "$scheme shop.test/cart (open host): served, and the forged X-Outhaul-User is stripped"
	else
		fail "$scheme shop.test/cart: want 200 with no X-Outhaul-User" "status=$code" "$body"
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
		code=$(req "$scheme" wiki.test "$p" -H 'Accept: text/html' -H 'X-Outhaul-User: mallory' -H "X-Gatetest-Id: $tid")
		body=$(cat "$work/b")
		if [ "$code" = 200 ] && [ "$(count_lines "$body" "path: $p")" = 1 ] &&
			! printf '%s\n' "$body" | grep -qi 'x-outhaul-user' && [ -z "$(gate_lines "$tid")" ]; then
			pass "$scheme wiki.test$p (open path, protected host): reaches the backend, no gate call, forged header stripped"
		else
			fail "$scheme wiki.test$p: want 200 from the backend with no gate call and no X-Outhaul-User" \
				"status=$code" "$body" "gate saw: $(gate_lines "$tid")"
		fi
	done

	# What the stub receives: the browser's Cookie and Accept, and X-Forwarded-*
	# set by Traefik, not by the client, even through entrypoints that trust a
	# client's X-Forwarded-* (only the gate's own setting stops them there).
	for via in outhaul trusting; do
		if [ "$scheme" = https ]; then forged_proto=http; else forged_proto=https; fi
		tid=$((tid + 1))
		code=$(req "$scheme" wiki.test '/page?q=1' \
			-H 'Cookie: gatetest=ok; theme=dark' -H 'Accept: text/html,application/xhtml+xml' \
			-H 'X-Forwarded-Host: evil.test' -H 'X-Forwarded-Uri: /webhooks' -H 'X-Forwarded-Method: POST' \
			-H "X-Forwarded-Proto: $forged_proto" -H "X-Gatetest-Id: $tid")
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
	done
	via=outhaul
	echo
done

echo "== the gate stopped"
docker stop -t 1 "$gate_c" >/dev/null || die "stop the gate stub"
for scheme in http https; do
	for target in "wiki.test /page" "shop.test /admin/x"; do
		set -- $target
		code=$(req "$scheme" "$1" "$2" -H 'Cookie: gatetest=ok' -H 'Accept: text/html')
		if [ "$code" = 500 ]; then
			pass "$scheme $1$2 (protected) with the gate down: 500, not the app"
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
echo

# Traefik's own account of the gate being down, and anything else it
# complained about. Informational: the assertions above decide the result.
echo "Traefik's errors:"
docker logs "$traefik_c" 2>&1 | grep -e ' ERR ' -e ' FTL ' | sed 's/^/  /' | tail -8
echo

printf '%d passed, %d failed\n' "$passed" "$failed"
[ "$failed" -eq 0 ]
