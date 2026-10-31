# API contract checklist

What this page is: the result of checking `api/openapi.yaml` and the running server against the Google AIP rules we
claim to follow, plus the findings of the OpenAPI tooling. It is a manual checklist: **Google's `api-linter` lints
protobuf, not OpenAPI, so it was not used and no tool-verified AIP compliance is claimed.** The AIP text of every cited
rule was read from <https://google.aip.dev/<n>> (fetched 2026-10-31); "conforms" means the rule's MUST/SHOULD lines hold.

Re-run everything with `scripts/check-contract.sh` (`--static` for the spec-only part).

## 1. Tooling

| Tool (version) | What it checks | Result |
| --- | --- | --- |
| `openapi-spec-validator` 0.9.0 | OpenAPI 3.0 schema validity | 0 findings |
| `oasdiff` v1.35.0 `breaking` | Breaking changes between `api/openapi.baseline.yaml` and `api/openapi.yaml`; gate in `check-contract.sh` (`--fail-on ERR`) | baseline == current, 0 findings. Comparing the spec as received with the corrected one reports 7 "breaking" items, all of them corrections that make the contract match what the server already returned (see 2.3) |
| `vacuum` v0.36.1 (recommended OAS ruleset) | Lint | 241 findings on the spec as received (9 rules), 219 now; the rest are ignored on purpose, see 2.2. `api/vacuum-ruleset.yaml` switches off exactly those rules; anything new fails the gate |
| oapi-codegen v2.8.1 | `internal/api/types.gen.go` is regenerated from the spec; gate checks it is current | `internal/api/gen.go` pinned to v2.8.1 (was `@latest`) |
| `scripts/contract_conformance.py` | Black-box: every operationId against a live server, response bodies validated against the response schemas (types, enums, required, formats, patterns, undeclared fields, writeOnly leaks), AIP-193 error shape | 110 calls, 43/43 operations, 0 divergences after the fixes below |

## 2. Findings and triage

### 2.1 Server vs spec divergences found by the conformance pass (all fixed in the spec; the server was right)

| # | Divergence | Fix |
| --- | --- | --- |
| 1 | `GET .../attachments/{attachment}` is served but was not in the spec | added operation `getAttachment` |
| 2 | `diffImportJob` returns actions `TASK_NEW/CHANGED/UNCHANGED/MISSING/CONFLICT` that the `ImportAction` enum lacked | enum extended |
| 3 | `createImportJob` returns `row_errors`, undeclared | added (readOnly) |
| 4 | diff `changes[].kind` (`source`/`fill`/`conflict`) undeclared | added |
| 5 | non-admin `getConfig` returns `"settings": null`, schema was not nullable | `nullable: true` |
| 6 | `createWorkflow ?workflow_id=` / `createTask ?task_id=` documented as working; the server rejects any value with `CLIENT_ID_UNSUPPORTED` | parameters kept but marked `deprecated` and documented as reserved |
| 7 | `query.as_of` documented as working; the server answers `AS_OF_UNSUPPORTED` (the CLI still sends it when asked) | marked `deprecated`, behaviour documented. Not removed: removal is breaking |
| 8 | `query.page_size` documented up to 10000; the server coerces to 1000 | documented |
| 9 | query parameter `request_id` documented as `format: uuid`; the server accepts any string up to 64 chars (body `request_id` is parsed as a UUID) | query parameter documented as `maxLength: 64`. Server tests rely on non-UUID query ids (`request_id=r1`), so the server was not tightened |
| 10 | soft delete / undelete are idempotent (200 on an already deleted / live resource) | documented in the operations (the AIP says otherwise, see AIP-164 below) |
| 11 | `405` for a wrong HTTP method on a known route is `400 INVALID_ARGUMENT / METHOD_NOT_ALLOWED` | documented in `info.description` (pinned by `gateway_test.go`) |

### 2.1b Deliberate contract corrections (baseline refreshed for exactly these)

`api/openapi.baseline.yaml` was copied from `api/openapi.yaml` after `oasdiff breaking` listed these and nothing else. All of them make the
spec say what the server already did, or retire a field that never worked:

| # | Change | Why it is safe |
| --- | --- | --- |
| C1 | `QueryRequest.page_size.maximum` 10000 -> 1000 (also `openetl query --help`) | the server always clamped to 1000 |
| C2 | `QueryRequest.as_of` removed, and the `--as-of` flag of the query command removed | every request that set it was refused with `AS_OF_UNSUPPORTED`; a body that still carries it is still refused with that reason. `listEvents` keeps `as_of` |
| C3 | diff `changes[].kind` values `source`/`fill`/`conflict` -> `SOURCE`/`FILL`/`CONFLICT` (+ `KIND_UNSPECIFIED`), server and CLI output changed with it | UPPER_SNAKE rule of `docs/m0-naming-spec.md`; consumers of `openetl import diff` must compare upper case (documented in `docs/import-format.md`, `docs/cli-import.md`) |
| C4 | error description lists `ENTITY_DELETED` (FAILED_PRECONDITION) | new refusal of INV-26 (writes to a soft-deleted entity); documentation only (`DUPLICATE_PRIMARY`, listed here before, was removed again by C6) |
| C5 | `createWorkflow ?workflow_id=` / `createTask ?task_id=` unchanged in the spec ("reserved, any value is rejected with CLIENT_ID_UNSUPPORTED", which is what the server does); ADR 0009 now says "deviates", `openetl workflow create --id` help says reserved | no spec change needed, docs aligned |

### 2.1c Deliberate incompatible changes of the product decisions (baseline refreshed)

Pre-release, no production data: these change the contract on purpose and `api/openapi.baseline.yaml` was copied from `api/openapi.yaml` after `oasdiff breaking` listed them.

| # | Change | Decision |
| --- | --- | --- |
| C6 | `Task.workflows[]` (`TaskMembership`: `workflow`, `primary`) replaced by one `Task.workflow` (resource name, required on create); `AssignTaskRequest.membership` removed, `:assign` means "move this task to workflow X"; list filter `workflows.workflow = "..."` is now `workflow = "..."`; error `DUPLICATE_PRIMARY` / `MULTIPLE_PRIMARY` removed; CLI `task create --workflow` takes exactly one value and is required, `task assign` has no `--membership` | a task belongs to exactly one workflow (INV-27) |
| C7 | SQL view `tasks.primary_workflow_id` renamed `tasks.workflow_id` (migration 0007); stored task key `in.<workflow>` replaced by `workflow` | same |
| C8 | `MutationOptions.evidence_attachments` added (optional, used by `advanceStage`, `regressStage`, `setScope`, `updateWorkflow`); new FAILED_PRECONDITION reasons `EVIDENCE_REQUIRED_FOR_OVERRIDE`, `REGRESS_LATER_STAGES_PASSED`, `PARENT_DELETED`; events of an override carry `override.<key>` deltas | admin override needs evidence (INV-29); regress refuses to leave later stages PASSED (INV-31); cascade (INV-30) |
| C9 | A workflow delete/undelete writes one extra event per task (`cascade` key); a task can no longer be created in or moved to a deleted workflow (was WORKFLOW_NOT_FOUND on create, now ENTITY_DELETED) | INV-30 |
| C10 | Filters: no implicit AND, a bare juxtaposition is INVALID_ARGUMENT/AMBIGUOUS_FILTER (was accepted as AND) | backlog A-6 |

### 2.2 Linter findings that are ignored on purpose (`api/vacuum-ruleset.yaml`)

| Rule (count) | Why it stays |
| --- | --- |
| `camel-case-properties` (62) | JSON fields are snake_case by M0 decision (`docs/m0-naming-spec.md`, "do not re-open"). The protobuf JSON mapping of AIP APIs would be lowerCamelCase; this is a deliberate deviation, see AIP-140 row |
| `paths-kebab-case` (6) | `importJobs` (AIP-122 requires camelCase collection ids) and `:advanceStage` etc. (AIP-136 requires camelCase custom verbs) |
| `oas3-missing-example` (65) | Schemas carry descriptions; examples exist for the two error/stage shapes that are not obvious |
| `operation-description` (22) | Missing `description` on request bodies; the operation summary and the schema description already say it |
| `description-duplication` (24) | Shared `OK` response descriptions |
| `oas-missing-type` (3) | `AnyValue` is deliberately "any JSON value" (event old/new values, query cells) |
| `nullable-enum-contains-null` (1) | `Config.rules[].reason_kind` is nullable. Adding `null` to the enum is what the rule wants, but oapi-codegen then emits a bogus `"<nil>"` enum constant; the server really sends `null` |
| `component-description` (36) | Wrapper and enum schemas are self-describing; entity, error and derived-state schemas are described |

