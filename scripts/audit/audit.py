#!/usr/bin/env python3
"""openetl consistency audit.  Run from anywhere:  python3 scripts/audit/audit.py [--strict] [--json OUT] [--no-db]

Extracts the normative vocabulary of every artefact (OpenAPI, SQL migrations, Go code, the live CLI, Skills, docs,
README, scripts, views.json) and cross-checks them mechanically.  Exit code 0 only if no check failed.
Python 3 standard library only (PyYAML is used for the OpenAPI property-name checks when installed).

Environment (all optional):
  AUDIT_MYSQL   mysql client command, default "mysql -h127.0.0.1 -P33307 -uoe -poe"
  AUDIT_DB      migrated scratch database used for EXPLAIN of SQL snippets and view columns, default openetl_d
                (the database is only read; if it is unreachable the DB-backed checks are SKIPPED, loudly)
  AUDIT_PROBE   base URL of a running server for route probing (e.g. http://127.0.0.1:18204)
  AUDIT_TOKEN   admin token for the probe (file path or the token itself); the probe only sends invalid bodies
"""
import concurrent.futures
import glob
import http.client
import json
import os
import re
import shlex
import subprocess
import sys
import tempfile
import urllib.parse

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", ".."))
os.chdir(ROOT)

# --------------------------------------------------------------------------- framework
class Audit:
    def __init__(self):
        self.results = []  # (status, area, name, detail)

    def add(self, status, area, name, detail=""):
        self.results.append((status, area, name, detail))

    def check(self, area, name, cond, detail=""):
        self.add("ok" if cond else "FAIL", area, name, "" if cond else detail)
        return cond

    def warn(self, area, name, cond, detail=""):
        self.add("ok" if cond else "WARN", area, name, "" if cond else detail)
        return cond

    def skip(self, area, name, why):
        self.add("SKIP", area, name, why)



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


A = Audit()
STRICT = "--strict" in sys.argv
NO_DB = "--no-db" in sys.argv


def read(path):
    with open(os.path.join(ROOT, path), encoding="utf-8") as f:
        return f.read()


def exists(path):
    return os.path.exists(os.path.join(ROOT, path))


def sh(cmd, inp=None, env=None, timeout=120):
    p = subprocess.run(cmd, input=inp, capture_output=True, text=True, env=env, timeout=timeout)
    return p.returncode, p.stdout, p.stderr


def lineno(text, pos):
    return text.count("\n", 0, pos) + 1


# --------------------------------------------------------------------------- build helpers
TMP = tempfile.mkdtemp(prefix="openetl-audit-")
BIN_CLI = os.path.join(TMP, "openetl")
BIN_GUARD = os.path.join(TMP, "guardcheck")


def build():
    for out, pkg in ((BIN_CLI, "./cmd/openetl"), (BIN_GUARD, "./scripts/audit/guardcheck")):
        rc, o, e = sh(["go", "build", "-o", out, pkg])
        if rc != 0:
            A.check("build", "go build " + pkg, False, e.strip()[:500])
            return False
        A.check("build", "go build " + pkg, True)
    return True


def cli_env():
    home = os.path.join(TMP, "home")
    os.makedirs(home, exist_ok=True)
    return {"PATH": os.environ.get("PATH", ""), "HOME": home}  # no OPENETL_* => nothing can reach a server


# --------------------------------------------------------------------------- markdown sources
GENERATED_DOCS = ["docs/cli-reference.md"]  # produced from the live CLI by this script; compared for staleness, not scanned for claims
QUOTING_DOCS = ["docs/consistency-report.md", "docs/backlog/D.md"]  # quote broken examples on purpose; their paths are still checked below
DOC_FILES = sorted(f for f in ["README.md"] + glob.glob("docs/**/*.md", recursive=True) + glob.glob("skills/*/SKILL.md")
                   if f not in GENERATED_DOCS and f not in QUOTING_DOCS)
SCRIPT_FILES = sorted(glob.glob("scripts/*.sh"))


class Block:
    def __init__(self, file, line, lang, text):
        self.file, self.line, self.lang, self.text = file, line, lang, text


def md_parse(path):
    """-> (fenced blocks, prose lines [(lineno, text)]) """
    blocks, prose = [], []
    cur = None
    for i, ln in enumerate(read(path).split("\n"), 1):
        m = re.match(r"^\s*```(\w*)\s*$", ln)
        if m and cur is None:
            cur = Block(path, i + 1, m.group(1).lower(), [])
            continue
        if re.match(r"^\s*```\s*$", ln) and cur is not None:
            cur.text = "\n".join(cur.text)
            blocks.append(cur)
            cur = None
            continue
        if cur is not None:
            cur.text.append(ln)
        else:
            prose.append((i, ln))
    return blocks, prose


MD = {f: md_parse(f) for f in DOC_FILES}


def inline_codes(prose):
    for i, ln in prose:
        for m in re.finditer(r"`([^`\n]+)`", ln):
            yield i, m.group(1)


# --------------------------------------------------------------------------- Go sources
GO_FILES = [f for f in glob.glob("**/*.go", recursive=True) if not f.startswith("scripts/audit/")]
GO_PROD = [f for f in GO_FILES if not f.endswith("_test.go")]
GO_TEST = [f for f in GO_FILES if f.endswith("_test.go")]


def strip_go_comments(src):
    out, i, n = [], 0, len(src)
    while i < n:
        c = src[i]
        if c == "/" and src.startswith("//", i):
            j = src.find("\n", i)
            i = n if j < 0 else j
        elif c == "/" and src.startswith("/*", i):
            j = src.find("*/", i + 2)
            i = n if j < 0 else j + 2
        elif c == '"':
            j = i + 1
            while j < n and src[j] != '"':
                j += 2 if src[j] == "\\" else 1
            out.append(src[i:j + 1]); i = j + 1
        elif c == "`":
            j = src.find("`", i + 1)
            j = n - 1 if j < 0 else j
            out.append(src[i:j + 1]); i = j + 1
        elif c == "'":
            j = i + 1
            while j < n and src[j] != "'":
                j += 2 if src[j] == "\\" else 1
            out.append(src[i:j + 1]); i = j + 1
        else:
            out.append(c); i += 1
    return "".join(out)


GO_SRC = {f: strip_go_comments(read(f)) for f in GO_FILES}

# --------------------------------------------------------------------------- vocabulary extraction
def load_openapi():
    text = read("api/openapi.yaml")
    spec = None
    try:
        import yaml  # optional
        spec = yaml.safe_load(text)
    except Exception:
        spec = None
    return text, spec


OAS_TEXT, OAS = load_openapi()


def walk(o, path=()):
    yield path, o
    if isinstance(o, dict):
        for k, v in o.items():
            yield from walk(v, path + (str(k),))
    elif isinstance(o, list):
        for i, v in enumerate(o):
            yield from walk(v, path + (str(i),))


def oas_enums():
    """{path-string: [values]} for every enum in the OpenAPI document."""
    out = {}
    if OAS:
        for p, o in walk(OAS):
            if isinstance(o, dict) and isinstance(o.get("enum"), list):
                out["/".join(p)] = [str(x) for x in o["enum"]]
    else:  # regex fallback: inline flow lists only
        for m in re.finditer(r"enum:\s*\[([^\]]*)\]", OAS_TEXT):
            out["line%d" % lineno(OAS_TEXT, m.start())] = [x.strip() for x in m.group(1).split(",")]
    return out


def migrations():
    files = sorted(glob.glob("db/migrations/*.sql"))
    return {f: read(f) for f in files}


MIG = migrations()
MIG_ALL = "\n".join(MIG.values())


def sql_rows(block):
    """parse the tuples of an INSERT ... VALUES block into lists of python values"""
    rows = []
    for m in re.finditer(r"\(((?:'(?:[^']|'')*'|NULL|\d+|\s|,)+)\)", block):
        vals = []
        for t in re.finditer(r"'((?:[^']|'')*)'|NULL|\d+", m.group(1)):
            if t.group(0) == "NULL":
                vals.append(None)
            elif t.group(1) is not None:
                vals.append(t.group(1).replace("''", "'"))
            else:
                vals.append(int(t.group(0)))
        rows.append(vals)
    return rows


def seed_block(table):
    m = re.search(r"INSERT\s+(?:IGNORE\s+)?INTO\s+%s\s*\([^)]*\)\s*VALUES(.*?);" % table, MIG_ALL, re.S | re.I)
    return sql_rows(m.group(1)) if m else []


STAGES = seed_block("stage_def")  # key, seq, zh, en
REASONS = seed_block("reason")  # key, kind, owner_side, zh, en, note_required
RULES = seed_block("rule")  # k_pattern, to_value, side, reason_kind
STAGE_KEYS = [r[0] for r in sorted(STAGES, key=lambda r: r[1])]
REASON_KEYS = {r[0]: r for r in REASONS}


def sql_enum(table, col):
    m = re.search(r"CREATE TABLE IF NOT EXISTS %s\s*\((.*?)\n\)\s*ENGINE" % table, MIG_ALL, re.S)
    if not m:
        return []
    e = re.search(r"\b%s\s+ENUM\(([^)]*)\)" % col, m.group(1))
    return re.findall(r"'([^']+)'", e.group(1)) if e else []


def go_reasons():
    """{reason: set(status)} from NewError(<status>, "REASON", ...), row errors and the invalid("REASON", ...) helpers"""
    out = {}
    for f in GO_PROD:
        for m in re.finditer(r"\b(?:core\.)?(Status[A-Z]\w*)\s*,\s*\"([A-Z][A-Z0-9_]+)\"", GO_SRC[f]):
            out.setdefault(m.group(2), set()).add(m.group(1))
        for m in re.finditer(r"\b\w*[Ee]rr\w*\(\s*\w+\s*,\s*\"([A-Z][A-Z0-9_]+)\"", GO_SRC[f]):
            out.setdefault(m.group(1), set()).add("rowError")
        for m in re.finditer(r"\b(?:invalid|invalidArg)\(\s*\"([A-Z][A-Z0-9_]+)\"", GO_SRC[f]):  # server/store helpers: INVALID_ARGUMENT + reason
            out.setdefault(m.group(1), set()).add("StatusInvalidArgument")
    return out


def go_statuses():
    src = GO_SRC["internal/core/errors.go"]
    return dict(re.findall(r"(Status\w+)\s*=\s*\"([A-Z_]+)\"", src))


GO_REASONS = go_reasons()
GO_STATUS = go_statuses()  # const name -> value


def go_http_map():
    src = GO_SRC["internal/server/server.go"]
    m = re.search(r"func [^\n]*\(([^)]*)\)[^\n]*int\s*\{(.*?)\n\}", src[src.find("StatusBadRequest") - 600:], re.S)
    out = {}
    body = src[max(0, src.find("StatusBadRequest") - 800): src.find("StatusInternalServerError") + 60]
    for mm in re.finditer(r"case\s+((?:core\.)?Status\w+(?:\s*,\s*(?:core\.)?Status\w+)*)\s*:\s*return\s+http\.(Status\w+)", body):
        for c in re.findall(r"Status\w+", mm.group(1)):
            out[GO_STATUS.get(c, c)] = mm.group(2)
    return out


HTTP_CODE = {"StatusBadRequest": 400, "StatusUnauthorized": 401, "StatusForbidden": 403, "StatusNotFound": 404,
             "StatusConflict": 409, "StatusTooManyRequests": 429, "StatusNotImplemented": 501,
             "StatusInternalServerError": 500, "StatusPreconditionFailed": 412}

# --------------------------------------------------------------------------- CLI tree (recursive --help)
class Node:
    def __init__(self, path):
        self.path = path
        self.children = {}
        self.flags = {}  # name -> (short, type or "")
        self.gflags = {}
        self.usage = ""
        self.help = ""

    def all_flags(self):
        d = dict(self.gflags)
        d.update(self.flags)
        d["help"] = ("h", "")
        return d


def parse_help(path):
    rc, o, e = sh([BIN_CLI] + path + ["--help"], env=cli_env())
    n = Node(path)
    n.help = o
    sect = None
    for ln in o.split("\n"):
        if re.match(r"^[A-Z][A-Za-z ]+:$", ln):
            sect = ln[:-1]
            continue
        if not ln.strip():
            continue
        if sect == "Usage" and not n.usage:
            n.usage = ln.strip()
        elif sect == "Available Commands":
            m = re.match(r"^\s{2}([\w-]+)\s+(.*)$", ln)
            if m and m.group(1) not in ("help", "completion"):
                n.children[m.group(1)] = m.group(2)
        elif sect in ("Flags", "Global Flags"):
            m = re.match(r"^\s+(?:-(\w), )?--([\w-]+)(?: (\w+))?\s{2,}.*$", ln) or re.match(r"^\s+(?:-(\w), )?--([\w-]+)(?: (\w+))?$", ln)
            if m:
                (n.flags if sect == "Flags" else n.gflags)[m.group(2)] = (m.group(1) or "", m.group(3) or "")
    n.usages = []
    m = re.search(r"Usage:\n((?:  .*\n)+)", o)
    if m:
        n.usages = [x.strip() for x in m.group(1).split("\n") if x.strip()]
    return n


