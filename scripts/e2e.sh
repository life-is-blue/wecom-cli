#!/usr/bin/env bash
# Full flow on a clean machine with Docker + Go + python3: compose up, tokens, import, lifecycle, report, assertions.
#   scripts/e2e.sh [--small] [--keep]       exit 0 only if every assertion holds
set -euo pipefail
ROOT=$(cd "$(dirname "$0")/.." && pwd); cd "$ROOT"
SMALL=""; KEEP=0
for a in "$@"; do case $a in --small) SMALL=--small;; --keep) KEEP=1;; *) echo "usage: $0 [--small] [--keep]"; exit 2;; esac; done

W=$(mktemp -d); export HOME=$W/home; mkdir -p "$HOME"          # isolate the CLI credential store
export OPENETL_BASE_URL=http://127.0.0.1:18080 OPENETL_PROJECT=acme-dw
OE=$W/openetl
cleanup() { [ "$KEEP" = 1 ] || docker compose down -v >/dev/null 2>&1 || true; rm -r "$W"; }
trap cleanup EXIT
fail() { echo "FAIL: $*" >&2; exit 1; }
now() { date +%s.%N; }
step() { STEP=$1; T0=$(now); printf '\n== %s\n' "$1"; }
done_() { printf '   %s: %.1fs\n' "$STEP" "$(echo "$(now) - $T0" | bc)"; }
MAXMS=0
# oe_as TOKEN args...: run the CLI, fail on non-zero exit or a call slower than LIMIT_MS (5s; batch calls get more)
oe_as() {
  local tok=$1; shift; local s e ms rc=0
  s=$(date +%s%N); OPENETL_TOKEN=$tok "$OE" "$@" || rc=$?; e=$(date +%s%N); ms=$(( (e - s) / 1000000 ))
  [ $ms -gt $MAXMS ] && MAXMS=$ms
  [ $rc -eq 0 ] || fail "openetl $* (exit $rc)"
  [ $ms -le "${LIMIT_MS:-5000}" ] || fail "openetl $* took ${ms}ms (> ${LIMIT_MS:-5000})"
}
jget() { python3 -c "import sys,json;d=json.load(sys.stdin);print(eval(sys.argv[1]))" "$1"; }
q() { oe_as "$ADMIN" query "$1" --page-size 10000; }          # raw JSON
q1() { q "$1" | jget "d['rows'][0][0]"; }                       # one number
check() { [ "$2" = "$3" ] && echo "   ok  $1 = $2" || fail "$1: got $2, expected $3"; }

step "build CLI + docker compose up"
go build -o "$OE" ./cmd/openetl
docker compose up -d ${E2E_BUILD---build} --wait server   # E2E_BUILD= (empty) reuses an existing image >/dev/null
done_

step "tokens + login"
mint() { docker compose exec -T server openetl-server admin create-token --project acme-dw --account "$1" --side "$2" --role "$3" 2>/dev/null | tail -1; }
ADMIN=$(mint alice delivery admin); EDT=$(mint bob delivery editor); CUS=$(mint carol customer editor)
echo "$ADMIN" | OPENETL_TOKEN= "$OE" auth login --token-stdin >/dev/null
OPENETL_TOKEN= "$OE" auth status >/dev/null; unset OPENETL_TOKEN
done_

step "generate dataset"
python3 scripts/gen-dataset.py "$W/data" $SMALL | tee "$W/gen.txt"
N=$(awk '{print $1}' "$W/gen.txt"); NT=$(awk '{print $3}' "$W/gen.txt")
done_

step "import stage / diff / apply"
oe_as "$ADMIN" import stage --from "$W/data/rows.ndjson" --map "$W/data/mapping.yaml" --max-rows 100000 >"$W/job.json"
JOB=$(jget "d['name'].split('/')[-1]" <"$W/job.json")
oe_as "$ADMIN" import diff "$JOB" --limit 2 >"$W/diff.json"
check "diff NEW" "$(jget "d['counts']['NEW']" <"$W/diff.json")" "$N"
LIMIT_MS=30000 oe_as "$ADMIN" import apply "$JOB" --yes >/dev/null   # one-off batch of ~13k rows
check "workflows" "$(q1 'SELECT COUNT(*) FROM workflows')" "$N"
check "tasks" "$(q1 'SELECT COUNT(*) FROM tasks')" "$NT"
check "empty-string labels" "$(q1 "SELECT COUNT(*) FROM workflows WHERE JSON_EXTRACT(attrs,'\$.area') = ''")" 0
done_

