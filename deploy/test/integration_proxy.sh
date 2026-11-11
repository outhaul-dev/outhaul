#!/usr/bin/env sh
# Runs a real Outhaul, Traefik and Outhaul's own Docker daemon behind a
# stand-in CDN, and checks sign-in, protected routes and webhooks through it.
# Not run by CI.
#
#   sh deploy/test/integration_proxy.sh
#
# It builds deploy/test/Dockerfile.proxytest (Debian 13 with systemd and its
# own Docker daemon, plus the outhaul binary and deploy/test/oidcstub) and
# starts it privileged as outhaul-proxytest-*. Outhaul runs inside with the
# local CA, OUTHAUL_HTTP_PORT=off, a required header, OUTHAUL_ADMIN_DIRECT=off
# and a public URL. nginx inside that container is the stand-in CDN: it
# terminates TLS with its own certificate, forwards to Traefik on 8443 with the
# request's Host as both Host and SNI, verifies the origin certificate against
# the local CA's root, and sets X-Probe-Id itself, overwriting any the client
# sent. All requests go through it with curl --resolve. A stand-in OIDC
# provider (`oidcstub`) is the identity provider.
#
# The container and its image are named outhaul-proxytest-*, and the container
# is removed on exit, failure included. Prints PASS, FAIL and INFO lines, and
# exits non-zero if any assertion fails.
set -u

root=$(cd "$(dirname "$0")/../.." && pwd)
img=outhaul-proxytest-img
cid=outhaul-proxytest
public=https://outhaul.shared.demo.example.com
publichost=outhaul.shared.demo.example.com
apphost=app.shared.demo.example.com
header_value=abc-123
admin_port=8080
if ! docker info >/dev/null 2>&1; then
	echo "BLOCKED: Docker is not usable here"
	exit 2
fi
if [ -n "$(docker ps -aq --filter name=outhaul-proxytest)" ]; then
	echo "BLOCKED: outhaul-proxytest-* containers already exist; remove them first:"
	docker ps -a --filter name=outhaul-proxytest --format '  container {{.Names}}'
	exit 2
fi

work=$(mktemp -d)
cleanup() {
	if [ "${PROXYTEST_KEEP:-0}" = 1 ]; then
		echo "KEEP $cid (work dir $work)"
		return
	fi
	docker rm -f "$cid" >/dev/null 2>&1 || true
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
	docker exec "$cid" sh -c 'tail -40 /work/outhaul.log 2>/dev/null' 2>/dev/null || true
	echo "nginx:"
	docker exec "$cid" nginx -t 2>&1 || true
	docker exec "$cid" sh -c 'tail -20 /var/log/nginx/error.log 2>/dev/null' 2>&1 || true
	exit 1
}
ce() { docker exec "$cid" "$@"; }
csh() { docker exec "$cid" sh -c "$1"; }

echo "== build the test container"
docker build -f "$root/deploy/test/Dockerfile.proxytest" -t "$img" "$root" >"$work/build.log" 2>&1 ||
	{ echo "ERROR docker build"; tail -20 "$work/build.log"; exit 1; }
docker run -d --privileged --cgroupns=host --security-opt seccomp=unconfined \
	--name "$cid" --tmpfs /run --tmpfs /run/lock -v /sys/fs/cgroup:/sys/fs/cgroup:rw \
	"$img" >/dev/null || die "start the test container"
i=0
while [ $i -lt 30 ]; do
	if docker exec "$cid" systemctl is-system-running >/dev/null 2>&1; then break; fi
	sleep 1
	i=$((i + 1))
done
ce systemctl start docker >/dev/null 2>&1 || true
i=0
while [ $i -lt 60 ]; do
	if ce docker info >/dev/null 2>&1; then break; fi
	sleep 1
	i=$((i + 1))
done
[ $i -lt 60 ] || die "the inner Docker daemon never started"

# --- inside scripts -------------------------------------------------------

