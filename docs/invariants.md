# Invariants

What the system must always hold. Each line: the rule, where it is enforced, and the test that proves it.
Tests marked (D) are covered by the differential test `internal/store/diff_test.go`, which drives random
operation sequences through the reference model `internal/store/refmodel` and through the real HTTP server and
MySQL and compares outcomes, state and event counts after every step (defaults: 20 seeds x 300 steps;
`SEED=n` replays one seed; `DIFF_SEEDS` / `DIFF_STEPS` go bigger). Conflicts between sources are in
`docs/backlog/A.md`.

Test files: `core` = `internal/core/validate_test.go`, `store` = `internal/store/store_test.go`,
`diff` = `internal/store/diff_test.go`, `conc` = `internal/store/concurrency_test.go`,
`chaos` = `internal/store/chaos_test.go`, `hard` = `internal/store/hardening_test.go`,
`latest` = `internal/store/latest_test.go`, `import` = `internal/store/import_test.go`,
`gw` = `internal/server/gateway_test.go`.

## Stages and rules

| ID | Invariant | Enforced in | Proven by |
| --- | --- | --- | --- |
| INV-01 | A stage becomes PASSED only if every earlier stage (by `seq`) is PASSED or NOT_APPLICABLE after the same event. | `core.builtin` (`STAGE_ORDER_VIOLATION`) | `core` "order violation", "same event ordered passes"; `store` TestStageOrderAndSides; (D) |
| INV-02 | A PASSED stage leaves PASSED only through a reject-kind reason (seeded: `rej_row_count`, `rej_value_or_schema`, `rej_business_logic`, `rej_verification_method`, `rej_other`), given to the regress operation or to setting the stage OPEN. PASSED -> NOT_APPLICABLE in one step is refused (regress first). | `core.builtin` (`StateOpen`, `StateNA`), `regressStage` (`NOT_REGRESSIBLE`, `REASON_REQUIRED`) | `core` "regress without reason", "regress wrong kind", "passed to na ..."; `store` TestRegressNeedsReason; (D) |
| INV-03 | Marking a stage NOT_APPLICABLE needs a `not_applicable`-kind reason (and its note when the reason requires one); any side may do it. | `core.builtin`, rule `stage.% na any` | `core` "na without reason", "na wrong kind"; (D) |
| INV-04 | Side rules come from the rule table: who may PASS or reopen which stage; an `admin` role satisfies every side requirement; a `viewer` can never write. | `core.checkRules`; `server.route` (`ROLE_INSUFFICIENT`) | `core` "accept by delivery denied", "accept by admin ok"; `gw` TestHappyPath; (D) |
| INV-05 | A reason named on any write must exist and be active, and its note is required when the reason says so; a blank note counts as none. | `core.Validate` (`UNKNOWN_REASON`, `REASON_REQUIRED`) | `core` "unknown reason", "inactive reason"; (D) |
| INV-06 | Going OUT_OF_SCOPE needs a `discard`-kind reason; going IN_SCOPE needs none and clears `merged_into`. | `core.builtin` (`scope`), `setScope` | `core` "scope out needs discard", "scope in"; (D). Gap: backlog A-2 |
| INV-07 | Merge rules: reason `merged` needs a target; the target is a live workflow of the same project; a workflow cannot merge into itself or into a chain that leads back to it (no cycles). The check also runs when only `into` changes. | `core.checkMerge`; `store.txEnv` | `core` "merged into self", "merge cycle via chain", "merge cycle two hops away", "retargeting a merge is checked too"; `store` TestScopeAndMergeCycle; (D) |
| INV-08 | Concurrent merges cannot create a cycle (A into B racing B into A: at most one commits). Merge validation row-locks every workflow it reads; a deadlock victim is retried and then sees the other merge. | `store.txEnv.lock`, `Apply` retry on error 1213 | `conc` TestConcurrentMergesNeverFormACycle |
| INV-09 | Events with channel `legacy` or `import` are historical facts and bypass validation. | `core.Validate` | `core` "legacy bypasses everything"; `store` TestLegacyChannelBypassesRules |

## Derived state

