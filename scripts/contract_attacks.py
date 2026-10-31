#!/usr/bin/env python3
"""Live regression attacks against a running openetl-server (see docs/threat-model.md, section 'Verification').

usage: contract_attacks.py BASE_URL TOKENS_JSON CLI_BINARY
TOKENS_JSON: {"alice": admin of pa, "bob": editor of pa, "dave": admin of pb, "erin": editor of pb}
Every attack prints `ATTACK <id> <PASS|FAIL> - <what was observed>`; exit 1 if any FAIL.
"""
import hashlib, json, os, subprocess, sys, tempfile, threading, uuid
from http.server import BaseHTTPRequestHandler, HTTPServer
import requests

BASE, TOK, CLI = sys.argv[1].rstrip("/"), json.load(open(sys.argv[2])), sys.argv[3]
fails = []


def req(who, method, path, **kw):
    h = {"Authorization": "Bearer " + TOK[who]} if who else {}
    return requests.request(method, BASE + path, headers=h, timeout=30, **kw)


def verdict(aid, ok, msg):
    print(f"ATTACK {aid} {'PASS' if ok else 'FAIL'} - {msg}")
    if not ok: fails.append(aid)


def q(who, project, sql):
    return req(who, "POST", f"/v1/projects/{project}:query", json={"sql": sql, "page_size": 1000})


