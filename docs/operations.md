# Operations

Runbook for running openetl-server on MySQL 8.0. Every command in the backup section is the one `scripts/backup-drill.sh` executes; the drill restores a real database and fails if any assertion is off. Run it after changing anything in this file.

## What must be backed up

| What | Why | How |
| --- | --- | --- |
| The MySQL database | all state: events, deltas, tokens, config, import jobs, attachment *metadata* | logical dump + binary logs (below) |
| The blob directory (`OPENETL_STORAGE_DIR`, default `./data/blobs`; `/data/blobs` in the image) | attachment bytes; metadata without its blob is a dead link | file copy (`tar`/`rsync`), taken **after** the DB dump |
| `OPENETL_STORAGE_SECRET`, `OPENETL_PAGE_TOKEN_KEY`, DSNs, other env | not recoverable from the database | your secret store |

`OPENETL_PAGE_TOKEN_KEY` signs every `next_page_token` (lists, members, tokens, import diff, query). When it is unset each process draws a random key, so a token issued by one instance, or before a restart, is refused by another (`INVALID_PAGE_TOKEN`): **set the same value on every instance** behind a load balancer, and keep it across restarts if clients page across them. Changing it only invalidates tokens in flight.

With `OPENETL_STORAGE=s3`, the bucket is the blob store: enable versioning there instead of copying a directory.

Blobs are write-once, so a blob copy taken after the database point holds every blob the restored database can reference; extra blobs (uploaded after the recovery point) are harmless orphans. Never restore a blob copy older than the recovery point. `entity_latest` is a rebuildable cache, but it is in the dump anyway (restore is simpler and `verify-latest` proves it).

MySQL must run with binary logging and durable commits for point-in-time recovery:
`--log-bin=binlog --server-id=1 --binlog-format=ROW --sync-binlog=1 --innodb-flush-log-at-trx-commit=1` (8.0 enables `log_bin` by default, but set the rest explicitly).

## Backup

```bash
# 1. consistent logical dump with the binlog coordinates it corresponds to (no table locks, InnoDB snapshot)
mysqldump -uroot --single-transaction --source-data=2 --routines --triggers --events --hex-blob \
  --set-gtid-purged=OFF --databases openetl > dump.sql
grep -m1 -E "(MASTER|SOURCE)_LOG_FILE" dump.sql     # e.g. MASTER_LOG_FILE='binlog.000003', MASTER_LOG_POS=601124
# 2. blob directory, AFTER the dump
tar -C /data/blobs -cf blobs.tar .
# 3. ship binary logs continuously (or at least every RPO interval): FLUSH BINARY LOGS, then copy the closed files
mysql -uroot -e "FLUSH BINARY LOGS"; cp /var/lib/mysql/binlog.0* /backup/binlogs/
```

`--source-data` is the 8.0.26+ name (`--master-data` before; 8.0 prints `CHANGE MASTER TO MASTER_LOG_*`, 8.4 prints `SOURCE_LOG_*`). Keep the dump, the blob tar and the binlogs from the dump's file onward as one set.

## Restore and point-in-time recovery

```bash
systemctl stop openetl-server                                    # nothing may write while restoring
mysql -uroot < dump.sql                                          # re-creates the database
# replay from the dump's coordinates; --stop-position (or --stop-datetime) is the recovery point, omit it to replay everything.
# List the files from the dump's file to the one holding the stop position; start applies to the first, stop to the last.
cd /backup/binlogs && mysqlbinlog --database=openetl --start-position=601124 --stop-position=4059 \
  binlog.000003 binlog.000004 | mysql -uroot
tar -C /data/blobs -xf blobs.tar                                 # empty the directory first
systemctl start openetl-server                                   # migrations are a no-op; ANALYZE runs after bulk imports
openetl-server admin verify-latest                               # must print "entity_latest matches delta"
```

`mysqlbinlog` is **not** in the `mysql:8.0` image (a "server-minimal" Oracle Linux build); the drill uses `mysql:8.0-debian`, same 8.0.x, which has it. MariaDB's `mysqlbinlog` cannot read MySQL 8 logs (`Character set '#255'`, `check_constraint_checks`).

