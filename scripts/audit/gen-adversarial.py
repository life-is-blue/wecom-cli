#!/usr/bin/env python3
"""Adversarial sample database: raw `entity` / `event` / `delta` rows full of awkward cases.

  python3 scripts/audit/gen-adversarial.py [--seed N] [--out FILE.sql] [--stats FILE.json]

Load the SQL into a migrated, empty database (scripts/audit/run.sh adversarial does everything, including
`openetl-server admin rebuild-latest` to build the entity_latest cache from the log).  Project 'adv' (id 1) holds the
data; project 'other' (id 2) holds a few unrelated entities so that project isolation is exercised too.

Shape facts taken from docs/ and scripts/gen-dataset.py: workflows ("pipelines") with 0..18 tasks (skewed, 8% none),
labels such as domain/area/application/priority/category, assignee roles dev_owner / customer_pic / verify_pic,
stages stored as event-sourced keys stage.<stage> = pass|open|na, comments of kind note|question|issue|risk with
resolve / reopen / delete, scope in|out with a merge target `into`.  The old sample had 40 pipelines and 88 jobs; this
one has ~330 workflows and ~1000 tasks, built from a bulk part (random histories) and ~35 hand-written awkward cases.

Key vocabulary written (see scripts/audit/vocab.md): name, scope, into, deleted, stage.<k>, attr.<k>, ann.<k>,
assignee.<role>, source.system|ref|hash (workflows/tasks); in.<workflow-id> = primary|shared (tasks);
on, kind, body, resolved, deleted (comments).  Removing a key writes new_v NULL.
"""
import argparse
import datetime
import json
import random

ap = argparse.ArgumentParser()
ap.add_argument("--seed", type=int, default=1)
ap.add_argument("--out", default="adversarial.sql")
ap.add_argument("--stats")
a = ap.parse_args()
R = random.Random(a.seed)

STAGES = ["development", "deployment", "verify", "accept", "dual_run", "cutover"]
REMOVE = object()
T0 = datetime.datetime(2026, 3, 2, 8, 0, 0)


def ts(dt):
    return dt.strftime("%Y-%m-%d %H:%M:%S.%f")


def q(s):
    if s is None:
        return "NULL"
    return "'" + s.replace("\\", "\\\\").replace("'", "''").replace("\0", "") + "'"


def jtext(v):
    return json.dumps(v, ensure_ascii=False, separators=(",", ":"))


