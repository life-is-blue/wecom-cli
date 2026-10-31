# Consistency report

Mechanical cross-check of every artefact that carries vocabulary, plus an independent reconciliation of the public views
against the raw event log. Nothing here was verified by reading: every number below comes from `scripts/audit/audit.py` or
`scripts/reconcile.py`.

## Rerun

```bash
python3 scripts/audit/audit.py                  # exit 0 only if no check fails; WARN/SKIP/ACCEPT are printed, never silent
python3 scripts/audit/audit.py --strict         # WARN also fails
python3 scripts/audit/audit.py --write-vocab --write-cli-reference   # regenerate scripts/audit/vocab.md and docs/cli-reference.md
scripts/audit/run.sh all                        # reconcile: lifecycle dataset, adversarial dataset, reconcile self-test (needs MySQL 8)
```

Environment of the audit (all optional): `AUDIT_MYSQL` (default `mysql -h127.0.0.1 -P33307 -uoe -poe`), `AUDIT_DB` (migrated scratch
database, default `openetl_d`; used for `EXPLAIN` of every SQL snippet and for the live view columns; read-only), `AUDIT_PROBE` +
`AUDIT_TOKEN` + `AUDIT_PROJECT` (a running server on a scratch database: every OpenAPI operation is sent with an invalid body and must
not answer `route not found`). Without the database or the server those checks are reported as `SKIP`, not silently passed.
`go`, `python3` and (for the database checks) a `mysql` client are required; PyYAML is used for the OpenAPI structure checks when installed
(otherwise those checks are skipped, loudly). The audit builds the CLI and `scripts/audit/guardcheck` into a temp dir and never contacts a
server except for the optional probe; documented commands are checked with `--help` appended, which cobra honours after parsing flags.

`scripts/audit/accepted.json` lists findings that cannot be fixed (checksummed migration files) with the reason; they are printed as `ACCEPT`.

## What is checked (2466 checks at the time of writing)

| Area | What is extracted and cross-checked |
| --- | --- |
| `openapi` | operationId uniqueness and camelCase; every enum UPPER_SNAKE with a leading `*_UNSPECIFIED`; snake_case fields and query parameters; collections and custom verbs lowerCamel; `$ref`s resolve; `required` names exist; resource-name examples are AIP-shaped; verbs and collections named in `docs/m0-naming-spec.md` exist; `Error.status` enum equals the Go `Status*` constants; every reason named in the contract is raised in Go; `internal/api/types.gen.go` carries every enum value |
| `routes` | every path segment and custom verb of the contract appears as a literal in the server router and vice versa; live probe of every operation (route exists) |
| `sql` | migration numbering and header comments; seeds parsed (stages, reasons, rules): ENUM membership, a `pass` and a `reject` rule per stage, `merged` is a discard reason; public views in the migrations == Go `PublicViews` == `views.json`; `views.json` columns == live view columns; `openetl views` prints `views.json`; `views.json` notes list the real values |
| `enums` | OpenAPI enums against the SQL ENUMs and Go constants: side, member side, role, account kind, import state, reason kind and owner side, rule side and kind, comment kind, channel, health, scope, stage state, import action |
| `env` | every `OPENETL_*` literal read by shipped Go code is documented; every documented one is read by code, tests or scripts; compose variables are read by the server; CLI help names only real variables |
| `paths` | every file or directory named in a doc or README exists; README layout lists every top-level directory; binary names exist under `cmd/`; Skill frontmatter and cross references |
| `cli` | the full `--help` tree is parsed recursively; every `openetl ...` line in docs, Skills, README and scripts (fenced blocks, inline code, `oe_as`/`as_dev` helpers) must resolve to a real command, use real flags, and have the right number of arguments; each is also dry-parsed by the real CLI (`--help` appended); every real leaf command and flag appears in some doc; `openetl-server` subcommands and flags; behaviours the Skills promise (`--version` text, `auth status` exit 1, usage errors exit 2, `--max-rows` and wait defaults) |
| `import` | `gen-dataset.py` output is accepted by `import stage --dry-run`; the JSON example of `docs/import-format.md` fits the OpenAPI schemas |
| `sql-snippets` | every `SELECT` in docs, Skills, README, the OpenAPI examples and the query calls of the scripts passes the real query guard (`scripts/audit/guardcheck`, which calls `store.GuardQuery`) and then `EXPLAIN`s on MySQL after the project rewrite, so view and column names are authoritative |
| `docs-vocab` | stage keys (`--stage`, `stage.<k>`), Chinese stage names, stage chains, `--reason` values and `rej_*`/`na_*` keys against the seeds; UPPER_SNAKE tokens in user-facing docs are real statuses, reasons or enum values; `STATUS/REASON` pairs and `STATUS (code)` claims match the code and the HTTP mapping; "N public views" claims; health, scope, kind and event-key literals in SQL |
| `limits` | defaults and limits that appear in prose, OpenAPI, CLI help and Go constants agree (query caps, pool size, storage quota, page size, 100 MiB attachment cap); contract features the server refuses; statuses never raised |
| `misc` | go.mod vs Dockerfile vs docs; sizes quoted in README and quickstart vs `gen-dataset.py`; scripts reference scripts that exist; Skill `--filter` fields |
| `vocab`, `cli-reference` | `scripts/audit/vocab.md` and `docs/cli-reference.md` are generated from the sources; the audit fails when they are stale |

