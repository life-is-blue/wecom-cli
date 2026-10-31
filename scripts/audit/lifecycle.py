#!/usr/bin/env python3
"""Scripted lifecycle through the real CLI, as scripts/e2e.sh does it but with the awkward paths added.

  python3 scripts/audit/lifecycle.py --cli BIN --base-url URL --project P --tokens DIR [--n 60]

DIR holds tok_alice (delivery admin), tok_bob (delivery editor), tok_carol (customer editor) as printed by
`openetl-server admin create-token`.  A fresh gen-dataset.py dataset is imported first (stage -> diff -> apply), then every
workflow gets one of 20 scripted histories (regress several times, comments of every kind with resolve / reopen /
delete, merge chains, out of scope and back, soft delete and restore, label replacement, unicode names) and tasks
are created, shared between workflows, re-pointed, detached, deleted and restored.
"""
import argparse
import json
import os
import subprocess
import sys
import tempfile

ap = argparse.ArgumentParser()
ap.add_argument("--cli", required=True)
ap.add_argument("--base-url", required=True)
ap.add_argument("--project", required=True)
ap.add_argument("--tokens", required=True)
ap.add_argument("--n", type=int, default=60)
a = ap.parse_args()
HERE = os.path.dirname(os.path.abspath(__file__))
TOK = {n: open(os.path.join(a.tokens, "tok_" + n)).read().strip() for n in ("alice", "bob", "carol")}
HOME = tempfile.mkdtemp(prefix="lc-home-")
calls = {"ok": 0, "expected_fail": 0}


def oe(who, *args, fail=False, raw=False):
    env = {"PATH": os.environ["PATH"], "HOME": HOME, "OPENETL_BASE_URL": a.base_url, "OPENETL_PROJECT": a.project,
           "OPENETL_TOKEN": TOK[who]}
    p = subprocess.run([a.cli] + list(args), capture_output=True, text=True, env=env)
    if fail:
        if p.returncode == 0:
            raise SystemExit("expected failure but succeeded: %s" % list(args))
        calls["expected_fail"] += 1
        return None
    if p.returncode != 0:
        raise SystemExit("FAILED (%d): openetl %s\n%s%s" % (p.returncode, " ".join(args), p.stdout, p.stderr))
    calls["ok"] += 1
    return p.stdout if raw else json.loads(p.stdout)


def q(sql):
    cols, rows = None, []
    d = oe("alice", "query", sql, "--page-size", "1000")
    return d["rows"]


def last(name):
    return name.split("/")[-1]


# ----------------------------------------------------------------------------- import
data = tempfile.mkdtemp(prefix="lc-data-")
out = subprocess.run([sys.executable, os.path.join(HERE, "..", "gen-dataset.py"), data] + (["--small"] if a.n <= 60 else []),
                     capture_output=True, text=True, check=True).stdout
print(out.strip())
job = last(oe("alice", "import", "stage", "--from", data + "/rows.ndjson", "--map", data + "/mapping.yaml", "--max-rows", "100000")["name"])
oe("alice", "import", "diff", job, "--limit", "2")
oe("alice", "import", "apply", job, "--yes")
codes = [r[0] for r in q("SELECT code FROM workflows ORDER BY id")]
tcodes = [r[0] for r in q("SELECT code FROM tasks ORDER BY id")]
print("imported %d workflows, %d tasks" % (len(codes), len(tcodes)))


def adv(c, *stages, who="bob"):
    for s in stages:
        oe(who, "stage", "advance", c, "--stage", s)


def comment(c, kind, body, who="bob"):
    return last(oe(who, "comment", "add", c, "--kind", kind, "--body", body)["name"])