class Gen:
    def __init__(self):
        self.eid = 0
        self.vid = 0
        self.entities, self.events, self.deltas = [], [], []
        self.state = {}  # (entity, key) -> json text (current value)
        self.clock = T0
        self.kinds = {}
        self.stats = {}

    def stat(self, k, n=1):
        self.stats[k] = self.stats.get(k, 0) + n

    def tick(self, seconds=0, micros=0):
        self.clock = self.clock + datetime.timedelta(seconds=seconds, microseconds=micros)
        return self.clock

    def entity(self, typ, project=1, create_time=None):
        self.eid += 1
        self.kinds[self.eid] = typ
        self.entities.append((self.eid, project, typ, create_time or self.clock))
        return self.eid

    def event(self, ent, changes, actor="bob", side="delivery", channel="cli", reason=None, note=None, project=1,
              event_time=None, create_time=None, corrects=None, idem=None):
        """changes: {key: python value | REMOVE}; returns the event id"""
        self.vid += 1
        ct = create_time or self.clock
        et = event_time or ct
        self.events.append((self.vid, project, ent, actor, side, channel, reason, note, idem, corrects, et, ct))
        for k, v in changes.items():
            old = self.state.get((ent, k))
            new = None if v is REMOVE else jtext(v)
            self.deltas.append((self.vid, ent, k, old, new))
            self.state[(ent, k)] = new
        return self.vid

    # -- composite operations -------------------------------------------------
    def workflow(self, name, labels=None, assignees=None, anns=None, source=None, project=1, stages=None, actor="alice", **kw):
        w = self.entity("workflow", project)
        ch = {}
        if name is not None:
            ch["name"] = name
        for k, v in (labels or {}).items():
            ch["attr." + k] = v
        for k, v in (anns or {}).items():
            ch["ann." + k] = v
        for k, v in (assignees or {}).items():
            ch["assignee." + k] = v
        if source:
            ch["source.system"], ch["source.ref"], ch["source.hash"] = source
        for s, v in (stages or {}).items():
            ch["stage." + s] = v
        self.event(w, ch, actor=actor, project=project, **kw)
        self.stat("workflows")
        return w

    def advance(self, w, stage, actor="bob", side="delivery", **kw):
        self.event(w, {"stage." + stage: "pass"}, actor=actor, side=side, **kw)

    def advance_upto(self, w, n):
        for s in STAGES[:n]:
            self.advance(w, s, **({"actor": "carol", "side": "customer"} if s == "accept" else {}))

    def regress(self, w, stages, reason="rej_row_count", actor="carol", side="customer", **kw):
        self.event(w, {"stage." + s: "open" for s in stages}, actor=actor, side=side, reason=reason, **kw)

    def scope(self, w, to, reason=None, into=None, **kw):
        ch = {"scope": to}
        ch["into"] = into if into is not None else REMOVE
        self.event(w, ch, actor="alice", reason=reason, **kw)

    def delete(self, w, flag=True, **kw):
        self.event(w, {"deleted": flag}, actor="alice", **kw)

    def comment(self, w, kind, body, resolved=None, deleted=None, project=1, on=None, **kw):
        c = self.entity("comment", project)
        self.event(c, {"on": on if on is not None else w, "kind": kind, "body": body}, project=project, **kw)
        self.tick(1)
        if resolved is not None:
            self.event(c, {"resolved": resolved}, project=project)
        if deleted is not None:
            self.event(c, {"deleted": deleted}, project=project)
        self.stat("comments")
        return c

    def task(self, name, links=None, labels=None, assignees=None, source=None, project=1, **kw):
        t = self.entity("task", project)
        ch = {"name": name}
        for w, m in (links or {}).items():
            ch["in.%d" % w] = m
        for k, v in (labels or {}).items():
            ch["attr." + k] = v
        for k, v in (assignees or {}).items():
            ch["assignee." + k] = v
        if source:
            ch["source.system"], ch["source.ref"], ch["source.hash"] = source
        self.event(t, ch, actor="bob", project=project, **kw)
        self.stat("tasks")
        return t

    # -- output -----------------------------------------------------------------
    def sql(self):
        o = ["SET NAMES utf8mb4;", "SET FOREIGN_KEY_CHECKS=0;",
             "INSERT INTO project(id,name) VALUES (1,'adv'),(2,'other');",
             "INSERT IGNORE INTO account(user_name,display_name,kind) VALUES ('alice','Alice','human'),('bob','Bob','human'),('carol','Carol','human'),('dev01','Dev 01','human');"]

        def batch(table, cols, rows, n=400):
            for i in range(0, len(rows), n):
                o.append("INSERT INTO %s(%s) VALUES\n%s;" % (table, cols, ",\n".join(rows[i:i + n])))
        batch("entity", "id,project_id,type,create_time",
              ["(%d,%d,%s,%s)" % (i, p, q(t), q(ts(c))) for i, p, t, c in self.entities])
        batch("event", "id,project_id,entity_id,actor,side,channel,reason_key,note,idempotency_key,corrects_event_id,event_time,create_time",
              ["(%d,%d,%d,%s,%s,%s,%s,%s,%s,%s,%s,%s)" % (i, p, e, q(ac), q(sd), q(ch), q(rk), q(nt), q(ik), "NULL" if co is None else co, q(ts(et)), q(ts(ct)))
               for i, p, e, ac, sd, ch, rk, nt, ik, co, et, ct in self.events])
        batch("delta", "event_id,entity_id,k,old_v,new_v",
              ["(%d,%d,%s,%s,%s)" % (ev, e, q(k), q(ov), q(nv)) for ev, e, k, ov, nv in self.deltas], 300)
        o.append("SET FOREIGN_KEY_CHECKS=1;")
        return "\n".join(o) + "\n"


G = Gen()
domains = ["dwh", "fin", "hr", "mkt"]
areas = ["ods", "dwd", "dws", "ads", "rpt"]
prios = ["a1", "a2", "b1", "b2", "c1"]
devs = ["dev%02d" % i for i in range(1, 14)]
pics = ["cust%d" % i for i in range(1, 8)]
nouns = ["orders", "users", "invoice", "payroll", "events", "stock", "ledger", "leads"]

