#!/usr/bin/env bash
# Migration discipline (rules: docs/operations.md, "Schema changes: expand / contract").
#   scripts/check-migrations.sh            verify frozen files against docs/migrations.sha256 + lint unfrozen ones
#   scripts/check-migrations.sh --freeze   after a release: append the unfrozen files to the manifest
# 1. Every file listed in the manifest was applied somewhere: it must be byte-identical and still exist.
# 2. A migration not in the manifest is new: it may only contain additive DDL (CREATE TABLE, CREATE [UNIQUE] INDEX,
#    ALTER TABLE .. ADD .., CREATE OR REPLACE VIEW, INSERT IGNORE, SET NAMES); DROP/RENAME/MODIFY/CHANGE/TRUNCATE/
#    DELETE/UPDATE and ALTER .. DROP are flagged. Destructive "contract" steps need a human: write them as a new
#    migration, add `-- contract-approved: <reason / ticket>` on its first line, and ship it a release after the code stopped using the object.
#   scripts/check-migrations.sh --apply    also prove, on a throwaway MySQL 8.0 container (oe-b-mig, 127.0.0.1:33321):
#       apply from every prefix (0001, 0001-0002, ...) + the real `openetl-server migrate` (run twice: 2nd is a no-op),
#       and that the server refuses a database that holds a migration newer than the binary.
# MIG_DIR / MANIFEST override the locations (used to test this script).
set -euo pipefail
ROOT=$(cd "$(dirname "$0")/.." && pwd); cd "$ROOT"
APPLY=0; [ "${1:-}" = "--apply" ] && { APPLY=1; set --; }
DIR=${MIG_DIR:-db/migrations}; MANIFEST=${MANIFEST:-docs/migrations.sha256}
[ -f "$MANIFEST" ] || { echo "FAIL: $MANIFEST missing" >&2; exit 1; }
rc=0
declare -A frozen
while read -r sum path; do
  [ -n "$sum" ] || continue
  frozen[$path]=1
  if [ ! -f "$path" ]; then echo "FAIL: $path is in the manifest but missing (applied migrations must never be deleted)"; rc=1; continue; fi
  got=$(sha256sum "$path" | cut -d' ' -f1)
  [ "$got" = "$sum" ] || { echo "FAIL: $path was edited after it was frozen (fix forward with a new migration)"; rc=1; }
done <"$MANIFEST"

new=()
for f in "$DIR"/*.sql; do [ -n "${frozen[$f]:-}" ] || new+=("$f"); done
# numbering: NNNN_name.sql, strictly increasing and gap-free
prev=0
for f in "$DIR"/*.sql; do
  b=$(basename "$f"); n=${b%%_*}
  [[ $n =~ ^[0-9]{4}$ ]] || { echo "FAIL: $b does not start with a 4-digit number"; rc=1; continue; }
  n=$((10#$n)); [ "$n" -eq $((prev + 1)) ] || { echo "FAIL: $b breaks the sequence (expected $(printf %04d $((prev + 1))))"; rc=1; }
  prev=$n
done

if [ ${#new[@]} -gt 0 ]; then
  python3 -I - "${new[@]}" <<'PY' || rc=1
import re, sys
OK = [r"CREATE TABLE", r"CREATE (UNIQUE )?INDEX", r"CREATE OR REPLACE VIEW", r"INSERT IGNORE", r"SET NAMES"]
BAD = re.compile(r"\b(DROP|RENAME|MODIFY|CHANGE|TRUNCATE|DELETE|UPDATE|REPLACE\s+INTO)\b", re.I)
def statements(text):
    text = re.sub(r"/\*.*?\*/", " ", text, flags=re.S)
    out, cur, q, i = [], [], None, 0
    while i < len(text):
        c = text[i]
        if q:
            cur.append(c)
            if c == "\\": cur.append(text[i+1]); i += 1
            elif c == q: q = None
        elif c in "'\"`": q = c; cur.append(c)
        elif c == "#" or text[i:i+3] == "-- ":
            while i < len(text) and text[i] != "\n": i += 1
            continue
        elif c == ";": out.append("".join(cur).strip()); cur = []
        else: cur.append(c)
        i += 1
    out.append("".join(cur).strip())
    return [s for s in out if s]
def strip_strings(s): return re.sub(r"'(?:[^'\\]|\\.|'')*'|\"(?:[^\"\\]|\\.)*\"", "''", s)
bad = 0
for path in sys.argv[1:]:
    text = open(path, encoding="utf-8").read()
    approved = text.lstrip().startswith("-- contract-approved:")
    for n, st in enumerate(statements(text), 1):
        flat = " ".join(strip_strings(st).split())
        why = None
        m = re.match(r"ALTER TABLE \S+ (.*)", flat, re.I | re.S)
        if m:
            parts = re.split(r",\s*(?=ADD\b|DROP\b|MODIFY\b|CHANGE\b|RENAME\b|ALTER\b)", m.group(1), flags=re.I)
            if not all(re.match(r"ADD\b", p, re.I) for p in parts): why = "ALTER TABLE with a non-ADD clause"
        elif not any(re.match(p, flat, re.I) for p in OK): why = "statement is not additive DDL"
        elif BAD.search(flat) and not re.match(r"CREATE OR REPLACE VIEW|CREATE TABLE|INSERT IGNORE", flat, re.I): why = "destructive keyword"
        elif re.match(r"CREATE TABLE", flat, re.I) and re.search(r"\bDROP\b", flat, re.I): why = "DROP inside CREATE"
        if why and not approved:
            print(f"FAIL: {path} statement {n}: {why}: {flat[:90]}"); bad += 1
