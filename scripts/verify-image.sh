#!/usr/bin/env sh
# Prove a saved release image works the way an offline site runs it: load the
# tarball itself (not the local build cache), start it with a data volume,
# then check health, reported version, non-root process, HEALTHCHECK, writes to
# the volume, persistence into a fresh container, graceful stop, which DB
# drivers are linked in, and a bind-mounted host directory the way the admin
# guide runs it. docs/RELEASE_CHECKLIST.md says why each check exists.
#
# Usage: sh scripts/verify-image.sh <tarball> <image:tag> <oracle 0|1>
#
# For the Oracle image, SQLON_VERIFY_ORACLE also opens a real Oracle
# connection from inside the container:
#   SQLON_VERIFY_ORACLE='<docker-network> <host:port/service> <user> <password>'
set -eu
TAR="${1:?usage: verify-image.sh <tarball> <image:tag> <oracle 0|1>}"
IMG="${2:?usage: verify-image.sh <tarball> <image:tag> <oracle 0|1>}"
WANT_ORACLE="${3:?usage: verify-image.sh <tarball> <image:tag> <oracle 0|1>}"
VER="${IMG##*:v}"
N="sqlon-verify-$$"; VOL="$N-data"; TOK="verify-$$-token"; NET=""; BIND=""

fail() { echo "FAIL: $*" >&2; docker logs "$N" 2>&1 | tail -20 >&2 || true; exit 1; }
ok() { echo "  ok  $*"; }
cleanup() {
  docker rm -f "$N" >/dev/null 2>&1 || true
  docker volume rm "$VOL" >/dev/null 2>&1 || true
  # files in the bind dir belong to the container user; remove them as it
  if [ -n "$BIND" ]; then
    docker run --rm --user 0 --entrypoint sh -v "$BIND:/d" "$IMG" -c 'rm -rf /d/* /d/.[!.]*' >/dev/null 2>&1 || true
    rm -rf "$BIND"
  fi
}
trap cleanup EXIT INT TERM
json() { python3 -c "import sys, json; d = json.load(sys.stdin); print($1)"; }
api() { curl -fsS -H "X-Admin-Token: $TOK" -H 'Content-Type: application/json' "$@"; }

if [ -n "${SQLON_VERIFY_ORACLE:-}" ] && [ "$WANT_ORACLE" = 1 ]; then
  set -- $SQLON_VERIFY_ORACLE
  NET="--network $1"; ORA_CONNECT="$2"; ORA_USER="$3"; ORA_PW="$4"
fi

# start <volume name or host dir mounted as the data dir>
start() {
  docker run -d --name "$N" $NET -p 127.0.0.1::6767 -v "$1:/app/data/sqlon" \
    -e SQLON_ADMIN_TOKEN="$TOK" "$IMG" >/dev/null
  URL="http://127.0.0.1:$(docker port "$N" 6767/tcp | head -1 | sed 's/.*://')"
  i=0
  until [ "$(docker inspect -f '{{.State.Health.Status}}' "$N")" = healthy ]; do
    [ "$(docker inspect -f '{{.State.Running}}' "$N")" = true ] || fail "container exited"
    i=$((i + 1)); [ "$i" -le 60 ] || fail "not healthy after 120s"
    sleep 2
  done
}

echo "verify $TAR -> $IMG"
( cd "$(dirname "$TAR")" && sha256sum -c "$(basename "$TAR").sha256" >/dev/null ) || fail "sha256 sidecar does not match"
ok "sha256 sidecar matches"

# drop the tag so the image under test is the one the tarball carries
docker rmi -f "$IMG" >/dev/null 2>&1 || true
loaded="$(docker load -i "$TAR")"
echo "$loaded" | grep -qx "Loaded image: $IMG" || fail "tarball loads as '$loaded', want $IMG"
ok "docker load -i → $IMG"

start "$VOL"
ok "HEALTHCHECK healthy (data volume mounted)"
[ "$(curl -fsS "$URL/healthz" | json "d['status']")" = ok ] || fail "/healthz not ok"
ok "/healthz ok"

got="$(curl -fsS -X POST "$URL/mcp" -H 'Content-Type: application/json' -H 'Accept: application/json' \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"verify-image","version":"0"}}}' \
  | json "d['result']['serverInfo']['version']")"
[ "$got" = "$VER" ] || fail "MCP serverInfo reports $got, want $VER"
ok "reports version $got"