| ID | Invariant | Enforced in | Proven by |
| --- | --- | --- | --- |
| INV-10 | `entity_latest` equals the recomputation from `delta` (newest value per entity and key, `v` NULL for removed keys), always: after create, update, corrections, soft delete, import apply and rollback, crash and kill. | same-transaction `upsertLatest`; `RebuildLatest`; `VerifyLatest` | `latest` TestEntityLatestMatchesDelta, TestEntityLatestUntouchedByRollback; `diff` (per touched entity after every write, whole database every 25 steps and at the end); `chaos` (after every SIGKILL) |
| INV-11 | An event is written iff the stored value of some key changes; key-level, not semantic ("absent -> default" counts as a change). A request that changes nothing writes no event and keeps the etag. | `Apply` step 4 (no-op elimination) | `store` TestNoOpWritesNothing; (D) event counts. See backlog A-3 |
| INV-12 | Per entity, event ids strictly increase in commit order; the etag is `ev-<latest event id>`, so it changes on every recorded event and only then. | `Apply` (row lock before insert), `Entity.Etag` | `diff` (id monotonic, etag derived from `MAX(event.id)`); `conc` TestConcurrentWritersOnOneEntity; `gw` TestConcurrentEtagWriters |
| INV-13 | Each delta's `old_v` is the previous `new_v` of the same (entity, key); every event has at least one delta; every delta belongs to an event of the same entity. | `Apply` | `logProblems` in `integrity_test.go`, run by `diff`, `conc`, `chaos` |
| INV-14 | Health: an unresolved, undeleted `issue` comment makes a workflow BLOCKED; otherwise an unresolved `risk` makes it AT_RISK; otherwise NORMAL. Issue wins. `current_stage` is the first stage that is not PASSED/NOT_APPLICABLE ("" when done). | views `v_open_items`, `v_workflow`, `v_current_stage` | `store` TestHealthFromComments; (D) via API and the `workflows` view |

## Request semantics

| ID | Invariant | Enforced in | Proven by |
| --- | --- | --- | --- |
| INV-15 | Optimistic concurrency: a supplied etag that is not the current one gives `ETAG_MISMATCH` (current etag in the metadata); no etag skips the check. | `Apply` step 3 | `store` TestApplyCreateAndEtag; (D) |
| INV-16 | Idempotency: the same `request_id` and payload returns the original result and writes no second event, also when raced from many goroutines; the same id on another entity is `REQUEST_ID_REUSED`; the same id with a different payload is `ALREADY_EXISTS` / `IDEMPOTENCY_KEY_REUSED`. The payload fingerprint ignores `request_id`, `validate_only` and `etag`. | `Apply` step 2, `server.request.fingerprint`, `event.idem_fp` (migration 0005) | `store` TestApplyIdempotency, TestApplyIdempotencyConcurrent; `hard` TestIdempotencyKeyBoundToPayload; `conc` TestConcurrentSameRequestID; `chaos` replays; (D) |
| INV-17 | `validate_only` writes nothing (no entity, event, delta, cache row) and reserves no request id, but returns the outcome a real call would have. | `Apply` (rollback) | `store` TestValidateOnlyWritesNothing; `hard` TestIdempotencyKeyBoundToPayload; (D) |
| INV-18 | Soft delete and undelete are symmetric: undelete restores every value; deleted entities drop out of default lists and views, stay readable with `show_deleted` and in the audit trail. | key `deleted` | `gw` TestUpdateScopeDeleteTasks; (D) full state compare after delete/undelete |
| INV-19 | Comments are never mutated: body and kind are written once; only `resolved` and `deleted` change; a deleted comment cannot be resolved or deleted again (NOT_FOUND). | `mutateComment` | (D) comment ops; `gw` TestHappyPath |
| INV-20 | Project isolation: every read and write is scoped by `project_id`; an id of another project is NOT_FOUND (writes), absent from lists and views, and invalid as a merge target. | `loadEntity`, `Apply` lock query, `listQuery`, view rewriting in `GuardQuery` | `store` TestProjectIsolation; `diff` (foreign ids on every op kind; lists compared exactly) |
| INV-26 | A soft-deleted workflow or task accepts no change except the one that restores it (`:undelete`): any other write is FAILED_PRECONDITION `ENTITY_DELETED`; a comment of a soft-deleted workflow is read-only the same way. A soft-deleted comment stays NOT_FOUND (INV-19). Reads are unaffected, `legacy`/`import` events bypass it (INV-09). | `core.checkLive` | `store` TestDeletedEntitiesAreReadOnly; `gw` TestDeletedEntitiesRejectWrites; (D) delete/undelete interleaved with every write kind, model rule in `refmodel.validateWF` |
| INV-27 | A task has at most one primary workflow link: a write that makes a link primary while another stays primary is INVALID_ARGUMENT `DUPLICATE_PRIMARY` (moving the primary, new one primary and old one demoted in the same change set, is allowed). Historical data with two primaries shows the lowest workflow id in `tasks.primary_workflow_id`. | `core.checkPrimary`; view `v_task` | `store` TestTaskPrimaryDeterministicAndUnique; `gw` TestDeletedEntitiesRejectWrites (create and assign) |
| INV-28 | The public views list only live data: `workflow_stages`, `open_items` and `comments` carry no row of a soft-deleted workflow (the same liveness as `workflows`); `open_items` has a row only for a workflow with an open issue, risk or question; every string column built from a literal compares equal to a literal under any connection collation (no error 1267). `events` keeps the audit trail of deleted workflows. | migration 0006 (`v_workflow_base` joins, explicit `COLLATE`) | `store` TestViewsHideDeletedWorkflows, TestViewsAreCollationSafe; `scripts/reconcile.py` on both datasets |

