# References

Primary sources consulted for the foundation ADRs (`docs/adr/`). Read on 2026-10-31.
"Verified" means the quoted text or identifier was read in the source itself, not in a summary.
Airflow source was read from `raw.githubusercontent.com/apache/airflow/main/...` (branch `main`, 2026-10-31; the commit sha
could not be retrieved because the GitHub API is not reachable from the research sandbox). Line numbers in ADRs refer to that copy
and will drift.

## BigQuery

| # | URL | What was read | Status |
| --- | --- | --- | --- |
| B1 | https://bigquery.googleapis.com/discovery/v1/apis/bigquery/v2/rest (revision `20261009`) | Machine-readable v2 REST description: schemas `Job`, `JobStatus`, `JobReference`, `QueryRequest`, `QueryResponse`, `ErrorProto`, `JobCreationReason`; methods `jobs.insert/get/getQueryResults/list/query/cancel/delete` (paths, descriptions, parameters) | Verified |
| B2 | https://docs.cloud.google.com/bigquery/docs/reference/rest/v2/Job (and `jobs/insert`, `jobs/getQueryResults`) | HTML pages: only navigation was returned (page text >100k chars, truncated). Content taken from B1 instead | Not used; B1 is the source |
| B3 | https://docs.cloud.google.com/bigquery/docs/information-schema-intro | `INFORMATION_SCHEMA` syntax (region/dataset qualifier), read-only, limitations, pricing | Verified (2nd page of text via offset) |
| B4 | https://docs.cloud.google.com/bigquery/docs/information-schema-jobs | `JOBS` view: 180-day history, region qualifier, columns `job_id`, `state`, `error_result`, `creation_time` | Verified |
| B5 | https://docs.cloud.google.com/bigquery/docs/time-travel | Time travel window (2-7 days, default 7), fail-safe 7 days, unsupported table types | Verified |
| B6 | Dremel / BigQuery papers | Not read. Not needed: nothing in the decisions depends on the storage or execution engine | Not consulted |
| B7 | `jobs.insert` behaviour for a duplicate client-supplied `jobId` (`ALREADY_EXISTS`) | Not found in B1 (the `insert` description does not mention it) and the HTML page was truncated | **Unverified** |

## Airflow (apache/airflow, `main`)

