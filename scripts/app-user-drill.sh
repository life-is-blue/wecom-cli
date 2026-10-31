#!/usr/bin/env bash
# Proves scripts/create-app-user.sql: the server and the admin CLI run a full flow as the least-privilege user
# (no UPDATE/DELETE on event/delta), and the database really refuses to rewrite history.
#   scripts/app-user-drill.sh [--keep]      own container oe-b-app (127.0.0.1:33323), server on 127.0.0.1:18323
set -euo pipefail
ROOT=$(cd "$(dirname "$0")/.." && pwd); cd "$ROOT"
KEEP=0; [ "${1:-}" = "--keep" ] && KEEP=1
C=oe-b-app; MPORT=${APP_MYSQL_PORT:-33323}; SPORT=${APP_SERVER_PORT:-18323}; DB=apptest
W=$(mktemp -d); export HOME=$W/home; mkdir -p "$HOME"; SRV=$W/openetl-server; OE=$W/openetl; SRVPID=""
export OPENETL_BASE_URL=http://127.0.0.1:$SPORT OPENETL_PROJECT=acme-dw
OWNER="oe:oe@tcp(127.0.0.1:$MPORT)/$DB?parseTime=true&multiStatements=true"
APPDSN="openetl_app:app-pw@tcp(127.0.0.1:$MPORT)/$DB?parseTime=true&multiStatements=true"
cleanup() { [ -n "$SRVPID" ] && kill "$SRVPID" 2>/dev/null || true
  [ -n "${SHOWLOG:-}" ] && tail -5 "$W/server.log" >&2 || true
  if [ "$KEEP" = 0 ]; then docker rm -f "$C" >/dev/null 2>&1 || true; rm -r "$W"; else echo "kept $C and $W"; fi; }
trap cleanup EXIT
fail() { echo "FAIL: $*" >&2; exit 1; }
check() { [ "$2" = "$3" ] && echo "   ok  $1 = $2" || fail "$1: got '$2', expected '$3'"; }
jget() { python3 -c "import sys,json;d=json.load(sys.stdin);print(eval(sys.argv[1]))" "$1"; }
sql() { docker exec -i "$C" mysql -uroot "$@"; }
oe_as() { local tok=$1; shift; OPENETL_TOKEN=$tok "$OE" "$@"; }

echo "== build, MySQL 8.0, migrate as OWNER, create restricted users from the shipped scripts"
go build -o "$SRV" ./cmd/openetl-server; go build -o "$OE" ./cmd/openetl
docker rm -f "$C" >/dev/null 2>&1 || true
docker run -d --name "$C" -e MYSQL_ALLOW_EMPTY_PASSWORD=yes -p 127.0.0.1:$MPORT:3306 mysql:8.0 --character-set-server=utf8mb4 >/dev/null
for _ in $(seq 90); do docker exec "$C" mysqladmin -uroot -h127.0.0.1 ping >/dev/null 2>&1 && break; sleep 2; done
sleep 2; docker exec "$C" mysqladmin -uroot -h127.0.0.1 ping >/dev/null || fail "mysql not ready"
sql -e "CREATE USER 'oe'@'%' IDENTIFIED WITH mysql_native_password BY 'oe'; GRANT ALL ON *.* TO 'oe'@'%'; CREATE DATABASE $DB CHARACTER SET utf8mb4"
OPENETL_DB_DSN=$OWNER "$SRV" migrate
sed -e "s/\`openetl\`/\`$DB\`/g" -e "s/CHANGE-ME-app-password/app-pw/" scripts/create-app-user.sql | sql
sed -e "s/\`openetl\`/\`$DB\`/g" -e "s/CHANGE-ME-long-random-password/query-pw/" scripts/create-query-user.sql | sql
sql -e "SHOW GRANTS FOR 'openetl_app'@'%'" | grep -E "event|delta" || true

echo "== the database refuses to rewrite history for the app user"
app_sql() { docker exec -i "$C" mysql -uopenetl_app -papp-pw -h127.0.0.1 "$DB" -e "$1" 2>&1; }
for stmt in "UPDATE event SET note='x'" "DELETE FROM event" "UPDATE delta SET new_v='1'" "DELETE FROM delta" "TRUNCATE delta" "DROP TABLE event" "ALTER TABLE delta ADD COLUMN zz INT" "UPDATE entity SET type='task'" "UPDATE entity SET project_id=1" "DELETE FROM entity"; do
  out=$(app_sql "$stmt" || true); echo "$out" | grep -q "ERROR 1142\|ERROR 1044\|ERROR 1143" || fail "'$stmt' was not refused: $out"
  echo "   refused: $stmt"
done

echo "== full flow as the restricted user"
OPENETL_DB_DSN=$APPDSN OPENETL_ADDR=127.0.0.1:$SPORT OPENETL_QUERY_DSN="openetl_query:query-pw@tcp(127.0.0.1:$MPORT)/$DB" \
  OPENETL_STORAGE=fs OPENETL_STORAGE_DIR=$W/blobs OPENETL_STORAGE_SECRET=x OPENETL_PUBLIC_URL=$OPENETL_BASE_URL OPENETL_ACCESS_LOG=off \
  "$SRV" serve >"$W/server.log" 2>&1 & SRVPID=$!
