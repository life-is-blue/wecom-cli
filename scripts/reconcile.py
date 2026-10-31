#!/usr/bin/env python3
"""Independent reconciliation of the public views against the raw event log.

A second implementation, written from the specification and the raw tables only: it reads `entity`, `event` and
`delta` (plus the stage_def configuration) straight from MySQL, folds them in Python and compares the result, row by
row, with what the server returns for the public views `workflows`, `tasks`, `workflow_stages`, `open_items`,
`stage_distribution`, `comments` and `events` through the query endpoint.  It does not read `entity_latest`, `v_*`
or any view from the database; it also checks that the `entity_latest` cache equals the fold of `delta`.

  python3 scripts/reconcile.py --db openetl_d --project acme-dw --base-url http://127.0.0.1:18204 --token-file TOKEN
         [--mysql "mysql -h127.0.0.1 -P33307 -uoe -poe"] [--json OUT.json] [--label NAME]

Semantics implemented here (the spec, see docs/m0-naming-spec.md and db/migrations/0001..0004 headers):
  * the current value of a key is the `new_v` of the delta with the greatest event_id for (entity, key); a removed key
    has new_v SQL NULL.  Event order is the event id, never event_time (same-second events, back-dated imports).
  * a workflow / task is listed when it has at least one delta and its latest `deleted` is not true.
  * scope defaults to 'in'; a stage with no value is 'open'; current_stage is the first stage (by seq) that is neither
    'pass' nor 'na', NULL when all are; labels = attr.*, assignees = assignee.* (removed keys vanish).
  * a comment counts as open when it is not resolved and not deleted; issues block, risks put at risk.
  * merged_into_id is only shown while scope is 'out'.
  * workflow_stages, open_items and comments only carry rows of workflows that `workflows` lists (soft-deleted, unknown or
    event-less workflows are hidden); open_items has a row only for a workflow with at least one open issue, risk or
    question; tasks.primary_workflow_id is the lowest workflow id among the task's primary links.
Exit status 0 only if everything matches.
"""
import argparse
import datetime
import json
import shlex
import subprocess
import sys
import http.client
import urllib.parse

PAGE = 1000


# ----------------------------------------------------------------------------- raw dump
def unescape(s):
    if "\\" not in s:
        return s
    out, i = [], 0
    while i < len(s):
        c = s[i]
        if c == "\\" and i + 1 < len(s):
            n = s[i + 1]
            out.append({"n": "\n", "t": "\t", "0": "\0", "\\": "\\", "r": "\r"}.get(n, n))
            i += 2
        else:
            out.append(c)
            i += 1
    return "".join(out)


def dump(mysql, db, sql):
    cmd = shlex.split(mysql) + ["--default-character-set=utf8mb4", "-N", "-B", db]
    p = subprocess.run(cmd, input="SET NAMES utf8mb4 COLLATE utf8mb4_0900_ai_ci;\n" + sql, capture_output=True, text=True)
    if p.returncode != 0:
        raise SystemExit("mysql failed: " + p.stderr[:500])
    rows = []
    for ln in p.stdout.split("\n"):
        if ln == "":
            continue
        rows.append([None if f == "NULL" else unescape(f) for f in ln.split("\t")])
    return rows


def jv(s):
    return None if s is None else json.loads(s)


def parse_ts(s):
    if s is None:
        return None
    s = s.replace("T", " ").rstrip("Z")
    for fmt in ("%Y-%m-%d %H:%M:%S.%f", "%Y-%m-%d %H:%M:%S"):
        try:
            return datetime.datetime.strptime(s, fmt)
        except ValueError:
            pass
    raise ValueError(s)


# ----------------------------------------------------------------------------- server query path

