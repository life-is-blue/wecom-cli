# 0001. One server owns the metadata database

Status: accepted (adopt) · Date: 2026-10-31

## Context

Constraint already decided: one server owns the DB. The question is how far that discipline has to go, and what a
precedent system does when the rule is tested by many workers and a CLI. Airflow is the closest metadata-DB-centred
platform.

What the sources say:

* Airflow 3 moved task code off the database: "Task code can no longer directly import and use Airflow database sessions or
  models." and "The API server is currently the sole access point for the metadata DB for tasks and workers." Runtime
  interactions "(state transitions, heartbeats, XComs, and resource fetching) are handled through a dedicated Task Execution API"
  (A9). The same page admits the rule is not total: "Dag author code potentially still executes with direct database access in
  the Dag File Processor and Triggerer" (A9).
* Airflow tells operators the schema is private: "the structure is internal and you should not access the DB directly to
  retrieve or modify any data - you should use the REST API" (A8).
* In the Task Execution API every state transition begins with a row lock on the one row it changes, e.g. `ti_run` selects
  `.where(TI.id == task_instance_id).with_for_update(of=TI)` (A4, line ~211).
* BigQuery exposes metadata only through `INFORMATION_SCHEMA`, described as "read-only, system-defined views" (B3); jobs are
  read through `jobs.get` / `jobs.list` (B1).

openetl today: only `internal/store` talks SQL; `internal/cli` and `cmd/openetl` import no `database/sql` (checked with grep).
The one door for user-supplied SQL is `POST ...:query`, guarded by `sqlguard.go` and a separate `SELECT`-only DB user
(`docs/security.md`). `openetl-server admin ...` (create-token, verify-latest, rebuild-latest) talks to the DB directly, as an
operator tool.

## Options

1. Server is the only process with DB credentials; CLI, skills and agents use the REST API only (status quo).
2. Let the CLI read views directly for speed (reject: tokens and project filter would be bypassed).
3. Add a worker/executor tier with a DB-free "execution API" like Airflow 3 (reject: openetl has no workers; it "does not
   execute migrations").

## Decision

**Adopt option 1, and write the boundary down.**

* DB credentials exist only in the server environment (`OPENETL_DB_DSN`, `OPENETL_QUERY_DSN`). The CLI never has a DB flag.
* The seven public views are the only documented read surface (the analogue of `INFORMATION_SCHEMA`); `v_*` helper views,
  `entity_latest`, `delta` and the config tables are internal and may change in any migration without notice.
* `openetl-server admin *` is the explicit operator exception, like Airflow's own `airflow db` commands (A10); it must stay in
  the server binary and never be exposed over HTTP.
* Do not copy Airflow's SKIP LOCKED claim queues. Airflow needs competing consumers for scheduling work (A5); openetl has
  none. The right analogue is the blocking `SELECT ... FOR UPDATE` on the one entity row that `applyTx` already takes.

## Consequences

* No code change. Add two sentences to `docs/security.md` or `docs/design.md` stating "internal tables are not API" so the
  view-column contract (`internal/cli/views.json` and its drift test) is the only compatibility promise.
* Enforcement gap (see README conflicts table): `0001_init.sql` says the app account gets INSERT+SELECT on the business
  tables, but no script creates that account (only `scripts/create-query-user.sql`). Airflow's "sole access point" is
  enforced by architecture, ours only by convention.

## Evidence

A4, A8, A9, A5, B1, B3; `docs/security.md` (Query isolation).