step "lifecycle through the CLI"
oe_as "$ADMIN" query 'SELECT code FROM workflows ORDER BY id' --page-count 100 | python3 -c "import sys,json;[print(r[0]) for l in sys.stdin for r in json.loads(l)['rows']]" >"$W/codes"
mapfile -t CODES <"$W/codes"
check "workflow codes listed" "${#CODES[@]}" "$N"
declare -A EXP=([dual_run]=0 [accept]=0 [verify]=0 [deployment]=0 [development]=0)
OUT=0; BLOCKED=0; ISSUE_ALL=0; RISK=0; ACCEPTED=0; REGRESSED=0
adv() { for s in "${@:2}"; do oe_as "$EDT" stage advance "$1" --stage "$s" >/dev/null; done; }
for i in "${!CODES[@]}"; do
  c=${CODES[$i]}; r=$(( i % 50 )); cur=development
  if   [ $r -le 3 ];  then adv "$c" development deployment verify; oe_as "$CUS" stage advance "$c" --stage accept >/dev/null; cur=dual_run; ACCEPTED=$((ACCEPTED+1))
  elif [ $r -le 5 ];  then adv "$c" development deployment verify; oe_as "$CUS" stage advance "$c" --stage accept >/dev/null
                           oe_as "$CUS" stage regress "$c" --stage accept --reason rej_row_count >/dev/null; cur=accept; ACCEPTED=$((ACCEPTED+1)); REGRESSED=$((REGRESSED+1))
  elif [ $r -le 9 ];  then adv "$c" development deployment verify; cur=accept
  elif [ $r -le 14 ]; then adv "$c" development deployment; cur=verify
  elif [ $r -le 19 ]; then adv "$c" development; cur=deployment
  elif [ $r -eq 20 ]; then oe_as "$EDT" comment add "$c" --kind issue --body "target schema permission missing" >/dev/null; BLOCKED=$((BLOCKED+1)); ISSUE_ALL=$((ISSUE_ALL+1))
  elif [ $r -eq 21 ]; then cm=$(oe_as "$EDT" comment add "$c" --kind issue --body "row count off" | jget "d['name'].split('/')[-1]")
                           oe_as "$EDT" comment resolve "$c" "$cm" >/dev/null; ISSUE_ALL=$((ISSUE_ALL+1))
  elif [ $r -eq 22 ]; then oe_as "$EDT" comment add "$c" --kind risk --body "late source delivery" >/dev/null; RISK=$((RISK+1))
  elif [ $r -eq 23 ]; then oe_as "$ADMIN" scope set "$c" --to out --reason obsolete >/dev/null; OUT=$((OUT+1)); cur=out
  elif [ $r -eq 24 ] && [ -n "${CODES[$((i+1))]:-}" ]; then oe_as "$ADMIN" scope set "$c" --to out --reason merged --into "${CODES[$((i+1))]}" >/dev/null; OUT=$((OUT+1)); cur=out
  fi
  [ $cur = out ] || EXP[$cur]=$(( ${EXP[$cur]} + 1 ))
done
echo "   accepted=$ACCEPTED regressed=$REGRESSED blocked=$BLOCKED out_of_scope=$OUT"
done_

step "management report (openetl query)"
echo "-- stage distribution"; q 'SELECT stage, SUM(workflows) AS n FROM stage_distribution GROUP BY stage ORDER BY stage' | tee "$W/sd.json" | jget "d['rows']"
for s in dual_run accept verify deployment development; do
  check "stage $s" "$(jget "int(next((r[1] for r in d['rows'] if r[0]=='$s'),0))" <"$W/sd.json")" "${EXP[$s]}"
done
check "total workflows == in-scope + out-of-scope" "$(q1 'SELECT COUNT(*) FROM workflows')" "$N"
check "stage_distribution sum == in-scope" "$(jget "sum(int(r[1]) for r in d['rows'])" <"$W/sd.json")" "$((N - OUT))"
check "out of scope" "$(q1 "SELECT COUNT(*) FROM workflows WHERE scope='out'")" "$OUT"
echo "-- blocked list (first 5)"; q "SELECT code, name, open_issues FROM workflows WHERE health='blocked' ORDER BY id LIMIT 5" | jget "d['rows']"
check "blocked workflows" "$(q1 "SELECT COUNT(*) FROM workflows WHERE health='blocked'")" "$BLOCKED"
check "unresolved issues" "$(q1 'SELECT SUM(open_issues) FROM open_items')" "$BLOCKED"
check "open risks" "$(q1 'SELECT SUM(open_risks) FROM open_items')" "$RISK"
check "all issues ever == comments" "$(q1 "SELECT COUNT(*) FROM comments WHERE kind='issue'")" "$ISSUE_ALL"
echo "-- open items per dev owner (top 5)"
q "SELECT JSON_UNQUOTE(e.new_v) AS owner, SUM(o.open_issues) AS issues, SUM(o.open_risks) AS risks FROM events e JOIN open_items o ON o.workflow_id = e.entity_id WHERE e.k = 'assignee.dev_owner' GROUP BY owner ORDER BY issues DESC LIMIT 5" | jget "d['rows']"
check "owner issues add up" "$(q1 "SELECT SUM(o.open_issues) FROM events e JOIN open_items o ON o.workflow_id = e.entity_id WHERE e.k = 'assignee.dev_owner'")" "$BLOCKED"
echo "-- burn-up: accepted per day"
q "SELECT DATE(event_time) AS day, COUNT(*) AS n FROM events WHERE k = 'stage.accept' AND JSON_UNQUOTE(new_v) = 'pass' GROUP BY DATE(event_time) ORDER BY day" | tee "$W/bu.json" | jget "d['rows']"
check "burn-up total" "$(jget "sum(int(r[1]) for r in d['rows'])" <"$W/bu.json")" "$ACCEPTED"
check "reopened accepts" "$(q1 "SELECT COUNT(*) FROM events WHERE k = 'stage.accept' AND JSON_UNQUOTE(new_v) = 'open' AND reason_key = 'rej_row_count'")" "$REGRESSED"
done_

printf '\nslowest single CLI call: %dms\nE2E OK (%s workflows, %s tasks)\n' "$MAXMS" "$N" "$NT"