def http_call(method, url, token, body=None, timeout=20):
    """One HTTP request -> (status, text). Only http(s) URLs are accepted (urllib would also open file: and ftp:)."""
    u = urllib.parse.urlsplit(url)
    if u.scheme not in ("http", "https") or not u.hostname:
        raise ValueError("unsupported URL: %r" % url)
    conn = (http.client.HTTPSConnection if u.scheme == "https" else http.client.HTTPConnection)(u.hostname, u.port, timeout=timeout)
    try:
        conn.request(method, u.path + ("?" + u.query if u.query else ""), body=body,
                     headers={"Authorization": "Bearer " + token, "Content-Type": "application/json"})
        r = conn.getresponse()
        return r.status, r.read().decode()
    finally:
        conn.close()


class Query:
    def __init__(self, base, project, token):
        self.url = "%s/v1/projects/%s:query" % (base.rstrip("/"), project)
        self.token = token

    def run(self, sql):
        rows, tok, cols = [], "", None
        while True:
            body = {"sql": sql, "page_size": PAGE}
            if tok:
                body["page_token"] = tok
            status, text = http_call("POST", self.url, self.token, json.dumps(body).encode(), timeout=300)
            if status >= 400:
                raise SystemExit("query failed: %s\n%s" % (sql, text[:500]))
            resp = json.loads(text)
            cols = [c["name"] for c in resp["columns"]]
            rows += resp["rows"]
            tok = resp.get("next_page_token") or ""
            if not tok:
                return cols, rows


# ----------------------------------------------------------------------------- fold the raw log
class Raw:
    def __init__(self, mysql, db, project):
        pid = dump(mysql, db, "SELECT id FROM project WHERE name='%s'" % project.replace("'", "''"))
        if not pid:
            raise SystemExit("no such project " + project)
        self.pid = int(pid[0][0])
        P = self.pid
        self.stages = [(r[0], int(r[1])) for r in dump(mysql, db, "SELECT stage_key, seq FROM stage_def ORDER BY seq")]
        self.entity = {}  # id -> (type, create_time)
        for r in dump(mysql, db, "SELECT id, type, create_time FROM entity WHERE project_id=%d ORDER BY id" % P):
            self.entity[int(r[0])] = (r[1], r[2])
        self.events = {}  # id -> dict
        for r in dump(mysql, db, "SELECT id, entity_id, actor, side, channel, reason_key, corrects_event_id, event_time, create_time FROM event WHERE project_id=%d ORDER BY id" % P):
            self.events[int(r[0])] = dict(entity_id=int(r[1]), actor=r[2], side=r[3], channel=r[4], reason_key=r[5],
                                          corrects=int(r[6]) if r[6] else None, event_time=r[7], create_time=r[8])
        self.deltas = []  # (event_id, entity_id, k, old_v(str|None), new_v(str|None))
        for r in dump(mysql, db, "SELECT d.event_id, d.entity_id, d.k, CAST(d.old_v AS CHAR), CAST(d.new_v AS CHAR) FROM delta d JOIN event e ON e.id=d.event_id WHERE e.project_id=%d ORDER BY d.event_id, d.k" % P):
            self.deltas.append((int(r[0]), int(r[1]), r[2], r[3], r[4]))
        self.cache = {}
        for r in dump(mysql, db, "SELECT l.entity_id, l.k, CAST(l.v AS CHAR), l.event_id FROM entity_latest l JOIN entity e ON e.id=l.entity_id WHERE e.project_id=%d" % P):
            self.cache[(int(r[0]), r[1])] = (r[2], int(r[3]))
        self.fold()

    def fold(self):
        """latest[entity][key] = (event_id, parsed json or None)"""
        self.latest = {}
        self.has_delta = set()
        for ev, eid, k, old, new in self.deltas:
            self.has_delta.add(eid)
            cur = self.latest.setdefault(eid, {})
            if k not in cur or ev > cur[k][0]:
                cur[k] = (ev, jv(new), new)

    def val(self, eid, k):
        """current value of a key or None"""
        t = self.latest.get(eid, {}).get(k)
        return None if t is None else t[1]

    def keys(self, eid, prefix):
        return {k[len(prefix):]: t[1] for k, t in self.latest.get(eid, {}).items() if k.startswith(prefix) and t[1] is not None}

    def deleted(self, eid):
        return self.val(eid, "deleted") is True or self.val(eid, "deleted") == "true"

    def of_type(self, typ):
        return [i for i, (t, _) in sorted(self.entity.items()) if t == typ]

    # ------------------------------------------------------------------ expected views
    def stage_state(self, eid, stage):
        v = self.val(eid, "stage." + stage)
        return "open" if v is None else v

    def current_stage(self, eid):
        for k, _seq in self.stages:
            if self.stage_state(eid, k) not in ("pass", "na"):
                return k
        return None

    def open_counts(self):
        """{workflow_id: {issue, risk, question, note}} over open (unresolved, undeleted) comments"""
        out = {}
        for cid in self.of_type("comment"):
            if cid not in self.has_delta:
                continue
            if self.val(cid, "resolved") is True or self.deleted(cid):
                continue
            wf = self.val(cid, "on")
            if wf is None:
                continue
            c = out.setdefault(int(wf), {"issue": 0, "risk": 0, "question": 0, "note": 0})
            kind = self.val(cid, "kind")
            if kind in c:
                c[kind] += 1
        return out

    def workflows(self):
        oc = self.open_counts()
        out = {}
        for wid in self.of_type("workflow"):
            if wid not in self.has_delta or self.deleted(wid):
                continue
            scope = self.val(wid, "scope") or "in"
            into = self.val(wid, "into")
            c = oc.get(wid, {"issue": 0, "risk": 0})
            health = "blocked" if c["issue"] > 0 else "at_risk" if c["risk"] > 0 else "ok"
            out[wid] = dict(code="WF-%06d" % wid, name=self.val(wid, "name"), scope=scope,
                            attrs=self.keys(wid, "attr.") or None, assignees=self.keys(wid, "assignee.") or None,
                            merged_into_id=int(into) if (scope == "out" and into is not None) else None,
                            current_stage=self.current_stage(wid), open_issues=c["issue"], open_risks=c["risk"], health=health,
                            create_time=parse_ts(self.entity[wid][1]))
        return out

    def tasks(self):
        out = {}
        for tid in self.of_type("task"):
            if tid not in self.has_delta or self.deleted(tid):
                continue
            links = {int(k): v for k, v in self.keys(tid, "in.").items()}
            prim = sorted(k for k, v in links.items() if v == "primary")
            out[tid] = dict(code="TK-%06d" % tid, name=self.val(tid, "name"), attrs=self.keys(tid, "attr.") or None,
                            assignees=self.keys(tid, "assignee.") or None, primaries=prim,
                            create_time=parse_ts(self.entity[tid][1]))
        return out