# ----------------------------------------------------------------------------- histories
for i, c in enumerate(codes):
    nxt = codes[(i + 1) % len(codes)]
    r = i % 20
    if r == 0:
        adv(c, "development", "deployment", "verify"); oe("carol", "stage", "advance", c, "--stage", "accept")
    elif r == 1:  # regressed several times
        adv(c, "development", "deployment", "verify"); oe("carol", "stage", "advance", c, "--stage", "accept")
        oe("carol", "stage", "regress", c, "--stage", "accept", "--reason", "rej_row_count")
        oe("carol", "stage", "advance", c, "--stage", "accept")
        oe("carol", "stage", "regress", c, "--stage", "verify", "--stage", "accept", "--reason", "rej_value_or_schema")
        adv(c, "verify")
        oe("alice", "stage", "regress", c, "--stage", "verify", "--reason", "rej_business_logic")
    elif r == 2:
        adv(c, "development", "deployment", "verify")
    elif r == 3:
        adv(c, "development", "deployment")
    elif r == 4:
        adv(c, "development")
    elif r == 5:
        comment(c, "issue", "target schema permission missing")
    elif r == 6:
        cm = comment(c, "issue", "row count off"); oe("bob", "comment", "resolve", c, cm)
    elif r == 7:
        comment(c, "risk", "late source delivery")
    elif r == 8:
        comment(c, "note", "ping 工作流 \U0001F680"); comment(c, "question", "who owns the lookup table?")
    elif r == 9:
        cm = comment(c, "issue", "flaky"); oe("bob", "comment", "resolve", c, cm); oe("bob", "comment", "reopen", c, cm)
    elif r == 10:
        cm = comment(c, "issue", "entered by mistake"); oe("bob", "comment", "delete", c, cm)
        cm = comment(c, "risk", "also resolved"); oe("bob", "comment", "resolve", c, cm)
    elif r == 11:
        oe("alice", "scope", "set", c, "--to", "out", "--reason", "obsolete")
    elif r in (12, 13):  # merge chain 12 -> 13 -> 14
        oe("alice", "scope", "set", c, "--to", "out", "--reason", "merged", "--into", nxt)
    elif r == 14:
        oe("alice", "scope", "set", c, "--to", "out", "--reason", "merged", "--into", nxt)
    elif r == 15:  # out of scope, then back
        adv(c, "development")
        oe("alice", "scope", "set", c, "--to", "out", "--reason", "source_retired")
        oe("alice", "scope", "set", c, "--to", "in", "--note", "restored")
        oe("alice", "scope", "set", c, "--to", "out", "--reason", "duplicate")
        oe("alice", "scope", "set", c, "--to", "in")
    elif r == 16:  # soft-deleted and restored, with an open issue in between
        comment(c, "issue", "blocked while deleted")
        oe("alice", "workflow", "delete", c); oe("alice", "workflow", "restore", c)
    elif r == 17:  # stays deleted, with an open issue and a risk
        adv(c, "development", "deployment")
        comment(c, "issue", "orphan issue"); comment(c, "risk", "orphan risk")
        oe("alice", "workflow", "delete", c)
    elif r == 18:  # label replacement, unicode, long name
        oe("alice", "workflow", "update", c, "--name", "переименован_工作流_" + "x" * 200, "--label", "domain=fin", "--label", "tier=gold",
           "--assignee", "dev_owner=zoë")
        oe("alice", "workflow", "update", c, "--label", "domain=fin")
        oe("alice", "workflow", "update", c, "--assignee", "dev_owner=alice", "--assignee", "customer_pic=carol")
    elif r == 19:  # all stages passed, accepted by the customer, then an issue
        adv(c, "development", "deployment", "verify"); oe("carol", "stage", "advance", c, "--stage", "accept"); adv(c, "dual_run", "cutover")
        comment(c, "issue", "post-cutover defect")
# scope out then merged chain: bring 13 back in while 12 still points at it
if len(codes) > 14:
    oe("alice", "scope", "set", codes[13], "--to", "in")

# ----------------------------------------------------------------------------- tasks
t = lambda i: tcodes[i % len(tcodes)]
for k in range(0, 12):
    w1, w2 = codes[(k * 3) % len(codes)], codes[(k * 3 + 1) % len(codes)]
    name = oe("bob", "task", "create", "--name", "adhoc 任务 %d" % k, "--workflow", w1, "--workflow", w2, "--label", "kind=check")["name"]
    tk = last(name)
    if k % 4 == 1:
        oe("bob", "task", "assign", tk, "--workflow", w2, "--membership", "primary")      # primary moves
    if k % 4 == 2:
        oe("bob", "task", "assign", tk, "--workflow", w1, "--membership", "none")          # detach primary
    if k % 4 == 3:
        oe("bob", "task", "delete", tk)
        if k == 3:
            oe("bob", "task", "restore", tk)
for k in range(6):  # tasks created by the import
    tk = t(k * 7)
    if k == 0:
        oe("bob", "task", "assign", tk, "--workflow", codes[2], "--membership", "shared")
    elif k == 1:
        oe("bob", "task", "assign", tk, "--workflow", codes[2], "--membership", "primary")
    elif k == 2:
        oe("bob", "task", "delete", tk)
    elif k == 3:
        oe("bob", "task", "update", tk, "--name", "renamed " + "z" * 150, "--label", "kind=sql", "--assignee", "dev_owner=bob")
    elif k == 4:
        oe("bob", "task", "assign", tk, "--workflow", codes[0], "--membership", "none")
# rejections must not leave traces
oe("bob", "stage", "advance", codes[4 if len(codes) > 4 else 0], "--stage", "accept", fail=True)  # wrong side + order
oe("bob", "scope", "set", codes[0], "--to", "out", "--reason", "no_such_reason", fail=True)
print("lifecycle done: %(ok)d calls ok, %(expected_fail)d expected rejections" % calls)