def build_tree():
    root = parse_help([])
    todo = [root]
    nodes = {(): root}
    while todo:
        n = todo.pop()
        for c in sorted(n.children):
            ch = parse_help(n.path + [c])
            nodes[tuple(ch.path)] = ch
            n.children[c] = ch
            todo.append(ch)
    return root, nodes


def usage_tokens(node):
    line = next((u for u in node.usages if not u.endswith("[command]")), node.usage)
    toks = re.findall(r'\[[^\]]*\]\.{0,3}|"[^"]*"|\S+', line)
    return toks[1 + len(node.path):]


def positional_spec(node):
    """(min, max) positionals from the Usage line of a leaf: 'openetl a b X Y [--z N] [flags]'"""
    toks = usage_tokens(node)
    mn = mx = 0
    i = 0
    while i < len(toks):
        t = toks[i]
        if t == "[flags]":
            break
        if t.startswith("--"):
            typ = node.all_flags().get(t[2:].split("=")[0], ("", ""))[1]
            if typ and i + 1 < len(toks) and not toks[i + 1].startswith(("-", "[")):
                i += 1
        elif t.startswith("["):
            if not t.startswith("[-"):
                mx += 99 if t.endswith("...") else 1
        else:
            mn += 1
            mx += 99 if t.endswith("...") else 1
        i += 1
    return mn, mx


def required_flags(node):
    toks = usage_tokens(node)
    out = set()
    for i, t in enumerate(toks):
        if t.startswith("--") and "[" not in t:
            out.add(t[2:])
    return out


# --------------------------------------------------------------------------- command line extraction
def shell_tokens(s):
    lex = shlex.shlex(s, posix=True, punctuation_chars="|&;()")
    lex.whitespace_split = True
    lex.commenters = "#"
    toks = []
    for t in lex:
        if t and all(ch in "|&;()" for ch in t):
            break
        toks.append(t)
    return toks


def join_continuations(text):
    return re.sub(r"\\\n\s*", " ", text)


def segments(line):
    """split a shell line into simple commands (quote aware); drop leading VAR=value words"""
    parts, cur, q, i = [], [], None, 0
    while i < len(line):
        c = line[i]
        if q:
            cur.append(c)
            if c == "\\" and q == '"' and i + 1 < len(line):
                cur.append(line[i + 1]); i += 1
            elif c == q:
                q = None
        elif c in "\"'":
            q = c; cur.append(c)
        elif c in "|;&`(" or (c == "$" and line[i:i + 2] == "$("):
            parts.append("".join(cur)); cur = []
            if c == "$":
                i += 1
        else:
            cur.append(c)
        i += 1
    parts.append("".join(cur))
    out = []
    for seg in parts:
        seg = seg.strip()
        while re.match(r"^[A-Za-z_]\w*=(?:\"[^\"]*\"|'[^']*'|\S*)\s+", seg):
            seg = re.sub(r"^[A-Za-z_]\w*=(?:\"[^\"]*\"|'[^']*'|\S*)\s+", "", seg, count=1)
        seg = re.sub(r"^(time|sudo|exec|command)\s+", "", seg)
        out.append(seg)
    return out


def find_cmd(line, name="openetl"):
    """the argument string of the first simple command in `line` whose program is `name`, else None"""
    for seg in segments(line):
        m = re.match(r"^%s(?![\w./-])\s*(.*)$" % re.escape(name), seg)
        if m:
            return m.group(1)
    return None
SCRIPT_RE = re.compile(r"(?:oe_as\s+\"?\$\w+\"?|as_(?:dev|edt|cus)|\"\$OE\"|\$OE)\s+(.*)$")


def extract_commands():
    """-> list of (file, line, tokens) from fenced shell blocks, inline code and scripts"""
    out = []

    def add(f, ln, rest, partial=False):
        rest = rest.strip()
        if not rest or rest.startswith("...") or "..." in rest.split()[0]:
            return
        try:
            toks = shell_tokens(rest)
        except ValueError:
            A.check("cmd-parse", "%s:%d tokenises" % (f, ln), False, rest)
            return
        toks = [t for t in toks if not (re.match(r"^(\d?>{1,2}|&>)", t) or re.match(r"^\d?<", t) and not t.endswith(">"))]
        if not toks or toks[0].startswith(("$", ".", "<", "*")) or toks[0] == "--":
            return  # shell plumbing ($@, ./cmd/openetl) or an output description (`openetl <version>`)
        out.append((f, ln, toks, partial))

    for f in DOC_FILES:
        blocks, prose = MD[f]
        for b in blocks:
            if b.lang not in ("bash", "sh", "shell", "console", ""):
                continue
            for k, ln in enumerate(join_continuations(b.text).split("\n")):
                s = ln.strip()
                if s.startswith("#"):
                    continue
                rest = find_cmd(s)
                if rest is not None:
                    add(f, b.line + k, rest)
        for i, code in inline_codes(prose):
            m = re.match(r"^openetl(?![\w-])\s+(.*)$", code.strip())
            if m:
                add(f, i, m.group(1), partial=True)  # prose mention: arguments may be elided
    for f in SCRIPT_FILES:
        for i, ln in enumerate(join_continuations(read(f)).split("\n"), 1):
            s = ln.strip()
            if s.startswith("#") or "openetl-server" in s.split("oe_as")[0] and not SCRIPT_RE.search(s):
                continue
            m = SCRIPT_RE.search(s) or None
            if m:
                add(f, i, m.group(1), partial="expect_fail" in s)
    # the command table of docs/cli-import.md lists commands without the binary name
    for i, ln in MD["docs/cli-import.md"][1]:
        m = re.match(r"^\|\s*`((?:import)\s[^`]*?)`\s*\|", ln)
        if m:
            cmd = m.group(1).replace("\\|", "|")
            cmd = re.sub(r"\[[^\]]*\]", lambda mm: " ".join(re.findall(r"--[\w-]+", mm.group(0))), cmd)
            cmd = re.sub(r"\b[A-Z]{2,}\b", "X", cmd)
            cmd = cmd.replace("=N", "").replace("...", "")
            add("docs/cli-import.md", i, cmd, partial=True)
    return out


def check_command(nodes, f, ln, toks, root, partial=False):
    """lexical check against the tree; returns (ok, message, resolved node)"""
    node = root
    i = 0
    gv = {k: v for k, v in root.gflags.items()}
    # leading global flags (only before the first sub-command), then descend through the command path
    while i < len(toks):
        t = toks[i]
        if t.startswith("-"):
            if node is not root:
                break
            name = t.lstrip("-").split("=")[0]
            fl = {**gv, **root.flags, "help": ("h", ""), "version": ("v", "")}
            short = {v[0]: k for k, v in fl.items() if v[0]}
            if t.startswith("--"):
                if name not in fl:
                    return False, "unknown flag --%s" % name, node
                typ = fl[name][1]
            else:
                if name not in short:
                    return False, "unknown flag -%s" % name, node
                typ = fl[short[name]][1]
            i += 1 + (1 if typ and "=" not in t else 0)
        elif t in node.children and isinstance(node.children[t], Node):
            node = node.children[t]
            i += 1
        else:
            break
    flags = node.all_flags()
    short = {v[0]: k for k, v in flags.items() if v[0]}
    pos = 0
    seen = set()
    rest = toks[i:]
    j = 0
    if node.children and rest and not rest[0].startswith("-"):
        return False, "unknown subcommand %r of %s" % (rest[0], " ".join(["openetl"] + node.path)), node
    while j < len(rest):
        t = rest[j]
        if t.startswith("--") and len(t) > 2:
            name = t[2:].split("=")[0]
            if name not in flags and name != "version":
                return False, "unknown flag --%s for %s" % (name, " ".join(["openetl"] + node.path)), node
            seen.add(name)
            typ = flags.get(name, ("", ""))[1]
            j += 1 + (1 if typ and "=" not in t else 0)
        elif t.startswith("-") and len(t) > 1 and not t[1].isdigit():
            nm = t[1]
            if nm not in short and nm != "v":
                return False, "unknown flag -%s for %s" % (nm, " ".join(["openetl"] + node.path)), node
            typ = flags.get(short.get(nm, ""), ("", ""))[1]
            j += 1 + (1 if typ and len(t) == 2 else 0)
        else:
            pos += 1
            j += 1
    if not node.children:
        mn, mx = positional_spec(node)
        if not partial and "help" not in seen and not (pos >= mn and pos <= mx):
            return False, "%s takes %d..%d positional args, got %d" % (" ".join(["openetl"] + node.path), mn, mx, pos), node
    return True, "", node


# --------------------------------------------------------------------------- SQL extraction
# docs that discuss internal SQL (schema, performance, research): their snippets are not query-endpoint input
INTERNAL_SQL_DOCS = ("docs/adr/", "docs/performance.md", "docs/references.md", "docs/operations.md", "docs/security.md",
                     "docs/threat-model.md", "docs/slo.md", "docs/invariants.md", "docs/backlog/", "docs/consistency-report.md",
                     "docs/contract-checklist.md")
# docs about the outside world: upper-case tokens there are not openetl vocabulary
EXTERNAL_VOCAB_DOCS = ("docs/adr/", "docs/references.md", "docs/performance.md", "docs/backlog/", "docs/consistency-report.md",
                       "docs/operations.md", "docs/slo.md", "docs/threat-model.md", "docs/invariants.md", "docs/security.md",
                       "docs/contract-checklist.md")


def extract_sql():
    """-> [(id, file, line, sql)]"""
    out = []

    def add(f, ln, s):
        s = s.strip().rstrip(";")
        if not re.match(r"(?i)^select\b", s) or not re.search(r"(?i)\bfrom\b", s) or "..." in s or "<" in s and ">" in s and "<=" not in s and "<>" not in s and " < " not in s:
            return
        if re.search(r"\$[A-Za-z{(]", s):
            return
        out.append(("%s:%d" % (f, ln), f, ln, s))

    for f in DOC_FILES:
        if f.startswith(INTERNAL_SQL_DOCS):
            continue
        blocks, prose = MD[f]
        for b in blocks:
            if b.lang == "sql":
                for st in re.split(r";\s*\n|;\s*$", b.text):
                    add(f, b.line, " ".join(st.split()))
            elif b.lang in ("bash", "sh", "shell", ""):
                for k, ln in enumerate(join_continuations(b.text).split("\n")):
                    for m in re.finditer(r"\"((?:SELECT|select)\b(?:\\.|[^\"\\])*)\"|'((?:SELECT|select)\b[^']*)'", ln):
                        s = m.group(1) if m.group(1) is not None else m.group(2)
                        add(f, b.line + k, s.replace('\\"', '"').replace("\\$", "$"))
        for i, code in inline_codes(prose):
            if re.match(r"(?i)^select\b", code):
                add(f, i, code)
            m = re.match(r"^openetl\s+query\s+\"(.*)\"", code)
            if m:
                add(f, i, m.group(1))
    for f in SCRIPT_FILES:
        txt = join_continuations(read(f))
        for i, ln in enumerate(txt.split("\n"), 1):
            if not re.search(r"\bquery\b|(?:^|[\s(])q1?\s", ln) or "expect_fail" in ln:
                continue
            for m in re.finditer(r"\"((?:SELECT|select)\b(?:\\.|[^\"\\])*)\"|'((?:SELECT|select)\b[^']*)'", ln):
                s = m.group(1) if m.group(1) is not None else m.group(2)
                add(f, i, s.replace('\\"', '"').replace("\\$", "$").replace("\\'", "'"))
    if OAS:
        for p, o in walk(OAS):
            if isinstance(o, str) and re.match(r"(?i)^select\b", o):
                out.append(("api/openapi.yaml:" + "/".join(p), "api/openapi.yaml", 0, o))
    # de-duplicate by sql text but keep every location for messages
    return out


def mysql_cmd():
    return shlex.split(os.environ.get("AUDIT_MYSQL", "mysql -h127.0.0.1 -P33307 -uoe -poe"))


def mysql(db, sql):
    return sh(mysql_cmd() + ["--default-character-set=utf8mb4", "-N", "-B", db], inp="SET NAMES utf8mb4 COLLATE utf8mb4_0900_ai_ci;\n" + sql)


