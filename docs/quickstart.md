# Quickstart

Needs Docker, Go 1.24 and python3. About 5 minutes. `scripts/e2e.sh` runs exactly this flow unattended and asserts the numbers.

## 1. Start the stack and build the CLI

```bash
docker compose up -d --build --wait server     # MySQL 8.0 + server (migrates on start, restricted query user)
go build -o bin/openetl ./cmd/openetl
curl -s localhost:18080/healthz                 # {"status":"ok"}
```

## 2. Tokens and login

Tokens are shown once; only their hash is stored.

```bash
mint() { docker compose exec -T server openetl-server admin create-token --project acme-dw --account "$1" --side "$2" --role "$3" 2>/dev/null | tail -1; }
ADMIN=$(mint alice delivery admin); EDT=$(mint bob delivery editor); CUS=$(mint carol customer editor)
export OPENETL_BASE_URL=http://127.0.0.1:18080 OPENETL_PROJECT=acme-dw PATH=$PWD/bin:$PATH
echo "$ADMIN" | openetl auth login --token-stdin   # stored encrypted on this machine
```

## 3. Import a migration list

```bash
python3 scripts/gen-dataset.py /tmp/oe-data          # 3000 workflows, ~9500 tasks (--small: 60 workflows)
J=$(openetl import stage --from /tmp/oe-data/rows.ndjson --map /tmp/oe-data/mapping.yaml | python3 -c "import sys,json;print(json.load(sys.stdin)['name'].split('/')[-1])")
openetl import diff "$J" --limit 3                   # read before you apply
openetl import apply "$J" --yes
```

## 4. Work the lifecycle

```bash
WF=$(openetl query "SELECT code FROM workflows ORDER BY id LIMIT 1" | python3 -c "import sys,json;print(json.load(sys.stdin)['rows'][0][0])")
for s in development deployment verify; do OPENETL_TOKEN=$EDT openetl stage advance $WF --stage $s; done
OPENETL_TOKEN=$CUS openetl stage advance $WF --stage accept            # only the customer side accepts
OPENETL_TOKEN=$CUS openetl stage regress $WF --stage accept --reason rej_row_count
OPENETL_TOKEN=$EDT openetl comment add $WF --kind issue --body "target schema permission missing"   # -> BLOCKED
openetl scope set WF-000002 --to out --reason obsolete                 # admin: out of scope
```

## 5. Management report

```bash
openetl query "SELECT stage, SUM(workflows) FROM stage_distribution GROUP BY stage"
openetl query "SELECT code, name, open_issues FROM workflows WHERE health='blocked'"
openetl query "SELECT DATE(event_time) d, COUNT(*) FROM events WHERE k='stage.accept' AND JSON_UNQUOTE(new_v)='pass' GROUP BY d"
openetl views                                                           # all queryable views and columns
```

## 6. Let an Agent do it

The CLI owns the token, so an Agent never sees it: log in once (step 2), then point the Agent at `skills/`.
Start with `skills/openetl-shared` (preflight: `openetl auth status`, base URL, project), then the task Skill that
matches the request (import, stage changes, comments, reporting). The Agent runs the same `openetl ...` commands
as above and reads their compact JSON; destructive steps (`import apply`, `rollback`) still need `--yes`.

Tear down: `docker compose down -v`.
