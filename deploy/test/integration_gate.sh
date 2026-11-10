#!/usr/bin/env sh
# Proves the protected-route labels on a real Traefik. Not run by CI.
#
#   sh deploy/test/integration_gate.sh
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
# Every container and the network are named outhaul-gatetest-*, and are removed
# on exit, failure included. Prints one PASS or FAIL line per assertion and
# exits non-zero if any fails.
set -u

root=$(cd "$(dirname "$0")/../.." && pwd)
net=outhaul-gatetest-net
traefik_c=outhaul-gatetest-traefik
trusting_c=outhaul-gatetest-traefik-trusting
redirect_c=outhaul-gatetest-traefik-redirect
unmapped_c=outhaul-gatetest-traefik-unmapped
app_c=outhaul-gatetest-app
gate_c=outhaul-gatetest-gate
alias_c=outhaul-gatetest-alias
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
	docker rm -f "$traefik_c" "$trusting_c" "$redirect_c" "$unmapped_c" "$app_c" "$gate_c" "$alias_c" >/dev/null 2>&1 || true
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
routes="gt-wiki,wiki.test,,,protected gt-hooks,wiki.test,/webhooks,,open gt-shop,shop.test,,,open gt-admin,shop.test,/admin,/,protected,notls"
# shellcheck disable=SC2086 # one word per route
"$work/gatelabels" -tls -port 8000 -admin-port "$admin_port" $routes >"$work/labels" || die "gatelabels"
printf 'outhaul.gatetest=true\n' >>"$work/labels"
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

api_version=$(docker version --format '{{.Server.APIVersion}}') || die "docker version"
docker network create "$net" >/dev/null || die "create network $net"
docker run -d --name "$gate_c" --network "$net" \
	"$python_image" python -u -c "$gate_py" >/dev/null || die "start the gate stub"
gate_ip=$(docker inspect --format '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "$gate_c")
[ -n "$gate_ip" ] || die "find the gate stub's address"
docker run -d --name "$alias_c" --network "$net" --network-alias host.docker.internal \
	"$python_image" python -u -c "$alias_py" >/dev/null || die "start the alias stub"
docker run -d --name "$app_c" --network "$net" --label-file "$work/labels" \
	"$python_image" python -u -c "$echo_py" >/dev/null || die "start the echo backend"
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
start_traefik "$trusting_c" mapped \
	--entrypoints.web.forwardedheaders.insecure=true \
	--entrypoints.websecure.forwardedheaders.insecure=true
start_traefik "$redirect_c" mapped \
	--entrypoints.web.http.redirections.entrypoint.to=websecure \
	--entrypoints.web.http.redirections.entrypoint.scheme=https
start_traefik "$unmapped_c" unmapped

host_port() { # container, container port
	docker port "$1" "$2/tcp" | sed -n 's/^127\.0\.0\.1:\([0-9][0-9]*\)$/\1/p' | head -1
}
http_port=$(host_port "$traefik_c" 80)
https_port=$(host_port "$traefik_c" 443)
api_port=$(host_port "$traefik_c" 8080)
trusting_http_port=$(host_port "$trusting_c" 80)
trusting_https_port=$(host_port "$trusting_c" 443)
redirect_http_port=$(host_port "$redirect_c" 80)
redirect_https_port=$(host_port "$redirect_c" 443)
unmapped_http_port=$(host_port "$unmapped_c" 80)
unmapped_https_port=$(host_port "$unmapped_c" 443)
[ -n "$http_port" ] && [ -n "$https_port" ] && [ -n "$api_port" ] &&
	[ -n "$trusting_http_port" ] && [ -n "$trusting_https_port" ] &&
	[ -n "$redirect_http_port" ] && [ -n "$redirect_https_port" ] &&
	[ -n "$unmapped_http_port" ] && [ -n "$unmapped_https_port" ] || die "find Traefik's loopback ports"
echo "Main Traefik $traefik_image on 127.0.0.1: web $http_port, websecure $https_port, api $api_port"
echo "Trusting Traefik on 127.0.0.1: web $trusting_http_port, websecure $trusting_https_port"
echo "Redirecting Traefik on 127.0.0.1: web $redirect_http_port, websecure $redirect_https_port"
echo "Unmapped Traefik on 127.0.0.1: web $unmapped_http_port, websecure $unmapped_https_port"
echo "Gate stub at $gate_ip, mapped as host.docker.internal in every Traefik but the unmapped one"

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

for via in main trusting redirect unmapped; do
	if [ "$via" = redirect ]; then first=https; else first=http; fi
	ready=no
	i=0
	while [ $i -lt 30 ]; do
		if [ "$(req "$first" shop.test /)" = 200 ] && [ "$(req https shop.test /)" = 200 ]; then
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

tid=0
echo "== the labels as Docker and Traefik keep them"
for c in "$traefik_c" "$trusting_c" "$redirect_c" "$unmapped_c" "$app_c" "$gate_c" "$alias_c"; do
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
	'traefik.http.routers.outhaul-gt-d3-tls.middlewares: outhaul-gt-d3-gate,outhaul-gt-d3-noalias'; do
	if printf '%s\n' "$merged" | grep -qF -- "$want"; then
		pass "docker compose keeps $want"
	else
		fail "docker compose keeps $want" "$(printf '%s\n' "$merged" | grep -i -e outhaul_user -e outhaul-user -e trustforward -e d3-tls -e error | head -8)"
	fi
done
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

printf '%d passed, %d failed\n' "$passed" "$failed"
[ "$failed" -eq 0 ]
