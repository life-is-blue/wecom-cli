# Backlog D: consistency and verification

Items found by `scripts/audit/audit.py` and `scripts/reconcile.py`. Effort: S under half a day, M one to two days, L longer.
Evidence lines are rerunnable with `python3 scripts/audit/audit.py` unless stated.

1. **Query page size: contract says 10000, server clamps to 1000** (S)
   **Fixed: OpenAPI maximum, `openetl query --help` and the docs say 1000 (test `TestQueryFlags`).**
   Evidence: `[limits]` checks; `openetl query "SELECT event_id FROM events" --page-size 10000` returns 1000 rows plus a token;
   OpenAPI `QueryRequest.page_size.maximum` and `openetl query --help` say 10000; `internal/store/query.go` `queryMaxPS = 1000`; `docs/security.md` says 1000.
   Fix: set the OpenAPI maximum and the CLI help to 1000 (or raise the constant after measuring the 8 MiB result cap), regenerate `internal/api`, run `audit.py --write-cli-reference`.

2. **`as_of` on the query endpoint is advertised and always refused** (S)
   **Fixed: `as_of` is gone from `QueryRequest` and `openetl query` has no `--as-of`; a body that still sends it is refused with `AS_OF_UNSUPPORTED` (`gw` test); events keep `as_of`.**
   Evidence: `[limits]` checks; the handler answers `AS_OF_UNSUPPORTED` while `QueryRequest.as_of` and `openetl query --as-of` exist.
   Fix: drop the property and the flag until the feature exists (events keep `as_of`), or implement it; either way the audit goes green.

3. **Lowercase enum values in the contract** (S)
   **Fixed: `KIND_UNSPECIFIED, SOURCE, FILL, CONFLICT` in the contract, server and CLI output; `docs/import-format.md` and `docs/cli-import.md` updated.**
   Evidence: `DiffImportJobResponse.rows[].changes[].kind` is `source|fill|conflict`, violating the UPPER_SNAKE-with-`*_UNSPECIFIED` rule of `docs/m0-naming-spec.md`.
   Fix: `KIND_UNSPECIFIED, SOURCE, FILL, CONFLICT` in the contract and in the importer's output (a visible change for CLI consumers: update `docs/import-format.md`).

4. **Reason named in the contract that no code raises** (S)
   **Fixed: the server does raise it (`workflow_id`/`task_id` query parameters, any value); the audit's extractor missed the `invalid("REASON", ...)` helper and now reads it. Contract, ADR 0009, `docs/m0-naming-spec.md` and `openetl workflow create --help` say "reserved, rejected".**
   Evidence: `CLIENT_ID_UNSUPPORTED` in `api/openapi.yaml`.
   Fix: implement the check or remove the name; keep one list of reasons (`vocab.md` is generated from the code).

5. **Uniform liveness in the public views** (M, needs a new migration)
   **Fixed (migration 0006, INV-28): `workflow_stages`, `open_items` and `comments` join `v_workflow_base`; `reconcile.py` expects no rows for hidden workflows.**
   Evidence: reconcile notes on both datasets: `workflow_stages` exposes 18 and 36 rows of soft-deleted workflows, `open_items` 3 and 2 rows. Reproduce:
   create a workflow, advance `development`, `openetl workflow delete`, then `SELECT stage_key, status FROM workflow_stages WHERE workflow_id = <id>` still returns six rows.
   Management queries such as "workflows past verification" over `workflow_stages` therefore disagree with `workflows`.
   Fix: decide in the spec (ADR 0006): either filter deleted workflows in `v_stage_p` and `v_open_items_p` (join `v_workflow_base`), or document the exposure and add a `deleted` column. Add the choice to `scripts/reconcile.py` (currently reports the exposure as a note).

6. **`open_items` rows with all zeros** (S)
   **Fixed (migration 0006): `v_open_items` has `HAVING SUM(kind IN ('issue','risk','question')) > 0`.**
   Evidence: a workflow whose only open comments are notes gets a row `(0, 0, 0)`; the view is described as "unresolved issues, risks and questions".
   Fix: add `HAVING SUM(kind IN ('issue','risk','question')) > 0`, or describe the view as "workflows with any open comment".

