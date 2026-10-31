# openetl

Migration progress management platform: records the progress of migrating workflows and tasks
(stages, scope, owners, comments with evidence, audit events). It does not execute migrations.

- Server and CLI (`openetl`) are both written in Go and live in this repository.
- The server exposes a versioned REST + JSON API named after Google AIP conventions.
- Skills under `skills/` drive the CLI; the CLI owns token storage.
- Metadata store: MySQL 8.0 (behind a storage adapter).

## Layout

| Path | Purpose |
| --- | --- |
| `api/` | OpenAPI contract (source of truth for server and CLI types) |
| `db/migrations/` | Versioned MySQL schema scripts |
| `cmd/openetl-server/` | Server entrypoint |
| `cmd/openetl/` | CLI entrypoint |
| `skills/` | Agent Skills (`openetl-shared` preflight first) |
| `docs/` | Design notes |

## Quick start (development)

```bash
# an empty MySQL 8.0 / MariaDB database, then:
export OPENETL_DB_DSN='user:pw@tcp(127.0.0.1:3306)/openetl_demo?parseTime=true&multiStatements=true'
scripts/demo.sh                      # builds, migrates, runs the server and walks through the M1 scenario
OPENETL_TEST_DSN=$OPENETL_DB_DSN go test ./...   # DB tests skip when the DSN is unset
```

`openetl-server admin create-token` mints the first token (shown once, only its hash is stored);
`openetl auth login --token-stdin` stores it encrypted on the client, or set `OPENETL_TOKEN`.

`delta` is the append-only source of truth; `entity_latest` (migration 0004) is a derived cache of the newest value per
(entity, key) that the public views read. It is written in the same transaction as the deltas and can be checked or rebuilt:
`openetl-server admin verify-latest` (exit 1 on any mismatch) and `openetl-server admin rebuild-latest`.
Numbers and the seeding/measuring scripts: `docs/performance.md`.

## Security notes

The query endpoint (`openetl query`) accepts one `SELECT` over the seven public views only (no subqueries, `UNION`,
CTEs, comma joins, joins without a real `ON`/`USING` condition, or parentheses after `FROM`/`JOIN`); every view reference is rewritten with the project filter.
As a second layer, run it over a database user that can only `SELECT` those views:

```bash
# edit the database name and password first
mysql -u root -p < scripts/create-query-user.sql
export OPENETL_QUERY_DSN='openetl_query:PASSWORD@tcp(db-host:3306)/openetl'   # server env; without it the main DSN is used (startup warning)
```

Query limits (server): `OPENETL_QUERY_MAX_PER_ACCOUNT` (default 3) and `OPENETL_QUERY_MAX_TOTAL` (default 6) concurrent queries,
over a dedicated 8-connection pool; excess requests get `429` + `Retry-After`. `OPENETL_PROJECT_STORAGE_QUOTA_BYTES` caps the
registered attachment bytes per project (default 10 GiB). `auth login` binds the saved token to its base URL.

Other switches: `OPENETL_PUBLIC_URL` (base of signed blob URLs), `OPENETL_TRUST_PROXY=1` (honour `X-Forwarded-*`),
`OPENETL_ALLOW_GLOBAL_CONFIG=1` (config updates on multi-project instances), client `OPENETL_ALLOW_HTTP=1`
(plain http to non-loopback hosts). Details and known limits: `docs/security.md`.

## Status

M0 (contract), M1 (server, CLI, Skills) and the import/attachment/admin parts of M2 are done. The full flow
(compose up, import 3000 workflows with ~9500 tasks, drive the lifecycle through the CLI, management report) runs
unattended with `scripts/e2e.sh` (`--small` for a 1-minute run); see `docs/quickstart.md`.
Verified on MySQL 8.0.46. Known limits: `docs/security.md`. Not yet: backups, public-cloud deployment, S3 backend
tested against a real store, the Dockerfile's `go mod download` step is unverified inside this sandbox.
Design: see `docs/design.md`.