## Crash safety

| ID | Invariant | Enforced in | Proven by |
| --- | --- | --- | --- |
| INV-21 | A write is one transaction: after a kill -9 of the server or a killed DB connection mid-transaction no entity, event, delta or cache row of it exists, and the client saw an error. | `Apply` (single tx, deferred rollback) | `chaos` TestChaosKill9 (event/delta integrity after each kill); `conc` TestKilledConnectionLeavesNothingPartial |
| INV-22 | Every acknowledged write (HTTP 200) is in the log exactly once; a request id never yields two events; replays after a restart return the original result. | commit before response | `chaos` TestChaosKill9 |

## Import

| ID | Invariant | Enforced in | Proven by |
| --- | --- | --- | --- |
| INV-23 | Import apply and rollback are atomic: one transaction, channel `import`; a failed apply leaves nothing; rollback writes correcting events, never rewrites history, is refused when a person edited the entity afterwards, and a second rollback is a no-op. Platform-owned keys (stages, scope, assignees, labels, deletion, ...) are never overwritten by an import; only source-owned keys change and never-set platform keys may be filled. | `store.ApplyImport`, `RollbackImport`, `core.Classify` | `import` TestImportRollback, TestImportRollbackRefusedAfterLaterChange, TestImportDiffClassifications; `core` TestClassify; `latest` TestEntityLatestUntouchedByRollback |

## Listing

| ID | Invariant | Enforced in | Proven by |
| --- | --- | --- | --- |
| INV-24 | Every page token (lists, members, tokens, import diff, query) is HMAC-signed (`OPENETL_PAGE_TOKEN_KEY`, random per process when unset: multi-instance deployments must set it) and bound to a hash of project, filter, order and parent; a token used with another query, tampered or forged is `INVALID_PAGE_TOKEN`. | `store/pagetoken.go`, `listQuery.run`, `Store.Query` | `hard` TestPageTokenBoundAndSigned, TestAdminPageTokensSignedAndBound; `store` TestListFilterPagingAndAsOf, TestImportDiffClassifications (diff pages) |
| INV-25 | A filter that mixes AND (explicit or implicit) with OR at one level without parentheses is `AMBIGUOUS_FILTER` (AIP-160 and SQL disagree on precedence); parenthesised and single-operator filters work. | `filterParser.parseOr` | `hard` TestFilterRejectsAmbiguousMix |

## Known limits of the test net

The differential test checks one project through the HTTP API (workflows, comments, scope, merge, soft delete,
idempotency, etags, validate-only, five actors) and compares the `workflows` view; tasks, attachments, import and
admin are covered by their own tests. The chaos test uses SIGKILL, not power loss (backlog A-11).
