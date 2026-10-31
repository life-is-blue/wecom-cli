# openetl M0 shared spec (decisions already made; do not re-open)

Source of truth for the *current* schema/views/rules/CLI: `db/migrations/`, `api/openapi.yaml`, `internal/cli/views.json` and
`scripts/audit/vocab.md` (the original contract text lives in the "M0 契约草案" tab of the design document, see `docs/design.md`).
This file lists ONLY what changed versus that draft, plus constraints.

## Product
Migration progress management platform. Records progress of migrating workflows/tasks (stages, scope, owners,
comments with evidence, audit events). It does not execute migrations. Server + CLI (`openetl`) in Go, one repo.
One instance = one project in phase 1 (keep project_id columns anyway). MySQL 8.0 only. Stage keys:
development, deployment, verify, accept, dual_run, cutover (NOT developer_validation/user_validation).

## Resource names (Google AIP-122)
- projects/{project}                                  e.g. projects/acme-dw  (user-chosen lowercase slug)
- projects/{project}/workflows/{workflow}             e.g. wf-000123   (display code WF-000123, parsed case-insensitively)
- projects/{project}/tasks/{task}                     e.g. tk-004567   (belongs to exactly one workflow: the `workflow` field, resource name; changed only by `:assign`)
- projects/{project}/workflows/{workflow}/comments/{comment}
- projects/{project}/workflows/{workflow}/attachments/{attachment}
- projects/{project}/workflows/{workflow}/events/{event}   (read-only audit)
- projects/{project}/importJobs/{importJob}           (long-running; states queryable)
- accounts/{account}  (people and agents)

## Field naming (API layer; DB columns keep current names EXCEPT time columns below)
- `name` (resource name, OUTPUT_ONLY), `uid` (OUTPUT_ONLY), `display_name` (was key `name`), `etag`
- `labels` map<string,string> lowercase, filterable (was attr.*) ; `annotations` map<string,string> free text, not filterable
- `assignees` map<role, accounts/{id}>
- `stages`: list of {stage, state}; state enum STATE_UNSPECIFIED, OPEN, PASSED, NOT_APPLICABLE
- `scope` enum SCOPE_UNSPECIFIED, IN_SCOPE, OUT_OF_SCOPE; `merged_into` (workflow resource name)
- `health` OUTPUT_ONLY enum HEALTH_UNSPECIFIED, NORMAL, AT_RISK, BLOCKED (derived from unresolved comments)
- `current_stage` OUTPUT_ONLY (first stage not PASSED/NOT_APPLICABLE; empty when done)
- `delete_time` (soft delete); lists take `show_deleted`; `:undelete`
- `source` {system, reference, row_hash}
- times: `create_time`, `event_time` (when it happened, business time), DB: entity.created_at->create_time,
  event.occurred_at->event_time, event.recorded_at->create_time. Other DB columns unchanged.
- `creator` (accounts/{id}), `side` enum SIDE_UNSPECIFIED, DELIVERY, CUSTOMER, SYSTEM
- `request_id` (UUID, idempotency; DB column idempotency_key stays), `etag` replaces expect_event (etag derived from
  the entity's latest event id, opaque string), `validate_only` replaces dry_run
- comment kind enum: NOTE, QUESTION, ISSUE, RISK ; comment `resolved` bool + `resolve_time`
- enums UPPER_SNAKE with *_UNSPECIFIED = 0

## Methods (REST, /v1)
- List: GET /v1/{parent}/workflows?filter=&order_by=&page_size=&page_token=  -> {workflows[], next_page_token}
- Get: GET /v1/{name}
- Create: POST /v1/{parent}/workflows (`?workflow_id=` is reserved: rejected with CLIENT_ID_UNSUPPORTED, ids are server-assigned)
- Update: PATCH /v1/{workflow.name}?update_mask=   body carries etag, reason, note, request_id, validate_only
- Delete (soft): DELETE /v1/{name}?etag=   ; POST /v1/{name}:undelete
- Custom: POST /v1/{name}:advanceStage {stage, reason?, note?, evidence_attachments?, etag?, request_id?, validate_only?}
          POST /v1/{name}:regressStage {stages[], reason (required), ...}
          POST /v1/{name}:setScope {scope, reason (required when OUT_OF_SCOPE), merged_into?, ...}
          POST /v1/{name}:resolve   (comments)   POST /v1/{name}:reopen
- Same pattern for tasks (create/update/delete/undelete/list/get, :assign = move the task to another workflow). Deleting a workflow soft-deletes its tasks in the same transaction (docs/invariants.md INV-30).
- Comments: create/list/get under workflow; attachments: POST /v1/{parent}/attachments:initUpload -> presigned URL,
  then POST /v1/{parent}/attachments (metadata: filename,size,sha256,storage_key); GET /v1/{name}:download -> URL. Cap 100 MiB (104857600 bytes).
- Events: list/get only (GET /v1/{parent}/events?filter=&as_of=)
- Query: POST /v1/projects/{project}:query {sql, page_size, page_token} -> {columns, rows, next_page_token}
  read-only SELECT over documented views only.
- ImportJobs: POST create (stage rows), GET, :diff (GET), :apply, :rollback; apply/rollback return Operation.
- Members/tokens/config admin: /v1/projects/{p}/members, /v1/accounts/{a}/tokens, /v1/projects/{p}/config
- GET /v1/me

## Errors (AIP-193 style)
{"error":{"code":<http>,"status":"ABORTED","message":"...","details":[{"@type":"ErrorInfo","reason":"ETAG_MISMATCH","domain":"openetl","metadata":{}}]}}
status/reasons: INVALID_ARGUMENT; UNAUTHENTICATED; PERMISSION_DENIED(SIDE_NOT_ALLOWED); NOT_FOUND; ALREADY_EXISTS;
ABORTED(ETAG_MISMATCH); FAILED_PRECONDITION(STAGE_ORDER_VIOLATION, REASON_REQUIRED, MERGE_CYCLE, NOT_REGRESSIBLE);
RESOURCE_EXHAUSTED (rate limit); INTERNAL.

## Views (query surface; lowercase plural names; DB view names may keep v_ prefix but expose these names via
## the server or create views with these exact names): workflows, tasks, events, comments, workflow_stages,
## stage_distribution, open_items