sys.exit(1 if bad else 0)
PY
  if [ "${1:-}" = "--freeze" ]; then
    [ $rc -eq 0 ] || { echo "not freezing: fix the failures first" >&2; exit 1; }
    for f in "${new[@]}"; do sha256sum "$f" >>"$MANIFEST"; echo "frozen $f"; done
  else
    echo "note: unfrozen (new) migration(s) linted: ${new[*]##*/} -- run '$0 --freeze' once released"
  fi
fi
[ $rc -eq 0 ] && echo "migrations OK (${#frozen[@]} frozen, ${#new[@]} new)"

if [ "$APPLY" = 1 ] && [ $rc -eq 0 ]; then
  C=oe-b-mig; PORT=${MIG_PORT:-33321}; W=$(mktemp -d); BIN=$W/openetl-server
  trap 'docker rm -f "$C" >/dev/null 2>&1 || true; rm -r "$W"' EXIT
  go build -o "$BIN" ./cmd/openetl-server
  docker rm -f "$C" >/dev/null 2>&1 || true
  docker run -d --name "$C" -e MYSQL_ALLOW_EMPTY_PASSWORD=yes -p 127.0.0.1:$PORT:3306 mysql:8.0 --character-set-server=utf8mb4 >/dev/null
  # TCP ping, not the socket: the image first runs a socket-only bootstrap server that then restarts
  for _ in $(seq 90); do docker exec "$C" mysqladmin -uroot -h127.0.0.1 ping >/dev/null 2>&1 && break; sleep 2; done
  sleep 2; docker exec "$C" mysqladmin -uroot -h127.0.0.1 ping >/dev/null
  sql() { docker exec -i "$C" mysql -uroot "$@"; }
  sql -e "CREATE USER 'oe'@'%' IDENTIFIED WITH mysql_native_password BY 'oe'; GRANT ALL ON *.* TO 'oe'@'%'"
  mapfile -t FILES < <(ls "$DIR"/*.sql | sort)
  dsn() { echo "oe:oe@tcp(127.0.0.1:$PORT)/$1?parseTime=true&multiStatements=true"; }
  for k in $(seq 0 ${#FILES[@]}); do        # k = number of migrations already applied (0 = fresh database)
    d=mig_prefix_$k; sql -e "DROP DATABASE IF EXISTS $d; CREATE DATABASE $d CHARACTER SET utf8mb4"
    if [ "$k" -gt 0 ]; then
      sql -e "CREATE TABLE $d.schema_migrations (version VARCHAR(64) PRIMARY KEY, applied_at DATETIME(6) NOT NULL)"
      for f in "${FILES[@]:0:$k}"; do sql "$d" <"$f"; sql -e "INSERT INTO $d.schema_migrations VALUES ('$(basename "$f")', NOW(6))"; done
    fi
    OPENETL_DB_DSN=$(dsn $d) "$BIN" migrate >"$W/out" || { cat "$W/out"; echo "FAIL: migrate from prefix $k"; exit 1; }
    OPENETL_DB_DSN=$(dsn $d) "$BIN" migrate | grep -q "up to date" || { echo "FAIL: second migrate from prefix $k not a no-op"; exit 1; }
    n=$(sql -N -e "SELECT COUNT(*) FROM $d.schema_migrations"); [ "$n" = "${#FILES[@]}" ] || { echo "FAIL: prefix $k ended with $n versions"; exit 1; }
    echo "ok   prefix $k -> head ($n migrations), second run no-op"
  done
  # NOTE: re-running the raw SQL files on a migrated schema is NOT supported (ADD COLUMN has no IF NOT EXISTS in
  # MySQL, e.g. 0003 fails with 1060); idempotence comes from schema_migrations. See docs/backlog/B.md (B-1).
  # downgrade guard
  sql -e "INSERT INTO mig_prefix_0.schema_migrations VALUES ('9999_from_the_future.sql', NOW(6))"
  if OPENETL_DB_DSN=$(dsn mig_prefix_0) "$BIN" migrate 2>"$W/err"; then echo "FAIL: migrate accepted a newer database"; exit 1; fi
  grep -q "does not know" "$W/err" || { cat "$W/err"; echo "FAIL: unexpected refusal text"; exit 1; }
  echo "ok   binary refuses a database with newer migrations: $(cat "$W/err")"
    echo "migration apply checks OK"
fi
exit $rc