# ============================================================================== bulk part: random histories
bulk = []
for i in range(120):
    G.tick(R.choice([0, 0, 1, 7]), R.randint(0, 999999))
    labels = {"domain": R.choice(domains), "priority": R.choice(prios)}
    if R.random() < 0.85:
        labels["area"] = R.choice(areas)
    if R.random() < 0.1:
        labels["area"] = ""  # empty label value
    w = G.workflow("%s_%s_%04d" % (labels["domain"], R.choice(nouns), i), labels,
                   {"dev_owner": R.choice(devs), "customer_pic": R.choice(pics)} | ({"verify_pic": R.choice(pics)} if R.random() < 0.3 else {}),
                   source=("legacy-adv", "src-%05d" % i, "%064x" % R.getrandbits(256)))
    bulk.append(w)
    G.advance_upto(w, R.choice([0, 0, 1, 2, 3, 4, 5, 6]))
    if R.random() < 0.15:
        G.event(w, {"stage.verify": "open"}, actor="carol", side="customer", reason="rej_row_count")
    for j in range(max(0, min(18, int(R.expovariate(1 / 2.2))))) if R.random() > 0.08 else []:
        G.task("%s_t%d" % (labels["domain"], j), {w: "primary"}, {"kind": R.choice(["table", "script", "check"]), "line": str(2 + i * 20 + j)})
    for _ in range(R.choice([0, 0, 0, 1, 2, 4])):
        kind = R.choice(["note", "question", "issue", "risk", "issue"])
        r = R.random()
        G.comment(w, kind, "bulk %s %d" % (kind, R.randint(0, 999)), resolved=True if r < 0.4 else None, deleted=True if 0.4 <= r < 0.5 else None)
    if R.random() < 0.06:
        G.scope(w, "out", reason=R.choice(["obsolete", "duplicate", "source_retired"]))

# ============================================================================== awkward cases
# 1. workflow with 0 tasks and one with exactly 40 tasks, plus tasks shared by several workflows
w0 = G.workflow("zero tasks", {"domain": "dwh"})
w40 = G.workflow("forty tasks", {"domain": "fin"})
t40 = [G.task("t%02d" % k, {w40: "primary"}) for k in range(40)]
G.stat("tasks_in_40_workflow", 40)

# 2. unicode and long names, hostile characters
names = ["数据仓库_订单_日加载", "ñandú 🚀 emoji 🧪 ZWJ 👨‍👩‍👧", "مرحبا بالعالم RTL ‮ txt", "é = é (combining)", "日本語のワークフロー名",
         "tab\there newline\nsecond line", "back\\slash and \"quotes\" and 'single'", "%_wildcards_%", "'; DROP TABLE workflows; --", "  leading and trailing  ",
         "x" * 255, "long " + "ü" * 3000, "emoji-end \U0001F600" * 40]
uni = [G.workflow(n, {"domain": "dwh"}) for n in names]
G.stat("unicode_or_long_names", len(names))
# very long names for tasks too
G.task("task " + "ä" * 2000, {uni[0]: "primary"})

# 3. duplicate display names (also across a soft-deleted twin)
dups = [G.workflow("duplicate name", {"domain": "hr", "priority": p}) for p in ("a1", "a2", "b1")]
dd = G.workflow("duplicate name", {"domain": "hr"})
G.delete(dd)
G.stat("duplicate_names", 4)

# 4. empty / removed labels, annotations, assignees
el = G.workflow("empty labels", {"domain": "", "area": "", "priority": "a1"}, {"dev_owner": ""}, anns={"memo": ""})
G.tick(2); G.event(el, {"attr.priority": REMOVE, "assignee.dev_owner": REMOVE}, actor="alice")        # all but empties removed
G.tick(2); G.event(el, {"attr.priority": "b2", "ann.memo": REMOVE}, actor="alice")                    # re-added after removal
nl = G.workflow(None, None)                                                                              # no name, no labels at all
G.event(nl, {"ann.only": "annotation"}, actor="alice")
ew = G.entity("workflow")                                                                                # entity without any event
ee = G.entity("task"); ec = G.entity("comment")                                                         # (invisible; workflow_stages still lists ew)
G.stat("empty_entities", 3)

# 5. tasks linked to several workflows, primary moving, detaching, double primary (invalid by API, possible from legacy)
wa, wb, wc = (G.workflow("link target %s" % x, {"domain": "mkt"}) for x in "abc")
tm = G.task("multi", {wa: "primary", wb: "shared", wc: "shared"})
G.tick(1); G.event(tm, {"in.%d" % wb: "primary", "in.%d" % wa: "shared"}, actor="bob")                   # primary moves a -> b
G.tick(1); G.event(tm, {"in.%d" % wb: REMOVE}, actor="bob")                                              # detach the primary: no primary left
G.tick(1); G.event(tm, {"in.%d" % wc: "primary"}, actor="bob")
td = G.task("double primary (legacy)", {wa: "primary", wb: "primary"}, channel="legacy", side="system")
tdel = G.task("linked then deleted", {wa: "primary"})
G.tick(1); G.delete(tdel)
tund = G.task("deleted then restored", {wa: "primary"})
G.tick(1); G.delete(tund); G.tick(1); G.delete(tund, False)
tnull = G.task("deleted then key removed", {wb: "primary"})
G.tick(1); G.delete(tnull); G.tick(1); G.event(tnull, {"deleted": REMOVE}, actor="alice")
G.stat("multi_link_tasks", 6)