# ----------------------------------------------------------------------------- comparison
class Report:
    def __init__(self):
        self.compared = {}
        self.mismatches = []
        self.notes = []

    def count(self, view, n):
        self.compared[view] = self.compared.get(view, 0) + n

    def bad(self, view, key, field, expected, actual, hint=""):
        self.mismatches.append(dict(view=view, key=key, field=field, expected=expected, actual=actual, hint=hint))


def canon(x):
    return json.dumps(x, sort_keys=True, ensure_ascii=False, separators=(",", ":"))


def compare_rows(rep, view, expected, actual, fields):
    """expected/actual: {key: {field: value}}"""
    rep.count(view, len(expected))
    for k in sorted(set(expected) | set(actual)):
        if k not in actual:
            rep.bad(view, k, "<row>", "present", "missing from view")
            continue
        if k not in expected:
            rep.bad(view, k, "<row>", "absent", "unexpected row in view")
            continue
        for f in fields:
            e, a = expected[k][f], actual[k][f]
            if isinstance(e, datetime.datetime):
                a = parse_ts(a) if isinstance(a, str) else a
            if canon(e) != canon(a) if not isinstance(e, datetime.datetime) else e != a:
                rep.bad(view, k, f, e if not isinstance(e, datetime.datetime) else str(e), a if not isinstance(a, datetime.datetime) else str(a))


def as_dict(cols, rows, key):
    out = {}
    for r in rows:
        d = dict(zip(cols, r))
        out[d[key] if isinstance(key, str) else tuple(d[k] for k in key)] = d
    return out