cat >"$work/start-outhaul.sh" <<'EOF'
#!/bin/sh
set -eu
value=$1
if [ -f /work/outhaul.pid ]; then
	kill "$(cat /work/outhaul.pid)" >/dev/null 2>&1 || true
	sleep 1
fi
OUTHAUL_DATA_DIR=/var/lib/outhaul \
OUTHAUL_LISTEN_ADDR=:8080 \
OUTHAUL_PUBLIC_URL=https://outhaul.shared.demo.example.com \
OUTHAUL_LOCAL_CA=true \
OUTHAUL_HTTP_PORT=off \
OUTHAUL_HTTPS_PORT=443 \
OUTHAUL_NETWORK=outhaul \
OUTHAUL_REQUIRE_HEADER_NAME=X-Probe-Id \
OUTHAUL_REQUIRE_HEADER_VALUE="$value" \
OUTHAUL_ADMIN_DIRECT=off \
	outhaul serve >/work/outhaul.log 2>&1 &
echo $! >/work/outhaul.pid
EOF
cat >"$work/write-nginx.sh" <<'EOF'
#!/bin/sh
set -eu
value=$1
cat >/etc/nginx/conf.d/cdn.conf <<CONF
server {
    listen 8443 ssl;
    server_name _;
    ssl_certificate /work/cdn.crt;
    ssl_certificate_key /work/cdn.key;
    location / {
        proxy_pass https://127.0.0.1:443;
        proxy_http_version 1.1;
        proxy_set_header Host \$host;
        proxy_ssl_server_name on;
        proxy_ssl_name \$host;
        proxy_ssl_verify on;
        proxy_ssl_trusted_certificate /work/outhaul-ca.pem;
        proxy_set_header X-Probe-Id $value;
    }
}
CONF
if [ -f /run/nginx.pid ] && kill -0 "$(cat /run/nginx.pid)" 2>/dev/null; then nginx -s reload; else nginx; fi
EOF
chmod +x "$work/start-outhaul.sh" "$work/write-nginx.sh"
docker cp "$work/start-outhaul.sh" "$cid:/work-start-outhaul.sh"
docker cp "$work/write-nginx.sh" "$cid:/work-write-nginx.sh"
ce mkdir -p /work /usr/share/nginx/html

# CDN certificate and key, inside the container.
ce openssl req -x509 -newkey rsa:2048 -nodes -keyout /work/cdn.key -out /work/cdn.crt -days 1 \
	-subj /CN=cdn -addext "subjectAltName=DNS:*.shared.demo.example.com,DNS:outhaul.shared.demo.example.com,DNS:app.shared.demo.example.com" \
	>/dev/null 2>&1 || die "generate the CDN certificate"

# A tiny Dockerfile app, with a repository Outhaul clones locally.
ce mkdir -p /work/repo
ce sh -c 'cat >/work/repo/Dockerfile <<"EOF"
FROM python:3-slim
WORKDIR /app
COPY server.py .
CMD ["python", "server.py"]
EOF
cat >/work/repo/server.py <<"EOF"
import http.server, os
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        body = ("user: %s\npath: %s\n" % (self.headers.get("X-Outhaul-User", ""), self.path)).encode()
        self.send_response(200)
        self.send_header("Content-Type", "text/plain")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)
    def log_message(self, *args):
        pass
http.server.ThreadingHTTPServer(("", int(os.environ.get("PORT", "8080"))), H).serve_forever()
EOF
cd /work/repo && git init -q -b main && git config user.email test@example.com && git config user.name test && git add . && git commit -qm init && mkdir -p /work/www && git clone -q --bare /work/repo /work/www/repo.git && cd /work/www/repo.git && git update-server-info'
ce sh -c 'cd /work/www && python3 -m http.server 8000 >/work/git-http.log 2>&1 &'

