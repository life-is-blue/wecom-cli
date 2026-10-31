#!/usr/bin/env bash
# Reconciliation runs on scratch databases.  Needs go, python3, a MySQL 8 reachable with a user that may create databases.
#   scripts/audit/run.sh lifecycle     fresh e2e-style dataset (gen-dataset --small) + scripted lifecycle through the CLI -> DB openetl_d
#   scripts/audit/run.sh adversarial   scripts/audit/gen-adversarial.py dataset loaded as raw rows                      -> DB openetl_d_adv
#   scripts/audit/run.sh selftest      sensitivity test: corrupt the adversarial database in 5 ways, each must be caught by reconcile.py
#   scripts/audit/run.sh all           lifecycle + adversarial + selftest (default)
# Env: AUDIT_PORT (18204)  AUDIT_MYSQL ("mysql -h127.0.0.1 -P33307 -uoe -poe")  AUDIT_DSN_BASE ("oe:oe@tcp(127.0.0.1:33307)")
#      AUDIT_SEED (adversarial generator seed, default 1)   AUDIT_OUT (output dir, default: a temp dir, kept)
# Databases are dropped and recreated; the server started here is stopped again.  Exit 0 only if every reconcile matched.
set -uo pipefail
ROOT=$(cd "$(dirname "$0")/../.." && pwd); cd "$ROOT"
MODE=${1:-all}
PORT=${AUDIT_PORT:-18204}
MYSQL=${AUDIT_MYSQL:-mysql -h127.0.0.1 -P33307 -uoe -poe}
DSN_BASE=${AUDIT_DSN_BASE:-oe:oe@tcp(127.0.0.1:33307)}
OUT=${AUDIT_OUT:-$(mktemp -d)}; mkdir -p "$OUT"
echo "output in $OUT"
go build -o "$OUT/openetl" ./cmd/openetl && go build -o "$OUT/openetl-server" ./cmd/openetl-server || exit 1
rc=0
SRV=""
stop_server() { if [ -n "$SRV" ]; then kill "$SRV" 2>/dev/null; wait "$SRV" 2>/dev/null; SRV=""; fi; }
trap stop_server EXIT

start_server() { # $1 database
  export OPENETL_DB_DSN="$DSN_BASE/$1?parseTime=true&multiStatements=true"
  OPENETL_ADDR=127.0.0.1:$PORT OPENETL_STORAGE=fs OPENETL_STORAGE_DIR="$OUT/blobs-$1" OPENETL_STORAGE_SECRET=audit-only \
    OPENETL_PUBLIC_URL=http://127.0.0.1:$PORT "$OUT/openetl-server" serve >"$OUT/server-$1.log" 2>&1 &
  SRV=$!
  for _ in $(seq 100); do curl -sf "http://127.0.0.1:$PORT/healthz" >/dev/null && return 0; sleep 0.2; done
  echo "server did not start"; cat "$OUT/server-$1.log"; return 1
}
tokens() { # $1 db $2 project
  mkdir -p "$OUT/tok-$1"
  for u in "alice delivery admin" "bob delivery editor" "carol customer editor"; do
    set -- "$1" "$2" $u
    OPENETL_DB_DSN="$DSN_BASE/$1?parseTime=true&multiStatements=true" "$OUT/openetl-server" admin create-token --project "$2" --account "$3" --side "$4" --role "$5" 2>/dev/null | tail -1 >"$OUT/tok-$1/tok_$3"
  done
}
fresh_db() { $MYSQL -e "DROP DATABASE IF EXISTS \`$1\`; CREATE DATABASE \`$1\` CHARACTER SET utf8mb4;" && OPENETL_DB_DSN="$DSN_BASE/$1?parseTime=true&multiStatements=true" "$OUT/openetl-server" migrate >/dev/null; }

if [ "$MODE" = lifecycle ] || [ "$MODE" = all ]; then
  echo "=== (a) lifecycle dataset on openetl_d"
  fresh_db openetl_d || exit 1
  tokens openetl_d acme-dw
  start_server openetl_d || exit 1
  python3 scripts/audit/lifecycle.py --cli "$OUT/openetl" --base-url "http://127.0.0.1:$PORT" --project acme-dw --tokens "$OUT/tok-openetl_d" || rc=1
  OPENETL_DB_DSN="$DSN_BASE/openetl_d?parseTime=true&multiStatements=true" "$OUT/openetl-server" admin verify-latest || rc=1
  python3 scripts/reconcile.py --db openetl_d --project acme-dw --base-url "http://127.0.0.1:$PORT" --token-file "$OUT/tok-openetl_d/tok_alice" \
    --mysql "$MYSQL" --json "$OUT/reconcile-lifecycle.json" --label lifecycle || rc=1
  stop_server