def reconcile(raw, q, rep):
    # ---- raw-level invariants (do not involve any view)
    ev_ent = {i: e["entity_id"] for i, e in raw.events.items()}
    bad_ent = [(ev, eid) for ev, eid, *_ in raw.deltas if ev_ent.get(ev) != eid]
    rep.count("raw: delta.entity_id == event.entity_id", len(raw.deltas))
    for ev, eid in bad_ent[:20]:
        rep.bad("raw", "event %d" % ev, "delta.entity_id", ev_ent.get(ev), eid)
    # entity_latest cache == fold(delta)
    exp = {(eid, k): (t[0], t[2]) for eid, d in raw.latest.items() for k, t in d.items()}
    rep.count("raw: entity_latest == fold(delta)", len(exp))
    for key in sorted(set(exp) | set(raw.cache)):
        e, a = exp.get(key), raw.cache.get(key)
        if e is None:
            rep.bad("entity_latest", key, "<row>", "absent", a)
        elif a is None:
            rep.bad("entity_latest", key, "<row>", "present", "missing")
        else:
            ev, ejs = e
            av, ajs = a[1], a[0]
            if ev != av or canon(jv(ejs)) != canon(jv(ajs)):
                rep.bad("entity_latest", key, "v/event_id", (ev, ejs), (av, ajs))
    # old_v chain: old_v of a delta equals the new_v of the previous delta of the same key
    prev, broken = {}, 0
    for ev, eid, k, old, new in raw.deltas:
        pass
    seq = {}
    for ev, eid, k, old, new in sorted(raw.deltas, key=lambda d: (d[1], d[2], d[0])):
        key = (eid, k)
        if key in seq and canon(jv(seq[key])) != canon(jv(old)):
            broken += 1
            if broken <= 10:
                rep.notes.append("old_v chain broken: entity %d key %s event %d old=%s previous new=%s" % (eid, k, ev, old, seq[key]))
        seq[key] = new
    rep.count("raw: old_v == previous new_v", len(raw.deltas))
    if broken:
        rep.notes.append("old_v chain: %d deltas whose old_v differs from the previous new_v of the same key" % broken)

    # ---- workflows
    ew = raw.workflows()
    cols, rows = q.run("SELECT id, code, name, scope, attrs, assignees, merged_into_id, current_stage, open_issues, open_risks, health, create_time FROM workflows ORDER BY id")
    aw = as_dict(cols, rows, "id")
    for d in aw.values():
        d["merged_into_id"] = d["merged_into_id"]
        d["open_issues"], d["open_risks"] = int(d["open_issues"]), int(d["open_risks"])
    compare_rows(rep, "workflows", ew, aw, ["code", "name", "scope", "attrs", "assignees", "merged_into_id", "current_stage", "open_issues", "open_risks", "health", "create_time"])

    # ---- tasks
    et = raw.tasks()
    cols, rows = q.run("SELECT id, code, name, attrs, assignees, primary_workflow_id, create_time FROM tasks ORDER BY id")
    at = as_dict(cols, rows, "id")
    rep.count("tasks", len(et))
    for tid in sorted(set(et) | set(at)):
        if tid not in at:
            rep.bad("tasks", tid, "<row>", "present", "missing"); continue
        if tid not in et:
            rep.bad("tasks", tid, "<row>", "absent", "unexpected"); continue
        e, a = et[tid], at[tid]
        for f in ("code", "name", "attrs", "assignees"):
            if canon(e[f]) != canon(a[f]):
                rep.bad("tasks", tid, f, e[f], a[f])
        if parse_ts(a["create_time"]) != e["create_time"]:
            rep.bad("tasks", tid, "create_time", str(e["create_time"]), a["create_time"])
        prim = e["primaries"]
        apw = a["primary_workflow_id"]
        if len(prim) > 1:
            rep.notes.append("task %d has %d primary links %s (only legacy/import data); view shows %s" % (tid, len(prim), prim, apw))
        if (prim[0] if prim else None) != apw:  # prim is sorted: the lowest workflow id wins
            rep.bad("tasks", tid, "primary_workflow_id", prim[0] if prim else None, apw,
                    "the view takes the lowest id among the in.<id> keys whose latest value is 'primary'")

    # ---- workflow_stages (every workflow entity, all stages)
    cols, rows = q.run("SELECT workflow_id, stage_key, seq, status FROM workflow_stages ORDER BY workflow_id, seq")
    ast = as_dict(cols, rows, ("workflow_id", "stage_key"))
    est = {}
    for wid in raw.of_type("workflow"):
        for k, seq_ in raw.stages:
            est[(wid, k)] = dict(seq=seq_, status=raw.stage_state(wid, k))
    # a workflow that `workflows` hides (soft-deleted, no delta) has no rows here either
    hidden = {wid for wid in raw.of_type("workflow") if wid not in ew}
    strict_e = {k: v for k, v in est.items() if k[0] in ew}
    compare_rows(rep, "workflow_stages", strict_e, ast, ["seq", "status"])
    rep.compared["workflow_stages (hidden workflows: must have no rows)"] = sum(1 for k in ast if k[0] in hidden)

    # ---- open_items
    oc = raw.open_counts()
    # a workflow has an open_items row only while it has an open issue, risk or question (notes alone give none)
    ooc = {w: c for w, c in oc.items() if w in ew and c["issue"] + c["risk"] + c["question"] > 0}
    cols, rows = q.run("SELECT workflow_id, open_issues, open_risks, open_questions FROM open_items ORDER BY workflow_id")
    aoi = as_dict(cols, rows, "workflow_id")
    eoi = {w: dict(open_issues=c["issue"], open_risks=c["risk"], open_questions=c["question"]) for w, c in ooc.items()}
    aoi_n = {w: {k: int(v) for k, v in d.items() if k != "workflow_id"} for w, d in aoi.items()}
    compare_rows(rep, "open_items", eoi, aoi_n, ["open_issues", "open_risks", "open_questions"])
    rep.compared["open_items (hidden workflows: must have no rows)"] = sum(1 for w in aoi if w not in ew)

    # ---- stage_distribution
    cols, rows = q.run("SELECT stage, health, workflows FROM stage_distribution")
    asd = {(r[0], r[1]): int(r[2]) for r in rows}
    esd = {}
    for w, d in ew.items():
        if d["scope"] == "in":
            key = (d["current_stage"] or "done", d["health"])
            esd[key] = esd.get(key, 0) + 1
    rep.count("stage_distribution", len(esd))
    for key in sorted(set(esd) | set(asd)):
        if esd.get(key) != asd.get(key):
            rep.bad("stage_distribution", key, "workflows", esd.get(key), asd.get(key))

    # ---- per owner (any role present in the data)
    roles = sorted({k for w in ew.values() for k in (w["assignees"] or {})})
    for role in roles:
        ex = {}
        for w, d in ew.items():
            o = (d["assignees"] or {}).get(role)
            if o is None:
                continue
            c = ooc.get(w)
            if c is None:
                continue
            t = ex.setdefault(o, [0, 0, 0])
            t[0] += c["issue"]; t[1] += c["risk"]; t[2] += 1
        cols, rows = q.run("SELECT JSON_UNQUOTE(JSON_EXTRACT(w.assignees, '$.%s')) AS owner, SUM(o.open_issues) AS issues, SUM(o.open_risks) AS risks, COUNT(*) AS n "
                           "FROM workflows w JOIN open_items o ON o.workflow_id = w.id WHERE JSON_EXTRACT(w.assignees, '$.%s') IS NOT NULL GROUP BY owner ORDER BY owner" % (role, role))
        ac = {r[0]: [int(r[1]), int(r[2]), int(r[3])] for r in rows}
        rep.count("per-owner open items (%s)" % role, len(ex))
        for o in sorted(set(ex) | set(ac)):
            if ex.get(o) != ac.get(o):
                rep.bad("per-owner(%s)" % role, o, "[issues,risks,workflows with open items]", ex.get(o), ac.get(o))

    # ---- comments
    cols, rows = q.run("SELECT id, project_id, workflow_id, kind, resolved, deleted FROM comments ORDER BY id")
    ac = as_dict(cols, rows, "id")
    ec = {}
    for cid in raw.of_type("comment"):
        # a comment of a workflow that `workflows` hides, or pointing at no workflow, is not listed
        if cid in raw.has_delta and isinstance(raw.val(cid, "on"), int) and raw.val(cid, "on") in ew:
            ec[cid] = dict(workflow_id=raw.val(cid, "on"), kind=raw.val(cid, "kind"),
                           resolved=1 if raw.val(cid, "resolved") is True else 0, deleted=1 if raw.deleted(cid) else 0)
    for d in ac.values():
        d["resolved"], d["deleted"] = int(d["resolved"]), int(d["deleted"])
    compare_rows(rep, "comments", ec, ac, ["workflow_id", "kind", "resolved", "deleted"])

    # ---- events: one row per delta, or one row with k NULL for an event without deltas
    cols, rows = q.run("SELECT event_id, entity_id, type, actor, side, channel, reason_key, corrects_event_id, k, new_v FROM events ORDER BY event_id, k")
    aev = [tuple([canon(c) if i == 9 else c for i, c in enumerate(r)]) for r in rows]
    by_event = {}
    for ev, eid, k, old, new in raw.deltas:
        by_event.setdefault(ev, []).append((k, new))
    eev = []
    for ev, e in raw.events.items():
        t = raw.entity[e["entity_id"]][0]
        ds = by_event.get(ev) or [(None, None)]
        for k, new in ds:
            eev.append((ev, e["entity_id"], t, e["actor"], e["side"], e["channel"], e["reason_key"], e["corrects"], k, canon(jv(new))))
    rep.count("events (rows)", len(eev))
    if sorted(aev, key=lambda r: (r[0], r[8] or "")) != sorted(eev, key=lambda r: (r[0], r[8] or "")):
        ea, ee = set(map(str, aev)), set(map(str, eev))
        for x in sorted(ee - ea)[:10]:
            rep.bad("events", x[:40], "<row>", "expected", "missing")
        for x in sorted(ea - ee)[:10]:
            rep.bad("events", x[:40], "<row>", "unexpected", "present")
        if len(aev) != len(eev):
            rep.bad("events", "count", "rows", len(eev), len(aev))


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--db", required=True)
    ap.add_argument("--project", required=True)
    ap.add_argument("--base-url", required=True)
    ap.add_argument("--token-file", required=True)
    ap.add_argument("--mysql", default="mysql -h127.0.0.1 -P33307 -uoe -poe")
    ap.add_argument("--json")
    ap.add_argument("--label", default="")
    a = ap.parse_args()
    token = open(a.token_file).read().strip()
    raw = Raw(a.mysql, a.db, a.project)
    q = Query(a.base_url, a.project, token)
    rep = Report()
    reconcile(raw, q, rep)
    total = sum(rep.compared.values())
    print("== reconcile %s db=%s project=%s" % (a.label, a.db, a.project))
    print("raw: %d entities, %d events, %d deltas" % (len(raw.entity), len(raw.events), len(raw.deltas)))
    for k, v in rep.compared.items():
        print("  compared %-48s %8d" % (k, v))
    print("rows compared in total: %d; mismatches: %d" % (total, len(rep.mismatches)))
    for n in rep.notes:
        print("NOTE", n)
    for m in rep.mismatches[:60]:
        print("MISMATCH %s key=%s field=%s expected=%s actual=%s %s" % (m["view"], m["key"], m["field"], m["expected"], m["actual"], m["hint"]))
    if len(rep.mismatches) > 60:
        print("... %d more" % (len(rep.mismatches) - 60))
    if a.json:
        json.dump(dict(label=a.label, compared=rep.compared, total=total, mismatches=rep.mismatches, notes=rep.notes), open(a.json, "w"), indent=1, default=str)
    return 1 if rep.mismatches else 0


if __name__ == "__main__":
    sys.exit(main())