| # | URL | What was read | Status |
| --- | --- | --- | --- |
| A1 | https://raw.githubusercontent.com/apache/airflow/main/airflow-core/src/airflow/utils/state.py | `TaskInstanceState`, `DagRunState`, `TerminalTIState`, `IntermediateTIState`, `State.finished_dr_states` | Verified |
| A2 | .../airflow-core/src/airflow/models/dagrun.py | `DagRun` table `dag_run`, constraints `dag_run_dag_id_run_id_key`, `dag_run_dag_id_logical_date_key`, `clear_number`; `set_state` docstring "State transition matrix" and code | Verified |
| A3 | .../airflow-core/src/airflow/models/taskinstance.py | `task_instance` columns `id` (UUID), `state`, `try_number`, `max_tries` | Verified |
| A4 | .../airflow-core/src/airflow/api_fastapi/execution_api/routes/task_instances.py | `ti_run`, `ti_update_state`: `SELECT ... FOR UPDATE` then check `previous_state`, `409` with `reason: invalid_state`; duplicate-start tolerance; `410 Gone` when row moved to history | Verified |
| A5 | .../airflow-core/src/airflow/jobs/scheduler_job_runner.py and .../utils/sqlalchemy.py (`with_row_locks`) | `with_row_locks(..., skip_locked=True)` for TI, DagRun, DagModel claims; MySQL <8 / MariaDB fallback | Verified |
| A6 | .../airflow-core/src/airflow/utils/db_cleanup.py | `_TableConfig`, `config_list` (includes `log`, `task_instance`, `dag_run` with `keep_last`), `ARCHIVE_TABLE_PREFIX = "_airflow_deleted__"`, `task_instance_history` | Verified |
| A7 | .../airflow-core/src/airflow/api_fastapi/core_api/openapi/v2-rest-api-generated.yaml | REST conventions: `/api/v2`, `limit` (default 50) + `offset`, `order_by` with `-` prefix, `*_pattern` filters, error schema `HTTPExceptionResponse {detail}`, `DagRunState` enum | Verified |
| A8 | .../airflow-core/docs/migrations-ref.rst | Revision table (Revision ID / Revises ID / Airflow Version); warning that the DB structure is internal and must not be accessed directly | Verified |
| A9 | https://airflow.apache.org/docs/apache-airflow/stable/installation/upgrading_to_airflow3.html | Airflow 3: "Database Access Restrictions", Task Execution API, API server as sole DB access point for tasks and workers | Verified |
| A10 | https://airflow.apache.org/docs/apache-airflow/stable/installation/upgrading.html | `airflow db migrate`, `--show-sql-only`, backup advice, half-migrated failure | Verified |
| A11 | https://airflow.apache.org/docs/apache-airflow/stable/howto/usage-cli.html | "Purge history from metadata database": `db clean`, `--clean-before-timestamp`, `--dry-run`, archive tables, `db drop-archived` | Verified |
| A12 | https://airflow.apache.org/docs/apache-airflow/stable/administration-and-deployment/scheduler.html | HA schedulers: no consensus protocol, `SELECT ... FOR UPDATE`, MySQL 8.0+ / PostgreSQL 12+ | Verified |
| A13 | Allowed-transition table for `TaskInstanceState` | Searched `state.py`, `taskinstance.py`, `task_instances.py`: **no declarative from->to table exists**; guards are `if previous_state != ...` checks in code. `DagRun.set_state` has only a docstring matrix of side effects | Verified (absence) |
| A14 | `trigger_dag_run` duplicate `dag_run_id` response | Spec documents `409 Conflict` for the operation; the cause is not stated there | Cause **unverified** |
| A15 | Alembic details of Airflow migrations (downgrade, `db check-migrations`) | A10 did not cover them; A8 only shows the revision chain | Downgrade/check: **unverified** |

## Google AIPs (https://google.aip.dev/N)

| # | AIP | What was read | Status |
| --- | --- | --- | --- |
| G1 | 121 resource-oriented design | Hierarchy, standard over custom methods, statelessness | Verified |
| G2 | 122 resource names | `name` field, collection ids plural camelCase, user IDs lowercase RFC-1034, `uid` OUTPUT_ONLY | Verified |
| G3 | 131 / 133 / 135 | Get is GET without body; Create `{resource}_id`, `ALREADY_EXISTS`; Delete with `etag` | Verified |
| G4 | 132 list | `order_by` syntax, `filter`, `show_deleted` | Verified |
| G5 | 134 update | `update_mask`, PATCH, omitted mask, `*` | Verified |
| G6 | 136 custom methods | `:verb`, GET vs POST rule | Verified |
| G7 | 151 long-running operations | `google.longrunning.Operation`, 10 s rule of thumb, `done`, `Operation.error` | Verified |
| G8 | 154 etags | `etag` string, `ABORTED` on mismatch | Verified |
| G9 | 155 request IDs | `request_id`, idempotency, duplicate returns previous response | Verified |
| G10 | 158 pagination | `page_size`, `page_token`, opacity, parameter matching, expiry | Verified |
| G11 | 160 filtering | Operators, `:`; OR binds tighter than AND; implicit AND (not adopted: openetl refuses a bare juxtaposition, `AMBIGUOUS_FILTER`, INV-25); `INVALID_ARGUMENT`. The EBNF file `/assets/misc/ebnf-filtering.txt` was **not** read | Verified (grammar file unread) |
| G12 | 164 soft delete | `delete_time`, `purge_time`, `Undelete`, `show_deleted` | Verified |
| G13 | 193 errors | `error{code,message,status,details}`, JSON `code` is HTTP status, ErrorInfo reason regex | Verified |
| G14 | https://raw.githubusercontent.com/googleapis/googleapis/master/google/rpc/error_details.proto | `ErrorInfo.reason/domain/metadata` rules, `RetryInfo` | Verified |
| G15 | Exact `@type` URL string for ErrorInfo in JSON | Not shown on the pages read | **Unverified** |
