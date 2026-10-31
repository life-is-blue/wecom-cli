#!/usr/bin/env bash
# Seeds a FRESH database with a realistic load for performance measurements and prints nothing but progress.
#   PERF_DB=openetl_perf2 scripts/perf-seed.sh
# Creates (dropping any previous copy!) database $PERF_DB, applies migrations 0001..$PERF_MAX_MIGRATION
# (default 3, i.e. the schema BEFORE entity_latest; run `openetl-gateway migrate` afterwards to get the
# new schema, which backfills entity_latest from delta), then inserts project `perf` with
#   PERF_WORKFLOWS (3000) workflows, PERF_TASKS (10000) tasks linked via in.<workflow> keys,
#   ~2 comments per workflow and stage advances for ~60% of the workflows.
# The rows are written with plain SQL but have exactly the shape Apply produces (one create event per
# entity, one event per stage advance, one delta per key). Connection: PERF_MYSQL (default below).
set -euo pipefail
ROOT=$(cd "$(dirname "$0")/.." && pwd)
DB=${PERF_DB:?set PERF_DB to the database to (re)create}
MAXMIG=${PERF_MAX_MIGRATION:-3}
MYSQL=${PERF_MYSQL:-mysql --protocol=tcp -h127.0.0.1 -P33306 -uroot}
export PERF_WORKFLOWS=${PERF_WORKFLOWS:-3000} PERF_TASKS=${PERF_TASKS:-10000}

$MYSQL -e "DROP DATABASE IF EXISTS \`$DB\`; CREATE DATABASE \`$DB\` CHARACTER SET utf8mb4"
$MYSQL "$DB" -e "CREATE TABLE schema_migrations (version VARCHAR(64) PRIMARY KEY, applied_at DATETIME(6) NOT NULL) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4"
for f in "$ROOT"/db/migrations/*.sql; do
  n=$(basename "$f")
  [ "${n%%_*}" -le "$MAXMIG" ] || continue
  $MYSQL "$DB" < "$f"
  $MYSQL "$DB" -e "INSERT INTO schema_migrations VALUES ('$n', NOW(6))"
done
python3 -I - <<'PY' | $MYSQL "$DB"
import json, os, random
random.seed(42)
W, T = int(os.environ["PERF_WORKFLOWS"]), int(os.environ["PERF_TASKS"])
ts = "'2026-01-01 00:00:00.000000'"
q = lambda s: "'" + s.replace("\\", "\\\\").replace("'", "''") + "'"
ent, ev, dl = [], [], []
eid = evid = 0
def entity(typ):
    global eid
    eid += 1
    ent.append(f"(1,'{typ}',{ts})")
    return eid
def event(e):
    global evid
    evid += 1
    ev.append(f"(1,{e},'alice','delivery','api',{ts},{ts})")
    return evid
def delta(evn, e, k, v):
    dl.append(f"({evn},{e},{q(k)},NULL,{q(json.dumps(v))})")
print("SET NAMES utf8mb4; SET FOREIGN_KEY_CHECKS=0;")
print("INSERT INTO project(id,name) VALUES (1,'perf'); INSERT INTO account(user_name,kind) VALUES ('alice','human');")
print("INSERT INTO member(project_id,user_name,role,side) VALUES (1,'alice','admin','delivery');")
stages = ["development", "deployment", "verify", "accept", "dual_run", "cutover"]
wfs = []
for i in range(W):
    w = entity("workflow"); wfs.append(w)
    n = event(w)
    for k, v in (("name", f"wf_{i:05d}"), ("attr.domain", random.choice(["dwh", "ads", "risk", "crm"])), ("assignee.dev_owner", "alice")):
        delta(n, w, k, v)
    depth = random.choice([0, 0, 1, 2, 3, 4, 6]) # 0 = untouched
    for s in stages[:depth]:
        n = event(w); delta(n, w, "stage." + s, "pass")
    for _ in range(2):
        c = entity("comment"); n = event(c)
        delta(n, c, "on", str(w)); delta(n, c, "kind", random.choice(["note", "issue", "risk", "question"]))
        delta(n, c, "body", "comment body for " + str(w))
        if random.random() < 0.5:
            n = event(c); delta(n, c, "resolved", True)
for i in range(T):
    t = entity("task"); n = event(t)
    delta(n, t, "name", f"task_{i:05d}"); delta(n, t, "attr.kind", "sql")
    delta(n, t, "in." + str(random.choice(wfs)), "primary")
def flush(table, cols, rows):
    for i in range(0, len(rows), 1000):
        print(f"INSERT INTO {table}({cols}) VALUES " + ",".join(rows[i:i+1000]) + ";")
# explicit ids keep entity/event/delta consistent (fresh database, ids start at 1)
flush("entity", "project_id,type,create_time", ent)
flush("event", "project_id,entity_id,actor,side,channel,event_time,create_time", ev)
flush("delta", "event_id,entity_id,k,old_v,new_v", dl)
PY
$MYSQL "$DB" -e "SELECT (SELECT COUNT(*) FROM entity) entities, (SELECT COUNT(*) FROM event) events, (SELECT COUNT(*) FROM delta) deltas"
