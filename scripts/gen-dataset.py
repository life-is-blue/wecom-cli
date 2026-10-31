#!/usr/bin/env python3
"""Deterministic migration list: gen-dataset.py OUTDIR [--small] -> OUTDIR/rows.ndjson + OUTDIR/mapping.yaml.
One NDJSON row per workflow, its tasks nested in `tasks_json` (format of docs/cli-import.md)."""
import json, os, random, sys

args = [a for a in sys.argv[1:] if not a.startswith("--")]
out = args[0] if args else "."
n = 60 if "--small" in sys.argv else 3000
r = random.Random(20260101)
os.makedirs(out, exist_ok=True)

domains = ["dwh", "fin", "hr", "mkt"]
prios = ["A1", "A2", "B1", "B2", "B3", "C1", "C2", "C3"]
areas = ["ods", "dwd", "dws", "ads", "rpt"]
apps = ["billing", "crm", "ledger", "recruit", "campaign", "inventory", "risk"]
cats = ["etl", "sync", "report", "export"]
devs = ["dev%02d" % i for i in range(1, 14)]
pics = ["cust%d" % i for i in range(1, 8)]
kinds = ["table", "script", "schedule", "check"]
sheets = ["sheet_a", "sheet_b", "sheet_c"]
nouns = ["orders", "users", "invoice", "payroll", "events", "stock", "ledger", "leads", "refund", "audit"]

def empty(p, v):  # a real list has many blanks, both "" and null
    return r.choice(["", None]) if r.random() < p else v

def ntasks():  # skewed 0..18, mean ~3.3, some workflows have none
    if r.random() < 0.08:
        return 0
    return min(18, 1 + int(r.expovariate(1 / 3.0)))

rows = []
for i in range(1, n + 1):
    d = r.choice(domains)
    nm = "%s_%s_%s_%04d" % (d, r.choice(nouns), r.choice(["daily", "hourly", "monthly", "snap"]), i)
    batch = "b%d" % (1 + (i - 1) * 4 // n)
    ts = [{"tid": "t-%05d-%02d" % (i, j), "title": "%s_t%d" % (nm, j), "type": r.choice(kinds), "sheet": r.choice(sheets),
           "line": 2 + i * 20 + j, "batch": batch} for j in range(ntasks())]
    rows.append({"id": "src-%05d" % i, "pipeline_name": nm, "dom": d, "prio": r.choice(prios),
                 "area": empty(0.15, r.choice(areas)), "app": empty(0.08, r.choice(apps)),
                 "category": empty(0.08, r.choice(cats)), "dev": r.choice(devs), "cust": r.choice(pics),
                 "verify_pic": empty(0.95, r.choice(pics)), "tasks_json": ts})

with open(os.path.join(out, "rows.ndjson"), "w") as f:
    for x in rows:
        f.write(json.dumps(x, separators=(",", ":")) + "\n")
with open(os.path.join(out, "mapping.yaml"), "w") as f:
    f.write("""source_system: legacy-e2e
reference: [id]
fields:
  name: pipeline_name
  domain: dom
  priority: prio
  area: area
  application: app
  category: category
assignees: {dev_owner: dev, customer_pic: cust, verify_pic: verify_pic}
tasks:
  from: tasks_json
  reference: tid
  name: title
  labels: {kind: type, sheet: sheet, line: line, batch: batch}
""")
print("%d workflows, %d tasks" % (n, sum(len(x["tasks_json"]) for x in rows)))
