# Architecture decision records

Foundation scope only. Evidence index: `docs/references.md` (ids such as B1, A4, G9 refer to its tables).

| ADR | Decision | Verdict |
| --- | --- | --- |
| [0001](0001-one-server-owns-the-metadata-db.md) | Only the server holds DB credentials; seven views are the only read contract; internal tables are not API; no SKIP LOCKED queues | adopt |
| [0002](0002-import-jobs-as-operations.md) | importJobs return a synchronous `Operation` (`done:true`); lifecycle/outcome (`done/error`) separate from domain `state`; no RUNNING state until apply exceeds ~10 s | adapt |
| [0003](0003-state-machine-guard-and-rule-table.md) | Judge transitions against the value read under the row lock; rule table = who/why, code = structural invariants; add a transition-matrix test | adapt |
| [0004](0004-idempotency-and-retries.md) | `request_id` replays must also match the request (BigQuery "actually duplicates"); retention unbounded | adapt |
| [0005](0005-history-retention-and-append-only-events.md) | Never purge events/deltas/soft-deleted rows; copy Airflow `db clean` pattern only for `import_row` if ever needed; enforce append-only with DB grants | reject purge / adapt staging |
| [0006](0006-read-surface-views-list-paging-filter.md) | Keep the seven views and `:query` POST; sign and bind page tokens; fix or forbid ambiguous AND/OR | adopt / adapt |
| [0007](0007-error-model.md) | Keep AIP-193 body and stable reasons; reasons are append-only; documented retry classes | adopt |
| [0008](0008-schema-migration-discipline.md) | Forward-only, immutable, re-runnable statements, previewable; mind MySQL non-atomic multi-statement DDL | adapt |
| [0009](0009-resource-conventions-etag-soft-delete-custom-methods.md) | AIP 121/122/131-136/154/164 conformance table; record deviations (`:query` POST, etag scope, Operation name) | adopt |

## Conflicts between existing openetl choices and the evidence

| # | Conflict | Evidence | Smallest fix | Effort |
| --- | --- | --- | --- | --- |
| C1 | `request_id` replay returns the stored result without checking that the new payload matches; a reused id with a different payload silently "succeeds" (`internal/store/apply.go:96-117`) | AIP-155 (G9); BigQuery `QueryRequest.requestId`: duplicate "only if ... actually duplicates" (B1) | On replay, compare stored event changes with the recomputed changes; mismatch -> `ALREADY_EXISTS REQUEST_ID_REUSED` (ADR 0004 option 3) | S |
| C2 | Filter parser binds `AND` tighter than `OR`: `a AND b OR c` = `(a AND b) OR c` (`internal/store/filter.go:55-92`); AIP-160 says `a AND (b OR c)` | AIP-160 (G11) | Reject unparenthesised mixed `AND`/`OR` with `INVALID_ARGUMENT` now; swap precedence later if full AIP compliance is wanted | XS (reject) / S (swap + tests) |
| C3 | Page token is `base64url("k:<id>")`/`"o:<n>"`: parseable, and not bound to filter/order_by (`internal/store/list.go:97-115`) | AIP-158 (G10): opaque, "Base-64 encoding ... is not a sufficient obfuscation mechanism", other params must match | HMAC-sign the token and include a hash of (parent, filter, order_by); mismatch -> `INVALID_PAGE_TOKEN` | S |
| C4 | "Append-only, grant INSERT+SELECT only" is stated in `0001_init.sql` but no app-user grant script exists; only the query user is scripted | Airflow's sole-DB-access rule is enforced by architecture (A9); ADR 0005 | Add `scripts/create-app-user.sql` with least-privilege grants per `0004_entity_latest.sql` comment | S |
| C5 | `docs/cli-import.md` says the CLI waits for the job to "leave the running state"; no running state exists, apply is synchronous | OpenAPI `ImportJob.state` enum; `handlers_import.go` always `done:true`; AIP-151 10 s rule (G7) | Reword the doc; keep `--wait` as forward-compatible | XS |
| C6 | `0003_import_job.sql` header says "openetl 0004" | `db/migrations/` | Fix the comment (or leave and note in ADR 0008 if applied files are frozen by the manifest) | XS |
| C7 | `0001` rule seed `INSERT INTO rule` has no natural key and is "must run once" (partial-failure re-run could duplicate rules) | Airflow: failed migration leaves DB half-migrated (A10) | Use a guarded insert (`INSERT ... SELECT ... WHERE NOT EXISTS`) in any future rule seed; do not edit 0001 | XS |
| C8 | `ErrorInfo.domain` is `"openetl"`, not "globally unique"; `@type` is the short `ErrorInfo` | AIP-193 / `error_details.proto` (G13, G14); exact `@type` form unverified (G15) | Document as opaque; make domain configurable only if an external client needs it | XS |

Not conflicts (checked): `request_id` UUID and retention, ETag/`ABORTED`, soft delete fields, `:verb` custom methods, reason
regex, resource names, page tokens' keyset paging for id order, row-lock-then-validate, `FOR UPDATE` without SKIP LOCKED.

## Could not verify

* BigQuery `jobs.insert` behaviour for a duplicate client `jobId` (HTML pages truncated; Discovery text silent) - B7.
* Airflow: the cause of `409` on `trigger_dag_run`; downgrade and `db check-migrations` docs; exact commit sha of `main` read - A14, A15.
* AIP-160 EBNF file; the JSON `@type` string of `ErrorInfo` - G11, G15.
* openetl: behaviour for an omitted `update_mask`, project-slug regex enforcement, `validate_only` not consuming a `request_id` (not tested here).