# oidcstub on the container's loopback.
ce sh -c 'oidcstub -listen 127.0.0.1:9000 -issuer http://127.0.0.1:9000 -client-id outhaul -client-secret shh -email dana@example.com >/work/oidc.log 2>&1 &'
i=0
while [ $i -lt 20 ]; do
	if ce curl -sS --max-time 1 http://127.0.0.1:9000/.well-known/openid-configuration >/dev/null 2>&1; then break; fi
	sleep 1
	i=$((i + 1))
done
[ $i -lt 20 ] || die "oidcstub never answered"

echo "== start Outhaul"
ce sh /work-start-outhaul.sh "$header_value" || die "start Outhaul"
i=0
while [ $i -lt 60 ]; do
	if ce curl -sS --max-time 1 http://127.0.0.1:8080/healthz >/dev/null 2>&1; then break; fi
	sleep 1
	i=$((i + 1))
done
[ $i -lt 60 ] || die "Outhaul never answered /healthz"
i=0
while [ $i -lt 120 ]; do
	if ce sh -c 'ss -ltn | grep -q ":443 "'; then break; fi
	sleep 2
	i=$((i + 2))
done
[ $i -lt 120 ] || die "Traefik never listened on 443"
ce sh -c 'OUTHAUL_DATA_DIR=/var/lib/outhaul outhaul ca root >/work/outhaul-ca.pem' || die "write the local CA root"
ce sh /work-write-nginx.sh "$header_value" || die "start nginx"
i=0
while [ $i -lt 30 ]; do
	if ce curl -sk --max-time 2 --connect-to "$publichost:443:127.0.0.1:8443" --cacert /work/cdn.crt "$public/login" >/dev/null 2>&1; then break; fi
	sleep 1
	i=$((i + 1))
done
[ $i -lt 30 ] || die "nginx never answered"

# curl through the stand-in CDN. Cookies are kept in /work/jar.
cdn() {
	ce curl -sS -L -c /work/jar -b /work/jar -H 'Accept: text/html,application/xhtml+xml' \
		--connect-to "$publichost:443:127.0.0.1:8443" --connect-to "$apphost:443:127.0.0.1:8443" \
		--cacert /work/cdn.crt "$@"
}
# Direct to Traefik, past the stand-in.
direct() {
	host=$1 path=$2
	shift 2
	ce curl -sS -k --max-time 20 --resolve "$host:443:127.0.0.1" "https://$host$path" "$@"
}

echo "== first-boot setup and password sign-in"
setup_url=$(ce sh -c 'grep -o "https://outhaul.shared.demo.example.com/setup?token=[a-f0-9]*" /work/outhaul.log | head -1')
[ -n "$setup_url" ] || die "Outhaul printed no setup URL"
token=${setup_url#*token=}
code=$(ce curl -sS -o /work/setup.html -w '%{http_code}' -c /work/admin.jar -b /work/admin.jar \
	--connect-to "$publichost:443:127.0.0.1:8443" --cacert /work/cdn.crt "$setup_url")
[ "$code" = 200 ] && pass "the setup URL opens through the stand-in" || fail "the setup URL opens through the stand-in" "status=$code"
code=$(ce curl -sS -o /dev/null -w '%{http_code}' -c /work/admin.jar -b /work/admin.jar \
	--connect-to "$publichost:443:127.0.0.1:8443" --cacert /work/cdn.crt \
	-X POST "$public/setup" --data-urlencode "token=$token" --data-urlencode username=admin --data-urlencode password=supersecret)
[ "$code" = 303 ] && pass "the first system admin is created through the stand-in" || fail "the first system admin is created through the stand-in" "status=$code"

ce rm -f /work/admin.jar
code=$(ce curl -sS -D /work/login.h -o /dev/null -w '%{http_code}' -c /work/admin.jar \
	--connect-to "$publichost:443:127.0.0.1:8443" --cacert /work/cdn.crt \
	-X POST "$public/login" --data-urlencode username=admin --data-urlencode password=supersecret)
cookie=$(ce sh -c 'grep -i "^set-cookie: outhaul_session" /work/login.h | head -1 | tr -d "\r"')
case $code:$cookie in
303:*Secure*) pass "password sign-in works and the session cookie is Secure" ;;
*) fail "password sign-in works and the session cookie is Secure" "status=$code cookie='$cookie'" ;;
esac

