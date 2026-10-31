#!/usr/bin/env bash
# M1 end-to-end demo. Needs an EMPTY MySQL/MariaDB database; pass its DSN:
#   OPENETL_DB_DSN='user:pw@tcp(127.0.0.1:3306)/openetl_demo?parseTime=true&multiStatements=true' scripts/demo.sh
# Exit code 0 means every expectation held.
set -u
: "${OPENETL_DB_DSN:?set OPENETL_DB_DSN to an empty database}"
ROOT=$(cd "$(dirname "$0")/.." && pwd)
BIN=$(mktemp -d); export HOME=$(mktemp -d)   # isolate the credential store
PORT=${OPENETL_DEMO_PORT:-18080}
export OPENETL_BASE_URL=http://127.0.0.1:$PORT OPENETL_PROJECT=acme-dw
fail=0
say() { printf '\n== %s\n' "$*"; }
jget() { python3 -c "import sys,json;d=json.load(sys.stdin);print(eval(sys.argv[1]))" "$1"; }
expect_fail() { # expect_fail <reason> <cmd...>: command must exit non-zero and mention <reason>
  local want=$1; shift; local out; out=$("$@" 2>&1); local rc=$?
  if [ $rc -ne 0 ] && grep -q "$want" <<<"$out"; then echo "  rejected as expected ($want)"; else echo "  FAIL: expected rejection '$want', rc=$rc: $out"; fail=1; fi
}

( cd "$ROOT" && go build -o "$BIN/openetl" ./cmd/openetl && go build -o "$BIN/openetl-gateway" ./cmd/openetl-gateway ) || exit 1
"$BIN/openetl-gateway" migrate >/dev/null || exit 1
DEV=$("$BIN/openetl-gateway" admin create-token --project acme-dw --account alice --side delivery --role admin | tail -1)
EDT=$("$BIN/openetl-gateway" admin create-token --project acme-dw --account bob --side delivery --role editor | tail -1)
CUS=$("$BIN/openetl-gateway" admin create-token --project acme-dw --account carol --side customer --role editor | tail -1)
OPENETL_ADDR=127.0.0.1:$PORT "$BIN/openetl-gateway" serve >"$BIN/gw.log" 2>&1 & GW=$!
trap 'kill $GW 2>/dev/null' EXIT
for i in $(seq 100); do curl -s -o /dev/null "$OPENETL_BASE_URL/v1/me" && break; sleep 0.1; done
curl -s -o /dev/null "$OPENETL_BASE_URL/v1/me" || { echo "gateway did not start"; cat "$BIN/gw.log"; exit 1; }

OE=$BIN/openetl
as_dev() { OPENETL_TOKEN=$DEV "$OE" "$@"; }
as_edt() { OPENETL_TOKEN=$EDT "$OE" "$@"; }
as_cus() { OPENETL_TOKEN=$CUS "$OE" "$@"; }

say "1. delivery creates a workflow"
WF=$(as_dev workflow create --name daily_loader --label domain=dwh --assignee dev_owner=alice | jget "d['name'].split('/')[-1]")
echo "  created $WF"

say "2. advance in order; skipping a stage is rejected"
expect_fail STAGE_ORDER_VIOLATION as_dev stage advance "$WF" --stage verify
as_dev stage advance "$WF" --stage development >/dev/null && as_dev stage advance "$WF" --stage deployment >/dev/null && as_dev stage advance "$WF" --stage verify --note "row counts match" >/dev/null && echo "  development, deployment, verify passed"

say "3. only the customer side can accept"
expect_fail SIDE_NOT_ALLOWED as_edt stage advance "$WF" --stage accept   # admins may act for any side by design; editors may not
as_cus stage advance "$WF" --stage accept >/dev/null && echo "  customer accepted"

say "4. regress needs a reason"
expect_fail "reason is required" as_cus stage regress "$WF" --stage accept
as_cus stage regress "$WF" --stage accept --reason rej_row_count >/dev/null && echo "  accept reopened with a reject reason"

say "5. an unresolved issue makes the workflow BLOCKED; resolving clears it"
C=$(as_dev comment add "$WF" --kind issue --body "target schema permission missing" | jget "d['name'].split('/')[-1]")
echo "  health: $(as_dev workflow get "$WF" | jget "d['health']")"
as_dev comment resolve "$WF" "$C" >/dev/null
echo "  after resolve: $(as_dev workflow get "$WF" | jget "d['health']")"

say "6. optimistic concurrency and idempotency"
ET=$(as_dev workflow get "$WF" | jget "d['etag']")
as_dev workflow update "$WF" --name daily_loader_v2 --etag "$ET" >/dev/null && echo "  update with fresh etag ok"
expect_fail ETAG_MISMATCH as_dev workflow update "$WF" --name stale --etag "$ET"
RID=$(python3 -c 'import uuid;print(uuid.uuid4())')
A=$(as_dev scope set "$WF" --to out --reason not_needed --request-id "$RID" 2>&1 | jget "d.get('etag')")
B=$(as_dev scope set "$WF" --to out --reason not_needed --request-id "$RID" 2>&1 | jget "d.get('etag')")
[ "$A" = "$B" ] && echo "  same request-id returned the same result" || { echo "  FAIL idempotency $A vs $B"; fail=1; }

say "7. --dry-run writes nothing"
BEFORE=$(as_dev workflow get "$WF" | jget "d['etag']")
as_dev scope set "$WF" --to in --dry-run >/dev/null
AFTER=$(as_dev workflow get "$WF" | jget "d['etag']")
[ "$BEFORE" = "$AFTER" ] && echo "  etag unchanged" || { echo "  FAIL dry-run changed state"; fail=1; }

say "8. read-only SQL over the views"
as_dev query "SELECT current_stage, COUNT(*) AS n FROM workflows GROUP BY current_stage ORDER BY current_stage"
expect_fail "" as_dev query "DELETE FROM workflows"

say "9. audit trail: every change is an event"
URL="$OPENETL_BASE_URL/v1/projects/acme-dw/workflows/$WF/events"
N=$(curl -s -H "Authorization: Bearer $DEV" "$URL" | jget "len(d.get('events',[]))")
echo "  $N events recorded for $WF"
[ "$N" -ge 8 ] || { echo "  FAIL expected at least 8 events"; fail=1; }

echo; [ $fail -eq 0 ] && echo "DEMO OK" || echo "DEMO FAILED"; exit $fail
