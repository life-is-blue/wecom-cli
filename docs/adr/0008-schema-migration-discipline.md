# 0008. Schema migrations: forward-only, immutable files, previewable

Status: accepted (adapt; small conflicts found) · Date: 2026-10-31

## Context

What the sources say:

* Airflow applies schema changes with `airflow db migrate`; "it's safe to run even if there are no migrations to perform";
  `-s/--show-sql-only` prints "the SQL statements that would be executed"; "It is highly recommended to make a backup of your
  metadata DB before any migration"; a failed migration "can leave the database half-migrated", and restoring a backup and
  rerunning may be simplest (A10).
* Airflow migrations form a revision chain, documented as a table `Revision ID | Revises ID | Airflow Version | Description`
  generated from the migration files by a hook, so the doc cannot drift (A8). Downgrade support and `db check-migrations` were
  not in the page read (A15, unverified).
* The same page warns users not to read or write tables directly and to use the REST API (A8) (see 0001).
* The scheduler's HA story relies on MySQL 8.0+ / PostgreSQL 12+ row locks (A12); MySQL 8.0 only is also our constraint.
* AIP-121/122 give no migration guidance; the relevant AIP point is that published field and enum semantics are a contract.

openetl today: `Store.Migrate` takes `GET_LOCK('openetl_migrate', 60)`, creates `schema_migrations(version, applied_at)`, runs the
embedded `db/migrations/*.sql` in filename order, splits each file on `;` and executes statements one by one, then records the
version. MySQL DDL implicitly commits, so a file that fails midway is half-applied and not recorded, like Airflow's warning.
`0002` states the rule "0001 is never edited". `openetl-server migrate` is the CLI entry. At the time of writing another worker is adding `scripts/check-migrations.sh`
(frozen-file manifest, additive-DDL lint, apply-from-every-prefix, run-twice no-op) and `docs/operations.md` (expand/contract);
that work is in flight and was only skimmed (header comment), so this ADR does not judge it.

Findings in the current files:

* `0003_import_job.sql` and `0004_entity_latest.sql` both carry the comment "openetl 0004" at the top; `0003` is mislabeled.
* `0003` uses plain `ALTER TABLE ... ADD COLUMN`, not re-runnable after a partial failure. `0001` ends with
  `INSERT INTO rule` described as "must run once" (no natural key), so re-running `0001` after a failure in `schema_migrations`
  insert could duplicate rules. `0004` begins `DELETE FROM entity_latest` and then rebuilds, which is safely re-runnable.

## Options

1. Keep the lightweight runner and add rules.
2. Adopt a migration library (goose / golang-migrate) with up/down.
3. Copy Airflow's revision chain with `Revises` ids.

## Decision

**Adapt: option 1.** A linear numeric prefix is enough for one team and one DB engine; a library adds a dependency without
addressing DDL non-atomicity (the real risk, shared with Airflow). Rules:

1. Forward-only. Applied files are immutable (already practice); fixes are new files. No down migrations in the foundation;
   the recovery path is backup + rerun, as Airflow documents (A10).
2. (The check script's run-twice test covers whole-file re-runs; this rule is about a file that failed midway.) Every statement in a new file must be re-runnable (`IF NOT EXISTS`, `CREATE OR REPLACE`, guarded backfills) or the file must
   contain exactly one statement (MySQL 8.0 DDL is atomic per statement).
3. Seed rows get a natural key or `INSERT IGNORE` (`reason`, `stage_def` already do; `rule` does not).
4. A dry-run that prints pending files without executing (`openetl-server migrate --show-sql`), the analogue of Airflow's
   `--show-sql-only`, if the in-flight check script does not already cover the need. Small, operator-only.
5. Keep the advisory lock; it is the equivalent of Airflow relying on a single migrating process.
6. Public views are recreated with `CREATE OR REPLACE VIEW`, and a column-list change needs a new file that recreates every
   dependent view (0002 explains why). The view contract test must run after migration in CI.
7. Fix the `0003` header comment. A comment-only edit of an applied file does not change behaviour; if the project forbids any
   edit of applied files, leave it and note it here.

## Consequences

* A reviewer can check each migration against a three-line checklist. Backups remain README-listed as "not yet".
* The in-flight `docs/operations.md` / `scripts/check-migrations.sh` should link this ADR (other workers own those files); where
  they disagree with rules 1-7 above, theirs win for the mechanics and this ADR for the Airflow/BigQuery evidence.

## Evidence

A8, A10, A12, A15 (gap); `internal/store/store.go:92-141`, `db/migrations/0001..0004`.
