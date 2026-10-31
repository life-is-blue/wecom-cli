# 0005. History retention: events are never purged; staging data may be

Status: accepted (reject Airflow-style purge for events; adapt for staging tables) · Date: 2026-10-31

## Context

openetl's `event` and `delta` tables are the system of record and append-only; `entity_latest` is a rebuildable cache.
Question: should the foundation copy the retention tooling of Airflow (`db clean`) and BigQuery (windowed job history)?

What the sources say:

* Airflow treats its metadata as operational data that grows without bound and must be trimmed: `airflow db clean` deletes
  "from each table the records older than the provided `--clean-before-timestamp`", offers `--tables`, `--dry-run`, archives by
  default into tables named `_airflow_deleted__<table>__<timestamp>`, `--skip-archive`, and `db drop-archived` (A11, A6).
  The purge list includes its audit trail (`table_name="log", recency_column_name="dttm"`), `dag_run` (with `keep_last=True`
  per dag so the newest non-manual run survives), `task_instance` and `xcom` (A6). Per-table errors are suppressed by default
  and "exits with code 0 even if one or more tables were not cleaned" (A11).
* Airflow keeps previous tries in a separate history table (`task_instance_history`, A6); a missing live row can answer
  `410 Gone` with "it may have been moved to the Task Instance History table" (A4 line ~1177).
* BigQuery windows its job history: `INFORMATION_SCHEMA.JOBS` "displays running jobs along with job history for the past 180
  days" (B4); `jobs.get`/`jobs.list` "available for a six month period" (B1); `jobs.delete` removes "the metadata of a job"
  (B1). Time travel is 2-7 days with a 7-day non-configurable fail-safe (B5). None of that is the business data.
* AIP-164: soft-deleted resources "should" have `delete_time` and `purge_time`, and the API "should document when soft deleted
  resources will be completely removed" (G12).

Distinction that matters: Airflow's and BigQuery's purged rows are *execution telemetry* of work they ran. openetl's events
are the *audit record of decisions* (who passed which stage, with what reason and evidence). They are the product.

## Options

1. Never delete `event`/`delta`; soft-delete only; do not ship any purge command in the foundation.
2. Ship `admin purge --before` for events with archive tables (Airflow clone).
3. Window the `events` view (BigQuery-style 180 days) while keeping rows.

## Decision

* **Reject** purge/windowing for `event`, `delta`, `entity`, soft-deleted entities. Document "soft-deleted resources are never
  purged; `purge_time` is not exposed" to satisfy AIP-164's documentation clause.
* **Adapt** only for non-audit staging bulk: `import_row.row_json` (full source rows for every staged job, JSON) is the only
  table whose size grows with data volume rather than with human actions. If retention is ever needed, the pattern to copy
  from Airflow is: `--dry-run` first, cutoff timestamp, archive-by-default, keep the newest applied job per source
  (`keep_last`), refuse to touch rows whose job can still be rolled back. Do **not** build it in the foundation; record the
  pattern and revisit when `docs/performance.md` numbers say the table matters.
* **Enforce** append-only at the database, not only in application code. A grep shows no `UPDATE`/`DELETE` on `event`,
  `delta` or `entity` in `internal/` or `scripts/`; the claim in `0001_init.sql` ("grant app account INSERT+SELECT only") is
  not backed by a script. Provide an app-user grant script next to `scripts/create-query-user.sql`: `INSERT, SELECT` on
  `event`, `delta`, `entity`, `INSERT, UPDATE, DELETE, SELECT` on `entity_latest`, `UPDATE` on `import_batch`, per
  `0004_entity_latest.sql` comment.
* Backups are a retention policy for the whole DB; Airflow's upgrade guide says "highly recommended to make a backup ... before
  any migration" (A10) and README lists backups as "Not yet". Keep that listed.

## Consequences

* The event log can only grow; sizing numbers belong in `docs/performance.md`.
* Corrections stay corrections: `corrects_event_id` (already in the schema) is the way to undo, never delete.
* Time travel (`as_of` on events and query) is the openetl analogue of BigQuery's `FOR SYSTEM_TIME AS OF`, but without a window,
  because history is never dropped. No work needed.

## Evidence

A4, A6, A10, A11, B1, B4, B5, G12; `db/migrations/0001_init.sql` header, `0004_entity_latest.sql`, `docs/performance.md`.