fi
if [ "$MODE" = adversarial ] || [ "$MODE" = all ]; then
  echo "=== (b) adversarial dataset on openetl_d_adv"
  fresh_db openetl_d_adv || exit 1
  python3 scripts/audit/gen-adversarial.py --seed "${AUDIT_SEED:-1}" --out "$OUT/adversarial.sql" --stats "$OUT/adversarial-stats.json" || exit 1
  $MYSQL --default-character-set=utf8mb4 openetl_d_adv <"$OUT/adversarial.sql" || exit 1
  OPENETL_DB_DSN="$DSN_BASE/openetl_d_adv?parseTime=true&multiStatements=true" "$OUT/openetl-server" admin rebuild-latest || exit 1
  tokens openetl_d_adv adv
  start_server openetl_d_adv || exit 1
  python3 scripts/reconcile.py --db openetl_d_adv --project adv --base-url "http://127.0.0.1:$PORT" --token-file "$OUT/tok-openetl_d_adv/tok_alice" \
    --mysql "$MYSQL" --json "$OUT/reconcile-adversarial.json" --label adversarial || rc=1
  stop_server
fi
if [ "$MODE" = selftest ] || [ "$MODE" = all ]; then
  echo "=== (c) reconcile self-test: corrupt openetl_d_adv, every corruption must be detected"
  [ "$MODE" = selftest ] && { fresh_db openetl_d_adv || exit 1
    python3 scripts/audit/gen-adversarial.py --seed "${AUDIT_SEED:-1}" --out "$OUT/adversarial.sql" >/dev/null || exit 1
    $MYSQL --default-character-set=utf8mb4 openetl_d_adv <"$OUT/adversarial.sql" || exit 1
    OPENETL_DB_DSN="$DSN_BASE/openetl_d_adv?parseTime=true&multiStatements=true" "$OUT/openetl-server" admin rebuild-latest >/dev/null || exit 1
    tokens openetl_d_adv adv; }
  start_server openetl_d_adv || exit 1
  restore() { OPENETL_DB_DSN="$DSN_BASE/openetl_d_adv?parseTime=true&multiStatements=true" "$OUT/openetl-server" admin rebuild-latest >/dev/null; }
  while IFS='|' read -r label want sqlstmt; do
    $MYSQL openetl_d_adv -e "$sqlstmt" || { echo "mutation failed: $label"; rc=1; continue; }
    res=$(python3 scripts/reconcile.py --db openetl_d_adv --project adv --base-url "http://127.0.0.1:$PORT" --token-file "$OUT/tok-openetl_d_adv/tok_alice" --mysql "$MYSQL" 2>&1)
    if echo "$res" | grep -q "MISMATCH.*$want"; then echo "  detected: $label"; else echo "  NOT DETECTED: $label"; echo "$res" | tail -5; rc=1; fi
    restore
  done <<'MUT'
a workflow name rewritten in the cache|workflows|UPDATE entity_latest SET v='"mutated"' WHERE k='name' AND entity_id IN (SELECT id FROM entity WHERE type='workflow') ORDER BY entity_id LIMIT 1
a soft-delete marker lost from the cache|deleted|DELETE FROM entity_latest WHERE k='deleted' AND v=CAST('true' AS JSON) ORDER BY entity_id LIMIT 1
a passed stage turned open in the cache|workflow_stages|UPDATE entity_latest SET v='"open"' WHERE k='stage.development' AND v=CAST('"pass"' AS JSON) ORDER BY entity_id LIMIT 1
a comment un-resolved in the cache|open_items|UPDATE entity_latest SET v=CAST('false' AS JSON) WHERE k='resolved' AND v=CAST('true' AS JSON) ORDER BY entity_id LIMIT 1
a task moved to another workflow in the cache|tasks|UPDATE entity_latest SET v=CAST('999999' AS JSON) WHERE k='workflow' ORDER BY entity_id LIMIT 1
MUT
  stop_server
fi
exit $rc
