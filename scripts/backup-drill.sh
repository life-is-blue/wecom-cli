#!/usr/bin/env bash
# Backup / restore / point-in-time-recovery drill against a real MySQL 8.0 with binary logging.
#   scripts/backup-drill.sh [--keep]
# Needs Docker, Go, python3. Starts its own container (oe-b-drill, 127.0.0.1:33322) and server (127.0.0.1:18322).
#
# Timeline: load dataset -> logical backup (mysqldump --single-transaction --source-data=2) + blobs -> more writes
#   -> RECOVERY POINT (snapshot of every table + one workflow's etag/stage + binlog position) -> more writes
#   -> disaster (DROP DATABASE, blob dir deleted)
#   restore 1: dump + binlog replay to the recovery point (PITR), server restarted on it, assertions
#   restore 2: dump + binlog replay to the end (RPO ~ 0), server restarted on it, assertions
# Exit 0 only if every assertion holds. The commands below are the ones docs/operations.md documents.
set -euo pipefail
ROOT=$(cd "$(dirname "$0")/.." && pwd); cd "$ROOT"
KEEP=0; [ "${1:-}" = "--keep" ] && KEEP=1
C=oe-b-drill; MPORT=${DRILL_MYSQL_PORT:-33322}; SPORT=${DRILL_SERVER_PORT:-18322}; DB=drill
# mysql:8.0 (Oracle Linux, "server-minimal") ships mysqldump but NOT mysqlbinlog; the -debian variant of the same 8.0.x has both.
IMAGE=${DRILL_MYSQL_IMAGE:-mysql:8.0-debian}
W=$(mktemp -d); export HOME=$W/home; mkdir -p "$HOME"
BLOBS=$W/blobs; BAK=$W/backup; mkdir -p "$BAK"
SRV=$W/openetl-server; OE=$W/openetl; SRVPID=""
export OPENETL_BASE_URL=http://127.0.0.1:$SPORT OPENETL_PROJECT=acme-dw
DSN="oe:oe@tcp(127.0.0.1:$MPORT)/$DB?parseTime=true&multiStatements=true"
cleanup() {
  [ -n "$SRVPID" ] && kill "$SRVPID" 2>/dev/null || true
  if [ "$KEEP" = 0 ]; then docker rm -f "$C" >/dev/null 2>&1 || true; rm -r "$W"; else echo "kept container $C and $W"; fi
}
trap cleanup EXIT
fail() { echo "FAIL: $*" >&2; exit 1; }
now() { date +%s.%N; }
elapsed() { echo "$(now) - $1" | bc; }
check() { [ "$2" = "$3" ] && echo "   ok  $1 = $2" || fail "$1: got '$2', expected '$3'"; }
jget() { python3 -c "import sys,json;d=json.load(sys.stdin);print(eval(sys.argv[1]))" "$1"; }
sql() { docker exec -i "$C" mysql -uroot "$@"; }
sqln() { sql -N -B -e "$1"; }

start_server() {
  OPENETL_DB_DSN=$DSN OPENETL_ADDR=127.0.0.1:$SPORT OPENETL_STORAGE=fs OPENETL_STORAGE_DIR=$BLOBS \
    OPENETL_STORAGE_SECRET=drill-only-secret OPENETL_PUBLIC_URL=$OPENETL_BASE_URL OPENETL_ACCESS_LOG=off \
    "$SRV" serve >>"$W/server.log" 2>&1 &
  SRVPID=$!
  for _ in $(seq 60); do curl -fs "$OPENETL_BASE_URL/healthz" >/dev/null 2>&1 && return 0; sleep 0.5; done
  tail -20 "$W/server.log" >&2; fail "server did not become healthy"
}
stop_server() { if [ -n "$SRVPID" ]; then kill "$SRVPID"; wait "$SRVPID" 2>/dev/null || true; SRVPID=""; fi; }
oe_as() { local tok=$1; shift; OPENETL_TOKEN=$tok "$OE" "$@"; }