echo "== SSO"
code=$(ce curl -sS -o /work/sso.html -w '%{http_code}' -c /work/admin.jar -b /work/admin.jar \
	--connect-to "$publichost:443:127.0.0.1:8443" --cacert /work/cdn.crt \
	-X POST "$public/settings/sso" --data-urlencode "issuer=http://127.0.0.1:9000" \
	--data-urlencode "client_id=outhaul" --data-urlencode "client_secret=shh")
case $code in
200 | 303) pass "the SSO settings are accepted through the stand-in" ;;
*) fail "the SSO settings are accepted through the stand-in" "status=$code" ;;
esac
ce rm -f /work/jar
code=$(cdn -o /work/sso-login.html -w '%{http_code}' "$public/auth/sso/start")
case $code in
200 | 403)
	if ce sh -c 'grep -q "Signed in as dana@example.com" /work/sso-login.html'; then
		pass "SSO sign-in ends signed in as dana@example.com"
	else
		fail "SSO sign-in ends signed in as dana@example.com" "status=$code" "$(ce sh -c 'head -c 300 /work/sso-login.html')"
	fi
	;;
*) fail "SSO sign-in ends signed in as dana@example.com" "status=$code" "$(ce sh -c 'head -c 300 /work/sso-login.html')" ;;
esac

echo "== an app with a protected row"
code=$(ce curl -sS -o /work/create.html -w '%{http_code}' -c /work/admin.jar -b /work/admin.jar \
	--connect-to "$publichost:443:127.0.0.1:8443" --cacert /work/cdn.crt \
	-X POST "$public/apps" --data-urlencode name=app --data-urlencode source=push \
	--data-urlencode kind=dockerfile --data-urlencode dockerfile_path=Dockerfile \
	--data-urlencode branch=main --data-urlencode auto_deploy=on \
	--data-urlencode domain=other.shared.demo.example.com)
[ "$code" = 303 ] || die "create app: status $code $(ce sh -c 'grep -o "alert alert-error[^<]*" /work/create.html | head -1')"
ce curl -sS -o /work/apps.html -c /work/admin.jar -b /work/admin.jar --connect-to "$publichost:443:127.0.0.1:8443" --cacert /work/cdn.crt "$public/apps"
app=$(ce sh -c 'grep -o "/apps/[0-9]*" /work/apps.html | head -1 | cut -d/ -f3')
[ -n "$app" ] || die "the apps page does not link the new app"
# Seed the bare repo a git push would fill.
ce sh -c 'rm -rf /var/lib/outhaul/git/app.git && git clone -q --bare /work/repo /var/lib/outhaul/git/app.git'
pass "an app is created through the stand-in (id $app)"
ce curl -sS -o /dev/null -c /work/admin.jar -b /work/admin.jar --connect-to "$publichost:443:127.0.0.1:8443" --cacert /work/cdn.crt \
	-X POST "$public/apps/$app/domains" --data-urlencode host_kind=custom --data-urlencode "host=$apphost" --data-urlencode protected=on
ce curl -sS -o /dev/null -c /work/admin.jar -b /work/admin.jar --connect-to "$publichost:443:127.0.0.1:8443" --cacert /work/cdn.crt \
	-X POST "$public/apps/$app/domains" --data-urlencode host_kind=custom --data-urlencode "host=$apphost" --data-urlencode path=/open
ce curl -sS -o /dev/null -c /work/admin.jar -b /work/admin.jar --connect-to "$publichost:443:127.0.0.1:8443" --cacert /work/cdn.crt \
	-X POST "$public/apps/$app/deploy"
i=0
while [ $i -lt 180 ]; do
	code=$(ce curl -sS -o /work/open.html -w '%{http_code}' --connect-to "$apphost:443:127.0.0.1:8443" --cacert /work/cdn.crt "https://$apphost/open")
	[ "$code" = 200 ] && break
	sleep 2
	i=$((i + 2))