# =========================================================================== CHECKS
def section_openapi():
    area = "openapi"
    ops = re.findall(r"operationId:\s*(\w+)", OAS_TEXT)
    A.check(area, "operationIds unique", len(ops) == len(set(ops)), "duplicates: %s" % sorted({o for o in ops if ops.count(o) > 1}))
    for o in ops:
        A.check(area, "operationId lowerCamelCase: " + o, re.match(r"^[a-z][a-zA-Z0-9]*$", o) is not None)
    enums = oas_enums()
    for p, vals in enums.items():
        if all(re.match(r"^[A-Z][A-Z0-9_]*$", v) for v in vals):
            unspec = [v for v in vals if v.endswith("_UNSPECIFIED")]
            is_status = "INTERNAL" in vals and "ABORTED" in vals
            A.check(area, "enum %s UPPER_SNAKE with *_UNSPECIFIED first" % p, is_status or (len(unspec) == 1 and vals[0] == unspec[0]),
                    "values %s" % vals)
        else:
            A.check(area, "enum %s is UPPER_SNAKE (or the ErrorInfo @type)" % p, vals == ["ErrorInfo"] or p.endswith("@type/enum"), "values %s" % vals)
    if OAS:
        # field names
        for p, o in walk(OAS):
            if len(p) >= 2 and p[-1] == "properties" and isinstance(o, dict) and p[0] == "components":
                for name in o:
                    A.check(area, "snake_case field %s (%s)" % (name, "/".join(p[2:-1])),
                            name == "@type" or re.match(r"^[a-z][a-z0-9]*(_[a-z0-9]+)*$", name) is not None)
        # query / path parameter names
        for p, o in walk(OAS):
            if isinstance(o, dict) and o.get("in") in ("query", "header") and "name" in o:
                A.check(area, "parameter name snake_case: " + o["name"], re.match(r"^[a-z][a-z0-9]*(_[a-z0-9]+)*$", o["name"]) is not None)
        # collections: AIP lowerCamel plural, custom verbs lowerCamel
        colls, verbs = set(), set()
        for path in OAS["paths"]:
            for seg in path.strip("/").split("/"):
                name, _, verb = seg.partition(":")
                if name and not name.startswith("{"):
                    colls.add(name)
                if verb:
                    verbs.add(verb)
        for c in sorted(colls - {"v1"}):
            A.check(area, "collection id lowerCamel: " + c, re.match(r"^[a-z][a-zA-Z0-9]*$", c) is not None)
        for v in sorted(verbs):
            A.check(area, "custom verb lowerCamel: " + v, re.match(r"^[a-z][a-zA-Z0-9]*$", v) is not None)
        # naming spec: verbs and collections it names must exist in the contract
        spec = read("docs/m0-naming-spec.md")
        for v in sorted(set(re.findall(r"[:/](?:\{[^}]+\})?:([a-zA-Z]+)", spec)) | set(re.findall(r"\s:(advanceStage|regressStage|setScope|resolve|reopen|undelete|diff|apply|rollback|initUpload|download|query)\b", spec))):
            A.check(area, "naming spec verb :%s exists in OpenAPI" % v, v in verbs or v == "assign" and "assign" in verbs or any(vv.startswith(v) for vv in verbs), "OpenAPI has %s" % sorted(verbs))
        for c in re.findall(r"/(importJobs|workflows|tasks|comments|attachments|events|members|tokens|config)\b", spec):
            A.check(area, "naming spec collection %s exists in OpenAPI" % c, c in colls)
        # resource-name schema examples
        for m in re.finditer(r"example:\s*(projects/[\w/-]+|accounts/[\w/-]+)", OAS_TEXT):
            ex = m.group(1)
            A.check(area, "resource name example is AIP-shaped: " + ex,
                    re.match(r"^(projects/[a-z0-9-]+(/(workflows|tasks|comments|attachments|events|importJobs|members|config)(/[\w-]+)*)?|accounts/[a-z0-9-]+(/tokens/\d+)?)$", ex) is not None)
        # required properties exist
        for p, o in walk(OAS):
            if isinstance(o, dict) and isinstance(o.get("required"), list) and isinstance(o.get("properties"), dict):
                for r in o["required"]:
                    A.check(area, "required %s exists in %s" % (r, "/".join(p[-2:])), r in o["properties"])
        # refs resolve
        for p, o in walk(OAS):
            if isinstance(o, dict) and isinstance(o.get("$ref"), str) and o["$ref"].startswith("#/"):
                cur = OAS
                ok = True
                for part in o["$ref"][2:].split("/"):
                    if isinstance(cur, dict) and part in cur:
                        cur = cur[part]
                    else:
                        ok = False
                        break
                A.check(area, "$ref resolves: " + o["$ref"], ok)
    else:
        A.skip(area, "property-name / ref / required checks", "PyYAML not installed (stdlib-only mode)")
    # Error.status enum == Go statuses
    est = None
    for p, vals in enums.items():
        if "INTERNAL" in vals and "ABORTED" in vals:
            est = set(vals)
    A.check(area, "Error.status enum equals the Go Status* constants", est == set(GO_STATUS.values()),
            "openapi-only %s, go-only %s" % (sorted((est or set()) - set(GO_STATUS.values())), sorted(set(GO_STATUS.values()) - (est or set()))))
    # error status/code pairs in examples
    if OAS:
        for p, o in walk(OAS):
            if isinstance(o, dict) and "code" in o and "status" in o and isinstance(o.get("status"), str):
                exp = {"ABORTED": 409, "NOT_FOUND": 404, "INVALID_ARGUMENT": 400, "UNAUTHENTICATED": 401, "PERMISSION_DENIED": 403,
                       "ALREADY_EXISTS": 409, "FAILED_PRECONDITION": 400, "RESOURCE_EXHAUSTED": 429, "UNIMPLEMENTED": 501, "INTERNAL": 500}
                A.check(area, "example status/code pair %s/%s" % (o["status"], o["code"]), exp.get(o["status"]) == o["code"] or o["status"] == "FAILED_PRECONDITION" and o["code"] in (400, 412, 409))
    # reasons mentioned in the contract exist in Go
    mentioned = set(re.findall(r"\b([A-Z]{3,}(?:_[A-Z]+)+)\b", re.sub(r"enum:\s*\[[^\]]*\]", "", OAS_TEXT)))
    enumvals = {v for vals in enums.values() for v in vals}
    for r in sorted(mentioned - enumvals):
        if r.startswith(("OPENETL_",)) or r in ("OUTPUT_ONLY", "TASK_CREATED", "TASK_UPDATED", "TASK_SKIPPED"):
            continue
        A.check(area, "reason named in OpenAPI exists in code: " + r, r in GO_REASONS, "not raised anywhere in Go")
    # types.gen.go carries every enum value
    gen = read("internal/api/types.gen.go")
    for p, vals in enums.items():
        for v in vals:
            if re.match(r"^[A-Z][A-Z0-9_]+$", v) and v not in ("INTERNAL",):
                A.check(area, "types.gen.go has enum value " + v, '"%s"' % v in gen, "regenerate internal/api (go generate)")


def section_routes():
    area = "routes"
    if not OAS:
        A.skip(area, "route coverage", "PyYAML not installed")
        return
    lits = set()
    for f in GO_PROD:
        if f.startswith("internal/server/"):
            lits |= set(re.findall(r"\"([A-Za-z]+)\"", GO_SRC[f]))
    for path, item in OAS["paths"].items():
        for seg in path.strip("/").split("/"):
            if seg.startswith("{") or seg == "v1":
                continue
            name, _, verb = seg.partition(":")
            for w in (name, verb):
                if w:
                    A.check(area, "server routes literal %r (from %s)" % (w, path), w in lits)
    for f in GO_PROD:
        if f.startswith("internal/server/"):
            for m in re.finditer(r"case\s+\"(\w+)\"\s*:", GO_SRC[f]):
                pass
    # verbs the server dispatches vs verbs in the contract
    verbs_oas = {seg.partition(":")[2] for p in OAS["paths"] for seg in p.split("/") if ":" in seg}
    verbs_go = set()
    for f in GO_PROD:
        if f.startswith("internal/server/"):
            for m in re.finditer(r"q\.verb\s*==\s*\"([a-z][A-Za-z]+)\"", GO_SRC[f]):
                verbs_go.add(m.group(1))
            for m in re.finditer(r"switch\s+q\.verb\s*\{", GO_SRC[f]):
                block = GO_SRC[f][m.end(): m.end() + 1200]
                depth, end = 1, 0
                for k, ch in enumerate(block):
                    depth += ch == "{"
                    depth -= ch == "}"
                    if depth == 0:
                        end = k
                        break
                for mm in re.finditer(r"case\s+\"([a-z][A-Za-z]+)\"", block[:end]):
                    verbs_go.add(mm.group(1))
    base = {"workflows", "tasks", "comments", "events", "attachments", "config", "members", "importJobs", "projects", "accounts", "tokens", "me"}
    for v in sorted(verbs_go | verbs_oas):
        A.check(area, "custom verb :%s is both handled by the server and in the OpenAPI" % v, v in lits and v in verbs_oas,
                "server literals lack it, or OpenAPI lacks it (server switch has %s; OpenAPI has %s)" % (sorted(verbs_go), sorted(verbs_oas)))
    # live probe
    base_url = os.environ.get("AUDIT_PROBE")
    tok = os.environ.get("AUDIT_TOKEN", "")
    if not base_url or not tok:
        A.skip(area, "live route probe", "set AUDIT_PROBE and AUDIT_TOKEN (server on a scratch database)")
        return
    if os.path.exists(tok):
        tok = open(tok).read().strip()
    proj = os.environ.get("AUDIT_PROJECT", "acme-dw")
    for path, item in OAS["paths"].items():
        for method in ("get", "post", "patch", "put", "delete"):
            if method not in item:
                continue
            url = path.replace("{project}", proj).replace("{workflow}", "wf-999999").replace("{task}", "tk-999999") \
                .replace("{comment}", "cm-999999").replace("{attachment}", "at-999999").replace("{event}", "ev-999999") \
                .replace("{importJob}", "999999").replace("{account}", "nobody-probe").replace("{token}", "999999") \
                .replace("{member}", "nobody-probe")
            body = None if method in ("get", "delete") else b'{"__probe":true}'
            try:
                _, text = http_call(method.upper(), base_url + url, tok, body)
            except Exception as e:
                A.check(area, "probe %s %s" % (method.upper(), path), False, str(e))
                continue
            A.check(area, "server routes %s %s (%s)" % (method.upper(), path, item[method].get("operationId")), "route not found" not in text, text[:160])


