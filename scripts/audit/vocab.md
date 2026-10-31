# openetl vocabulary

Single source of the normative vocabulary. **Generated** by `python3 scripts/audit/audit.py --write-vocab` from the SQL seeds,
the OpenAPI contract, the Go code, the live CLI and `internal/cli/views.json`; `audit.py` fails when this file is stale.
Hand-written explanation lives in the sections marked *(convention)*; they are checked against the code where a check is possible.

## Stages (seed `stage_def`, order matters)

| key | seq | 中文名 | English |
| --- | --- | --- | --- |
| development | 1 | 开发 | Development |
| deployment | 2 | 部署 | Deployment |
| verify | 3 | 校验 | Verification |
| accept | 4 | 验收 | Acceptance |
| dual_run | 5 | 双跑 | Dual run |
| cutover | 6 | 割接 | Cutover |

Stored state of a stage (key `stage.<key>`): `pass` | `open` | `na`; no value means `open`. API: `PASSED` | `OPEN` | `NOT_APPLICABLE`.
`current_stage` = first stage (by seq) whose state is neither `pass` nor `na`; NULL / empty when none. `stage_distribution.stage` shows `done` for NULL.

## Reason kinds, owner sides and seeded reasons

Kinds (`reason.kind`): `gate_override`, `discard`, `not_applicable`, `reject`. Owner sides: `delivery`, `customer`, `unknown`.
Kind usage *(convention, enforced by core.Validate)*: `reject` = regress a PASSED stage; `discard` = move a workflow out of scope (`merged` also needs `into`);
`not_applicable` = mark a stage `na`; `gate_override` = let a side pass a gate it normally may not.

| reason_key | kind | owner_side | 中文名 | note_required |
| --- | --- | --- | --- | --- |
| customer_env_issue | gate_override | customer | 客户环境问题 |  |
| customer_source_data | gate_override | customer | 客户源数据问题 |  |
| customer_takeover | discard | customer | 移交客户自行迁移 |  |
| duplicate | discard | customer | 重复 |  |
| known_acceptable_diff | gate_override | delivery | 已知可接受差异 | yes |
| lookup_static_verified | gate_override | delivery | 维表抽检通过无日常对账 | yes |
| merged | discard | customer | 已合并 |  |
| na_code_outside_repo | not_applicable | delivery | 代码不在本仓库 | yes |
| na_continuous_stream | not_applicable | delivery | 常驻流任务 | yes |
| na_event_or_queue | not_applicable | delivery | 事件或队列触发 | yes |
| na_external_scheduler | not_applicable | unknown | 外部系统调度 | yes |
| na_lookup_passive | not_applicable | delivery | 维表无独立调度 | yes |
| na_manual_trigger | not_applicable | delivery | 手工触发 | yes |
| na_other | not_applicable | unknown | 其他 | yes |
| no_legacy_output | gate_override | customer | 老系统没有可比对的输出 |  |
| no_source | discard | customer | 无数据源 |  |
| obsolete | discard | customer | 业务不再需要 |  |
| other_discard | discard | unknown | 其他 | yes |
| other_override | gate_override | unknown | 其他 | yes |
| rej_business_logic | reject | delivery | 业务逻辑错误 |  |
| rej_other | reject | unknown | 其他 | yes |
| rej_row_count | reject | delivery | 行数不一致 |  |
| rej_value_or_schema | reject | delivery | 数值或结构不一致 |  |
| rej_verification_method | reject | unknown | 验数方法不认可 |  |
| rule_not_applicable | gate_override | delivery | 验数规则不适用 | yes |
| source_retired | discard | customer | 源已下线 |  |

## Rule table (seed `rule`)

A change to a key matching any `k_pattern` must satisfy one matching rule (value, side, reason kind). `_` in a pattern is literal. Unmatched keys: free for editors.

| k_pattern | to_value | side | reason_kind |
| --- | --- | --- | --- |
| stage.development | pass | delivery |  |
| stage.deployment | pass | delivery |  |
| stage.verify | pass | delivery |  |
| stage.accept | pass | customer |  |
| stage.dual_run | pass | delivery |  |
| stage.cutover | pass | delivery |  |
| stage.% | na | any | not_applicable |
| stage.development | open | delivery | reject |
| stage.deployment | open | delivery | reject |
| stage.verify | open | delivery | reject |
| stage.verify | open | customer | reject |
| stage.accept | open | customer | reject |
| stage.dual_run | open | delivery | reject |
| stage.cutover | open | delivery | reject |
| scope | out | any | discard |
| scope | in | any |  |

## Roles, sides, kinds, channels

