-- Least-privilege database user for `openetl-server serve` (OPENETL_DB_DSN), so that "append-only" is enforced by
-- the database and not only by the code: this user has NO UPDATE and NO DELETE on `event` and `delta`, and no DDL
-- except CREATE on schema_migrations.  The grant list was derived from what the code writes and PROVEN by
-- scripts/app-user-drill.sh (the whole flow runs as this user; UPDATE/DELETE on event/delta are shown to fail).
--
-- Roles: three database accounts.
--   owner/root          runs `openetl-server migrate` (DDL) on every upgrade, and backups
--   openetl_app (this)  runs `openetl-server serve` and `admin ...` commands
--   openetl_query       SELECT on the 7 public views only (scripts/create-query-user.sql, OPENETL_QUERY_DSN)
--
-- Usage (as a DB admin, AFTER `openetl-server migrate` created the tables):
--   sed -e 's/`openetl`/`yourdb`/g' -e 's/CHANGE-ME-app-password/…/' scripts/create-app-user.sql | mysql -u root -p
--   export OPENETL_DB_DSN='openetl_app:PASSWORD@tcp(db-host:3306)/yourdb?parseTime=true&multiStatements=true'
-- Re-run it after a migration that adds a table (the new table needs a grant line here first: see docs/operations.md).

CREATE USER IF NOT EXISTS 'openetl_app'@'%' IDENTIFIED BY 'CHANGE-ME-app-password';

-- Reads: tables and views (the server resolves names, lists and folds state).
GRANT SELECT ON `openetl`.* TO 'openetl_app'@'%';

-- Append-only history: INSERT only.  No UPDATE, no DELETE: corrections are new events (import rollback included).
GRANT INSERT ON `openetl`.`entity` TO 'openetl_app'@'%';
-- Found by the drill: `SELECT ... FOR UPDATE` (the per-entity write lock in store.Apply and import rollback) needs
-- UPDATE, DELETE or LOCK TABLES privilege in MySQL.  Column-level UPDATE on the single least meaningful column is the
-- smallest grant that satisfies it; entity.type / project_id / id stay immutable for this user.  Residual risk:
-- entity.create_time (the as-of cut-off) could be rewritten by a compromised app credential; events cannot.
GRANT UPDATE (create_time) ON `openetl`.`entity` TO 'openetl_app'@'%';
GRANT INSERT ON `openetl`.`event`  TO 'openetl_app'@'%';
GRANT INSERT ON `openetl`.`delta`  TO 'openetl_app'@'%';
GRANT INSERT ON `openetl`.`project` TO 'openetl_app'@'%';   -- INSERT IGNORE by create-token
GRANT INSERT ON `openetl`.`account` TO 'openetl_app'@'%';

-- Access control and configuration (mutable by design).
GRANT INSERT, UPDATE         ON `openetl`.`api_token` TO 'openetl_app'@'%';   -- UPDATE: revoked_at
GRANT INSERT, UPDATE, DELETE ON `openetl`.`member`    TO 'openetl_app'@'%';   -- add / remove / re-role
GRANT INSERT, UPDATE         ON `openetl`.`stage_def` TO 'openetl_app'@'%';   -- config set (upsert)
GRANT INSERT, UPDATE         ON `openetl`.`reason`    TO 'openetl_app'@'%';
GRANT INSERT, DELETE         ON `openetl`.`rule`      TO 'openetl_app'@'%';
GRANT INSERT, UPDATE, DELETE ON `openetl`.`setting`   TO 'openetl_app'@'%';

-- Import staging (operational state of a job).
GRANT INSERT, UPDATE ON `openetl`.`import_batch` TO 'openetl_app'@'%';       -- UPDATE: state, summary
GRANT INSERT, UPDATE ON `openetl`.`import_row`   TO 'openetl_app'@'%';       -- UPDATE: resolved action

-- Derived cache, rebuildable from delta (`admin rebuild-latest` empties it with DELETE).
GRANT INSERT, UPDATE, DELETE ON `openetl`.`entity_latest` TO 'openetl_app'@'%';

-- Migration bookkeeping: serve/admin run `CREATE TABLE IF NOT EXISTS schema_migrations` on start (MySQL checks the
-- CREATE privilege even when the table exists) and record applied versions.  Real DDL stays with the owner account.
GRANT CREATE, INSERT ON `openetl`.`schema_migrations` TO 'openetl_app'@'%';

-- verify: SHOW GRANTS FOR 'openetl_app'@'%';  -- UPDATE/DELETE on event and delta must not appear