def section_migrations():
    area = "sql"
    names = sorted(MIG)
    for i, f in enumerate(names, 1):
        num = re.match(r"db/migrations/(\d{4})_", f)
        A.check(area, "migration numbering contiguous: " + f, num and int(num.group(1)) == i, "expected %04d" % i)
        m = re.search(r"--\s*openetl\s+(?:M0\s+migration\s+)?(\d{4})", MIG[f])
        A.check(area, "header comment number matches filename: " + f, m is not None and num is not None and m.group(1) == num.group(1),
                "header says %s, filename says %s" % (m.group(1) if m else None, num.group(1) if num else None))
        A.check(area, "no table is dropped or renamed (append-only history): " + f, not re.search(r"\bDROP\s+TABLE\b|\bRENAME\s+TABLE\b", MIG[f], re.I))
    A.check(area, "seed: stage keys parsed", len(STAGES) == 6, "got %s" % STAGE_KEYS)
    A.check(area, "seed: stage seq contiguous 1..N", sorted(r[1] for r in STAGES) == list(range(1, len(STAGES) + 1)))
    A.check(area, "seed: reasons parsed", len(REASONS) > 20, "got %d" % len(REASONS))
    kinds = set(sql_enum("reason", "kind"))
    A.check(area, "seed: reason kinds within the ENUM", {r[1] for r in REASONS} <= kinds, str({r[1] for r in REASONS} - kinds))
    owners = set(sql_enum("reason", "owner_side"))
    A.check(area, "seed: reason owner_side within the ENUM", {r[2] for r in REASONS} <= owners)
    for k in ("reject", "discard", "not_applicable", "gate_override"):
        A.check(area, "seed: at least one reason of kind " + k, any(r[1] == k for r in REASONS))
    A.check(area, "seed: reason 'merged' is a discard reason", REASON_KEYS.get("merged", [0, ""])[1] == "discard")
    A.check(area, "seed: reason keys snake_case", all(re.match(r"^[a-z][a-z0-9_]*$", r[0]) and len(r[0]) <= 48 for r in REASONS))
    A.check(area, "seed: rules parsed", len(RULES) >= 12, "got %d" % len(RULES))
    sides = set(sql_enum("rule", "side"))
    for r in RULES:
        pat, to, side, rk = r
        A.check(area, "rule %s -> %s [%s] side in ENUM" % (pat, to, side), side in sides)
        A.check(area, "rule %s -> %s reason_kind valid" % (pat, to), rk is None or rk in kinds)
        m = re.match(r"^stage\.(\w+)$", pat)
        if m:
            A.check(area, "rule pattern %s names a seeded stage" % pat, m.group(1) in STAGE_KEYS)
    for s in STAGE_KEYS:
        A.check(area, "a 'pass' rule exists for stage " + s, any(r[0] == "stage." + s and r[1] == "pass" for r in RULES))
        A.check(area, "a reject rule (-> open) exists for stage " + s, any(r[0] == "stage." + s and r[1] == "open" and r[3] == "reject" for r in RULES))
    A.check(area, "rule: accept is passed by the customer side", any(r[0] == "stage.accept" and r[1] == "pass" and r[2] == "customer" for r in RULES))
    # public views
    pub = re.findall(r"CREATE OR REPLACE VIEW (\w+)\s+AS\s+SELECT \* FROM (v_\w+)", MIG_ALL)
    pub_names = sorted({a for a, b in pub})
    gov = sorted(re.findall(r"\"(\w+)\":\s*true", re.search(r"var PublicViews = map\[string\]bool\{(.*?)\n\}", GO_SRC["internal/store/sqlguard.go"], re.S).group(1)))
    A.check(area, "public views: migrations == Go PublicViews", pub_names == gov, "sql %s go %s" % (pub_names, gov))
    try:
        vj = json.loads(read("internal/cli/views.json"))
    except Exception as e:
        A.check(area, "views.json parses", False, str(e))
        return
    vnames = sorted(v["name"] for v in vj)
    A.check(area, "public views: views.json == migrations", vnames == pub_names, "json %s sql %s" % (vnames, pub_names))
    for v in vj:
        cols = [c["name"] for c in v["columns"]]
        A.check(area, "views.json %s: unique column names and types set" % v["name"], len(cols) == len(set(cols)) and all(c.get("type") for c in v["columns"]))
        A.check(area, "views.json %s: has description" % v["name"], bool(v.get("description")))
    notes = {(v["name"], c["name"]): c.get("note", "") for v in vj for c in v["columns"]}
    for (vw, col), note in sorted(notes.items()):
        if re.match(r"^[a-z_]+(\|[a-z_]+)+$", note):
            vals = set(note.split("|"))
            known = {"scope": {"in", "out"}, "health": {"ok", "at_risk", "blocked"}, "kind": {"note", "question", "issue", "risk"},
                     "status": {"open", "pass", "na"}, "type": {"workflow", "task", "comment", "attachment"}}.get(col)
            A.check(area, "views.json note of %s.%s lists the real values" % (vw, col), known is not None and vals == known, "note %r, expected %s" % (note, known))
    A.check(area, "health CASE values in the view are ok/at_risk/blocked", set(re.findall(r"THEN '(blocked|at_risk)'", MIG_ALL)) == {"blocked", "at_risk"} and "ELSE 'ok'" in MIG_ALL)
    # live view columns
    if NO_DB:
        A.skip(area, "views.json columns vs live database", "--no-db")
        return
    db = os.environ.get("AUDIT_DB", "openetl_d")
    rc, o, e = mysql(db, "SELECT table_name, GROUP_CONCAT(column_name ORDER BY ordinal_position) FROM information_schema.columns WHERE table_schema=DATABASE() GROUP BY table_name;")
    if rc != 0 or not o.strip():
        A.skip(area, "views.json columns vs live database", "database %s unreachable: %s" % (db, e.strip()[:120]))
        return
    live = dict(l.split("\t") for l in o.strip().split("\n"))
    for v in vj:
        want = ",".join(c["name"] for c in v["columns"])
        A.check(area, "views.json columns == live view %s" % v["name"], live.get(v["name"]) == want, "json %s live %s" % (want, live.get(v["name"])))
    # the CLI embeds the same file
    rc, o, e = sh([BIN_CLI, "views"], env=cli_env())
    try:
        cj = json.loads(o)
        A.check(area, "`openetl views` prints views.json", cj.get("views") == vj)
    except Exception as ex:
        A.check(area, "`openetl views` prints JSON", False, str(ex))


def enum_set(vals):
    return {v for v in vals if not v.endswith("_UNSPECIFIED")}


def section_enums():
    area = "enums"
    en = oas_enums()
    byname = {}
    if OAS:
        sch = OAS["components"]["schemas"]

        def ev(*path):
            cur = sch
            for p in path:
                cur = cur[p]
            return enum_set(cur["enum"])
        pairs = [
            ("Side", ev("Side"), {x.upper() for x in sql_enum("event", "side")}),
            ("MemberSide", ev("MemberSide"), {x.upper() for x in sql_enum("member", "side")}),
            ("Role", ev("Role"), {x.upper() for x in sql_enum("member", "role")}),
            ("AccountKind", ev("AccountKind"), {x.upper() for x in sql_enum("account", "kind")}),
            ("ImportJob.state", ev("ImportJob", "properties", "state"), {x.upper() for x in sql_enum("import_batch", "state")}),
            ("Config.reason.kind", ev("Config", "properties", "reasons", "items", "properties", "kind"), {x.upper() for x in sql_enum("reason", "kind")}),
            ("Config.reason.owner_side", ev("Config", "properties", "reasons", "items", "properties", "owner_side"), {x.upper() for x in sql_enum("reason", "owner_side")}),
            ("Config.rule.side", ev("Config", "properties", "rules", "items", "properties", "side"), {x.upper() for x in sql_enum("rule", "side")}),
            ("Config.rule.reason_kind", ev("Config", "properties", "rules", "items", "properties", "reason_kind"), {x.upper() for x in sql_enum("rule", "reason_kind")}),
            ("Comment.kind", ev("Comment", "properties", "kind"), {"NOTE", "QUESTION", "ISSUE", "RISK"}),
            ("Event.channel", ev("Event", "properties", "channel"), {"CLI", "API", "IMPORT", "LEGACY"}),
            ("Health", ev("Health"), {"NORMAL", "AT_RISK", "BLOCKED"}),
            ("Scope", ev("Scope"), {"IN_SCOPE", "OUT_OF_SCOPE"}),
            ("StageState", ev("StageState"), {"OPEN", "PASSED", "NOT_APPLICABLE"}),
        ]
        for name, a, b in pairs:
            A.check(area, "OpenAPI %s == SQL/code vocabulary" % name, a == b, "openapi %s, other %s" % (sorted(a), sorted(b)))
        a = ev("ImportAction")
        b = {x.upper() for x in re.findall(r"'(\w+)'", re.search(r"action ENUM\(([^)]*)\)", MIG_ALL).group(1))}
        A.check(area, "import_row.action ENUM is within OpenAPI ImportAction; extra OpenAPI actions are TASK_* only", b <= a and all(x.startswith("TASK_") for x in a - b), "openapi %s sql %s" % (sorted(a), sorted(b)))
        # actions the import docs describe (TASK_*, INVALID) must be in the contract that the summary/diff expose
        doc = read("docs/import-format.md") + read("docs/cli-import.md")
        used = set(re.findall(r"`((?:TASK_)?(?:NEW|CHANGED|UNCHANGED|MISSING|CONFLICT|SUSPECT|INVALID))`", doc))
        A.check(area, "every import class used in docs is an OpenAPI ImportAction", used <= a, "docs-only classes: %s" % sorted(used - a))
        # comment kinds in the SQL views (lowercase) are compared in section_vocab
    # db column check for stage state values used in Go
    A.check(area, "Go stage state values pass/open/na", all(x in GO_SRC["internal/core/validate.go"] for x in ('StatePass = "pass"', 'StateOpen = "open"', 'StateNA   = "na"')))
    # server http mapping vs spec statuses
    hm = go_http_map()
    A.check(area, "every Go status has an HTTP mapping", set(GO_STATUS.values()) - {"INTERNAL"} <= set(hm) | {"INVALID_ARGUMENT", "FAILED_PRECONDITION"}, "mapped %s" % hm)