- Member roles: `viewer`, `editor`, `admin` (API `VIEWER|EDITOR|ADMIN`); agents cannot be admin (`AGENT_CANNOT_BE_ADMIN`).
- Member sides: `delivery`, `customer`; event sides add `system` (API `SIDE_UNSPECIFIED|DELIVERY|CUSTOMER|SYSTEM`). Rule sides add `any`.
- Account kinds: `human`, `agent`.
- Event channels (`event.channel`): `cli`, `api`, `import`, `legacy` (API `CLI|API|IMPORT|LEGACY`). `import` and `legacy` events are historical facts and skip validation.
- Assignee roles *(convention, free keys)*: `dev_owner`, `customer_pic`, `verify_pic`; account values are stored without the `accounts/` prefix.

## Health, scope, comment kinds

| concept | stored / view value | API value |
| --- | --- | --- |
| health | ok | NORMAL |
| health | at_risk | AT_RISK |
| health | blocked | BLOCKED |
| scope | in | IN_SCOPE |
| scope | out | OUT_OF_SCOPE |
| comment kind | note | NOTE |
| comment kind | question | QUESTION |
| comment kind | issue | ISSUE |
| comment kind | risk | RISK |

Health: `blocked` when the workflow has an open `issue`, else `at_risk` when it has an open `risk`, else `ok`. A comment is *open* when it is neither
resolved nor deleted (`notes` and `questions` never change health). Scope defaults to `in`; `merged_into_id` is only shown while scope is `out`.

## Import

- Job states (`import_batch.state`): `staged`, `applied`, `failed`, `rolled_back` (API `STAGED|APPLIED|FAILED|ROLLED_BACK`).
- Row classes: `NEW`, `CHANGED`, `UNCHANGED`, `MISSING`, `CONFLICT`, `SUSPECT`; task classes `TASK_NEW|TASK_CHANGED|TASK_UNCHANGED|TASK_MISSING|TASK_CONFLICT`; `INVALID` rows are reported in `row_errors`.
- Row error reasons (Go `rowErr`): `DUPLICATE_NAME`, `DUPLICATE_SOURCE_REFERENCE`, `ENTITY_DELETED`, `EVIDENCE_REQUIRED_FOR_OVERRIDE`, `INVALID_DISPLAY_NAME`, `INVALID_KEY`, `INVALID_NAME`, `INVALID_ROW`, `INVALID_SOURCE`, `INVALID_STAGE_STATE`, `INVALID_TASK`, `INVALID_VALUE`, `MERGE_CYCLE`, `MERGE_TARGET_INVALID`, `MERGE_TARGET_REQUIRED`, `NO_RULE_FOR_VALUE`, `PARENT_DELETED`, `REASON_REQUIRED`, `SIDE_NOT_ALLOWED`, `STAGE_ORDER_VIOLATION`, `UNKNOWN_REASON`, `UNKNOWN_STAGE`, `UNSUPPORTED_FIELD`, `VALUE_TOO_LONG`.
- Diff change kinds: `source`, `fill`, `conflict`. Resolutions: `merge`, `skip`, `link`, `create`.

## Public views (query surface, from `internal/cli/views.json`)

- `workflows` (One row per live workflow with derived stage and health): `id`, `code`, `project_id`, `name`, `scope`, `attrs`, `assignees`, `merged_into_id`, `create_time`, `current_stage`, `open_issues`, `open_risks`, `health`
- `tasks` (One row per live task): `id`, `code`, `project_id`, `name`, `attrs`, `assignees`, `create_time`, `workflow_id`
- `events` (Audit log: one row per changed key of every event): `event_id`, `project_id`, `entity_id`, `type`, `actor`, `side`, `channel`, `reason_key`, `note`, `corrects_event_id`, `event_time`, `create_time`, `k`, `old_v`, `new_v`
- `comments` (One row per comment of a live workflow (comments of soft-deleted workflows are hidden)): `id`, `project_id`, `workflow_id`, `kind`, `body`, `resolved`, `deleted`
- `workflow_stages` (Every stage of every live workflow; no change means open): `workflow_id`, `project_id`, `stage_key`, `seq`, `status`
- `stage_distribution` (In-scope workflows grouped by current stage and health): `project_id`, `stage`, `health`, `workflows`
- `open_items` (Unresolved issues, risks and questions per live workflow; only workflows with at least one): `workflow_id`, `project_id`, `open_issues`, `open_risks`, `open_questions`

## Event-log key conventions *(convention, mirrored by scripts/reconcile.py)*

`delta.k` / `entity_latest.k`; the current value of a key is the `new_v` of the delta with the greatest `event_id` (never `event_time`). A removed key has `new_v` NULL.