done
[ "$code" = 200 ] || die "the app never answered its open path"
pass "the app is running and its open path answers without sign-in"
ce rm -f /work/jar
read -r code gate_url <<EOF
$(cdn -o /work/gate.html -w '%{http_code} %{url_effective}' "https://$apphost/")
EOF
if [ "$code" = 200 ] && { case $gate_url in */gate/start*) true ;; *) false ;; esac; }; then
	pass "an anonymous page load is sent to sign in on the public URL"
else
	fail "an anonymous page load is sent to sign in on the public URL" "status=$code" "$(ce sh -c 'head -c 300 /work/gate.html')"
fi
sso=$(ce sh -c 'grep -o "href=\"/auth/sso/start[^\"]*\"" /work/gate.html | head -1 | sed "s/^href=\"//; s/\"$//" | sed "s/&amp;/\\&/g"')
if [ -n "$sso" ]; then
	code=$(cdn -o /work/app-signed.html -w '%{http_code}' "$public$sso")
	if [ "$code" = 200 ] && ce sh -c 'grep -q "user: dana@example.com" /work/app-signed.html'; then
		pass "after signing in, the app receives X-Outhaul-User"
	else
		fail "after signing in, the app receives X-Outhaul-User" "status=$code" "$(ce sh -c 'head -c 300 /work/app-signed.html')"
	fi
	if ce sh -c 'grep -q "__Host-outhaul_route" /work/jar'; then
		pass "the __Host-outhaul_route cookie is set on the app's host"
	else
		fail "the __Host-outhaul_route cookie is set on the app's host" "$(ce sh -c 'cat /work/jar')"
	fi
else
	fail "the sign-in page offers SSO" "$(ce sh -c 'head -c 500 /work/gate.html')"
fi
ce rm -f /work/jar
code=$(ce curl -sS -o /work/open2.html -w '%{http_code}' --connect-to "$apphost:443:127.0.0.1:8443" --cacert /work/cdn.crt "https://$apphost/open")
if [ "$code" = 200 ] && ce sh -c 'grep -q "path: /open" /work/open2.html'; then
	pass "the open path under the protected host still answers without sign-in"
else
	fail "the open path under the protected host still answers without sign-in" "status=$code"
fi

echo "== a signed webhook delivery"
ce curl -sS -o /work/app-page.html -c /work/admin.jar -b /work/admin.jar --connect-to "$publichost:443:127.0.0.1:8443" --cacert /work/cdn.crt "$public/apps/$app"
hook=$(ce sh -c 'grep -o "https://outhaul[^<]*/webhooks/app/[A-Za-z0-9_-]*" /work/app-page.html | head -1')
secret=$(ce sh -c 'sed -n "/secret-reveal/,/<\/details>/p" /work/app-page.html | grep -oE "[0-9a-f]{32}" | head -1')
if [ -z "$hook" ] || [ -z "$secret" ]; then
	fail "the app page shows its webhook URL and secret" "url='$hook' secret='$secret'"
else
	body='{"ref":"refs/heads/main","after":"0123456789012345678901234567890123456789","repository":{"clone_url":"http://127.0.0.1:8000/repo.git"}}'
	ce sh -c "printf '%s' '$body' > /work/push.json"
	sig=$(ce sh -c "openssl dgst -sha256 -hmac '$secret' /work/push.json | sed 's/^.*= //'")
	code=$(ce curl -sS -o /work/hook.out -w '%{http_code}' --connect-to "$publichost:443:127.0.0.1:8443" --cacert /work/cdn.crt \
		-X POST "$hook" -H 'X-GitHub-Event: push' -H "X-Hub-Signature-256: sha256=$sig" -H 'Content-Type: application/json' --data-binary @/work/push.json)
	[ "$code" = 200 ] && pass "a push delivery signed with the app's secret is accepted through the stand-in" ||
		fail "a push delivery signed with the app's secret is accepted through the stand-in" "status=$code $(ce sh -c 'cat /work/hook.out')"
	before=$(ce sh -c 'grep -o "/deployments/[0-9]*" /work/app-page.html | sort -u | wc -l' | tr -d ' ')
	i=0
	while [ $i -lt 30 ]; do
		ce curl -sS -o /work/app-page2.html -c /work/admin.jar -b /work/admin.jar --connect-to "$publichost:443:127.0.0.1:8443" --cacert /work/cdn.crt "$public/apps/$app"
		after=$(ce sh -c 'grep -o "/deployments/[0-9]*" /work/app-page2.html | sort -u | wc -l' | tr -d ' ')
		[ "$after" -gt "$before" ] && break
		sleep 1
		i=$((i + 1))
	done
	[ "$after" -gt "$before" ] && pass "the delivery queued a deployment" ||
		fail "the delivery queued a deployment" "deployments $before -> $after"
