#!/usr/bin/env bash
# Times SELECT COUNT(*) on every public view (and a few full-row reads) against a seeded database.
#   PERF_DB=openetl_perf2 scripts/perf-measure.sh
# Every statement runs with a server-side 60s limit (MariaDB max_statement_time / MySQL
# max_execution_time), so a runaway query is aborted by the server instead of lingering.
# API-level timings (list page, get, stage advance) come from the perf-tagged Go test:
#   OPENETL_PERF_DSN='user:pw@tcp(host:port)/$PERF_DB' go test -tags perf -run TestPerf -v ./internal/store
set -u
DB=${PERF_DB:?set PERF_DB}
MYSQL=${PERF_MYSQL:-mysql --protocol=tcp -h127.0.0.1 -P33306 -uroot}
LIMIT=${PERF_LIMIT_S:-60}
timeit() { # timeit <label> <sql>
  local t0 t1 out rc
  t0=$(date +%s.%N)
  out=$($MYSQL -N -B "$DB" -e "SET SESSION max_statement_time=$LIMIT; $2" 2>&1 || true)
  if grep -q "max_statement_time\|Unknown system variable" <<<"$out" && grep -qi "unknown system variable" <<<"$out"; then
    out=$($MYSQL -N -B "$DB" -e "SET SESSION max_execution_time=$((LIMIT*1000)); $2" 2>&1 || true)
  fi
  t1=$(date +%s.%N)
  printf '%-34s %8.3fs   %s\n' "$1" "$(echo "$t1 - $t0" | bc -l)" "$(tr '\n' ' ' <<<"$out" | cut -c1-60)"
}
for v in workflows tasks events comments workflow_stages stage_distribution open_items; do
  timeit "COUNT(*) $v" "SELECT COUNT(*) FROM $v"
done
timeit "stage_distribution (rows)" "SELECT stage, health, workflows FROM stage_distribution ORDER BY stage, health"
timeit "open_items (rows)" "SELECT COUNT(*), SUM(open_issues) FROM open_items"
timeit "workflows page of 100" "SELECT id, code, name, current_stage, health FROM workflows ORDER BY id LIMIT 100"
timeit "tasks page of 100" "SELECT id, code, name, primary_workflow_id FROM tasks ORDER BY id LIMIT 100"