| entity type | key | value (JSON) |
| --- | --- | --- |
| workflow, task | name | string (display_name) |
| workflow, task | attr.<label> | string (labels, lower-case; empty string allowed) |
| workflow, task | ann.<name> | string (annotations, free text) |
| workflow, task | assignee.<role> | string (account name) |
| workflow, task | source.system / source.ref / source.hash | string |
| workflow, task | deleted | true \| false (soft delete; the `delete_time` is the event's create_time) |
| workflow | stage.<stage> | "pass" \| "open" \| "na" |
| workflow | scope | "in" \| "out" |
| workflow | into | number (merge target workflow id; used with scope out) |
| workflow | evidence.<stage> | list of attachment names |
| task | workflow | <workflow entity id>; exactly one workflow per task (assign moves it) |
| task | cascade | <event id of the workflow delete>; set with deleted=true when a workflow delete took the task along, cleared by the workflow undelete |
| workflow, task | override.<changed key> | {"side": reserved side, "evidence": [attachment names]}; an admin acted for the other side (INV-29) |
| comment | on | number (workflow id) |
| comment | kind | "note" \| "question" \| "issue" \| "risk" |
| comment | body | string (immutable) |
| comment | resolved | true \| false |
| comment | deleted | true \| false |
| comment | attachments | list of attachment names |
| attachment | filename, size, sha256, storage_key | string / number |

Idempotency keys *(convention)*: import events `imp-<job>-<row>` (tasks `imp-<job>-<row>-<n>`), rollback events `imprb-<job>-<event>`; otherwise the client's `request_id`.

## Resource names and ids *(convention + names.go)*

`projects/{project}` (lower-case slug), `.../workflows/wf-000123` (code `WF-000123`, parsed case-insensitively), `projects/{p}/tasks/tk-000123`, `.../comments/cm-000123`,
`.../attachments/at-000123`, `.../events/ev-123` (not zero padded; also the etag), `projects/{p}/importJobs/{n}`, `accounts/{account}`, `projects/{p}/config`.
Id prefixes in code: `wf`, `tk`, `cm`, `ev`, `at`.

## Error statuses, HTTP codes and reasons

| status | HTTP | reasons raised in code |
| --- | --- | --- |
| ABORTED | 409 | `ETAG_MISMATCH` |
| ALREADY_EXISTS | 409 | `IDEMPOTENCY_KEY_REUSED`, `REQUEST_ID_REUSED`, `STORAGE_KEY_REGISTERED` |
| FAILED_PRECONDITION | 400 | `AGENT_CANNOT_BE_ADMIN`, `ENTITY_DELETED`, `EVIDENCE_REQUIRED_FOR_OVERRIDE`, `IMPORT_JOB_STATE`, `LAST_ADMIN`, `MERGE_CYCLE`, `NOT_REGRESSIBLE`, `PARENT_DELETED`, `QUERY_TIMEOUT_UNSUPPORTED`, `REASON_REQUIRED`, `REGRESS_LATER_STAGES_PASSED`, `ROLLBACK_BLOCKED`, `SHA256_MISMATCH`, `SIZE_MISMATCH`, `STAGE_ORDER_VIOLATION`, `UPLOAD_NOT_FOUND` |
| INTERNAL | 500 | `INTERNAL` |
| INVALID_ARGUMENT | 400 | `AMBIGUOUS_FILTER`, `AS_OF_UNSUPPORTED`, `CLIENT_ID_UNSUPPORTED`, `EVENT_TIME_IN_FUTURE`, `FILE_TOO_LARGE`, `INVALID_ACCOUNT`, `INVALID_ARGUMENT`, `INVALID_AS_OF`, `INVALID_BODY`, `INVALID_DISPLAY_NAME`, `INVALID_EVIDENCE`, `INVALID_EXPIRE_TIME`, `INVALID_FILENAME`, `INVALID_FILTER`, `INVALID_KEY`, `INVALID_KIND`, `INVALID_MERGED_INTO`, `INVALID_ORDER_BY`, `INVALID_PAGE_SIZE`, `INVALID_PAGE_TOKEN`, `INVALID_PROJECT_ID`, `INVALID_QUERY`, `INVALID_REASON`, `INVALID_REQUEST_ID`, `INVALID_RESOLUTION`, `INVALID_ROLE`, `INVALID_RULE`, `INVALID_SCOPE`, `INVALID_SETTING`, `INVALID_SHA256`, `INVALID_SIDE`, `INVALID_SIZE`, `INVALID_SOURCE`, `INVALID_STAGE`, `INVALID_STAGE_STATE`, `INVALID_STORAGE_KEY`, `INVALID_UPDATE_MASK`, `INVALID_VALUE`, `INVALID_WORKFLOW`, `MERGE_TARGET_INVALID`, `MERGE_TARGET_REQUIRED`, `METHOD_NOT_ALLOWED`, `NO_ROWS`, `NO_RULE_FOR_VALUE`, `QUERY_FAILED`, `RESULT_TOO_LARGE`, `RULES_EMPTY`, `UNKNOWN_REASON`, `UNKNOWN_STAGE`, `VALUE_TOO_LONG`, `WORKFLOW_NOT_FOUND` |
| NOT_FOUND | 404 | `NOT_FOUND` |
| PERMISSION_DENIED | 403 | `ACCOUNT_IN_OTHER_PROJECT`, `GLOBAL_CONFIG_SHARED`, `NOT_A_MEMBER`, `ROLE_INSUFFICIENT`, `SIDE_NOT_ALLOWED` |
| RESOURCE_EXHAUSTED | 429 | `PROJECT_STORAGE_QUOTA_EXCEEDED`, `QUERY_SERVER_BUSY`, `QUERY_TOO_MANY_CONCURRENT`, `TOO_MANY_PENDING_UPLOADS` |
| UNAUTHENTICATED | 401 | `UNAUTHENTICATED` |
| UNIMPLEMENTED | 501 |  |

`FAILED_PRECONDITION` maps to HTTP 400 (AIP-193). Error body: `{"error":{"code","status","message","details":[{"@type":"ErrorInfo","reason","domain":"openetl","metadata"}]}}`.

## Environment variables read by shipped code

| variable | read in |
| --- | --- |
| OPENETL_ACCESS_LOG | cmd/openetl-server/main.go |
| OPENETL_ADDR | cmd/openetl-server/main.go |
| OPENETL_ALLOW_GLOBAL_CONFIG | internal/server/handlers_admin.go |
| OPENETL_ALLOW_HTTP | internal/cli/client.go |
| OPENETL_BASE_URL | internal/cli/app.go |
| OPENETL_DB_DSN | cmd/openetl-server/main.go |
| OPENETL_METRICS_TOKEN | internal/server/metrics.go |
| OPENETL_PAGE_TOKEN_KEY | internal/store/pagetoken.go |
| OPENETL_PROJECT | internal/cli/app.go |
| OPENETL_PROJECT_STORAGE_QUOTA_BYTES | internal/server/handlers_attachment.go |
| OPENETL_PUBLIC_URL | internal/server/handlers_attachment.go, internal/storage/env.go |
| OPENETL_QUERY_DSN | cmd/openetl-server/main.go |
| OPENETL_QUERY_MAX_PER_ACCOUNT | internal/store/query.go |
| OPENETL_QUERY_MAX_TOTAL | internal/store/query.go |
| OPENETL_S3_ACCESS_KEY_ID | internal/storage/env.go |
| OPENETL_S3_BUCKET | internal/storage/env.go |
| OPENETL_S3_ENDPOINT | internal/storage/env.go |
| OPENETL_S3_PATH_STYLE | internal/storage/env.go |
| OPENETL_S3_PREFIX | internal/storage/env.go |
| OPENETL_S3_REGION | internal/storage/env.go |
| OPENETL_S3_SECRET_ACCESS_KEY | internal/storage/env.go |
| OPENETL_SOURCE_PASSWORD | internal/importsrc/source.go |
| OPENETL_STORAGE | internal/storage/env.go |
| OPENETL_STORAGE_DIR | internal/storage/env.go |
| OPENETL_STORAGE_SECRET | internal/storage/env.go |
| OPENETL_TOKEN | internal/cli/app.go |
| OPENETL_TRUST_PROXY | internal/server/handlers_attachment.go |

## CLI (live `--help` tree)

Commands: `attachment get`, `auth login`, `auth logout`, `auth status`, `comment add`, `comment delete`, `comment list`, `comment reopen`, `comment resolve`, `config get`, `config set`, `events get`, `events list`, `import apply`, `import diff`, `import rollback`, `import show`, `import stage`, `me`, `member add`, `member list`, `member remove`, `query`, `scope set`, `stage advance`, `stage regress`, `task assign`, `task create`, `task delete`, `task get`, `task list`, `task restore`, `task update`, `token create`, `token list`, `token revoke`, `views`, `workflow create`, `workflow delete`, `workflow get`, `workflow list`, `workflow restore`, `workflow update`.
Global flags: . Exit codes: `0` ok, `1` runtime/API error, `2` usage error.
Output: compact JSON on stdout; errors as `{"error":{...}}` on stderr.