Observed in the drill (60 workflows, 222 tasks, 4 attachments, 440 KB dump, local Docker): dump 0.4 s; **RTO 8.5 s** (PITR) / **9.2 s** (full replay) from `DROP DATABASE` to a healthy, verified server; **RPO**: a full binlog replay lost 0 acknowledged writes (every table's row count and `CHECKSUM TABLE` equals the pre-disaster snapshot); the PITR deliberately dropped the 6.7 s of writes after the chosen point. With `sync_binlog=1` the RPO is bounded by how fresh the *archived* binlogs are, not by MySQL. RTO scales with dump size and binlog volume; it has not been measured at 3000 workflows.

What the drill asserts after each restore: row count and checksum of all 15 tables equal the snapshot; `admin verify-latest` is clean; a known workflow's etag and stage state equal the recovery point (PITR) or the pre-disaster state (full replay); a workflow created after the recovery point is absent after PITR and present after the full replay; every `storage_key` referenced by attachment metadata exists in the restored blob directory; the server serves and the old API token still works.

Retention: keep daily dumps 14 days and weekly dumps 8 weeks; keep binlogs from the oldest retained dump onward (`binlog_expire_logs_seconds` must exceed that, MySQL's default is 30 days); keep blob copies as long as the newest dump they belong to. Run the drill (or a restore into a scratch instance plus `verify-latest`) monthly: an untested backup is not a backup.

## Upgrades and rollback

1. Take a backup (above). 2. Stop traffic or accept a short restart. 3. Run the new binary: `serve` and `migrate` apply pending `db/migrations/*.sql` in order under an advisory lock (`GET_LOCK('openetl_migrate')`), recording each in `schema_migrations`. 4. `admin verify-latest`.

Rollback story: migrations are forward-only and additive (rules below), so the **previous binary keeps working on the migrated schema**. That is the supported rollback: redeploy the old binary. The server refuses to start (and `migrate` refuses to run) against a database that holds migrations newer than the binary knows, so an old binary cannot silently run on a schema it does not understand (`database has migrations this binary does not know`); in that case run the newer version or restore the backup. Restoring the pre-upgrade backup loses writes made after the upgrade, so prefer rolling the binary back.

With the least-privilege user (below), run `openetl-server migrate` with the owner account first; `serve` then finds nothing to apply.

### Migration 0007 and existing databases (pre-release)

0007 changes the task model: a task belongs to exactly one workflow and keeps one key, `workflow` (the workflow's entity id), instead of the
`in.<workflow>` link keys with a primary flag; `tasks.primary_workflow_id` is now `tasks.workflow_id`. The migration only replaces the views. There is
no production data before the first release, so **databases created before 0007 must be recreated** (drop, create, `openetl-server migrate`, re-import):
their tasks still carry the old keys, have no `workflow` key and show `workflow_id = NULL`.

### Schema changes: expand / contract

Never edit or delete a committed migration; `scripts/check-migrations.sh` compares every file against `docs/migrations.sha256` and fails on any difference, a gap in numbering, or a deleted file. A new migration may contain only additive DDL (`CREATE TABLE`, `CREATE [UNIQUE] INDEX`, `ALTER TABLE .. ADD ..`, `CREATE OR REPLACE VIEW`, `INSERT IGNORE`, `SET NAMES`); the lint flags `DROP`, `RENAME`, `MODIFY`, `CHANGE`, `ALTER .. DROP`, `TRUNCATE`, `DELETE`, `UPDATE`.

Changing something incompatibly takes two releases:
- **Expand** (release N): add the new column/table/view alongside the old; code writes both, reads the new one with fallback. Old and new binaries both run on this schema.
- **Contract** (release N+1 or later, once N is no longer a rollback target): stop using the old object, then drop it in a migration whose first line is `-- contract-approved: <ticket/reason>` (a human decision; the lint then accepts it).

After a release ships, freeze its migrations: `scripts/check-migrations.sh --freeze` appends them to the manifest. `scripts/check-migrations.sh --apply` additionally proves on a throwaway MySQL 8.0 that the real migrate runner reaches head from every prefix (empty, 0001, 0001-0002, ...), is a no-op the second time, and that the downgrade guard fires. Migration DDL is not transactional in MySQL: a failure in the middle of a file leaves it half applied, and re-running raw SQL fails (`0003`: error 1060 duplicate column). Restore the backup or finish by hand; see `docs/backlog/B.md` B-1.

## Least-privilege database users

`scripts/create-app-user.sql` creates `openetl_app` for `OPENETL_DB_DSN`: **INSERT only on `event` and `delta`**, so history cannot be rewritten even by a compromised app credential; `scripts/create-query-user.sql` creates the SELECT-only `openetl_query` for `OPENETL_QUERY_DSN`; the owner account runs `migrate` and backups. `scripts/app-user-drill.sh` proves the grants: it migrates as owner, creates both users from the shipped scripts, shows UPDATE/DELETE/TRUNCATE/DROP/ALTER on event/delta/entity are refused, then runs import (stage, diff, apply, rollback), lifecycle, comments with an attachment, scope, config set, member and token administration, `rebuild-latest` and `verify-latest` as the restricted user. Findings: the import rollback writes correcting events and needs no UPDATE/DELETE on event/delta; `SELECT .. FOR UPDATE` on `entity` (per-entity write lock) needs an UPDATE privilege, granted as column-level `UPDATE (create_time)` only; `serve` runs `CREATE TABLE IF NOT EXISTS schema_migrations` at start, which needs `CREATE` on that table even when it exists. A new table added by a migration needs its grant line in the script (the drill fails otherwise).

## Observability

`GET /metrics` (Prometheus text, hand-written, aggregate only): `openetl_http_requests_total` and `openetl_http_request_duration_seconds` (histogram) by route template, method and status class (ids become `{id}`, projects `{p}`, so no project or account names appear), `openetl_import_duration_seconds{op=stage|apply|rollback}`, `openetl_query_limiter_rejections_total{reason}`, `openetl_db_pool_*{pool=main|query}` (open, in use, idle, max open, waits), `openetl_uptime_seconds`. Access: only a direct loopback connection (requests carrying `X-Forwarded-For`, `X-Real-Ip` or `Forwarded` are treated as proxied and refused), or `Authorization: Bearer $OPENETL_METRICS_TOKEN` when that variable is set. Scrape from localhost/sidecar, or set the token. Counters are per process and reset on restart.

Each request writes one JSON line to stderr: `{"ts","request_id","method","route","status","duration_ms","account"}` (no tokens, bodies, query strings or project names; `/healthz` is not logged). The id comes from a clean incoming `X-Request-Id` or is generated, and is returned in the response header. `OPENETL_ACCESS_LOG=off` disables it.

## Container image

`Dockerfile`: build stage pinned to `golang:1.26.10-alpine`, final stage `alpine:3.22`, non-root user (uid 10001), `HEALTHCHECK` on `/healthz`, dependency layer cached on `go.mod`/`go.sum`. Release builds must use a Go version where `govulncheck ./...` is clean (the standard library is linked into the binary): `go run golang.org/x/vuln/cmd/govulncheck@latest ./...`. Go 1.24.7 had 39 standard-library findings, 1.26.10 none; `go.mod`'s `go 1.24.7` is only the minimum and needs no bump.

**Verified here:** the image tag `golang:1.26.10-alpine` exists and pulls; `go build ./...`, `go vet ./...`, the unit tests and the drills with a locally built binary. **Not verified:** `docker build` of this Dockerfile end to end (the sandbox cannot download Go modules through its proxy), the HEALTHCHECK and non-root behaviour in a running container, the `alpine:3.22` tag pull, `govulncheck` in this sandbox (reported by another worker), the compose stack with the new image.

## Troubleshooting (real failures)

- **Queries slow right after a big import, `EXPLAIN` shows bad plans.** InnoDB statistics are recomputed lazily and are stale after a bulk load. The server runs `ANALYZE TABLE entity, entity_latest, event, delta` after every import apply/rollback; after a restore or manual bulk load run it yourself.
- **Reports disagree with the event history (`entity_latest` drift).** It is a cache of the latest value per (entity, key). `openetl-server admin verify-latest` recomputes from `delta` and exits 1 on any difference; `admin rebuild-latest` rebuilds it in one transaction. Always run `verify-latest` after a restore.
- **Scripts fail with `ERROR 2002` / connection refused / "mysqld is not running" right after `docker run mysql`.** The image starts a temporary socket-only bootstrap server, stops it and starts the real one. A socket ping, or a TCP ping that lands in the gap, succeeds too early. Wait on TCP (`mysqladmin -h127.0.0.1 ping`), then wait a moment and ping again (the drills do).
- **`mysqlbinlog: command not found` in `mysql:8.0`.** Use `mysql:8.0-debian` or install the client package; do not use MariaDB's tool.
- **`database has migrations this binary does not know`.** An older binary met a database upgraded by a newer one. Run the newer binary or restore.
- **`Error 1142 SELECT with locking clause command denied`.** The app user lacks the `UPDATE (create_time)` grant on `entity` (use the current `create-app-user.sql`).
- **`/metrics` returns 403 behind a reverse proxy.** By design; set `OPENETL_METRICS_TOKEN` and send it as a bearer token, or scrape from the host.