# 6. merges and merge chains; scope out then back in; into kept after scope in (legacy)
m1, m2, m3, m4 = (G.workflow("merge %d" % k, {"domain": "fin"}) for k in range(1, 5))
G.scope(m1, "out", "merged", into=m2); G.tick(1)
G.scope(m2, "out", "merged", into=m3); G.tick(1)
G.scope(m3, "out", "merged", into=m4); G.tick(1)                  # chain m1 -> m2 -> m3 -> m4
G.scope(m3, "in"); G.tick(1)                                       # middle of the chain comes back in
m5 = G.workflow("merged into a soft-deleted workflow", {"domain": "fin"})
G.scope(m5, "out", "merged", into=m4); G.tick(1); G.delete(m4)
oq = G.workflow("out, back in, out, back in", {"domain": "hr"}, stages={"development": "pass"})
for _ in range(3):
    G.scope(oq, "out", "obsolete"); G.tick(1); G.scope(oq, "in"); G.tick(1)
legacy_in = G.workflow("scope in but stale into (legacy)", {"domain": "hr"})
G.event(legacy_in, {"scope": "in", "into": m1}, actor="legacy", side="system", channel="legacy")
out_noinfo = G.workflow("out without into", {"domain": "hr"})
G.event(out_noinfo, {"scope": "out"}, actor="alice", reason="obsolete")
G.stat("merge_workflows", 8)

# 7. stages regressed many times, na states, removed state, unknown stage key, wrong-case garbage
sr = G.workflow("regressed many times", {"domain": "fin"})
for rep in range(5):
    for s in STAGES[:4]:
        G.advance(sr, s, **({"actor": "carol", "side": "customer"} if s == "accept" else {}));
    G.tick(1)
    G.regress(sr, ["accept", "verify"] if rep % 2 else ["accept"], reason=["rej_row_count", "rej_value_or_schema", "rej_business_logic"][rep % 3])
    G.tick(1)
G.stat("regress_rounds", 5)
na = G.workflow("not applicable stages", {"domain": "fin"})
G.event(na, {"stage.development": "na", "stage.deployment": "na"}, actor="alice", reason="na_other", note="n/a")
for s in STAGES[2:]:
    G.advance(na, s, **({"actor": "carol", "side": "customer"} if s == "accept" else {}))
done = G.workflow("everything passed", {"domain": "fin"}, stages={s: "pass" for s in STAGES})
rem = G.workflow("stage state removed", {"domain": "fin"}, stages={"development": "pass"})
G.tick(1); G.event(rem, {"stage.development": REMOVE}, actor="alice")                                    # back to the default 'open'
unk = G.workflow("unknown stage key", {"domain": "fin"}, stages={"development": "pass"})
G.event(unk, {"stage.legacy_phase": "pass"}, actor="legacy", side="system", channel="legacy")           # not in stage_def
bad = G.workflow("garbage stage value (wrong case)", {"domain": "fin"}, stages={"development": "pass", "deployment": "PASSED"})

# 8. same-second events, back-dated events, event ids not in time order
ss = G.workflow("same second", {"domain": "dwh"})
sec = G.tick(5)
for k in range(8):                                                       # eight flips inside the same microsecond: only the id orders them
    G.event(ss, {"stage.development": "pass" if k % 2 == 0 else "open"}, actor="bob", create_time=sec, event_time=sec, reason="rej_row_count" if k % 2 else None)
G.event(ss, {"stage.development": "pass"}, actor="bob", create_time=sec, event_time=sec)
bd = G.workflow("back-dated", {"domain": "dwh"})
G.event(bd, {"stage.development": "pass"}, actor="bob", create_time=G.tick(10), event_time=G.clock - datetime.timedelta(days=30))
G.event(bd, {"stage.development": "open", "stage.deployment": "pass"}, actor="legacy", side="system", channel="legacy",
        create_time=G.tick(1), event_time=G.clock - datetime.timedelta(days=90), reason="rej_other")        # later id, much older event_time
G.stat("same_second_flips", 9)