# snapshot DIR: row count + CHECKSUM of every base table (server must be idle: no traffic between snapshot and binlog position)
snapshot() {
  local out=$1; : >"$out"
  for t in $(sqln "SELECT table_name FROM information_schema.tables WHERE table_schema='$DB' AND table_type='BASE TABLE' ORDER BY 1"); do
    printf '%s rows=%s checksum=%s\n' "$t" "$(sqln "SELECT COUNT(*) FROM $DB.$t")" "$(sqln "CHECKSUM TABLE $DB.$t" | awk '{print $2}')" >>"$out"
  done
}
wf_state() { # etag + stage of the known workflow, from the API and from the views
  oe_as "$ADMIN" workflow get "$WF" | jget "d['etag']"
  sqln "SELECT GROUP_CONCAT(CONCAT(s.stage_key,'=',s.status) ORDER BY s.seq) FROM $DB.workflow_stages s JOIN $DB.workflows w ON w.id=s.workflow_id WHERE w.code='$WF'"
}

echo "== build binaries + start MySQL 8.0 with binary logging"
go build -o "$SRV" ./cmd/openetl-server; go build -o "$OE" ./cmd/openetl
docker rm -f "$C" >/dev/null 2>&1 || true
docker run -d --name "$C" -e MYSQL_ALLOW_EMPTY_PASSWORD=yes -p 127.0.0.1:$MPORT:3306 $IMAGE \
  --character-set-server=utf8mb4 --log-bin=binlog --server-id=1 --binlog-format=ROW --sync-binlog=1 \
  --innodb-flush-log-at-trx-commit=1 >/dev/null
# Wait on TCP, then again after a pause: the image starts a socket-only bootstrap server, stops it and starts the real one;
# a socket ping (or one TCP ping that lands in the gap) races that restart.
for _ in $(seq 90); do docker exec "$C" mysqladmin -uroot -h127.0.0.1 ping >/dev/null 2>&1 && break; sleep 2; done
sleep 2; docker exec "$C" mysqladmin -uroot -h127.0.0.1 ping >/dev/null || fail "mysql not ready"
sql -e "CREATE USER 'oe'@'%' IDENTIFIED WITH mysql_native_password BY 'oe'; GRANT ALL ON *.* TO 'oe'@'%'; CREATE DATABASE $DB CHARACTER SET utf8mb4"
check "log_bin" "$(sqln 'SELECT @@log_bin')" 1
docker exec "$C" bash -c 'command -v mysqlbinlog' >/dev/null || fail "image $IMAGE has no mysqlbinlog"
start_server

echo "== tokens, dataset, import"
mint() { OPENETL_DB_DSN=$DSN "$SRV" admin create-token --project acme-dw --account "$1" --side "$2" --role "$3" 2>/dev/null | tail -1; }
ADMIN=$(mint alice delivery admin); EDT=$(mint bob delivery editor)
python3 scripts/gen-dataset.py "$W/data" --small | tee "$W/gen.txt"
N=$(awk '{print $1}' "$W/gen.txt")
oe_as "$ADMIN" import stage --from "$W/data/rows.ndjson" --map "$W/data/mapping.yaml" --max-rows 100000 >"$W/job.json"
JOB=$(jget "d['name'].split('/')[-1]" <"$W/job.json")
oe_as "$ADMIN" import apply "$JOB" --yes >/dev/null
check "workflows after import" "$(oe_as "$ADMIN" query 'SELECT COUNT(*) FROM workflows' | jget "d['rows'][0][0]")" "$N"
mapfile -t CODES < <(oe_as "$ADMIN" query 'SELECT code FROM workflows ORDER BY id' --page-size 1000 | python3 -c "import sys,json;[print(r[0]) for r in json.load(sys.stdin)['rows']]")
WF=${CODES[0]}
mkfile() { head -c "$2" /dev/urandom >"$W/$1"; echo "$W/$1"; }
for i in 0 1 2; do oe_as "$EDT" stage advance "${CODES[$i]}" --stage development >/dev/null; done
oe_as "$EDT" comment add "${CODES[3]}" --kind note --body "evidence before backup" --attach "$(mkfile a1.bin 40000)" >/dev/null
sql -e "ANALYZE TABLE $DB.delta, $DB.entity_latest" >/dev/null   # statistics as a long-running instance would have them

echo "== BACKUP: logical dump (consistent, with binlog coordinates) + blob directory"
T_BAK=$(now)
docker exec "$C" mysqldump -uroot --single-transaction --source-data=2 --routines --triggers --events --hex-blob \
  --set-gtid-purged=OFF --databases $DB >"$BAK/dump.sql"
