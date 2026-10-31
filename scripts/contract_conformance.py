#!/usr/bin/env python3
"""Black-box conformance pass: drive a running openetl-server and compare every response with api/openapi.yaml.

usage: contract_conformance.py BASE_URL SPEC TOKENS_JSON   (TOKENS_JSON: {"alice":..,"bob":..,"carol":..,"vera":..})

Checks per call: HTTP status is declared by the spec (200/204 or `default`), Content-Type, response body against the
response schema (types, enums, required, formats, patterns, undeclared fields, writeOnly leaks), AIP-193 error shape.
Prints one `DIVERGENCE:` line per finding and exits 1 if there is any.  Needs: python3, pyyaml, requests.
"""
import hashlib, json, re, sys, uuid
from datetime import datetime
import requests, yaml

BASE, SPEC_PATH, TOK_PATH = sys.argv[1].rstrip("/"), sys.argv[2], sys.argv[3]
SPEC = yaml.safe_load(open(SPEC_PATH))
TOK = json.load(open(TOK_PATH))
findings, covered, ncalls, nerrors = [], set(), 0, 0
OPS = {}
for p, item in SPEC["paths"].items():
    for m, op in item.items():
        if isinstance(op, dict) and "operationId" in op:
            OPS[op["operationId"]] = (m.upper(), p, op, item.get("parameters", []))


def div(msg):
    findings.append(msg)
    print("DIVERGENCE:", msg)


def ref(node):
    while isinstance(node, dict) and "$ref" in node:
        cur = SPEC
        for part in node["$ref"][2:].split("/"):
            cur = cur[part]
        node = cur
    return node


def jtype(v):
    if v is None: return "null"
    if isinstance(v, bool): return "boolean"
    if isinstance(v, int): return "integer"
    if isinstance(v, float): return "number"
    if isinstance(v, str): return "string"
    if isinstance(v, list): return "array"
    return "object"


def check(schema, v, path, where, mode="response"):
    s = ref(schema)
    if s is None: return
    if "allOf" in s:
        for sub in s["allOf"]:
            check(sub, v, path, where, mode)
    if v is None:
        if s.get("nullable") or ("type" not in s and "allOf" not in s): return
        div(f"{where} {path}: null but schema is not nullable")
        return
    t = s.get("type")
    jt = jtype(v)
    if t and not (t == jt or (t == "number" and jt == "integer")):
        div(f"{where} {path}: expected {t}, got {jt} ({str(v)[:40]!r})")
        return
    if "enum" in s and v not in s["enum"]:
        div(f"{where} {path}: {v!r} not in enum {s['enum']}")
    if t == "string":
        if "pattern" in s and not re.search(s["pattern"], v): div(f"{where} {path}: {v!r} does not match {s['pattern']}")
        f = s.get("format")
        if f == "date-time":
            try: datetime.fromisoformat(v.replace("Z", "+00:00"))
            except ValueError: div(f"{where} {path}: {v!r} is not RFC 3339")
        if f == "uuid":
            try: uuid.UUID(v)
            except ValueError: div(f"{where} {path}: {v!r} is not a uuid")
        if f == "uri" and not re.match(r"^[a-z][a-z0-9+.-]*://", v): div(f"{where} {path}: {v!r} is not an absolute uri")
    if t in ("integer", "number"):
        if "minimum" in s and v < s["minimum"]: div(f"{where} {path}: {v} < minimum")
        if "maximum" in s and v > s["maximum"]: div(f"{where} {path}: {v} > maximum")
    if t == "array":
        if "minItems" in s and len(v) < s["minItems"]: div(f"{where} {path}: fewer than minItems")
        for i, it in enumerate(v):
            check(s.get("items", {}), it, f"{path}[{i}]", where, mode)
    if t == "object" or "properties" in s:
        props = s.get("properties", {})
        for r in s.get("required", []):
            if r not in v: div(f"{where} {path}: required field {r!r} missing")
        # allOf members declare their own properties; only flag undeclared fields on plain objects
        declared_elsewhere = "allOf" in s
        for k, val in v.items():
            if k in props:
                ps = ref(props[k])
                if mode == "response" and ps.get("writeOnly"): div(f"{where} {path}.{k}: writeOnly field present in response")
                check(props[k], val, f"{path}.{k}", where, mode)
            elif isinstance(s.get("additionalProperties"), dict):
                check(s["additionalProperties"], val, f"{path}.{k}", where, mode)
            elif s.get("additionalProperties") is True or declared_elsewhere:
                pass
            elif props and "additionalProperties" not in s:
                div(f"{where} {path}: undeclared field {k!r}")


