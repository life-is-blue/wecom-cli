# openetl CLI: batch import

Import is a three-step governance flow: **stage** (nothing changes), **diff** (read and decide), **apply**
(admin, confirmed). A whole applied job can be rolled back with `import rollback`.

```bash
openetl import stage --from mysql://readonly@host:3306/legacy --map mapping.yaml   # -> job 17
openetl import show 17
openetl import diff 17 --limit 10
openetl import apply 17            # TTY: asks; non-interactive: needs --yes
openetl import rollback 17 --yes
```

## Commands

| Command | Meaning |
| --- | --- |
| `import stage --from SRC --map FILE [--source-system NAME] [--format csv\|ndjson] [--max-rows N] [--request-id UUID] [--dry-run]` | Read the source, map every row, `POST /importJobs`. `--dry-run` reads and maps locally and prints row count plus a sample; nothing is sent. |
| `import show JOB` | The job (`state`, `summary`) plus `total` and `needs_review` (CONFLICT + SUSPECT > 0). |
| `import diff JOB [--limit N=10] [--action CLASS]...` | Agent-readable report: `counts`, and per class `{count, rows[first N], truncated}`. Classes in review order: CONFLICT, SUSPECT, CHANGED, NEW, MISSING (UNCHANGED only with `--action UNCHANGED`). One filtered `:diff` call per non-empty class. |
| `import diff JOB --raw [--filter] [--page-size] [--page-count]` | The server's diff pages unchanged (NDJSON with `--page-count`). |
| `import apply JOB`, `import rollback JOB` | Admin. `--dry-run` sends `validate_only` and needs no confirmation. Otherwise a TTY gets a `[y/N]` prompt (shows the class counts); without a TTY `--yes` is required (exit 2 otherwise). `--yes` on a job with CONFLICT/SUSPECT rows prints a warning on stderr. The command waits (`--wait 10m`, `--no-wait` to skip) until the job leaves the running state and prints the final Operation; a FAILED job exits 1. |

`JOB` is `17` or `projects/acme-dw/importJobs/17`. Output is compact JSON; `--output FILE` writes it to a file.
Exit codes: 0 ok, 1 runtime/API error, 2 usage error.

## Sources (`--from`)

| Value | Reader |
| --- | --- |
| `rows.ndjson` (any extension except `.csv`) | One JSON object per line; nested values allowed (needed for `tasks`). |
| `rows.csv` | Header row + flat records (UTF-8, BOM skipped). Every value is a string. |
| `-` | NDJSON on stdin (`--format csv` for CSV). |
| `mysql://user[:password]@host[:port]/db` | Read-only scan (`START TRANSACTION READ ONLY`, a single `SELECT`). The table or query comes from the mapping. The password comes from the DSN, otherwise from `OPENETL_SOURCE_PASSWORD`; it is never printed, logged or sent to the gateway (the job's `source` label is the DSN without password unless `source_system` is set). `?tls=true` is passed to the driver. |

## mapping.yaml

```yaml
version: 1                       # optional
source_system: legacy-pipeline   # label of the job; --source-system overrides; default: the source (no credentials)

source:                          # mysql:// only; exactly one of table / query
  table: pipeline                #   SELECT * FROM `pipeline` [ORDER BY `order_by`]
  order_by: id
  # query: "SELECT p.id, p.name, ... FROM pipeline p WHERE p.retired = 0"   # a single SELECT/WITH only

reference: [id]                  # required. Column(s) identifying the row -> source.reference (joined with ":"). Must be unique.
hash_exclude: [updated_at]       # columns left out of source.row_hash (volatile columns)

fields:                          # standard row fields
  name: pipeline_name            # required -> display_name
  domain: dom                    # -> labels.domain
  area: {column: area, default: unknown}
  application: app
  priority: prio
  category: {const: etl}         # a constant instead of a column

labels:      {team: team_col}    # extra labels (key -> field)
annotations: {remark: note_col}  # free text
assignees:   {dev_owner: dev_col, customer_pic: cust_col}   # value "alice" -> accounts/alice
stages:      {development: dev_status, verify: ver_status}  # -> stages[{stage,state}]
stage_values: {finished: pass}   # extra spellings for stage columns

tasks:                           # optional nested tasks
  from: tasks_json               # column / NDJSON key holding a JSON array of objects
  name: title                    # key of the task name inside each element (default "name")
  labels: {kind: type}           # task label -> element key
```

A *field* is a bare column name, or `{column: c, default: d}`, or `{const: v}`. Values are trimmed; empty or NULL
values are omitted (unless `default` is set). Label values are lower-cased (the API requires lowercase).
Unknown keys under `fields` or at the top level are errors, as are a missing `name` or an empty/duplicate `reference`
(the error names the source row; at most 10 are reported).

Stage values (case-insensitive): `pass/passed/done/yes/true/1` -> PASSED, `open/no/false/0` -> OPEN,
`na/n/a/not_applicable` -> NOT_APPLICABLE; anything else must be listed in `stage_values`. An empty value skips the stage.

### Row hash

`source.row_hash` = hex SHA-256 of the canonical JSON object `{column: string|null}` over **all** columns of the
source row except `hash_exclude`, keys sorted, values rendered as strings (numbers keep their text form, NULL stays
`null` and differs from `""`, nested JSON is re-encoded with sorted keys). It does not depend on column order or on
whether the row came from CSV, NDJSON or MySQL, so the same data staged twice hashes identically. Rows are sent
sorted by `source.reference`.

### Tasks

`ImportRow.data` is a `Workflow`, which has no task list. Until the gateway defines one, tasks are carried as the
annotation `tasks`: a compact JSON array `[{"display_name": "...", "labels": {...}}]`.

## Row limits

`POST /importJobs` stages the whole job in one request; there is no "append rows" call. The CLI therefore sends one
request and refuses sources larger than `--max-rows` (default 100000). Stage bigger sources in slices
(`source.query` with a range) as separate jobs.
