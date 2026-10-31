#!/usr/bin/env bash
# Contract gate for api/openapi.yaml.
#   scripts/check-contract.sh            static checks + live conformance + live attack drill (needs docker mysql, go, python3)
#   scripts/check-contract.sh --static   static checks only (no database, no server)
# Static: openapi-spec-validator, oasdiff breaking (baseline vs current), vacuum lint (api/vacuum-ruleset.yaml),
#         generated types up to date.  Live: starts its own server on :18203 over database openetl_c (DROPPED and
#         recreated each run), mints tokens, replays every operationId (scripts/contract_conformance.py) and the
#         security regression attacks (scripts/contract_attacks.py).
# Accepting an intentional contract change: edit api/openapi.yaml, regenerate (cd internal/api && go generate ./...),
#   then `cp api/openapi.yaml api/openapi.baseline.yaml` in the same commit, after reading the oasdiff output.
# Env: OE_MYSQL_CONTAINER (default openetl-mysql8), OE_PORT (18203), OE_DB (openetl_c), OE_DSN_HOST (127.0.0.1:33307).
set -u
ROOT=$(cd "$(dirname "$0")/.." && pwd)
SPEC=$ROOT/api/openapi.yaml
BASELINE=$ROOT/api/openapi.baseline.yaml
OASDIFF_VER=v1.35.0; VACUUM_VER=v0.36.1; OAPI_VER=v2.8.1
PORT=${OE_PORT:-18203}; DB=${OE_DB:-openetl_c}; CT=${OE_MYSQL_CONTAINER:-openetl-mysql8}; HOSTPORT=${OE_DSN_HOST:-127.0.0.1:33307}
fail=0
step() { printf '\n== %s\n' "$*"; }
bad() { echo "  FAIL: $*"; fail=1; }
tool() { # tool <binary> <module@version> args...: use the installed binary, else go run the pinned module
  local bin=$1 mod=$2; shift 2
  if command -v "$bin" >/dev/null 2>&1; then "$bin" "$@"; else (cd "$ROOT" && go run "$mod" "$@"); fi
}

step "1. openapi-spec-validator (OpenAPI 3.0 schema validity)"
if command -v openapi-spec-validator >/dev/null; then openapi-spec-validator "$SPEC" || bad "invalid spec"; else bad "openapi-spec-validator missing (pip install openapi-spec-validator)"; fi

step "2. oasdiff breaking: api/openapi.baseline.yaml -> api/openapi.yaml"
[ -f "$BASELINE" ] || bad "baseline missing"
tool oasdiff github.com/oasdiff/oasdiff@$OASDIFF_VER breaking "$BASELINE" "$SPEC" --fail-on ERR || bad "breaking contract change (see above)"

step "3. vacuum lint (ruleset api/vacuum-ruleset.yaml, fail on warnings)"
tool vacuum github.com/daveshanley/vacuum@$VACUUM_VER lint -d -q -b -n warn -r "$ROOT/api/vacuum-ruleset.yaml" "$SPEC" >"${TMPDIR:-/tmp}/vacuum.$$" 2>&1 || { tail -n 30 "${TMPDIR:-/tmp}/vacuum.$$"; bad "lint findings"; }
rm -f "${TMPDIR:-/tmp}/vacuum.$$"

step "4. generated types are current"
TMP=$(mktemp -d)
if (cd "$ROOT/internal/api" && go run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@$OAPI_VER -generate types -package api -o "$TMP/types.gen.go" ../../api/openapi.yaml) 2>/dev/null; then
  diff -q "$TMP/types.gen.go" "$ROOT/internal/api/types.gen.go" >/dev/null && echo "  up to date" || bad "internal/api/types.gen.go is stale: cd internal/api && go generate ./..."
else bad "oapi-codegen failed"; fi
rm -rf "$TMP"

if [ "${1:-}" = "--static" ]; then
  [ $fail = 0 ] && echo "static checks OK" || echo "static checks FAILED"; exit $fail
fi

step "5. live: build, fresh database $DB, start server on :$PORT"
W=$(mktemp -d); trap 'kill "$SRV" 2>/dev/null; wait "$SRV" 2>/dev/null; rm -rf "$W"' EXIT
(cd "$ROOT" && go build -o "$W/openetl-server" ./cmd/openetl-server && go build -o "$W/openetl" ./cmd/openetl) || { echo "build failed"; exit 1; }
docker exec -i "$CT" mysql -uroot -e "DROP DATABASE IF EXISTS $DB; CREATE DATABASE $DB CHARACTER SET utf8mb4" || { echo "cannot reach mysql container $CT"; exit 1; }
export OPENETL_DB_DSN="oe:oe@tcp($HOSTPORT)/$DB?parseTime=true&multiStatements=true"
"$W/openetl-server" migrate >/dev/null || exit 1
mint() { "$W/openetl-server" admin create-token --project "$1" --account "$2" --side "$3" --role "$4" 2>/dev/null | tail -1; }
# second project (pb) is created only after the conformance pass: config updates are refused once two projects exist
python3 - "$W" "$(mint pa alice delivery admin)" "$(mint pa bob delivery editor)" "$(mint pa carol customer editor)" "$(mint pa vera delivery viewer)" <<'PY'
import json, sys
w, *t = sys.argv[1:]
json.dump(dict(zip(["alice", "bob", "carol", "vera"], t)), open(w + "/tokens.json", "w"))
PY
OPENETL_ADDR=127.0.0.1:$PORT OPENETL_STORAGE=fs OPENETL_STORAGE_DIR="$W/blobs" OPENETL_PUBLIC_URL=http://127.0.0.1:$PORT OPENETL_ACCESS_LOG=off \
  "$W/openetl-server" serve >"$W/server.log" 2>&1 & SRV=$!
for _ in $(seq 100); do curl -sf "http://127.0.0.1:$PORT/healthz" >/dev/null && break; sleep 0.1; done
curl -sf "http://127.0.0.1:$PORT/healthz" >/dev/null || { echo "server did not start"; cat "$W/server.log"; exit 1; }

step "6. live: conformance of every operationId against the spec"
python3 -I "$ROOT/scripts/contract_conformance.py" "http://127.0.0.1:$PORT" "$SPEC" "$W/tokens.json" || bad "server and spec diverge"

step "7. live: security regression attacks"
python3 - "$W" "$(mint pb dave delivery admin)" <<'PY'
import json, sys
w, dave = sys.argv[1:]
t = json.load(open(w + "/tokens.json"))
json.dump({"alice": t["alice"], "bob": t["bob"], "dave": dave}, open(w + "/tokens-attack.json", "w"))
PY
python3 -I "$ROOT/scripts/contract_attacks.py" "http://127.0.0.1:$PORT" "$W/tokens-attack.json" "$W/openetl" || bad "an attack got through"

[ $fail = 0 ] && echo "contract checks OK" || echo "contract checks FAILED"
exit $fail