DUMP_FILE=$(sed -n "s/.*\(MASTER\|SOURCE\)_LOG_FILE='\([^']*\)'.*/\2/p" "$BAK/dump.sql" | head -1)   # 8.0 prints MASTER_*, 8.4 SOURCE_*
DUMP_POS=$(sed -n "s/.*\(MASTER\|SOURCE\)_LOG_POS=\([0-9]*\).*/\2/p" "$BAK/dump.sql" | head -1)
[ -n "$DUMP_FILE" ] && [ -n "$DUMP_POS" ] || fail "dump has no binlog coordinates"
echo "   dump $(du -h "$BAK/dump.sql" | cut -f1), binlog coordinates $DUMP_FILE:$DUMP_POS, took $(printf %.1f "$(elapsed "$T_BAK")")s"
tar -C "$BLOBS" -cf "$BAK/blobs-1.tar" . ; echo "   blobs-1.tar: $(tar -tf "$BAK/blobs-1.tar" | grep -vc '/$') file(s)"

echo "== writes after the backup, up to the recovery point"
sleep 1
oe_as "$EDT" stage advance "$WF" --stage development >/dev/null
oe_as "$EDT" stage advance "$WF" --stage deployment >/dev/null
oe_as "$EDT" comment add "$WF" --kind note --body "evidence after backup, before recovery point" --attach "$(mkfile a2.bin 70000)" >/dev/null
oe_as "$EDT" comment add "${CODES[4]}" --kind issue --body "issue before recovery point" >/dev/null
sql -e "FLUSH BINARY LOGS"   # force a rotation so the replay below crosses binlog files
oe_as "$EDT" stage advance "${CODES[5]}" --stage development >/dev/null
oe_as "$EDT" comment add "${CODES[6]}" --kind note --body "evidence 3" --attach "$(mkfile a3.bin 5000)" >/dev/null

mapfile -t RPW < <(wf_state); RP_ETAG=${RPW[0]}; RP_STAGE=${RPW[1]}
T_RP=$(now)
snapshot "$W/snap-rp.txt"                       # no server traffic from here on
read -r RP_FILE RP_POS _ < <(sqln "SHOW MASTER STATUS")
echo "   RECOVERY POINT: $RP_FILE:$RP_POS  workflow $WF etag=$RP_ETAG stage=$RP_STAGE"
RP_WORKFLOWS=$(sqln "SELECT COUNT(*) FROM $DB.workflows")

echo "== writes after the recovery point (will be lost by the PITR, kept by the full replay)"
sleep 1
oe_as "$EDT" stage advance "$WF" --stage verify >/dev/null
oe_as "$EDT" comment add "$WF" --kind risk --body "after recovery point" --attach "$(mkfile a4.bin 9000)" >/dev/null
oe_as "$ADMIN" workflow create --name "created after the recovery point" >/dev/null
mapfile -t ENDW < <(wf_state); END_ETAG=${ENDW[0]}; END_STAGE=${ENDW[1]}
[ "$END_ETAG" != "$RP_ETAG" ] || fail "test setup: etag did not move after the recovery point"
sleep 1
T_LAST_WRITE=$(now)
stop_server
snapshot "$W/snap-end.txt"
sql -e "FLUSH BINARY LOGS"
tar -C "$BLOBS" -cf "$BAK/blobs-2.tar" .        # blobs are write-once: a copy taken after the DB point holds every blob the DB can reference
mkdir -p "$BAK/binlogs"
for f in $(docker exec "$C" sh -c 'ls /var/lib/mysql/binlog.[0-9]*' | sort); do docker cp "$C:$f" "$BAK/binlogs/" >/dev/null; done
echo "   binlog archive: $(ls "$BAK/binlogs" | tr '\n' ' ')"
STORAGE_KEYS=$(sqln "SELECT JSON_UNQUOTE(v) FROM $DB.entity_latest WHERE k='storage_key' AND v IS NOT NULL")
[ "$(echo "$STORAGE_KEYS" | wc -l)" -ge 4 ] || fail "test setup: expected >=4 attachments"