Fixed from the lint: 13 undescribed path parameters, 6 undescribed components (error envelope, ErrorInfo, `Side`,
`Scope`, `Health`, `StageState`), tag descriptions.

Not changed, deliberately: schemas have no `required` for output fields (e.g. `Workflow.name`). The same schema is the
request body of create and sparse update; marking output fields required would make oapi-codegen emit non-pointer
fields and break server and CLI. Output completeness is verified by the conformance pass instead.

### 2.3 The 7 "breaking" items between the received spec and the corrected one

`response-property-became-nullable` x2 (`settings`) and `response-property-enum-value-added` x5 (`TASK_*` actions).
They are contract corrections: the server returned these values before, clients written from the old spec were
already wrong. Nothing in the server or CLI changed behaviour. The new spec is the baseline.

## 3. AIP conformance table

Legend: **conforms**, **deviates** (place in `openapi.yaml`, reason), **n/a**. "Verified" means the conformance pass or the attack drill
exercised it against the running server.

| AIP | Rule (read in the AIP) | Verdict | Where / reason |
| --- | --- | --- | --- |
| 122 resource names | `name` field holds the full resource name (string) | conforms (verified) | `Workflow.name`, `Task.name`, `Comment.name`, ... all `readOnly` |
| 122 | collection ids plural camelCase, alternate with ids | conforms | `workflows`, `tasks`, `comments`, `attachments`, `events`, `importJobs`, `members`, `tokens` |
| 122 | resource ids lowercase, RFC-1034-like; documented format | conforms | server-assigned `wf-NNNNNN`, `tk-`, `cm-`, `at-`, `ev-`; project slug pattern on `ProjectP`. Upper-case input (`WF-000123`) is a programmatic alias, output is always canonical (verified) |
| 122 | `uid` system-generated unique id (may) | conforms | `uid` on resources; numeric database id as a string |
| 122 | references to other resources are strings holding the resource name, field named after the resource | conforms | `merged_into`, `workflow`, `creator`/`actor` = `accounts/{id}` |
| 122 | fields must not be called `name` except for the resource name | conforms | human name is `display_name` |
| 122 | resource must be addressable by its `name` | deviates | `Member.name` is `projects/p/members/alice` and the route is `/members/{account}` (no `getMember`); `Operation.name` is the import job name; `accounts/{a}` has no Get. Reason: members and tokens are admin plumbing, not user resources |
| 131 Get | GET on `/v1/{name}`, no body, returns the resource | conforms (verified) | `getWorkflow`, `getTask`, `getComment`, `getAttachment` (new), `getEvent`, `getImportJob`, `getConfig` (singleton). Missing Get for members/tokens/accounts (see 122) |
| 132 List | GET on the collection, `parent` in the path, one repeated field + `next_page_token` | conforms (verified) | `list*Response`. Empty page is `[]`, not null (verified) |
| 132 | soft-deleted excluded by default, `show_deleted` | conforms (verified) | `ShowDeleted` on workflows and tasks. Comments: deleted comments are hidden from list and get (404) and there is no `show_deleted` (deviates, minor; verified) |
| 132 | `total_size` (optional) | n/a | not offered |
| 133 Create | POST to the collection, body is the resource, returns the fully populated resource | conforms (verified) | `createWorkflow`, `createTask`, `createComment`, `createAttachment`. Status is 200, the AIP fixes none |
| 133 | `{resource}_id` should exist on data-plane APIs | deviates | `workflow_id`/`task_id` are reserved and rejected (`CLIENT_ID_UNSUPPORTED`, verified). Reason: ids are display codes imported from legacy systems (`WF-000123`) and must be unique and sequential. ADR-0009 lists this row as "conforms"; it should say "deviates" |
| 134 Update | PATCH `/v1/{name}`, mask in query, returns the resource | conforms (verified) | `updateWorkflow`, `updateTask`, `updateConfig` |
| 134 | body maps to the resource field | deviates | body is a wrapper `{workflow: {...}, etag, reason, note, request_id, validate_only}` (`UpdateWorkflowRequest`). Reason: audit metadata (`reason`, `note`) belongs to the change, not the resource. A flat body is accepted but then **ignored silently** (backlog C-9) |
| 134 | `update_mask` optional, omitted = all populated fields, `*` = replace, empty mask = no-op | deviates | `UpdateMask` is `required: true`; missing, empty and `*` all answer 400 `INVALID_UPDATE_MASK` (verified). Reason: every write is an audited event; implicit masks hide what changed |
| 134 | state fields must not be directly writable | deviates (partly) | `stages` is writable via `update_mask=stages` under the rule table; `scope`/`merged_into` are not (`:setScope` only, verified). Reason: the rule table (`docs/adr/0003`) is the guard |
| 135 Delete | DELETE `/v1/{name}`, `etag` optional, soft delete returns the resource | conforms (verified) | `deleteWorkflow/Task/Comment` return the resource; stale `etag` is 409 ABORTED (verified). `removeMember` is a hard delete and returns 204 |
| 136 Custom | POST + `:camelVerb`, GET only for reads | conforms | `:advanceStage`, `:regressStage`, `:setScope`, `:assign`, `:resolve`, `:reopen`, `:undelete`, `:apply`, `:rollback`, `:revoke`, `:initUpload`, `:query` are POST; `:diff`, `:download` are GET reads |
| 136 | custom method for the resource is called on `name` | conforms | verb hangs on the resource path |
| 136 | operationIds are verb + noun | deviates (minor) | `query`, `getMe`, `setScope`; operationId has no AIP weight in OpenAPI |
| 126 Enums | UPPER_SNAKE, first value `<ENUM>_UNSPECIFIED` | conforms | `Side`, `StageState`, `Scope`, `Health`, `Role`, `AccountKind`, `ImportAction`, comment `kind`, event `channel`, `membership`. `*_UNSPECIFIED` is never produced and is rejected on input (`INVALID_KIND`, ...; verified) |
| 126 | enums for sets that change rarely; strings (kebab-case) for sets that change often | deviates (minor) | stage keys (`development`, `dual_run`) are configurable strings in snake_case; diff `changes[].kind` (`source`/`fill`/`conflict`) is a lowercase enum. Reason: stage keys are database keys of the rule table |
| 151 LRO | long calls return `google.longrunning.Operation`, Operations service exists, metadata type defined | deviates | `applyImportJob`/`rollbackImportJob` return `Operation{name, done:true, response}` synchronously (documented in the `Operation` schema). No Operations service, no `GetOperation`/`ListOperations`, no `metadata`; `name` is the import job name and polling is `getImportJob`. Reason: work completes inside one transaction (ADR-0002); the shape keeps clients forward compatible |
| 154 etag | `etag` string on the resource, server provides it, mismatch is ABORTED, absent is allowed | conforms (verified) | `etag` readOnly on `Workflow`, `Task`, `Comment`, `Config`; stale etag 409 `ABORTED/ETAG_MISMATCH` with `current_etag`; omitted allowed |
| 154 | etag values per RFC 7232 (quoted); no behaviour annotation | deviates (minor) | values are opaque unquoted tokens (`ev-8102`, derived from the latest event id) and the field is marked `readOnly` |
| 155 request ids | optional `request_id` on request messages (not on the resource), UUID-able, replay returns first result | conforms (verified) | replay returns the same workflow; reuse with a different body is 409 `ALREADY_EXISTS/IDEMPOTENCY_KEY_REUSED` (the AIP leaves this open). `Event.request_id` is output only |
| 155 | UUID format documented | deviates (minor) | body `request_id` is `format: uuid`, the query parameter accepts any string up to 64 characters (finding 9) |
| 158 pagination | `page_size` optional with documented default/max, negative is INVALID_ARGUMENT, larger than max is coerced | conforms (verified) | default 50, max 1000; `-1` is 400, `100000` is 200 |
| 158 | `page_token` opaque, changed arguments are INVALID_ARGUMENT, empty `next_page_token` at the end | conforms for workflows/tasks/comments/events (verified) | signed tokens; a token replayed with another filter is 400 `INVALID_PAGE_TOKEN`; garbage is 400 |
| 158 | tokens must be opaque | deviates | `listMembers` token is base64 of the last account name, `listTokens` of the last id, `diffImportJob`/`query` use a plain decimal offset (`"100"`). None grants access (authorization is re-checked), but they are user-parseable. Backlog C-5 |
| 160 filtering | one `filter` string, `=`, `!=`, `<`.., `AND`/`OR`/`NOT`/`-`, `:` has, traversal with `.`, invalid filter is INVALID_ARGUMENT | conforms (subset, verified) | documented per list operation. `a AND b OR c` is rejected as `AMBIGUOUS_FILTER` instead of applying the AIP precedence (stricter, safe). `labels:domain` (has on a map) is rejected |
| 160 | `*` wildcards in string equality | deviates | `display_name = "pg*"` matches literally (0 rows); substring search is `display_name:text` |
| 160 | malformed filter is INVALID_ARGUMENT | deviates | an unterminated quote (`display_name = unterminated"`) is accepted (200) instead of rejected. Store-level, backlog C-6 |
| 134/203 field masks | masks name resource fields, nested paths allowed | conforms (verified) | `labels`, `labels.k`, `assignees.role`, `stages.verify`; unknown or read-only path is 400 (hint `use :setScope`) |
| 164 soft delete | `delete_time` on the resource; Delete returns the resource | conforms for workflows/tasks (verified) | `delete_time` readOnly on `Workflow`/`Task`; `deleteComment` returns the comment but `Comment` has no `delete_time` and there is no undelete (deviates, comments are immutable evidence) |
| 164 | `purge_time` field and a documented purge strategy | deviates | there is no purge: soft-deleted rows are retained forever (events are append-only, ADR-0005). No `:expunge` |
| 164 | Undelete is POST `:undelete`, returns the resource | conforms (verified) | |
| 164 | Get on a soft-deleted resource returns it | conforms for workflows/tasks, deviates for comments (verified) | `getComment` on a deleted comment is 404 |
| 164 | Delete of a deleted resource is NOT_FOUND; Undelete of a live one is ALREADY_EXISTS | deviates | both are idempotent 200 (verified) and write no event. Reason: retried CLI/agent calls must be safe; documented on the operations |
| 193 errors | `{error:{code,status,message,details}}`, `code` is the HTTP status, canonical `status` | conforms (verified) | all 30 error responses of the pass validated against the `Error` schema |
| 193 | every error has an `ErrorInfo` with `reason` matching `[A-Z][A-Z0-9_]+[A-Z0-9]`, `domain`, `metadata` | conforms (verified) | domain is `openetl` (the AIP wants a globally unique service name) |
| 193 | PERMISSION_DENIED before NOT_FOUND | conforms (verified) | a non-member gets 403 `NOT_A_MEMBER` even for a project that does not exist; 404 only for members |
| 193 | canonical code for a wrong HTTP method | deviates | `400 INVALID_ARGUMENT / METHOD_NOT_ALLOWED`, no `Allow` header; REST would say 405. Pinned by `gateway_test.go`; documented in `info.description`. Backlog C-7 |
| 193 | message stable, dynamic data in metadata | deviates (minor) | messages embed dynamic values (project id, method, path) in addition to metadata |
| 203 field behaviour | OUTPUT_ONLY: ignored on input without error | conforms (verified) | `name`, `uid`, `health`, `create_time`, `scope`, `creator`, `current_stage`, `delete_time` sent on create were all ignored (200, server values returned) |
| 203 | REQUIRED fields marked | deviates | `display_name` of `Workflow` is required on create but is not in `required` (same schema serves sparse update); stated in the schema description and enforced by the server (400 `INVALID_DISPLAY_NAME`, verified). `Task`, `Comment`, `Attachment`, `Member`, request schemas do list `required` |
| 203 | readOnly means output only | deviates | `ImportRow.data` reuses the `Workflow` schema, whose `scope` is `readOnly`, but import rows accept `scope: OUT_OF_SCOPE` (`docs/import-format.md`). Backlog C-8 |
| 203 | INPUT_ONLY | conforms | `ImportJob.rows` is `writeOnly` and never returned (verified: no writeOnly leak in any response) |
| 140 field names | snake_case | conforms in the spec, JSON mapping deviates | JSON uses the snake_case names verbatim (proto JSON mapping would be camelCase); recorded M0 decision |
| 123/124/127/128/129/142/148 | | not assessed | not part of this review |

## 4. Server vs spec: result of the live pass

110 calls over 43 operations, 0 divergences after the spec fixes in 2.1. The server fixes made while doing this are
limited to removing two dead helpers (`ctxKey`, `unimplemented` in `internal/server/server.go`, flagged by staticcheck).
Store-level items for worker A: `filter` accepts an unterminated string literal (AIP-160 row), list page tokens for
members/tokens/diff are not opaque (AIP-158 row), `ListWorkflows`-style filters ignore `*` wildcards.