def section_vocab_docs():
    """stage keys, reason keys, error reasons, statuses, view names as written in docs/skills/README"""
    area = "docs-vocab"
    enums = oas_enums()
    enumvals = {v for vals in enums.values() for v in vals}
    known_upper = set(enumvals) | set(GO_REASONS) | set(GO_STATUS.values())
    known_upper |= {"NEW", "CHANGED", "UNCHANGED", "MISSING", "CONFLICT", "SUSPECT", "INVALID", "TASK_NEW", "TASK_CHANGED", "TASK_UNCHANGED",
                    "TASK_MISSING", "TASK_CONFLICT", "TASK_CREATED", "TASK_UPDATED", "TASK_SKIPPED", "CREATED", "UPDATED", "SKIPPED",
                    "SELECT", "NULL", "NDJSON", "CSV", "JSON", "SQL", "TTY", "HTTP", "UUID", "DSN", "TSV", "URL", "TLS", "HMAC", "PUT", "GET", "POST",
                    "PATCH", "DELETE", "PASS", "AND", "OR", "NOT", "NOTE", "OK", "ID", "AIP", "REST", "API", "CLI", "RFC", "UTC", "LIKE", "IS", "SHA256",
                    "NA", "IN", "OUT", "DONE", "OPEN", "ON", "MD", "UNIQUE", "JOB", "FILE", "CM", "WF", "TK", "SRC", "STAGE", "CLASS", "N", "FROM", "WHERE",
                    "LIMIT", "GROUP", "ORDER", "BY", "AS", "JOIN", "LEFT", "COUNT", "SUM", "DATE", "FALSE", "TRUE", "WITH", "UNION", "EXISTS", "CTE", "HAVING", "ACCOUNT",
                    "TODO", "MAX", "MIN", "AVG", "DESC", "ASC", "DISTINCT", "BETWEEN", "ELSE", "THEN", "WHEN", "CASE", "END", "MB", "GB", "KB", "TB", "MiB", "GiB", "S3", "ISO", "BOM", "TOCTOU", "MVP", "SRE", "OOM", "LF", "CRLF", "HEAD", "HTTPS"}
    # --- uppercase tokens in backticks: error statuses / reasons / enum values
    for f in DOC_FILES:
        if f.startswith(EXTERNAL_VOCAB_DOCS):
            continue
        blocks, prose = MD[f]
        for i, code in inline_codes(prose):
            for tok in re.findall(r"(?<![\w$.-])([A-Z][A-Z0-9]+(?:_[A-Z0-9]+)*)(?![\w-])", code):
                if tok.startswith("OPENETL_") or len(tok) < 3 and tok not in ("OK",):
                    continue
                if tok in known_upper:
                    if tok in GO_STATUS.values() or tok in GO_REASONS or tok in enumvals:
                        A.check(area, "%s:%d `%s` is a status/reason/enum of the contract" % (f, i, tok), tok in GO_STATUS.values() or tok in GO_REASONS or tok in enumvals)
                    continue
                if "_" in tok or tok in ("ABORTED",):
                    A.check(area, "%s:%d `%s` is a known status/reason/enum" % (f, i, tok), False,
                            "unknown UPPER_SNAKE token (not an error reason raised in Go, not an OpenAPI enum value)")
    # explicit "reason X" claims: statuses paired with reasons e.g. ABORTED/ETAG_MISMATCH
    for f in DOC_FILES:
        text = read(f)
        for m in re.finditer(r"\b(%s)\s*/\s*([A-Z][A-Z0-9_]+)\b" % "|".join(sorted(set(GO_STATUS.values()))), text):
            st, rs = m.groups()
            stat_consts = {k for k, v in GO_STATUS.items() if v == st}
            ok = rs in GO_REASONS and bool(GO_REASONS[rs] & stat_consts)
            A.check(area, "%s:%d %s/%s pair matches the code" % (f, lineno(text, m.start()), st, rs), ok,
                    "code raises %s with %s" % (rs, sorted(GO_REASONS.get(rs, []))))
    spec = read("docs/m0-naming-spec.md")
    for m in re.finditer(r"\b([A-Z_]+)\(([A-Z_, ]+)\)", spec):
        st, rs = m.group(1), [x.strip() for x in m.group(2).split(",")]
        if st in GO_STATUS.values():
            stat_consts = {k for k, v in GO_STATUS.items() if v == st}
            for r in rs:
                A.check(area, "naming spec %s(%s) matches the code" % (st, r), r in GO_REASONS and bool(GO_REASONS[r] & stat_consts), "code raises %s with %s" % (r, sorted(GO_REASONS.get(r, []))))
    # --- HTTP codes next to status names
    hm = go_http_map()
    for f in DOC_FILES:
        text = read(f)
        for m in re.finditer(r"`(%s)`\s*[（(](\d{3})[）)]" % "|".join(sorted(set(GO_STATUS.values()))), text):
            st, code = m.group(1), int(m.group(2))
            consts = [k for k, v in GO_STATUS.items() if v == st]
            actual = HTTP_CODE.get(hm.get(st, ""), None)
            A.check(area, "%s:%d %s (%d) matches the HTTP mapping" % (f, lineno(text, m.start()), st, code), actual == code, "server maps it to %s" % actual)
    # --- stage keys
    stage_ctx = [r"--stage\s+(\w+)", r"\bstages?\.(\w+)(?![\w.])(?!\s*\))", r"stage:\s*\"?(\w+)", r"stage\s+(?:advance|regress)\s+\S+\s+--stage\s+(\w+)", r"`(\w+)`\s*(?:开发|部署|校验|验收|双跑|割接)"]
    for f in DOC_FILES + SCRIPT_FILES:
        text = read(f)
        for rx in stage_ctx:
            for m in re.finditer(rx, text):
                v = m.group(1)
                if v in ("STAGE", "stage", "x", "name", "state", "N", "json", "yaml", "txt", "md", "sh", "sql") or v.startswith("$") or v.isupper() or v in ("key",):
                    continue
                A.check(area, "%s:%d stage key %r exists in the seeds" % (f, lineno(text, m.start()), v), v in STAGE_KEYS, "seeded stages: %s" % STAGE_KEYS)
    zh = {r[0]: r[2] for r in STAGES}
    for f in DOC_FILES:
        text = read(f)
        for m in re.finditer(r"`(\w+)`\s*(开发|部署|校验|验收|双跑|割接)", text):
            A.check(area, "%s:%d Chinese name of %s" % (f, lineno(text, m.start()), m.group(1)), zh.get(m.group(1)) == m.group(2), "seed says %r" % zh.get(m.group(1)))
        # arrow chains of stage keys keep the seeded order
        for m in re.finditer(r"((?:`\w+`\s*\S*\s*→\s*)+`\w+`\s*\S*)", text):
            keys = re.findall(r"`(\w+)`", m.group(1))
            if all(k in STAGE_KEYS for k in keys):
                A.check(area, "%s:%d stage chain follows the seeded order" % (f, lineno(text, m.start())), keys == STAGE_KEYS[:len(keys)] or keys == STAGE_KEYS, str(keys))
    # stage list in the naming spec
    m = re.search(r"Stage keys:\s*([a-z_, ]+?)\s*\(", spec)
    if m:
        A.check(area, "naming spec stage list == seeds", [x.strip() for x in m.group(1).split(",")] == STAGE_KEYS, m.group(1))
    desc = OAS_TEXT
    m = re.search(r"defaults:\s*([a-z_, ]+?)\.", desc)
    if m:
        A.check(area, "OpenAPI StageKey default list == seeds", [x.strip() for x in m.group(1).split(",")] == STAGE_KEYS, m.group(1))
    # --- reason keys
    for f in DOC_FILES + SCRIPT_FILES:
        text = read(f)
        for m in re.finditer(r"--reason[ =]+[\"']?([a-z][a-z0-9_]*)", text):
            if "expect_fail" in text[text.rfind("\n", 0, m.start()) + 1: text.find("\n", m.start())]:
                continue
            A.check(area, "%s:%d --reason %s exists in the seeds" % (f, lineno(text, m.start()), m.group(1)), m.group(1) in REASON_KEYS)
        for m in re.finditer(r"\b(rej_[a-z_]+|na_[a-z_]+)\b", text):
            A.check(area, "%s:%d reason key %s exists in the seeds" % (f, lineno(text, m.start()), m.group(1)), m.group(1) in REASON_KEYS)
        for m in re.finditer(r"`(%s)`" % "|".join(sorted(REASON_KEYS, key=len, reverse=True)), text):
            pass
        for m in re.finditer(r"(?:reason(?:_key)?|原因)[^\n`]{0,12}`([a-z][a-z0-9_]{2,})`", text):
            v = m.group(1)
            if v in ("reason_key", "reason", "reasons", "kind", "name", "id") or v.startswith("openetl"):
                continue
            A.check(area, "%s:%d reason `%s` exists in the seeds" % (f, lineno(text, m.start()), v), v in REASON_KEYS)
    # reason kinds in Skills (config get 'kind' values) are the API enum
    for f in DOC_FILES:
        text = read(f)
        for m in re.finditer(r"`kind`\s*为\s*`(\w+)`", text):
            A.check(area, "%s:%d reason kind %s exists" % (f, lineno(text, m.start()), m.group(1)), m.group(1) in {"REJECT", "DISCARD", "NOT_APPLICABLE", "GATE_OVERRIDE"})
    # --- view names: "N public views" claims
    nums = {"two": 2, "three": 3, "four": 4, "five": 5, "six": 6, "seven": 7, "eight": 8, "nine": 9, "ten": 10}
    npub = len(json.loads(read("internal/cli/views.json")))
    for f in DOC_FILES:
        text = read(f)
        for m in re.finditer(r"\b(%s|\d+)\s+(?:documented\s+|public\s+)+views" % "|".join(nums), text):
            n = nums.get(m.group(1)) or int(m.group(1))
            A.check(area, "%s:%d '%s public views' matches the %d views" % (f, lineno(text, m.start()), m.group(1), npub), n == npub)
    # views named in prose/code spans are public views (words directly before 'view')
    pv = {v["name"] for v in json.loads(read("internal/cli/views.json"))}
    for f in DOC_FILES:
        blocks, prose = MD[f]
        for i, code in inline_codes(prose):
            m = re.match(r"^`?(\w+)`?\s+(?:view|视图)$", code)
        if f.startswith(EXTERNAL_VOCAB_DOCS):
            continue
        for i, ln in prose:
            for m in re.finditer(r"`(\w+)`\s*(?:视图|view)\b", ln):
                A.check(area, "%s:%d view `%s` is a public view" % (f, i, m.group(1)), m.group(1) in pv)
    # --- health words
    for f in DOC_FILES:
        text = read(f)
        for m in re.finditer(r"ok\|at_risk\|blocked", text):
            A.check(area, "%s:%d health list ok|at_risk|blocked matches the views" % (f, lineno(text, m.start())), True)
        for m in re.finditer(r"health\s*(?:=|<>|!=)\s*'(\w+)'", text):
            A.check(area, "%s:%d health literal '%s' is a view value" % (f, lineno(text, m.start()), m.group(1)), m.group(1) in ("ok", "at_risk", "blocked"))
        for m in re.finditer(r"\bscope\s*(?:=|<>|!=)\s*'(\w+)'", text):
            A.check(area, "%s:%d scope literal '%s' is a view value" % (f, lineno(text, m.start()), m.group(1)), m.group(1) in ("in", "out"))
        for m in re.finditer(r"\bkind\s*=\s*'(\w+)'", text):
            A.check(area, "%s:%d comment kind literal '%s'" % (f, lineno(text, m.start()), m.group(1)), m.group(1) in ("note", "question", "issue", "risk"))
        for m in re.finditer(r"\bk\s*(?:=|LIKE)\s*'([a-z_.%]+)'", text):
            k = m.group(1)
            if f.startswith(EXTERNAL_VOCAB_DOCS) or "..." in k:
                continue
            ok = bool(re.match(r"^(stage\.(%s|%%)|scope|name|deleted|into|on|kind|body|resolved|attr\.|assignee\.|ann\.|workflow|cascade|evidence\.|override\.|source\.|assignee\.\w+|attr\.\w+)" % "|".join(STAGE_KEYS), k))
            A.check(area, "%s:%d event key '%s' is a known key shape" % (f, lineno(text, m.start()), k), ok)
    # --- numeric claims
    cap = 104857600
    for f in DOC_FILES:
        text = read(f)
        for m in re.finditer(r"(\d+)\s*(MiB|MB)", text):
            if m.group(1) == "100":
                A.warn(area, "%s:%d attachment cap '%s' (limit is %d bytes = 100 MiB)" % (f, lineno(text, m.start()), m.group(0), cap), m.group(2) == "MiB",
                       "unit should be MiB (104857600 bytes)")


def section_env():
    area = "env"
    code_prod, code_test = {}, {}
    for f in GO_FILES:
        for m in re.finditer(r"\"(OPENETL_[A-Z0-9_]+)\"", GO_SRC[f]):
            (code_test if f.endswith("_test.go") else code_prod).setdefault(m.group(1), set()).add(f)
    scripts = {}
    for f in SCRIPT_FILES + ["docker-compose.yml", "Dockerfile", "scripts/create-query-user.sql"]:
        if exists(f):
            for v in re.findall(r"OPENETL_[A-Z0-9_]+", read(f)):
                scripts.setdefault(v, set()).add(f)
    docs = {}
    for f in DOC_FILES:
        for i, ln in enumerate(read(f).split("\n"), 1):
            for v in re.findall(r"OPENETL_[A-Z0-9_]+", ln):
                docs.setdefault(v, set()).add("%s:%d" % (f, i))
    for v, where in sorted(docs.items()):
        real = {w for w in where if not w.startswith("docs/backlog/")}
        if not real:
            A.warn(area, "%s is only mentioned in backlog proposals" % v, v in code_prod, "proposal, not implemented: %s" % sorted(where)[:2])
            continue
        A.check(area, "documented %s exists in code, tests or scripts" % v, v in code_prod or v in code_test or v in scripts,
                "documented at %s but nothing reads or sets it" % sorted(real)[:3])
    for v, where in sorted(code_prod.items()):
        A.check(area, "env var %s read by code is documented" % v, v in docs and any(not w.startswith("docs/backlog/") for w in docs[v]),
                "read in %s, not mentioned in README/docs/skills" % sorted(where)[0])
    for v in sorted(set(scripts) - set(code_prod) - set(code_test)):
        if v.startswith(("OPENETL_DEMO", "OPENETL_DEV")):
            continue
        A.check(area, "script/compose var %s is read by code" % v, False, "set in %s but never read" % sorted(scripts[v]))
    # env var in compose really consumed by the server
    for v in sorted(re.findall(r"^\s+(OPENETL_\w+):", read("docker-compose.yml"), re.M)):
        A.check(area, "docker-compose sets %s which the server reads" % v, v in code_prod)
    # CLI --help texts name only real env vars
    rc, o, e = sh([BIN_CLI, "--help"], env=cli_env())
    for v in re.findall(r"OPENETL_[A-Z0-9_]+", o):
        A.check(area, "CLI help names %s which the CLI reads" % v, v in code_prod)


def section_paths():
    area = "paths"
    top = {"api", "cmd", "db", "docs", "internal", "scripts", "skills"}  # bin/ is a build output
    rootfiles = {"README.md", "Dockerfile", "docker-compose.yml", "go.mod", "go.sum", ".dockerignore"}
    for f in DOC_FILES:
        blocks, prose = MD[f]
        cands = []
        for i, code in inline_codes(prose):
            for tok in re.split(r"[\s,;()]+", code):
                cands.append((i, tok))
        for b in blocks:
            if b.lang in ("bash", "sh", ""):
                for k, ln in enumerate(b.text.split("\n")):
                    for tok in re.split(r"[\s,;()<>|\"']+", ln):
                        cands.append((b.line + k, tok))
        for i, ln in prose:
            for m in re.finditer(r"\]\(([^)#\s]+)(?:#[^)]*)?\)", ln):
                if not m.group(1).startswith(("http", "mailto")):
                    cands.append((i, os.path.normpath(os.path.join(os.path.dirname(f), m.group(1)))))
        seen = set()
        for i, tok in cands:
            t = tok.strip("`'\".:,").lstrip("./") if tok.startswith("./") else tok.strip("`'\".:,")
            if not t or any(c in t for c in "<>$*{}=~") or ".." in t:
                continue
            first = t.split("/")[0]
            if (first in top and ("/" in t or "." in t)) or t in rootfiles:
                if (f, t) in seen:
                    continue
                seen.add((f, t))
                p = t.rstrip("/")
                p = re.sub(r"(\.(?:md|sql|sh|py|go|yaml|yml|json))[:#].*$", r"\1", p)
                if re.search(r"[A-Za-z0-9_-]+\.(?:md|sql|sh|py|go|yaml|yml|json)$", p) or "/" in p or p in rootfiles:
                    A.check(area, "%s:%d path %s exists" % (f, i, t), exists(p) or bool(glob.glob(os.path.join(ROOT, p) + "*")), "referenced but missing")
    # Layout table of the README lists every top-level dir that exists and nothing else
    readme = read("README.md")
    layout = re.search(r"## Layout\n(.*?)\n## ", readme, re.S)
    listed = set(re.findall(r"^\|\s*`([\w./-]+)`\s*\|", layout.group(1) if layout else "", re.M))
    for d in sorted(listed):
        A.check(area, "README layout entry %s exists" % d, exists(d.rstrip("/")))
    for d in sorted(os.listdir(ROOT)):
        if os.path.isdir(os.path.join(ROOT, d)) and not d.startswith(".") and d not in ("bin", "data", "node_modules"):
            A.check(area, "top-level dir %s/ is listed in the README layout table" % d, any(x == d or x.startswith(d + "/") for x in listed), "missing from README Layout")
    # binaries named in docs exist as cmd dirs
    cmds = set(os.listdir(os.path.join(ROOT, "cmd")))
    for f in DOC_FILES + SCRIPT_FILES + GO_FILES + list(MIG):
        text = read(f)
        for m in re.finditer(r"(?<![\w/.])(openetl-[a-z]+)\b", text):
            n = m.group(1)
            if n in ("openetl-cli",) or exists("skills/%s" % n) or text[max(0, m.start() - 40):m.start()].endswith("github.com/life-is-blue/"):
                continue
            A.check(area, "%s:%d binary name %s exists under cmd/" % (f, lineno(text, m.start()), n), n in cmds or n in ("openetl-mysql8",), "cmd/ has %s" % sorted(cmds))
    # Skills: frontmatter and cross references
    for sk in sorted(glob.glob("skills/*/SKILL.md")):
        d = os.path.basename(os.path.dirname(sk))
        text = read(sk)
        fm = re.match(r"^---\nname:\s*(\S+)\ndescription:\s*(.+)\n---\n", text)
        A.check(area, "%s has frontmatter" % sk, fm is not None)
        if fm:
            A.check(area, "%s: name == directory" % sk, fm.group(1) == d)
            A.check(area, "%s: description is non-empty and < 1024 chars" % sk, 20 < len(fm.group(2)) < 1024)
        for ref in set(re.findall(r"`(openetl-[a-z]+)`", text)):
            A.check(area, "%s: referenced skill %s exists" % (sk, ref), exists("skills/%s/SKILL.md" % ref) or ref in cmds)