fi

echo "== straight to Traefik, past the stand-in"
for target in "$apphost /" "$publichost /"; do
	set -- $target
	host=$1 path=$2
	code=$(direct "$host" "$path" -o /dev/null -w '%{http_code}')
	[ "$code" = 404 ] && pass "$host$path with no header gets 404" || fail "$host$path with no header gets 404" "status=$code"
	code=$(direct "$host" "$path" -H 'X-Probe-Id: wrong' -o /dev/null -w '%{http_code}')
	[ "$code" = 404 ] && pass "$host$path with a wrong header gets 404" || fail "$host$path with a wrong header gets 404" "status=$code"
done

echo "== the admin port"
got=$(ce curl -sS --max-time 2 http://127.0.0.1:$admin_port/healthz)
[ "$got" = ok ] && pass "the admin port answers on loopback" || fail "the admin port answers on loopback" "got '$got'"
container_ip=$(ce hostname -I | awk '{print $1}')
if ce curl -sS --max-time 2 "http://$container_ip:$admin_port/healthz" >/dev/null 2>&1; then
	fail "the container's main address refuses the admin port"
else
	pass "the container's main address refuses the admin port"
fi
listeners=$(ce sh -c "ss -ltn | grep ':$admin_port ' | wc -l" | tr -d ' ')
[ "$listeners" = 2 ] && pass "ss lists port 8080 on two addresses only" || fail "ss lists port 8080 on two addresses only" "count=$listeners" "$(ce sh -c "ss -ltn | grep ':$admin_port '")"

echo "== restart with another header value"
ce sh /work-write-nginx.sh def-456 >/dev/null 2>&1 || die "reload nginx with the new header"
ce sh /work-start-outhaul.sh def-456 || die "restart Outhaul with the new header"
i=0
while [ $i -lt 60 ]; do
	if ce curl -sS --max-time 1 http://127.0.0.1:$admin_port/healthz >/dev/null 2>&1; then break; fi
	sleep 1
	i=$((i + 1))
done
[ $i -lt 60 ] || die "Outhaul never came back"
code=$(direct "$apphost" /open -H 'X-Probe-Id: abc-123' -o /dev/null -w '%{http_code}')
[ "$code" = 404 ] && pass "the old header value stops working at once" || fail "the old header value stops working at once" "status=$code"
i=0
while [ $i -lt 120 ]; do
	code=$(ce curl -sS -o /work/after.html -w '%{http_code}' --connect-to "$apphost:443:127.0.0.1:8443" --cacert /work/cdn.crt "https://$apphost/open")
	[ "$code" = 200 ] && break
	sleep 2
	i=$((i + 2))
done
[ "$code" = 200 ] && pass "the app answers again without anyone deploying it" || fail "the app answers again without anyone deploying it" "status=$code"
if ce sh -c 'grep -q "ingress:" /work/outhaul.log'; then
	pass "the log has the ingress line naming the redeploy"
else
	fail "the log has the ingress line naming the redeploy" "$(ce sh -c 'grep -i ingress /work/outhaul.log | tail -3')"
fi

echo
finish