for _ in $(seq 60); do curl -fs "$OPENETL_BASE_URL/healthz" >/dev/null 2>&1 && break; sleep 0.5; done
curl -fs "$OPENETL_BASE_URL/healthz" >/dev/null || { cat "$W/server.log"; fail "server (restricted user) did not start"; }
mint() { OPENETL_DB_DSN=$APPDSN "$SRV" admin create-token --project acme-dw --account "$1" --side "$2" --role "$3" 2>/dev/null | tail -1; }
ADMIN=$(mint alice delivery admin); EDT=$(mint bob delivery editor); CUS=$(mint carol customer editor)
python3 scripts/gen-dataset.py "$W/data" --small >"$W/gen.txt"; N=$(awk '{print $1}' "$W/gen.txt")
oe_as "$ADMIN" import stage --from "$W/data/rows.ndjson" --map "$W/data/mapping.yaml" >"$W/job.json"
JOB=$(jget "d['name'].split('/')[-1]" <"$W/job.json")
oe_as "$ADMIN" import diff "$JOB" --limit 1 >/dev/null
oe_as "$ADMIN" import apply "$JOB" --yes >/dev/null
check "workflows" "$(oe_as "$ADMIN" query 'SELECT COUNT(*) FROM workflows' | jget "d['rows'][0][0]")" "$N"
mapfile -t CODES < <(oe_as "$ADMIN" query 'SELECT code FROM workflows ORDER BY id LIMIT 8' | python3 -c "import sys,json;[print(r[0]) for r in json.load(sys.stdin)['rows']]")
for s in development deployment verify; do oe_as "$EDT" stage advance "${CODES[0]}" --stage $s >/dev/null; done
oe_as "$CUS" stage advance "${CODES[0]}" --stage accept >/dev/null
oe_as "$CUS" stage regress "${CODES[0]}" --stage accept --reason rej_row_count >/dev/null
head -c 20000 /dev/urandom >"$W/ev.bin"
oe_as "$EDT" comment add "${CODES[1]}" --kind issue --body "blocked" --attach "$W/ev.bin" >/dev/null
CM=$(oe_as "$EDT" comment add "${CODES[2]}" --kind issue --body "row count off" | jget "d['name'].split('/')[-1]")
oe_as "$EDT" comment resolve "${CODES[2]}" "$CM" >/dev/null
oe_as "$ADMIN" scope set "${CODES[3]}" --to out --reason obsolete >/dev/null
oe_as "$EDT" workflow create --name "created by app user" >/dev/null
oe_as "$EDT" workflow update "${CODES[4]}" --label area=ods >/dev/null
oe_as "$ADMIN" workflow delete "${CODES[5]}" >/dev/null; oe_as "$ADMIN" workflow restore "${CODES[5]}" >/dev/null
echo "   imports, lifecycle, comments + attachment, scope, create/update/delete/restore: ok"
# administration: config (stages/reasons/rules/settings upsert + delete), members, tokens
oe_as "$ADMIN" config get >"$W/config.json"
for sec in stages reasons rules settings; do oe_as "$ADMIN" config get --section $sec >"$W/$sec.json"; done
oe_as "$ADMIN" config set --stages "$W/stages.json" --reasons "$W/reasons.json" --rules "$W/rules.json" >/dev/null
oe_as "$ADMIN" config set --setting demo.key=1 >/dev/null
oe_as "$ADMIN" config set --setting demo.key=2 --setting demo.other=x >/dev/null
echo '{}' | oe_as "$ADMIN" config set --settings - >/dev/null || true   # replace the whole section: deletes keys
oe_as "$ADMIN" member add dave --role viewer --side customer >/dev/null
oe_as "$ADMIN" member add dave --role editor --side customer >/dev/null
oe_as "$ADMIN" member remove dave >/dev/null
TID=$(oe_as "$ADMIN" token create --account bob --ttl 1h | jget "d['name'].split('/')[-1]")
oe_as "$ADMIN" token revoke "$TID" --account bob >/dev/null
echo "   config set (stages/reasons/rules/settings), member add/re-role/remove, token create/revoke: ok"
# import rollback: writes correcting events, must not need UPDATE/DELETE on event/delta
head -3 "$W/data/rows.ndjson" | sed -e 's/src-000/src-9990/; s/"pipeline_name":"/"pipeline_name":"rb_/' >"$W/rb.ndjson"
oe_as "$ADMIN" import stage --from "$W/rb.ndjson" --map "$W/data/mapping.yaml" >"$W/job2.json"
JOB2=$(jget "d['name'].split('/')[-1]" <"$W/job2.json")
oe_as "$ADMIN" import apply "$JOB2" --yes >/dev/null
BEFORE=$(oe_as "$ADMIN" query 'SELECT COUNT(*) FROM workflows' | jget "d['rows'][0][0]")
oe_as "$ADMIN" import rollback "$JOB2" --yes >/dev/null
echo "   import rollback ($BEFORE workflows before): ok"
# derived cache maintenance
OPENETL_DB_DSN=$APPDSN "$SRV" admin rebuild-latest
OPENETL_DB_DSN=$APPDSN "$SRV" admin verify-latest
check "import job state" "$(oe_as "$ADMIN" import show "$JOB2" | jget "d.get('state')")" "ROLLED_BACK"
G=$(sql -N -e "SHOW GRANTS FOR 'openetl_app'@'%'")
echo "$G" | grep -E '`(event|delta)`' | grep -qiE "UPDATE|DELETE|ALL PRIVILEGES" && fail "app user can change event/delta" || echo "   ok  grants on event/delta are INSERT only"
grep -i "denied\|error" "$W/server.log" && fail "server log shows errors (above)" || true
echo; echo "APP USER DRILL OK"