def section_cli(root, nodes):
    area = "cli"
    cmds = extract_commands()
    A.check(area, "found documented command lines", len(cmds) > 50, "only %d" % len(cmds))
    doc_paths, doc_flags = set(), {}
    env = cli_env()
    for f, ln, toks, partial in cmds:
        label = "%s:%d openetl %s" % (f, ln, " ".join(toks)[:90])
        ok, msg, node = check_command(nodes, f, ln, toks, root, partial)
        A.check(area, "parses against the CLI tree: " + label, ok, msg)
        if ok:
            doc_paths.add(tuple(node.path))
            for t in toks:
                if t.startswith("--"):
                    doc_flags.setdefault((tuple(node.path), t[2:].split("=")[0]), []).append("%s:%d" % (f, ln))
            # the real parser must agree (flags are parsed before --help is honoured; nothing is sent)
            real = [t for t in toks if "\n" not in t]
            if not partial:
                rc, o, e = sh([BIN_CLI] + real + ["--help"], env=env)
                A.check(area, "real CLI accepts it (--help dry parse): " + label, rc == 0, (e or o).strip()[:200])
            # required flags per Usage line are present (unless asking for help)
            req = required_flags(node) if not node.children else set()
            present = {t[2:].split("=")[0] for t in toks if t.startswith("--")}
            if req and not partial and "help" not in present and "dry-run" not in present:
                A.warn(area, "required flags %s given: %s" % (sorted(req), label), req <= present, "missing %s" % sorted(req - present))
    # reverse direction: every real command and flag is documented somewhere
    alltext = "\n".join(read(f) for f in DOC_FILES + [g for g in GENERATED_DOCS if exists(g)])
    for path, n in sorted(nodes.items()):
        if not path:
            continue
        leaf = not n.children
        if leaf:
            documented = path in doc_paths or " ".join(path) in alltext
            A.warn(area, "command `openetl %s` is documented" % " ".join(path), documented, "not mentioned in README/docs/skills")
    for path, n in sorted(nodes.items()):
        if n.children or not path:
            continue
        for fl in sorted(n.flags):
            if fl == "help":
                continue
            A.warn(area, "flag --%s of `openetl %s` is documented" % (fl, " ".join(path)), ("--" + fl) in alltext, "no doc mentions --%s" % fl)
    # subcommand groups named by the shared Skill
    shared = read("skills/openetl-shared/SKILL.md")
    m = re.search(r"子命令组：(.*)", shared)
    if m:
        for g in re.findall(r"`(\w+)`", m.group(1)):
            A.check(area, "skill names command group `%s` which exists" % g, (g,) in nodes)
    # behaviours the Skills promise
    rc, o, e = sh([BIN_CLI, "--version"], env=env)
    A.check(area, "`openetl --version` prints 'openetl <version>'", re.match(r"^openetl \S+", o) is not None, o)
    rc, o, e = sh([BIN_CLI, "auth", "status"], env=env)
    A.check(area, "`auth status` without a token exits 1 with \"authenticated\":false", rc == 1 and '"authenticated":false' in o, "rc=%s out=%s" % (rc, o))
    rc, o, e = sh([BIN_CLI, "no-such-command"], env=env)
    A.check(area, "unknown command exits 2 (usage error)", rc == 2, "rc=%s" % rc)
    rc, o, e = sh([BIN_CLI, "stage", "advance"], env=env)
    A.check(area, "missing arguments exit 2 with a JSON error on stderr", rc == 2 and '"error"' in e, "rc=%s err=%s" % (rc, e[:100]))
    # defaults promised in docs equal the flag defaults
    imp = nodes[("import", "stage")].help
    m = re.search(r"--max-rows[^\n]*\(default (\d+)\)", imp)
    A.check(area, "--max-rows default 100000 as documented", m is not None and m.group(1) == "100000", imp)
    A.check(area, "docs say 10 minute default wait", "(default 10m0s)" in nodes[("import", "apply")].help, nodes[("import", "apply")].help)
    A.check(area, "docs say diff --limit default 10", "(default 10)" in nodes[("import", "diff")].help)
    # openetl-server commands
    srv = GO_SRC["cmd/openetl-server/main.go"]
    sub = set(re.findall(r"case \"([\w-]+)\"", srv))
    flags = set(re.findall(r"fs\.\w+\(\"([\w-]+)\"", srv))
    for f in DOC_FILES + SCRIPT_FILES:
        lines = []
        if f in MD:
            blocks, prose = MD[f]
            for b_ in blocks:
                if b_.lang in ("bash", "sh", "shell", "console", ""):
                    lines += [(b_.line + k, l.strip()) for k, l in enumerate(join_continuations(b_.text).split("\n")) if not l.strip().startswith("#")]
            lines += [(i, c) for i, c in inline_codes(prose)]
        else:
            lines += [(i, l.strip()) for i, l in enumerate(join_continuations(read(f)).split("\n"), 1) if not l.strip().startswith("#")]
        for i, l in lines:
            rest = find_cmd(l, "openetl-server")
            if rest is None:
                continue
            try:
                toks = shell_tokens(rest)
            except ValueError:
                continue
            if not toks or not re.match(r"^[a-z][a-z-]*$", toks[0]) or any(t.startswith(("...", "*")) for t in toks):
                continue
            where = "%s:%d" % (f, i)
            A.check(area, "%s openetl-server %s" % (where, " ".join(toks)[:60]), toks[0] in sub and (toks[0] != "admin" or len(toks) < 2 or toks[1] in sub), "server subcommands: %s" % sorted(sub))
            for t in toks:
                if t.startswith("--"):
                    A.check(area, "%s server flag %s" % (where, t), t[2:].split("=")[0] in flags, "create-token flags: %s" % sorted(flags))


def section_mapping():
    area = "import"
    # mapping keys allowed by the Go struct tags
    src = GO_SRC["internal/importsrc/mapping.go"]
    allowed = set(re.findall(r"yaml:\"(\w+)\"", src))
    # example mapping in the docs
    blocks, _ = MD["docs/cli-import.md"]
    ex = next((b for b in blocks if b.lang == "yaml"), None)
    if ex:
        keys = set(re.findall(r"^\s*#?\s*([a-z_]+):", ex.text, re.M)) - {"query", "http", "mysql"}
        for k in sorted(keys):
            A.check(area, "mapping key `%s` in docs/cli-import.md is accepted by the mapping parser" % k, k in allowed or k in {"dom", "area", "application", "priority", "category", "name", "domain"} or k in ("column", "default", "const"), "")
    # generated mapping/rows from gen-dataset.py run through the real CLI, locally only
    d = os.path.join(TMP, "gen")
    rc, o, e = sh([sys.executable, "scripts/gen-dataset.py", d, "--small"])
    A.check(area, "gen-dataset.py --small runs", rc == 0, e)
    if rc == 0:
        m = re.match(r"(\d+) workflows, (\d+) tasks", o)
        A.check(area, "gen-dataset.py prints '<n> workflows, <m> tasks' (e2e.sh parses it)", m is not None, o)
        rc, o2, e2 = sh([BIN_CLI, "import", "stage", "--from", d + "/rows.ndjson", "--map", d + "/mapping.yaml", "--dry-run"], env=cli_env())
        A.check(area, "mapping.yaml of gen-dataset.py is accepted by `import stage --dry-run`", rc == 0, e2 or o2[:200])
        if rc == 0 and m:
            A.check(area, "dry-run row count equals the generator's count", json.loads(o2).get("rows") == int(m.group(1)))
    # import-format.md example is valid JSON and its keys exist in the OpenAPI ImportRow/Workflow schema
    blocks, _ = MD["docs/import-format.md"]
    jb = next((b for b in blocks if b.lang == "json"), None)
    if jb and OAS:
        try:
            req = json.loads(jb.text)
            A.check(area, "docs/import-format.md example is valid JSON", True)
            sch = OAS["components"]["schemas"]
            row = req["rows"][0]
            A.check(area, "example row keys within ImportRow", set(row) <= set(sch["ImportRow"]["properties"]), str(set(row) - set(sch["ImportRow"]["properties"])))
            A.check(area, "example data keys within Workflow", set(row["data"]) <= set(sch["Workflow"]["properties"]), str(set(row["data"]) - set(sch["Workflow"]["properties"])))
            A.check(area, "example task keys within ImportTask/Task", set(row["tasks"][0]["data"]) <= set(sch["Task"]["properties"]))
            A.check(area, "example request keys within ImportJob", set(req) <= set(sch["ImportJob"]["properties"]) | {"request_id"}, str(set(req) - set(sch["ImportJob"]["properties"])))
            for s in row["data"]["stages"]:
                A.check(area, "example stage %s exists" % s["stage"], s["stage"] in STAGE_KEYS)
                A.check(area, "example state %s is a StageState" % s["state"], s["state"] in sch["StageState"]["enum"])
            A.check(area, "example scope is a Scope", row["data"]["scope"] in sch["Scope"]["enum"])
        except Exception as ex:
            A.check(area, "docs/import-format.md example is valid JSON", False, str(ex))


def section_sql():
    area = "sql-snippets"
    snippets = extract_sql()
    A.check(area, "found SQL snippets in docs/skills/scripts/OpenAPI", len(snippets) > 10, "only %d" % len(snippets))
    inp = "\n".join(json.dumps({"id": sid, "sql": s}) for sid, f, ln, s in snippets)
    rc, o, e = sh([BIN_GUARD], inp=inp)
    A.check(area, "guardcheck ran", rc == 0, e)
    res = {}
    for l in o.strip().split("\n"):
        if l:
            r = json.loads(l)
            res[r["id"]] = r
    for sid, f, ln, s in snippets:
        r = res.get(sid, {"ok": False, "err": "no result"})
        A.check(area, "passes the query guard: %s  %s" % (sid, s[:70]), r["ok"], r.get("err", ""))
    if NO_DB:
        A.skip(area, "EXPLAIN of snippets (columns exist)", "--no-db")
        return
    db = os.environ.get("AUDIT_DB", "openetl_d")
    rc, o, e = mysql(db, "SELECT 1;")
    if rc != 0:
        A.skip(area, "EXPLAIN of snippets (columns exist)", "database %s unreachable: %s" % (db, e.strip()[:120]))
        return

    def explain(item):
        sid, f, ln, s = item
        r = res.get(sid)
        if not r or not r["ok"]:
            return sid, s, None
        rc, o, e = mysql(db, "EXPLAIN " + r["rewritten"] + ";")
        return sid, s, (rc, e)
    with concurrent.futures.ThreadPoolExecutor(8) as ex:
        for sid, s, out in ex.map(explain, snippets):
            if out is not None:
                err = next((l for l in out[1].split("\n") if l.startswith("ERROR")), out[1].strip()[:200])
                A.check(area, "columns and functions exist in MySQL (EXPLAIN): %s  %s" % (sid, s[:60]), out[0] == 0, err)


def go_consts():
    out = {}
    for f in GO_PROD:
        for m in re.finditer(r"\b(\w+)\s*=\s*(?:int64\()?(\d+)\)?(?:\s*<<\s*(\d+))?(?=\s*(?:\n|//|$))", GO_SRC[f]):
            out[m.group(1)] = int(m.group(2)) << int(m.group(3) or 0)
    return out