uid="$(docker exec "$N" cat /proc/1/status | awk '/^Uid:/ {print $2}')"
[ "$uid" != 0 ] || fail "server runs as root"
ok "server runs as uid $uid"

oracle="$(api "$URL/api/db-profiles" | json "1 if d['drivers'].get('oracle') else 0")"
[ "$oracle" = "$WANT_ORACLE" ] || fail "oracle driver linked=$oracle, want $WANT_ORACLE"
ok "drivers: $(api "$URL/api/db-profiles" | json "' '.join(k for k, v in sorted(d['drivers'].items()) if v)")"

# a profile write lands in the volume: an unwritable volume fails here
api -X POST "$URL/api/db-profiles" -d '{"id":"verify-persist","name":"verify","type":"postgres","connect_string":"db.invalid:5432/app","username":"u","password_ref":"plain:p"}' >/dev/null \
  || fail "cannot write a profile to the data volume"
ok "profile written to the data volume"

if [ -n "$NET" ]; then
  api -X POST "$URL/api/db-profiles" -d "{\"id\":\"verify-oracle\",\"name\":\"verify oracle\",\"type\":\"oracle\",\"connect_string\":\"$ORA_CONNECT\",\"username\":\"$ORA_USER\",\"password_ref\":\"plain:$ORA_PW\"}" >/dev/null \
    || fail "cannot create the oracle profile"
  res="$(api -X POST "$URL/api/db-profiles/verify-oracle/test")"
  [ "$(echo "$res" | json "d['ok']")" = True ] || fail "oracle connection failed: $res"
  ok "real Oracle connection ($ORA_CONNECT, $(echo "$res" | json "d['elapsed_ms']") ms)"
  # a wrong password must read as an auth failure, not a TLS hint (v0.6.1)
  api -X POST "$URL/api/db-profiles" -d "{\"id\":\"verify-oracle-badpw\",\"name\":\"verify oracle badpw\",\"type\":\"oracle\",\"connect_string\":\"$ORA_CONNECT\",\"username\":\"$ORA_USER\",\"password_ref\":\"plain:not-the-password\"}" >/dev/null \
    || fail "cannot create the wrong-password oracle profile"
  got="$(api -X POST "$URL/api/db-profiles/verify-oracle-badpw/test" | json "str(d.get('category')) + ' ' + str(d.get('error_code'))")"
  [ "$got" = "authentication ORA-01017" ] || fail "wrong Oracle password diagnosed as '$got', want 'authentication ORA-01017'"
  ok "wrong Oracle password → $got"
fi

t0="$(date +%s)"
docker stop "$N" >/dev/null
el=$(( $(date +%s) - t0 )); code="$(docker inspect -f '{{.State.ExitCode}}' "$N")"
[ "$code" != 137 ] && [ "$el" -lt 10 ] || fail "docker stop took ${el}s, exit $code (killed, not a graceful SIGTERM)"
ok "docker stop graceful in ${el}s (exit $code)"

docker rm "$N" >/dev/null
start "$VOL"
api "$URL/api/db-profiles" | json "[p['id'] for p in d['profiles']]" | grep -q verify-persist \
  || fail "profile lost in a fresh container on the same volume"
ok "profile survives into a fresh container"

# the admin guide's way: a freshly made host directory bind-mounted as the
# data dir. Unlike a named volume it does not receive the image's files, and
# before v0.6.1 sqlon exited at boot with "load SQLON catalog".
docker rm -f "$N" >/dev/null
BIND="$(mktemp -d)"; chmod 755 "$BIND"
if [ "$(id -u)" != 10001 ]; then
  docker run -d --name "$N" -v "$BIND:/app/data/sqlon" -e SQLON_ADMIN_TOKEN="$TOK" "$IMG" >/dev/null
  docker wait "$N" >/dev/null
  docker logs "$N" 2>&1 | grep -q "chown -R" || fail "an unwritable data dir does not tell the operator to chown it"
  docker rm "$N" >/dev/null
  ok "unwritable host dir → exits with the chown instruction"
fi
chmod 777 "$BIND"
start "$BIND"
[ -e "$BIND/meta_physical_models.json" ] || fail "empty host dir was not seeded"
api -X POST "$URL/api/db-profiles" -d '{"id":"verify-bind","name":"verify","type":"postgres","connect_string":"db.invalid:5432/app","username":"u","password_ref":"plain:p"}' >/dev/null \
  || fail "cannot write a profile to the bind-mounted dir"
ok "empty bind-mounted host dir is seeded and writable"

echo "PASS $IMG"
