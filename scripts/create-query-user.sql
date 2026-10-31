-- Read-only database user for the gateway's query endpoint (POST /v1/projects/{p}:query).
--
-- The SQL guard (internal/store/sqlguard.go) is the first layer; this user is the second: even a
-- guard bug cannot read api_token, member, delta... because the account has SELECT on the seven
-- public views and nothing else. The views run with DEFINER rights, so no grant on the base tables
-- is needed (or wanted).
--
-- Usage (as a DB admin, after `openetl-gateway migrate`):
--   1. replace `openetl` (database), 'openetl_query' and the password below
--   2. mysql -u root -p < scripts/create-query-user.sql
--   3. export OPENETL_QUERY_DSN='openetl_query:PASSWORD@tcp(db-host:3306)/openetl'
--
-- Re-run the GRANTs if a future migration drops and recreates a view under a new name.

CREATE USER IF NOT EXISTS 'openetl_query'@'%' IDENTIFIED BY 'CHANGE-ME-long-random-password';
-- limit what a runaway query can hold (MySQL and MariaDB both accept these)
ALTER USER 'openetl_query'@'%' WITH MAX_USER_CONNECTIONS 10;

GRANT SELECT ON `openetl`.`workflows`          TO 'openetl_query'@'%';
GRANT SELECT ON `openetl`.`tasks`              TO 'openetl_query'@'%';
GRANT SELECT ON `openetl`.`events`             TO 'openetl_query'@'%';
GRANT SELECT ON `openetl`.`comments`           TO 'openetl_query'@'%';
GRANT SELECT ON `openetl`.`workflow_stages`    TO 'openetl_query'@'%';
GRANT SELECT ON `openetl`.`stage_distribution` TO 'openetl_query'@'%';
GRANT SELECT ON `openetl`.`open_items`         TO 'openetl_query'@'%';

-- verify: only the seven views, SELECT only
-- SHOW GRANTS FOR 'openetl_query'@'%';