## Findings

"Reported" = code, spec, SQL or someone else's document; the owner decides. Details and fixes: `docs/backlog/D.md`.

| # | Where | What | Status |
| --- | --- | --- | --- |
| 1 | `api/openapi.yaml` QueryRequest.page_size, `openetl query --help` | promise a maximum of 10000; `internal/store/query.go` `queryMaxPS` is 1000 and silently clamps (`docs/security.md` says 1000). Reproduce: `openetl query "SELECT event_id FROM events" --page-size 10000` returns 1000 rows and a page token | fixed |
| 2 | OpenAPI QueryRequest.as_of, `openetl query --as-of` | advertised, but the server answers `AS_OF_UNSUPPORTED` to every request that sets it | fixed |
| 3 | OpenAPI `DiffImportJobResponse...changes[].kind` | enum values `source`, `fill`, `conflict` break the UPPER_SNAKE enum rule of `docs/m0-naming-spec.md` | fixed |
| 4 | OpenAPI | names the reason `CLIENT_ID_UNSUPPORTED`, which no code raises | fixed (audit extractor missed the `invalid()` helper; contract and docs say "reserved, rejected") |
| 5 | OpenAPI `ImportAction` | lacked the `TASK_*` classes that `docs/import-format.md` and the CLI use (first run); fixed in the meantime by the contract owner, now green | fixed by C |
| 6 | `core.StatusUnimplemented` / `UNIMPLEMENTED` | in the Error.status enum and in two Skill passages, but never raised by the server | Skill text fixed, enum reported |
| 7 | `db/migrations/0003_import_job.sql` | header comment says "openetl 0004" | accepted (checksummed, `docs/migrations.sha256`) |
| 8 | `db/migrations/0004_entity_latest.sql` | comment names the former binary `openetl-gateway` | accepted (checksummed) |
| 9 | `scripts/demo.sh:61-62` | the idempotency demo uses `--reason not_needed`, which is not a seeded reason: both calls fail with `UNKNOWN_REASON`, both `jget` print `None`, so "same request-id returned the same result" passes vacuously | reported |
| 10 | `docs/adr/0008-schema-migration-discipline.md:50` | mentions `openetl-server ... --show-sql`, a flag that does not exist (planned) | fixed |
| 11 | Skills `openetl-import`, `openetl-workflow` | told the Agent that `UNIMPLEMENTED` (501) means import or attachments are not deployed; both are implemented | fixed |
| 12 | Skills | no task or workflow create/update/delete commands anywhere in the docs | fixed (flow 7 in `openetl-workflow`) |
| 13 | README / docs | 13 environment variables read by the code were undocumented (`OPENETL_ADDR`, `OPENETL_STORAGE*`, `OPENETL_S3_*`, ...) | fixed (README table; audit keeps it complete) |
| 14 | README Layout | missing `internal/` and `scripts/` | fixed |
| 15 | docs | 19 CLI commands and about 60 flags documented nowhere | fixed (generated `docs/cli-reference.md`, kept current by the audit) |
| 16 | `docs/m0-naming-spec.md` | pointed to a non-existent `./contract.txt`; "100MB" cap (also `docs/design.md`) is 100 MiB = 104857600 bytes | fixed |
| 17 | `workflow_stages`, `open_items` | expose rows of soft-deleted workflows (and entities without any event) that `workflows` hides | fixed (migration 0006) |
| 18 | `open_items` | has a row with all counts 0 for a workflow whose only open comments are notes, although the view is documented as "unresolved issues, risks and questions" | fixed (migration 0006) |
| 19 | `tasks.primary_workflow_id` | `SELECT ... LIMIT 1` without `ORDER BY` over the task's `in.<id> = primary` keys: arbitrary when legacy or imported data carries two primaries | fixed (migration 0006 and `DUPLICATE_PRIMARY`), then removed with the multi-link model: a task has one workflow (migration 0007, `tasks.workflow_id`) |
| 20 | views vs connection collation | literals in the views take the collation of the connection that ran the migration (`utf8mb4_0900_ai_ci`); a client connecting with `utf8mb4_general_ci` (for example `mysql --default-character-set=utf8mb4`) gets error 1267 on `WHERE health = 'blocked'` | fixed (migration 0006, explicit collations; store test) |