7. **`tasks.primary_workflow_id` is arbitrary with two primaries** (S)
   **Superseded (migration 0007, INV-27): a task has exactly one workflow (key `workflow`, view column `tasks.workflow_id`); there are no links and no primary flag any more.**
   Evidence: dataset (b), task 490 has `in.486 = in.487 = primary`; the subselect uses `LIMIT 1` without `ORDER BY`.
   Fix: `ORDER BY CAST(SUBSTRING(k, 4) AS UNSIGNED) LIMIT 1`, and make import and legacy writers reject or normalise a second primary.

8. **Views depend on the migrating connection's collation** (M)
   **Fixed (migration 0006): every literal-derived string column carries an explicit `COLLATE utf8mb4_unicode_ci`; test `TestViewsAreCollationSafe` queries every public view under four connection collations.**
   Evidence: views store string literals with the collation of the connection that created them (`utf8mb4_0900_ai_ci`); `mysql --default-character-set=utf8mb4`
   (collation `utf8mb4_general_ci`) gets `ERROR 1267 Illegal mix of collations` on `SELECT ... FROM workflows WHERE health = 'blocked'`. The Go server works because its connections match.
   Risk: a restricted query user or a BI tool with another collation breaks on `health`, `scope`, `kind` comparisons.
   Fix: put the collation in `OPENETL_QUERY_DSN` guidance (`collation=utf8mb4_0900_ai_ci`) and in the `create-query-user` doc, or add `COLLATE` to the literals in a new migration; add a test that queries `health = 'blocked'` over a `general_ci` connection.

9. **`scripts/demo.sh` idempotency step passes vacuously** (S)
   Evidence: lines 61-62 use `--reason not_needed` (not a seeded reason); both calls fail with `UNKNOWN_REASON`, both `jget` print `None`.
   Fix: use `--reason obsolete`, and assert that the etag is not `None`. `audit.py` reports it under `[docs-vocab]`.

10. **`UNIMPLEMENTED` is in the contract but never raised** (S)
    Evidence: `[limits]` WARN; `core.StatusUnimplemented` is only mapped to HTTP 501.
    Fix: keep it as a documented reserved status (and say so in OpenAPI) or remove it; the Skills no longer rely on it.

11. **ADR 0008 names `--show-sql`, which does not exist** (S)
   **Fixed: the ADR names `scripts/check-migrations.sh` as the dry run and says `migrate` has no print-only flag.**
    Evidence: `[cli]` check on `docs/adr/0008-schema-migration-discipline.md:50`. Fix: implement `openetl-server migrate --show-sql` or mark it "planned" in the ADR.

12. **Run the audit and the reconcile in CI and in lefthook** (S)
    Evidence: the audit finds drift within minutes of a parallel edit (seen several times while it was written).
    Fix: `python3 scripts/audit/audit.py` as a pre-push step (about 5 s without a database, 15 s with one); `scripts/audit/run.sh all` nightly against the compose MySQL. Regenerate `vocab.md` and `docs/cli-reference.md` in the same commit as any CLI or seed change.

13. **Reconcile at scale and over time** (M)
    Evidence: the runs cover 60 and 164 workflows; `events` is compared through 1000-row pages.
    Fix: run `reconcile.py` after the 3000-workflow e2e; add a second pass after a restart and after `rebuild-latest`; when `as_of` returns, compare time-travel reads with a fold bounded by `create_time`.

14. **Spec the event ordering rule** (S)
    Evidence: nothing states that the current value is decided by event id (recording order) rather than `event_time`; a back-dated import event with a higher id wins over a newer human edit. `reconcile.py` encodes the id rule.
    Fix: one paragraph in ADR 0009 or `docs/m0-naming-spec.md`, plus a test with a back-dated import event.

15. **Policy for comment-only fixes in frozen migrations** (S)
    Evidence: two `ACCEPT`ed findings (`0003` header, `0004` binary name) cannot be fixed without breaking `docs/migrations.sha256`.
    Fix: either allow comment-only edits with a re-signed manifest and a note in ADR 0008, or keep accepting them in `scripts/audit/accepted.json`.

16. **Audit without PyYAML** (S)
    Evidence: the OpenAPI structure checks (about 750) are skipped when `import yaml` fails.
    Fix: vendor a 150-line subset parser or add PyYAML to the dev requirements; CI should fail on `SKIP`.