# 9. corrections of events (import then rollback, correction of a correction)
imp = G.workflow("imported then rolled back", {"domain": "dwh"}, source=("legacy-adv", "src-imp", "ab" * 32), channel="import", side="system", actor="alice",
                 idem="imp-1-1")
ev_u = G.event(imp, {"name": "renamed by import", "attr.priority": "a1"}, actor="alice", channel="import", side="system", idem="imp-2-1")
c1 = G.event(imp, {"name": REMOVE, "attr.priority": REMOVE}, actor="alice", channel="import", side="system", corrects=ev_u, idem="imprb-2-%d" % ev_u)
G.event(imp, {"name": "renamed by import", "attr.priority": "a1"}, actor="alice", channel="import", side="system", corrects=c1, idem="imprb-2-%d-b" % c1)
imp2 = G.workflow("created by import, rolled back (soft-deleted)", {"domain": "dwh"}, channel="import", side="system", idem="imp-3-1")
G.event(imp2, {"deleted": True}, actor="alice", channel="import", side="system", corrects=G.vid, idem="imprb-3-1")
G.stat("corrected_events", 4)

# 10. soft delete / undelete / key removal
sd = G.workflow("deleted, restored, deleted", {"domain": "hr"})
G.tick(1); G.delete(sd); G.tick(1); G.delete(sd, False); G.tick(1); G.delete(sd)
sd2 = G.workflow("deleted with open issue and risk", {"domain": "hr"}, stages={"development": "pass"})
G.comment(sd2, "issue", "issue on deleted workflow"); G.comment(sd2, "risk", "risk on deleted workflow")
G.tick(1); G.delete(sd2)
sd3 = G.workflow("deleted flag removed", {"domain": "hr"})
G.delete(sd3); G.tick(1); G.event(sd3, {"deleted": REMOVE}, actor="alice")
G.stat("delete_cycles", 4)

# 11. comments of every kind, resolved / reopened / deleted combinations, many comments on one workflow
cw = G.workflow("comment zoo", {"domain": "mkt"})
for kind in ("note", "question", "issue", "risk"):
    G.comment(cw, kind, "open %s" % kind)
    G.comment(cw, kind, "resolved %s" % kind, resolved=True)
    G.comment(cw, kind, "deleted %s" % kind, deleted=True)
    G.comment(cw, kind, "resolved then deleted %s" % kind, resolved=True, deleted=True)
    c = G.comment(cw, kind, "resolved then reopened %s" % kind, resolved=True); G.tick(1); G.event(c, {"resolved": False}, actor="bob")
    c = G.comment(cw, kind, "deleted then undeleted %s" % kind, deleted=True); G.tick(1); G.event(c, {"deleted": False}, actor="bob")
    c = G.comment(cw, kind, "resolved flag removed %s" % kind, resolved=True); G.tick(1); G.event(c, {"resolved": REMOVE}, actor="bob")
only_notes = G.workflow("only notes and questions open", {"domain": "mkt"})
G.comment(only_notes, "note", "just a note"); G.comment(only_notes, "question", "a question")
risk_only = G.workflow("risk only", {"domain": "mkt"}); G.comment(risk_only, "risk", "risk")
fifty = G.workflow("fifty issues", {"domain": "mkt"})
for k in range(50):
    G.comment(fifty, "issue", "issue %d" % k, resolved=True if k % 5 == 0 else None)
cm_on_task = G.comment(None, "issue", "comment addressed to a task id", on=t40[0])         # `on` points at a non-workflow entity
cm_late = G.comment(m1, "issue", "issue on a merged-away workflow")
G.stat("comment_zoo", 28)

# 12. the other project: unrelated data must never leak into project adv
ow = G.workflow("other project workflow", {"domain": "dwh"}, project=2)
G.task("other task", {ow: "primary"}, project=2)
G.comment(ow, "issue", "other issue", project=2)

# 13. attachments (type attachment) exist and must stay out of every view
at = G.entity("attachment")
G.event(at, {"filename": "evidence.png", "size": 1234, "sha256": "0" * 64, "storage_key": "k/1", "on": w0}, actor="bob")

sql = G.sql()
open(a.out, "w", encoding="utf-8").write(sql)
stats = dict(G.stats, entities=len(G.entities), events=len(G.events), deltas=len(G.deltas), seed=a.seed,
             by_type={t: sum(1 for e in G.entities if e[2] == t) for t in ("workflow", "task", "comment", "attachment")})
if a.stats:
    json.dump(stats, open(a.stats, "w"), indent=1)
print(json.dumps(stats))
