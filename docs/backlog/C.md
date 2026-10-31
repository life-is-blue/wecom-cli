# Backlog C (contract and security). Found, real, deliberately not built.

Effort: S under a day, M a few days, L a week or more. Ids are referenced from `docs/threat-model.md` and `docs/contract-checklist.md`.

| Id | Title | Evidence | Fix | Effort |
| --- | --- | --- | --- | --- |
| C-1 | No rate limiting on authentication or in general | `server.go` `authenticate` hits MySQL for every request with any token; no per-IP/per-token limit; only `:query` and uploads have caps | token-bucket per source IP and per token in a middleware (or at the reverse proxy); small auth failure cache | M |
| C-2 | Event log is append-only only by convention | `db/migrations/*.sql` have no triggers; the server's DB user can UPDATE/DELETE `event`/`delta`; `verify-latest` only checks `entity_latest` against `delta` | split roles: migrations user vs a runtime user without UPDATE/DELETE on `event`/`delta`; optionally a hash chain column verified by `admin verify-log` | M (roles) / L (chain) |
| C-3 | Query guard is the only barrier unless `OPENETL_QUERY_DSN` is set | `cmd/openetl-server/main.go` warns at startup but starts; the 2026-10-31 attack run used the main DSN | refuse to start `serve` without `OPENETL_QUERY_DSN` unless `OPENETL_ALLOW_MAIN_DSN_QUERY=1` | S |
| C-4 | Blob store hardening | `absURL` takes the `Host` header when `OPENETL_PUBLIC_URL` is unset (signed URLs point at a caller-chosen host); no encryption at rest; S3 commit not atomic and untested; upload reservations per process | require `OPENETL_PUBLIC_URL` outside loopback; document SSE for S3 and directory perms for fs; integration test against MinIO; DB-backed reservations | M |
| C-5 | Non-opaque page tokens | Fixed: `listMembers`, `listTokens` and `diffImportJob` use the signed, query-bound tokens of `store/pagetoken.go`; `query` already did | done | S |
| C-6 | Filter parser accepts malformed input | `filter=display_name = unterminated"` returns 200 with 0 rows; `*` wildcards in `=` are literal (AIP-160) | reject unterminated literals with `INVALID_FILTER`; add `*` wildcard support | S |
| C-7 | Wrong HTTP method is 400 instead of 405 | `server.go` `methodNotAllowed`; pinned by `gateway_test.go:422`; no `Allow` header | map to 405 `UNIMPLEMENTED`-style status with `Allow`, update the test and spec | S |
| C-8 | `readOnly` fields reused as input | `ImportRow.data` is the `Workflow` schema whose `scope` is `readOnly`, but rows accept `scope: OUT_OF_SCOPE` | dedicated `ImportWorkflowData` schema | S |
| C-9 | Update with a mask path that has no value in the body is a silent no-op | `PATCH ...?update_mask=display_name` with a flat body (no `workflow` wrapper) returns 200 and changes nothing (`handlers_workflow.go` `updateWorkflow`) | 400 when a masked scalar field is absent from the body, or accept the AIP-134 flat body | S |
| C-10 | `channel` of an event is client-asserted | `server.go` `channelOf` trusts `X-Openetl-Channel` | derive the channel from the token (issue CLI tokens as such) or drop the header | S |
| C-11 | Admin actions only reach the process log | `handlers_admin.go` logs member/token/config changes with `s.Log.Printf` | write them as events of a project-level entity so they share the audit trail | M |
| C-12 | Tokens are account-wide and have no scopes or per-token labels | `api_token` has no project/scope columns; `Token` schema has no display name | optional project scoping and a label; `last_used_time` | M |
| C-13 | Release toolchain has 39 stdlib vulnerabilities | `Dockerfile` pins `golang:1.24-alpine`; `govulncheck` with Go 1.24.7 reports 39, with 1.26.10 none | move the build image (and CI) to Go >= 1.26.10, re-run `govulncheck` in CI | S |
| C-14 | CLI wrote downloaded evidence world-readable | Fixed: `attachment download` and `-o` output are created with mode 0600 (tests `TestPresignedSamePortGetsNoTokenAcrossSchemes`, `TestOutputFileIsPrivate`) | done | S |
| C-15 | Stale ADR claim | Fixed: `docs/adr/0009...md` lists `Create: {resource}_id` as "deviates" (the server rejects it with `CLIENT_ID_UNSUPPORTED`) | done | S |
| C-16 | Dead code reported by staticcheck outside `internal/server` | `internal/cli/common.go:162 optStr`, `internal/store/list.go:48 defDesc`, `internal/store/workflow.go:15 nonEmptyMap`, `refmodel/model.go:308-311`, `store/query_limit_test.go:23` (SA4000) | delete | S |