def main():
    # fixtures: one workflow per project, distinguishable by name
    wa = req("alice", "POST", "/v1/projects/pa/workflows", json={"display_name": "SECRET-PA"}).json()
    wb = req("dave", "POST", "/v1/projects/pb/workflows", json={"display_name": "OWN-PB"}).json()
    assert "name" in wa and "name" in wb, (wa, wb)

    # ---- A1: cross-project read through the query endpoint
    own = q("dave", "pb", "SELECT name FROM workflows")
    names = [r[0] for r in own.json().get("rows", [])]
    verdict("A1a", own.status_code == 200 and names == ["OWN-PB"], f"dave queries pb: {names}")
    r = q("dave", "pa", "SELECT name FROM workflows")
    verdict("A1b", r.status_code == 403, f"dave queries projects/pa -> {r.status_code} {r.json()['error']['details'][0]['reason']}")
    attempts = [
        "SELECT name FROM workflows WHERE project_id = 1",
        "SELECT name FROM workflows WHERE 1=1 OR project_id <> 0",
        "SELECT name FROM workflows UNION SELECT name FROM workflows",
        "SELECT name FROM (SELECT * FROM workflows) x",
        "SELECT w.name FROM workflows w WHERE w.name IN (SELECT name FROM workflows)",
        "SELECT name FROM openetl_c.workflows",
        "SELECT name FROM `openetl_c`.`workflows`",
        "SELECT * FROM api_token",
        "SELECT * FROM information_schema.tables",
        "SELECT name FROM workflows /*!50000 UNION SELECT name FROM workflows */",
        "SELECT name FROM workflows; SELECT 1",
        "SELECT name FROM workflows WHERE name = @@version",
        "SELECT name FROM workflows INTO OUTFILE '/tmp/x'",
        "WITH x AS (SELECT * FROM workflows) SELECT name FROM x",
    ]
    leaked, accepted = [], []
    for sql in attempts:
        r = q("dave", "pb", sql)
        if r.status_code == 200:
            accepted.append(sql)
            rows = json.dumps(r.json().get("rows"))
            if "SECRET-PA" in rows: leaked.append(sql)
    # statements the guard accepts are still wrapped in the project filter: acceptable as long as nothing of pa shows up
    verdict("A1c", not leaked, f"{len(attempts)} bypass attempts; {len(attempts) - len(accepted)} rejected, {len(accepted)} accepted but project-filtered ({accepted}); rows of project pa leaked: {leaked}")

    # ---- A2: parenthesised / multi-table FROM
    paren = ["SELECT name FROM (workflows)", "SELECT name FROM (workflows) w", "SELECT w.name FROM workflows w, tasks t",
             "SELECT w.name FROM (workflows w JOIN tasks t ON w.id = t.primary_workflow_id)",
             "SELECT w.name FROM workflows w JOIN (tasks) t ON w.id = t.primary_workflow_id",
             "SELECT w.name FROM workflows w CROSS JOIN tasks t", "SELECT w.name FROM workflows w JOIN tasks t ON 1=1",
             "SELECT w.name FROM workflows w JOIN tasks t ON 1", "SELECT w.name FROM workflows w NATURAL JOIN tasks t"]
    res = [(s, q("dave", "pb", s).status_code) for s in paren]
    verdict("A2", all(c == 400 for _, c in res), f"statuses={[c for _, c in res]} for {len(paren)} statements")
    ok = q("dave", "pb", "SELECT w.name, t.name FROM workflows w JOIN tasks t ON w.id = t.primary_workflow_id")
    verdict("A2-control", ok.status_code == 200, f"legitimate join still works -> {ok.status_code}")

    # ---- A3: cross-project token / membership escalation
    r1 = req("alice", "POST", "/v1/accounts/dave/tokens", json={})
    r2 = req("alice", "GET", "/v1/accounts/dave/tokens")
    r3 = req("alice", "PUT", "/v1/projects/pa/members/dave", json={"role": "ADMIN", "side": "DELIVERY"})
    r4 = req("alice", "POST", "/v1/accounts/dave/tokens/1:revoke")
    reasons = [r.json()["error"]["details"][0]["reason"] for r in (r1, r2, r3, r4)]
    verdict("A3", [r.status_code for r in (r1, r2, r3, r4)] == [403] * 4,
            f"alice(admin pa) on dave(admin pb): create token / list tokens / add to pa / revoke -> {[r.status_code for r in (r1, r2, r3, r4)]} {reasons}")
    me = req("dave", "GET", "/v1/me").json()
    verdict("A3-control", [m["name"] for m in me["memberships"]] == ["projects/pb/members/dave"], f"dave memberships unchanged: {[m['name'] for m in me['memberships']]}")

    # ---- A4: replay of an upload URL after registration
    wf = wa["name"].split("/")[-1]
    data = b"original-evidence-" + uuid.uuid4().bytes
    sha = hashlib.sha256(data).hexdigest()
    up = req("alice", "POST", f"/v1/projects/pa/workflows/{wf}/attachments:initUpload", json={"filename": "e.bin", "size": len(data), "sha256": sha}).json()
    put1 = requests.put(up["upload_url"], data=data, headers=up.get("upload_headers") or {}, timeout=30)
    at = req("alice", "POST", f"/v1/projects/pa/workflows/{wf}/attachments", json={"filename": "e.bin", "size": len(data), "sha256": sha, "storage_key": up["storage_key"]})
    evil = b"TAMPERED-evidence-" + uuid.uuid4().bytes
    put2 = requests.put(up["upload_url"], data=evil, headers=up.get("upload_headers") or {}, timeout=30)
    dl = req("alice", "GET", "/v1/" + at.json()["name"] + ":download").json()
    got = requests.get(dl["download_url"], timeout=30).content
    re_reg = req("alice", "POST", f"/v1/projects/pa/workflows/{wf}/attachments", json={"filename": "e.bin", "size": len(evil), "sha256": hashlib.sha256(evil).hexdigest(), "storage_key": up["storage_key"]})
    verdict("A4", put1.status_code < 300 and at.status_code == 200 and got == data and re_reg.status_code == 409,
            f"first PUT {put1.status_code}, register {at.status_code}, replayed PUT {put2.status_code}, committed bytes unchanged={got == data}, re-register tampered bytes -> {re_reg.status_code} {re_reg.json().get('error', {}).get('details', [{}])[0].get('reason')}")

    # ---- A5: CLI must not send a saved token to another base URL
    seen = {"b": [], "c": []}

    def handler(tag, redirect_to=None):
        class H(BaseHTTPRequestHandler):
            def do_GET(self):
                seen[tag].append(self.headers.get("Authorization"))
                if redirect_to:
                    self.send_response(302); self.send_header("Location", redirect_to + self.path); self.end_headers()
                else:
                    self.send_response(200); self.send_header("Content-Type", "application/json"); self.end_headers(); self.wfile.write(b"{}")
            def log_message(self, *a): pass
        return H
    sc = HTTPServer(("127.0.0.1", 0), handler("c"))            # ephemeral ports: other tools may hold fixed ones
    sb = HTTPServer(("127.0.0.1", 0), handler("b", f"http://127.0.0.1:{sc.server_port}"))
    OTHER = f"http://127.0.0.1:{sb.server_port}"
    for s in (sc, sb): threading.Thread(target=s.serve_forever, daemon=True).start()
    home = tempfile.mkdtemp()
    env = {"PATH": os.environ["PATH"], "HOME": home, "OPENETL_PROJECT": "pa"}
    login = subprocess.run([CLI, "auth", "login", "--token-stdin", "--base-url", BASE], input=TOK["alice"], capture_output=True, text=True, env=env)
    other = subprocess.run([CLI, "me", "--base-url", OTHER], capture_output=True, text=True, env=env)
    verdict("A5a", login.returncode == 0 and other.returncode != 0 and seen["b"] == [] and "belongs to" in other.stderr,
            f"login rc={login.returncode}; `me --base-url <other host>` rc={other.returncode}, requests received by the other host={len(seen['b'])}, stderr={other.stderr.strip()[:110]!r}")
    # explicit token to a host that redirects to a third origin: the redirect must not carry the token
    env2 = {**env, "OPENETL_TOKEN": TOK["alice"]}
    subprocess.run([CLI, "me", "--base-url", OTHER], capture_output=True, text=True, env=env2)
    verdict("A5b", len(seen["b"]) == 1 and seen["c"] and all(a is None for a in seen["c"]),
            f"explicit OPENETL_TOKEN: first hop got Authorization={'yes' if seen['b'] and seen['b'][0] else 'no'} (by design), cross-origin redirect target got {seen['c']}")
    # same-host different port, stored token: refused as well
    other2 = subprocess.run([CLI, "me", "--base-url", "http://localhost:18203"], capture_output=True, text=True, env=env)
    verdict("A5c", other2.returncode != 0 and "belongs to" in other2.stderr, f"stored token to http://localhost:18203 (same port, other host name) rc={other2.returncode}")
    sb.shutdown(); sc.shutdown()
    print(f"\n{len(fails)} attack(s) succeeded: {fails}" if fails else "\nall attacks were stopped")
    return 1 if fails else 0


sys.exit(main())