def section_limits():
    """numbers that appear in code, OpenAPI, CLI help and prose must agree"""
    area = "limits"
    C = go_consts()
    readme, sec = read("README.md"), read("docs/security.md")
    def need(name):
        if name not in C:
            A.check(area, "Go constant %s found" % name, False, "renamed? update audit.py")
            return None
        return C[name]
    per, tot, pool = need("DefaultQueryMaxPerAccount"), need("DefaultQueryMaxTotal"), need("QueryPoolSize")
    mps, dps = need("queryMaxPS"), need("queryDefaultPS")
    quota = need("DefaultProjectStorageQuota")
    for f in DOC_FILES:
        text = read(f)
        for m in re.finditer(r"OPENETL_QUERY_MAX_PER_ACCOUNT`?\s*\(default (\d+)\)", text):
            A.check(area, "%s: QUERY_MAX_PER_ACCOUNT default %s equals code (%s)" % (f, m.group(1), per), int(m.group(1)) == per)
        for m in re.finditer(r"OPENETL_QUERY_MAX_TOTAL`?\s*\(default (\d+)\)", text):
            A.check(area, "%s: QUERY_MAX_TOTAL default %s equals code (%s)" % (f, m.group(1), tot), int(m.group(1)) == tot)
        for m in re.finditer(r"(\d+)-connection pool", text):
            A.check(area, "%s: query pool of %s connections equals code (%s)" % (f, m.group(1), pool), int(m.group(1)) == pool)
        for m in re.finditer(r"OPENETL_PROJECT_STORAGE_QUOTA_BYTES`?\s*\(default (\d+) GiB\)|caps the[^\n]{0,80}\(default (\d+) GiB\)", text):
            g = int(m.group(1) or m.group(2))
            A.check(area, "%s: storage quota default %d GiB equals code" % (f, g), quota == g << 30)
        for m in re.finditer(r"`page_size`\s*<=\s*(\d+)", text):
            A.check(area, "%s: query page_size <= %s equals server maximum (%s)" % (f, m.group(1), mps), int(m.group(1)) == mps)
    if OAS and mps:
        ps = OAS["components"]["schemas"]["QueryRequest"]["properties"]["page_size"]
        A.check(area, "OpenAPI QueryRequest.page_size maximum (%s) equals the server's queryMaxPS (%s)" % (ps.get("maximum"), mps), ps.get("maximum") == mps,
                "the server silently clamps larger page sizes")
    rc, o, e = sh([BIN_CLI, "query", "--help"], env=cli_env())
    m = re.search(r"--page-size int\s+rows per page \(max (\d+)\)", o)
    if m and mps:
        A.check(area, "`openetl query --help` page-size maximum (%s) equals the server's queryMaxPS (%s)" % (m.group(1), mps), int(m.group(1)) == mps,
                "the CLI promises more than the server returns")
    if OAS:
        cap = OAS["components"]["schemas"]["InitUploadRequest"]["properties"]["size"]["maximum"]
        A.check(area, "attachment cap in OpenAPI is 100 MiB", cap == 100 << 20, str(cap))
        found = [v for k, v in C.items() if v == 100 << 20]
        A.check(area, "attachment cap 100 MiB exists as a Go constant", bool(found) or "104857600" in "".join(GO_SRC.values()) or "100 << 20" in "".join(GO_SRC.values()) or "100*1024*1024" in "".join(GO_SRC.values()))
    # features advertised by the contract but refused by the server
    rejects_asof = any("AS_OF_UNSUPPORTED" in GO_SRC[f] for f in GO_PROD)
    if OAS:
        has = "as_of" in OAS["components"]["schemas"]["QueryRequest"]["properties"]
        A.check(area, "OpenAPI QueryRequest.as_of is not advertised while the server answers AS_OF_UNSUPPORTED", not (has and rejects_asof),
                "the server rejects as_of on :query; remove it from the contract and from `openetl query --as-of`, or implement it")
    A.check(area, "`openetl query --as-of` is not offered while the server answers AS_OF_UNSUPPORTED", not ("--as-of" in o and rejects_asof),
            "the CLI flag always fails")
    # a status that the contract lists but no code raises
    raised = {st for r, sts in GO_REASONS.items() for st in sts}
    for const, val in sorted(GO_STATUS.items()):
        A.warn(area, "status %s is raised somewhere in the server code" % val, const in raised or const.replace("Status", "") == "Internal", "declared in core and the OpenAPI Error.status enum, never raised")


def section_misc():
    area = "misc"
    # cli_ docs versus openapi: skills mention filter fields of list calls
    filt = re.search(r"`filter` supports ([^\n]*)", OAS_TEXT)
    fields = set(re.findall(r"`(?:labels\.<k>|stages\.<stage>|assignees\.<role>|[a-z_]+)(?:[ =:][^`]*)?`", filt.group(0))) if filt else set()
    sk = read("skills/openetl-workflow/SKILL.md")
    for m in re.finditer(r"--filter\s+'([^']*)'", sk):
        for fld in re.findall(r"\b([a-z_]+(?:\.[a-z_]+)?)\s*(?:=|:)", m.group(1)):
            base = fld.split(".")[0]
            A.check(area, "skill --filter field %s is supported by listWorkflows/listComments" % fld, base in ("labels", "stages", "assignees", "display_name", "health", "scope", "current_stage", "resolved", "kind", "side", "creator"))
    # the dev scripts must reference scripts that exist
    for f in SCRIPT_FILES:
        text = read(f)
        for m in re.finditer(r"scripts/[\w./-]+", text):
            if "$" not in m.group(0):
                A.check(area, "%s:%d references %s" % (f, lineno(text, m.start()), m.group(0)), exists(m.group(0)))
    # e2e.sh stage keys & numbers against the gen-dataset facts documented in README
    readme = read("README.md")
    m = re.search(r"import (\d+) workflows with ~(\d+) tasks", readme)
    if m:
        A.check(area, "README e2e size (%s workflows) equals gen-dataset default" % m.group(1), "else 3000" in read("scripts/gen-dataset.py") and m.group(1) == "3000")
    qs = read("docs/quickstart.md")
    m = re.search(r"(\d+) workflows, ~(\d+) tasks \(--small: (\d+) workflows\)", qs)
    if m:
        A.check(area, "quickstart dataset size equals the generator (3000/60)", m.group(1) == "3000" and m.group(3) == "60" and "n = 60 if" in read("scripts/gen-dataset.py"))
    # Go version in docs vs go.mod
    gm = re.search(r"^go (\d+\.\d+)", read("go.mod"), re.M).group(1)
    for f in DOC_FILES:
        for m in re.finditer(r"\bGo (\d+\.\d+)\b", read(f)):
            A.check(area, "%s mentions Go %s which matches go.mod (%s)" % (f, m.group(1), gm), m.group(1) == gm)
    df = read("Dockerfile")
    for m in re.finditer(r"golang:(\d+\.\d+)", df):
        A.check(area, "Dockerfile Go image %s is not older than go.mod (%s)" % (m.group(1), gm),
                tuple(map(int, m.group(1).split("."))) >= tuple(map(int, gm.split("."))))
    # port numbers in compose and e2e.sh
    A.check(area, "e2e.sh base URL port equals the compose port mapping", "18080" in read("scripts/e2e.sh") and '"18080:8080"' in read("docker-compose.yml"))



# =========================================================================== vocabulary file
VOCAB_PATH = "scripts/audit/vocab.md"


def md_table(head, rows):
    out = ["| " + " | ".join(head) + " |", "|" + "|".join(" --- " for _ in head) + "|"]
    for r in rows:
        out.append("| " + " | ".join(str(x).replace("|", "\\|") if x is not None else "" for x in r) + " |")
    return "\n".join(out)