def call(opid, who="alice", path_vars=None, query=None, body=None, expect=200, raw=None, headers=None, tag=None):
    """Perform one call; validate; return (status, json-or-None, response)."""
    global ncalls, nerrors
    m, tmpl, op, _ = OPS[opid]
    pth = tmpl
    for k, v in (path_vars or {}).items():
        pth = pth.replace("{" + k + "}", str(v))
    h = {"Accept": "application/json"}
    if who: h["Authorization"] = "Bearer " + TOK[who]
    h.update(headers or {})
    if raw is not None:
        r = requests.request(m, BASE + pth, params=query, data=raw, headers={**h, "Content-Type": "application/json"}, timeout=30)
    else:
        r = requests.request(m, BASE + pth, params=query, json=body, headers=h, timeout=30)
    ncalls += 1
    where = f"{opid}{'/' + tag if tag else ''} {m} {pth} -> {r.status_code}"
    if r.status_code != expect:
        div(f"{where}: expected status {expect}")
    js = None
    if r.content:
        try: js = r.json()
        except ValueError: div(f"{where}: body is not JSON: {r.text[:80]!r}")
        if not r.headers.get("Content-Type", "").startswith("application/json"):
            div(f"{where}: Content-Type {r.headers.get('Content-Type')!r}")
    resp = op["responses"]
    decl = resp.get(str(r.status_code))
    if expect < 400:
        covered.add(opid)
        if decl is None:
            div(f"{where}: success status not declared in spec ({list(resp)})")
        else:
            c = ref(decl).get("content", {}).get("application/json")
            if c: check(c["schema"], js, "$", where)
            elif js is not None: div(f"{where}: spec declares no body but server sent one")
    else:
        nerrors += 1
        check(SPEC["components"]["schemas"]["Error"], js, "$", where)
        e = (js or {}).get("error", {})
        if e.get("code") != r.status_code: div(f"{where}: error.code {e.get('code')} != HTTP status")
        det = e.get("details") or []
        if not det or det[0].get("@type") != "ErrorInfo": div(f"{where}: AIP-193 requires an ErrorInfo in details")
        else:
            rs = det[0].get("reason", "")
            if not re.fullmatch(r"[A-Z][A-Z0-9_]+[A-Z0-9]", rs): div(f"{where}: ErrorInfo.reason {rs!r} violates [A-Z][A-Z0-9_]+[A-Z0-9]")
    return r.status_code, js, r


def rid(name): return name.rsplit("/", 1)[-1]


