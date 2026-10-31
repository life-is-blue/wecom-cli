# openetl

Migration progress management platform: records the progress of migrating workflows and tasks
(stages, scope, owners, comments with evidence, audit events). It does not execute migrations.

- Gateway and CLI (`openetl`) are both written in Go and live in this repository.
- The gateway exposes a versioned REST + JSON API named after Google AIP conventions.
- Skills under `skills/` drive the CLI; the CLI owns token storage.
- Metadata store: MySQL 8.0 (behind a storage adapter).

## Layout

| Path | Purpose |
| --- | --- |
| `api/` | OpenAPI contract (source of truth for server and CLI types) |
| `db/migrations/` | Versioned MySQL schema scripts |
| `cmd/openetl-gateway/` | Gateway entrypoint |
| `cmd/openetl/` | CLI entrypoint |
| `skills/` | Agent Skills (`openetl-shared` preflight first) |
| `docs/` | Design notes |

## Quick start (development)

```bash
# an empty MySQL 8.0 / MariaDB database, then:
export OPENETL_DB_DSN='user:pw@tcp(127.0.0.1:3306)/openetl_demo?parseTime=true&multiStatements=true'
scripts/demo.sh                      # builds, migrates, runs the gateway and walks through the M1 scenario
OPENETL_TEST_DSN=$OPENETL_DB_DSN go test ./...   # DB tests skip when the DSN is unset
```

`openetl-gateway admin create-token` mints the first token (shown once, only its hash is stored);
`openetl auth login --token-stdin` stores it encrypted on the client, or set `OPENETL_TOKEN`.

## Status

M0 (contract) done. M1 (gateway, CLI, first Skills) done and covered by `scripts/demo.sh`;
verified on MariaDB 10.11 only, not yet on MySQL 8.0. Not yet built: attachments, import jobs,
members/tokens/config endpoints (M2). Design: see `docs/design.md`.