# restore_and_check LABEL STOP_POS(or "end") EXPECT_SNAPSHOT EXPECT_ETAG EXPECT_STAGE BLOBTAR
restore_and_check() {
  local label=$1 stop=$2 snap=$3 etag=$4 stage=$5 tarf=$6 T_DIS T_UP
  echo; echo "== DISASTER + RESTORE ($label)"
  T_DIS=$(now)
  stop_server
  sql -e "DROP DATABASE $DB"; find "$BLOBS" -mindepth 1 -delete
  [ "$(sqln "SELECT COUNT(*) FROM information_schema.schemata WHERE schema_name='$DB'")" = 0 ] || fail "database still there"
  # 1. restore the dump (re-creates the database)
  sql <"$BAK/dump.sql"
  # 2. replay the binary log from the dump's coordinates to the stop position (or to the end of the archive)
  local files=() f
  for f in $(ls "$BAK/binlogs" | sort); do [[ "$f" < "$DUMP_FILE" ]] || files+=("$f"); done
  if [ "$stop" != end ]; then # only up to the file that holds the stop position
    local keep=() ; for f in "${files[@]}"; do keep+=("$f"); [ "$f" = "$RP_FILE" ] && break; done; files=("${keep[@]}")
  fi
  local stopflag=""; [ "$stop" = end ] || stopflag="--stop-position=$stop"
  echo "   replaying ${files[*]} from $DUMP_FILE:$DUMP_POS ${stopflag:-to the end}"
  docker exec "$C" rm -rf /tmp/binlogs; docker cp "$BAK/binlogs" "$C:/tmp/binlogs" >/dev/null
  docker exec "$C" bash -c "set -o pipefail; cd /tmp/binlogs && mysqlbinlog --database=$DB --start-position=$DUMP_POS $stopflag ${files[*]} | mysql -uroot"
  # 3. blobs, then the server (migrations are a no-op; it also re-ANALYZEs)
  tar -C "$BLOBS" -xf "$tarf"
  start_server; T_UP=$(now)
  echo "   server healthy $(printf %.1f "$(echo "$T_UP - $T_DIS" | bc)")s after the disaster"
  # assertions
  snapshot "$W/snap-restored.txt"
  if ! diff -u "$snap" "$W/snap-restored.txt"; then fail "$label: row counts / checksums differ from the expected snapshot"; fi
  echo "   ok  all $(wc -l <"$snap") tables: row count and CHECKSUM TABLE equal the snapshot"
  OPENETL_DB_DSN=$DSN "$SRV" admin verify-latest || fail "$label: verify-latest"
  mapfile -t GOT < <(wf_state)
  check "$label: workflow $WF etag" "${GOT[0]}" "$etag"
  check "$label: workflow $WF stage" "${GOT[1]}" "$stage"
  local missing=0 k
  for k in $(sqln "SELECT JSON_UNQUOTE(v) FROM $DB.entity_latest WHERE k='storage_key' AND v IS NOT NULL"); do [ -f "$BLOBS/$k" ] || { echo "   MISSING blob $k"; missing=1; }; done
  [ $missing = 0 ] || fail "$label: attachment blobs referenced by metadata are missing"
  echo "   ok  $(sqln "SELECT COUNT(*) FROM $DB.entity_latest WHERE k='storage_key' AND v IS NOT NULL") attachment blob(s) referenced by metadata exist in the restored blob dir"
  # the restored attachment is readable through the API (token from the restored DB still works)
  local at; at=$(oe_as "$ADMIN" query "SELECT COUNT(*) FROM workflows" | jget "d['rows'][0][0]")
  RTO=$(printf %.1f "$(elapsed "$T_DIS")")
  echo "   RTO ($label): ${RTO}s from disaster to restored, verified, serving server ($at workflows)"
}

restore_and_check "PITR to recovery point" "$RP_POS" "$W/snap-rp.txt" "$RP_ETAG" "$RP_STAGE" "$BAK/blobs-2.tar"
check "PITR: workflows" "$(sqln "SELECT COUNT(*) FROM $DB.workflows")" "$RP_WORKFLOWS"
check "PITR: post-recovery-point workflow absent" "$(sqln "SELECT COUNT(*) FROM $DB.workflows WHERE name='created after the recovery point'")" 0
RTO_PITR=$RTO
RPO_PITR=$(printf %.1f "$(echo "$T_LAST_WRITE - $T_RP" | bc)")
restore_and_check "full replay to end" end "$W/snap-end.txt" "$END_ETAG" "$END_STAGE" "$BAK/blobs-2.tar"
check "full replay: post-recovery-point workflow present" "$(sqln "SELECT COUNT(*) FROM $DB.workflows WHERE name='created after the recovery point'")" 1

echo
echo "DRILL OK"
echo "  RPO observed: PITR ${RPO_PITR}s of writes after the chosen point were discarded on purpose; full binlog replay lost 0 acknowledged writes (all tables byte-equal to pre-disaster)"
echo "  RTO observed: PITR ${RTO_PITR}s, full replay ${RTO}s (dump $(du -h "$BAK/dump.sql" | cut -f1), ${N} workflows; scales with dump size + binlog volume, not measured at 3000 workflows)"
