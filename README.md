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
| `internal/` | Server (`server`, `store`, `core`, `storage`, `importsrc`) and CLI (`cli`) packages; `internal/cli/views.json` is the table printed by `openetl views` |
| `skills/` | Agent Skills (`openetl-shared` preflight first) |
| `scripts/` | Demo, e2e, performance, drill and audit scripts (`scripts/audit/`: consistency audit and reconciliation, see `docs/consistency-report.md`) |
| `docs/` | Design notes, CLI reference (`docs/cli-reference.md`), import format, security, performance; `scripts/audit/vocab.md` is the vocabulary (stages, reasons, enums, keys) |

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

Other switches are listed in the next section. Details and known limits: `docs/security.md`.

## Environment variables

Server (`openetl-server`):

| Variable | Default | Meaning |
| --- | --- | --- |
| `OPENETL_DB_DSN` | required | go-sql-driver DSN of the metadata database, with `parseTime=true&multiStatements=true` |
| `OPENETL_ADDR` | `:8080` | listen address |
| `OPENETL_QUERY_DSN` | the main DSN (startup warning) | DSN of a `SELECT`-only user for `openetl query` |
| `OPENETL_QUERY_MAX_PER_ACCOUNT`, `OPENETL_QUERY_MAX_TOTAL` | 3, 6 | concurrent query caps; excess requests get `429` |
| `OPENETL_PROJECT_STORAGE_QUOTA_BYTES` | 10 GiB | cap of registered attachment bytes per project |
| `OPENETL_PUBLIC_URL` | derived from the request | base of signed blob URLs |
| `OPENETL_TRUST_PROXY` | off | `1` honours `X-Forwarded-*` |
| `OPENETL_ALLOW_GLOBAL_CONFIG` | off | `1` allows config updates on multi-project instances |
| `OPENETL_PAGE_TOKEN_KEY` | random per process | secret that signs page tokens; set the same value on every instance and across restarts |
| `OPENETL_ACCESS_LOG` | on | `off` disables the JSON access-log line per request on stderr |
| `OPENETL_METRICS_TOKEN` | unset | bearer token that lets a non-loopback caller read `/metrics` (loopback needs none) |
| `OPENETL_STORAGE` | `fs` | attachment backend: `fs` or `s3` |
| `OPENETL_STORAGE_DIR` | `./data/blobs` | `fs` backend directory |
| `OPENETL_STORAGE_SECRET` | random per process | HMAC key of `fs` signed URLs; set it so URLs survive a restart |
| `OPENETL_S3_BUCKET`, `OPENETL_S3_REGION`, `OPENETL_S3_ENDPOINT`, `OPENETL_S3_ACCESS_KEY_ID`, `OPENETL_S3_SECRET_ACCESS_KEY`, `OPENETL_S3_PREFIX`, `OPENETL_S3_PATH_STYLE` | | `s3` backend settings (`OPENETL_S3_PATH_STYLE=true` for path-style addressing) |

Client (`openetl`): `OPENETL_BASE_URL`, `OPENETL_PROJECT`, `OPENETL_TOKEN` (wins over the saved login), `OPENETL_ALLOW_HTTP=1`
(plain http to non-loopback hosts), `OPENETL_SOURCE_PASSWORD` (password of a `mysql://` import source; never printed or sent).
Tests and scripts only: `OPENETL_TEST_DSN`, `OPENETL_PERF_DSN`, `OPENETL_PERF_MIGRATE`.
The audit (`python3 scripts/audit/audit.py`) fails when this table and the code disagree.

## Status

M0 (contract), M1 (server, CLI, Skills) and the import/attachment/admin parts of M2 are done. The full flow
(compose up, import 3000 workflows with ~9500 tasks, drive the lifecycle through the CLI, management report) runs
unattended with `scripts/e2e.sh` (`--small` for a 1-minute run); see `docs/quickstart.md`.
Verified on MySQL 8.0.46. Known limits: `docs/security.md`. Not yet: backups, public-cloud deployment, S3 backend
tested against a real store, the Dockerfile's `go mod download` step is unverified inside this sandbox.
Design: see `docs/design.md`.
