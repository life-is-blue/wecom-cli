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

## Status

M0 (contract) in progress. Design: see `docs/design.md`.