By area: OpenAPI/contract 6 (1 fixed by its owner), SQL 3 (2 accepted), scripts 1, Skills 2 fixed, README/docs 5 (all fixed except ADR 0008), views 3 reported, one environment fragility.

## Reconciliation of the views (`scripts/reconcile.py`)

A second implementation written from the specification and the raw tables only (`entity`, `event`, `delta`, plus `stage_def`): it folds
the log in Python (latest value per key by event id, soft delete, scope, stage states, current stage, open comments, health, assignees,
labels, task links), asks the server for the public views through the query endpoint, and compares row by row. It also checks, from
the raw tables alone, that `entity_latest` equals the fold of `delta`, that `delta.entity_id` equals `event.entity_id`, and that every
`old_v` equals the previous `new_v` of its key. `scripts/audit/run.sh` builds the databases, starts and stops the server (port 18204).

| Dataset | Database | Raw size | Rows compared | Mismatches |
| --- | --- | --- | --- | --- |
| (a) `gen-dataset.py --small` imported through the CLI, then `scripts/audit/lifecycle.py` (210 CLI calls: stages, regressions by both sides, comments of every kind with resolve/reopen/delete, merge chain 12 to 13 to 14 with 13 coming back, out of scope and back, soft delete and restore, label replacement, unicode names, tasks created, shared, re-pointed, detached, deleted) | `openetl_d` | 330 entities, 485 events, 2996 deltas | 12609 | 0 |
| (b) `scripts/audit/gen-adversarial.py` (seed 1), loaded as raw rows, then `admin rebuild-latest` | `openetl_d_adv` | 604 entities, 1091 events, 3178 deltas | 14290 | 0 |

Dataset (b) is built for awkward shapes: 120 random histories (labels with empty values, 0 to 18 tasks) plus hand-written cases: workflows with 0 and with 40
tasks; 13 unicode and very long names (CJK, emoji with ZWJ, RTL override, combining marks, 3000-character name, newline, quotes, SQL text);
four workflows with the same display name; empty and removed labels and assignees, re-added; a workflow with no name and one entity
without any event; a task linked to three workflows with its primary moving and then detached; a task with two primaries; merge chains m1 to m2 to m3 to m4 with the middle one
coming back in and a merge into a soft-deleted target; out and back in three times; scope `in` with a stale `into`; stages regressed five rounds, `na`
stages, a removed stage value, an unknown stage key, a wrong-case stage value; eight stage flips in the same microsecond and back-dated events whose
ids are higher than their business time suggests; corrections of events and of corrections (`corrects_event_id`, import and rollback keys); delete,
restore, delete again, and the `deleted` key removed; a comment zoo (every kind resolved, reopened, deleted, undeleted, flag removed), a workflow with 50 issues,
a comment pointing at a task id, comments on deleted and merged workflows; a second project and an attachment entity that must never leak.

Result: no mismatch between the views and the independent fold on either dataset. Classification of everything the comparison surfaced:

* **Liveness of the derived views** (findings 17 and 18, fixed by migration 0006): the first runs found `workflow_stages` and `open_items` exposing
  soft-deleted workflows (18 rows of 3 workflows in (a), 36 rows of 6 in (b)) and `open_items` rows with all counts 0. `reconcile.py` now expects
  no row of a hidden workflow in `workflow_stages`, `open_items` and `comments`, and an `open_items` row only for a workflow with an open issue, risk or
  question; with the old views it reports 41 mismatches on dataset (b), with migration 0006 none.
* **Tasks with two primaries** (finding 19, fixed): task 490 of dataset (b) has two primary links; the view now answers the lowest id (486) by definition and
  `reconcile.py` requires it. The API refuses a second primary (`DUPLICATE_PRIMARY`, INV-27); only legacy or import data can carry the shape.
* **Reconcile bugs**: none surfaced; every run ended with zero mismatches, so the comparison logic was validated by the self-test below rather than by disagreements.
* **Sensitivity**: `scripts/audit/run.sh selftest` corrupts the cache five ways (name rewritten, delete marker lost, passed stage turned open, comment
  unresolved, primary link downgraded) and requires `reconcile.py` to report each; all five are detected.
* No view or store bug (a view that disagrees with the event log) was found on either dataset.

## Files

`scripts/audit/audit.py`, `scripts/audit/guardcheck/main.go`, `scripts/audit/accepted.json`, `scripts/audit/vocab.md` (generated),
`scripts/audit/gen-adversarial.py`, `scripts/audit/lifecycle.py`, `scripts/audit/run.sh`, `scripts/reconcile.py`, `docs/cli-reference.md` (generated),
this report, `docs/backlog/D.md`. Edited: `README.md`, `docs/m0-naming-spec.md`, `docs/design.md`, `skills/openetl-import/SKILL.md`, `skills/openetl-workflow/SKILL.md`.