def render_vocab(nodes):
    """The single source of vocabulary for docs and the design document.  Everything derivable is derived."""
    L = []
    w = L.append
    w("# openetl vocabulary")
    w("")
    w("Single source of the normative vocabulary. **Generated** by `python3 scripts/audit/audit.py --write-vocab` from the SQL seeds,")
    w("the OpenAPI contract, the Go code, the live CLI and `internal/cli/views.json`; `audit.py` fails when this file is stale.")
    w("Hand-written explanation lives in the sections marked *(convention)*; they are checked against the code where a check is possible.")
    w("")
    w("## Stages (seed `stage_def`, order matters)")
    w("")
    w(md_table(["key", "seq", "中文名", "English"], [(r[0], r[1], r[2], r[3]) for r in sorted(STAGES, key=lambda r: r[1])]))
    w("")
    w("Stored state of a stage (key `stage.<key>`): `pass` | `open` | `na`; no value means `open`. API: `PASSED` | `OPEN` | `NOT_APPLICABLE`.")
    w("`current_stage` = first stage (by seq) whose state is neither `pass` nor `na`; NULL / empty when none. `stage_distribution.stage` shows `done` for NULL.")
    w("")
    w("## Reason kinds, owner sides and seeded reasons")
    w("")
    w("Kinds (`reason.kind`): " + ", ".join("`%s`" % x for x in sql_enum("reason", "kind")) + ". Owner sides: " + ", ".join("`%s`" % x for x in sql_enum("reason", "owner_side")) + ".")
    w("Kind usage *(convention, enforced by core.Validate)*: `reject` = regress a PASSED stage; `discard` = move a workflow out of scope (`merged` also needs `into`);")
    w("`not_applicable` = mark a stage `na`; `gate_override` = let a side pass a gate it normally may not.")
    w("")
    w(md_table(["reason_key", "kind", "owner_side", "中文名", "note_required"], [(r[0], r[1], r[2], r[3], "yes" if r[5] else "") for r in REASONS]))
    w("")
    w("## Rule table (seed `rule`)")
    w("")
    w("A change to a key matching any `k_pattern` must satisfy one matching rule (value, side, reason kind). `_` in a pattern is literal. Unmatched keys: free for editors.")
    w("")
    w(md_table(["k_pattern", "to_value", "side", "reason_kind"], [(r[0], r[1] if r[1] is not None else "(any)", r[2], r[3] or "") for r in RULES]))
    w("")
    w("## Roles, sides, kinds, channels")
    w("")
    w("- Member roles: " + ", ".join("`%s`" % x for x in sql_enum("member", "role")) + " (API `VIEWER|EDITOR|ADMIN`); agents cannot be admin (`AGENT_CANNOT_BE_ADMIN`).")
    w("- Member sides: " + ", ".join("`%s`" % x for x in sql_enum("member", "side")) + "; event sides add `system` (API `SIDE_UNSPECIFIED|DELIVERY|CUSTOMER|SYSTEM`). Rule sides add `any`.")
    w("- Account kinds: " + ", ".join("`%s`" % x for x in sql_enum("account", "kind")) + ".")
    w("- Event channels (`event.channel`): `cli`, `api`, `import`, `legacy` (API `CLI|API|IMPORT|LEGACY`). `import` and `legacy` events are historical facts and skip validation.")
    w("- Assignee roles *(convention, free keys)*: `dev_owner`, `customer_pic`, `verify_pic`; account values are stored without the `accounts/` prefix.")
    w("")
    w("## Health, scope, comment kinds")
    w("")
    w(md_table(["concept", "stored / view value", "API value"], [
        ("health", "ok", "NORMAL"), ("health", "at_risk", "AT_RISK"), ("health", "blocked", "BLOCKED"),
        ("scope", "in", "IN_SCOPE"), ("scope", "out", "OUT_OF_SCOPE"),
        ("comment kind", "note", "NOTE"), ("comment kind", "question", "QUESTION"), ("comment kind", "issue", "ISSUE"), ("comment kind", "risk", "RISK"),
    ]))
    w("")
    w("Health: `blocked` when the workflow has an open `issue`, else `at_risk` when it has an open `risk`, else `ok`. A comment is *open* when it is neither")
    w("resolved nor deleted (`notes` and `questions` never change health). Scope defaults to `in`; `merged_into_id` is only shown while scope is `out`.")
    w("")
    w("## Import")
    w("")
    w("- Job states (`import_batch.state`): " + ", ".join("`%s`" % x for x in sql_enum("import_batch", "state")) + " (API `STAGED|APPLIED|FAILED|ROLLED_BACK`).")
    acts = [x for x in re.search(r"action ENUM\(([^)]*)\)", MIG_ALL).group(1).replace("'", "").split(",")]
    w("- Row classes: " + ", ".join("`%s`" % x.strip().upper() for x in acts) + "; task classes `TASK_NEW|TASK_CHANGED|TASK_UNCHANGED|TASK_MISSING|TASK_CONFLICT`; `INVALID` rows are reported in `row_errors`.")
    rowerrs = sorted(r for r, st in GO_REASONS.items() if "rowError" in st)
    w("- Row error reasons (Go `rowErr`): " + ", ".join("`%s`" % x for x in rowerrs) + ".")
    w("- Diff change kinds: `source`, `fill`, `conflict`. Resolutions: `merge`, `skip`, `link`, `create`.")
    w("")
    w("## Public views (query surface, from `internal/cli/views.json`)")
    w("")
    for v in json.loads(read("internal/cli/views.json")):
        w("- `%s` (%s): %s" % (v["name"], v["description"].rstrip("."), ", ".join("`%s`" % c["name"] for c in v["columns"])))
    w("")
    w("## Event-log key conventions *(convention, mirrored by scripts/reconcile.py)*")
    w("")
    w("`delta.k` / `entity_latest.k`; the current value of a key is the `new_v` of the delta with the greatest `event_id` (never `event_time`). A removed key has `new_v` NULL.")
    w("")
    w(md_table(["entity type", "key", "value (JSON)"], [
        ("workflow, task", "name", "string (display_name)"),
        ("workflow, task", "attr.<label>", "string (labels, lower-case; empty string allowed)"),
        ("workflow, task", "ann.<name>", "string (annotations, free text)"),
        ("workflow, task", "assignee.<role>", "string (account name)"),
        ("workflow, task", "source.system / source.ref / source.hash", "string"),
        ("workflow, task", "deleted", "true | false (soft delete; the `delete_time` is the event's create_time)"),
        ("workflow", "stage.<stage>", "\"pass\" | \"open\" | \"na\""),
        ("workflow", "scope", "\"in\" | \"out\""),
        ("workflow", "into", "number (merge target workflow id; used with scope out)"),
        ("workflow", "evidence.<stage>", "list of attachment names"),
        ("task", "workflow", "<workflow entity id>; exactly one workflow per task (assign moves it)"),
        ("task", "cascade", "<event id of the workflow delete>; set with deleted=true when a workflow delete took the task along, cleared by the workflow undelete"),
        ("workflow, task", "override.<changed key>", "{\"side\": reserved side, \"evidence\": [attachment names]}; an admin acted for the other side (INV-29)"),
        ("comment", "on", "number (workflow id)"),
        ("comment", "kind", "\"note\" | \"question\" | \"issue\" | \"risk\""),
        ("comment", "body", "string (immutable)"),
        ("comment", "resolved", "true | false"),
        ("comment", "deleted", "true | false"),
        ("comment", "attachments", "list of attachment names"),
        ("attachment", "filename, size, sha256, storage_key", "string / number"),
    ]))
    w("")
    w("Idempotency keys *(convention)*: import events `imp-<job>-<row>` (tasks `imp-<job>-<row>-<n>`), rollback events `imprb-<job>-<event>`; otherwise the client's `request_id`.")
    w("")
    w("## Resource names and ids *(convention + names.go)*")
    w("")
    w("`projects/{project}` (lower-case slug), `.../workflows/wf-000123` (code `WF-000123`, parsed case-insensitively), `projects/{p}/tasks/tk-000123`, `.../comments/cm-000123`,")
    w("`.../attachments/at-000123`, `.../events/ev-123` (not zero padded; also the etag), `projects/{p}/importJobs/{n}`, `accounts/{account}`, `projects/{p}/config`.")
    w("Id prefixes in code: " + ", ".join("`%s`" % x for x in re.findall(r'Prefix\w+\s*=\s*"(\w+)"', GO_SRC["internal/store/names.go"])) + ".")
    w("")
    w("## Error statuses, HTTP codes and reasons")
    w("")
    hm = go_http_map()
    code_of = {k: HTTP_CODE.get(v) for k, v in hm.items()}
    rows = []
    for const, val in sorted(GO_STATUS.items(), key=lambda x: x[1]):
        rs = sorted(r for r, st in GO_REASONS.items() if const in st)
        rows.append((val, code_of.get(val, "500" if val == "INTERNAL" else ""), ", ".join("`%s`" % r for r in rs)))
    w(md_table(["status", "HTTP", "reasons raised in code"], rows))
    w("")
    w("`FAILED_PRECONDITION` maps to HTTP 400 (AIP-193). Error body: `{\"error\":{\"code\",\"status\",\"message\",\"details\":[{\"@type\":\"ErrorInfo\",\"reason\",\"domain\":\"openetl\",\"metadata\"}]}}`.")
    w("")
    w("## Environment variables read by shipped code")
    w("")
    envs = {}
    for f in GO_PROD:
        for m in re.finditer(r"\"(OPENETL_[A-Z0-9_]+)\"", GO_SRC[f]):
            envs.setdefault(m.group(1), set()).add(f)
    w(md_table(["variable", "read in"], [(k, ", ".join(sorted(v))) for k, v in sorted(envs.items())]))
    w("")
    w("## CLI (live `--help` tree)")
    w("")
    w("Commands: " + ", ".join("`%s`" % " ".join(p) for p, n in sorted(nodes.items()) if p and not n.children) + ".")
    w("Global flags: " + ", ".join("`--%s`" % f for f in sorted(nodes[()].gflags) if f != "help") + ". Exit codes: `0` ok, `1` runtime/API error, `2` usage error.")
    w("Output: compact JSON on stdout; errors as `{\"error\":{...}}` on stderr.")
    w("")
    return "\n".join(L)


CLIREF_PATH = "docs/cli-reference.md"


def render_cli_reference(nodes):
    L = ["# CLI reference", "",
         "Generated from the live `openetl --help` tree by `python3 scripts/audit/audit.py --write-cli-reference`; `audit.py` fails when this",
         "page is stale, so every command and flag below exists and every real command and flag is listed. Workflows are in `skills/`, the",
         "import mapping in `docs/cli-import.md`, vocabulary (stages, reasons, enums) in `scripts/audit/vocab.md`.", "",
         "Conventions: output is one compact JSON document on stdout (`--output FILE` writes a file, `--page-count N` prints NDJSON, one page per line);",
         "errors are `{\"error\":{\"code\",\"status\",\"message\",\"details\"}}` on stderr. Exit codes: `0` ok, `1` runtime or API error, `2` usage error.",
         "`WF` is `WF-000123`, `wf-000123` or `projects/p/workflows/wf-000123`; `TK`, `CM`, `AT` likewise. Write commands accept `--dry-run`",
         "(`validate_only`), `--etag`, `--request-id`, `--note` and `--reason`.", "",
         "Environment: `OPENETL_BASE_URL`, `OPENETL_PROJECT`, `OPENETL_TOKEN` (wins over the saved login), `OPENETL_ALLOW_HTTP=1`, `OPENETL_SOURCE_PASSWORD`.", "",
         "## Global flags", "", "```"]
    root = nodes[()]
    gl = re.search(r"Flags:\n((?:  .*\n?)+)", root.help)
    L += [x for x in (gl.group(1).rstrip("\n").split("\n") if gl else [])]
    L += ["```", "", "## Commands", ""]
    for path, n in sorted(nodes.items()):
        if not path:
            continue
        if n.children:
            L.append("- `openetl %s` — %s" % (" ".join(path), ", ".join(sorted(n.children))))
    L.append("")
    for path, n in sorted(nodes.items()):
        if not path or n.children:
            continue
        text = n.help.split("\nGlobal Flags:")[0].rstrip("\n")
        L += ["### openetl %s" % " ".join(path), "", "```", text, "```", ""]
    return "\n".join(L)


def section_cli_reference(nodes):
    area = "cli-reference"
    text = render_cli_reference(nodes)
    if "--write-cli-reference" in sys.argv:
        with open(os.path.join(ROOT, CLIREF_PATH), "w", encoding="utf-8") as f:
            f.write(text)
        A.check(area, "wrote " + CLIREF_PATH, True)
        return
    if not exists(CLIREF_PATH):
        A.check(area, CLIREF_PATH + " exists", False, "run: python3 scripts/audit/audit.py --write-cli-reference")
        return
    cur = read(CLIREF_PATH)
    A.check(area, CLIREF_PATH + " equals the live --help tree", cur == text, "stale; regenerate with: python3 scripts/audit/audit.py --write-cli-reference")


def section_vocab_file(nodes):
    area = "vocab"
    text = render_vocab(nodes)
    if "--write-vocab" in sys.argv:
        with open(os.path.join(ROOT, VOCAB_PATH), "w", encoding="utf-8") as f:
            f.write(text)
        A.check(area, "wrote " + VOCAB_PATH, True)
        return
    if not exists(VOCAB_PATH):
        A.check(area, VOCAB_PATH + " exists", False, "run: python3 scripts/audit/audit.py --write-vocab")
        return
    cur = read(VOCAB_PATH)
    A.check(area, VOCAB_PATH + " equals the vocabulary extracted from the sources", cur == text,
            "stale; regenerate with: python3 scripts/audit/audit.py --write-vocab (first difference near: %s)" % next(
                (b for a_, b in zip(cur.split("\n"), text.split("\n")) if a_ != b), "end of file")[:100])
    # every stage key / reason key / view / status named in the vocabulary file is real (guards a hand-edited file)
    for k in STAGE_KEYS:
        A.check(area, "vocab lists stage " + k, "| %s |" % k in cur)
    for r in REASON_KEYS:
        A.check(area, "vocab lists reason " + r, "| %s |" % r in cur)
    for v in json.loads(read("internal/cli/views.json")):
        A.check(area, "vocab lists view " + v["name"], "`%s`" % v["name"] in cur)
    for st in GO_STATUS.values():
        A.check(area, "vocab lists status " + st, "| %s |" % st in cur)
    for r in GO_REASONS:
        A.check(area, "vocab lists error reason " + r, "`%s`" % r in cur)


# =========================================================================== accepted findings
def load_accepted():
    """scripts/audit/accepted.json: [{"match": "<substring of '[area] name'>", "why": "<reason>"}]  -- findings that cannot be fixed
    (e.g. checksummed migration files) are listed here with a reason; they are reported as ACCEPT, never silently dropped."""
    path = os.path.join(ROOT, "scripts/audit/accepted.json")
    if not os.path.exists(path):
        return []
    return json.load(open(path))



def main():
    if not build():
        return finish()
    root, nodes = build_tree()
    A.check("cli", "command tree discovered (%d nodes)" % len(nodes), len(nodes) > 30)
    section_openapi()
    section_migrations()
    section_enums()
    section_routes()
    section_env()
    section_paths()
    section_cli(root, nodes)
    section_mapping()
    section_sql()
    section_vocab_docs()
    section_misc()
    section_limits()
    section_vocab_file(nodes)
    section_cli_reference(nodes)
    return finish()


def finish():
    acc = load_accepted()
    for i, (st, area, name, detail) in enumerate(A.results):
        if st == "FAIL":
            for a_ in acc:
                if a_["match"] in "[%s] %s" % (area, name):
                    A.results[i] = ("ACCEPT", area, name, "accepted: " + a_["why"])
                    break
    by = {}
    for st, area, name, detail in A.results:
        by.setdefault(area, {"ok": 0, "FAIL": 0, "WARN": 0, "SKIP": 0, "ACCEPT": 0})[st] += 1
    fails = [r for r in A.results if r[0] == "FAIL" or (STRICT and r[0] == "WARN")]
    for st, area, name, detail in A.results:
        if st in ("FAIL", "WARN", "SKIP", "ACCEPT"):
            print("%-4s [%s] %s%s" % (st, area, name, ("\n       -> " + detail) if detail else ""))
    print()
    print("%-14s %6s %6s %6s %6s %6s" % ("area", "ok", "FAIL", "WARN", "SKIP", "ACCEPT"))
    tot = {"ok": 0, "FAIL": 0, "WARN": 0, "SKIP": 0, "ACCEPT": 0}
    for a in sorted(by):
        print("%-14s %6d %6d %6d %6d %6d" % (a, by[a]["ok"], by[a]["FAIL"], by[a]["WARN"], by[a]["SKIP"], by[a]["ACCEPT"]))
        for k in tot:
            tot[k] += by[a][k]
    print("%-14s %6d %6d %6d %6d %6d   (%d checks)" % ("TOTAL", tot["ok"], tot["FAIL"], tot["WARN"], tot["SKIP"], tot["ACCEPT"], sum(tot.values())))
    if "--json" in sys.argv:
        out = sys.argv[sys.argv.index("--json") + 1]
        with open(out, "w") as f:
            json.dump([{"status": s, "area": a, "name": n, "detail": d} for s, a, n, d in A.results], f, indent=1, ensure_ascii=False)
    print("RESULT:", "FAIL (%d failing checks)" % len(fails) if fails else "OK")
    try:
        import shutil
        shutil.rmtree(TMP, ignore_errors=True)
    except Exception:
        pass
    return 1 if fails else 0


if __name__ == "__main__":
    sys.exit(main())