def main():
    # ---- identity, config
    _, me, _ = call("getMe")
    P = {"project": "pa"}
    _, cfg, _ = call("getConfig", path_vars=P)
    reasons = {x["kind"]: x["reason"] for x in cfg["reasons"]}
    reject, discard = reasons.get("REJECT"), reasons.get("DISCARD")
    # ---- workflows
    _, wf, _ = call("createWorkflow", path_vars=P, body={"display_name": "wf one", "labels": {"domain": "dwh"}, "assignees": {"dev_owner": "accounts/alice"}}, query={"request_id": str(uuid.uuid4())})
    w = {**P, "workflow": rid(wf["name"])}
    _, wf2, _ = call("createWorkflow", path_vars=P, body={"display_name": "wf two"})
    w2 = {**P, "workflow": rid(wf2["name"])}
    call("createWorkflow", path_vars=P, body={"display_name": "preview"}, query={"validate_only": "true"}, tag="validate_only")
    call("createWorkflow", path_vars=P, body={"display_name": "x"}, query={"workflow_id": "wf-custom"}, expect=400, tag="client-id")
    call("createWorkflow", path_vars=P, body={}, expect=400, tag="no display_name")
    st, l1, _ = call("listWorkflows", path_vars=P, query={"page_size": 1})
    if not l1.get("next_page_token"): div("listWorkflows page_size=1 with 2 workflows: next_page_token missing")
    else: call("listWorkflows", path_vars=P, query={"page_size": 1, "page_token": l1["next_page_token"]}, tag="page2")
    call("listWorkflows", path_vars=P, query={"filter": 'labels.domain = "dwh"', "order_by": "create_time desc"}, tag="filter")
    call("listWorkflows", path_vars=P, query={"page_size": -1}, expect=400, tag="negative page_size")
    call("listWorkflows", path_vars=P, query={"page_size": 100000}, tag="page_size above max (AIP-158: coerce)")
    call("listWorkflows", path_vars=P, query={"filter": "nosuchfield = 1"}, expect=400, tag="bad filter")
    # AIP-155: a repeated request_id returns the first result
    rq = str(uuid.uuid4())
    _, r1, _ = call("createWorkflow", path_vars=P, body={"display_name": "idem"}, query={"request_id": rq}, tag="request_id first")
    _, r2, _ = call("createWorkflow", path_vars=P, body={"display_name": "idem"}, query={"request_id": rq}, tag="request_id replay")
    if r1["name"] != r2["name"]: div(f"createWorkflow request_id replay created a second workflow ({r1['name']} vs {r2['name']})")
    # empty pages must still be arrays, and every filter form the spec lists must be accepted
    _, e1, _ = call("listWorkflows", path_vars=P, query={"filter": "display_name:zzzznomatch"}, tag="empty")
    if e1.get("workflows") != []: div(f"empty listWorkflows returns workflows={e1.get('workflows')!r}, expected []")
    for f in ['health = BLOCKED', 'scope = IN_SCOPE', 'current_stage = development', 'stages.development = PASSED', 'assignees.dev_owner = "accounts/alice"', 'NOT labels.domain = "x"', 'labels.domain = "dwh" OR labels.domain = "dwh2"', 'display_name:wf']:
        call("listWorkflows", path_vars=P, query={"filter": f}, tag="filter " + f)
    call("getWorkflow", path_vars=w)
    call("getWorkflow", path_vars={**P, "workflow": "WF-" + w["workflow"][3:].upper()}, tag="uppercase id")
    call("getWorkflow", path_vars={**P, "workflow": "wf-999999"}, expect=404)
    _, cur, _ = call("getWorkflow", path_vars=w)
    call("updateWorkflow", path_vars=w, query={"update_mask": "display_name,labels"}, body={"workflow": {"display_name": "wf one b", "labels": {"domain": "dwh2"}}, "etag": cur["etag"]})
    call("updateWorkflow", path_vars=w, query={"update_mask": "display_name"}, body={"workflow": {"display_name": "stale"}, "etag": cur["etag"]}, expect=409, tag="stale etag")
    call("updateWorkflow", path_vars=w, body={"workflow": {"display_name": "x"}}, expect=400, tag="no update_mask")
    call("updateWorkflow", path_vars=w, query={"update_mask": "scope"}, body={"workflow": {}}, expect=400, tag="scope in mask")
    call("advanceStage", path_vars=w, body={"stage": "verify"}, expect=400, tag="stage order")
    call("advanceStage", path_vars=w, body={"stage": "development", "validate_only": True}, tag="validate_only")
    call("advanceStage", path_vars=w, body={"stage": "development", "note": "done", "request_id": str(uuid.uuid4())})
    call("regressStage", path_vars=w, body={"stages": ["development"]}, expect=400, tag="reason required")
    call("regressStage", path_vars=w, body={"stages": ["development"], "reason": reject})
    call("setScope", path_vars=w, body={"scope": "OUT_OF_SCOPE"}, expect=400, tag="reason required")
    call("setScope", path_vars=w, body={"scope": "OUT_OF_SCOPE", "reason": discard})
    call("setScope", path_vars=w, body={"scope": "IN_SCOPE"}, tag="back in")
    call("deleteWorkflow", path_vars=w2, query={"etag": "ev-0"}, expect=409, tag="stale etag")
    call("deleteWorkflow", path_vars=w2)
    _, g, _ = call("getWorkflow", path_vars=w2, tag="soft-deleted get (AIP-164: must return resource)")
    call("deleteWorkflow", path_vars=w2, tag="delete already deleted: idempotent 200 (AIP-164 says NOT_FOUND; documented deviation)")
    call("listWorkflows", path_vars=P, query={"show_deleted": "true"}, tag="show_deleted")
    call("undeleteWorkflow", path_vars=w2)
    call("undeleteWorkflow", path_vars=w2, tag="undelete not deleted: idempotent 200 (AIP-164 says ALREADY_EXISTS; documented deviation)")
    # ---- tasks
    _, tk, _ = call("createTask", path_vars=P, body={"display_name": "task a", "labels": {"kind": "sql"}, "workflows": [{"workflow": wf["name"], "primary": True}]})
    t = {**P, "task": rid(tk["name"])}
    call("createTask", path_vars=P, body={"display_name": "t"}, query={"task_id": "tk-custom"}, expect=400, tag="client-id")
    call("listTasks", path_vars=P, query={"page_size": 5, "filter": f'workflows.workflow = "{wf["name"]}"'})
    call("getTask", path_vars=t)
    _, curt, _ = call("getTask", path_vars=t)
    call("updateTask", path_vars=t, query={"update_mask": "display_name"}, body={"task": {"display_name": "task a2"}, "etag": curt["etag"]})
    call("assignTask", path_vars=t, body={"workflow": wf2["name"], "membership": "SHARED"})
    call("assignTask", path_vars=t, body={"workflow": wf2["name"], "membership": "NONE"}, tag="detach")
    call("deleteTask", path_vars=t)
    call("undeleteTask", path_vars=t)
    # ---- comments
    _, cm, _ = call("createComment", path_vars=w, body={"kind": "ISSUE", "body": "blocked on grants"})
    c = {**w, "comment": rid(cm["name"])}
    call("createComment", path_vars=w, body={"kind": "BAD", "body": "x"}, expect=400)
    call("listComments", path_vars=w, query={"filter": "kind = ISSUE"})
    _, ec, _ = call("listComments", path_vars=w, query={"filter": "kind = RISK"}, tag="empty")
    if ec.get("comments") != []: div(f"empty listComments returns comments={ec.get('comments')!r}")
    call("listEvents", path_vars=w, query={"as_of": "2999-01-01T00:00:00Z", "filter": "channel = API"}, tag="as_of+filter")
    call("getComment", path_vars=c)
    call("resolveComment", path_vars=c)
    call("reopenComment", path_vars=c)
    call("deleteComment", path_vars=c)
    _, lc, _ = call("listComments", path_vars=w, tag="after delete")
    print("NOTE deleted comment still listed:", any(x["name"] == cm["name"] for x in lc["comments"]))
    st, gc, _ = call("getComment", path_vars=c, expect=404, tag="deleted comment get (documented: hidden)")
    print("NOTE getComment on a deleted comment ->", st)
    # ---- attachments
    data = b"evidence-bytes-" + uuid.uuid4().bytes
    sha = hashlib.sha256(data).hexdigest()
    _, up, _ = call("initAttachmentUpload", path_vars=w, body={"filename": "e.txt", "size": len(data), "sha256": sha})
    pr = requests.put(up["upload_url"], data=data, headers=up.get("upload_headers") or {}, timeout=30)
    if pr.status_code >= 300: div(f"PUT upload_url -> {pr.status_code}")
    _, at, _ = call("createAttachment", path_vars=w, body={"filename": "e.txt", "size": len(data), "sha256": sha, "storage_key": up["storage_key"]})
    a = {**w, "attachment": rid(at["name"])}
    call("createAttachment", path_vars=w, body={"filename": "e.txt", "size": len(data), "sha256": sha, "storage_key": up["storage_key"]}, expect=409, tag="storage_key replay")
    _, dl, _ = call("downloadAttachment", path_vars=a)
    dr = requests.get(dl["download_url"], timeout=30)
    if dr.content != data: div("download_url content differs from uploaded bytes")
    call("getAttachment", path_vars=a)
    # ---- events
    _, evs, _ = call("listEvents", path_vars=w, query={"page_size": 2})
    call("getEvent", path_vars={**w, "event": rid(evs["events"][0]["name"])})
    # ---- query
    call("query", path_vars=P, body={"sql": "SELECT stage, health, workflows FROM stage_distribution", "page_size": 10})
    call("query", path_vars=P, body={"sql": "SELECT stage FROM stage_distribution", "as_of": "2026-01-01T00:00:00Z"}, expect=400, tag="as_of (not in the spec, still refused)")
    call("query", path_vars=P, body={"sql": "SELECT code FROM workflows", "page_size": 5000}, tag="page_size above 1000")
    call("query", path_vars=P, body={"sql": "SELECT * FROM api_token"}, expect=400, tag="non-view")
    # ---- import jobs
    row = {"source": {"system": "legacy", "reference": "job1", "row_hash": "h1"}, "data": {"display_name": "imported one"},
           "tasks": [{"source": {"reference": "t1"}, "data": {"display_name": "imported task"}}]}
    _, ij, _ = call("createImportJob", path_vars=P, body={"source": "legacy", "rows": [row]})
    ij_v = {**P, "importJob": rid(ij["name"])}
    call("createImportJob", path_vars=P, body={"source": "legacy", "rows": [row, {"source": {"reference": "bad"}, "data": {}}]}, tag="with row_errors")
    call("createImportJob", path_vars=P, body={"rows": []}, expect=400, tag="no source")
    call("getImportJob", path_vars=ij_v)
    call("diffImportJob", path_vars=ij_v, query={"filter": "action=NEW"})
    call("diffImportJob", path_vars=ij_v, who="bob", tag="editor")
    call("applyImportJob", path_vars=ij_v, who="bob", expect=403, tag="editor cannot apply")
    call("applyImportJob", path_vars=ij_v, body={"note": "n"})
    row2 = {**row, "source": {**row["source"], "row_hash": "h2"}, "data": {"display_name": "imported one renamed"}}
    _, ij2, _ = call("createImportJob", path_vars=P, body={"source": "legacy", "rows": [row2]}, tag="re-import")
    call("diffImportJob", path_vars={**P, "importJob": rid(ij2["name"])}, tag="changed rows")
    call("rollbackImportJob", path_vars=ij_v)
    # ---- authorization / error shapes
    call("getMe", who=None, expect=401, tag="no token")
    TOK["bad"] = "oetl_" + "0" * 48
    call("getMe", who="bad", expect=401, tag="bad token")
    call("createWorkflow", who="vera", path_vars=P, body={"display_name": "x"}, expect=403, tag="viewer write")
    call("listWorkflows", who="vera", path_vars=P, tag="viewer read")
    call("listWorkflows", path_vars={"project": "nope"}, expect=403, tag="non-member project")
    call("createWorkflow", path_vars=P, raw="{not json", expect=400, tag="malformed body")
    call("getWorkflow", path_vars={**P, "workflow": "zzz"}, expect=404, tag="malformed id")
    # documented deviation (spec info.description): wrong method on a known route is 400 METHOD_NOT_ALLOWED, not 405
    st, js, _ = call("getWorkflow", path_vars=w, who="alice", tag="control")
    r = requests.post(f"{BASE}/v1/projects/pa/workflows/{w['workflow']}", headers={"Authorization": "Bearer " + TOK["alice"]})
    if (r.status_code, r.json()["error"]["details"][0]["reason"]) != (400, "METHOD_NOT_ALLOWED"):
        div(f"unsupported method: got {r.status_code}, spec documents 400 METHOD_NOT_ALLOWED")
    # ---- admin
    call("listMembers", path_vars=P)
    call("setMember", path_vars={**P, "account": "erin"}, body={"role": "EDITOR", "side": "CUSTOMER"})
    call("setMember", path_vars={**P, "account": "erin"}, body={"role": "BAD", "side": "CUSTOMER"}, expect=400)
    call("setMember", path_vars={**P, "account": "bob"}, who="bob", body={"role": "ADMIN", "side": "DELIVERY"}, expect=403, tag="self-promotion")
    call("removeMember", path_vars={**P, "account": "erin"}, expect=204)
    _, tkn, _ = call("createToken", path_vars={"account": "bob"}, body={})
    if not tkn.get("secret", "").startswith("oetl_"): div("createToken: secret missing or without oetl_ prefix")
    _, tl, _ = call("listTokens", path_vars={"account": "bob"})
    if any("secret" in x for x in tl.get("tokens", [])): div("listTokens leaks a secret")
    call("revokeToken", path_vars={"account": "bob", "token": rid(tkn["name"])})
    call("createToken", path_vars={"account": "bob"}, who="bob", body={}, expect=403, tag="editor creates token")
    call("updateConfig", path_vars=P, query={"update_mask": "settings"}, body={"settings": {"k": "v"}})
    call("updateConfig", path_vars=P, query={"update_mask": "name"}, body={}, expect=400, tag="bad mask")
    call("getConfig", path_vars=P, who="bob", tag="non-admin config (settings stripped)")
    # ---- coverage
    missing = sorted(set(OPS) - covered)
    for m in missing: div(f"operationId {m} was never exercised successfully")
    print(f"\n{ncalls} calls, {len(OPS)} operations in spec, {len(covered)} exercised, {len(findings)} divergence(s); {nerrors} error responses checked against the Error schema")
    return 1 if findings else 0


sys.exit(main())
